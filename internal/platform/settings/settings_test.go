package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type settingsFakeProvider struct {
	realm     string
	view      map[string]any
	saves     []map[string]any
	active    string
	healthHit int
}

func (p *settingsFakeProvider) ID() string                                  { return "fake-" + p.realm }
func (p *settingsFakeProvider) Realm() string                               { return p.realm }
func (p *settingsFakeProvider) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *settingsFakeProvider) Health(context.Context, bool) (providers.Health, error) {
	p.healthHit++
	return providers.Health{"ok": true}, nil
}
func (p *settingsFakeProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *settingsFakeProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *settingsFakeProvider) Routes() []providers.Route { return nil }
func (p *settingsFakeProvider) Jobs() []scheduler.Job     { return nil }

func (p *settingsFakeProvider) SettingsView(context.Context) (map[string]any, error) {
	return p.view, nil
}
func (p *settingsFakeProvider) SaveSettings(_ context.Context, patch map[string]any) error {
	p.saves = append(p.saves, patch)
	return nil
}
func (p *settingsFakeProvider) ActiveRealm(context.Context) string { return p.active }
func (p *settingsFakeProvider) SetActiveRealm(_ context.Context, realm string) error {
	p.active = realm
	return nil
}
func (p *settingsFakeProvider) RealmOptions() []string { return []string{"intl", "cn"} }

func TestSettingsMerge(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"future_group": map[string]any{"keep": true, "nested": map[string]any{"x": 1}},
		"media":        map[string]any{"future_media_key": "keep"},
	}); err != nil {
		t.Fatal(err)
	}
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{
		{ID: "k1", Name: "one", Key: "key-one", Realm: "intl", Models: []string{"deepseek/*"}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &settingsFakeProvider{
		realm: "intl",
		view: map[string]any{
			"workbuddy":      map[string]any{"intl": map[string]any{"timeout_seconds": 120}},
			"schedule":       map[string]any{"checkin": []any{9}},
			"daily_activity": true,
			"codearts":       map[string]any{"configured": false},
			"raccoon":        map[string]any{"configured": false},
		},
	}
	service := New(Service{
		Store: store, Keys: keyStore, Providers: []providers.Provider{provider},
		PanelPasswordIsDefault: func() bool { return false },
		SlotViews: func() []map[string]any {
			return []map[string]any{{"id": "slot-1", "label": "exit", "enabled": true}}
		},
		AccountsDir: dir, UsageDir: filepath.Join(dir, "usage"),
		Version: "1.6.5",
	})
	view, err := service.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"api_keys", "deleted_api_keys", "auth_required", "limits",
		"media", "log_db_max_days", "update_check_enabled", "keep_backups",
		"usage", "prompt", "proxySlots", "workbuddy", "schedule", "codearts", "raccoon",
	} {
		if _, ok := view[key]; !ok {
			t.Fatalf("view missing %q: %#v", key, view)
		}
	}
	if view["auth_required"] != true || view["panel_password_is_default"] != false {
		t.Fatalf("flags = %#v", view)
	}
	if got := view["log_db_max_days"]; got != defaultLogMaxDays {
		t.Fatalf("log max days = %#v", got)
	}
	if enabled, rows, days, bytes := service.LogCaps(); enabled || rows != 0 || days != defaultLogMaxDays || bytes != 0 {
		t.Fatalf("log caps = %v %d %d %d", enabled, rows, days, bytes)
	}
	if view["version"] != "1.6.5" || view["project"] != "mix2api" {
		t.Fatalf("metadata = %#v", view)
	}

	reply, err := service.Save(context.Background(), map[string]any{
		"pricing_enabled":         false,
		"pricing_variant_inherit": false,
		"pricing_refresh_minutes": 15,
		"log_db_enabled":          true,
		"log_db_max_rows":         1000,
		"log_db_max_days":         nil,
		"update_check_enabled":    true,
		"keep_backups":            2,
		"ui_language":             "en",
		"section_max_height":      700,
		"media": map[string]any{
			"enabled": true, "realm": "intl", "identity": "cli",
			"image_max_per_minute": 4, "video_max_seconds": 10,
			"daily_credits_per_key": 5, "video_concurrency": 2,
		},
		"usage":  map[string]any{"hour_keep_hours": 24, "day_keep_days": 14},
		"prompt": map[string]any{"mode": "append", "text": "hello"},
		"limits": map[string]any{
			"daily_token_limit": map[string]any{"global": 100, "cn": 50},
		},
		"workbuddy": map[string]any{"intl": map[string]any{"timeout_seconds": 30}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply["pricing_enabled"] != false || reply["log_db_enabled"] != true || reply["keep_backups"] != 2 {
		t.Fatalf("reply = %#v", reply)
	}
	if len(provider.saves) != 1 {
		t.Fatalf("provider saves = %d", len(provider.saves))
	}
	view, err = service.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view["ui_language"] != "en" || view["section_max_height"] != 700 {
		t.Fatalf("panel values = %#v", view)
	}
	media := view["media"].(map[string]any)
	if media["enabled"] != true || media["realm"] != "intl" || media["image_max_per_minute"] != 4 {
		t.Fatalf("media = %#v", media)
	}
	if media["future_media_key"] != "keep" {
		t.Fatalf("unknown nested media key lost: %#v", media)
	}
	data, _ := store.Load()
	future, _ := data["future_group"].(map[string]any)
	if future["keep"] != true {
		t.Fatalf("unknown top-level key lost: %#v", data)
	}
	limits := view["limits"].(map[string]any)
	entry := limits["daily_token_limit"].(map[string]any)
	if entry["global"] != 100 || entry["cn"] != 50 {
		t.Fatalf("limits = %#v", limits)
	}

	// API keys are dispatched to the key store, not written as raw settings.
	reply, err = service.Save(context.Background(), map[string]any{
		"api_keys": []any{
			map[string]any{"id": "k1", "key": "", "name": "renamed", "realm": "cn"},
		},
		"deleted_api_key_ids": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply["api_keys_saved"] != 1 {
		t.Fatalf("reply = %#v", reply)
	}
	entries, _ := keyStore.List(false)
	if len(entries) != 1 || entries[0].Key != "key-one" || entries[0].Name != "renamed" || entries[0].Realm != "cn" {
		t.Fatalf("keys = %#v", entries)
	}
	data, _ = store.Load()
	if _, ok := data["api_key"]; ok {
		t.Fatalf("legacy api_key survived key save: %#v", data)
	}
	if _, ok := data["api_key_set"]; ok {
		t.Fatalf("legacy api_key_set survived key save: %#v", data)
	}

	// Invalid groups keep v1 messages.
	for _, tc := range []struct {
		patch map[string]any
		want  string
	}{
		{map[string]any{"limits": []any{}}, "limits must be an object"},
		{map[string]any{"ui_language": "fr"}, "ui_language must be zh, zh-Hant or en"},
		{map[string]any{"media": map[string]any{"bogus": true}}, "unknown media setting"},
		{map[string]any{"pricing_enabled": "yes"}, "pricing_enabled must be true or false"},
	} {
		if _, err := service.Save(context.Background(), tc.patch); err == nil || !contains(err.Error(), tc.want) {
			t.Fatalf("patch %#v error = %v, want %q", tc.patch, err, tc.want)
		}
	}
}

func TestRealmEndpoint(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider := &settingsFakeProvider{realm: "intl", active: "intl"}
	service := NewRealm(store, dir, []providers.Provider{provider})
	service.Now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	if got := service.Options(); len(got) != 2 || got[0] != "intl" || got[1] != "cn" {
		t.Fatalf("options = %#v", got)
	}
	if got := service.Active(context.Background()); got != "intl" {
		t.Fatalf("initial active = %q", got)
	}
	if err := service.Set(context.Background(), "cn"); err != nil {
		t.Fatal(err)
	}
	if got := service.Active(context.Background()); got != "cn" {
		t.Fatalf("active after set = %q", got)
	}
	if provider.active != "cn" || provider.healthHit == 0 {
		t.Fatalf("selector active=%q health=%d", provider.active, provider.healthHit)
	}
	data, err := os.ReadFile(filepath.Join(dir, "active_realm.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), `"realm": "cn"`) || !contains(string(data), `"updated_iso"`) {
		t.Fatalf("active_realm.json = %s", data)
	}
	if err := service.Set(context.Background(), "bogus"); err == nil {
		t.Fatal("invalid realm accepted")
	}

	// Settings fallback and default.
	store2 := config.New(filepath.Join(t.TempDir(), "settings.json"))
	_, _ = store2.Merge(map[string]any{"active_realm": "cn"})
	fallback := NewRealm(store2, t.TempDir(), []providers.Provider{&settingsFakeProvider{realm: "intl"}})
	if got := fallback.Active(context.Background()); got != "cn" {
		t.Fatalf("settings fallback = %q", got)
	}
	empty := NewRealm(nil, t.TempDir(), nil)
	if got := empty.Active(context.Background()); got != "intl" {
		t.Fatalf("default = %q", got)
	}
}

func contains(value, needle string) bool {
	return len(needle) == 0 || len(value) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(value); i++ {
			if value[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
