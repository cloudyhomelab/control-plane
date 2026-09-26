// Package params validates caller-supplied action parameters against a typed schema.
package params

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

type Spec struct {
	Type    string   `yaml:"type" json:"type"`
	Values  []string `yaml:"values,omitempty" json:"values,omitempty"`
	Pattern string   `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	Min     *int     `yaml:"min,omitempty" json:"min,omitempty"`
	Max     *int     `yaml:"max,omitempty" json:"max,omitempty"`
	Default *string  `yaml:"default,omitempty" json:"default,omitempty"`

	re *regexp.Regexp
}

// Compile checks the spec and prepares it for validation.
func (s *Spec) Compile() error {
	switch s.Type {
	case "enum":
		if len(s.Values) == 0 {
			return fmt.Errorf("enum needs values")
		}
	case "string":
		// Free-form strings are not allowed: a pattern bounds what a caller can inject.
		if s.Pattern == "" {
			return fmt.Errorf("string needs a pattern")
		}
		re, err := regexp.Compile(`^(?:` + s.Pattern + `)$`)
		if err != nil {
			return fmt.Errorf("pattern: %w", err)
		}
		s.re = re
	case "int":
		if s.Min != nil && s.Max != nil && *s.Min > *s.Max {
			return fmt.Errorf("min > max")
		}
	case "bool":
	default:
		return fmt.Errorf("unknown type %q", s.Type)
	}
	if s.Default != nil {
		if _, err := s.check(*s.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

func (s *Spec) check(v string) (string, error) {
	switch s.Type {
	case "enum":
		for _, allowed := range s.Values {
			if v == allowed {
				return v, nil
			}
		}
		return "", fmt.Errorf("must be one of %v", s.Values)
	case "string":
		if !s.re.MatchString(v) {
			return "", fmt.Errorf("must match %s", s.Pattern)
		}
		return v, nil
	case "int":
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("must be an integer")
		}
		if s.Min != nil && n < *s.Min {
			return "", fmt.Errorf("must be >= %d", *s.Min)
		}
		if s.Max != nil && n > *s.Max {
			return "", fmt.Errorf("must be <= %d", *s.Max)
		}
		return strconv.Itoa(n), nil
	case "bool":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return "", fmt.Errorf("must be a boolean")
		}
		return strconv.FormatBool(b), nil
	}
	return "", fmt.Errorf("unknown type %q", s.Type)
}

type Schema map[string]*Spec

// Validate returns normalized values for every declared parameter.
func (sc Schema) Validate(in map[string]string) (map[string]string, error) {
	for k := range in {
		if _, ok := sc[k]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", k)
		}
	}
	out := make(map[string]string, len(sc))
	for _, name := range sc.Names() {
		spec := sc[name]
		v, ok := in[name]
		if !ok {
			if spec.Default == nil {
				return nil, fmt.Errorf("missing parameter %q", name)
			}
			v = *spec.Default
		}
		nv, err := spec.check(v)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		out[name] = nv
	}
	return out, nil
}

func (sc Schema) Names() []string {
	names := make([]string, 0, len(sc))
	for k := range sc {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
