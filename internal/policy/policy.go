// Package policy decides whether a verified caller may invoke an action.
package policy

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Claims are the string claims of a verified GitHub Actions OIDC token.
type Claims map[string]string

// KnownClaims are the claim names a rule may reference.
var KnownClaims = map[string]bool{
	"repository": true, "repository_owner": true, "ref": true, "ref_type": true,
	"event_name": true, "environment": true, "workflow_ref": true, "job_workflow_ref": true,
	"actor": true, "sub": true,
}

// Patterns accepts a scalar or a list in YAML. `*` matches any run of characters, including `/`.
type Patterns []string

func (p *Patterns) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*p = Patterns{n.Value}
		return nil
	case yaml.SequenceNode:
		var s []string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*p = s
		return nil
	}
	return fmt.Errorf("line %d: expected string or list", n.Line)
}

func (p Patterns) Match(v string) bool {
	for _, pat := range p {
		if Glob(pat, v) {
			return true
		}
	}
	return false
}

func Glob(pattern, v string) bool {
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(v)
}

// Rule matches when every claim it names matches one of its patterns.
type Rule map[string]Patterns

func (r Rule) Validate() error {
	if len(r) == 0 {
		return fmt.Errorf("empty rule would match everyone")
	}
	for k := range r {
		if !KnownClaims[k] {
			return fmt.Errorf("unknown claim %q", k)
		}
	}
	return nil
}

func (r Rule) Match(c Claims) bool {
	for k, pats := range r {
		v, ok := c[k]
		if !ok || !pats.Match(v) {
			return false
		}
	}
	return true
}

// Allowed reports whether any rule matches. No rules means nobody is allowed.
func Allowed(rules []Rule, c Claims) bool {
	for _, r := range rules {
		if r.Match(c) {
			return true
		}
	}
	return false
}
