package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
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

func TestHealthV1CompatibleShape(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	raccoon := &stubProvider{
		id: "raccoon", realm: "raccoon", realmAvailable: true,
		health: providers.Health{
			"ok": true, "configured": true, "accounts": 1, "healthy": 1,
			"base_url": "http://127.0.0.1:18764", "catalog_source": "raccoon", "models": 2,
		},
	}
	codearts := &stubProvider{
		id: "codearts", realm: "codearts",
		health: providers.Health{
			"ok": true, "configured": true, "accounts": 1, "healthy": 1,
			"base_url": "http://127.0.0.1:18768",
		},
	}
	intl := &stubProvider{
		id: "workbuddy", realm: "intl",
		health: providers.Health{
			"ok": true, "configured": true, "accounts": 0, "healthy": 0,
			"provider": "workbuddy", "realm": "intl", "base_url": "https://www.workbuddy.ai",
		},
	}
	cn := &stubProvider{
		id: "workbuddy", realm: "cn",
		health: providers.Health{
			"ok": true, "configured": true, "accounts": 0, "healthy": 0,
			"provider": "workbuddy", "realm": "cn", "base_url": "https://www.codebuddy.cn",
		},
	}
	if err := os.WriteFile(filepath.Join(dir, "active_realm.json"), []byte(`{"realm":"raccoon"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	realm := settings.NewRealm(store, dir, []providers.Provider{raccoon, codearts, intl, cn})
	srv := NewServer(ServerOptions{
		Version: "2.0.0-rc.1", Providers: []providers.Provider{raccoon, codearts, intl, cn}, Realm: realm,
	})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health?force=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["ok"] != true || body["realm"] != "raccoon" || body["crypto_ok"] != true {
		t.Fatalf("health identity = %#v", body)
	}
	if body["version"] != "2.0.0-rc.1" {
		t.Fatalf("additive version = %#v", body["version"])
	}
	for key, want := range map[string]float64{
		"accounts": 0, "accounts_ready": 0, "accounts_ready_current_realm": 0,
		"key_accounts_ready": 0, "key_accounts_bound": 0,
	} {
		if got, _ := body[key].(float64); got != want {
			t.Fatalf("%s = %#v, want %v", key, body[key], want)
		}
	}
	if body["api_key_required"] != false {
		t.Fatalf("api_key_required = %#v", body["api_key_required"])
	}
	for _, key := range []string{"accounts_by_realm", "accounts_ready_by_realm", "models_ready"} {
		value, ok := body[key].(map[string]any)
		if !ok || len(value) != 0 {
			t.Fatalf("%s = %#v, want an empty object", key, body[key])
		}
	}
	for _, key := range []string{"uid", "domain", "issuer", "credential_file", "expires_at"} {
		value, present := body[key]
		if !present || value != nil {
			t.Fatalf("%s = %#v (present=%v), want null", key, value, present)
		}
	}
	codeartsHealth, _ := body["codearts"].(map[string]any)
	if codeartsHealth["configured"] != true || codeartsHealth["accounts"] != float64(1) ||
		codeartsHealth["healthy"] != float64(1) || codeartsHealth["base_url"] != "http://127.0.0.1:18768" {
		t.Fatalf("codearts = %#v", codeartsHealth)
	}
	raccoonHealth, _ := body["raccoon"].(map[string]any)
	if raccoonHealth["catalog_source"] != "raccoon" || raccoonHealth["models"] != float64(2) {
		t.Fatalf("raccoon = %#v", raccoonHealth)
	}
	media, _ := body["media"].(map[string]any)
	if media["enabled"] != false || media["realm"] != "cn" || media["identity"] != "desktop" ||
		media["slots"] != float64(4) || media["inflight"] != float64(0) ||
		media["video_concurrency"] != float64(3) {
		t.Fatalf("media = %#v", media)
	}
	if providersMap, ok := body["providers"].(map[string]any); !ok || len(providersMap) != 4 {
		t.Fatalf("providers = %#v", body["providers"])
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
