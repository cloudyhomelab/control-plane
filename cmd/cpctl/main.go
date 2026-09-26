// Command cpctl is the workflow-side client for the control plane.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const usage = `usage:
  cpctl actions
  cpctl run [-p key=value]... [-ref REF] [-plan-job ID] [-no-wait] ACTION
  cpctl status JOB_ID
  cpctl logs JOB_ID
  cpctl cancel JOB_ID
  cpctl dev-token key=value...   token for a server started with -insecure-dev-auth

environment:
  CONTROLPLANE_URL        server base URL (required)
  CONTROLPLANE_AUDIENCE   OIDC audience to request from GitHub
  CONTROLPLANE_TOKEN      bearer token to use instead of GitHub OIDC (local testing)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "dev-token" {
		devToken(args)
		return
	}
	c, err := newClient()
	if err != nil {
		fatal(err)
	}
	switch cmd {
	case "actions":
		err = c.printJSON("GET", "/v1/actions", nil)
	case "run":
		var code int
		code, err = c.run(args)
		if err == nil {
			os.Exit(code)
		}
	case "status":
		err = c.printJSON("GET", "/v1/jobs/"+arg(args), nil)
	case "logs":
		_, err = c.stream(context.Background(), arg(args))
	case "cancel":
		err = c.printJSON("POST", "/v1/jobs/"+arg(args)+"/cancel", nil)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func arg(args []string) string {
	if len(args) != 1 {
		fatal(errors.New("expected exactly one JOB_ID"))
	}
	return url.PathEscape(args[0])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "cpctl:", err)
	os.Exit(1)
}

type client struct {
	base, audience, static string
	http                   *http.Client

	mu      sync.Mutex
	token   string
	fetched time.Time
}

func newClient() (*client, error) {
	base := strings.TrimSuffix(os.Getenv("CONTROLPLANE_URL"), "/")
	if base == "" {
		return nil, errors.New("CONTROLPLANE_URL is not set")
	}
	return &client{
		base: base,
		audience: os.Getenv("CONTROLPLANE_AUDIENCE"),
		static: os.Getenv("CONTROLPLANE_TOKEN"),
		http: &http.Client{Timeout: 3 * time.Minute},
	}, nil
}

// bearer returns a GitHub OIDC token, refreshed every minute since they are short lived.
func (c *client) bearer(ctx context.Context) (string, error) {
	if c.static != "" {
		return c.static, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.fetched) < time.Minute {
		return c.token, nil
	}
	reqURL, reqTok := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqTok == "" {
		return "", errors.New("no OIDC token available: add `permissions: id-token: write` to the workflow, or set CONTROLPLANE_TOKEN")
	}
	if c.audience != "" {
		reqURL += "&audience=" + url.QueryEscape(c.audience)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	req.Header.Set("Authorization", "Bearer "+reqTok)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch OIDC token: %w", err)
	}
	defer resp.Body.Close()
	var body struct{ Value string }
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil || body.Value == "" {
		return "", fmt.Errorf("fetch OIDC token: HTTP %d", resp.StatusCode)
	}
	c.token, c.fetched = body.Value, time.Now()
	return c.token, nil
}

func (c *client) do(ctx context.Context, method, path string, body any, hdr map[string]string) (*http.Response, []byte, error) {
	tok, err := c.bearer(ctx)
	if err != nil {
		return nil, nil, err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error, Code string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return resp, b, fmt.Errorf("%s %s: HTTP %d %s: %s", method, path, resp.StatusCode, e.Code, e.Error)
		}
		return resp, b, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return resp, b, nil
}

func (c *client) printJSON(method, path string, body any) error {
	_, b, err := c.do(context.Background(), method, path, body, nil)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	json.Indent(&out, b, "", "  ")
	fmt.Println(out.String())
	return nil
}

type job struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Ref       string `json:"ref"`
	CommitSHA string `json:"commit_sha"`
	ExitCode  *int   `json:"exit_code"`
	Error     string `json:"error"`
}

type paramFlags map[string]string

func (p paramFlags) String() string { return "" }
func (p paramFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	p[k] = val
	return nil
}

func (c *client) run(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	params := paramFlags{}
	fs.Var(params, "p", "parameter key=value (repeatable)")
	ref := fs.String("ref", "", "git ref (default: the action's default)")
	planJob := fs.String("plan-job", "", "plan job to apply")
	noWait := fs.Bool("no-wait", false, "print the job id and return immediately")

	// Accept flags before and after the action name.
	var action string
	for fs.Parse(args); fs.NArg() > 0; fs.Parse(args) {
		if action != "" {
			return 0, fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		action, args = fs.Arg(0), fs.Args()[1:]
	}
	if action == "" {
		return 0, errors.New("missing ACTION")
	}

	hdr := map[string]string{}
	if id, attempt := os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT"); id != "" {
		hdr["Idempotency-Key"] = id + "-" + attempt + "-" + os.Getenv("GITHUB_JOB") + "-" + action
	}
	body := map[string]any{"params": map[string]string(params)}
	if *ref != "" {
		body["ref"] = *ref
	}
	if *planJob != "" {
		body["plan_job_id"] = *planJob
	}
	_, b, err := c.do(context.Background(), "POST", "/v1/actions/"+url.PathEscape(action)+"/jobs", body, hdr)
	if err != nil {
		return 0, err
	}
	var j job
	if err := json.Unmarshal(b, &j); err != nil {
		return 0, err
	}
	if j.Ref != "" {
		fmt.Fprintf(os.Stderr, "job %s: %s at %s (%s)\n", j.ID, action, j.Ref, j.CommitSHA)
	} else {
		fmt.Fprintf(os.Stderr, "job %s: %s\n", j.ID, action)
	}
	setOutput("job_id", j.ID)
	if *noWait {
		fmt.Println(j.ID)
		return 0, nil
	}

	// A cancelled workflow sends SIGINT; forward it as a job cancel and keep streaming.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "cpctl: cancelling job", j.ID)
		c.do(context.Background(), "POST", "/v1/jobs/"+j.ID+"/cancel", nil, nil)
	}()
	final, err := c.stream(context.Background(), j.ID)
	if err != nil {
		return 0, err
	}
	setOutput("status", final.Status)
	fmt.Fprintf(os.Stderr, "job %s %s\n", final.ID, final.Status)
	if final.Status == "succeeded" {
		return 0, nil
	}
	if final.ExitCode != nil && *final.ExitCode > 0 {
		return *final.ExitCode, nil
	}
	return 1, nil
}

// stream copies the job log to stdout until the job is done, then returns its final state.
func (c *client) stream(ctx context.Context, id string) (*job, error) {
	offset := "0"
	for {
		resp, b, err := c.do(ctx, "GET", "/v1/jobs/"+id+"/logs?offset="+offset, nil, nil)
		if err != nil {
			return nil, err
		}
		os.Stdout.Write(b)
		offset = resp.Header.Get("X-Next-Offset")
		done := resp.Header.Get("X-Job-Status") != "queued" && resp.Header.Get("X-Job-Status") != "running"
		if done && len(b) == 0 {
			break
		}
		if len(b) == 0 {
			time.Sleep(2 * time.Second)
		}
	}
	_, b, err := c.do(ctx, "GET", "/v1/jobs/"+id, nil, nil)
	if err != nil {
		return nil, err
	}
	var j job
	return &j, json.Unmarshal(b, &j)
}

// devToken prints the unsigned claims token that -insecure-dev-auth accepts.
func devToken(args []string) {
	claims := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			fatal(fmt.Errorf("want key=value, got %q", a))
		}
		claims[k] = v
	}
	if claims["repository"] == "" {
		fatal(errors.New("repository=OWNER/NAME is required"))
	}
	if claims["repository_owner"] == "" {
		claims["repository_owner"], _, _ = strings.Cut(claims["repository"], "/")
	}
	b, _ := json.Marshal(claims)
	fmt.Println(base64.RawURLEncoding.EncodeToString(b))
}

func setOutput(k, v string) {
	f := os.Getenv("GITHUB_OUTPUT")
	if f == "" {
		return
	}
	if fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
		fmt.Fprintf(fh, "%s=%s\n", k, v)
		fh.Close()
	}
}
