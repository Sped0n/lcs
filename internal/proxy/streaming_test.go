package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestForwardReportsTruncatedChunkedResponse(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "complete"
		if !complete {
			name = "truncated"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer closeStream(connection)
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
			p := New(nil)
			p.Transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			}
			server := httptest.NewServer(p)
			defer server.Close()
			defer p.Close()
			proxyURL, _ := url.Parse(server.URL)
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			response, requestErr := client.Get("http://example.com/artifact")
			var body []byte
			var bodyErr error
			if requestErr == nil {
				body, bodyErr = io.ReadAll(response.Body)
				closeBody(t, response.Body)
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
				t.Fatalf("truncated response appeared complete: %q", body)
			}
		})
	}
}
