package toolchain

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spedon/lcs/internal/storage"
)

const artifactURL = "https://chrome-infra-packages.appspot.com/client?platform=linux-amd64&version=git_revision:0123456789abcdef"
const payload = "a public toolchain artifact\n"

func writePayload(t *testing.T, w io.Writer) {
	t.Helper()
	if _, err := io.WriteString(w, payload); err != nil {
		t.Error(err)
	}
}

func closeBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Error(err)
	}
}

func newCache(t *testing.T, limit int64, handler http.Handler) (*Cache, *storage.Store, string) {
	t.Helper()
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	root := t.TempDir()
	store, err := storage.Open(root, filepath.Join(root, "cache"), limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return New(store, transport), store, filepath.Join(root, "cache")
}

func readResponse(t *testing.T, client *http.Client, raw string, headers http.Header) string {
	t.Helper()
	r, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header = headers
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, response.Body)
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, data)
	}
	return string(data)
}

func TestFreshHitVaryAndStatistics(t *testing.T) {
	var calls atomic.Int64
	cache, store, _ := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Vary", "Accept-Language")
		writePayload(t, w)
	}))
	client := &http.Client{Transport: cache}
	for range 2 {
		if got := readResponse(t, client, artifactURL, nil); got != payload {
			t.Fatal(got)
		}
	}
	readResponse(t, client, artifactURL, http.Header{"Accept-Language": []string{"de"}})
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || stats.Hits != 1 || stats.Misses != 2 || stats.Saved != int64(len(payload)) {
		t.Fatalf("calls=%d stats=%+v", calls.Load(), stats)
	}
	response, err := client.Get(artifactURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, response.Body, 3); err != nil {
		t.Fatal(err)
	}
	closeBody(t, response.Body)
	stats, err = store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 2 || stats.Saved != int64(len(payload)+3) {
		t.Fatalf("unread bytes counted as saved: %+v", stats)
	}
}

func TestRevalidation(t *testing.T) {
	for _, validator := range []string{"ETag", "Last-Modified"} {
		t.Run(validator, func(t *testing.T) {
			var calls, conditionals atomic.Int64
			cache, _, _ := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "max-age=0")
				name, value := "If-None-Match", "\"v1\""
				if validator == "Last-Modified" {
					name = "If-Modified-Since"
					value = time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
				}
				w.Header().Set(validator, value)
				if r.Header.Get(name) != "" {
					conditionals.Add(1)
					w.Header().Set("Cache-Control", "max-age=3600")
					w.WriteHeader(304)
					return
				}
				writePayload(t, w)
			}))
			client := &http.Client{Transport: cache}
			for range 3 {
				if got := readResponse(t, client, artifactURL, nil); got != payload {
					t.Fatal(got)
				}
			}
			if calls.Load() != 2 || conditionals.Load() != 1 {
				t.Fatalf("calls=%d conditionals=%d", calls.Load(), conditionals.Load())
			}
		})
	}
}

func TestIneligibleResponsesBypass(t *testing.T) {
	for _, headers := range []http.Header{
		{"Cache-Control": []string{"no-store"}},
		{"Cache-Control": []string{"private, max-age=9999"}},
		{"Cache-Control": []string{"max-age=9999"}, "Set-Cookie": []string{"session=secret"}},
		{"Cache-Control": []string{"max-age=9999"}, "Vary": []string{"*"}},
	} {
		t.Run(fmt.Sprint(headers), func(t *testing.T) {
			var calls atomic.Int64
			cache, store, _ := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for key, values := range headers {
					w.Header()[key] = values
				}
				writePayload(t, w)
			}))
			client := &http.Client{Transport: cache}
			for range 2 {
				readResponse(t, client, artifactURL, nil)
			}
			stats, err := store.Stats()
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || stats.Toolchain != 0 {
				t.Fatalf("calls=%d stats=%+v", calls.Load(), stats)
			}
		})
	}
}

func TestOriginalRequestBypass(t *testing.T) {
	for _, headers := range []http.Header{
		{"Authorization": []string{"Bearer private"}}, {"Cookie": []string{"secret=1"}},
		{"Range": []string{"bytes=0-5"}}, {"If-None-Match": []string{"\"v1\""}},
		{"Cache-Control": []string{"no-cache"}},
	} {
		t.Run(fmt.Sprint(headers), func(t *testing.T) {
			var calls atomic.Int64
			cache, store, _ := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for key := range headers {
					if r.Header.Get(key) != headers.Get(key) {
						t.Errorf("%s changed", key)
					}
				}
				w.Header().Set("Cache-Control", "public,max-age=3600")
				writePayload(t, w)
			}))
			client := &http.Client{Transport: cache}
			for range 2 {
				readResponse(t, client, artifactURL, headers)
			}
			stats, err := store.Stats()
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || stats.Toolchain != 0 {
				t.Fatalf("calls=%d stats=%+v", calls.Load(), stats)
			}
		})
	}
}

func TestInterruptedOversizedAndBadDigest(t *testing.T) {
	for _, mode := range []string{"interrupted", "oversized", "digest"} {
		t.Run(mode, func(t *testing.T) {
			limit := int64(10000)
			if mode == "oversized" {
				limit = 1
			}
			cache, store, _ := newCache(t, limit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "max-age=3600")
				if mode == "digest" {
					w.Header().Set("Digest", "sha-256=not-a-digest")
				}
				writePayload(t, w)
			}))
			client := &http.Client{Transport: cache}
			response, err := client.Get(artifactURL)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "interrupted" {
				_, err = io.CopyN(io.Discard, response.Body, 3)
			} else {
				_, err = io.Copy(io.Discard, response.Body)
			}
			if err != nil {
				t.Fatal(err)
			}
			closeBody(t, response.Body)
			stats, err := store.Stats()
			if err != nil {
				t.Fatal(err)
			}
			if stats.Toolchain != 0 || stats.Temporary != 0 {
				t.Fatalf("partial object published: %+v", stats)
			}
		})
	}
}

func TestConcurrentDownloads(t *testing.T) {
	var calls atomic.Int64
	cache, _, _ := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Cache-Control", "max-age=3600")
		writePayload(t, w)
	}))
	client := &http.Client{Transport: cache}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			response, err := client.Get(artifactURL)
			if err != nil {
				failures <- err
				return
			}
			data, err := io.ReadAll(response.Body)
			closeBody(t, response.Body)
			if err != nil {
				failures <- err
			} else if string(data) != payload {
				failures <- fmt.Errorf("wrong body")
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream downloads=%d", calls.Load())
	}
}

func TestCorruptionAndRedirect(t *testing.T) {
	var calls atomic.Int64
	cache, store, root := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/client" {
			w.Header().Set("Location", "https://storage.googleapis.com/cipd-bucket/object?signature=public-redirect")
			w.WriteHeader(302)
			return
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		writePayload(t, w)
	}))
	client := &http.Client{Transport: cache}
	for range 2 {
		if got := readResponse(t, client, artifactURL, nil); got != payload {
			t.Fatal(got)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("redirect not cached: %d", calls.Load())
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Saved != int64(len(payload)) {
		t.Fatal(stats)
	}
	// The immutable body hash is checked before reuse.
	var path string
	entries, err := os.ReadDir(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		files, err := os.ReadDir(filepath.Join(root, "artifacts", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if len(file.Name()) == 64 {
				path = filepath.Join(root, "artifacts", entry.Name(), file.Name())
			}
		}
	}
	if path == "" {
		t.Fatal("no artifact")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", len(payload))), 0o600); err != nil {
		t.Fatal(err)
	}
	readResponse(t, client, artifactURL, nil)
	if calls.Load() != 5 {
		t.Fatalf("corrupt body reused: %d", calls.Load())
	}
}
