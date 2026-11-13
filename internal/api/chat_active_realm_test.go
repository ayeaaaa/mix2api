package api

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestChatUnboundKeyPrefersActiveRealmForSharedModel(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	if _, err := store.Merge(map[string]any{"active_realm": "cn"}); err != nil {
		t.Fatalf("seed active realm: %v", err)
	}

	codearts := &stubProvider{
		id: "codearts", realm: "codearts", realmAvailable: true,
		models:     []providers.Model{{"id": "shared-model"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	cn := &stubProvider{
		id: "workbuddy", realm: "cn", realmAvailable: true,
		models:     []providers.Model{{"id": "shared-model"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	realmService := settings.NewRealm(store, t.TempDir(), []providers.Provider{codearts, cn})
	srv := newTestHTTPServer(ServerOptions{
		Providers: []providers.Provider{codearts, cn},
		Realm:     realmService,
	})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"shared-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := len(codearts.opened()); got != 0 {
		t.Fatalf("CodeArts opened = %d, want 0 (active realm is cn)", got)
	}
	if got := len(cn.opened()); got != 1 {
		t.Fatalf("CN opened = %d, want 1", got)
	}
}

func TestChatActiveRealmFallsBackWhenModelMissing(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	if _, err := store.Merge(map[string]any{"active_realm": "cn"}); err != nil {
		t.Fatalf("seed active realm: %v", err)
	}
	codearts := &stubProvider{
		id: "codearts", realm: "codearts", realmAvailable: true,
		models:     []providers.Model{{"id": "shared-model"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	cn := &stubProvider{
		id: "workbuddy", realm: "cn", realmAvailable: true,
		models:     []providers.Model{{"id": "cn-only"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	realmService := settings.NewRealm(store, t.TempDir(), []providers.Provider{codearts, cn})
	srv := newTestHTTPServer(ServerOptions{
		Providers: []providers.Provider{codearts, cn},
		Realm:     realmService,
	})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"shared-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := len(codearts.opened()); got != 1 {
		t.Fatalf("CodeArts fallback opened = %d, want 1", got)
	}
	if got := len(cn.opened()); got != 0 {
		t.Fatalf("CN opened = %d, want 0", got)
	}
}
