package git

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneSurvivesClearBetweenRequests(t *testing.T) {
	f := newFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.cache.ServeHTTP(w, r)
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") {
			if err := f.store.Clear(); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "clone")
	gitRun(t, "", "clone", server.URL+"/git/example.com/owner/repo.git", destination)
	if data, err := os.ReadFile(filepath.Join(destination, "file")); err != nil || string(data) != "first\n" {
		t.Fatalf("clone after clear: %q %v", data, err)
	}
}

func TestRecursiveExplicitSubmodules(t *testing.T) {
	f := newFixture(t, "https://example.com/owner/child.git", "https://example.com/owner/grandchild.git")
	root := filepath.Dir(f.work)

	grandchild := filepath.Join(root, "grandchild")
	gitRun(t, root, "clone", f.work, grandchild)
	grandchildRemote := filepath.Join(filepath.Dir(f.remote), "grandchild.git")
	gitRun(t, root, "clone", "--bare", grandchild, grandchildRemote)

	child := filepath.Join(root, "child")
	gitRun(t, root, "clone", f.work, child)
	gitRun(t, child, "-c", "protocol.file.allow=always", "submodule", "add", grandchildRemote, "nested")
	gitRun(t, child, "config", "-f", ".gitmodules", "submodule.nested.url", "https://example.com/owner/grandchild.git")
	gitRun(t, child, "add", ".")
	gitRun(t, child, "commit", "-m", "nested submodule")
	childRemote := filepath.Join(filepath.Dir(f.remote), "child.git")
	gitRun(t, root, "clone", "--bare", child, childRemote)

	gitRun(t, f.work, "-c", "protocol.file.allow=always", "submodule", "add", childRemote, "module")
	gitRun(t, f.work, "config", "-f", ".gitmodules", "submodule.module.url", "https://example.com/owner/child.git")
	gitRun(t, f.work, "add", ".")
	gitRun(t, f.work, "commit", "-m", "submodule")
	gitRun(t, f.work, "push", f.remote, "main")

	destination := filepath.Join(t.TempDir(), "clone")
	gitRun(t, "", "clone", f.local, destination)
	base := strings.TrimSuffix(f.local, "owner/repo.git")
	gitRun(t, destination, "-c", "url."+base+".insteadOf=https://example.com/", "submodule", "update", "--init", "--recursive")

	data, err := os.ReadFile(filepath.Join(destination, "module", "nested", "file"))
	if err != nil || string(data) != "first\n" {
		t.Fatalf("nested clone: %q %v", data, err)
	}
}
