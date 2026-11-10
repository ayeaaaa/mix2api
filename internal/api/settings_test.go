package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestSettingsMergeAPI(t *testing.T) {
	store, keyStore, _, _, dir := platformFixture(t)
	if _, err := store.Merge(map[string]any{
		"future_group": map[string]any{"keep": true},
	}); err != nil {
		t.Fatal(err)
	}
	provider := &settingsProviderStub{
		realm: "intl",
		view: map[string]any{
			"workbuddy": map[string]any{"intl": map[string]any{"timeout_seconds": 120}},
			"codearts":  map[string]any{"configured": false},
			"raccoon":   map[string]any{"configured": false},
		},
	}
	service := settings.New(settings.Service{
		Store: store, Keys: keyStore, Providers: []providers.Provider{provider},
		AccountsDir: dir, UsageDir: filepath.Join(dir, "usage"), Version: "2.0.0-test",
	})
	server := New(ServerOptions{Settings: service, PanelAuth: passThroughAuth, KeyStore: keyStore})

	rec := doRequest(server, http.MethodPost, "/settings/save", `{
		"pricing_enabled": false,
		"pricing_refresh_minutes": 15,
		"log_db_enabled": true,
		"log_db_max_days": null,
		"media": {"enabled": true, "realm": "intl"},
		"api_keys": [{"id":"", "name":"new", "key":"key-new", "realm":"cn"}],
		"deleted_api_key_ids": []
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rec.Code, rec.Body)
	}
	reply := decodeBody(t, rec)
	if reply["ok"] != true || reply["pricing_enabled"] != false || reply["api_keys_saved"] == nil {
		t.Fatalf("reply = %#v", reply)
	}
	if len(provider.saves) != 1 {
		t.Fatalf("provider saves = %d", len(provider.saves))
	}

	rec = doRequest(server, http.MethodGet, "/settings", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("view status=%d body=%s", rec.Code, rec.Body)
	}
	view := decodeBody(t, rec)
	keysView, _ := view["api_keys"].([]any)
	if len(keysView) != 1 {
		t.Fatalf("api_keys = %#v", view["api_keys"])
	}
	if view["pricing_enabled"] != false || view["version"] != "2.0.0-test" {
		t.Fatalf("view = %#v", view)
	}
	media, _ := view["media"].(map[string]any)
	if media["enabled"] != true || media["realm"] != "intl" {
		t.Fatalf("media = %#v", media)
	}
	if _, ok := view["workbuddy"]; !ok {
		t.Fatalf("provider section missing: %#v", view)
	}
	data, _ := store.Load()
	future, _ := data["future_group"].(map[string]any)
	if future["keep"] != true {
		t.Fatalf("unknown group lost: %#v", data)
	}

	// Reveal requires the panel gate (pass-through here) and resolves live ids.
	rec = doRequest(server, http.MethodGet, "/settings/reveal?id="+urlQueryEscape(firstKeyID(view)), "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal status=%d body=%s", rec.Code, rec.Body)
	}
	if decodeBody(t, rec)["key"] != "key-new" {
		t.Fatalf("reveal body = %s", rec.Body)
	}
	rec = doRequest(server, http.MethodGet, "/settings/reveal?id=missing", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing reveal status=%d", rec.Code)
	}
}

func TestRealmEndpointAPI(t *testing.T) {
	store, _, _, _, dir := platformFixture(t)
	selector := &realmSelectorStub{active: "intl", realm: "intl"}
	realmService := settings.NewRealm(store, dir, []providers.Provider{selector})
	provider := &settingsProviderStub{
		realm: "intl",
		view:  map[string]any{"workbuddy": map[string]any{}},
	}
	service := settings.New(settings.Service{
		Store: store, Providers: []providers.Provider{provider},
		AccountsDir: dir, Version: "2.0.0-test",
	})
	server := New(ServerOptions{
		Settings: service, Realm: realmService, PanelAuth: passThroughAuth,
		Providers: []providers.Provider{provider, selector},
	})
	rec := doRequest(server, http.MethodGet, "/realm", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get realm status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["current"] != "intl" {
		t.Fatalf("realm = %#v", body)
	}
	options, _ := body["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("options = %#v", options)
	}
	rec = doRequest(server, http.MethodPost, "/realm", `{"realm":"cn"}`, nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["current"] != "cn" {
		t.Fatalf("set realm = %d %s", rec.Code, rec.Body)
	}
	if selector.active != "cn" {
		t.Fatalf("selector active = %q", selector.active)
	}
	data, err := os.ReadFile(filepath.Join(dir, "active_realm.json"))
	if err != nil || !containsText(string(data), `"realm": "cn"`) {
		t.Fatalf("active_realm.json = %s err=%v", data, err)
	}
	rec = doRequest(server, http.MethodPost, "/realm", `{"realm":"bogus"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid realm status=%d body=%s", rec.Code, rec.Body)
	}
}

type realmSelectorStub struct {
	realm  string
	active string
}

func (p *realmSelectorStub) ID() string                                  { return "realm" }
func (p *realmSelectorStub) Realm() string                               { return p.realm }
func (p *realmSelectorStub) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *realmSelectorStub) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *realmSelectorStub) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *realmSelectorStub) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *realmSelectorStub) Routes() []providers.Route          { return nil }
func (p *realmSelectorStub) Jobs() []scheduler.Job              { return nil }
func (p *realmSelectorStub) ActiveRealm(context.Context) string { return p.active }
func (p *realmSelectorStub) SetActiveRealm(_ context.Context, realm string) error {
	p.active = realm
	return nil
}
func (p *realmSelectorStub) RealmOptions() []string { return []string{"intl", "cn"} }

func firstKeyID(view map[string]any) string {
	list, _ := view["api_keys"].([]any)
	if len(list) == 0 {
		return ""
	}
	entry, _ := list[0].(map[string]any)
	id, _ := entry["id"].(string)
	return id
}

func urlQueryEscape(value string) string {
	replacer := map[byte]string{
		' ': "%20", '+': "%2B", '&': "%26", '=': "%3D", '?': "%3F", '#': "%23", '/': "%2F",
	}
	out := ""
	for i := 0; i < len(value); i++ {
		if escaped, ok := replacer[value[i]]; ok {
			out += escaped
		} else {
			out += string(value[i])
		}
	}
	return out
}
