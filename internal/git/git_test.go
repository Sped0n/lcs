package git

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spedon/lcs/internal/config"
	"github.com/spedon/lcs/internal/storage"
)

type fixture struct {
	cache               *Cache
	store               *storage.Store
	work, local, remote string
	fetches             atomic.Int64
	downloaded          atomic.Int64
	slow                atomic.Bool
	hooksMu             sync.Mutex
	afterRefs           func() error
	afterFetch          func() error
}

type countedResponse struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w countedResponse) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytes.Add(int64(n))
	return n, err
}

func (w countedResponse) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}

	return string(out)
}

func newFixture(t *testing.T, repositories ...string) *fixture {
	t.Helper()

	// Git hooks pass repository paths that must not affect fixture repositories.
	for _, value := range os.Environ() {
		name, previous, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "GIT_") {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Setenv(name, previous); err != nil {
				t.Error(err)
			}
		})
	}

	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	root := t.TempDir()
	work := filepath.Join(root, "work")

	gitRun(t, root, "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "first")
	gitRun(t, work, "tag", "v1")

	upstreamRoot := filepath.Join(root, "upstream")
	if err := os.MkdirAll(filepath.Join(upstreamRoot, "owner"), 0o700); err != nil {
		t.Fatal(err)
	}
	remotePath := filepath.Join(upstreamRoot, "owner", "repo.git")
	gitRun(t, root, "clone", "--bare", work, remotePath)

	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{work: work, remote: remotePath}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.slow.Load() {
			time.Sleep(50 * time.Millisecond)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials reached anonymous mirror fetch")
		}
		body, err := io.ReadAll(r.Body)
		closeErr := r.Body.Close()
		if err != nil || closeErr != nil {
			t.Errorf("read upstream request: %v %v", err, closeErr)
			http.Error(w, "request body failed", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fetch := r.Method == http.MethodPost && bytes.Contains(body, []byte("want "))
		refs := bytes.Contains(body, []byte("command=ls-refs\n")) || r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") && !strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
		if fetch {
			f.fetches.Add(1)
			w = countedResponse{w, &f.downloaded}
		}
		f.hooksMu.Lock()
		var afterResponse func() error
		if refs {
			afterResponse = f.afterRefs
		} else if fetch {
			afterResponse = f.afterFetch
		}
		f.hooksMu.Unlock()
		backend := &cgi.Handler{Path: binary, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + upstreamRoot, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL=" + r.Header.Get("Git-Protocol")}}
		if afterResponse == nil {
			backend.ServeHTTP(w, r)
			return
		}
		// Change upstream only after Git has generated the response, before the client sees it.
		response := httptest.NewRecorder()
		backend.ServeHTTP(response, r)
		if response.Code == http.StatusOK {
			if err := afterResponse(); err != nil {
				t.Errorf("change upstream refs: %v", err)
				http.Error(w, "upstream ref change failed", 500)
				return
			}
		}
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		if _, err := w.Write(response.Body.Bytes()); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)

	caPath := filepath.Join(root, "roots.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caPath)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())

	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"}}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}

	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(state, filepath.Join(root, "cache"), 100e6)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})

	f.cache, err = New(config.Config{Repositories: append([]string{"https://example.com/owner/repo.git"}, repositories...)}, f.store, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.cache.Close)
	t.Setenv("https_proxy", f.cache.proxyURL)

	local := httptest.NewServer(f.cache)
	t.Cleanup(local.Close)
	f.local = local.URL + "/git/example.com/owner/repo.git"

	return f
}

func TestCloneFetchAndProtocols(t *testing.T) {
	f := newFixture(t)

	for _, protocol := range []string{"0", "2"} {
		dest := filepath.Join(t.TempDir(), "clone")
		gitRun(t, "", "-c", "protocol.version="+protocol, "clone", f.local, dest)
		if got := gitRun(t, dest, "describe", "--tags"); strings.TrimSpace(got) != "v1" {
			t.Fatal(got)
		}
	}

	stats, err := f.store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Git == 0 || stats.Hits < 1 || stats.Misses != 1 {
		t.Fatalf("stats: %+v", stats)
	}

	if err := os.WriteFile(filepath.Join(f.work, "file"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.work, "commit", "-am", "second")
	gitRun(t, f.work, "push", f.remote, "main")
	clone := filepath.Join(t.TempDir(), "updated")
	gitRun(t, "", "clone", f.local, clone)
	if got := gitRun(t, clone, "log", "-1", "--format=%s"); strings.TrimSpace(got) != "second" {
		t.Fatal(got)
	}

	gitRun(t, f.work, "checkout", "-b", "next")
	gitRun(t, f.work, "push", f.remote, "next")
	gitRun(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/next")
	clone = filepath.Join(t.TempDir(), "default")
	gitRun(t, "", "clone", f.local, clone)
	if got := gitRun(t, clone, "branch", "--show-current"); strings.TrimSpace(got) != "next" {
		t.Fatal(got)
	}

	gitRun(t, f.work, "checkout", "main")
	gitRun(t, f.work, "reset", "--hard", "v1")
	gitRun(t, f.work, "push", "--force", f.remote, "main")
	gitRun(t, clone, "fetch", "origin")
	if got := gitRun(t, clone, "rev-parse", "origin/main"); got != gitRun(t, f.work, "rev-parse", "v1") {
		t.Fatal("force push not reflected")
	}
}

func TestShallowAndPartialClone(t *testing.T) {
	f := newFixture(t)
	gitRun(t, f.remote, "config", "uploadpack.allowFilter", "true")

	shallow := filepath.Join(t.TempDir(), "shallow")
	gitRun(t, "", "clone", "--depth=1", f.local, shallow)
	if got := gitRun(t, shallow, "rev-parse", "--is-shallow-repository"); strings.TrimSpace(got) != "true" {
		t.Fatal(got)
	}
	before, err := f.store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cold", "warm"} {
		if mode == "warm" {
			gitRun(t, "", "clone", f.local, filepath.Join(t.TempDir(), "full"))
		}

		partial := filepath.Join(t.TempDir(), "partial-"+mode)
		gitRun(t, "", "clone", "--filter=blob:none", "--no-checkout", f.local, partial)
		gitRun(t, partial, "checkout", "main")
		if data, err := os.ReadFile(filepath.Join(partial, "file")); err != nil || string(data) != "first\n" {
			t.Fatalf("partial checkout: %q %v", data, err)
		}
		if mode == "cold" {
			after, err := f.store.Stats()
			if err != nil || after.Git != before.Git {
				t.Fatalf("cold filtered clone downloaded a full mirror: %+v, %v", after, err)
			}
		}
	}
}

func TestConcurrentClones(t *testing.T) {
	f := newFixture(t)
	f.slow.Store(true)

	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan string, 8)
	for range 8 {
		destination := filepath.Join(t.TempDir(), "clone")
		wg.Go(func() {
			<-start
			cmd := exec.Command("git", "clone", f.local, destination)
			if output, err := cmd.CombinedOutput(); err != nil {
				failures <- string(output)
			}
		})
	}

	close(start)
	wg.Wait()
	close(failures)

	for failure := range failures {
		t.Error(failure)
	}
	if f.fetches.Load() != 1 {
		t.Fatalf("upstream object transfers=%d; want one mirror fetch", f.fetches.Load())
	}
}

func TestAuthenticationAndUnselectedRedirect(t *testing.T) {
	f := newFixture(t)

	for _, tc := range []struct{ path, auth string }{
		{"/git/example.com/owner/repo.git/info/refs?service=git-upload-pack", "Basic c2VjcmV0"},
		{"/git/example.com/owner/other.git/info/refs?service=git-upload-pack", ""},
		{"/git/example.com/owner/repo.git/info/lfs/objects/batch", "Bearer token"},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		f.cache.ServeHTTP(w, r)
		if w.Code != 307 || !strings.HasPrefix(w.Header().Get("Location"), "https://example.com/") {
			t.Fatalf("bypass: %d %s", w.Code, w.Body.String())
		}
	}

	stats, err := f.store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Git != 0 {
		t.Fatal("bypass stored data")
	}
}
