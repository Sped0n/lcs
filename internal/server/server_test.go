package server

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spedon/lcs/internal/config"
)

func TestLifecycleAndControlSocket(t *testing.T) {
	root := t.TempDir()
	paths := config.Paths{State: filepath.Join(root, "state"), Cache: filepath.Join(root, "cache")}
	active := config.Config{Limit: 1000000, Repositories: []string{"https://example.com/owner/selected.git"}}

	certificate := []byte("legacy CA state")
	if err := os.MkdirAll(paths.State, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca.crt", "ca.key"} {
		if err := os.WriteFile(filepath.Join(paths.State, name), certificate, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() { done <- serve(ctx, active, paths, listener) }()
		stopped := false
		t.Cleanup(func() {
			cancel()
			if !stopped {
				if err := <-done; err != nil {
					t.Error(err)
				}
			}
		})

		deadline := time.Now().Add(5 * time.Second)
		for {
			var out bytes.Buffer
			err := Control(context.Background(), paths, "status", &out)
			if err == nil && strings.Contains(out.String(), "Service: running") {
				break
			}
			select {
			case err := <-done:
				stopped = true
				t.Fatalf("start: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("service did not become ready")
			}
			time.Sleep(10 * time.Millisecond)
		}

		client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: listener.Addr().String()})}}
		response, err := client.Get("http://127.0.0.1:1/not-git")
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		client.CloseIdleConnections()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("public listener accepted a non-Git proxy request: %d", response.StatusCode)
		}

		direct := &http.Client{Timeout: time.Second, Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		_, port, err := net.SplitHostPort(listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			host   string
			status int
		}{
			{listener.Addr().String(), http.StatusTemporaryRedirect},
			{net.JoinHostPort("localhost", port), http.StatusTemporaryRedirect},
			{net.JoinHostPort("example.com", port), http.StatusBadRequest},
			{"localhost:1", http.StatusBadRequest},
		} {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/git/example.com/owner/unselected.git/info/refs", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = tc.host
			response, err := direct.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status {
				t.Fatalf("Host %s: status %d, want %d", tc.host, response.StatusCode, tc.status)
			}
		}
		direct.CloseIdleConnections()

		settings, err := ShellConfig(t.Context(), paths)
		if err != nil {
			t.Fatal(err)
		}
		if len(settings.Repositories) != 1 || settings.Repositories[0] != "https://example.com/owner/selected.git" {
			t.Fatalf("daemon configuration: %+v", settings)
		}

		data, err := os.ReadFile(filepath.Join(paths.State, "ca.crt"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, certificate) {
			t.Fatal("restart modified legacy service state")
		}

		var out bytes.Buffer
		if err := Control(context.Background(), paths, "clear", &out); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(paths.State, "ca.key")); err != nil {
			t.Fatal("clear removed CA key")
		}

		cancel()
		select {
		case err := <-done:
			stopped = true
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("shutdown did not finish")
		}
	}
}
