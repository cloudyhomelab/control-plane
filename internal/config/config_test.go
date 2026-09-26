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
	if apply.Repo != "infra" || apply.Dir != "terraform/network" || apply.Lock != "tf-network" || apply.EnvProfile != "aws-prod" {
		t.Errorf("apply did not inherit from plan: %+v", apply)
	}
	steps, err := apply.RenderSteps(map[string]string{JobDirKey: "/j/2", InputDirKey: "/j/1"})
	if err != nil || strings.Join(steps[1], " ") != "/usr/local/bin/terraform apply -input=false -no-color /j/1/tfplan" {
		t.Errorf("apply steps = %q, %v", steps, err)
	}
	steps, err = cfg.Actions["web.deploy"].RenderSteps(map[string]string{JobDirKey: "/j", "limit": "web", "app_version": "abc1234"})
	if err != nil || steps[0][3] != "--limit=web" || steps[0][4] != `--extra-vars={"app_version": "abc1234"}` {
		t.Errorf("ansible steps = %q, %v", steps, err)
	}
	if cfg.Server.OIDCJWKSURL != GitHubIssuer+"/.well-known/jwks" {
		t.Errorf("jwks url = %s", cfg.Server.OIDCJWKSURL)
	}
}

func TestRenderKeepsValuesAsOneArgument(t *testing.T) {
	cfg, err := Parse([]byte(base + `
  host.echo:
    params: { who: { type: enum, values: ["a b; rm -rf /"] } }
    steps: [[/bin/echo, "hello {{ .who }}", "{{ .job_dir }}"]]
    allow: [{repository: r}]
`))
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := cfg.Actions["host.echo"].RenderSteps(map[string]string{JobDirKey: "/j", "who": "a b; rm -rf /"})
	if len(steps[0]) != 3 || steps[0][1] != "hello a b; rm -rf /" {
		t.Errorf("argv = %q", steps[0])
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
  a: { steps: [[/bin/true]], allow: [{repository: r}], tool: terraform }`,
		"no steps": `
  a: { allow: [{repository: r}] }`,
		"empty step": `
  a: { steps: [[]], allow: [{repository: r}] }`,
		"relative binary": `
  a: { steps: [[terraform, plan]], allow: [{repository: r}] }`,
		"templated binary": `
  a: { params: { x: { type: enum, values: [ls] } }, steps: [["/bin/{{ .x }}"]], allow: [{repository: r}] }`,
		"undeclared template param": `
  a: { steps: [[/bin/echo, "{{ .nope }}"]], allow: [{repository: r}] }`,
		"input_dir without input_from": `
  a: { steps: [[/bin/cat, "{{ .input_dir }}/x"]], allow: [{repository: r}] }`,
		"reserved param": `
  a: { params: { job_dir: { type: enum, values: [x] } }, steps: [[/bin/true]], allow: [{repository: r}] }`,
		"escaping dir": `
  a: { repo: infra, dir: ../etc, allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }`,
		"repo without refs": `
  a: { repo: infra, steps: [[/bin/true]], allow: [{repository: r}] }`,
		"refs without repo": `
  a: { allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }`,
		"no allow rules": `
  a: { steps: [[/bin/true]] }`,
		"unknown input action": `
  a: { input_from: nope, steps: [[/bin/true]], allow: [{repository: r}] }`,
		"input action sets repo": `
  a: { repo: infra, allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }
  b: { input_from: a, repo: infra, allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }`,
		"input chain": `
  a: { repo: infra, allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }
  b: { input_from: a, allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }
  c: { input_from: b, allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }`,
		"inherited repo still needs refs": `
  a: { repo: infra, allowed_refs: [x], steps: [[/bin/true]], allow: [{repository: r}] }
  b: { input_from: a, steps: [[/bin/true]], allow: [{repository: r}] }`,
	}
	for name, actions := range cases {
		if _, err := Parse([]byte(base + actions)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if testing.Verbose() {
			t.Logf("%s: %v", name, strings.TrimSpace(err.Error()))
		}
	}
}
