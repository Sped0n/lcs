package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func cloneAsync(ctx context.Context, remote, destination string) <-chan error {
	done := make(chan error, 1)
	go func() {
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", remote, destination)
		cmd.WaitDelay = time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("clone: %w: %s", err, out)
		}
		done <- err
	}()

	return done
}

func TestDifferentShallowRepositoriesShareBudget(t *testing.T) {
	f := newFixture(t, "https://example.com/owner/other.git")
	gitRun(t, "", "clone", "--bare", f.work, filepath.Join(filepath.Dir(f.remote), "other.git"))

	started := make(chan struct{}, 2)
	resume := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	defer unblock()

	f.hooksMu.Lock()
	f.afterFetch = func() error {
		started <- struct{}{}
		<-resume
		return nil
	}
	f.hooksMu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	other := strings.TrimSuffix(f.local, "repo.git") + "other.git"
	first := cloneAsync(ctx, f.local, filepath.Join(t.TempDir(), "first"))
	second := cloneAsync(ctx, other, filepath.Join(t.TempDir(), "second"))

	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("independent repository downloads did not overlap")
		}
	}

	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	gitRun(t, "", "clone", "--depth=1", f.local, filepath.Join(t.TempDir(), "first-warm"))
	gitRun(t, "", "clone", "--depth=1", other, filepath.Join(t.TempDir(), "second-warm"))
	if f.fetches.Load() != 2 {
		t.Fatalf("one repository crowded the other out of the cache: %d upstream fetches", f.fetches.Load())
	}
}

type heldResponse struct {
	http.ResponseWriter
	started chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (w *heldResponse) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.resume
	return w.ResponseWriter.Write(data)
}

func (w *heldResponse) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func TestCachedReadersDoNotSerialize(t *testing.T) {
	f := newFixture(t)
	gitRun(t, "", "clone", "--depth=1", f.local, filepath.Join(t.TempDir(), "warm"))

	started := make(chan struct{})
	resume := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	defer unblock()

	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			closeErr := r.Body.Close()
			if err != nil || closeErr != nil {
				t.Errorf("read clone request: %v %v", err, closeErr)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			if bytes.Contains(data, []byte("want ")) {
				w = &heldResponse{ResponseWriter: w, started: started, resume: resume}
			}
		}
		f.cache.ServeHTTP(w, r)
	}))
	t.Cleanup(firstServer.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	clone := func(remote string) <-chan error {
		return cloneAsync(ctx, remote, filepath.Join(t.TempDir(), "client"))
	}

	first := clone(firstServer.URL + "/git/example.com/owner/repo.git")
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first reader never reached the cached response")
	}

	second := clone(f.local)
	var secondErr error
	select {
	case secondErr = <-second:
	case <-ctx.Done():
		secondErr = errors.New("second clone waited for the stalled reader")
	}

	unblock()
	firstErr := <-first
	if firstErr != nil || secondErr != nil {
		t.Fatalf("concurrent readers: first=%v, second=%v", firstErr, secondErr)
	}
}

func TestCanceledShallowDownloadCleansStaging(t *testing.T) {
	f := newFixture(t)

	started := make(chan struct{})
	resume := make(chan struct{})
	var first sync.Once
	defer close(resume)

	f.hooksMu.Lock()
	f.afterFetch = func() error {
		first.Do(func() { close(started) })
		<-resume
		return nil
	}
	f.hooksMu.Unlock()

	oid := strings.TrimSpace(gitRun(t, f.work, "rev-parse", "HEAD"))
	packet := func(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }
	body := packet("command=fetch\n") + "0001" + packet("want "+oid+"\n") + packet("deepen 1\n") + packet("done\n") + "0000"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.local+"/git-upload-pack", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	request.Header.Set("Git-Protocol", "version=2")

	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			err = response.Body.Close()
		}
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("shallow download never started")
	}

	stats, err := f.store.Stats()
	if err != nil || stats.Temporary == 0 {
		t.Fatalf("download did not reserve staging: %+v, %v", stats, err)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("client cancellation: %v", err)
	}
	closed := make(chan struct{})
	go func() { f.cache.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cache shutdown did not join the canceled download")
	}
	stats, err = f.store.Stats()
	if err != nil || stats.Git != 0 || stats.Temporary != 0 {
		t.Fatalf("canceled download retained data: %+v, %v", stats, err)
	}
}
