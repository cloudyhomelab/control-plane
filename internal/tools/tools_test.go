package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudyhome/controlplane/internal/config"
)

func catalog(t *testing.T) *config.Config {
	c, err := config.Load("../../examples/catalog.yml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func argvs(steps []Step, jobDir string) []string {
	var out []string
	for _, s := range steps {
		out = append(out, strings.ReplaceAll(strings.Join(s.Argv, " "), jobDir, "$JOB"))
	}
	return out
}

func TestTerraformPlan(t *testing.T) {
	c := catalog(t)
	job := t.TempDir()
	steps, err := Prepare(Input{
		Action: c.Actions["network.plan"], ToolPath: "terraform",
		Params: map[string]string{"region": "eu-west-1"}, SrcDir: "/src", JobDir: job,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"terraform init -input=false -no-color",
		"terraform plan -input=false -no-color -out=$JOB/tfplan -var-file=$JOB/vars.tfvars.json",
		"terraform show -json -no-color $JOB/tfplan",
	}
	if got := argvs(steps, job); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if steps[0].Dir != "/src/terraform/network" || steps[2].StdoutFile != filepath.Join(job, PlanJSONFile) {
		t.Errorf("dir/stdout = %s %s", steps[0].Dir, steps[2].StdoutFile)
	}
	var vars map[string]string
	b, _ := os.ReadFile(filepath.Join(job, "vars.tfvars.json"))
	json.Unmarshal(b, &vars)
	if vars["region"] != "eu-west-1" {
		t.Errorf("vars = %v", vars)
	}
}

func TestTerraformApply(t *testing.T) {
	c := catalog(t)
	steps, err := Prepare(Input{Action: c.Actions["network.apply"], ToolPath: "tf", SrcDir: "/src", JobDir: t.TempDir(), PlanFile: "/p/tfplan"})
	if err != nil {
		t.Fatal(err)
	}
	if got := steps[1].Argv; !reflect.DeepEqual(got, []string{"tf", "apply", "-input=false", "-no-color", "/p/tfplan"}) {
		t.Errorf("apply argv = %q", got)
	}
	if _, err := Prepare(Input{Action: c.Actions["network.apply"], ToolPath: "tf", JobDir: t.TempDir()}); err == nil {
		t.Error("apply without plan must fail")
	}
}

func TestPacker(t *testing.T) {
	c := catalog(t)
	job := t.TempDir()
	steps, err := Prepare(Input{Action: c.Actions["base-image.build"], ToolPath: "packer", Params: map[string]string{"version": "1.2.3"}, SrcDir: "/src", JobDir: job})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"packer init .", "packer build -color=false -timestamp-ui -var-file=$JOB/vars.pkrvars.json ."}
	if got := argvs(steps, job); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestAnsible(t *testing.T) {
	c := catalog(t)
	job := t.TempDir()
	steps, err := Prepare(Input{Action: c.Actions["web.deploy"], ToolPath: "ansible-playbook",
		Params: map[string]string{"limit": "web", "app_version": "abc1234"}, SrcDir: "/src", JobDir: job})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ansible-playbook -i inventories/prod --limit=web --extra-vars=@$JOB/extra-vars.json site.yml"}
	if got := argvs(steps, job); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	if steps[0].Dir != "/src/ansible" {
		t.Errorf("dir = %s", steps[0].Dir)
	}
}

func TestCommand(t *testing.T) {
	c, err := config.Parse([]byte(`
server: { oidc_audience: aud, allowed_org: o }
repos: { infra: { url: /x } }
actions:
  host.echo:
    tool: command
    command: [/bin/echo, "hello {{ .who }}"]
    params: { who: { type: enum, values: ["a b"] } }
    allow: [{repository: r}]
  repo.ls:
    tool: command
    repo: infra
    dir: sub
    command: [/bin/ls]
    allowed_refs: [refs/heads/main]
    allow: [{repository: r}]
`))
	if err != nil {
		t.Fatal(err)
	}
	job := t.TempDir()
	steps, err := Prepare(Input{Action: c.Actions["host.echo"], Params: map[string]string{"who": "a b"}, JobDir: job})
	if err != nil {
		t.Fatal(err)
	}
	// A value with a space stays one argv element.
	if !reflect.DeepEqual(steps[0].Argv, []string{"/bin/echo", "hello a b"}) || steps[0].Dir != job {
		t.Errorf("step = %+v", steps[0])
	}
	steps, _ = Prepare(Input{Action: c.Actions["repo.ls"], SrcDir: "/src", JobDir: job})
	if steps[0].Dir != "/src/sub" {
		t.Errorf("dir = %s", steps[0].Dir)
	}
}
