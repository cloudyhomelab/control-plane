// Package config loads and validates the action catalog.
package config

import (
	"fmt"
	"os"
	"path"
	"sort"
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

	templates map[string]*template.Template
}

// Ops lists the operations each tool supports.
var Ops = map[string][]string{
	"terraform": {"plan", "apply", "validate"},
	"packer":    {"build", "validate"},
	"ansible":   {"playbook", "check"},
}

// AnsibleArgs maps the allowed ansible `args` keys to their flags.
var AnsibleArgs = map[string]string{
	"limit":     "--limit",
	"tags":      "--tags",
	"skip_tags": "--skip-tags",
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

func Load(file string) (*Config, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	s := &c.Server
	if s.OIDCIssuer == "" {
		s.OIDCIssuer = GitHubIssuer
	}
	if s.OIDCJWKSURL == "" {
		s.OIDCJWKSURL = strings.TrimSuffix(s.OIDCIssuer, "/") + "/.well-known/jwks"
	}
	if s.OIDCAudience == "" {
		return fmt.Errorf("server.oidc_audience is required")
	}
	if s.AllowedOrg == "" {
		return fmt.Errorf("server.allowed_org is required")
	}
	if s.MaxConcurrentJobs <= 0 {
		s.MaxConcurrentJobs = 4
	}
	if s.PlanMaxAge.Duration == 0 {
		s.PlanMaxAge.Duration = 24 * time.Hour
	}
	if s.CancelGrace.Duration == 0 {
		s.CancelGrace.Duration = 60 * time.Second
	}
	for i, r := range s.Admins {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("server.admins[%d]: %w", i, err)
		}
	}
	if c.Tools == nil {
		c.Tools = map[string]Tool{}
	}
	for tool := range Ops {
		if c.Tools[tool].Path == "" {
			c.Tools[tool] = Tool{Path: defaultBinary(tool)}
		}
	}
	for name, r := range c.Repos {
		if r.URL == "" {
			return fmt.Errorf("repos.%s: url is required", name)
		}
		if r.DefaultRef == "" {
			r.DefaultRef = "refs/heads/main"
		}
	}
	// Plan actions first, so apply actions can inherit from validated plans.
	for _, name := range c.actionNames() {
		a := c.Actions[name]
		a.Name = name
		if a.FromPlan == "" {
			if err := c.validateAction(a); err != nil {
				return fmt.Errorf("actions.%s: %w", name, err)
			}
		}
	}
	for _, name := range c.actionNames() {
		if a := c.Actions[name]; a.FromPlan != "" {
			if err := c.validateApply(a); err != nil {
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

func (c *Config) actionNames() []string {
	names := make([]string, 0, len(c.Actions))
	for n := range c.Actions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (c *Config) validateAction(a *Action) error {
	ops, ok := Ops[a.Tool]
	if !ok {
		return fmt.Errorf("unknown tool %q", a.Tool)
	}
	if !contains(ops, a.Op) {
		return fmt.Errorf("tool %s has no op %q (have %v)", a.Tool, a.Op, ops)
	}
	if a.Tool == "terraform" && a.Op == "apply" {
		return fmt.Errorf("terraform apply must use from_plan")
	}
	if _, ok := c.Repos[a.Repo]; !ok {
		return fmt.Errorf("unknown repo %q", a.Repo)
	}
	if a.EnvProfile != "" {
		if _, ok := c.EnvProfiles[a.EnvProfile]; !ok {
			return fmt.Errorf("unknown env_profile %q", a.EnvProfile)
		}
	}
	if err := checkRelPath("dir", a.Dir, true); err != nil {
		return err
	}
	if a.Tool == "ansible" {
		if err := checkRelPath("playbook", a.Playbook, false); err != nil {
			return err
		}
		if err := checkRelPath("inventory", a.Inventory, false); err != nil {
			return err
		}
		for k := range a.Args {
			if _, ok := AnsibleArgs[k]; !ok {
				return fmt.Errorf("args: unsupported key %q", k)
			}
		}
	} else if a.Playbook != "" || a.Inventory != "" || len(a.Args) > 0 {
		return fmt.Errorf("playbook, inventory and args are ansible only")
	}
	if len(a.AllowedRefs) == 0 {
		return fmt.Errorf("allowed_refs is required")
	}
	if err := validateRules(a.Allow); err != nil {
		return err
	}
	if a.Timeout.Duration == 0 {
		a.Timeout.Duration = time.Hour
	}
	if a.Params == nil {
		a.Params = params.Schema{}
	}
	for name, spec := range a.Params {
		if err := spec.Compile(); err != nil {
			return fmt.Errorf("params.%s: %w", name, err)
		}
	}
	return a.compileTemplates()
}

func (c *Config) validateApply(a *Action) error {
	plan, ok := c.Actions[a.FromPlan]
	if !ok {
		return fmt.Errorf("from_plan: unknown action %q", a.FromPlan)
	}
	if plan.Tool != "terraform" || plan.Op != "plan" {
		return fmt.Errorf("from_plan must name a terraform plan action")
	}
	if a.Tool == "" {
		a.Tool = "terraform"
	}
	if a.Op == "" {
		a.Op = "apply"
	}
	if a.Tool != "terraform" || a.Op != "apply" {
		return fmt.Errorf("from_plan is only valid for terraform apply")
	}
	if a.Repo != "" || a.Dir != "" || a.EnvProfile != "" || len(a.Params) > 0 || len(a.Vars) > 0 {
		return fmt.Errorf("repo, dir, env_profile, params and vars are inherited from the plan action")
	}
	a.Repo, a.Dir, a.EnvProfile = plan.Repo, plan.Dir, plan.EnvProfile
	// Vars are baked into the saved plan, so apply renders none.
	a.Params, a.templates = params.Schema{}, map[string]*template.Template{}
	if a.Lock == "" {
		a.Lock = plan.Lock
	}
	if a.Timeout.Duration == 0 {
		a.Timeout.Duration = time.Hour
	}
	if len(a.AllowedRefs) == 0 {
		return fmt.Errorf("allowed_refs is required")
	}
	if a.RequireRefMatch == nil {
		t := true
		a.RequireRefMatch = &t
	}
	return validateRules(a.Allow)
}

func validateRules(rules []policy.Rule) error {
	if len(rules) == 0 {
		return fmt.Errorf("allow must have at least one rule")
	}
	for i, r := range rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("allow[%d]: %w", i, err)
		}
	}
	return nil
}

func checkRelPath(field, p string, allowEmpty bool) error {
	if p == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%s is required", field)
	}
	if path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("%s must be a clean relative path inside the repo", field)
	}
	return nil
}

// compileTemplates parses vars and args, and checks they only reference declared params.
func (a *Action) compileTemplates() error {
	a.templates = map[string]*template.Template{}
	probe := map[string]string{}
	for name := range a.Params {
		probe[name] = ""
	}
	compile := func(kind string, m map[string]string) error {
		for k, v := range m {
			t, err := template.New(kind + "." + k).Option("missingkey=error").Parse(v)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", kind, k, err)
			}
			if err := t.Execute(&strings.Builder{}, probe); err != nil {
				return fmt.Errorf("%s.%s: %w", kind, k, err)
			}
			a.templates[kind+"."+k] = t
		}
		return nil
	}
	if err := compile("vars", a.Vars); err != nil {
		return err
	}
	return compile("args", a.Args)
}

// Render evaluates vars or args ("vars" or "args") with validated parameter values.
func (a *Action) Render(kind string, values map[string]string) (map[string]string, error) {
	src := a.Vars
	if kind == "args" {
		src = a.Args
	}
	out := make(map[string]string, len(src))
	for k := range src {
		var b strings.Builder
		if err := a.templates[kind+"."+k].Execute(&b, values); err != nil {
			return nil, err
		}
		out[k] = b.String()
	}
	return out, nil
}

func (a *Action) RefMatchRequired() bool {
	return a.RequireRefMatch != nil && *a.RequireRefMatch
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
