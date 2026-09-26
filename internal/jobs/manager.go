// Package jobs stores, queues and executes control plane jobs.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/logs"
	"github.com/cloudyhome/controlplane/internal/source"
	"github.com/cloudyhome/controlplane/internal/tools"
)

var (
	errCancelled = errors.New("cancelled")
	errTimeout   = errors.New("timed out")
)

type Manager struct {
	cfg     *config.Config
	store   *Store
	src     *source.Source
	dataDir string
	getenv  func(string) string
	sem     chan struct{}

	mu      sync.Mutex
	locks   map[string]chan struct{}
	cancels map[string]context.CancelCauseFunc
	wg      sync.WaitGroup
}

func NewManager(cfg *config.Config, store *Store, src *source.Source, dataDir string) *Manager {
	return &Manager{
		cfg: cfg, store: store, src: src, dataDir: dataDir, getenv: os.Getenv,
		sem:     make(chan struct{}, cfg.Server.MaxConcurrentJobs),
		locks:   map[string]chan struct{}{},
		cancels: map[string]context.CancelCauseFunc{},
	}
}

func (manager *Manager) JobDir(id string) string  { return filepath.Join(manager.dataDir, "jobs", id) }
func (manager *Manager) LogFile(id string) string { return filepath.Join(manager.JobDir(id), "log") }

// Enqueue starts a job that has already been stored as queued.
func (manager *Manager) Enqueue(job *Job) {
	ctx, cancel := context.WithCancelCause(context.Background())
	manager.mu.Lock()
	manager.cancels[job.ID] = cancel
	manager.mu.Unlock()
	manager.wg.Add(1)
	go func() {
		defer manager.wg.Done()
		defer func() {
			manager.mu.Lock()
			delete(manager.cancels, job.ID)
			manager.mu.Unlock()
			cancel(nil)
		}()
		manager.run(ctx, job)
	}()
}

// Cancel stops a queued or running job. It returns false if the job is not active.
func (manager *Manager) Cancel(id string) bool {
	manager.mu.Lock()
	cancel, ok := manager.cancels[id]
	manager.mu.Unlock()
	if ok {
		cancel(errCancelled)
	}
	return ok
}

// Shutdown cancels all active jobs and waits for them to finish.
func (manager *Manager) Shutdown() {
	manager.mu.Lock()
	for _, cancel := range manager.cancels {
		cancel(errCancelled)
	}
	manager.mu.Unlock()
	manager.wg.Wait()
}

func (manager *Manager) run(ctx context.Context, job *Job) {
	action := manager.cfg.Actions[job.Action]
	status, exit, errMsg := Failed, (*int)(nil), ""
	finish := func() {
		if err := manager.store.Finish(context.Background(), job.ID, status, exit, errMsg, time.Now()); err != nil {
			slog.Error("finish job", "job", job.ID, "err", err)
		}
		slog.Info("job finished", "job", job.ID, "action", job.Action, "status", status)
	}
	defer finish()

	if err := os.MkdirAll(manager.JobDir(job.ID), 0o750); err != nil {
		errMsg = err.Error()
		return
	}
	logWriter, err := logs.Create(manager.LogFile(job.ID), manager.secretValues(action))
	if err != nil {
		errMsg = err.Error()
		return
	}
	defer logWriter.Close()

	status, exit, err = manager.execute(ctx, job, action, logWriter)
	if err != nil {
		errMsg = err.Error()
		logWriter.Line("error: " + errMsg)
	}
	logWriter.Line("job " + string(status))
}

func (manager *Manager) execute(ctx context.Context, job *Job, action *config.Action, logWriter *logs.Writer) (Status, *int, error) {
	if job.LockKey != "" {
		release, err := manager.acquireLock(ctx, job.LockKey, logWriter)
		if err != nil {
			return ctxStatus(ctx), nil, err
		}
		defer release()
	}
	select {
	case manager.sem <- struct{}{}:
		defer func() { <-manager.sem }()
	case <-ctx.Done():
		return ctxStatus(ctx), nil, context.Cause(ctx)
	}

	if err := manager.store.SetRunning(context.Background(), job.ID, time.Now()); err != nil {
		return Failed, nil, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, action.Timeout.Duration, errTimeout)
	defer cancel()

	var srcDir string
	if action.Repo != "" {
		logWriter.Line(fmt.Sprintf("action %s, ref %s, commit %s", job.Action, job.Ref, job.CommitSHA))
		srcDir = filepath.Join(manager.JobDir(job.ID), "src")
		if err := manager.src.Checkout(ctx, action.Repo, job.CommitSHA, srcDir); err != nil {
			return ctxStatusOr(ctx, Failed), nil, err
		}
		defer func() {
			if err := manager.src.Remove(context.Background(), action.Repo, srcDir); err != nil {
				slog.Warn("remove worktree", "job", job.ID, "err", err)
			}
		}()
	} else {
		logWriter.Line("action " + job.Action)
	}

	input := tools.Input{
		Action: action, ToolPath: manager.cfg.Tools[action.Tool].Path, Params: job.Params,
		SrcDir: srcDir, JobDir: manager.JobDir(job.ID),
	}
	if job.PlanJobID != "" {
		input.PlanFile = filepath.Join(manager.JobDir(job.PlanJobID), tools.PlanFile)
		if _, err := os.Stat(input.PlanFile); err != nil {
			return Failed, nil, fmt.Errorf("plan file: %w", err)
		}
	}
	steps, err := tools.Prepare(input)
	if err != nil {
		return Failed, nil, err
	}
	env := manager.env(action)
	for _, step := range steps {
		logWriter.Line("$ " + strings.Join(step.Argv, " "))
		code, err := runStep(ctx, step, env, logWriter, manager.cfg.Server.CancelGrace.Duration)
		if err != nil {
			if ctx.Err() != nil {
				return ctxStatus(ctx), &code, context.Cause(ctx)
			}
			return Failed, &code, fmt.Errorf("%s exited with code %d", step.Argv[0], code)
		}
	}
	zero := 0
	return Succeeded, &zero, nil
}

// acquireLock waits until no other job holds key.
func (manager *Manager) acquireLock(ctx context.Context, key string, logWriter *logs.Writer) (func(), error) {
	manager.mu.Lock()
	slot, ok := manager.locks[key]
	if !ok {
		slot = make(chan struct{}, 1)
		manager.locks[key] = slot
	}
	manager.mu.Unlock()
	select {
	case slot <- struct{}{}:
	default:
		logWriter.Line("waiting for lock " + key)
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	return func() { <-slot }, nil
}

func ctxStatus(ctx context.Context) Status {
	return ctxStatusOr(ctx, Failed)
}

func ctxStatusOr(ctx context.Context, fallback Status) Status {
	switch context.Cause(ctx) {
	case errCancelled:
		return Cancelled
	case errTimeout:
		return TimedOut
	}
	return fallback
}

// env builds the child environment from scratch; nothing from the caller reaches it.
func (manager *Manager) env(action *config.Action) []string {
	home := filepath.Join(manager.dataDir, "home")
	os.MkdirAll(home, 0o750)
	env := []string{
		"PATH=" + manager.getenv("PATH"),
		"HOME=" + home,
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"CHECKPOINT_DISABLE=1",
		"ANSIBLE_NOCOLOR=1",
		"ANSIBLE_FORCE_COLOR=0",
	}
	if action.EnvProfile == "" {
		return env
	}
	profile := manager.cfg.EnvProfiles[action.EnvProfile]
	for _, name := range append(append([]string{}, profile.PassThrough...), profile.Secrets...) {
		if value := manager.getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	for name, value := range profile.Set {
		env = append(env, name+"="+value)
	}
	return env
}

func (manager *Manager) secretValues(action *config.Action) []string {
	if action.EnvProfile == "" {
		return nil
	}
	var out []string
	for _, name := range manager.cfg.EnvProfiles[action.EnvProfile].Secrets {
		if value := manager.getenv(name); value != "" {
			out = append(out, value)
		}
	}
	return out
}
