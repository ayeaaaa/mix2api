package transport

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport/utlsbridge"
)

func TestDirectRequest(t *testing.T) {
	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		_, _ = io.WriteString(w, "direct-ok")
	}))
	defer server.Close()

	client := NewClient(ClientOptions{Timeout: 5 * time.Second})
	defer client.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/direct", nil)
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
	if resp.StatusCode != http.StatusOK || string(body) != "direct-ok" {
		t.Fatalf("response = %d %q, want 200 %q", resp.StatusCode, body, "direct-ok")
	}
	if path := <-paths; path != "/direct" {
		t.Fatalf("server path = %q, want %q", path, "/direct")
	}
}

func TestProxyRequestUsesAbsoluteURI(t *testing.T) {
	const target = "http://upstream.invalid/v1/models?q=1"

	var captured struct {
		sync.Mutex
		hits       int
		requestURI string
		proxyAuth  string
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Lock()
		captured.hits++
		captured.requestURI = r.RequestURI
		captured.proxyAuth = r.Header.Get("Proxy-Authorization")
		captured.Unlock()
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()

	proxyURL := "http://alice:s3cret@" + strings.TrimPrefix(proxy.URL, "http://")
	client := NewClient(ClientOptions{Timeout: 5 * time.Second})
	defer client.Close()

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(context.Background(), req, proxyURL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "proxied" {
		t.Fatalf("response = %d %q, want 200 %q", resp.StatusCode, body, "proxied")
	}

	tr, err := client.transportFor(proxyURL)
	if err != nil {
		t.Fatalf("transportFor: %v", err)
	}
	for _, scheme := range []string{"http", "https"} {
		probe, err := http.NewRequest(http.MethodGet, scheme+"://upstream.invalid/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		selected, err := tr.Proxy(probe)
		if err != nil {
			t.Fatalf("transport proxy for %s: %v", scheme, err)
		}
		if selected == nil || selected.String() != proxyURL {
			t.Fatalf("%s request proxy = %v, want %q", scheme, selected, proxyURL)
		}
	}

	captured.Lock()
	defer captured.Unlock()
	if captured.hits != 1 {
		t.Fatalf("proxy hits = %d, want 1", captured.hits)
	}
	if captured.requestURI != target {
		t.Fatalf("proxy request URI = %q, want absolute URI %q", captured.requestURI, target)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if captured.proxyAuth != wantAuth {
		t.Fatalf("Proxy-Authorization = %q, want %q", captured.proxyAuth, wantAuth)
	}
}

func TestSlotResolution(t *testing.T) {
	raw := map[string]any{
		"proxy_slots": []any{
			map[string]any{"id": "slot-1", "name": "primary", "url": "http://127.0.0.1:8080", "enabled": true},
			map[string]any{"id": "slot-2", "name": "off", "url": "http://127.0.0.1:8081", "enabled": false},
			map[string]any{"id": "slot-3", "name": "legacy", "url": "  proxy.example:3128:alice:s3cret  "},
			map[string]any{"name": "auto", "url": "http://127.0.0.1:9090", "enabled": true},
			"junk",
			map[string]any{"id": "slot-5", "name": "no-url", "url": "  ", "enabled": true},
		},
	}

	slots := SlotsFromSettings(raw)
	if len(slots) != 4 {
		t.Fatalf("SlotsFromSettings returned %d slots, want 4: %+v", len(slots), slots)
	}
	if slots[0].ID != "slot-1" || slots[0].Name != "primary" || !slots[0].Enabled {
		t.Fatalf("slot 0 = %+v", slots[0])
	}
	if slots[1].ID != "slot-2" || slots[1].Enabled {
		t.Fatalf("slot 1 = %+v", slots[1])
	}
	if slots[2].ID != "slot-3" || !slots[2].Enabled {
		t.Fatalf("legacy slot = %+v, want enabled legacy slot", slots[2])
	}
	if slots[2].URL != "http://alice:s3cret@proxy.example:3128" {
		t.Fatalf("legacy URL = %q, want normalized vendor URL", slots[2].URL)
	}
	if slots[3].ID != "slot-4" {
		t.Fatalf("auto id = %q, want %q", slots[3].ID, "slot-4")
	}

	cases := map[string]string{
		"slot-1":  "http://127.0.0.1:8080",
		" slot-1": "http://127.0.0.1:8080",
		"slot-2":  "",
		"slot-3":  "http://alice:s3cret@proxy.example:3128",
		"slot-4":  "http://127.0.0.1:9090",
		"missing": "",
		"":        "",
	}
	for id, want := range cases {
		if got := slots.URL(id); got != want {
			t.Fatalf("URL(%q) = %q, want %q", id, got, want)
		}
	}

	if got := SlotsFromSettings(nil); len(got) != 0 {
		t.Fatalf("SlotsFromSettings(nil) = %+v, want none", got)
	}
	if got := SlotsFromSettings(map[string]any{}); len(got) != 0 {
		t.Fatalf("SlotsFromSettings(empty) = %+v, want none", got)
	}

	typed := SlotsFromSettings(map[string]any{
		"proxy_slots": []map[string]any{
			{"id": "typed", "url": "127.0.0.1:7000", "enabled": true},
		},
	})
	if got := typed.URL("typed"); got != "http://127.0.0.1:7000" {
		t.Fatalf("URL(typed) = %q, want %q", got, "http://127.0.0.1:7000")
	}
}

func TestDirectIgnoresEnvironmentProxy(t *testing.T) {
	var proxyHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		http.Error(w, "direct request must not use the proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "target-ok")
	}))
	defer target.Close()

	for key, value := range map[string]string{
		"HTTP_PROXY":  proxy.URL,
		"HTTPS_PROXY": proxy.URL,
		"http_proxy":  proxy.URL,
		"https_proxy": proxy.URL,
		"NO_PROXY":    "",
		"no_proxy":    "",
	} {
		t.Setenv(key, value)
	}

	client := NewClient(ClientOptions{Timeout: 5 * time.Second})
	defer client.Close()

	direct, err := client.transportFor("")
	if err != nil {
		t.Fatalf("transportFor(\"\"): %v", err)
	}
	if direct.Proxy != nil {
		t.Fatal("direct transport has a Proxy function; it must be nil to ignore HTTP_PROXY/HTTPS_PROXY")
	}

	req, err := http.NewRequest(http.MethodGet, target.URL, nil)
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
	if string(body) != "target-ok" {
		t.Fatalf("body = %q, want %q", body, "target-ok")
	}
	if hits := atomic.LoadInt32(&proxyHits); hits != 0 {
		t.Fatalf("environment proxy was used %d times for a direct request", hits)
	}
}

func TestTransportReusePerNormalizedProxyURL(t *testing.T) {
	client := NewClient(ClientOptions{})
	defer client.Close()

	first, err := client.transportFor("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("transportFor: %v", err)
	}
	again, err := client.transportFor("  http://127.0.0.1:8080  ")
	if err != nil {
		t.Fatalf("transportFor trimmed URL: %v", err)
	}
	if first != again {
		t.Fatal("normalized equal proxy URLs returned different transports")
	}
	schemeless, err := client.transportFor("127.0.0.1:8080")
	if err != nil {
		t.Fatalf("transportFor schemeless URL: %v", err)
	}
	if first != schemeless {
		t.Fatal("schemeless proxy URL did not normalize to the same transport")
	}
	other, err := client.transportFor("http://127.0.0.1:8081")
	if err != nil {
		t.Fatalf("transportFor other proxy: %v", err)
	}
	if first == other {
		t.Fatal("different proxy URLs shared a transport")
	}

	const workers = 32
	results := make(chan *http.Transport, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, err := client.transportFor("http://127.0.0.1:8080")
			results <- tr
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent transportFor: %v", err)
		}
	}
	for tr := range results {
		if tr != first {
			t.Fatal("concurrent calls returned a different transport")
		}
	}

	client.Close()
	afterClose, err := client.transportFor("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("transportFor after Close: %v", err)
	}
	if afterClose == first {
		t.Fatal("Close did not drop cached transports")
	}
}

func TestSocks5hNormalizesToSocks5(t *testing.T) {
	client := NewClient(ClientOptions{})
	defer client.Close()

	tr, err := client.transportFor("socks5h://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("transportFor socks5h: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	selected, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("transport proxy: %v", err)
	}
	if selected == nil || selected.Scheme != "socks5" {
		t.Fatalf("socks5h proxy scheme = %v, want socks5", selected)
	}
	if selected.Host != "127.0.0.1:1080" {
		t.Fatalf("socks5h proxy host = %q, want %q", selected.Host, "127.0.0.1:1080")
	}
	plain, err := client.transportFor("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("transportFor socks5: %v", err)
	}
	if plain != tr {
		t.Fatal("socks5h and socks5 proxy URLs did not share a transport")
	}
}

func TestProxyErrorsRedactCredentials(t *testing.T) {
	client := NewClient(ClientOptions{})
	defer client.Close()

	req, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	cases := []struct {
		proxyURL string
		secrets  []string
	}{
		{"http://alice:p/ss@host:8080", []string{"alice", "p/ss"}},
		{"http://alice:s3cret@[::1", []string{"alice", "s3cret"}},
		{"http://alice:s3cret@host:badport", []string{"alice", "s3cret"}},
		{"http://alice:s3cret@", []string{"alice", "s3cret"}},
		{"ftp://alice:s3cret@proxy.example:21", []string{"alice", "s3cret"}},
	}
	for _, c := range cases {
		_, err := client.Do(context.Background(), req, c.proxyURL)
		if err == nil {
			t.Fatalf("Do with invalid proxy %q unexpectedly succeeded", c.proxyURL)
		}
		for _, secret := range c.secrets {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error for %q leaks credential %q: %v", c.proxyURL, secret, err)
			}
		}
	}
}

func TestDoRejectsNilInputs(t *testing.T) {
	client := NewClient(ClientOptions{})
	defer client.Close()

	if _, err := client.Do(context.Background(), nil, ""); err == nil {
		t.Fatal("Do with a nil request unexpectedly succeeded")
	}
	req, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := client.Do(nil, req, ""); err == nil {
		t.Fatal("Do with a nil context unexpectedly succeeded")
	}
}

func TestRefuseRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, "stolen")
	}))
	defer target.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/target", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := NewClient(ClientOptions{RefuseRedirects: true})
	defer client.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(context.Background(), req, "")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 returned to caller", resp.StatusCode)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target received %d request(s)", redirected.Load())
	}
}

func TestClientTimeout(t *testing.T) {
	unblock := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
	}))
	defer server.Close()
	defer close(unblock)

	client := NewClient(ClientOptions{Timeout: 30 * time.Millisecond})
	defer client.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := client.Do(context.Background(), req, ""); err == nil {
		t.Fatal("Do unexpectedly succeeded without waiting for the timeout")
	}
}

func TestNewClientUTLSUsesDefaultBridge(t *testing.T) {
	t.Setenv("MIX2API_TLS_FINGERPRINT", "node")
	client := NewClient(ClientOptions{UTLS: true})
	if client.utlsBridge == nil {
		t.Fatal("UTLS client has no default bridge")
	}
	client.Close()
	if err := utlsbridge.CloseDefault(); err != nil {
		t.Fatalf("CloseDefault: %v", err)
	}
}
