package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/logs"
	"github.com/cloudyhome/controlplane/internal/policy"
	"github.com/cloudyhome/controlplane/internal/source"
	"github.com/cloudyhome/controlplane/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunFakeTool()
	os.Exit(m.Run())
}

type env struct {
	cfg   *config.Config
	store *Store
	src   *source.Source
	mgr   *Manager
}

func setup(t *testing.T, mode string, maxJobs int) *env {
	t.Helper()
	origin := testutil.NewTestRepo(t, map[string]string{"tf/fake-mode": mode + "\n"})
	self, _ := os.Executable()
	cfg, err := config.Parse([]byte(`
server: { oidc_audience: aud, allowed_org: cloudyhome, cancel_grace: 2s, max_concurrent_jobs: ` + itoa(maxJobs) + ` }
repos: { infra: { url: ` + origin + ` } }
env_profiles:
  fake:
    secrets: [FAKE_SECRET]
    set: { ` + testutil.FakeToolEnv + `: "1" }
actions:
  net.plan:
    repo: infra
    dir: tf
    env_profile: fake
    lock: net
    timeout: 20s
    allowed_refs: ["refs/heads/*"]
    params: { region: { type: enum, values: [eu] } }
    steps:
      - [` + self + `, init, -input=false]
      - [` + self + `, plan, -input=false, "-out={{ .job_dir }}/tfplan", "-var=region={{ .region }}"]
    allow: [{ repository: cloudyhome/infra }]
  net.apply:
    input_from: net.plan
    allowed_refs: ["refs/heads/main"]
    steps:
      - [` + self + `, apply, "{{ .input_dir }}/tfplan"]
    allow: [{ repository: cloudyhome/infra }]
`))
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	store, err := OpenStore(filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	src := source.New(data, cfg.Repos)
	mgr := NewManager(cfg, store, src, data)
	mgr.getenv = func(name string) string {
		if name == "FAKE_SECRET" {
			return "hunter2hunter2"
		}
		return os.Getenv(name)
	}
	t.Cleanup(mgr.Shutdown)
	return &env{cfg, store, src, mgr}
}

func itoa(number int) string { return strconv.Itoa(number) }

func (testEnv *env) submit(t *testing.T, action, inputJobID string) *Job {
	t.Helper()
	sha, err := testEnv.src.Resolve(context.Background(), "infra", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: NewID(), Action: action, Status: Queued, Params: map[string]string{"region": "eu"},
		Ref: "refs/heads/main", CommitSHA: sha, InputJobID: inputJobID, LockKey: testEnv.cfg.Actions[action].Lock,
		Repository: "cloudyhome/infra", Caller: policy.Claims{"repository": "cloudyhome/infra"}, CreatedAt: time.Now(),
	}
	if action == "net.apply" {
		job.Params = map[string]string{}
	}
	if err := testEnv.store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	testEnv.mgr.Enqueue(job)
	return job
}

func (testEnv *env) wait(t *testing.T, id string, want ...Status) *Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := testEnv.store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range want {
			if job.Status == status {
				return job
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	job, _ := testEnv.store.Get(context.Background(), id)
	t.Fatalf("job %s: status %s, want %v\nlog:\n%s", id, job.Status, want, testEnv.log(id))
	return nil
}

func (testEnv *env) log(id string) string {
	content, _, _ := logs.Read(testEnv.mgr.LogFile(id), 0, 1<<20)
	return string(content)
}

func TestPlanThenApply(t *testing.T) {
	testEnv := setup(t, "ok", 2)
	plan := testEnv.submit(t, "net.plan", "")
	planJob := testEnv.wait(t, plan.ID, Succeeded)
	if planJob.ExitCode == nil || *planJob.ExitCode != 0 || planJob.StartedAt == nil || planJob.FinishedAt == nil {
		t.Errorf("plan job = %+v", planJob)
	}
	log := testEnv.log(plan.ID)
	for _, want := range []string{"fake init -input=false", "fake plan -input=false", "-var=region=eu", "secret is ***", "job succeeded"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "hunter2") {
		t.Error("secret leaked into log")
	}
	if _, err := os.Stat(filepath.Join(testEnv.mgr.JobDir(plan.ID), "tfplan")); err != nil {
		t.Errorf("plan file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(testEnv.mgr.JobDir(plan.ID), "src")); !os.IsNotExist(err) {
		t.Error("worktree should be removed after job")
	}

	apply := testEnv.submit(t, "net.apply", plan.ID)
	testEnv.wait(t, apply.ID, Succeeded)
	if log := testEnv.log(apply.ID); !strings.Contains(log, "Apply complete!") {
		t.Errorf("apply log:\n%s", log)
	}
}

func TestFailure(t *testing.T) {
	testEnv := setup(t, "fail", 1)
	job := testEnv.wait(t, testEnv.submit(t, "net.plan", "").ID, Failed)
	if job.ExitCode == nil || *job.ExitCode != 3 || !strings.Contains(job.Error, "code 3") {
		t.Errorf("job = %+v", job)
	}
}

func TestCancelInterruptsProcessGroup(t *testing.T) {
	testEnv := setup(t, "hang", 1)
	job := testEnv.submit(t, "net.plan", "")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(testEnv.log(job.ID), "fake plan") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !testEnv.mgr.Cancel(job.ID) {
		t.Fatal("cancel returned false")
	}
	got := testEnv.wait(t, job.ID, Cancelled, Failed, Succeeded)
	if got.Status != Cancelled {
		t.Fatalf("status = %s\n%s", got.Status, testEnv.log(job.ID))
	}
	if !strings.Contains(testEnv.log(job.ID), "interrupted, releasing lock") {
		t.Errorf("tool did not get SIGINT:\n%s", testEnv.log(job.ID))
	}
	if testEnv.mgr.Cancel(job.ID) {
		t.Error("cancel of finished job should return false")
	}
}

func TestLockSerializes(t *testing.T) {
	testEnv := setup(t, "hang", 4)
	first := testEnv.submit(t, "net.plan", "")
	testEnv.wait(t, first.ID, Running)
	second := testEnv.submit(t, "net.plan", "")
	time.Sleep(300 * time.Millisecond)
	if job, _ := testEnv.store.Get(context.Background(), second.ID); job.Status != Queued {
		t.Fatalf("second job should wait for lock, is %s", job.Status)
	}
	if !strings.Contains(testEnv.log(second.ID), "waiting for lock net") {
		t.Errorf("log:\n%s", testEnv.log(second.ID))
	}
	// Cancelling a queued job works without it ever running.
	testEnv.mgr.Cancel(second.ID)
	if job := testEnv.wait(t, second.ID, Cancelled); job.StartedAt != nil {
		t.Error("queued job should never have started")
	}
	testEnv.mgr.Cancel(first.ID)
	testEnv.wait(t, first.ID, Cancelled)
}

func TestFailInterrupted(t *testing.T) {
	testEnv := setup(t, "ok", 1)
	job := &Job{ID: NewID(), Action: "net.plan", Status: Running, Repository: "cloudyhome/infra", CreatedAt: time.Now()}
	testEnv.store.Create(context.Background(), job)
	if count, err := testEnv.store.FailInterrupted(context.Background()); err != nil || count != 1 {
		t.Fatalf("n = %d, err = %v", count, err)
	}
	got, _ := testEnv.store.Get(context.Background(), job.ID)
	if got.Status != Failed || got.Error == "" {
		t.Errorf("job = %+v", got)
	}
}

func TestIdempotencyKeyUnique(t *testing.T) {
	testEnv := setup(t, "ok", 1)
	createJob := func() error {
		return testEnv.store.Create(context.Background(), &Job{ID: NewID(), Action: "net.plan", Status: Failed,
			Repository: "cloudyhome/infra", IdempotencyKey: "run-1", CreatedAt: time.Now()})
	}
	if err := createJob(); err != nil {
		t.Fatal(err)
	}
	if err := createJob(); err == nil {
		t.Error("duplicate idempotency key should fail")
	}
	if job, err := testEnv.store.ByIdempotencyKey(context.Background(), "cloudyhome/infra", "run-1"); err != nil || job == nil {
		t.Errorf("lookup: %v", err)
	}
}
