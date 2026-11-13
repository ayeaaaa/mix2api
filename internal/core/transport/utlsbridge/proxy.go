package utlsbridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func (b *Bridge) dialUpstream(ctx context.Context, target, proxyURL string) (net.Conn, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return b.dialTCP(ctx, target)
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Hostname() == "" {
		return nil, errors.New("utlsbridge: invalid proxy URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return b.dialHTTPProxy(ctx, parsed, target)
	case "socks5", "socks5h":
		return b.dialSOCKS5(ctx, parsed, target)
	default:
		return nil, fmt.Errorf("utlsbridge: unsupported proxy scheme %q", parsed.Scheme)
	}
}

func (b *Bridge) dialTCP(ctx context.Context, target string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: b.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (b *Bridge) dialHTTPProxy(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	defaultPort := "80"
	if strings.EqualFold(proxyURL.Scheme, "https") {
		defaultPort = "443"
	}
	proxyAddr := proxyAddress(proxyURL, defaultPort)
	conn, err := b.dialTCP(ctx, proxyAddr)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	if strings.EqualFold(proxyURL.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: proxyURL.Hostname(),
			RootCAs:    b.rootCAs,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("utlsbridge: proxy TLS handshake: %w", err)
		}
		conn = tlsConn
	}

	deadline := time.Now().Add(b.handshakeTimeout)
	if ctxDeadline, hasDeadline := ctx.Deadline(); hasDeadline && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if auth := proxyAuthorization(proxyURL); auth != "" {
		fmt.Fprintf(&request, "Proxy-Authorization: %s\r\n", auth)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return nil, fmt.Errorf("utlsbridge: proxy CONNECT: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("utlsbridge: proxy response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("utlsbridge: proxy CONNECT returned %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func (b *Bridge) dialSOCKS5(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	var auth *xproxy.Auth
	if proxyURL.User != nil {
		auth = &xproxy.Auth{
			User:     proxyURL.User.Username(),
			Password: "",
		}
		auth.Password, _ = proxyURL.User.Password()
	}
	dialer, err := xproxy.SOCKS5("tcp", proxyAddress(proxyURL, "1080"), auth, &net.Dialer{Timeout: b.dialTimeout})
	if err != nil {
		return nil, fmt.Errorf("utlsbridge: SOCKS5: %w", err)
	}
	if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, "tcp", target)
	}
	return dialer.Dial("tcp", target)
}

func proxyAddress(proxyURL *url.URL, defaultPort string) string {
	port := proxyURL.Port()
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

func proxyAuthorization(proxyURL *url.URL) string {
	if proxyURL.User == nil {
		return ""
	}
	password, _ := proxyURL.User.Password()
	credentials := proxyURL.User.Username() + ":" + password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(credentials))
}
