package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudyhomelab/control-plane/internal/config"
	"github.com/cloudyhomelab/control-plane/internal/testutil"
)

func TestResolveCheckout(t *testing.T) {
	origin := testutil.NewTestRepo(t, map[string]string{"tf/main.tf": "# hi\n"})
	data := t.TempDir()
	source := New(data, map[string]*config.Repo{"infra": {URL: origin, DefaultRef: "refs/heads/main"}})
	ctx := context.Background()

	sha, err := source.Resolve(ctx, "infra", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 40 {
		t.Fatalf("sha = %q", sha)
	}
	// Second resolve reuses the mirror.
	if sha2, err := source.Resolve(ctx, "infra", "refs/heads/main"); err != nil || sha2 != sha {
		t.Fatalf("re-resolve = %q, %v", sha2, err)
	}

	dest := filepath.Join(data, "jobs", "j1", "src")
	if err := source.Checkout(ctx, "infra", sha, dest); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(dest, "tf/main.tf")); err != nil || string(content) != "# hi\n" {
		t.Fatalf("checkout content = %q, %v", content, err)
	}
	if err := source.Remove(ctx, "infra", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("worktree not removed")
	}

	if _, err := source.Resolve(ctx, "infra", "refs/heads/missing"); err == nil {
		t.Error("expected error for missing ref")
	}
}

func TestValidRef(t *testing.T) {
	ok := []string{"refs/heads/main", "refs/heads/feature/x-1", "refs/tags/v1.2.3", "refs/pull/12/merge"}
	bad := []string{"main", "refs/heads/../x", "refs/heads/a b", "--upload-pack=x", "refs/remotes/o/main", "refs/heads/x.lock", "refs/heads/"}
	for _, ref := range ok {
		if !ValidRef(ref) {
			t.Errorf("%q should be valid", ref)
		}
	}
	for _, ref := range bad {
		if ValidRef(ref) {
			t.Errorf("%q should be invalid", ref)
		}
	}
}
