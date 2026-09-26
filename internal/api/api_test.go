package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudyhome/controlplane/internal/auth"
	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/jobs"
	"github.com/cloudyhome/controlplane/internal/source"
	"github.com/cloudyhome/controlplane/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunFakeTool()
	os.Exit(m.Run())
}

type harness struct {
	t   *testing.T
	srv *httptest.Server
}

func newHarness(t *testing.T, mode string) *harness {
	origin := testutil.NewTestRepo(t, map[string]string{"tf/fake-mode": mode + "\n"})
	self, _ := os.Executable()
	cfg, err := config.Parse([]byte(`
server:
  oidc_audience: aud
  allowed_org: cloudyhome
  cancel_grace: 2s
  admins: [{ repository: cloudyhome/ops }]
tools: { terraform: { path: ` + self + ` } }
repos: { infra: { url: ` + origin + ` } }
env_profiles:
  fake: { set: { ` + testutil.FakeToolEnv + `: "1" } }
actions:
  net.plan:
    tool: terraform
    op: plan
    repo: infra
    dir: tf
    env_profile: fake
    lock: net
    allowed_refs: ["refs/heads/*"]
    params: { region: { type: enum, values: [eu, us] } }
    vars: { region: "{{ .region }}" }
    allow: [{ repository: cloudyhome/infra }]
  net.apply:
    from_plan: net.plan
    allowed_refs: ["refs/heads/main"]
    allow: [{ repository: cloudyhome/infra, environment: production }]
  host.check:
    tool: command
    command: [` + self + `, check, "{{ .target }}"]
    env_profile: fake
    params: { target: { type: enum, values: [disk, mem] } }
    allow: [{ repository: cloudyhome/app }]
`))
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	store, err := jobs.OpenStore(filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	src := source.New(data, cfg.Repos)
	mgr := jobs.NewManager(cfg, store, src, data)
	srv := httptest.NewServer(New(cfg, store, mgr, src, auth.Dev{AllowedOrg: "cloudyhome"}).Handler())
	t.Cleanup(func() {
		srv.Close()
		mgr.Shutdown()
		store.Close()
	})
	return &harness{t: t, srv: srv}
}

var (
	infraPush = map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/heads/main", "sub": "repo:cloudyhome/infra", "run_id": "7"}
	infraProd = map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/heads/main", "environment": "production"}
	infraPR   = map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/pull/5/merge", "environment": "production"}
	otherRepo = map[string]string{"repository": "cloudyhome/app", "repository_owner": "cloudyhome", "ref": "refs/heads/main"}
	admin     = map[string]string{"repository": "cloudyhome/ops", "repository_owner": "cloudyhome"}
)

func (h *harness) do(claims map[string]string, method, path, body string, hdr ...string) (*http.Response, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if claims != nil {
		req.Header.Set("Authorization", "Bearer "+auth.DevToken(claims))
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (h *harness) expect(status int, claims map[string]string, method, path, body string, hdr ...string) []byte {
	h.t.Helper()
	resp, b := h.do(claims, method, path, body, hdr...)
	if resp.StatusCode != status {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, status, b)
	}
	return b
}

func (h *harness) waitDone(claims map[string]string, id string) jobs.Job {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var j jobs.Job
		json.Unmarshal(h.expect(200, claims, "GET", "/v1/jobs/"+id, ""), &j)
		if j.Status.Done() {
			return j
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("job %s did not finish", id)
	return jobs.Job{}
}

func decodeJob(t *testing.T, b []byte) jobs.Job {
	var j jobs.Job
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestPlanApplyFlow(t *testing.T) {
	h := newHarness(t, "ok")

	var actions []actionInfo
	json.Unmarshal(h.expect(200, infraPush, "GET", "/v1/actions", ""), &actions)
	if len(actions) != 1 || actions[0].Name != "net.plan" || actions[0].Params["region"].Type != "enum" {
		t.Errorf("actions for push token = %+v", actions)
	}

	plan := decodeJob(t, h.expect(202, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu"}}`))
	if plan.Ref != "refs/heads/main" || len(plan.CommitSHA) != 40 {
		t.Errorf("plan job = %+v", plan)
	}
	if done := h.waitDone(infraPush, plan.ID); done.Status != jobs.Succeeded {
		t.Fatalf("plan status %s", done.Status)
	}

	resp, logBody := h.do(infraPush, "GET", "/v1/jobs/"+plan.ID+"/logs", "")
	if resp.Header.Get("X-Job-Status") != "succeeded" || !strings.Contains(string(logBody), "fake plan") {
		t.Errorf("logs: %s %s", resp.Header, logBody)
	}
	next := resp.Header.Get("X-Next-Offset")
	if _, rest := h.do(infraPush, "GET", "/v1/jobs/"+plan.ID+"/logs?offset="+next, ""); len(rest) != 0 {
		t.Errorf("expected empty tail, got %q", rest)
	}
	if b := h.expect(200, infraPush, "GET", "/v1/jobs/"+plan.ID+"/artifacts/plan.json", ""); !strings.Contains(string(b), "format_version") {
		t.Errorf("plan.json = %s", b)
	}

	// Apply is only for the production environment on main, and needs the plan.
	h.expect(403, infraPush, "POST", "/v1/actions/net.apply/jobs", `{"plan_job_id":"`+plan.ID+`"}`)
	h.expect(422, infraProd, "POST", "/v1/actions/net.apply/jobs", `{}`)
	h.expect(422, infraProd, "POST", "/v1/actions/net.apply/jobs", `{"plan_job_id":"`+plan.ID+`","params":{"region":"us"}}`)
	h.expect(403, infraPR, "POST", "/v1/actions/net.apply/jobs", `{"plan_job_id":"`+plan.ID+`"}`)

	apply := decodeJob(t, h.expect(202, infraProd, "POST", "/v1/actions/net.apply/jobs", `{"plan_job_id":"`+plan.ID+`"}`))
	if apply.CommitSHA != plan.CommitSHA || apply.Params["region"] != "eu" {
		t.Errorf("apply did not inherit plan: %+v", apply)
	}
	if done := h.waitDone(infraProd, apply.ID); done.Status != jobs.Succeeded {
		t.Fatalf("apply status %s", done.Status)
	}
	h.expect(409, infraProd, "POST", "/v1/actions/net.apply/jobs", `{"plan_job_id":"`+plan.ID+`"}`)
}

func TestUnresolvableRef(t *testing.T) {
	h := newHarness(t, "ok")
	featurePush := map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/heads/feature"}
	// The test origin has no feature branch, so resolving fails.
	h.expect(422, featurePush, "POST", "/v1/actions/net.plan/jobs", `{"ref":"refs/heads/feature","params":{"region":"eu"}}`)
}

func TestRejections(t *testing.T) {
	h := newHarness(t, "ok")
	h.expect(401, nil, "GET", "/v1/actions", "")
	h.expect(401, map[string]string{"repository": "evil/x", "repository_owner": "evil"}, "GET", "/v1/actions", "")
	h.expect(404, infraPush, "POST", "/v1/actions/nope/jobs", `{}`)
	h.expect(403, otherRepo, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu"}}`)
	h.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"mars"}}`)
	h.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu","x":"y"}}`)
	h.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"ref":"refs/tags/v1","params":{"region":"eu"}}`)
	h.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"ref":"--upload-pack=touch /tmp/x","params":{"region":"eu"}}`)
	h.expect(400, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"cmd":"rm -rf /"}`)
	h.expect(404, infraPush, "GET", "/v1/jobs/doesnotexist", "")
}

func TestVisibilityAndIdempotency(t *testing.T) {
	h := newHarness(t, "ok")
	body := `{"params":{"region":"eu"}}`
	first := decodeJob(t, h.expect(202, infraPush, "POST", "/v1/actions/net.plan/jobs", body, "Idempotency-Key", "7-1-net.plan"))
	again := decodeJob(t, h.expect(200, infraPush, "POST", "/v1/actions/net.plan/jobs", body, "Idempotency-Key", "7-1-net.plan"))
	if first.ID != again.ID {
		t.Errorf("idempotent retry created %s, want %s", again.ID, first.ID)
	}
	h.expect(404, otherRepo, "GET", "/v1/jobs/"+first.ID, "")
	h.expect(200, admin, "GET", "/v1/jobs/"+first.ID, "")
	h.waitDone(infraPush, first.ID)
	h.expect(409, infraPush, "POST", "/v1/jobs/"+first.ID+"/cancel", "")
}

func TestCancelAPI(t *testing.T) {
	h := newHarness(t, "hang")
	j := decodeJob(t, h.expect(202, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu"}}`))
	h.expect(404, otherRepo, "POST", "/v1/jobs/"+j.ID+"/cancel", "")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, b := h.do(infraPush, "GET", "/v1/jobs/"+j.ID+"/logs", ""); strings.Contains(string(b), "fake plan") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.expect(202, infraPush, "POST", "/v1/jobs/"+j.ID+"/cancel", "")
	if done := h.waitDone(infraPush, j.ID); done.Status != jobs.Cancelled {
		t.Fatalf("status %s", done.Status)
	}
}

func TestCommandAction(t *testing.T) {
	h := newHarness(t, "ok")
	j := decodeJob(t, h.expect(202, otherRepo, "POST", "/v1/actions/host.check/jobs", `{"params":{"target":"disk"}}`))
	if j.Ref != "" || j.CommitSHA != "" {
		t.Errorf("command job should have no ref: %+v", j)
	}
	if done := h.waitDone(otherRepo, j.ID); done.Status != jobs.Succeeded {
		t.Fatalf("status %s", done.Status)
	}
	if _, b := h.do(otherRepo, "GET", "/v1/jobs/"+j.ID+"/logs", ""); !strings.Contains(string(b), "fake check disk") {
		t.Errorf("log: %s", b)
	}
	h.expect(422, otherRepo, "POST", "/v1/actions/host.check/jobs", `{"ref":"refs/heads/main","params":{"target":"disk"}}`)
	h.expect(422, otherRepo, "POST", "/v1/actions/host.check/jobs", `{"params":{"target":"/etc/passwd"}}`)
	h.expect(403, infraPush, "POST", "/v1/actions/host.check/jobs", `{"params":{"target":"disk"}}`)
}
