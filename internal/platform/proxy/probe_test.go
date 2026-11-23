package proxy

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProbeHTTPProxyExitAndIntel(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "probe.invalid" {
			t.Errorf("proxy target = %q, want probe.invalid", r.URL.Host)
		}
		_, _ = io.WriteString(w, "203.0.113.7\n")
	}))
	defer proxyServer.Close()

	geoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "203.0.113.7") {
			t.Errorf("geo path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","country":"美国","countryCode":"US","isp":"Test ISP","as":"AS64500","hosting":false}`)
	}))
	defer geoServer.Close()

	t.Setenv("WB_PROXY_PROBE_URL", "http://probe.invalid/")
	t.Setenv("WB_PROXY_GEO_URL", geoServer.URL)

	result, err := Probe(context.Background(), proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("probe failed: %#v", result)
	}
	if result["exit_ip"] != "203.0.113.7" {
		t.Fatalf("exit ip = %#v", result["exit_ip"])
	}
	if latency, ok := result["latency_ms"].(int64); !ok || latency < 0 {
		t.Fatalf("latency = %#v", result["latency_ms"])
	}
	if result["error"] != "" {
		t.Fatalf("unexpected error = %#v", result["error"])
	}
	if intel, _ := result["intel_ok"].(bool); !intel {
		t.Fatalf("intel missing: %#v", result)
	}
	if result["country"] != "美国" || result["country_code"] != "US" ||
		result["ip_type"] != "residential" || result["isp"] != "Test ISP" || result["asn"] != "AS64500" {
		t.Fatalf("intel mismatch: %#v", result)
	}
}

func TestProbeFailureMasksProxyCredentials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	t.Setenv("WB_PROXY_PROBE_URL", "http://probe.invalid/")
	result, err := Probe(context.Background(), "http://user:supersecret@"+addr)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := result["ok"].(bool); ok {
		t.Fatalf("closed proxy reported success: %#v", result)
	}
	message, _ := result["error"].(string)
	if message == "" {
		t.Fatalf("missing probe error: %#v", result)
	}
	if strings.Contains(message, "supersecret") || strings.Contains(message, "user:") {
		t.Fatalf("probe error leaked credentials: %q", message)
	}
}

func TestDiscoverProbesConfiguredPorts(t *testing.T) {
	makeProxy := func(ip string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, ip)
		}))
	}
	first := makeProxy("203.0.113.8")
	defer first.Close()
	second := makeProxy("203.0.113.9")
	defer second.Close()

	firstPort := strings.TrimPrefix(first.URL, "http://127.0.0.1:")
	secondPort := strings.TrimPrefix(second.URL, "http://127.0.0.1:")
	t.Setenv("WB_PROXY_DISCOVER_HOST", "127.0.0.1")
	t.Setenv("WB_PROXY_DISCOVER_PORTS", firstPort+","+secondPort)
	t.Setenv("WB_PROXY_PROBE_URL", "http://probe.invalid/")
	t.Setenv("WB_PROXY_GEO_URL", "http://127.0.0.1:1/")

	got := Discover(context.Background())
	if len(got) != 2 {
		t.Fatalf("discover results = %#v", got)
	}
	if got[0]["url"] != first.URL || got[1]["url"] != second.URL {
		t.Fatalf("discover urls = %#v", got)
	}
	if reachable, _ := got[0]["reachable"].(bool); !reachable || got[0]["exit_ip"] != "203.0.113.8" {
		t.Fatalf("first candidate = %#v", got[0])
	}
	if reachable, _ := got[1]["reachable"].(bool); !reachable || got[1]["exit_ip"] != "203.0.113.9" {
		t.Fatalf("second candidate = %#v", got[1])
	}
}

func TestDiscoverPortsParsesRangesAndLists(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []int
	}{
		{"17901-17903", []int{17901, 17902, 17903}},
		{"17901 - 17903", []int{17901, 17902, 17903}},
		{"1080, 1081 1082", []int{1080, 1081, 1082}},
		{"1080,1080,1081", []int{1080, 1081}},
		{"0,65536,1,bad", []int{1}},
		{"", nil},
	} {
		if got := discoverPorts(tc.raw); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("discoverPorts(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestSafeProbeErrorMasksCredentialsDirectly(t *testing.T) {
	raw := "http://user:supersecret@127.0.0.1:1080"
	err := errors.New(`Get "` + raw + `": proxyconnect tcp: dial tcp 127.0.0.1:1080: connect: connection refused`)
	got := safeProbeError(err, raw)
	if got == "" || strings.Contains(got, "supersecret") || strings.Contains(got, "user:") {
		t.Fatalf("unsafe probe error = %q", got)
	}
}

func TestGeoEndpointPreservesBaseQuery(t *testing.T) {
	t.Setenv("WB_PROXY_GEO_URL", "http://geo.local/lookup?token=x")
	got, err := url.Parse(geoEndpoint("203.0.113.7"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/lookup/203.0.113.7" || got.Query().Get("token") != "x" ||
		got.Query().Get("lang") != "zh-CN" || !strings.Contains(got.Query().Get("fields"), "hosting") {
		t.Fatalf("geo endpoint = %q", got)
	}
}

func TestProbeHTTPSThroughConnectProxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "203.0.113.14\n")
	}))
	defer target.Close()
	certFile := filepath.Join(t.TempDir(), "target.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certFile)

	geoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","country":"美国","countryCode":"US","hosting":false}`)
	}))
	defer geoServer.Close()
	t.Setenv("WB_PROXY_PROBE_URL", target.URL)
	t.Setenv("WB_PROXY_GEO_URL", geoServer.URL)

	proxyServer := startConnectProxy(t)
	result, err := Probe(context.Background(), proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := result["ok"].(bool); !ok || result["exit_ip"] != "203.0.113.14" {
		t.Fatalf("HTTPS CONNECT probe = %#v", result)
	}
}

func TestProbeSOCKS5Proxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "203.0.113.15\n")
	}))
	defer target.Close()
	geoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","country":"美国","countryCode":"US","hosting":false}`)
	}))
	defer geoServer.Close()
	t.Setenv("WB_PROXY_PROBE_URL", target.URL)
	t.Setenv("WB_PROXY_GEO_URL", geoServer.URL)

	result, err := Probe(context.Background(), startSOCKS5Proxy(t))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := result["ok"].(bool); !ok || result["exit_ip"] != "203.0.113.15" {
		t.Fatalf("SOCKS5 probe = %#v", result)
	}
}

func startConnectProxy(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "hijack unavailable", http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		done := make(chan struct{}, 1)
		go func() {
			_, _ = io.Copy(upstream, client)
			done <- struct{}{}
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		_ = upstream.Close()
		<-done
	}))
	t.Cleanup(server.Close)
	return server
}

func startSOCKS5Proxy(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5(conn)
		}
	}()
	return "socks5://" + listener.Addr().String()
}

func serveSOCKS5(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	host := ""
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
	port := int(binary.BigEndian.Uint16(portBytes))
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	go func() {
		_, _ = io.Copy(upstream, conn)
	}()
	_, _ = io.Copy(conn, upstream)
}
