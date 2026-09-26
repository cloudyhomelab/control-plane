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

func (m *Manager) JobDir(id string) string  { return filepath.Join(m.dataDir, "jobs", id) }
func (m *Manager) LogFile(id string) string { return filepath.Join(m.JobDir(id), "log") }

// Enqueue starts a job that has already been stored as queued.
func (m *Manager) Enqueue(j *Job) {
	ctx, cancel := context.WithCancelCause(context.Background())
	m.mu.Lock()
	m.cancels[j.ID] = cancel
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.cancels, j.ID)
			m.mu.Unlock()
			cancel(nil)
		}()
		m.run(ctx, j)
	}()
}

// Cancel stops a queued or running job. It returns false if the job is not active.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	m.mu.Unlock()
	if ok {
		cancel(errCancelled)
	}
	return ok
}

// Shutdown cancels all active jobs and waits for them to finish.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	for _, c := range m.cancels {
		c(errCancelled)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) run(ctx context.Context, j *Job) {
	a := m.cfg.Actions[j.Action]
	status, exit, errMsg := Failed, (*int)(nil), ""
	finish := func() {
		if err := m.store.Finish(context.Background(), j.ID, status, exit, errMsg, time.Now()); err != nil {
			slog.Error("finish job", "job", j.ID, "err", err)
		}
		slog.Info("job finished", "job", j.ID, "action", j.Action, "status", status)
	}
	defer finish()

	if err := os.MkdirAll(m.JobDir(j.ID), 0o750); err != nil {
		errMsg = err.Error()
		return
	}
	lw, err := logs.Create(m.LogFile(j.ID), m.secretValues(a))
	if err != nil {
		errMsg = err.Error()
		return
	}
	defer lw.Close()

	status, exit, err = m.execute(ctx, j, a, lw)
	if err != nil {
		errMsg = err.Error()
		lw.Line("error: " + errMsg)
	}
	lw.Line("job " + string(status))
}

func (m *Manager) execute(ctx context.Context, j *Job, a *config.Action, lw *logs.Writer) (Status, *int, error) {
	if j.LockKey != "" {
		release, err := m.acquireLock(ctx, j.LockKey, lw)
		if err != nil {
			return ctxStatus(ctx), nil, err
		}
		defer release()
	}
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return ctxStatus(ctx), nil, context.Cause(ctx)
	}

	if err := m.store.SetRunning(context.Background(), j.ID, time.Now()); err != nil {
		return Failed, nil, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, a.Timeout.Duration, errTimeout)
	defer cancel()

	var srcDir string
	if a.Repo != "" {
		lw.Line(fmt.Sprintf("action %s, ref %s, commit %s", j.Action, j.Ref, j.CommitSHA))
		srcDir = filepath.Join(m.JobDir(j.ID), "src")
		if err := m.src.Checkout(ctx, a.Repo, j.CommitSHA, srcDir); err != nil {
			return ctxStatusOr(ctx, Failed), nil, err
		}
		defer func() {
			if err := m.src.Remove(context.Background(), a.Repo, srcDir); err != nil {
				slog.Warn("remove worktree", "job", j.ID, "err", err)
			}
		}()
	} else {
		lw.Line("action " + j.Action)
	}

	in := tools.Input{
		Action: a, ToolPath: m.cfg.Tools[a.Tool].Path, Params: j.Params,
		SrcDir: srcDir, JobDir: m.JobDir(j.ID),
	}
	if j.PlanJobID != "" {
		in.PlanFile = filepath.Join(m.JobDir(j.PlanJobID), tools.PlanFile)
		if _, err := os.Stat(in.PlanFile); err != nil {
			return Failed, nil, fmt.Errorf("plan file: %w", err)
		}
	}
	steps, err := tools.Prepare(in)
	if err != nil {
		return Failed, nil, err
	}
	env := m.env(a)
	for _, step := range steps {
		lw.Line("$ " + strings.Join(step.Argv, " "))
		code, err := runStep(ctx, step, env, lw, m.cfg.Server.CancelGrace.Duration)
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
func (m *Manager) acquireLock(ctx context.Context, key string, lw *logs.Writer) (func(), error) {
	m.mu.Lock()
	ch, ok := m.locks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		m.locks[key] = ch
	}
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
		lw.Line("waiting for lock " + key)
		select {
		case ch <- struct{}{}:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	return func() { <-ch }, nil
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
func (m *Manager) env(a *config.Action) []string {
	home := filepath.Join(m.dataDir, "home")
	os.MkdirAll(home, 0o750)
	env := []string{
		"PATH=" + m.getenv("PATH"),
		"HOME=" + home,
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"CHECKPOINT_DISABLE=1",
		"ANSIBLE_NOCOLOR=1",
		"ANSIBLE_FORCE_COLOR=0",
	}
	if a.EnvProfile == "" {
		return env
	}
	p := m.cfg.EnvProfiles[a.EnvProfile]
	for _, k := range append(append([]string{}, p.PassThrough...), p.Secrets...) {
		if v := m.getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range p.Set {
		env = append(env, k+"="+v)
	}
	return env
}

func (m *Manager) secretValues(a *config.Action) []string {
	if a.EnvProfile == "" {
		return nil
	}
	var out []string
	for _, k := range m.cfg.EnvProfiles[a.EnvProfile].Secrets {
		if v := m.getenv(k); v != "" {
			out = append(out, v)
		}
	}
	return out
}
