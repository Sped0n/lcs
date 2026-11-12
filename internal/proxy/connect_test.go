package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/spedon/lcs/internal/cert"
)

func TestConnectTrustAndSelectiveInterception(t *testing.T) {
	for _, mode := range []string{"intercept", "untrusted-tunnel", "unsupported-tunnel", "upstream-untrusted"} {
		t.Run(mode, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Authorization") != "Bearer upstream-only" {
					t.Error("credentials changed across CONNECT")
				}
				if _, err := io.WriteString(w, "verified upstream body"); err != nil {
					t.Error(err)
				}
			}))
			defer upstream.Close()
			ca, err := cert.Ensure(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := New(ca)
			p.Intercept = func(host string) bool { return mode != "unsupported-tunnel" && host == "example.com" }
			p.Transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			}
			upstreamRoots := x509.NewCertPool()
			upstreamRoots.AddCert(upstream.Certificate())
			p.Transport.TLSClientConfig = &tls.Config{RootCAs: upstreamRoots}
			if mode == "upstream-untrusted" {
				p.Transport.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool()}
			}
			server := httptest.NewServer(p)
			defer server.Close()
			defer p.Close()
			proxyURL, _ := url.Parse(server.URL)
			clientRoots := upstreamRoots
			if mode != "untrusted-tunnel" {
				proxyURL.User = url.UserPassword("lcs-intercept", "1")
			}
			if mode == "intercept" || mode == "upstream-untrusted" {
				clientRoots = x509.NewCertPool()
				clientRoots.AddCert(ca.Certificate)
			}
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientRoots}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport}
			r, _ := http.NewRequest("GET", "https://example.com/artifact", nil)
			r.Header.Set("Authorization", "Bearer upstream-only")
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "upstream-untrusted" {
				if response.StatusCode != 502 {
					t.Fatal("untrusted upstream accepted")
				}
				return
			}
			if response.StatusCode != 200 || string(data) != "verified upstream body" {
				t.Fatalf("CONNECT %d %q", response.StatusCode, data)
			}
		})
	}
}

func TestPrivateRedirectRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1/private")
		w.WriteHeader(302)
	}))
	defer upstream.Close()
	p := New(nil)
	defer p.Close()
	p.Transport = http.DefaultTransport.(*http.Transport).Clone()
	p.Transport.Proxy = nil
	server := httptest.NewServer(p)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 502 {
		t.Fatalf("unsafe redirect returned status %d", response.StatusCode)
	}
}
