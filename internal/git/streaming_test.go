package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spedon/lcs/internal/config"
	"github.com/spedon/lcs/internal/storage"
)

func TestPassthroughReportsTruncatedChunkedResponse(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "complete"
		if !complete {
			name = "truncated"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				connection, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() {
					if err := connection.Close(); err != nil {
						t.Error(err)
					}
				}()
				wire := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n7\r\npartial\r\n"
				if complete {
					wire += "0\r\n\r\n"
				}
				if _, err := fmt.Fprint(buffer, wire); err != nil {
					t.Error(err)
				}
				if err := buffer.Flush(); err != nil {
					t.Error(err)
				}
			}))
			defer upstream.Close()

			transport := upstream.Client().Transport.(*http.Transport).Clone()
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			}
			defer transport.CloseIdleConnections()

			store, err := storage.Open(t.TempDir(), filepath.Join(t.TempDir(), "cache"), 10000)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()

			cache, err := New(config.Config{Repositories: []string{"https://example.com/owner/repo.git"}}, store, transport)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()

			server := httptest.NewServer(cache)
			defer server.Close()

			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()

			response, requestErr := client.Post(server.URL+"/git/example.com/owner/repo.git/info/lfs/objects/batch", "application/json", strings.NewReader("{}"))

			var body []byte
			var bodyErr error
			if requestErr == nil {
				body, bodyErr = io.ReadAll(response.Body)
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
			}
			if complete {
				if requestErr != nil || bodyErr != nil || response.StatusCode != 200 || string(body) != "partial" {
					t.Fatalf("complete response: request=%v body=%v payload=%q", requestErr, bodyErr, body)
				}
				return
			}
			if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(bodyErr, context.DeadlineExceeded) {
				t.Fatal("truncated response timed out instead of aborting")
			}
			if requestErr == nil && bodyErr == nil && response.StatusCode == 200 {
				t.Fatalf("truncated passthrough response appeared complete: %q", body)
			}
		})
	}
}
