// Package tools turns a catalog action into the exact argv steps to run. Nothing here uses a shell.
package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cloudyhome/controlplane/internal/config"
)

type Step struct {
	Argv []string
	Dir  string
	// StdoutFile, when set, captures stdout to a job artifact instead of the log.
	StdoutFile string
}

type Input struct {
	Action   *config.Action
	ToolPath string
	Params   map[string]string
	SrcDir   string
	JobDir   string
	// SrcDir is the repo checkout, or empty for a command action without a repo.
	// PlanFile is the saved plan to apply (terraform apply only).
	PlanFile string
}

const (
	PlanFile     = "tfplan"
	PlanJSONFile = "plan.json"
)

// Prepare writes any var files into the job dir and returns the steps to run in order.
func Prepare(in Input) ([]Step, error) {
	a := in.Action
	dir := filepath.Join(in.SrcDir, a.Dir)
	if in.SrcDir == "" {
		dir = in.JobDir
	}
	vars, err := a.Render("vars", in.Params)
	if err != nil {
		return nil, err
	}
	bin := in.ToolPath
	step := func(args ...string) Step { return Step{Argv: append([]string{bin}, args...), Dir: dir} }

	switch a.Tool + "." + a.Op {
	case "terraform.plan":
		varFile, err := writeVars(in.JobDir, "vars.tfvars.json", vars)
		if err != nil {
			return nil, err
		}
		plan := filepath.Join(in.JobDir, PlanFile)
		show := step("show", "-json", "-no-color", plan)
		show.StdoutFile = filepath.Join(in.JobDir, PlanJSONFile)
		return []Step{
			step("init", "-input=false", "-no-color"),
			step(append([]string{"plan", "-input=false", "-no-color", "-out=" + plan}, varFile...)...),
			show,
		}, nil
	case "terraform.apply":
		if in.PlanFile == "" {
			return nil, fmt.Errorf("apply needs a plan file")
		}
		return []Step{
			step("init", "-input=false", "-no-color"),
			step("apply", "-input=false", "-no-color", in.PlanFile),
		}, nil
	case "terraform.validate":
		return []Step{
			step("init", "-input=false", "-no-color", "-backend=false"),
			step("validate", "-no-color"),
		}, nil
	case "packer.build", "packer.validate":
		varFile, err := writeVars(in.JobDir, "vars.pkrvars.json", vars)
		if err != nil {
			return nil, err
		}
		args := []string{a.Op, "-color=false"}
		if a.Op == "build" {
			args = append(args, "-timestamp-ui")
		}
		return []Step{
			step("init", "."),
			step(append(append(args, varFile...), ".")...),
		}, nil
	case "command.run":
		argv, err := a.RenderCommand(in.Params)
		if err != nil {
			return nil, err
		}
		return []Step{{Argv: argv, Dir: dir}}, nil
	case "ansible.playbook", "ansible.check":
		rendered, err := a.Render("args", in.Params)
		if err != nil {
			return nil, err
		}
		args := []string{"-i", a.Inventory}
		for _, k := range sortedKeys(rendered) {
			// --flag=value keeps a value starting with "-" from being parsed as an option.
			args = append(args, config.AnsibleArgs[k]+"="+rendered[k])
		}
		if len(vars) > 0 {
			f, err := writeVarsFile(in.JobDir, "extra-vars.json", vars)
			if err != nil {
				return nil, err
			}
			args = append(args, "--extra-vars=@"+f)
		}
		if a.Op == "check" {
			args = append(args, "--check", "--diff")
		}
		return []Step{step(append(args, a.Playbook)...)}, nil
	}
	return nil, fmt.Errorf("unsupported %s %s", a.Tool, a.Op)
}

// writeVars returns the -var-file argument, or nothing when there are no vars.
func writeVars(jobDir, name string, vars map[string]string) ([]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	f, err := writeVarsFile(jobDir, name, vars)
	if err != nil {
		return nil, err
	}
	return []string{"-var-file=" + f}, nil
}

func writeVarsFile(jobDir, name string, vars map[string]string) (string, error) {
	b, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return "", err
	}
	f := filepath.Join(jobDir, name)
	return f, os.WriteFile(f, b, 0o640)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
