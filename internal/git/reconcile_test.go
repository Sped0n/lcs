package git

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMirrorResolvesSymbolicBranchAliases(t *testing.T) {
	f := newFixture(t)
	gitRun(t, f.remote, "symbolic-ref", "refs/heads/alias", "refs/heads/main")

	for range 2 {
		dest := filepath.Join(t.TempDir(), "client")
		gitRun(t, "", "clone", f.local, dest)
		if got := strings.TrimSpace(gitRun(t, dest, "rev-parse", "origin/alias")); got != strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD")) {
			t.Fatalf("symbolic branch alias resolved to %s", got)
		}
	}

	stats, err := f.store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Git == 0 || stats.Misses != 1 || f.fetches.Load() != 1 {
		t.Fatalf("symbolic branch prevented mirror reuse: %+v, fetches=%d", stats, f.fetches.Load())
	}
}

func TestMovingRefsStopAfterBoundedRefresh(t *testing.T) {
	f := newFixture(t)
	original := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))
	gitRun(t, f.work, "commit", "--allow-empty", "-m", "next")
	changed := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))
	gitRun(t, f.work, "push", f.remote, "main")
	gitRun(t, f.remote, "update-ref", "refs/heads/main", original)
	gitRun(t, f.remote, "update-ref", "refs/tags/changed", changed)
	gitRun(t, f.remote, "config", "uploadpack.allowReachableSHA1InWant", "true")

	var moves atomic.Int64
	f.hooksMu.Lock()
	f.afterRefs = func() error {
		oid := changed
		if moves.Add(1)%2 == 0 {
			oid = original
		}
		return exec.Command("git", "-C", f.remote, "update-ref", "refs/heads/main", oid).Run()
	}
	f.hooksMu.Unlock()

	client := filepath.Join(t.TempDir(), "client")
	gitRun(t, "", "clone", f.local, client)
	if got := strings.TrimSpace(gitRun(t, client, "rev-parse", "HEAD")); got != original {
		t.Fatalf("fallback used a different advertisement: %s", got)
	}

	stats, err := f.store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Git != 0 || stats.Temporary != 0 || moves.Load() > 8 {
		t.Fatalf("moving refs did not cleanly fall back after three attempts: %+v, ref reads=%d", stats, moves.Load())
	}
}

func TestShallowCloneIncludesAnnotatedTag(t *testing.T) {
	f := newFixture(t)
	gitRun(t, f.work, "tag", "-a", "release", "-m", "release")
	gitRun(t, f.work, "push", f.remote, "--tags")

	for range 2 {
		client := filepath.Join(t.TempDir(), "client")
		gitRun(t, "", "clone", "--depth=1", f.local, client)
		if got := strings.TrimSpace(gitRun(t, client, "describe", "--exact-match")); got != "release" {
			t.Fatalf("shallow clone lost annotated tag: %s", got)
		}
	}

	gitRun(t, f.work, "tag", "-fa", "release", "-m", "retagged")
	gitRun(t, f.work, "push", "--force", f.remote, "refs/tags/release")

	client := filepath.Join(t.TempDir(), "retagged")
	gitRun(t, "", "clone", "--depth=1", f.local, client)
	if got := gitRun(t, client, "rev-parse", "refs/tags/release"); got != gitRun(t, f.work, "rev-parse", "refs/tags/release") {
		t.Fatal("shallow cache served an old annotated tag")
	}

	stats, err := f.store.Stats()
	if err != nil || stats.Git == 0 || f.fetches.Load() != 2 {
		t.Fatalf("annotated tag cache did not reuse and refresh: %+v, %v, fetches=%d", stats, err, f.fetches.Load())
	}
}
