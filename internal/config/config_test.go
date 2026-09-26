package config

import (
	"strings"
	"testing"
)

func TestExampleCatalog(t *testing.T) {
	cfg, err := Load("../../examples/catalog.yml")
	if err != nil {
		t.Fatal(err)
	}
	apply := cfg.Actions["network.apply"]
	if apply.Repo != "infra" || apply.Dir != "terraform/network" || apply.Lock != "tf-network" {
		t.Errorf("apply did not inherit from plan: %+v", apply)
	}
	if !apply.RefMatchRequired() {
		t.Error("apply should require ref match by default")
	}
	got, err := cfg.Actions["web.deploy"].Render("args", map[string]string{"limit": "web", "app_version": "abc1234"})
	if err != nil || got["limit"] != "web" {
		t.Errorf("render args = %v, %v", got, err)
	}
	if cfg.Server.OIDCJWKSURL != GitHubIssuer+"/.well-known/jwks" {
		t.Errorf("jwks url = %s", cfg.Server.OIDCJWKSURL)
	}
}

const base = `
server: { oidc_audience: aud, allowed_org: cloudyhome }
repos: { infra: { url: /tmp/x } }
actions:
`

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field": `
  a: { tool: terraform, op: plan, repo: infra, allowed_refs: [x], allow: [{repository: r}], bogus: 1 }`,
		"undeclared template param": `
  a: { tool: terraform, op: plan, repo: infra, allowed_refs: [x], allow: [{repository: r}], vars: { x: "{{ .nope }}" } }`,
		"escaping dir": `
  a: { tool: terraform, op: plan, repo: infra, dir: ../etc, allowed_refs: [x], allow: [{repository: r}] }`,
		"no allow rules": `
  a: { tool: terraform, op: plan, repo: infra, allowed_refs: [x] }`,
		"direct apply": `
  a: { tool: terraform, op: apply, repo: infra, allowed_refs: [x], allow: [{repository: r}] }`,
		"apply from non-plan": `
  a: { tool: terraform, op: validate, repo: infra, allowed_refs: [x], allow: [{repository: r}] }
  b: { from_plan: a, allowed_refs: [x], allow: [{repository: r}] }`,
		"apply overriding dir": `
  a: { tool: terraform, op: plan, repo: infra, allowed_refs: [x], allow: [{repository: r}] }
  b: { from_plan: a, dir: other, allowed_refs: [x], allow: [{repository: r}] }`,
		"ansible arg": `
  a: { tool: ansible, op: playbook, repo: infra, playbook: p.yml, inventory: i, allowed_refs: [x], allow: [{repository: r}], args: { extra: y } }`,
		"unknown tool": `
  a: { tool: bash, op: run, repo: infra, allowed_refs: [x], allow: [{repository: r}] }`,
	}
	for name, actions := range cases {
		if _, err := Parse([]byte(base + actions)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if testing.Verbose() {
			t.Logf("%s: %v", name, strings.TrimSpace(err.Error()))
		}
	}
}

func TestCommand(t *testing.T) {
	cfg, err := Parse([]byte(base + `
  host.uptime: { tool: command, command: [/usr/bin/uptime], allow: [{repository: r}] }
  host.ping:
    tool: command
    command: [/usr/bin/ping, -c, "{{ .count }}", "{{ .host }}"]
    params:
      count: { type: int, min: 1, max: 5, default: "1" }
      host: { type: enum, values: [a.example, b.example] }
    allow: [{repository: r}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Actions["host.uptime"].Op != "run" {
		t.Error("op should default to run")
	}
	argv, err := cfg.Actions["host.ping"].RenderCommand(map[string]string{"count": "3", "host": "a.example"})
	if err != nil || strings.Join(argv, " ") != "/usr/bin/ping -c 3 a.example" {
		t.Errorf("argv = %q, %v", argv, err)
	}

	bad := map[string]string{
		"relative binary":   `a: { tool: command, command: [uptime], allow: [{repository: r}] }`,
		"templated binary":  `a: { tool: command, command: ["/bin/{{ .x }}"], params: { x: { type: enum, values: [ls] } }, allow: [{repository: r}] }`,
		"empty command":     `a: { tool: command, allow: [{repository: r}] }`,
		"refs without repo": `a: { tool: command, command: [/usr/bin/uptime], allowed_refs: [x], allow: [{repository: r}] }`,
		"undeclared param":  `a: { tool: command, command: [/bin/echo, "{{ .nope }}"], allow: [{repository: r}] }`,
		"command on other":  `a: { tool: terraform, op: plan, repo: infra, allowed_refs: [x], command: [/bin/x], allow: [{repository: r}] }`,
		"repo needs refs":   `a: { tool: command, repo: infra, command: [/bin/ls], allow: [{repository: r}] }`,
	}
	for name, action := range bad {
		if _, err := Parse([]byte(base + "\n  " + action)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
