package utlsbridge_test

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport/utlsbridge"
)

func TestTransportUsesBridgeWithHTTPSProxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "https-proxied-ok")
	}))
	defer target.Close()

	var proxyHits int
	var proxyMu sync.Mutex
	proxy := httptest.NewTLSServer(connectProxyHandler(func() {
		proxyMu.Lock()
		proxyHits++
		proxyMu.Unlock()
	}))
	defer proxy.Close()

	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	roots.AddCert(proxy.Certificate())
	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer bridge.Close()

	client := newBridgeClient(bridge)
	defer client.Close()
	req, err := http.NewRequest(http.MethodGet, target.URL+"/v1/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(context.Background(), req, proxy.URL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "https-proxied-ok" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

func TestTransportUsesBridgeWithSOCKS5Proxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socks-ok")
	}))
	defer target.Close()

	socksURL := startSOCKS5(t)
	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{RootCAs: rootsFor(t, target)})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer bridge.Close()

	client := newBridgeClient(bridge)
	defer client.Close()
	req, err := http.NewRequest(http.MethodGet, target.URL+"/v1/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(context.Background(), req, socksURL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "socks-ok" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
}

func TestTransportUsesBridgeWithSOCKS5Auth(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socks-auth-ok")
	}))
	defer target.Close()

	socksURL := startSOCKS5WithAuth(t, "alice", "s3cret")
	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{RootCAs: rootsFor(t, target)})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer bridge.Close()

	client := newBridgeClient(bridge)
	defer client.Close()
	req, err := http.NewRequest(http.MethodGet, target.URL+"/v1/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(context.Background(), req, socksURL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "socks-auth-ok" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
}

func TestBridgeKeepsHTTPProxyPath(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain-ok")
	}))
	defer target.Close()

	var proxyHits int
	var proxyMu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyMu.Lock()
		proxyHits++
		proxyMu.Unlock()
		_, _ = io.WriteString(w, "proxied-http")
	}))
	defer proxy.Close()

	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer bridge.Close()

	client := newBridgeClient(bridge)
	defer client.Close()
	req, err := http.NewRequest(http.MethodGet, target.URL+"/v1/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(context.Background(), req, proxy.URL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "proxied-http" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

func connectProxyHandler(onHit func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		if onHit != nil {
			onHit()
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			_, _ = io.Copy(upstream, clientConn)
			upstream.Close()
		}()
		_, _ = io.Copy(clientConn, upstream)
		clientConn.Close()
	})
}

func startSOCKS5(t *testing.T) string {
	return startSOCKS5WithAuth(t, "", "")
}

func startSOCKS5WithAuth(t *testing.T, user, password string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				serveSOCKS5(conn, user, password)
			}()
		}
	}()
	address := ln.Addr().String()
	if user != "" {
		return "socks5://" + url.UserPassword(user, password).String() + "@" + address
	}
	return "socks5://" + address
}

func serveSOCKS5(conn net.Conn, user, password string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if user == "" {
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
	} else {
		offered := false
		for _, method := range methods {
			if method == 2 {
				offered = true
				break
			}
		}
		if !offered {
			_, _ = conn.Write([]byte{5, 0xff})
			return
		}
		if _, err := conn.Write([]byte{5, 2}); err != nil {
			return
		}
		authHeader := make([]byte, 2)
		if _, err := io.ReadFull(conn, authHeader); err != nil || authHeader[0] != 1 {
			return
		}
		username := make([]byte, int(authHeader[1]))
		if _, err := io.ReadFull(conn, username); err != nil {
			return
		}
		passwordLen := make([]byte, 1)
		if _, err := io.ReadFull(conn, passwordLen); err != nil {
			return
		}
		pass := make([]byte, int(passwordLen[0]))
		if _, err := io.ReadFull(conn, pass); err != nil {
			return
		}
		if string(username) != user || string(pass) != password {
			_, _ = conn.Write([]byte{1, 1})
			return
		}
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			return
		}
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = string(raw)
	case 4:
		raw := make([]byte, 16)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		_, _ = conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, target)
		done <- struct{}{}
	}()
	<-done
}
