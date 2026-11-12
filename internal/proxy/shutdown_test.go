package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/spedon/lcs/internal/cert"
)

func TestCloseReleasesSilentTunnel(t *testing.T) {
	upstream, peer := net.Pipe()
	p := New(nil)
	p.Transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return upstream, nil
	}
	server := httptest.NewServer(p)
	defer func() {
		closeStream(peer)
		p.Close()
		server.Close()
	}()
	client, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream(client)
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(client, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200 Connection Established") {
		t.Fatalf("CONNECT: %q %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy shutdown blocked on a silent upstream")
	}
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("upstream was not closed: %v", err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("client was not closed: %v", err)
	}
}

func TestCloseCancelsInterceptedRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
		select {
		case finished <- struct{}{}:
		default:
		}
	}))
	defer upstream.Close()
	ca, err := cert.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New(ca)
	p.Intercept = func(host string) bool { return host == "example.com" }
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p.Transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	p.Transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	var clientDone chan struct{}
	server := httptest.NewServer(p)
	defer func() {
		close(release)
		p.Close()
		server.Close()
		if clientDone != nil {
			<-clientDone
		}
	}()
	proxyURL, _ := url.Parse(server.URL)
	proxyURL.User = url.UserPassword("lcs-intercept", "1")
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(ca.Certificate)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientRoots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	clientDone = make(chan struct{})
	go func() {
		defer close(clientDone)
		response, err := client.Get("https://example.com/artifact")
		if err == nil {
			closeBody(t, response.Body)
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("intercepted request did not reach upstream")
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy shutdown blocked on an intercepted request")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel the upstream request")
	}
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not release the client")
	}
}
