// Package source keeps bare git mirrors and checks out per-job worktrees at a fixed commit.
package source

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/cloudyhomelab/control-plane/internal/config"
)

var refRE = regexp.MustCompile(`^refs/(heads|tags|pull)/[A-Za-z0-9._/-]+$`)

type Source struct {
	dir   string
	repos map[string]*config.Repo

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(dataDir string, repos map[string]*config.Repo) *Source {
	return &Source{dir: filepath.Join(dataDir, "repos"), repos: repos, locks: map[string]*sync.Mutex{}}
}

// ValidRef reports whether ref is a full branch, tag or pull ref that is safe to pass to git.
func ValidRef(ref string) bool {
	return refRE.MatchString(ref) && !strings.Contains(ref, "..") && !strings.HasSuffix(ref, "/") &&
		!strings.HasSuffix(ref, ".lock")
}

func (source *Source) DefaultRef(repo string) string {
	return source.repos[repo].DefaultRef
}

// Resolve fetches ref from the remote and returns its commit SHA.
func (source *Source) Resolve(ctx context.Context, repo, ref string) (string, error) {
	if !ValidRef(ref) {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	unlock := source.lock(repo)
	defer unlock()
	if err := source.ensureMirror(ctx, repo); err != nil {
		return "", err
	}
	if _, err := source.git(ctx, repo, "fetch", "--no-tags", "--force", "origin", "+"+ref+":"+ref); err != nil {
		return "", fmt.Errorf("fetch %s: %w", ref, err)
	}
	out, err := source.git(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Checkout creates a detached worktree of sha at dest. The sha must have been fetched by Resolve.
func (source *Source) Checkout(ctx context.Context, repo, sha, dest string) error {
	unlock := source.lock(repo)
	defer unlock()
	_, err := source.git(ctx, repo, "worktree", "add", "--detach", dest, sha)
	return err
}

func (source *Source) Remove(ctx context.Context, repo, dest string) error {
	unlock := source.lock(repo)
	defer unlock()
	if _, err := source.git(ctx, repo, "worktree", "remove", "--force", dest); err != nil {
		os.RemoveAll(dest)
		_, err = source.git(ctx, repo, "worktree", "prune")
		return err
	}
	return nil
}

func (source *Source) lock(repo string) func() {
	source.mu.Lock()
	repoLock, ok := source.locks[repo]
	if !ok {
		repoLock = &sync.Mutex{}
		source.locks[repo] = repoLock
	}
	source.mu.Unlock()
	repoLock.Lock()
	return repoLock.Unlock
}

func (source *Source) mirror(repo string) string {
	return filepath.Join(source.dir, repo+".git")
}

func (source *Source) ensureMirror(ctx context.Context, repo string) error {
	repoConfig, ok := source.repos[repo]
	if !ok {
		return fmt.Errorf("unknown repo %q", repo)
	}
	if _, err := os.Stat(filepath.Join(source.mirror(repo), "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(source.dir, 0o750); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare", "--quiet", source.mirror(repo)).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, out)
	}
	_, err := source.git(ctx, repo, "remote", "add", "origin", repoConfig.URL)
	return err
}

func (source *Source) git(ctx context.Context, repo string, args ...string) (string, error) {
	repoConfig := source.repos[repo]
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if repoConfig.DeployKeyFile != "" {
		ssh := "ssh -i " + repoConfig.DeployKeyFile + " -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes"
		if repoConfig.KnownHostsFile != "" {
			ssh += " -o UserKnownHostsFile=" + repoConfig.KnownHostsFile
		}
		env = append(env, "GIT_SSH_COMMAND="+ssh)
	}
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", source.mirror(repo)}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}
