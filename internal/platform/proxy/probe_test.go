package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
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
