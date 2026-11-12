package utlsbridge_test

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport/utlsbridge"
)

const (
	officialJA3    = "771,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-13-51-45-43,29-23-24,0"
	officialJA3MD5 = "71dc8c533dd919ae9f4963224a4ba8fd"
	officialJA4    = "t13d0a1200_5d04281c6031_b76c5ee2507d"
)

type helloFields struct {
	legacyVersion int
	sessionIDLen  int
	ciphers       []int
	extensions    []int
	groups        []int
	ecpf          []int
	sigalgs       []int
	hasALPN       bool
}

func TestOfficialClientHelloFingerprint(t *testing.T) {
	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{ServerName: "www.workbuddy.ai"})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer bridge.Close()

	addr, rawCh := captureClientHello(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = bridge.DialTLS(ctx, addr, "")

	select {
	case raw := <-rawCh:
		fields := parseClientHello(t, raw)
		gotJA3 := fmt.Sprintf("%d,%s,%s,%s,%s",
			fields.legacyVersion,
			joinInts(fields.ciphers, "-"),
			joinInts(fields.extensions, "-"),
			joinInts(fields.groups, "-"),
			joinInts(fields.ecpf, "-"))
		if gotJA3 != officialJA3 {
			t.Fatalf("JA3 = %q, want %q", gotJA3, officialJA3)
		}
		sum := md5.Sum([]byte(gotJA3))
		if got := hex.EncodeToString(sum[:]); got != officialJA3MD5 {
			t.Fatalf("JA3 md5 = %q, want %q", got, officialJA3MD5)
		}
		if got := ja4(fields); got != officialJA4 {
			t.Fatalf("JA4 = %q, want %q", got, officialJA4)
		}
		if fields.sessionIDLen != 32 {
			t.Fatalf("session id length = %d, want 32", fields.sessionIDLen)
		}
		if fields.hasALPN {
			t.Fatal("ClientHello unexpectedly advertises ALPN")
		}
		if len(raw) != 255 {
			t.Fatalf("ClientHello length = %d, want 255", len(raw))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ClientHello")
	}
}

func TestDefaultBridgeFromEnv(t *testing.T) {
	t.Setenv("MIX2API_TLS_FINGERPRINT", "node")
	bridge, err := utlsbridge.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if bridge == nil || bridge.Addr() == "" {
		t.Fatal("Default did not start a bridge")
	}
	if err := utlsbridge.CloseDefault(); err != nil {
		t.Fatalf("CloseDefault: %v", err)
	}
}

func TestBridgeRequiresSecret(t *testing.T) {
	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer bridge.Close()

	conn, err := net.Dial("tcp", bridge.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestTransportUsesBridge(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "bridge-ok")
	}))
	defer target.Close()

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
	resp, err := client.Do(context.Background(), req, "")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "bridge-ok" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("proto = %q, want HTTP/1.1", resp.Proto)
	}
}

func TestTransportUsesBridgeWithHTTPProxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "proxied-ok")
	}))
	defer target.Close()

	var proxyHits int
	var proxyMu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		proxyMu.Lock()
		proxyHits++
		proxyMu.Unlock()
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
	}))
	defer proxy.Close()

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
	resp, err := client.Do(context.Background(), req, proxy.URL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "proxied-ok" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

func TestLiveWorkBuddyHandshake(t *testing.T) {
	if os.Getenv("MIX2API_UTLS_LIVE") != "1" {
		t.Skip("set MIX2API_UTLS_LIVE=1 to exercise the real WorkBuddy endpoints")
	}
	for _, target := range []string{"www.workbuddy.ai:443", "www.workbuddy.cn:443"} {
		t.Run(target, func(t *testing.T) {
			bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{})
			if err != nil {
				t.Fatalf("NewWithConfig: %v", err)
			}
			defer bridge.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, err := bridge.DialTLS(ctx, target, "")
			if err != nil {
				t.Fatalf("DialTLS: %v", err)
			}
			_ = conn.Close()
		})
	}
}

func TestUnsupportedProxySchemeFails(t *testing.T) {
	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer bridge.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = bridge.DialTLS(ctx, "127.0.0.1:1", "ftp://127.0.0.1:21")
	if err == nil {
		t.Fatal("DialTLS succeeded, want unsupported proxy error")
	}
	if !strings.Contains(err.Error(), "CONNECT failed") {
		t.Fatalf("error = %v, want a failed CONNECT", err)
	}
}

func newBridgeClient(bridge *utlsbridge.Bridge) *transport.Client {
	return transport.NewClient(transport.ClientOptions{
		Timeout:    5 * time.Second,
		UTLSBridge: bridge,
	})
}

func captureClientHello(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ch := make(chan []byte, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		length := int(header[3])<<8 | int(header[4])
		payload := make([]byte, length)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		ch <- append(header, payload...)
	}()
	return ln.Addr().String(), ch
}

func parseClientHello(t *testing.T, raw []byte) helloFields {
	t.Helper()
	var handshake []byte
	for offset := 0; offset+5 <= len(raw); {
		typ := raw[offset]
		length := int(raw[offset+3])<<8 | int(raw[offset+4])
		if offset+5+length > len(raw) {
			t.Fatalf("truncated record")
		}
		if typ == 22 {
			handshake = append(handshake, raw[offset+5:offset+5+length]...)
		}
		offset += 5 + length
	}
	if len(handshake) < 4 || handshake[0] != 1 {
		t.Fatalf("first handshake message is not ClientHello")
	}
	bodyLen := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
	body := handshake[4:]
	if len(body) != bodyLen {
		t.Fatalf("body length = %d, want %d", len(body), bodyLen)
	}
	fields := helloFields{legacyVersion: int(body[0])<<8 | int(body[1])}
	offset := 2 + 32
	fields.sessionIDLen = int(body[offset])
	offset += 1 + fields.sessionIDLen
	cipherLen := int(body[offset])<<8 | int(body[offset+1])
	offset += 2
	for pos := offset; pos < offset+cipherLen; pos += 2 {
		fields.ciphers = append(fields.ciphers, int(body[pos])<<8|int(body[pos+1]))
	}
	offset += cipherLen
	compressionLen := int(body[offset])
	offset += 1 + compressionLen
	extLen := int(body[offset])<<8 | int(body[offset+1])
	offset += 2
	end := offset + extLen
	extData := make(map[int][]byte)
	for offset+4 <= end {
		typ := int(body[offset])<<8 | int(body[offset+1])
		length := int(body[offset+2])<<8 | int(body[offset+3])
		offset += 4
		data := body[offset : offset+length]
		offset += length
		fields.extensions = append(fields.extensions, typ)
		extData[typ] = data
	}
	if groupData := extData[10]; len(groupData) >= 2 {
		groupLen := int(groupData[0])<<8 | int(groupData[1])
		for pos := 2; pos < 2+groupLen; pos += 2 {
			fields.groups = append(fields.groups, int(groupData[pos])<<8|int(groupData[pos+1]))
		}
	}
	if sigData := extData[13]; len(sigData) >= 2 {
		sigLen := int(sigData[0])<<8 | int(sigData[1])
		for pos := 2; pos < 2+sigLen; pos += 2 {
			fields.sigalgs = append(fields.sigalgs, int(sigData[pos])<<8|int(sigData[pos+1]))
		}
	}
	if ecpfData := extData[11]; len(ecpfData) >= 1 {
		for _, value := range ecpfData[1 : 1+int(ecpfData[0])] {
			fields.ecpf = append(fields.ecpf, int(value))
		}
	}
	_, fields.hasALPN = extData[16]
	return fields
}

func ja4(fields helloFields) string {
	cipherHash := sha256.Sum256([]byte(sortedHex(fields.ciphers)))
	extParts := make([]string, 0, len(fields.extensions))
	for _, ext := range fields.extensions {
		if ext != 0 && ext != 16 {
			extParts = append(extParts, fmt.Sprintf("%04x", ext))
		}
	}
	sortStrings(extParts)
	sigParts := make([]string, 0, len(fields.sigalgs))
	for _, sig := range fields.sigalgs {
		sigParts = append(sigParts, fmt.Sprintf("%04x", sig))
	}
	sortStrings(sigParts)
	extHash := sha256.Sum256([]byte(strings.Join(extParts, ",") + "_" + strings.Join(sigParts, ",")))
	return fmt.Sprintf("t13d%02x%02x00_%s_%s", len(fields.extensions), len(fields.ciphers), hex.EncodeToString(cipherHash[:])[:12], hex.EncodeToString(extHash[:])[:12])
}

func sortedHex(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("%04x", value))
	}
	sortStrings(parts)
	return strings.Join(parts, ",")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func joinInts(values []int, sep string) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, sep)
}

func rootsFor(t *testing.T, server *httptest.Server) *x509.CertPool {
	t.Helper()
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("test server has no root CA pool")
	}
	return transport.TLSClientConfig.RootCAs
}
