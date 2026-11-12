package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spedon/lcs/internal/cert"
)

type Proxy struct {
	Transport *http.Transport
	Artifacts http.RoundTripper
	CA        *cert.Authority
	Intercept func(string) bool
	mu        sync.Mutex
	tunnels   map[*tunnel]struct{}
	closed    bool
	wg        sync.WaitGroup
}

func New(ca *cert.Authority) *Proxy {
	return &Proxy{CA: ca, Transport: &http.Transport{Proxy: nil, DialContext: PublicDial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: time.Minute, IdleConnTimeout: 90 * time.Second, DisableCompression: true}, tunnels: make(map[*tunnel]struct{})}
}

// PublicDial validates all DNS answers and connects to an already-validated address.
func PublicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if !validPort(port) {
		return nil, errors.New("upstream port must be between 1 and 65535")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("upstream has no addresses")
	}
	for _, address := range addresses {
		if !PublicIP(address) {
			return nil, errors.New("upstream resolves to a non-public address")
		}
	}
	var last error
	for _, address := range addresses {
		connection, err := (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		if err == nil {
			return connection, nil
		}
		last = err
	}
	return nil, last
}

func validPort(port string) bool {
	number, err := strconv.ParseUint(port, 10, 16)
	return err == nil && number != 0
}

func PublicIP(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b:1::/48", "64:ff9b::/96", "2002::/16", "2001::/32"} {
		if netip.MustParsePrefix(raw).Contains(address) {
			return false
		}
	}
	return true
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "absolute proxy URL required", http.StatusBadRequest)
		return
	}
	response, err := p.RoundTrip(r)
	if err != nil {
		log.Printf("proxy: %v", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer closeStream(response.Body)
	stripHop(response.Header)
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		log.Printf("proxy response: %v", err)
		panic(http.ErrAbortHandler)
	}
}

func (p *Proxy) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "http" && r.URL.Scheme != "https" || r.URL.User != nil || r.URL.Hostname() == "" {
		return nil, errors.New("invalid upstream URL")
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = out.URL.Host
	stripHop(out.Header)
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	transport := http.RoundTripper(p.Transport)
	if p.Artifacts != nil {
		transport = p.Artifacts
	}
	response, err := transport.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.Header.Get("Location") != "" {
		target, err := out.URL.Parse(response.Header.Get("Location"))
		if err == nil {
			err = validateRedirect(r.Context(), target)
		}
		if err != nil {
			closeStream(response.Body)
			return nil, fmt.Errorf("unsafe upstream redirect: %w", err)
		}
	}
	return response, nil
}

func validateRedirect(ctx context.Context, u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return errors.New("invalid redirect URL")
	}
	if port := u.Port(); port != "" && !validPort(port) {
		return errors.New("redirect uses an invalid TCP port")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		return errors.New("redirect has no addresses")
	}
	for _, address := range addresses {
		if !PublicIP(address) {
			return errors.New("redirect resolves to a non-public address")
		}
	}
	return nil
}

func stripHop(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" || !validPort(port) || strings.ContainsAny(host, "/\\@%") {
		http.Error(w, "CONNECT requires a hostname and TCP port between 1 and 65535", http.StatusBadRequest)
		return
	}
	intercept := false
	if credentials := r.Header.Get("Proxy-Authorization"); credentials != "" {
		copy := r.Clone(r.Context())
		copy.Header.Set("Authorization", credentials)
		username, _, ok := copy.BasicAuth()
		intercept = port == "443" && ok && username == "lcs-intercept" && p.Intercept != nil && p.Intercept(strings.ToLower(host))
	}
	var leaf tls.Certificate
	var upstream net.Conn
	if intercept {
		leaf, err = p.CA.Leaf(host)
	} else {
		upstream, err = p.Transport.DialContext(r.Context(), "tcp", r.Host)
	}
	if err != nil {
		http.Error(w, "CONNECT upstream failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		if upstream != nil {
			closeStream(upstream)
		}
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		if upstream != nil {
			closeStream(upstream)
		}
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	t := &tunnel{client: client, upstream: upstream, cancel: cancel}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		t.close()
		return
	}
	p.tunnels[t] = struct{}{}
	p.wg.Add(1)
	p.mu.Unlock()
	defer func() {
		t.close()
		p.mu.Lock()
		delete(p.tunnels, t)
		p.mu.Unlock()
		p.wg.Done()
	}()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	if err := client.SetDeadline(time.Now().Add(30 * time.Minute)); err != nil {
		return
	}
	connection := &bufferedConn{Conn: client, reader: buffered.Reader}
	if intercept {
		secure := tls.Server(connection, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
		defer closeStream(secure)
		if err := secure.HandshakeContext(ctx); err != nil {
			return
		}
		p.serveTLS(ctx, secure, r.Host)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(upstream, connection); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("tunnel upstream: %v", err)
		}
		if tcp, ok := upstream.(*net.TCPConn); ok {
			if err := tcp.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("close upstream write: %v", err)
			}
		}
	}()
	if _, err := io.Copy(connection, upstream); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("tunnel downstream: %v", err)
	}
	t.close()
	<-done
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(data []byte) (int, error) { return c.reader.Read(data) }

func (p *Proxy) serveTLS(ctx context.Context, connection net.Conn, authority string) {
	reader := bufio.NewReader(connection)
	for {
		r, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if !strings.EqualFold(r.Host, authority) && !strings.EqualFold(r.Host, strings.TrimSuffix(authority, ":443")) || r.URL.IsAbs() || r.Method == http.MethodConnect {
			closeStream(r.Body)
			return
		}
		r.URL = &url.URL{Scheme: "https", Host: authority, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		r = r.WithContext(ctx)
		response, err := p.RoundTrip(r)
		closeStream(r.Body)
		if err != nil {
			if _, err := fmt.Fprint(connection, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"); err != nil {
				log.Printf("proxy error response: %v", err)
			}
			return
		}
		stripHop(response.Header)
		err = response.Write(connection)
		closeStream(response.Body)
		if err != nil || r.Close || response.Close {
			return
		}
	}
}

func closeStream(stream io.Closer) {
	if err := stream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("close proxy stream: %v", err)
	}
}

type tunnel struct {
	client, upstream net.Conn
	cancel           context.CancelFunc
}

func (t *tunnel) close() {
	t.cancel()
	closeStream(t.client)
	if t.upstream != nil {
		closeStream(t.upstream)
	}
}

func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	tunnels := make([]*tunnel, 0, len(p.tunnels))
	for t := range p.tunnels {
		tunnels = append(tunnels, t)
	}
	p.mu.Unlock()
	for _, t := range tunnels {
		t.close()
	}
	p.wg.Wait()
	p.Transport.CloseIdleConnections()
}
