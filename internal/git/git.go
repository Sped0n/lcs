package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/spedon/lcs/internal/config"
	"github.com/spedon/lcs/internal/storage"
)

type Cache struct {
	store         *storage.Store
	selected      map[string]bool
	flights       singleflight.Group
	git, proxyURL string
	upstream      *upstreamTransport
	mu            sync.Mutex
	jobs          sync.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
	downloads     chan struct{}
}

func New(c config.Config, store *storage.Store, transport *http.Transport) (*Cache, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("system Git is required: %w", err)
	}

	selected := make(map[string]bool)
	for _, remote := range c.Repositories {
		selected[remote] = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	upstream, err := newUpstream(ctx, c.Repositories, transport)
	if err != nil {
		cancel()
		return nil, err
	}

	return &Cache{store: store, selected: selected, git: binary, proxyURL: upstream.proxyURL, upstream: upstream, ctx: ctx, cancel: cancel, downloads: make(chan struct{}, 4)}, nil
}

func (c *Cache) Close() {
	c.mu.Lock()
	c.cancel()
	c.mu.Unlock()

	c.upstream.Close()
	c.jobs.Wait()
}

func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	remote, suffix, err := route(r.URL)
	if err != nil {
		http.Error(w, "invalid Git cache path", http.StatusBadRequest)
		return
	}

	eligible := c.selected[remote] && len(r.Header.Values("Authorization")) == 0 && len(r.Header.Values("Cookie")) == 0 && r.Header.Get("Origin") == "" &&
		(r.Method == http.MethodGet && suffix == "/info/refs" && r.URL.RawQuery == "service=git-upload-pack" || r.Method == http.MethodPost && suffix == "/git-upload-pack" && r.Header.Get("Content-Type") == "application/x-git-upload-pack-request")
	if !eligible {
		c.bypass(w, r, remote, suffix)
		return
	}

	// Advertise upstream refs before deciding which objects the client needs.
	// Redirecting here would move the entire operation away from LCS.
	if r.Method == http.MethodGet {
		c.forward(w, r, remote, suffix)
		return
	}

	fetch, ok := readFetch(r)
	if !ok {
		c.forward(w, r, remote, suffix)
		return
	}

	key := "git:" + remote
	var tags map[string]string
	if fetch.depth == 1 && !fetch.filtered {
		if fetch.includeTags {
			tags, err = c.shallowTags(r.Context(), remote, fetch.wants)
			if err != nil {
				log.Printf("Git shallow tags %s: %v", remote, err)
				c.forward(w, r, remote, suffix)
				return
			}
			for _, oid := range tags {
				fetch.wants = append(fetch.wants, oid)
			}
			slices.Sort(fetch.wants)
			fetch.wants = slices.Compact(fetch.wants)
		}
		key = shallowKey(remote, fetch.wants)
	}

	// Do not download full history for a cold filtered or incremental fetch.
	if fetch.filtered || fetch.incremental && fetch.depth == 0 {
		_, release, found, err := c.store.Acquire(key)
		if err == nil {
			release()
		}
		if err != nil || !found {
			if err != nil {
				log.Printf("Git cache read %s: %v", remote, err)
			}
			c.forward(w, r, remote, suffix)
			return
		}
	}

	c.mu.Lock()
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		http.Error(w, "cache is shutting down", http.StatusServiceUnavailable)
		return
	}

	pending := make(chan error, 1)
	c.jobs.Go(func() {
		_, err, _ := c.flights.Do(key, func() (any, error) {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
			defer cancel()
			stop := context.AfterFunc(c.ctx, cancel)
			defer stop()
			if fetch.depth == 1 && !fetch.filtered {
				_, release, found, err := c.acquire(ctx, key, fetch.wants)
				release()
				if err != nil {
					return nil, err
				}
				if found {
					return nil, c.store.Record(true)
				}
				return nil, c.prepareShallow(ctx, remote, key, fetch.wants, tags)
			}
			return nil, c.prepare(ctx, remote, fetch.wants)
		})
		pending <- err
	})
	c.mu.Unlock()

	select {
	case <-r.Context().Done():
		return
	case err := <-pending:
		if err != nil {
			log.Printf("Git cache bypass %s: %v", remote, err)
			c.forward(w, r, remote, suffix)
			return
		}
	}

	entry, release, found, err := c.acquire(r.Context(), key, fetch.wants)
	if err != nil || !found {
		if err != nil {
			log.Printf("Git cache read %s: %v", remote, err)
		}
		c.forward(w, r, remote, suffix)
		return
	}
	defer release()
	c.serveRepository(w, r, entry, suffix)
}

func (c *Cache) acquire(ctx context.Context, key string, wants []string) (storage.Entry, func(), bool, error) {
	entry, release, found, err := c.store.Acquire(key)
	if err != nil {
		return entry, func() {}, false, err
	}
	if !found {
		return entry, release, false, nil
	}

	output, err := c.commandInput(ctx, filepath.Join(entry.Path, "repo.git"), strings.NewReader(strings.Join(wants, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		release()
		return storage.Entry{}, func() {}, false, err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != len(wants) {
		release()
		return storage.Entry{}, func() {}, false, errors.New("invalid Git object lookup")
	}

	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != wants[i] || fields[1] == "missing" {
			release()
			return storage.Entry{}, func() {}, false, nil
		}
	}

	return entry, release, true, nil
}

func (c *Cache) serveRepository(w http.ResponseWriter, r *http.Request, entry storage.Entry, suffix string) {
	request := r.Clone(r.Context())
	request.URL.Path = "/repo.git" + suffix
	request.URL.RawPath = ""
	handler := &cgi.Handler{Path: c.git, Args: []string{"http-backend"}, Dir: entry.Path, Root: "/", Env: append(cleanGitEnv(), "GIT_PROJECT_ROOT="+entry.Path, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))}
	handler.ServeHTTP(w, request)
}

func route(u *url.URL) (string, string, error) {
	if u.RawPath != "" || strings.Contains(u.EscapedPath(), "%") {
		return "", "", errors.New("encoded Git paths are not supported")
	}

	path := strings.TrimPrefix(u.Path, "/git/")
	if path == u.Path {
		return "", "", errors.New("missing Git prefix")
	}

	for _, suffix := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"} {
		if strings.HasSuffix(path, suffix) {
			remote, err := config.RepositoryURL("https://" + strings.TrimSuffix(path, suffix))
			return remote, suffix, err
		}
	}

	// LFS and other non-Smart-HTTP paths go back to the original server.
	if index := strings.Index(path, ".git/"); index >= 0 {
		remote, err := config.RepositoryURL("https://" + path[:index+4])
		return remote, path[index+4:], err
	}

	return "", "", errors.New("unknown Git operation")
}

func (c *Cache) bypass(w http.ResponseWriter, r *http.Request, remote, suffix string) {
	target := remote + suffix
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}

	c.forward(w, r, remote, suffix)
}

func (c *Cache) forward(w http.ResponseWriter, r *http.Request, remote, suffix string) {
	target := remote + suffix
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	out := r.Clone(r.Context())
	upstream, err := url.Parse(target)
	if err != nil {
		http.Error(w, "invalid upstream URL", http.StatusBadRequest)
		return
	}
	out.URL = upstream
	out.Host = upstream.Host
	out.RequestURI = ""

	response, err := c.upstream.RoundTrip(out)
	if err != nil {
		log.Printf("Git passthrough: %v", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			log.Printf("close Git upstream response: %v", err)
		}
	}()
	if r.Method == http.MethodGet && response.StatusCode == http.StatusUnauthorized {
		c.bypass(w, r, remote, suffix)
		return
	}
	for key, values := range response.Header {
		w.Header()[key] = values
	}
	w.WriteHeader(response.StatusCode)

	if _, err := io.Copy(w, response.Body); err != nil {
		log.Printf("Git passthrough response: %v", err)
		panic(http.ErrAbortHandler)
	}
}

func cleanGitEnv() []string {
	return []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GIT_SSL_NO_VERIFY=0"}
}

func (c *Cache) command(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return c.commandInput(ctx, dir, nil, args...)
}

func (c *Cache) commandInput(ctx context.Context, dir string, input io.Reader, args ...string) ([]byte, error) {
	base := []string{"-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.proxy=" + c.proxyURL, "-c", "http.sslVerify=true", "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	cmd := exec.CommandContext(ctx, c.git, append(base, args...)...)
	cmd.Dir = dir
	cmd.Stdin = input
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") || strings.HasPrefix(value, "GIT_SSL_CAINFO=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, cleanGitEnv()...)
	cmd.Env = append(cmd.Env, "NO_PROXY=", "no_proxy=", "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null", "CURL_HOME=/dev/null")

	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}

	return output, nil
}
