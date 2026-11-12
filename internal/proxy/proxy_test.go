package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func closeBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Error(err)
	}
}

func TestForwardAndLoopProtection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("authentication changed or proxy credentials leaked")
		}
		w.WriteHeader(201)
		if _, err := io.WriteString(w, r.Method+" "+r.URL.String()); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	p := New(nil)
	defer p.Close()
	server := httptest.NewServer(p)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	request, _ := http.NewRequest("POST", upstream.URL+"/object?q=1", strings.NewReader("body"))
	request.Header.Set("Authorization", "Bearer secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, response.Body)
	if response.StatusCode != 502 {
		t.Fatalf("local upstream allowed: %d", response.StatusCode)
	}
	// Inject only the dial boundary so the real forward protocol can use a local fixture.
	p.Transport = http.DefaultTransport.(*http.Transport).Clone()
	p.Transport.Proxy = nil
	request.Body, err = request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, response.Body)
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 201 || string(data) != "POST /object?q=1" {
		t.Fatalf("forward: %d %q", response.StatusCode, data)
	}
}

func TestPrivateTargetsRejected(t *testing.T) {
	p := New(nil)
	defer p.Close()
	server := httptest.NewServer(p)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, target := range []string{"http://127.0.0.1/", "http://[::1]/", "http://[::ffff:127.0.0.1]/", "http://169.254.169.254/", "http://10.1.2.3/", "http://100.64.0.1/", "http://[fd00::1]/"} {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		closeBody(t, response.Body)
		if response.StatusCode != 502 {
			t.Errorf("%s: status %d", target, response.StatusCode)
		}
	}
}
