package params

import "testing"

func ptr[T any](value T) *T { return &value }

func schema(t *testing.T) Schema {
	t.Helper()
	testSchema := Schema{
		"region":  {Type: "enum", Values: []string{"eu-west-1", "us-east-1"}},
		"version": {Type: "string", Pattern: `[0-9]+\.[0-9]+\.[0-9]+`},
		"count":   {Type: "int", Min: ptr(1), Max: ptr(5), Default: ptr("2")},
		"dry":     {Type: "bool", Default: ptr("false")},
	}
	for name, spec := range testSchema {
		if err := spec.Compile(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return testSchema
}

func TestValidate(t *testing.T) {
	testSchema := schema(t)
	got, err := testSchema.Validate(map[string]string{"region": "eu-west-1", "version": "1.2.3", "dry": "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"region": "eu-west-1", "version": "1.2.3", "count": "2", "dry": "true"}
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("%s = %q, want %q", name, got[name], expected)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	testSchema := schema(t)
	base := func() map[string]string {
		return map[string]string{"region": "eu-west-1", "version": "1.2.3"}
	}
	cases := map[string]func(values map[string]string){
		"unknown":          func(values map[string]string) { values["extra"] = "x" },
		"missing":          func(values map[string]string) { delete(values, "region") },
		"enum":             func(values map[string]string) { values["region"] = "mars-1" },
		"pattern anchored": func(values map[string]string) { values["version"] = "1.2.3; rm -rf /" },
		"newline":          func(values map[string]string) { values["version"] = "1.2.3\n" },
		"int range":        func(values map[string]string) { values["count"] = "9" },
		"int format":       func(values map[string]string) { values["count"] = "2x" },
		"bool":             func(values map[string]string) { values["dry"] = "maybe" },
	}
	for name, mutate := range cases {
		values := base()
		mutate(values)
		if _, err := testSchema.Validate(values); err == nil {
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
	for index, spec := range bad {
		if err := spec.Compile(); err == nil {
			t.Errorf("case %d: expected error", index)
		}
	}
}
