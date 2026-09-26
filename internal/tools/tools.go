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
func Prepare(input Input) ([]Step, error) {
	action := input.Action
	dir := filepath.Join(input.SrcDir, action.Dir)
	if input.SrcDir == "" {
		dir = input.JobDir
	}
	vars, err := action.Render("vars", input.Params)
	if err != nil {
		return nil, err
	}
	bin := input.ToolPath
	step := func(args ...string) Step { return Step{Argv: append([]string{bin}, args...), Dir: dir} }

	switch action.Tool + "." + action.Op {
	case "terraform.plan":
		varFile, err := writeVars(input.JobDir, "vars.tfvars.json", vars)
		if err != nil {
			return nil, err
		}
		plan := filepath.Join(input.JobDir, PlanFile)
		show := step("show", "-json", "-no-color", plan)
		show.StdoutFile = filepath.Join(input.JobDir, PlanJSONFile)
		return []Step{
			step("init", "-input=false", "-no-color"),
			step(append([]string{"plan", "-input=false", "-no-color", "-out=" + plan}, varFile...)...),
			show,
		}, nil
	case "terraform.apply":
		if input.PlanFile == "" {
			return nil, fmt.Errorf("apply needs a plan file")
		}
		return []Step{
			step("init", "-input=false", "-no-color"),
			step("apply", "-input=false", "-no-color", input.PlanFile),
		}, nil
	case "terraform.validate":
		return []Step{
			step("init", "-input=false", "-no-color", "-backend=false"),
			step("validate", "-no-color"),
		}, nil
	case "packer.build", "packer.validate":
		varFile, err := writeVars(input.JobDir, "vars.pkrvars.json", vars)
		if err != nil {
			return nil, err
		}
		args := []string{action.Op, "-color=false"}
		if action.Op == "build" {
			args = append(args, "-timestamp-ui")
		}
		return []Step{
			step("init", "."),
			step(append(append(args, varFile...), ".")...),
		}, nil
	case "command.run":
		argv, err := action.RenderCommand(input.Params)
		if err != nil {
			return nil, err
		}
		return []Step{{Argv: argv, Dir: dir}}, nil
	case "ansible.playbook", "ansible.check":
		rendered, err := action.Render("args", input.Params)
		if err != nil {
			return nil, err
		}
		args := []string{"-i", action.Inventory}
		for _, key := range sortedKeys(rendered) {
			// --flag=value keeps a value starting with "-" from being parsed as an option.
			args = append(args, config.AnsibleArgs[key]+"="+rendered[key])
		}
		if len(vars) > 0 {
			varsFile, err := writeVarsFile(input.JobDir, "extra-vars.json", vars)
			if err != nil {
				return nil, err
			}
			args = append(args, "--extra-vars=@"+varsFile)
		}
		if action.Op == "check" {
			args = append(args, "--check", "--diff")
		}
		return []Step{step(append(args, action.Playbook)...)}, nil
	}
	return nil, fmt.Errorf("unsupported %s %s", action.Tool, action.Op)
}

// writeVars returns the -var-file argument, or nothing when there are no vars.
func writeVars(jobDir, name string, vars map[string]string) ([]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	path, err := writeVarsFile(jobDir, name, vars)
	if err != nil {
		return nil, err
	}
	return []string{"-var-file=" + path}, nil
}

func writeVarsFile(jobDir, name string, vars map[string]string) (string, error) {
	encoded, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(jobDir, name)
	return path, os.WriteFile(path, encoded, 0o640)
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
