// Package utlsbridge provides an in-process loopback CONNECT bridge that
// performs WorkBuddy-compatible uTLS handshakes while leaving HTTP/1.1,
// keep-alive, SSE, and proxy selection to the caller.
package utlsbridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

const (
	bridgeAuthHeader    = "X-Mix2API-Bridge-Auth"
	upstreamProxyHeader = "X-Mix2API-Upstream-Proxy"

	defaultDialTimeout      = 10 * time.Second
	defaultHandshakeTimeout = 15 * time.Second
	closeWaitTimeout        = 5 * time.Second
)

// Config configures a Bridge.
type Config struct {
	// RootCAs overrides the system trust store. It is primarily useful for
	// tests and private deployments; nil keeps Go's normal certificate
	// verification.
	RootCAs *x509.CertPool

	// ServerName overrides the SNI/certificate name. Empty uses the CONNECT
	// target host. Tests use it to capture the WorkBuddy fingerprint while
	// dialing a loopback server.
	ServerName string

	// Listen overrides the loopback listen address. The bridge refuses a
	// non-loopback address.
	Listen string

	// DialTimeout bounds the TCP/proxy connection. Zero uses 10s.
	DialTimeout time.Duration

	// HandshakeTimeout bounds the proxy CONNECT and uTLS handshake. Zero uses
	// 15s.
	HandshakeTimeout time.Duration
}

// Bridge is an in-process uTLS CONNECT bridge. It listens only on loopback;
// callers authenticate with the random secret generated at startup.
type Bridge struct {
	ln               net.Listener
	secret           string
	rootCAs          *x509.CertPool
	serverName       string
	dialTimeout      time.Duration
	handshakeTimeout time.Duration

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewWithConfig starts a loopback bridge and its accept loop.
func NewWithConfig(cfg Config) (*Bridge, error) {
	listen := strings.TrimSpace(cfg.Listen)
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("utlsbridge: listen: %w", err)
	}
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil || !isLoopbackHost(host) {
		_ = ln.Close()
		return nil, errors.New("utlsbridge: listener must be loopback")
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("utlsbridge: secret: %w", err)
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = defaultDialTimeout
	}
	handshakeTimeout := cfg.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	bridge := &Bridge{
		ln:               ln,
		secret:           hex.EncodeToString(secretBytes),
		rootCAs:          cfg.RootCAs,
		serverName:       strings.TrimSpace(cfg.ServerName),
		dialTimeout:      dialTimeout,
		handshakeTimeout: handshakeTimeout,
		conns:            make(map[net.Conn]struct{}),
	}
	bridge.wg.Add(1)
	go bridge.acceptLoop()
	return bridge, nil
}

// Addr returns the loopback listen address.
func (b *Bridge) Addr() string {
	if b == nil || b.ln == nil {
		return ""
	}
	return b.ln.Addr().String()
}

// DialTLS asks the bridge to establish a TLS connection to target, optionally
// through proxyURL. The returned connection is already past the TLS handshake;
// net/http's custom DialTLSContext treats it as the HTTPS transport.
func (b *Bridge) DialTLS(ctx context.Context, target, proxyURL string) (net.Conn, error) {
	if b == nil {
		return nil, errors.New("utlsbridge: nil bridge")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	if strings.ContainsAny(proxyURL, "\r\n") {
		return nil, errors.New("utlsbridge: invalid proxy URL")
	}

	dialer := &net.Dialer{Timeout: b.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", b.Addr())
	if err != nil {
		return nil, fmt.Errorf("utlsbridge: dial bridge: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	deadline := time.Now().Add(b.handshakeTimeout)
	if ctxDeadline, hasDeadline := ctx.Deadline(); hasDeadline && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\n",
		target, target, bridgeAuthHeader, b.secret)
	if proxyURL != "" {
		fmt.Fprintf(&request, "%s: %s\r\n", upstreamProxyHeader, proxyURL)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return nil, fmt.Errorf("utlsbridge: write CONNECT: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("utlsbridge: read CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := bridgeErrorDetail(resp.Body)
		if detail != "" {
			return nil, fmt.Errorf("utlsbridge: CONNECT failed: %s: %s", resp.Status, detail)
		}
		return nil, fmt.Errorf("utlsbridge: CONNECT failed: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// Close stops accepting new connections and closes active ones.
func (b *Bridge) Close() error {
	if b == nil {
		return nil
	}
	var err error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		if b.ln != nil {
			err = b.ln.Close()
		}
		for conn := range b.conns {
			_ = conn.Close()
		}
		b.mu.Unlock()

		done := make(chan struct{})
		go func() {
			b.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeWaitTimeout):
			if err == nil {
				err = errors.New("utlsbridge: close timed out")
			}
		}
	})
	return err
}

func (b *Bridge) acceptLoop() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient accept failure (for example EMFILE) must not
			// permanently wedge the bridge; back off and keep serving.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !b.track(conn) {
			_ = conn.Close()
			continue
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer b.untrack(conn)
			b.serve(conn)
		}()
	}
}

func (b *Bridge) track(conn net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.conns[conn] = struct{}{}
	return true
}

func (b *Bridge) untrack(conn net.Conn) {
	b.mu.Lock()
	delete(b.conns, conn)
	b.mu.Unlock()
}

func (b *Bridge) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(b.handshakeTimeout))

	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		writeHTTPError(conn, http.StatusBadRequest, "Bad Request")
		return
	}
	if req.Method != http.MethodConnect {
		writeHTTPError(conn, http.StatusMethodNotAllowed, "CONNECT Required")
		return
	}
	provided := req.Header.Get(bridgeAuthHeader)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(b.secret)) != 1 {
		writeHTTPError(conn, http.StatusForbidden, "Forbidden")
		return
	}

	target := strings.TrimSpace(req.Host)
	if target == "" {
		target = strings.TrimSpace(req.RequestURI)
	}
	if err := validateTarget(target); err != nil {
		writeHTTPError(conn, http.StatusBadRequest, "Bad CONNECT Target")
		return
	}
	proxyURL := strings.TrimSpace(req.Header.Get(upstreamProxyHeader))
	if strings.ContainsAny(proxyURL, "\r\n") {
		writeHTTPError(conn, http.StatusBadRequest, "Bad Upstream Proxy")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.dialTimeout+b.handshakeTimeout)
	defer cancel()
	raw, err := b.dialUpstream(ctx, target, proxyURL)
	if err != nil {
		writeHTTPErrorDetail(conn, http.StatusBadGateway, "Upstream Connect Failed", err)
		return
	}
	if !b.track(raw) {
		_ = raw.Close()
		return
	}
	defer b.untrack(raw)
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(b.handshakeTimeout))

	host, _, err := net.SplitHostPort(target)
	if err != nil {
		writeHTTPError(conn, http.StatusBadGateway, "Bad CONNECT Target")
		return
	}
	serverName := b.serverName
	if serverName == "" {
		serverName = host
	}
	tlsConn := utls.UClient(raw, &utls.Config{
		ServerName: serverName,
		RootCAs:    b.rootCAs,
		MinVersion: tls.VersionTLS12,
	}, utls.HelloCustom)
	if err := tlsConn.ApplyPreset(officialHelloSpec()); err != nil {
		writeHTTPErrorDetail(conn, http.StatusBadGateway, "TLS Fingerprint Failed", err)
		return
	}
	if err := tlsConn.Handshake(); err != nil {
		writeHTTPErrorDetail(conn, http.StatusBadGateway, "TLS Handshake Failed", err)
		return
	}
	_ = raw.SetDeadline(time.Time{})
	_ = conn.SetDeadline(time.Time{})

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	proxyBidirectional(conn, reader, tlsConn)
}

func validateTarget(target string) error {
	host, port, err := net.SplitHostPort(target)
	if err != nil || strings.TrimSpace(host) == "" {
		return errors.New("utlsbridge: invalid CONNECT target")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return errors.New("utlsbridge: invalid CONNECT host")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("utlsbridge: invalid CONNECT port")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func proxyBidirectional(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
	closeBoth()
	<-done
}

type closeWriter interface {
	CloseWrite() error
}

func writeHTTPError(conn net.Conn, status int, reason string) {
	body := reason + "\n"
	_, _ = fmt.Fprintf(conn,
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

func writeHTTPErrorDetail(conn net.Conn, status int, reason string, err error) {
	if err != nil {
		detail := strings.TrimSpace(err.Error())
		detail = strings.ReplaceAll(detail, "\r", " ")
		detail = strings.ReplaceAll(detail, "\n", " ")
		if len(detail) > 256 {
			detail = detail[:256]
		}
		if detail != "" {
			reason += ": " + detail
		}
	}
	writeHTTPError(conn, status, reason)
}

func bridgeErrorDetail(body io.ReadCloser) string {
	if body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(body, 4096))
	_ = body.Close()
	text := strings.TrimSpace(string(data))
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *bufferedConn) CloseWrite() error {
	if writer, ok := c.Conn.(closeWriter); ok {
		return writer.CloseWrite()
	}
	return nil
}
