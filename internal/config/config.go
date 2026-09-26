// Package config loads and validates the action catalog.
package config

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cloudyhomelab/control-plane/internal/params"
	"github.com/cloudyhomelab/control-plane/internal/policy"
)

const GitHubIssuer = "https://token.actions.githubusercontent.com"

type Config struct {
	Server      Server                `yaml:"server"`
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
	CancelGrace       Duration      `yaml:"cancel_grace"`
	Admins            []policy.Rule `yaml:"admins"`
	// GitHubAPIURL is where workflow run approvals are read, for actions with approvers.
	GitHubAPIURL string `yaml:"github_api_url"`
	// GitHubTokenEnv names the server environment variable holding a GitHub token for that.
	// Optional for public repositories; private ones need a token that can read Actions.
	GitHubTokenEnv string `yaml:"github_token_env"`
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
	Name        string          `yaml:"-"`
	Description string          `yaml:"description"`
	Repo        string          `yaml:"repo"`
	Dir         string          `yaml:"dir"`
	EnvProfile  string          `yaml:"env_profile"`
	AllowedRefs policy.Patterns `yaml:"allowed_refs"`
	Lock        string          `yaml:"lock"`
	Timeout     Duration        `yaml:"timeout"`
	Params      params.Schema   `yaml:"params"`
	// Steps are argv lists run in order. Elements may be templates, each filling exactly one argv slot.
	Steps [][]string `yaml:"steps"`
	// InputFrom names the action whose job output this action reads, via {{ .input_dir }}.
	InputFrom string `yaml:"input_from"`
	// RequireRefMatch forces the token's ref to equal the job's ref.
	RequireRefMatch bool          `yaml:"require_ref_match"`
	Allow           []policy.Rule `yaml:"allow"`
	// Approvers are GitHub logins. If set, the calling job must run in a GitHub environment,
	// and one of them must have approved that environment in the caller's workflow run.
	Approvers []string `yaml:"approvers"`

	templates [][]*template.Template
}

// Template values the server provides alongside the action's params.
const (
	JobDirKey   = "job_dir"
	InputDirKey = "input_dir"
)

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
	if server.CancelGrace.Duration == 0 {
		server.CancelGrace.Duration = 60 * time.Second
	}
	if server.GitHubAPIURL == "" {
		server.GitHubAPIURL = "https://api.github.com"
	}
	for index, rule := range server.Admins {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("server.admins[%d]: %w", index, err)
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
	// Inherit from input actions before validating, so inherited settings are checked too.
	for _, name := range cfg.actionNames() {
		action := cfg.Actions[name]
		action.Name = name
		if action.InputFrom != "" {
			if err := cfg.inherit(action); err != nil {
				return fmt.Errorf("actions.%s: %w", name, err)
			}
		}
	}
	for _, name := range cfg.actionNames() {
		if err := cfg.validateAction(cfg.Actions[name]); err != nil {
			return fmt.Errorf("actions.%s: %w", name, err)
		}
	}
	return nil
}

func (cfg *Config) actionNames() []string {
	names := make([]string, 0, len(cfg.Actions))
	for name := range cfg.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// inherit fills an action from the action it reads input from. The repo and params always
// come from the input action, because the job runs at the input job's commit with its values.
func (cfg *Config) inherit(action *Action) error {
	input, ok := cfg.Actions[action.InputFrom]
	if !ok {
		return fmt.Errorf("input_from: unknown action %q", action.InputFrom)
	}
	if input.InputFrom != "" {
		return fmt.Errorf("input_from: %s itself reads input; chains are not supported", action.InputFrom)
	}
	if action.Repo != "" || len(action.Params) > 0 {
		return fmt.Errorf("repo and params are inherited from %s", action.InputFrom)
	}
	action.Repo, action.Params = input.Repo, input.Params
	if action.Dir == "" {
		action.Dir = input.Dir
	}
	if action.EnvProfile == "" {
		action.EnvProfile = input.EnvProfile
	}
	if action.Lock == "" {
		action.Lock = input.Lock
	}
	return nil
}

func (cfg *Config) validateAction(action *Action) error {
	if action.Repo == "" {
		// Without a repo the steps run in an empty job dir, so refs do not apply.
		if action.Dir != "" || len(action.AllowedRefs) > 0 || action.RequireRefMatch {
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
	if err := checkRelPath("dir", action.Dir); err != nil {
		return err
	}
	if len(action.Steps) == 0 {
		return fmt.Errorf("steps is required")
	}
	for index, step := range action.Steps {
		// An absolute, literal binary means no PATH lookup and no parameter choosing what runs.
		if len(step) == 0 || !path.IsAbs(step[0]) || strings.Contains(step[0], "{{") {
			return fmt.Errorf("steps[%d]: first element must be a literal absolute path", index)
		}
	}
	if err := validateRules(action.Allow); err != nil {
		return err
	}
	for _, login := range action.Approvers {
		if !loginPattern.MatchString(login) {
			return fmt.Errorf("approvers: %q is not a GitHub login", login)
		}
	}
	if action.Timeout.Duration == 0 {
		action.Timeout.Duration = time.Hour
	}
	if action.Params == nil {
		action.Params = params.Schema{}
	}
	for name, spec := range action.Params {
		if name == JobDirKey || name == InputDirKey {
			return fmt.Errorf("params.%s: name is reserved", name)
		}
		if err := spec.Compile(); err != nil {
			return fmt.Errorf("params.%s: %w", name, err)
		}
	}
	return action.compileTemplates()
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

var loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

// IsApprover reports whether login may approve this action's jobs. GitHub logins are case-insensitive.
func (action *Action) IsApprover(login string) bool {
	for _, approver := range action.Approvers {
		if strings.EqualFold(approver, login) {
			return true
		}
	}
	return false
}

func checkRelPath(field, relPath string) error {
	if relPath == "" {
		return nil
	}
	if path.IsAbs(relPath) || path.Clean(relPath) != relPath || relPath == ".." || strings.HasPrefix(relPath, "../") {
		return fmt.Errorf("%s must be a clean relative path inside the repo", field)
	}
	return nil
}

// compileTemplates parses every step element and checks it only references declared params
// and the server-provided values.
func (action *Action) compileTemplates() error {
	probe := map[string]string{JobDirKey: ""}
	if action.InputFrom != "" {
		probe[InputDirKey] = ""
	}
	for name := range action.Params {
		probe[name] = ""
	}
	action.templates = make([][]*template.Template, len(action.Steps))
	for stepIndex, step := range action.Steps {
		for elementIndex, element := range step {
			label := fmt.Sprintf("steps[%d][%d]", stepIndex, elementIndex)
			parsed, err := template.New(label).Option("missingkey=error").Parse(element)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			if err := parsed.Execute(&strings.Builder{}, probe); err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			action.templates[stepIndex] = append(action.templates[stepIndex], parsed)
		}
	}
	return nil
}

// RenderSteps returns the argv of every step. values holds the validated params plus
// JobDirKey and, for actions with input_from, InputDirKey.
func (action *Action) RenderSteps(values map[string]string) ([][]string, error) {
	steps := make([][]string, len(action.templates))
	for stepIndex, step := range action.templates {
		for _, parsed := range step {
			var rendered strings.Builder
			if err := parsed.Execute(&rendered, values); err != nil {
				return nil, err
			}
			steps[stepIndex] = append(steps[stepIndex], rendered.String())
		}
	}
	return steps, nil
}
