package git

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spedon/lcs/internal/storage"
)

func addDiscardedHistory(t *testing.T, f *fixture) string {
	t.Helper()

	for range 8 {
		data := make([]byte, 256<<10)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.work, "history.bin"), data, 0600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, f.work, "add", ".")
		gitRun(t, f.work, "commit", "-qm", "history")
	}

	gitRun(t, f.work, "rm", "history.bin")
	gitRun(t, f.work, "commit", "-qm", "pinned")
	pinned := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))

	gitRun(t, f.work, "commit", "--allow-empty", "-qm", "new tip")
	gitRun(t, f.work, "push", "-q", f.remote, "main")

	return pinned
}

func TestPinnedShallowSubmoduleCache(t *testing.T) {
	t.Setenv("GIT_INDEX_FILE", ".git/index")

	for _, protocol := range []string{"0", "2"} {
		t.Run("protocol-"+protocol, func(t *testing.T) {
			f := newFixture(t)
			pinned := addDiscardedHistory(t, f)
			gitRun(t, f.remote, "config", "uploadpack.allowReachableSHA1InWant", "true")

			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			gitRun(t, "", "init", "-q", "-b", "main", parent)
			gitRun(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", f.remote, "child")
			gitRun(t, filepath.Join(parent, "child"), "checkout", "-q", pinned)
			gitRun(t, parent, "config", "-f", ".gitmodules", "submodule.child.url", "https://example.com/owner/repo.git")
			gitRun(t, parent, "add", ".")
			gitRun(t, parent, "commit", "-qm", "pinned submodule")

			for i := range 2 {
				dest := filepath.Join(root, "checkout-"+strconv.Itoa(i))
				gitRun(t, "", "clone", "-q", parent, dest)
				gitRun(t, dest, "-c", "protocol.version="+protocol, "-c", "url."+f.local+".insteadOf=https://example.com/owner/repo.git", "submodule", "update", "--init", "--depth", "1")
				child := filepath.Join(dest, "child")
				if got := strings.TrimSpace(gitRun(t, child, "rev-parse", "HEAD")); got != pinned {
					t.Fatalf("submodule HEAD = %s, want %s", got, pinned)
				}
				if got := strings.TrimSpace(gitRun(t, child, "rev-list", "--count", "HEAD")); got != "1" {
					t.Fatalf("shallow history length = %s", got)
				}
				if data, err := os.ReadFile(filepath.Join(child, "file")); err != nil || string(data) != "first\n" {
					t.Fatalf("submodule contents = %q, %v", data, err)
				}
			}

			stats, err := f.store.Stats()
			if err != nil {
				t.Fatal(err)
			}
			if stats.Git == 0 || stats.Git >= 1<<20 || stats.Temporary != 0 || f.downloaded.Load() >= 1<<20 {
				t.Fatalf("shallow cache downloaded discarded history or retained staging: %+v, upstream bytes=%d", stats, f.downloaded.Load())
			}
			if got := f.fetches.Load(); got != 2 {
				t.Fatalf("upstream pack fetches = %d, want one for the tip and one for the pin", got)
			}

			t.Logf("two shallow submodule checkouts: %d upstream pack responses, %d response bytes, %d cached bytes", f.fetches.Load(), f.downloaded.Load(), stats.Git)

			child := filepath.Join(root, "checkout-1", "child")
			gitRun(t, child, "-c", "protocol.version="+protocol, "-c", "url."+f.local+".insteadOf=https://example.com/owner/repo.git", "fetch", "--deepen=1", "origin", pinned)
			if count, err := strconv.Atoi(strings.TrimSpace(gitRun(t, child, "rev-list", "--count", "HEAD"))); err != nil || count < 2 {
				t.Fatalf("relative deepening did not extend history: %d, %v", count, err)
			}
			gitRun(t, child, "-c", "protocol.version="+protocol, "-c", "url."+f.local+".insteadOf=https://example.com/owner/repo.git", "fetch", "--unshallow", "origin")
			if count, err := strconv.Atoi(strings.TrimSpace(gitRun(t, child, "rev-list", "--count", "HEAD"))); err != nil || count < 9 {
				t.Fatalf("unshallow lost history: %d, %v", count, err)
			}

			after, err := f.store.Stats()
			if err != nil || after.Git != stats.Git {
				t.Fatalf("unshallow should bypass without expanding the cache: %+v, %v", after, err)
			}

			full := filepath.Join(root, "full")
			gitRun(t, "", "clone", "-q", f.local, full)
			if count, err := strconv.Atoi(strings.TrimSpace(gitRun(t, full, "rev-list", "--count", "HEAD"))); err != nil || count < 10 {
				t.Fatalf("ordinary clone lost history: %d, %v", count, err)
			}
		})
	}
}

func TestRefListingDoesNotDownloadMirror(t *testing.T) {
	f := newFixture(t)

	refs := gitRun(t, "", "ls-remote", "--symref", f.local)
	if !strings.Contains(refs, "refs/heads/main") {
		t.Fatalf("missing upstream refs: %s", refs)
	}

	stats, err := f.store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Git != 0 || stats.Temporary != 0 || f.fetches.Load() != 0 {
		t.Fatalf("listing refs downloaded a repository: %+v, fetches=%d", stats, f.fetches.Load())
	}
}

func TestShallowCacheFallback(t *testing.T) {
	for _, cause := range []string{"capacity", "clear"} {
		t.Run(cause, func(t *testing.T) {
			f := newFixture(t)
			store := f.store
			if cause == "capacity" {
				var err error
				store, err = storage.Open(t.TempDir(), t.TempDir(), 100)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Error(err)
					}
				})
				f.cache.store = store
			} else {
				f.afterFetch = store.Clear
			}

			dest := filepath.Join(t.TempDir(), "client")
			gitRun(t, "", "clone", "-q", "--depth=1", f.local, dest)
			if got := strings.TrimSpace(gitRun(t, dest, "show", "HEAD:file")); got != "first" {
				t.Fatalf("fallback returned wrong contents: %q", got)
			}

			stats, err := store.Stats()
			if err != nil {
				t.Fatal(err)
			}
			if stats.Git != 0 || stats.Temporary != 0 {
				t.Fatalf("bypass published or retained staging: %+v", stats)
			}
		})
	}
}
