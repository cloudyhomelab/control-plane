package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// NewTestRepo creates a git repo with the given files committed on main and returns its path.
func NewTestRepo(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}
