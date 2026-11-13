package api

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	platformproxy "github.com/ayeaaaa/mix2api-v2/internal/platform/proxy"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestRealmGetReachableWithAPIKey(t *testing.T) {
	store, keyStore, _, _, dir := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{{ID: "k1", Key: "key-one", Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	selector := &realmSelectorStub{realm: "intl", active: "intl"}
	realmService := settings.NewRealm(store, dir, []providers.Provider{selector})
	server := New(ServerOptions{
		KeyStore: keyStore,
		Realm:    realmService,
		Settings: settings.New(settings.Service{Store: store, Keys: keyStore, AccountsDir: dir}),
	})
	rec := doRequest(server, http.MethodGet, "/realm", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("realm get without key = %d", rec.Code)
	}
	rec = doRequest(server, http.MethodGet, "/realm", "", map[string]string{"Authorization": "Bearer key-one"})
	if rec.Code != http.StatusOK {
		t.Fatalf("realm get with key = %d body=%s", rec.Code, rec.Body)
	}
	rec = doRequest(server, http.MethodPost, "/realm", `{"realm":"cn"}`, map[string]string{"Authorization": "Bearer key-one"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("realm post with API key = %d body=%s", rec.Code, rec.Body)
	}
}

func TestStaticAPIKeyAuthWithKeyStore(t *testing.T) {
	_, keyStore, _, _, dir := platformFixture(t)
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{{"id": "m1"}}}
	_ = dir
	server := New(ServerOptions{
		Providers:  []providers.Provider{provider},
		KeyStore:   keyStore,
		APIKeyAuth: RequireAPIKeyAuth("static-key"),
	})
	rec := doRequest(server, http.MethodGet, "/v1/models?realm=intl", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty store/no key = %d", rec.Code)
	}
	rec = doRequest(server, http.MethodGet, "/v1/models?realm=intl", "", map[string]string{"Authorization": "Bearer static-key"})
	if rec.Code != http.StatusOK {
		t.Fatalf("static key with key store = %d body=%s", rec.Code, rec.Body)
	}
}

func TestCompositionSiblingsFillSettings(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	keyStore := keys.New(store)
	password := panelplatform.NewPassword(store)
	slots := platformproxy.New(store, nil)
	service := settings.New(settings.Service{Store: store, AccountsDir: dir})
	server := New(ServerOptions{
		KeyStore:      keyStore,
		PanelPassword: password,
		ProxySlots:    slots,
		Settings:      service,
	})
	if server.settings.Keys != keyStore {
		t.Fatal("settings did not inherit key store")
	}
	if server.settings.PanelPasswordIsDefault == nil || !server.settings.PanelPasswordIsDefault() {
		t.Fatal("settings did not inherit panel default callback")
	}
	if server.settings.SlotViews == nil {
		t.Fatal("settings did not inherit proxy slot views")
	}
	if server.settings.SettingsFile != store.Path {
		t.Fatalf("settings file = %q", server.settings.SettingsFile)
	}
	if !keyStore.AuthRequired() {
		t.Fatal("empty key store should fail closed")
	}
}

func TestEmptyKeyStoreAuthRequired(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	keyStore := keys.New(store)
	if !keyStore.AuthRequired() {
		t.Fatal("empty key store reported auth not required")
	}
	if err := keyStore.SetAuthDisabled(true); err != nil {
		t.Fatal(err)
	}
	if keyStore.AuthRequired() {
		t.Fatal("auth_disabled still required a key")
	}
}
