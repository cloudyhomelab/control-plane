package params

import "testing"

func ptr[T any](v T) *T { return &v }

func schema(t *testing.T) Schema {
	t.Helper()
	sc := Schema{
		"region":  {Type: "enum", Values: []string{"eu-west-1", "us-east-1"}},
		"version": {Type: "string", Pattern: `[0-9]+\.[0-9]+\.[0-9]+`},
		"count":   {Type: "int", Min: ptr(1), Max: ptr(5), Default: ptr("2")},
		"dry":     {Type: "bool", Default: ptr("false")},
	}
	for name, s := range sc {
		if err := s.Compile(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return sc
}

func TestValidate(t *testing.T) {
	sc := schema(t)
	got, err := sc.Validate(map[string]string{"region": "eu-west-1", "version": "1.2.3", "dry": "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"region": "eu-west-1", "version": "1.2.3", "count": "2", "dry": "true"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	sc := schema(t)
	base := func() map[string]string {
		return map[string]string{"region": "eu-west-1", "version": "1.2.3"}
	}
	cases := map[string]func(m map[string]string){
		"unknown":          func(m map[string]string) { m["extra"] = "x" },
		"missing":          func(m map[string]string) { delete(m, "region") },
		"enum":             func(m map[string]string) { m["region"] = "mars-1" },
		"pattern anchored": func(m map[string]string) { m["version"] = "1.2.3; rm -rf /" },
		"newline":          func(m map[string]string) { m["version"] = "1.2.3\n" },
		"int range":        func(m map[string]string) { m["count"] = "9" },
		"int format":       func(m map[string]string) { m["count"] = "2x" },
		"bool":             func(m map[string]string) { m["dry"] = "maybe" },
	}
	for name, mutate := range cases {
		m := base()
		mutate(m)
		if _, err := sc.Validate(m); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCompileRejects(t *testing.T) {
	bad := []*Spec{
		{Type: "string"},
		{Type: "enum"},
		{Type: "int", Min: ptr(3), Max: ptr(1)},
		{Type: "enum", Values: []string{"a"}, Default: ptr("b")},
		{Type: "float"},
	}
	for i, s := range bad {
		if err := s.Compile(); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}
