package policy

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAllowed(t *testing.T) {
	var rules []Rule
	src := `
- repository: cloudyhome/infra
  ref: refs/heads/main
  environment: production
- repository: [cloudyhome/app, cloudyhome/web-*]
  event_name: push
`
	if err := yaml.Unmarshal([]byte(src), &rules); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		claims Claims
		want   bool
	}{
		{Claims{"repository": "cloudyhome/infra", "ref": "refs/heads/main", "environment": "production"}, true},
		{Claims{"repository": "cloudyhome/infra", "ref": "refs/heads/main"}, false},
		{Claims{"repository": "cloudyhome/infra", "ref": "refs/pull/1/merge", "environment": "production"}, false},
		{Claims{"repository": "cloudyhome/web-shop", "event_name": "push"}, true},
		{Claims{"repository": "cloudyhome/web-shop", "event_name": "pull_request"}, false},
		{Claims{"repository": "evil/app", "event_name": "push"}, false},
	}
	for i, c := range cases {
		if got := Allowed(rules, c.claims); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
	if Allowed(nil, Claims{"repository": "cloudyhome/infra"}) {
		t.Error("no rules must deny")
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, v string
		want   bool
	}{
		{"refs/heads/*", "refs/heads/feature/x", true},
		{"refs/heads/main", "refs/heads/main2", false},
		{"a.b", "axb", false},
		{"*", "", true},
	}
	for _, c := range cases {
		if got := Glob(c.pat, c.v); got != c.want {
			t.Errorf("Glob(%q, %q) = %v", c.pat, c.v, got)
		}
	}
}

func TestRuleValidate(t *testing.T) {
	if err := (Rule{}).Validate(); err == nil {
		t.Error("empty rule should fail")
	}
	if err := (Rule{"repo": {"x"}}).Validate(); err == nil {
		t.Error("unknown claim should fail")
	}
}
