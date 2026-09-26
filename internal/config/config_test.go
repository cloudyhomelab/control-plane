package config

import (
	"strings"
	"testing"
)

func TestExampleCatalog(t *testing.T) {
	c, err := Load("../../examples/catalog.yml")
	if err != nil {
		t.Fatal(err)
	}
	apply := c.Actions["network.apply"]
	if apply.Repo != "infra" || apply.Dir != "terraform/network" || apply.Lock != "tf-network" {
		t.Errorf("apply did not inherit from plan: %+v", apply)
	}
	if !apply.RefMatchRequired() {
		t.Error("apply should require ref match by default")
	}
	got, err := c.Actions["web.deploy"].Render("args", map[string]string{"limit": "web", "app_version": "abc1234"})
	if err != nil || got["limit"] != "web" {
		t.Errorf("render args = %v, %v", got, err)
	}
	if c.Server.OIDCJWKSURL != GitHubIssuer+"/.well-known/jwks" {
		t.Errorf("jwks url = %s", c.Server.OIDCJWKSURL)
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
