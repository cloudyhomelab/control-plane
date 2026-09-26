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

	regex *regexp.Regexp
}

// Compile checks the spec and prepares it for validation.
func (spec *Spec) Compile() error {
	switch spec.Type {
	case "enum":
		if len(spec.Values) == 0 {
			return fmt.Errorf("enum needs values")
		}
	case "string":
		// Free-form strings are not allowed: a pattern bounds what a caller can inject.
		if spec.Pattern == "" {
			return fmt.Errorf("string needs a pattern")
		}
		compiled, err := regexp.Compile(`^(?:` + spec.Pattern + `)$`)
		if err != nil {
			return fmt.Errorf("pattern: %w", err)
		}
		spec.regex = compiled
	case "int":
		if spec.Min != nil && spec.Max != nil && *spec.Min > *spec.Max {
			return fmt.Errorf("min > max")
		}
	case "bool", "directory":
	default:
		return fmt.Errorf("unknown type %q", spec.Type)
	}
	if spec.Type != "enum" && len(spec.Values) > 0 || spec.Type != "string" && spec.Pattern != "" {
		return fmt.Errorf("values are only for enum, pattern only for string")
	}
	if spec.Default != nil {
		if _, err := spec.check(*spec.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

func (spec *Spec) check(value string) (string, error) {
	switch spec.Type {
	case "enum":
		for _, allowed := range spec.Values {
			if value == allowed {
				return value, nil
			}
		}
		return "", fmt.Errorf("must be one of %v", spec.Values)
	case "string":
		if !spec.regex.MatchString(value) {
			return "", fmt.Errorf("must match %s", spec.Pattern)
		}
		return value, nil
	case "int":
		number, err := strconv.Atoi(value)
		if err != nil {
			return "", fmt.Errorf("must be an integer")
		}
		if spec.Min != nil && number < *spec.Min {
			return "", fmt.Errorf("must be >= %d", *spec.Min)
		}
		if spec.Max != nil && number > *spec.Max {
			return "", fmt.Errorf("must be <= %d", *spec.Max)
		}
		return strconv.Itoa(number), nil
	case "bool":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return "", fmt.Errorf("must be a boolean")
		}
		return strconv.FormatBool(parsed), nil
	case "directory":
		if len(value) > 255 || !directoryPattern.MatchString(value) {
			return "", fmt.Errorf("must be a relative directory like terraform/vms (no leading /, no . or .. segments, no hidden directories)")
		}
		return value, nil
	}
	return "", fmt.Errorf("unknown type %q", spec.Type)
}

// directoryPattern matches a relative path whose segments start with a letter, digit or "_".
// That rules out "." and "..", hidden directories, a leading "-", absolute paths, empty
// segments and a trailing "/", so the value stays inside the directory it is joined to.
var directoryPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`)

type Schema map[string]*Spec

// Validate returns normalized values for every declared parameter.
func (schema Schema) Validate(input map[string]string) (map[string]string, error) {
	for name := range input {
		if _, ok := schema[name]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}
	}
	out := make(map[string]string, len(schema))
	for _, name := range schema.Names() {
		spec := schema[name]
		value, ok := input[name]
		if !ok {
			if spec.Default == nil {
				return nil, fmt.Errorf("missing parameter %q", name)
			}
			value = *spec.Default
		}
		normalized, err := spec.check(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		out[name] = normalized
	}
	return out, nil
}

func (schema Schema) Names() []string {
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
