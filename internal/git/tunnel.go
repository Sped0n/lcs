package git

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

func (u *upstreamTransport) connect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "Git upstream transport requires CONNECT", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != "443" || strings.ContainsAny(host, "/\\@%") || !u.hosts[strings.ToLower(host)] {
		http.Error(w, "CONNECT requires a selected Git host on port 443", http.StatusBadRequest)
		return
	}

	upstream, err := u.transport.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, "Git upstream connection failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		closeStream(upstream)
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}

	client, buffered, err := hijacker.Hijack()
	if err != nil {
		closeStream(upstream)
		return
	}

	tunnel := &gitTunnel{client: client, upstream: upstream}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		tunnel.close()
		return
	}
	u.tunnels[tunnel] = struct{}{}
	u.wg.Add(1)
	u.mu.Unlock()
	defer func() {
		tunnel.close()
		u.mu.Lock()
		delete(u.tunnels, tunnel)
		u.mu.Unlock()
		u.wg.Done()
	}()
	deadline := time.Now().Add(30 * time.Minute)
	if err := errors.Join(client.SetDeadline(deadline), upstream.SetDeadline(deadline)); err != nil {
		return
	}

	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	connection := &bufferedConn{Conn: client, reader: buffered.Reader}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(upstream, connection); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("Git tunnel upstream: %v", err)
		}
		if tcp, ok := upstream.(*net.TCPConn); ok {
			if err := tcp.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("close Git upstream write: %v", err)
			}
		}
	}()
	if _, err := io.Copy(connection, upstream); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("Git tunnel downstream: %v", err)
	}
	tunnel.close()
	<-done
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(data []byte) (int, error) { return c.reader.Read(data) }

type gitTunnel struct{ client, upstream net.Conn }

func (t *gitTunnel) close() {
	closeStream(t.client)
	closeStream(t.upstream)
}

func closeStream(stream io.Closer) {
	if err := stream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("close Git stream: %v", err)
	}
}
