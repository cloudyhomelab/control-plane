package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudyhomelab/control-plane/internal/auth"
	"github.com/cloudyhomelab/control-plane/internal/config"
	"github.com/cloudyhomelab/control-plane/internal/github"
	"github.com/cloudyhomelab/control-plane/internal/jobs"
	"github.com/cloudyhomelab/control-plane/internal/source"
	"github.com/cloudyhomelab/control-plane/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunFakeTool()
	os.Exit(m.Run())
}

type harness struct {
	t    *testing.T
	srv  *httptest.Server
	runs *fakeRuns
}

// fakeRuns stands in for the GitHub API's workflow run approvals.
type fakeRuns struct {
	approvals map[string][]github.Approval // by run_id
	err       error
}

func (runs *fakeRuns) RunApprovals(_ context.Context, repository, runID string) ([]github.Approval, error) {
	return runs.approvals[repository+"#"+runID], runs.err
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
repos: { infra: { url: ` + origin + ` } }
env_profiles:
  fake: { set: { ` + testutil.FakeToolEnv + `: "1" } }
actions:
  net.plan:
    repo: infra
    dir: tf
    env_profile: fake
    lock: net
    allowed_refs: ["refs/heads/*"]
    params: { region: { type: enum, values: [eu, us] } }
    steps:
      - [` + self + `, init]
      - [` + self + `, plan, "-out={{ .job_dir }}/tfplan", "-var=region={{ .region }}"]
    allow: [{ repository: cloudyhome/infra }]
  net.apply:
    input_from: net.plan
    allowed_refs: ["refs/heads/main"]
    require_ref_match: true
    steps:
      - [` + self + `, apply, "{{ .region }}", "{{ .input_dir }}/tfplan"]
    allow: [{ repository: cloudyhome/infra, environment: production }]
  host.check:
    steps:
      - [` + self + `, check, "{{ .target }}"]
    env_profile: fake
    params: { target: { type: enum, values: [disk, mem] } }
    allow: [{ repository: cloudyhome/app }]
  host.gated:
    steps:
      - [` + self + `, gated]
    env_profile: fake
    approvers: [binarycodes, other-admin]
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
	runs := &fakeRuns{approvals: map[string][]github.Approval{}}
	srv := httptest.NewServer(New(cfg, store, mgr, src, auth.Dev{AllowedOrg: "cloudyhome"}, runs).Handler())
	t.Cleanup(func() {
		srv.Close()
		mgr.Shutdown()
		store.Close()
	})
	return &harness{t: t, srv: srv, runs: runs}
}

var (
	infraPush = map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/heads/main", "sub": "repo:cloudyhome/infra", "run_id": "7"}
	infraProd = map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/heads/main", "environment": "production"}
	infraPR   = map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/pull/5/merge", "environment": "production"}
	otherRepo = map[string]string{"repository": "cloudyhome/app", "repository_owner": "cloudyhome", "ref": "refs/heads/main"}
	admin     = map[string]string{"repository": "cloudyhome/ops", "repository_owner": "cloudyhome"}
)

func (testHarness *harness) do(claims map[string]string, method, path, body string, hdr ...string) (*http.Response, []byte) {
	testHarness.t.Helper()
	req, _ := http.NewRequest(method, testHarness.srv.URL+path, strings.NewReader(body))
	if claims != nil {
		req.Header.Set("Authorization", "Bearer "+auth.DevToken(claims))
	}
	for index := 0; index+1 < len(hdr); index += 2 {
		req.Header.Set(hdr[index], hdr[index+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		testHarness.t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	return resp, responseBody
}

func (testHarness *harness) expect(status int, claims map[string]string, method, path, body string, hdr ...string) []byte {
	testHarness.t.Helper()
	resp, responseBody := testHarness.do(claims, method, path, body, hdr...)
	if resp.StatusCode != status {
		testHarness.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, status, responseBody)
	}
	return responseBody
}

func (testHarness *harness) waitDone(claims map[string]string, id string) jobs.Job {
	testHarness.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var job jobs.Job
		json.Unmarshal(testHarness.expect(200, claims, "GET", "/v1/jobs/"+id, ""), &job)
		if job.Status.Done() {
			return job
		}
		time.Sleep(25 * time.Millisecond)
	}
	testHarness.t.Fatalf("job %s did not finish", id)
	return jobs.Job{}
}

func decodeJob(t *testing.T, body []byte) jobs.Job {
	var job jobs.Job
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPlanApplyFlow(t *testing.T) {
	testHarness := newHarness(t, "ok")

	var actions []actionInfo
	json.Unmarshal(testHarness.expect(200, infraPush, "GET", "/v1/actions", ""), &actions)
	if len(actions) != 1 || actions[0].Name != "net.plan" || actions[0].Params["region"].Type != "enum" {
		t.Errorf("actions for push token = %+v", actions)
	}

	plan := decodeJob(t, testHarness.expect(202, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu"}}`))
	if plan.Ref != "refs/heads/main" || len(plan.CommitSHA) != 40 {
		t.Errorf("plan job = %+v", plan)
	}
	if done := testHarness.waitDone(infraPush, plan.ID); done.Status != jobs.Succeeded {
		t.Fatalf("plan status %s", done.Status)
	}

	resp, logBody := testHarness.do(infraPush, "GET", "/v1/jobs/"+plan.ID+"/logs", "")
	if resp.Header.Get("X-Job-Status") != "succeeded" || !strings.Contains(string(logBody), "fake plan") {
		t.Errorf("logs: %s %s", resp.Header, logBody)
	}
	next := resp.Header.Get("X-Next-Offset")
	if _, rest := testHarness.do(infraPush, "GET", "/v1/jobs/"+plan.ID+"/logs?offset="+next, ""); len(rest) != 0 {
		t.Errorf("expected empty tail, got %q", rest)
	}

	// Apply is only for the production environment on main, and needs a plan job.
	withInput := func(id string) string { return `{"input_job_id":"` + id + `"}` }
	testHarness.expect(403, infraPush, "POST", "/v1/actions/net.apply/jobs", withInput(plan.ID))
	testHarness.expect(422, infraProd, "POST", "/v1/actions/net.apply/jobs", `{}`)
	testHarness.expect(404, infraProd, "POST", "/v1/actions/net.apply/jobs", withInput("doesnotexist"))
	testHarness.expect(403, infraPR, "POST", "/v1/actions/net.apply/jobs", withInput(plan.ID))
	testHarness.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu"},"input_job_id":"`+plan.ID+`"}`)
	testHarness.expect(422, infraProd, "POST", "/v1/actions/net.apply/jobs", `{"input_job_id":"`+plan.ID+`","params":{"region":"us"}}`)

	apply := decodeJob(t, testHarness.expect(202, infraProd, "POST", "/v1/actions/net.apply/jobs", withInput(plan.ID)))
	if apply.CommitSHA != plan.CommitSHA || apply.Ref != plan.Ref || apply.InputJobID != plan.ID || apply.Params["region"] != "eu" {
		t.Errorf("apply did not run at the plan's commit: %+v", apply)
	}
	if done := testHarness.waitDone(infraProd, apply.ID); done.Status != jobs.Succeeded {
		t.Fatalf("apply status %s", done.Status)
	}
	if _, applyLog := testHarness.do(infraProd, "GET", "/v1/jobs/"+apply.ID+"/logs", ""); !strings.Contains(string(applyLog), "fake apply eu ") || !strings.Contains(string(applyLog), "Apply complete!") {
		t.Errorf("apply log: %s", applyLog)
	}
	// An apply job is not a plan job, so it can't be used as input.
	testHarness.expect(409, infraProd, "POST", "/v1/actions/net.apply/jobs", withInput(apply.ID))
}

func TestUnresolvableRef(t *testing.T) {
	testHarness := newHarness(t, "ok")
	featurePush := map[string]string{"repository": "cloudyhome/infra", "repository_owner": "cloudyhome", "ref": "refs/heads/feature"}
	// The test origin has no feature branch, so resolving fails.
	testHarness.expect(422, featurePush, "POST", "/v1/actions/net.plan/jobs", `{"ref":"refs/heads/feature","params":{"region":"eu"}}`)
}

func TestRejections(t *testing.T) {
	testHarness := newHarness(t, "ok")
	testHarness.expect(401, nil, "GET", "/v1/actions", "")
	testHarness.expect(401, map[string]string{"repository": "evil/x", "repository_owner": "evil"}, "GET", "/v1/actions", "")
	testHarness.expect(404, infraPush, "POST", "/v1/actions/nope/jobs", `{}`)
	testHarness.expect(403, otherRepo, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu"}}`)
	testHarness.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"mars"}}`)
	testHarness.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu","x":"y"}}`)
	testHarness.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"ref":"refs/tags/v1","params":{"region":"eu"}}`)
	testHarness.expect(422, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"ref":"--upload-pack=touch /tmp/x","params":{"region":"eu"}}`)
	testHarness.expect(400, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"cmd":"rm -rf /"}`)
	testHarness.expect(404, infraPush, "GET", "/v1/jobs/doesnotexist", "")
}

func TestVisibilityAndIdempotency(t *testing.T) {
	testHarness := newHarness(t, "ok")
	body := `{"params":{"region":"eu"}}`
	first := decodeJob(t, testHarness.expect(202, infraPush, "POST", "/v1/actions/net.plan/jobs", body, "Idempotency-Key", "7-1-net.plan"))
	again := decodeJob(t, testHarness.expect(200, infraPush, "POST", "/v1/actions/net.plan/jobs", body, "Idempotency-Key", "7-1-net.plan"))
	if first.ID != again.ID {
		t.Errorf("idempotent retry created %s, want %s", again.ID, first.ID)
	}
	testHarness.expect(404, otherRepo, "GET", "/v1/jobs/"+first.ID, "")
	testHarness.expect(200, admin, "GET", "/v1/jobs/"+first.ID, "")
	testHarness.waitDone(infraPush, first.ID)
	testHarness.expect(409, infraPush, "POST", "/v1/jobs/"+first.ID+"/cancel", "")
}

func TestCancelAPI(t *testing.T) {
	testHarness := newHarness(t, "hang")
	job := decodeJob(t, testHarness.expect(202, infraPush, "POST", "/v1/actions/net.plan/jobs", `{"params":{"region":"eu"}}`))
	testHarness.expect(404, otherRepo, "POST", "/v1/jobs/"+job.ID+"/cancel", "")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, logBody := testHarness.do(infraPush, "GET", "/v1/jobs/"+job.ID+"/logs", ""); strings.Contains(string(logBody), "fake plan") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	testHarness.expect(202, infraPush, "POST", "/v1/jobs/"+job.ID+"/cancel", "")
	if done := testHarness.waitDone(infraPush, job.ID); done.Status != jobs.Cancelled {
		t.Fatalf("status %s", done.Status)
	}
}

func TestCommandAction(t *testing.T) {
	testHarness := newHarness(t, "ok")
	job := decodeJob(t, testHarness.expect(202, otherRepo, "POST", "/v1/actions/host.check/jobs", `{"params":{"target":"disk"}}`))
	if job.Ref != "" || job.CommitSHA != "" {
		t.Errorf("command job should have no ref: %+v", job)
	}
	if done := testHarness.waitDone(otherRepo, job.ID); done.Status != jobs.Succeeded {
		t.Fatalf("status %s", done.Status)
	}
	if _, logBody := testHarness.do(otherRepo, "GET", "/v1/jobs/"+job.ID+"/logs", ""); !strings.Contains(string(logBody), "fake check disk") {
		t.Errorf("log: %s", logBody)
	}
	testHarness.expect(422, otherRepo, "POST", "/v1/actions/host.check/jobs", `{"ref":"refs/heads/main","params":{"target":"disk"}}`)
	testHarness.expect(422, otherRepo, "POST", "/v1/actions/host.check/jobs", `{"params":{"target":"/etc/passwd"}}`)
	testHarness.expect(403, infraPush, "POST", "/v1/actions/host.check/jobs", `{"params":{"target":"disk"}}`)
}

func TestApprovers(t *testing.T) {
	testHarness := newHarness(t, "ok")
	token := func(runID, environment string) map[string]string {
		claims := map[string]string{"repository": "cloudyhome/app", "repository_owner": "cloudyhome", "run_id": runID}
		if environment != "" {
			claims["environment"] = environment
		}
		return claims
	}
	approve := func(runID, login, state string, environments ...string) {
		key := "cloudyhome/app#" + runID
		testHarness.runs.approvals[key] = append(testHarness.runs.approvals[key],
			github.Approval{Login: login, State: state, Environments: environments})
	}
	submit := func(status int, claims map[string]string) []byte {
		return testHarness.expect(status, claims, "POST", "/v1/actions/host.gated/jobs", `{}`)
	}

	submit(403, token("1", ""))           // not in an environment
	submit(403, token("2", "production")) // no approvals recorded
	approve("3", "someone", "approved", "production")
	if body := submit(403, token("3", "production")); !strings.Contains(string(body), "approved by someone") {
		t.Errorf("expected the actual approver in the error: %s", body)
	}
	approve("4", "binarycodes", "approved", "staging")
	submit(403, token("4", "production")) // approved, but a different environment
	approve("5", "binarycodes", "rejected", "production")
	submit(403, token("5", "production"))

	approve("6", "BinaryCodes", "approved", "production") // logins are case-insensitive
	job := decodeJob(t, submit(202, token("6", "production")))
	if done := testHarness.waitDone(token("6", "production"), job.ID); done.Status != jobs.Succeeded {
		t.Fatalf("status %s", done.Status)
	}

	testHarness.runs.err = context.DeadlineExceeded
	submit(502, token("6", "production"))

	// Actions without approvers never ask GitHub.
	testHarness.expect(202, token("7", ""), "POST", "/v1/actions/host.check/jobs", `{"params":{"target":"disk"}}`)
}
