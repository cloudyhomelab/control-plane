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
	"repository":       true,
	"repository_owner": true,
	"ref":              true,
	"ref_type":         true,
	"event_name":       true,
	"environment":      true,
	"workflow_ref":     true,
	"job_workflow_ref": true,
	"actor":            true,
	"sub":              true,
}

// Patterns accepts a scalar or a list in YAML. `*` matches any run of characters, including `/`.
type Patterns []string

func (patterns *Patterns) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*patterns = Patterns{node.Value}
		return nil
	case yaml.SequenceNode:
		var values []string
		if err := node.Decode(&values); err != nil {
			return err
		}
		*patterns = values
		return nil
	}
	return fmt.Errorf("line %d: expected string or list", node.Line)
}

func (patterns Patterns) Match(value string) bool {
	for _, pat := range patterns {
		if Glob(pat, value) {
			return true
		}
	}
	return false
}

func Glob(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	for index := range parts {
		parts[index] = regexp.QuoteMeta(parts[index])
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(value)
}

// Rule matches when every claim it names matches one of its patterns.
type Rule map[string]Patterns

func (rule Rule) Validate() error {
	if len(rule) == 0 {
		return fmt.Errorf("empty rule would match everyone")
	}
	for claim := range rule {
		if !KnownClaims[claim] {
			return fmt.Errorf("unknown claim %q", claim)
		}
	}
	return nil
}

func (rule Rule) Match(claims Claims) bool {
	for claim, patterns := range rule {
		value, ok := claims[claim]
		if !ok || !patterns.Match(value) {
			return false
		}
	}
	return true
}

// Allowed reports whether any rule matches. No rules means nobody is allowed.
func Allowed(rules []Rule, claims Claims) bool {
	for _, rule := range rules {
		if rule.Match(claims) {
			return true
		}
	}
	return false
}
