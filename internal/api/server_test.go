package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestHealth(t *testing.T) {
	srv := NewServer(ServerOptions{Addr: "127.0.0.1:0", Version: "2.0.0-dev"})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("body=%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"version":"2.0.0-dev"`) {
		t.Fatalf("body=%s", rec.Body)
	}
}

func TestHealthIncludesProviderHealth(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		health: providers.Health{"ok": true, "accounts": 2, "healthy": 1},
	}
	srv := NewServer(ServerOptions{Version: "2.0.0-test", Providers: []providers.Provider{provider}})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["ok"] != true || body["version"] != "2.0.0-test" {
		t.Fatalf("top-level health = %#v", body)
	}
	providerHealth, ok := body["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers = %#v", body["providers"])
	}
	raccoon, ok := providerHealth["raccoon"].(map[string]any)
	if !ok {
		t.Fatalf("providers[raccoon] = %#v", providerHealth["raccoon"])
	}
	if raccoon["ok"] != true || raccoon["accounts"] != float64(2) {
		t.Fatalf("providers[raccoon] = %#v", raccoon)
	}
}

func TestServerTimeoutsAndPanicRecovery(t *testing.T) {
	srv := NewServer(ServerOptions{})
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Fatalf("timeouts = %#v", srv)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 for SSE", srv.WriteTimeout)
	}

	provider := &stubProvider{
		id: "panic", realm: "panic",
		routes: []providers.Route{{
			Method: http.MethodGet, Path: "/panic", Auth: providers.AuthNone,
			Handler: func(http.ResponseWriter, *http.Request) { panic("boom") },
		}},
	}
	live := NewServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := httptest.NewRecorder()
	live.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("panic detail leaked: %s", rec.Body)
	}
}

type listenerPortProvider struct {
	*stubProvider
	port int
}

func (p *listenerPortProvider) SetListenerPort(port int) { p.port = port }

func TestServerInjectsListenerPort(t *testing.T) {
	stub := &stubProvider{id: "oauth", realm: "oauth"}
	provider := &listenerPortProvider{stubProvider: stub}
	New(ServerOptions{Providers: []providers.Provider{provider}, ListenerPort: 54321})
	if provider.port != 54321 {
		t.Fatalf("listener port = %d, want 54321", provider.port)
	}
}

func TestHealthProviderFailureStillOK(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		healthErr: errors.New("provider exploded: /secret/path.json"),
	}
	srv := NewServer(ServerOptions{Version: "2.0.0-test", Providers: []providers.Provider{provider}})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["ok"] != true {
		t.Fatalf("top-level ok = %#v", body["ok"])
	}
	providerHealth := body["providers"].(map[string]any)
	raccoon := providerHealth["raccoon"].(map[string]any)
	if raccoon["ok"] != false {
		t.Fatalf("providers[raccoon] = %#v", raccoon)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("health leaked provider error detail: %s", rec.Body)
	}
}
