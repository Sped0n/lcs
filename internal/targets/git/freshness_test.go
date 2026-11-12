package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRefreshDoesNotKeepAChangedAndRevertedSnapshot(t *testing.T) {
	f := newFixture(t)
	t.Setenv("NO_PROXY", "127.0.0.1")
	t.Setenv("no_proxy", "127.0.0.1")
	original := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(f.work, "file"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.work, "commit", "-am", "second")
	changed := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))
	gitRun(t, f.work, "push", f.remote, "main")
	gitRun(t, f.remote, "update-ref", "refs/heads/main", original)

	var advanceOnce, revertOnce sync.Once
	var advanced, reverted atomic.Bool
	f.hooksMu.Lock()
	f.afterRefs = func() error {
		var err error
		advanceOnce.Do(func() {
			err = exec.Command("git", "-C", f.remote, "update-ref", "refs/heads/main", changed).Run()
			advanced.Store(err == nil)
		})
		return err
	}
	f.afterFetch = func() error {
		var err error
		revertOnce.Do(func() {
			err = exec.Command("git", "-C", f.remote, "update-ref", "refs/heads/main", original).Run()
			reverted.Store(err == nil)
		})
		return err
	}
	f.hooksMu.Unlock()

	for i := range 3 {
		destination := filepath.Join(t.TempDir(), "clone")
		gitRun(t, "", "-c", "http.proxy="+f.cache.proxyURL, "clone", f.local, destination)
		if i > 0 {
			if subject := strings.TrimSpace(gitRun(t, destination, "log", "-1", "--format=%s")); subject != "first" {
				t.Fatalf("clone after upstream reverted used stale commit: %s", subject)
			}
		}
	}
	if !advanced.Load() || !reverted.Load() {
		t.Fatal("upstream did not change and revert during the mirror download")
	}
	stats, err := f.store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Git == 0 || stats.Hits == 0 {
		t.Fatalf("stable upstream never returned to cached serving: %+v", stats)
	}
}

func TestRefreshTracksDetachedAndSymbolicHEAD(t *testing.T) {
	f := newFixture(t)
	gitRun(t, "", "clone", f.local, filepath.Join(t.TempDir(), "warm"))
	original := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(f.work, "file"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.work, "commit", "-am", "second")
	changed := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))
	gitRun(t, f.work, "push", f.remote, "HEAD:refs/heads/head-only")
	gitRun(t, f.remote, "update-ref", "--no-deref", "HEAD", changed)
	gitRun(t, f.remote, "update-ref", "-d", "refs/heads/head-only")

	for _, tc := range []struct {
		name, oid, subject string
	}{
		{"detached-unreferenced", changed, "second"},
		{"detached-existing", original, "first"},
		{"symbolic", "", "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.oid != "" {
				gitRun(t, f.remote, "update-ref", "--no-deref", "HEAD", tc.oid)
			} else {
				gitRun(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
			}
			destination := filepath.Join(t.TempDir(), "clone")
			gitRun(t, "", "clone", f.local, destination)
			if subject := strings.TrimSpace(gitRun(t, destination, "log", "-1", "--format=%s")); subject != tc.subject {
				t.Fatalf("HEAD subject=%q; want %q", subject, tc.subject)
			}
			advertisement := gitRun(t, "", "ls-remote", "--symref", f.local)
			symbolic := strings.Contains(advertisement, "ref: refs/heads/main\tHEAD\n")
			if symbolic != (tc.oid == "") {
				t.Fatalf("incorrect HEAD advertisement: %s", advertisement)
			}
			head := tc.oid
			if head == "" {
				head = original
			}
			if !strings.Contains(advertisement, head+"\tHEAD\n") {
				t.Fatalf("HEAD commit not advertised: %s", advertisement)
			}
			if main := strings.TrimSpace(gitRun(t, destination, "rev-parse", "origin/main")); main != original {
				t.Fatalf("updating HEAD changed main: %s", main)
			}
		})
	}
}
