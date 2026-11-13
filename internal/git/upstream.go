package git

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

type upstreamTransport struct {
	transport *http.Transport
	hosts     map[string]bool
	proxyURL  string
	server    *http.Server
	served    chan struct{}
	mu        sync.Mutex
	tunnels   map[*gitTunnel]struct{}
	closed    bool
	wg        sync.WaitGroup
}

func newUpstream(ctx context.Context, repositories []string, transport *http.Transport) (*upstreamTransport, error) {
	if transport == nil {
		transport = &http.Transport{DialContext: publicDial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: time.Minute, IdleConnTimeout: 90 * time.Second, DisableCompression: true}
	} else {
		transport = transport.Clone()
	}

	transport.Proxy = nil
	if transport.DialContext == nil {
		transport.DialContext = publicDial
	}

	u := &upstreamTransport{transport: transport, hosts: map[string]bool{}, tunnels: map[*gitTunnel]struct{}{}}
	for _, repository := range repositories {
		remote, err := url.Parse(repository)
		if err != nil {
			return nil, err
		}
		u.hosts[strings.ToLower(remote.Hostname())] = true
	}

	if len(u.hosts) == 0 {
		return u, nil
	}

	// System Git needs a CONNECT transport so DNS checks also apply to its fetches.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	u.proxyURL = "http://" + listener.Addr().String()
	u.server = &http.Server{Handler: http.HandlerFunc(u.connect), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	u.served = make(chan struct{})
	go func() {
		defer close(u.served)
		if err := u.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Git upstream transport: %v", err)
		}
	}()
	return u, nil
}

func (u *upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.User != nil || r.URL.Hostname() == "" || r.URL.Port() != "" && r.URL.Port() != "443" {
		return nil, errors.New("upstream must use HTTPS port 443 without URL credentials")
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = out.URL.Host
	stripHop(out.Header)
	out.Header.Del("Proxy-Connection")

	response, err := u.transport.RoundTrip(out)
	if err != nil {
		return nil, err
	}

	stripHop(response.Header)
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.Header.Get("Location") != "" {
		target, err := out.URL.Parse(response.Header.Get("Location"))
		if err == nil {
			err = validateRedirect(r.Context(), target)
		}
		if err != nil {
			closeStream(response.Body)
			return nil, fmt.Errorf("unsafe Git redirect: %w", err)
		}
	}
	return response, nil
}

func validateRedirect(ctx context.Context, u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.Port() != "" && u.Port() != "443" {
		return errors.New("redirect must use HTTPS port 443 without credentials")
	}

	_, err := publicAddresses(ctx, u.Hostname())
	return err
}

func publicAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("upstream has no addresses")
	}

	for _, address := range addresses {
		if !publicIP(address) {
			return nil, errors.New("upstream resolves to a non-public address")
		}
	}

	return addresses, nil
}

// Dial a checked address directly so a second DNS lookup cannot change the destination.
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if port != "443" {
		return nil, errors.New("upstream must use HTTPS port 443")
	}

	addresses, err := publicAddresses(ctx, host)
	if err != nil {
		return nil, err
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

func publicIP(address netip.Addr) bool {
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

func (u *upstreamTransport) Close() {
	u.mu.Lock()
	u.closed = true
	tunnels := make([]*gitTunnel, 0, len(u.tunnels))
	for tunnel := range u.tunnels {
		tunnels = append(tunnels, tunnel)
	}
	u.mu.Unlock()

	if u.server != nil {
		if err := u.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("close Git upstream listener: %v", err)
		}
		<-u.served
	}

	for _, tunnel := range tunnels {
		tunnel.close()
	}

	u.wg.Wait()
	u.transport.CloseIdleConnections()
}
