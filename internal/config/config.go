// Package config loads and validates the action catalog.
package config

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cloudyhome/controlplane/internal/params"
	"github.com/cloudyhome/controlplane/internal/policy"
)

const GitHubIssuer = "https://token.actions.githubusercontent.com"

type Config struct {
	Server      Server                `yaml:"server"`
	Tools       map[string]Tool       `yaml:"tools"`
	Repos       map[string]*Repo      `yaml:"repos"`
	EnvProfiles map[string]EnvProfile `yaml:"env_profiles"`
	Actions     map[string]*Action    `yaml:"actions"`
}

type Server struct {
	OIDCIssuer        string        `yaml:"oidc_issuer"`
	OIDCJWKSURL       string        `yaml:"oidc_jwks_url"`
	OIDCAudience      string        `yaml:"oidc_audience"`
	AllowedOrg        string        `yaml:"allowed_org"`
	MaxConcurrentJobs int           `yaml:"max_concurrent_jobs"`
	PlanMaxAge        Duration      `yaml:"plan_max_age"`
	CancelGrace       Duration      `yaml:"cancel_grace"`
	Admins            []policy.Rule `yaml:"admins"`
}

type Tool struct {
	Path string `yaml:"path"`
}

type Repo struct {
	URL            string `yaml:"url"`
	DeployKeyFile  string `yaml:"deploy_key_file"`
	KnownHostsFile string `yaml:"known_hosts_file"`
	DefaultRef     string `yaml:"default_ref"`
}

type EnvProfile struct {
	// PassThrough copies these variables from the server environment.
	PassThrough []string `yaml:"pass_through"`
	// Secrets are passed through like PassThrough, and their values are scrubbed from logs.
	Secrets []string          `yaml:"secrets"`
	Set     map[string]string `yaml:"set"`
}

type Action struct {
	Name        string            `yaml:"-"`
	Description string            `yaml:"description"`
	Tool        string            `yaml:"tool"`
	Op          string            `yaml:"op"`
	Repo        string            `yaml:"repo"`
	Dir         string            `yaml:"dir"`
	EnvProfile  string            `yaml:"env_profile"`
	AllowedRefs policy.Patterns   `yaml:"allowed_refs"`
	Lock        string            `yaml:"lock"`
	Timeout     Duration          `yaml:"timeout"`
	Params      params.Schema     `yaml:"params"`
	Vars        map[string]string `yaml:"vars"`
	FromPlan    string            `yaml:"from_plan"`
	// RequireRefMatch forces the token's ref to equal the requested ref. Defaults to true for apply.
	RequireRefMatch *bool         `yaml:"require_ref_match"`
	Allow           []policy.Rule `yaml:"allow"`

	// Ansible only.
	Playbook  string            `yaml:"playbook"`
	Inventory string            `yaml:"inventory"`
	Args      map[string]string `yaml:"args"`

	// Command only: fixed argv. Elements may be templates, each filling exactly one argv slot.
	Command []string `yaml:"command"`

	templates map[string]*template.Template
}

// Ops lists the operations each tool supports.
var Ops = map[string][]string{
	"terraform": {"plan", "apply", "validate"},
	"packer":    {"build", "validate"},
	"ansible":   {"playbook", "check"},
	"command":   {"run"},
}

// AnsibleArgs maps the allowed ansible `args` keys to their flags.
var AnsibleArgs = map[string]string{
	"limit":     "--limit",
	"tags":      "--tags",
	"skip_tags": "--skip-tags",
}

type Duration struct{ time.Duration }

func (duration *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	duration.Duration = parsed
	return nil
}

func Load(file string) (*Config, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return Parse(content)
}

func Parse(content []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(content)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (cfg *Config) validate() error {
	server := &cfg.Server
	if server.OIDCIssuer == "" {
		server.OIDCIssuer = GitHubIssuer
	}
	if server.OIDCJWKSURL == "" {
		server.OIDCJWKSURL = strings.TrimSuffix(server.OIDCIssuer, "/") + "/.well-known/jwks"
	}
	if server.OIDCAudience == "" {
		return fmt.Errorf("server.oidc_audience is required")
	}
	if server.AllowedOrg == "" {
		return fmt.Errorf("server.allowed_org is required")
	}
	if server.MaxConcurrentJobs <= 0 {
		server.MaxConcurrentJobs = 4
	}
	if server.PlanMaxAge.Duration == 0 {
		server.PlanMaxAge.Duration = 24 * time.Hour
	}
	if server.CancelGrace.Duration == 0 {
		server.CancelGrace.Duration = 60 * time.Second
	}
	for index, rule := range server.Admins {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("server.admins[%d]: %w", index, err)
		}
	}
	if cfg.Tools == nil {
		cfg.Tools = map[string]Tool{}
	}
	for tool := range Ops {
		if tool != "command" && cfg.Tools[tool].Path == "" {
			cfg.Tools[tool] = Tool{Path: defaultBinary(tool)}
		}
	}
	for name, repo := range cfg.Repos {
		if repo.URL == "" {
			return fmt.Errorf("repos.%s: url is required", name)
		}
		if repo.DefaultRef == "" {
			repo.DefaultRef = "refs/heads/main"
		}
	}
	// Plan actions first, so apply actions can inherit from validated plans.
	for _, name := range cfg.actionNames() {
		action := cfg.Actions[name]
		action.Name = name
		if action.FromPlan == "" {
			if err := cfg.validateAction(action); err != nil {
				return fmt.Errorf("actions.%s: %w", name, err)
			}
		}
	}
	for _, name := range cfg.actionNames() {
		if action := cfg.Actions[name]; action.FromPlan != "" {
			if err := cfg.validateApply(action); err != nil {
				return fmt.Errorf("actions.%s: %w", name, err)
			}
		}
	}
	return nil
}

func defaultBinary(tool string) string {
	if tool == "ansible" {
		return "ansible-playbook"
	}
	return tool
}

func (cfg *Config) actionNames() []string {
	names := make([]string, 0, len(cfg.Actions))
	for name := range cfg.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (cfg *Config) validateAction(action *Action) error {
	if action.Tool == "command" && action.Op == "" {
		action.Op = "run"
	}
	ops, ok := Ops[action.Tool]
	if !ok {
		return fmt.Errorf("unknown tool %q", action.Tool)
	}
	if !contains(ops, action.Op) {
		return fmt.Errorf("tool %s has no op %q (have %v)", action.Tool, action.Op, ops)
	}
	if action.Tool == "terraform" && action.Op == "apply" {
		return fmt.Errorf("terraform apply must use from_plan")
	}
	if action.Repo == "" && action.Tool == "command" {
		// A command without a repo runs in an empty job dir; refs do not apply.
		if action.Dir != "" || len(action.AllowedRefs) > 0 || action.RefMatchRequired() {
			return fmt.Errorf("dir, allowed_refs and require_ref_match need a repo")
		}
	} else if _, ok := cfg.Repos[action.Repo]; !ok {
		return fmt.Errorf("unknown repo %q", action.Repo)
	} else if len(action.AllowedRefs) == 0 {
		return fmt.Errorf("allowed_refs is required")
	}
	if action.EnvProfile != "" {
		if _, ok := cfg.EnvProfiles[action.EnvProfile]; !ok {
			return fmt.Errorf("unknown env_profile %q", action.EnvProfile)
		}
	}
	if err := checkRelPath("dir", action.Dir, true); err != nil {
		return err
	}
	if action.Tool == "ansible" {
		if err := checkRelPath("playbook", action.Playbook, false); err != nil {
			return err
		}
		if err := checkRelPath("inventory", action.Inventory, false); err != nil {
			return err
		}
		for key := range action.Args {
			if _, ok := AnsibleArgs[key]; !ok {
				return fmt.Errorf("args: unsupported key %q", key)
			}
		}
	} else if action.Playbook != "" || action.Inventory != "" || len(action.Args) > 0 {
		return fmt.Errorf("playbook, inventory and args are ansible only")
	}
	if action.Tool == "command" {
		if len(action.Command) == 0 {
			return fmt.Errorf("command is required")
		}
		// An absolute, literal binary means no PATH lookup and no parameter choosing what runs.
		if !path.IsAbs(action.Command[0]) || strings.Contains(action.Command[0], "{{") {
			return fmt.Errorf("command[0] must be a literal absolute path")
		}
		if len(action.Vars) > 0 {
			return fmt.Errorf("vars are not supported for command; use templates in command")
		}
	} else if len(action.Command) > 0 {
		return fmt.Errorf("command is only valid for tool command")
	}
	if err := validateRules(action.Allow); err != nil {
		return err
	}
	if action.Timeout.Duration == 0 {
		action.Timeout.Duration = time.Hour
	}
	if action.Params == nil {
		action.Params = params.Schema{}
	}
	for name, spec := range action.Params {
		if err := spec.Compile(); err != nil {
			return fmt.Errorf("params.%s: %w", name, err)
		}
	}
	return action.compileTemplates()
}

func (cfg *Config) validateApply(action *Action) error {
	plan, ok := cfg.Actions[action.FromPlan]
	if !ok {
		return fmt.Errorf("from_plan: unknown action %q", action.FromPlan)
	}
	if plan.Tool != "terraform" || plan.Op != "plan" {
		return fmt.Errorf("from_plan must name a terraform plan action")
	}
	if action.Tool == "" {
		action.Tool = "terraform"
	}
	if action.Op == "" {
		action.Op = "apply"
	}
	if action.Tool != "terraform" || action.Op != "apply" {
		return fmt.Errorf("from_plan is only valid for terraform apply")
	}
	if action.Repo != "" || action.Dir != "" || action.EnvProfile != "" || len(action.Params) > 0 || len(action.Vars) > 0 {
		return fmt.Errorf("repo, dir, env_profile, params and vars are inherited from the plan action")
	}
	action.Repo, action.Dir, action.EnvProfile = plan.Repo, plan.Dir, plan.EnvProfile
	// Vars are baked into the saved plan, so apply renders none.
	action.Params, action.templates = params.Schema{}, map[string]*template.Template{}
	if action.Lock == "" {
		action.Lock = plan.Lock
	}
	if action.Timeout.Duration == 0 {
		action.Timeout.Duration = time.Hour
	}
	if len(action.AllowedRefs) == 0 {
		return fmt.Errorf("allowed_refs is required")
	}
	if action.RequireRefMatch == nil {
		required := true
		action.RequireRefMatch = &required
	}
	return validateRules(action.Allow)
}

func validateRules(rules []policy.Rule) error {
	if len(rules) == 0 {
		return fmt.Errorf("allow must have at least one rule")
	}
	for index, rule := range rules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("allow[%d]: %w", index, err)
		}
	}
	return nil
}

func checkRelPath(field, relPath string, allowEmpty bool) error {
	if relPath == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%s is required", field)
	}
	if path.IsAbs(relPath) || path.Clean(relPath) != relPath || relPath == ".." || strings.HasPrefix(relPath, "../") {
		return fmt.Errorf("%s must be a clean relative path inside the repo", field)
	}
	return nil
}

// compileTemplates parses vars and args, and checks they only reference declared params.
func (action *Action) compileTemplates() error {
	action.templates = map[string]*template.Template{}
	probe := map[string]string{}
	for name := range action.Params {
		probe[name] = ""
	}
	compile := func(kind string, sources map[string]string) error {
		for key, text := range sources {
			parsed, err := template.New(kind + "." + key).Option("missingkey=error").Parse(text)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", kind, key, err)
			}
			if err := parsed.Execute(&strings.Builder{}, probe); err != nil {
				return fmt.Errorf("%s.%s: %w", kind, key, err)
			}
			action.templates[kind+"."+key] = parsed
		}
		return nil
	}
	if err := compile("vars", action.Vars); err != nil {
		return err
	}
	if err := compile("args", action.Args); err != nil {
		return err
	}
	cmd := map[string]string{}
	for index, element := range action.Command {
		cmd[strconv.Itoa(index)] = element
	}
	return compile("command", cmd)
}

// RenderCommand returns the argv of a command action with validated parameter values.
func (action *Action) RenderCommand(values map[string]string) ([]string, error) {
	argv := make([]string, len(action.Command))
	for index := range action.Command {
		var rendered strings.Builder
		if err := action.templates["command."+strconv.Itoa(index)].Execute(&rendered, values); err != nil {
			return nil, err
		}
		argv[index] = rendered.String()
	}
	return argv, nil
}

// Render evaluates vars or args ("vars" or "args") with validated parameter values.
func (action *Action) Render(kind string, values map[string]string) (map[string]string, error) {
	src := action.Vars
	if kind == "args" {
		src = action.Args
	}
	out := make(map[string]string, len(src))
	for key := range src {
		var rendered strings.Builder
		if err := action.templates[kind+"."+key].Execute(&rendered, values); err != nil {
			return nil, err
		}
		out[key] = rendered.String()
	}
	return out, nil
}

func (action *Action) RefMatchRequired() bool {
	return action.RequireRefMatch != nil && *action.RequireRefMatch
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
