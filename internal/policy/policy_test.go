package policy

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAllowed(t *testing.T) {
	var rules []Rule
	src := `
- repository: cloudyhomelab/infra
  ref: refs/heads/main
  environment: production
- repository: [cloudyhomelab/app, cloudyhomelab/web-*]
  event_name: push
`
	if err := yaml.Unmarshal([]byte(src), &rules); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		claims Claims
		want   bool
	}{
		{Claims{"repository": "cloudyhomelab/infra", "ref": "refs/heads/main", "environment": "production"}, true},
		{Claims{"repository": "cloudyhomelab/infra", "ref": "refs/heads/main"}, false},
		{Claims{"repository": "cloudyhomelab/infra", "ref": "refs/pull/1/merge", "environment": "production"}, false},
		{Claims{"repository": "cloudyhomelab/web-shop", "event_name": "push"}, true},
		{Claims{"repository": "cloudyhomelab/web-shop", "event_name": "pull_request"}, false},
		{Claims{"repository": "evil/app", "event_name": "push"}, false},
	}
	for index, testCase := range cases {
		if got := Allowed(rules, testCase.claims); got != testCase.want {
			t.Errorf("case %d: got %v, want %v", index, got, testCase.want)
		}
	}
	if Allowed(nil, Claims{"repository": "cloudyhomelab/infra"}) {
		t.Error("no rules must deny")
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"refs/heads/*", "refs/heads/feature/x", true},
		{"refs/heads/main", "refs/heads/main2", false},
		{"a.b", "axb", false},
		{"*", "", true},
	}
	for _, testCase := range cases {
		if got := Glob(testCase.pattern, testCase.value); got != testCase.want {
			t.Errorf("Glob(%q, %q) = %v", testCase.pattern, testCase.value, got)
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
