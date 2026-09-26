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
tools: { terraform: { path: ` + self + ` } }
repos: { infra: { url: ` + origin + ` } }
env_profiles:
  fake:
    secrets: [FAKE_SECRET]
    set: { ` + testutil.FakeToolEnv + `: "1" }
actions:
  net.plan:
    tool: terraform
    op: plan
    repo: infra
    dir: tf
    env_profile: fake
    lock: net
    timeout: 20s
    allowed_refs: ["refs/heads/*"]
    params: { region: { type: enum, values: [eu] } }
    vars: { region: "{{ .region }}" }
    allow: [{ repository: cloudyhome/infra }]
  net.apply:
    from_plan: net.plan
    allowed_refs: ["refs/heads/main"]
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
	mgr.getenv = func(k string) string {
		if k == "FAKE_SECRET" {
			return "hunter2hunter2"
		}
		return os.Getenv(k)
	}
	t.Cleanup(mgr.Shutdown)
	return &env{cfg, store, src, mgr}
}

func itoa(n int) string { return strconv.Itoa(n) }

func (e *env) submit(t *testing.T, action, planID string) *Job {
	t.Helper()
	sha, err := e.src.Resolve(context.Background(), "infra", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	j := &Job{
		ID: NewID(), Action: action, Status: Queued, Params: map[string]string{"region": "eu"},
		Ref: "refs/heads/main", CommitSHA: sha, PlanJobID: planID, LockKey: e.cfg.Actions[action].Lock,
		Repository: "cloudyhome/infra", Caller: policy.Claims{"repository": "cloudyhome/infra"}, CreatedAt: time.Now(),
	}
	if action == "net.apply" {
		j.Params = map[string]string{}
	}
	if err := e.store.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	e.mgr.Enqueue(j)
	return j
}

func (e *env) wait(t *testing.T, id string, want ...Status) *Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j, err := e.store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range want {
			if j.Status == s {
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	j, _ := e.store.Get(context.Background(), id)
	t.Fatalf("job %s: status %s, want %v\nlog:\n%s", id, j.Status, want, e.log(id))
	return nil
}

func (e *env) log(id string) string {
	b, _, _ := logs.Read(e.mgr.LogFile(id), 0, 1<<20)
	return string(b)
}

func TestPlanThenApply(t *testing.T) {
	e := setup(t, "ok", 2)
	plan := e.submit(t, "net.plan", "")
	pj := e.wait(t, plan.ID, Succeeded)
	if pj.ExitCode == nil || *pj.ExitCode != 0 || pj.StartedAt == nil || pj.FinishedAt == nil {
		t.Errorf("plan job = %+v", pj)
	}
	log := e.log(plan.ID)
	for _, want := range []string{"fake init -input=false", "fake plan -input=false", "-var-file=", "secret is ***", "job succeeded"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "hunter2") {
		t.Error("secret leaked into log")
	}
	if b, _ := os.ReadFile(filepath.Join(e.mgr.JobDir(plan.ID), "plan.json")); !strings.Contains(string(b), "format_version") {
		t.Errorf("plan.json = %q", b)
	}
	if _, err := os.Stat(filepath.Join(e.mgr.JobDir(plan.ID), "src")); !os.IsNotExist(err) {
		t.Error("worktree should be removed after job")
	}

	apply := e.submit(t, "net.apply", plan.ID)
	e.wait(t, apply.ID, Succeeded)
	if log := e.log(apply.ID); !strings.Contains(log, "Apply complete!") {
		t.Errorf("apply log:\n%s", log)
	}
	if used, _ := e.store.PlanConsumed(context.Background(), plan.ID); !used {
		t.Error("plan should be consumed")
	}
}

func TestFailure(t *testing.T) {
	e := setup(t, "fail", 1)
	j := e.wait(t, e.submit(t, "net.plan", "").ID, Failed)
	if j.ExitCode == nil || *j.ExitCode != 3 || !strings.Contains(j.Error, "code 3") {
		t.Errorf("job = %+v", j)
	}
}

func TestCancelInterruptsProcessGroup(t *testing.T) {
	e := setup(t, "hang", 1)
	j := e.submit(t, "net.plan", "")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(e.log(j.ID), "fake plan") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !e.mgr.Cancel(j.ID) {
		t.Fatal("cancel returned false")
	}
	got := e.wait(t, j.ID, Cancelled, Failed, Succeeded)
	if got.Status != Cancelled {
		t.Fatalf("status = %s\n%s", got.Status, e.log(j.ID))
	}
	if !strings.Contains(e.log(j.ID), "interrupted, releasing lock") {
		t.Errorf("tool did not get SIGINT:\n%s", e.log(j.ID))
	}
	if e.mgr.Cancel(j.ID) {
		t.Error("cancel of finished job should return false")
	}
}

func TestLockSerializes(t *testing.T) {
	e := setup(t, "hang", 4)
	first := e.submit(t, "net.plan", "")
	e.wait(t, first.ID, Running)
	second := e.submit(t, "net.plan", "")
	time.Sleep(300 * time.Millisecond)
	if j, _ := e.store.Get(context.Background(), second.ID); j.Status != Queued {
		t.Fatalf("second job should wait for lock, is %s", j.Status)
	}
	if !strings.Contains(e.log(second.ID), "waiting for lock net") {
		t.Errorf("log:\n%s", e.log(second.ID))
	}
	// Cancelling a queued job works without it ever running.
	e.mgr.Cancel(second.ID)
	if j := e.wait(t, second.ID, Cancelled); j.StartedAt != nil {
		t.Error("queued job should never have started")
	}
	e.mgr.Cancel(first.ID)
	e.wait(t, first.ID, Cancelled)
}

func TestFailInterrupted(t *testing.T) {
	e := setup(t, "ok", 1)
	j := &Job{ID: NewID(), Action: "net.plan", Status: Running, Repository: "cloudyhome/infra", CreatedAt: time.Now()}
	e.store.Create(context.Background(), j)
	if n, err := e.store.FailInterrupted(context.Background()); err != nil || n != 1 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	got, _ := e.store.Get(context.Background(), j.ID)
	if got.Status != Failed || got.Error == "" {
		t.Errorf("job = %+v", got)
	}
}

func TestIdempotencyKeyUnique(t *testing.T) {
	e := setup(t, "ok", 1)
	mk := func() error {
		return e.store.Create(context.Background(), &Job{ID: NewID(), Action: "net.plan", Status: Failed,
			Repository: "cloudyhome/infra", IdempotencyKey: "run-1", CreatedAt: time.Now()})
	}
	if err := mk(); err != nil {
		t.Fatal(err)
	}
	if err := mk(); err == nil {
		t.Error("duplicate idempotency key should fail")
	}
	if j, err := e.store.ByIdempotencyKey(context.Background(), "cloudyhome/infra", "run-1"); err != nil || j == nil {
		t.Errorf("lookup: %v", err)
	}
}
