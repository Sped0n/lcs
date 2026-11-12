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
	mu            sync.Mutex
	locks         map[string]*sync.Mutex
	flights       singleflight.Group
	git, proxyURL string
	upstream      http.RoundTripper
}

func New(c config.Config, store *storage.Store, upstream http.RoundTripper) (*Cache, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("system Git is required: %w", err)
	}
	selected := make(map[string]bool)
	for _, remote := range c.Repositories {
		selected[remote] = true
	}

	return &Cache{store: store, selected: selected, locks: map[string]*sync.Mutex{}, git: binary, proxyURL: config.Endpoint, upstream: upstream}, nil
}

func (c *Cache) lock(remote string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock := c.locks[remote]
	if lock == nil {
		lock = &sync.Mutex{}
		c.locks[remote] = lock
	}
	return lock
}

func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	remote, suffix, err := route(r.URL)
	if err != nil {
		http.Error(w, "invalid Git cache path", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	selected := c.selected[remote]
	c.mu.Unlock()
	eligible := selected && len(r.Header.Values("Authorization")) == 0 && len(r.Header.Values("Cookie")) == 0 && r.Header.Get("Origin") == "" &&
		(r.Method == http.MethodGet && suffix == "/info/refs" && r.URL.RawQuery == "service=git-upload-pack" || r.Method == http.MethodPost && suffix == "/git-upload-pack" && r.Header.Get("Content-Type") == "application/x-git-upload-pack-request")
	if !eligible {
		c.bypass(w, r, remote, suffix)
		return
	}
	if r.Method == http.MethodGet {
		_, err, _ := c.flights.Do(remote, func() (any, error) {
			lock := c.lock(remote)
			lock.Lock()
			defer lock.Unlock()
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
			defer cancel()
			return nil, c.prepare(ctx, remote)
		})
		if err != nil {
			log.Printf("Git cache bypass: %v", err)
			c.bypass(w, r, remote, suffix)
			return
		}
	}
	lock := c.lock(remote)
	lock.Lock()
	defer lock.Unlock()
	entry, release, found, err := c.store.Acquire("git:" + remote)
	if err != nil {
		log.Printf("Git metadata: %v", err)
	}
	if !found || err != nil {
		c.bypass(w, r, remote, suffix)
		return
	}
	defer release()
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
	base := []string{"-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.proxy=" + c.proxyURL, "-c", "http.sslVerify=true", "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	cmd := exec.CommandContext(ctx, c.git, append(base, args...)...)
	cmd.Dir = dir
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

func (c *Cache) prepare(ctx context.Context, remote string) error {
	refs, err := c.command(ctx, "", "ls-remote", "--symref", remote)
	if err != nil {
		return err
	}
	upstream, err := parseRefSnapshot(refs)
	if err != nil {
		return err
	}
	entry, release, found, err := c.store.Acquire("git:" + remote)
	if err != nil {
		return err
	}
	defer release()
	if found {
		cached, err := c.localSnapshot(ctx, filepath.Join(entry.Path, "repo.git"))
		if err != nil {
			return err
		}
		if cached.fingerprint == upstream.fingerprint {
			return c.store.Record(true, 0)
		}
	}
	if err := c.store.Record(false, 0); err != nil {
		return err
	}
	reservation, err := c.store.Reserve(0)
	if err != nil {
		return err
	}
	defer func() {
		if err := reservation.Abort(); err != nil {
			log.Printf("remove Git staging: %v", err)
		}
	}()
	stage := filepath.Join(reservation.Path, "repo.git")
	if found {
		if entry.Size > reservation.Limit {
			return storage.ErrCapacity
		}
		if err := copyRepository(filepath.Join(entry.Path, "repo.git"), stage); err != nil {
			return err
		}
	}
	// Git does not expose a byte reservation API. Monitor all staging files and cancel
	// before publication if the reservation is exceeded; this is not a filesystem quota.
	download, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	checked := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				checked <- nil
				return
			case <-download.Done():
				checked <- download.Err()
				return
			case <-ticker.C:
				size, err := storage.DiskUsage(reservation.Path)
				if err != nil || size > reservation.Limit {
					cancel()
					if err == nil {
						err = storage.ErrCapacity
					}
					checked <- err
					return
				}
			}
		}
	}()
	if found {
		_, err = c.command(download, stage, "fetch", "--prune", "--force", "--no-auto-maintenance", "origin", "+refs/*:refs/*", "HEAD")
	} else {
		_, err = c.command(download, "", "clone", "--mirror", "--", remote, stage)
	}
	close(done)
	checkErr := <-checked
	if err != nil || checkErr != nil {
		return errors.Join(err, checkErr)
	}
	refs, err = c.command(ctx, "", "ls-remote", "--symref", remote)
	if err != nil {
		return err
	}
	upstream, err = parseRefSnapshot(refs)
	if err != nil {
		return err
	}
	if upstream.headRef != "" {
		_, err = c.command(ctx, stage, "symbolic-ref", "HEAD", upstream.headRef)
	} else {
		_, err = c.command(ctx, stage, "update-ref", "--no-deref", "HEAD", upstream.headOID)
	}
	if err != nil {
		return err
	}
	staged, err := c.localSnapshot(ctx, stage)
	if err != nil {
		return err
	}
	if staged.fingerprint != upstream.fingerprint {
		return errors.New("upstream refs changed during cache refresh")
	}
	if _, err := c.command(ctx, stage, "fsck", "--connectivity-only"); err != nil {
		return err
	}
	for _, setting := range [][2]string{{"uploadpack.allowFilter", "true"}, {"http.receivepack", "false"}, {"uploadpack.allowReachableSHA1InWant", "true"}} {
		if _, err := c.command(ctx, stage, "config", setting[0], setting[1]); err != nil {
			return err
		}
	}
	release()
	return reservation.Publish("git:"+remote, "git", staged.fingerprint)
}

func copyRepository(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("unexpected Git repository file type")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return errors.Join(err, in.Close())
		}
		_, copyErr := io.Copy(out, in)
		return errors.Join(copyErr, out.Close(), in.Close())
	})
}
