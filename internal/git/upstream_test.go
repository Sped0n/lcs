package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitTransportRejectsPrivateTargets(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "169.254.169.254", "10.1.2.3", "100.64.0.1", "fd00::1", "2001:db8::1"} {
		if _, err := publicAddresses(t.Context(), host); err == nil {
			t.Errorf("accepted non-public address %s", host)
		}
	}

	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Error("unselected CONNECT reached the dialer")
		return nil, errors.New("unexpected dial")
	}}
	u, err := newUpstream(t.Context(), []string{"https://example.com/owner/repo.git"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	for _, authority := range []string{"other.example:443", "example.com:6688", "127.0.0.1:443"} {
		request := httptest.NewRequest(http.MethodConnect, "http://unused", nil)
		request.Host = authority
		response := httptest.NewRecorder()
		u.connect(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("CONNECT %s: status %d", authority, response.Code)
		}
	}

	for _, target := range []string{"https://127.0.0.1/", "https://[::1]/", "https://example.com:6688/", "http://example.com/", "https://user:secret@example.com/"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if err := validateRedirect(t.Context(), request.URL); err == nil {
			t.Errorf("accepted unsafe redirect %s", target)
		}
	}
}

func TestGitTransportPreservesOriginCredentials(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if string(data) != "git body" || r.Header.Get("Authorization") != "Basic dXNlcjp0ZXN0" || r.Header.Get("Cookie") != "session=test" || r.Header.Get("Git-Protocol") != "version=2" {
			t.Errorf("origin request changed: %q %v", data, r.Header)
		}
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Hop") != "" {
			t.Error("proxy credentials or hop headers reached origin")
		}
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "private-hop-value")
		if _, err := w.Write([]byte("response")); err != nil {
			t.Error(err)
		}
	}))
	defer origin.Close()

	transport := origin.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}

	u, err := newUpstream(t.Context(), []string{"https://example.com/owner/repo.git"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	request := httptest.NewRequest(http.MethodPost, "https://example.com/owner/repo.git/git-upload-pack", strings.NewReader("git body"))
	request.Header.Set("Authorization", "Basic dXNlcjp0ZXN0")
	request.Header.Set("Cookie", "session=test")
	request.Header.Set("Git-Protocol", "version=2")
	request.Header.Set("Proxy-Authorization", "Basic internal-proxy-secret")
	request.Header.Set("Connection", "X-Hop")
	request.Header.Set("X-Hop", "private-hop-value")

	response, err := u.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || string(data) != "response" || response.Header.Get("X-Hop") != "" {
		t.Fatalf("response changed: %q %v %v %v", data, response.Header, err, closeErr)
	}
}

func TestCloseReleasesSilentGitTunnel(t *testing.T) {
	upstream, peer := net.Pipe()
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return upstream, nil }}

	u, err := newUpstream(t.Context(), []string{"https://example.com/owner/repo.git"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { closeStream(peer); u.Close() }()

	client, err := net.Dial("tcp", strings.TrimPrefix(u.proxyURL, "http://"))
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
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT failed: %v, %v", response, err)
	}
	defer closeStream(response.Body)

	closed := make(chan struct{})
	go func() { u.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Git transport shutdown blocked on a silent upstream")
	}

	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("upstream was not closed: %v", err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("client was not closed: %v", err)
	}
}
