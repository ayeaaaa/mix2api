package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type settingsProviderStub struct {
	realm string
	view  map[string]any
	saves []map[string]any
}

func (p *settingsProviderStub) ID() string                                  { return "stub-" + p.realm }
func (p *settingsProviderStub) Realm() string                               { return p.realm }
func (p *settingsProviderStub) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *settingsProviderStub) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *settingsProviderStub) Models(context.Context, bool) ([]providers.Model, error) {
	return []providers.Model{{"id": "m1"}}, nil
}
func (p *settingsProviderStub) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return newStubStream("data: [DONE]\n\n"), nil
}
func (p *settingsProviderStub) Routes() []providers.Route { return nil }
func (p *settingsProviderStub) Jobs() []scheduler.Job     { return nil }
func (p *settingsProviderStub) SettingsView(context.Context) (map[string]any, error) {
	return p.view, nil
}
func (p *settingsProviderStub) SaveSettings(_ context.Context, patch map[string]any) error {
	p.saves = append(p.saves, patch)
	return nil
}

func platformFixture(t *testing.T) (*config.Store, *keys.Store, *panelplatform.Password, *panelplatform.Sessions, string) {
	t.Helper()
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	keyStore := keys.New(store)
	password := panelplatform.NewPassword(store)
	sessions := panelplatform.NewSessions(time.Hour)
	return store, keyStore, password, sessions, dir
}

func doRequest(server *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestPanelPassword(t *testing.T) {
	_, keyStore, password, sessions, _ := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{{ID: "k1", Name: "one", Key: "key-one"}}, nil); err != nil {
		t.Fatal(err)
	}
	server := New(ServerOptions{
		KeyStore:      keyStore,
		PanelPassword: password,
		PanelSessions: sessions,
	})
	rec := doRequest(server, http.MethodPost, "/panel/login", `{"password":"admin"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	token, _ := body["token"].(string)
	if token == "" || body["using_default_password"] != true {
		t.Fatalf("login body = %#v", body)
	}
	rec = doRequest(server, http.MethodGet, "/panel/status", "", map[string]string{"X-Panel-Token": token})
	if body := decodeBody(t, rec); body["authenticated"] != true {
		t.Fatalf("status = %#v", body)
	}

	rec = doRequest(server, http.MethodPost, "/panel/password", `{"current":"admin","new":"secret123"}`, map[string]string{"X-Panel-Token": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("password change status=%d body=%s", rec.Code, rec.Body)
	}
	newToken, _ := decodeBody(t, rec)["token"].(string)
	if newToken == "" || newToken == token {
		t.Fatalf("new token = %q", newToken)
	}
	// The old session is revoked.
	rec = doRequest(server, http.MethodPost, "/panel/logout", `{}`, map[string]string{"X-Panel-Token": token})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token logout status = %d", rec.Code)
	}
	rec = doRequest(server, http.MethodPost, "/panel/logout", `{}`, map[string]string{"X-Panel-Token": newToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("new token logout status = %d", rec.Code)
	}

	// Old password no longer opens; the new one does.
	rec = doRequest(server, http.MethodPost, "/panel/login", `{"password":"admin"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password status = %d", rec.Code)
	}
	rec = doRequest(server, http.MethodPost, "/panel/login", `{"password":"secret123"}`, nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["using_default_password"] != false {
		t.Fatalf("new password login = %d %s", rec.Code, rec.Body)
	}
}

func TestPanelStatus(t *testing.T) {
	_, keyStore, password, sessions, _ := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{{ID: "k1", Key: "key-one"}}, nil); err != nil {
		t.Fatal(err)
	}
	raccoon := &settingsProviderStub{
		realm: "raccoon",
		view:  map[string]any{"raccoon": map[string]any{"configured": false, "timeout_seconds": 60}},
	}
	server := New(ServerOptions{
		Providers:     []providers.Provider{raccoon},
		KeyStore:      keyStore,
		PanelPassword: password,
		PanelSessions: sessions,
	})
	rec := doRequest(server, http.MethodGet, "/panel/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	for _, key := range []string{"panel_password_required", "panel_password_is_default", "authenticated", "crypto_ok", "api_key_set", "raccoon"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("status missing %q: %#v", key, body)
		}
	}
	if body["authenticated"] != false || body["api_key_set"] != true || body["crypto_ok"] != true {
		t.Fatalf("status flags = %#v", body)
	}
	section, _ := body["raccoon"].(map[string]any)
	if section["configured"] != false {
		t.Fatalf("raccoon section = %#v", body["raccoon"])
	}
}

func TestPanelLoginRateLimitAPI(t *testing.T) {
	_, _, password, sessions, _ := platformFixture(t)
	server := New(ServerOptions{PanelPassword: password, PanelSessions: sessions})
	for i := 0; i < 5; i++ {
		rec := doRequest(server, http.MethodPost, "/panel/login", `{"password":"wrong"}`, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i+1, rec.Code)
		}
	}
	rec := doRequest(server, http.MethodPost, "/panel/login", `{"password":"admin"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit status = %d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	body := decodeBody(t, rec)
	errBody, _ := body["error"].(map[string]any)
	if errBody["type"] != "rate_limit_error" || errBody["reset_at"] == nil {
		t.Fatalf("rate limit body = %#v", body)
	}
}
