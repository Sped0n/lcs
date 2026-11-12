package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestCustomPortForwardAndRedirect(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != "1.1.1.1:6688" || r.Header.Get("Authorization") != "Bearer upstream-only" {
			t.Errorf("upstream request changed: host=%q authorization=%q", r.Host, r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "http://1.1.1.1:6688/artifact")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if _, err := io.WriteString(w, "custom-port artifact"); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	p := New(nil)
	defer p.Close()
	p.Transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "1.1.1.1:6688" {
			t.Errorf("custom port changed to %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	server := httptest.NewServer(p)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequest("GET", "http://1.1.1.1:6688/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer upstream-only")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, response.Body)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "custom-port artifact" || calls.Load() != 2 {
		t.Fatalf("custom-port redirect: status=%d body=%q upstream requests=%d", response.StatusCode, body, calls.Load())
	}
}

func TestPublicDialAcceptsCustomPort(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	connection, err := PublicDial(ctx, "tcp", "1.1.1.1:6688")
	if connection != nil {
		closeStream(connection)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("valid custom port rejected before dialing: %v", err)
	}
}

func TestConnectRejectsInvalidAndPrivateTargets(t *testing.T) {
	p := New(nil)
	defer p.Close()
	server := httptest.NewServer(p)
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, tc := range []struct {
		target string
		status int
	}{
		{"example.com:0", http.StatusBadRequest},
		{"example.com:65536", http.StatusBadRequest},
		{"example.com:https", http.StatusBadRequest},
		{"example.com:", http.StatusBadRequest},
		{"127.0.0.1:6688", http.StatusBadGateway},
		{"[::1]:6688", http.StatusBadGateway},
		{"10.1.2.3:6688", http.StatusBadGateway},
	} {
		t.Run(tc.target, func(t *testing.T) {
			request, err := http.NewRequest("CONNECT", server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = tc.target
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			closeBody(t, response.Body)
			if response.StatusCode != tc.status {
				t.Fatalf("CONNECT %s: status=%d; want %d", tc.target, response.StatusCode, tc.status)
			}
		})
	}
}
