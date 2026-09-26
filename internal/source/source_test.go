package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/testutil"
)

func TestResolveCheckout(t *testing.T) {
	origin := testutil.NewTestRepo(t, map[string]string{"tf/main.tf": "# hi\n"})
	data := t.TempDir()
	s := New(data, map[string]*config.Repo{"infra": {URL: origin, DefaultRef: "refs/heads/main"}})
	ctx := context.Background()

	sha, err := s.Resolve(ctx, "infra", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 40 {
		t.Fatalf("sha = %q", sha)
	}
	// Second resolve reuses the mirror.
	if sha2, err := s.Resolve(ctx, "infra", "refs/heads/main"); err != nil || sha2 != sha {
		t.Fatalf("re-resolve = %q, %v", sha2, err)
	}

	dest := filepath.Join(data, "jobs", "j1", "src")
	if err := s.Checkout(ctx, "infra", sha, dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "tf/main.tf")); err != nil || string(b) != "# hi\n" {
		t.Fatalf("checkout content = %q, %v", b, err)
	}
	if err := s.Remove(ctx, "infra", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("worktree not removed")
	}

	if _, err := s.Resolve(ctx, "infra", "refs/heads/missing"); err == nil {
		t.Error("expected error for missing ref")
	}
}

func TestValidRef(t *testing.T) {
	ok := []string{"refs/heads/main", "refs/heads/feature/x-1", "refs/tags/v1.2.3", "refs/pull/12/merge"}
	bad := []string{"main", "refs/heads/../x", "refs/heads/a b", "--upload-pack=x", "refs/remotes/o/main", "refs/heads/x.lock", "refs/heads/"}
	for _, r := range ok {
		if !ValidRef(r) {
			t.Errorf("%q should be valid", r)
		}
	}
	for _, r := range bad {
		if ValidRef(r) {
			t.Errorf("%q should be invalid", r)
		}
	}
}
