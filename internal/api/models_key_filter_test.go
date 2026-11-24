package api

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func modelsFilterFixture(t *testing.T, enabled bool) (*Server, *keys.Store, *stubProvider) {
	t.Helper()
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{"filter_models_by_key": enabled}); err != nil {
		t.Fatal(err)
	}
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{{
		ID: "restricted", Name: "restricted", Key: "rk-test", Realm: "intl",
		Models: []string{"allowed-model"}, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	service := settings.New(settings.Service{Store: store, Keys: keyStore})
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{
		{"id": "allowed-model"}, {"id": "blocked-model"},
	}}
	server := New(ServerOptions{Settings: service, KeyStore: keyStore, Providers: []providers.Provider{provider}, PanelAuth: passThroughAuth})
	return server, keyStore, provider
}

func TestFilterModelsByKey(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		want    []string
	}{
		{false, []string{"allowed-model", "blocked-model"}},
		{true, []string{"allowed-model"}},
	} {
		server, _, _ := modelsFilterFixture(t, tc.enabled)
		rec := serveRequest(server.Handler(), http.MethodGet, "/v1/models?realm=intl", "", map[string]string{
			"Authorization": "Bearer rk-test",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%v status=%d body=%s", tc.enabled, rec.Code, rec.Body)
		}
		body := decodeBody(t, rec)
		data, _ := body["data"].([]any)
		if len(data) != len(tc.want) {
			t.Fatalf("enabled=%v models = %#v, want %v", tc.enabled, data, tc.want)
		}
		for index, entry := range data {
			object, _ := entry.(map[string]any)
			if object["id"] != tc.want[index] {
				t.Fatalf("enabled=%v model %d = %#v, want %s", tc.enabled, index, object["id"], tc.want[index])
			}
		}
	}
}

func TestFilterModelsByKeyGatewayAndAll(t *testing.T) {
	for _, path := range []string{"/v1/models?realm=gateway", "/v1/models?realm=all"} {
		t.Run(path, func(t *testing.T) {
			server, _, _ := modelsFilterFixture(t, true)
			rec := serveRequest(server.Handler(), http.MethodGet, path, "", map[string]string{
				"Authorization": "Bearer rk-test",
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			body := decodeBody(t, rec)
			for _, key := range []string{"data", "models"} {
				if list, ok := body[key].([]any); ok {
					if len(list) != 1 {
						t.Fatalf("%s: filtered models = %#v", key, list)
					}
					return
				}
			}
			t.Fatalf("no model list in %#v", body)
		})
	}
}

func TestFilterModelsByKeyAllMatchesQualifiedAllowList(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{"filter_models_by_key": true}); err != nil {
		t.Fatal(err)
	}
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{{
		ID: "qualified", Name: "qualified", Key: "rk-qualified", Realm: "intl",
		Models: []string{"intl/allowed-model"}, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	service := settings.New(settings.Service{Store: store, Keys: keyStore})
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{
		{"id": "allowed-model"}, {"id": "blocked-model"},
	}}
	server := New(ServerOptions{Settings: service, KeyStore: keyStore, Providers: []providers.Provider{provider}, PanelAuth: passThroughAuth})
	rec := serveRequest(server.Handler(), http.MethodGet, "/v1/models?realm=all", "", map[string]string{
		"Authorization": "Bearer rk-qualified",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	data, _ := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("qualified allow-list returned %#v", data)
	}
	if object, _ := data[0].(map[string]any); object["id"] != "allowed-model" {
		t.Fatalf("model = %#v", data[0])
	}
}
