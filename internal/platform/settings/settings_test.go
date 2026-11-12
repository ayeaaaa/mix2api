package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

type availableProviderFake struct {
	realm string
}

func (p *availableProviderFake) ID() string                                  { return "available-" + p.realm }
func (p *availableProviderFake) Realm() string                               { return p.realm }
func (p *availableProviderFake) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *availableProviderFake) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *availableProviderFake) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *availableProviderFake) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *availableProviderFake) Routes() []providers.Route { return nil }
func (p *availableProviderFake) Jobs() []scheduler.Job     { return nil }
func (p *availableProviderFake) RealmAvailable() bool      { return true }

func TestRealmEndpointProviderOnlyRealm(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	selector := &settingsFakeProvider{realm: "intl", active: "intl"}
	provider := &availableProviderFake{realm: "doubao"}
	service := NewRealm(store, dir, []providers.Provider{selector, provider})
	if got := service.Options(); len(got) != 3 || got[2] != "doubao" {
		t.Fatalf("options = %#v", got)
	}
	if err := service.Set(context.Background(), "doubao"); err != nil {
		t.Fatal(err)
	}
	if selector.active != "intl" {
		t.Fatalf("WorkBuddy selector was notified for doubao: %q", selector.active)
	}
	if got := service.Active(context.Background()); got != "doubao" {
		t.Fatalf("active after doubao = %q", got)
	}
	if err := service.Set(context.Background(), "cn"); err != nil {
		t.Fatal(err)
	}
	if selector.active != "cn" {
		t.Fatalf("WorkBuddy selector active = %q", selector.active)
	}
}

func TestAPIKeyAcceptsDoubaoRealm(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	keyStore := keys.New(store)
	provider := &settingsFakeProvider{realm: "doubao", view: map[string]any{"doubao": map[string]any{"enabled": true}}}
	service := New(Service{
		Store: store, Keys: keyStore, Providers: []providers.Provider{provider},
		AccountsDir: dir, UsageDir: filepath.Join(dir, "usage"),
	})
	_, err := service.Save(context.Background(), map[string]any{
		"api_keys": []any{
			map[string]any{"key": "doubao-key-1234", "name": "doubao", "realm": "doubao", "enabled": true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := keyStore.List(false)
	if err != nil || len(entries) != 1 || entries[0].Realm != "doubao" {
		t.Fatalf("keys = %#v, err = %v", entries, err)
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

type settingsRealmOnlyProvider struct {
	realm     string
	available bool
}

func (p *settingsRealmOnlyProvider) ID() string    { return p.realm }
func (p *settingsRealmOnlyProvider) Realm() string { return p.realm }
func (p *settingsRealmOnlyProvider) Overview(context.Context, bool) (any, error) {
	return nil, nil
}
func (p *settingsRealmOnlyProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *settingsRealmOnlyProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *settingsRealmOnlyProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *settingsRealmOnlyProvider) Routes() []providers.Route { return nil }
func (p *settingsRealmOnlyProvider) Jobs() []scheduler.Job     { return nil }
func (p *settingsRealmOnlyProvider) RealmAvailable() bool      { return p.available }

func TestRealmOptionsIncludeConfiguredExits(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	workbuddy := &settingsFakeProvider{realm: "intl", active: "intl"}
	raccoon := &settingsRealmOnlyProvider{realm: "raccoon", available: true}
	codearts := &settingsRealmOnlyProvider{realm: "codearts", available: false}
	service := NewRealm(store, dir, []providers.Provider{workbuddy, raccoon, codearts})
	options := service.Options()
	want := []string{"intl", "cn", "raccoon"}
	if len(options) != len(want) {
		t.Fatalf("options = %#v, want %#v", options, want)
	}
	for index := range want {
		if options[index] != want[index] {
			t.Fatalf("options = %#v, want %#v", options, want)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "active_realm.json"), []byte(`{"realm":"raccoon"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := service.Active(context.Background()); got != "raccoon" {
		t.Fatalf("active realm = %q, want raccoon", got)
	}
}

func TestLimitsScopedMerge(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{"limits": map[string]any{
		"daily_token_limit": map[string]any{"global": 100, "intl": 50, "cn": 70},
	}}); err != nil {
		t.Fatal(err)
	}
	service := New(Service{Store: store, AccountsDir: dir})
	if _, err := service.Save(context.Background(), map[string]any{
		"limits": map[string]any{"daily_token_limit": map[string]any{"cn": 40}},
	}); err != nil {
		t.Fatal(err)
	}
	view, _ := service.View(context.Background())
	entry := view["limits"].(map[string]any)["daily_token_limit"].(map[string]any)
	if entry["global"] != 100 || entry["intl"] != 50 || entry["cn"] != 40 {
		t.Fatalf("scoped patch lost siblings: %#v", entry)
	}

	// Flat global updates preserve realm overrides.
	if _, err := service.Save(context.Background(), map[string]any{"daily_token_limit": 200}); err != nil {
		t.Fatal(err)
	}
	view, _ = service.View(context.Background())
	entry = view["limits"].(map[string]any)["daily_token_limit"].(map[string]any)
	if entry["global"] != 200 || entry["intl"] != 50 || entry["cn"] != 40 {
		t.Fatalf("flat patch lost overrides: %#v", entry)
	}

	// Clearing a realm override returns it to inherit, not zero.
	if _, err := service.Save(context.Background(), map[string]any{
		"limits": map[string]any{"daily_token_limit": map[string]any{"cn": nil}},
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = service.View(context.Background())
	entry = view["limits"].(map[string]any)["daily_token_limit"].(map[string]any)
	if entry["cn"] != nil || entry["intl"] != 50 {
		t.Fatalf("override clear = %#v", entry)
	}
}

func TestLegacyLimitsMigration(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"daily_token_limit": 123,
		"reserve_credits":   5,
		"future":            map[string]any{"keep": true},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(Service{Store: store, AccountsDir: dir})
	view, err := service.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	limits := view["limits"].(map[string]any)
	daily := limits["daily_token_limit"].(map[string]any)
	if daily["global"] != 123 || view["reserve_credits"] != 5 {
		t.Fatalf("legacy limits not read: %#v", view)
	}
	if limits["expiring_window_days"].(map[string]any)["global"] != 7 {
		t.Fatalf("expiring default = %#v", limits["expiring_window_days"])
	}
	data, _ := store.Load()
	if _, exists := data["daily_token_limit"]; exists {
		t.Fatalf("legacy flat key not migrated: %#v", data)
	}
	if _, exists := data["reserve_credits"]; exists {
		t.Fatalf("legacy reserve_credits not migrated: %#v", data)
	}
	if _, exists := data["limits"]; !exists {
		t.Fatalf("limits group missing after migration: %#v", data)
	}
	if future, _ := data["future"].(map[string]any); future["keep"] != true {
		t.Fatalf("unknown key lost during migration: %#v", data)
	}

	// A flat update after migration keeps the legacy global as the base while
	// applying only the named guard.
	if _, err := service.Save(context.Background(), map[string]any{"daily_credit_limit": 9}); err != nil {
		t.Fatal(err)
	}
	view, _ = service.View(context.Background())
	limits = view["limits"].(map[string]any)
	if limits["daily_token_limit"].(map[string]any)["global"] != 123 ||
		limits["daily_credit_limit"].(map[string]any)["global"] != 9 {
		t.Fatalf("post-migration patch = %#v", limits)
	}
}

func TestRuntimeSettingsDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{"proxy_slots": []any{
		map[string]any{"id": "slot-1", "url": "http://127.0.0.1:7890"},
	}}); err != nil {
		t.Fatal(err)
	}
	service := New(Service{Store: store, AccountsDir: dir})
	view, err := service.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view["affinity_stable_key_enabled"] != false {
		t.Fatalf("affinity_stable_key_enabled default = %#v, want false", view["affinity_stable_key_enabled"])
	}
	if view["reasoning_effort_map"] != true {
		t.Fatalf("reasoning_effort_map default = %#v, want true", view["reasoning_effort_map"])
	}
	if got := view["proxySlots"].([]map[string]any); len(got) != 1 || got[0]["id"] != "slot-1" {
		t.Fatalf("proxy slot fallback = %#v", got)
	}
	if got := view["update_check_proxy_slots"].([]map[string]any); len(got) != 1 {
		t.Fatalf("update slot fallback = %#v", got)
	}
	for _, tc := range []struct {
		name  string
		patch map[string]any
		want  string
	}{
		{"hidden string", map[string]any{"hidden_pages": "tasks"}, "hidden_pages must be a list of page keys"},
		{"hidden non-string", map[string]any{"hidden_pages": []any{1}}, "hidden_pages must be a list of page keys"},
		{"slot non-string", map[string]any{"update_check_proxy_slot": 1}, "update_check_proxy_slot must be a string"},
		{"slot missing", map[string]any{"update_check_proxy_slot": "slot-9"}, "update_check_proxy_slot does not exist"},
		{"keep bool", map[string]any{"keep_backups": true}, "keep_backups must be an integer 0-3"},
		{"keep string", map[string]any{"keep_backups": "2"}, "keep_backups must be an integer 0-3"},
		{"keep fraction", map[string]any{"keep_backups": 1.5}, "keep_backups must be an integer 0-3"},
	} {
		if _, err := service.Save(context.Background(), tc.patch); err == nil || !contains(err.Error(), tc.want) {
			t.Fatalf("%s error = %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, err := service.Save(context.Background(), map[string]any{"update_check_proxy_slot": "slot-1"}); err != nil {
		t.Fatalf("valid slot rejected: %v", err)
	}
	if _, err := service.Save(context.Background(), map[string]any{"hidden_pages": []any{"tasks", "settings", "bad key"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), map[string]any{"keep_backups": 99}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), map[string]any{
		"log_db_max_rows": true, "log_db_max_days": -1, "log_db_max_bytes": "nonsense",
	}); err != nil {
		t.Fatalf("unparseable caps should normalize to unlimited: %v", err)
	}
	view, _ = service.View(context.Background())
	if view["keep_backups"] != maxKeepBackups {
		t.Fatalf("keep_backups clamp = %#v", view["keep_backups"])
	}
	if view["hidden_pages"].([]string)[0] != "tasks" {
		t.Fatalf("hidden pages = %#v", view["hidden_pages"])
	}
	for _, key := range []string{"log_db_max_rows", "log_db_max_days", "log_db_max_bytes"} {
		if view[key] != nil {
			t.Fatalf("%s = %#v, want unlimited", key, view[key])
		}
	}
	enabled, rows, days, bytes := service.LogCaps()
	if enabled || rows != 0 || days != 0 || bytes != 0 {
		t.Fatalf("LogCaps after clear = %v %d %d %d", enabled, rows, days, bytes)
	}
}

func TestSiteBrandingSettings(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	service := New(Service{Store: store, AccountsDir: dir})
	view, err := service.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view["site_title"] != "" || view["site_icon"] != "" {
		t.Fatalf("default branding = %#v / %#v", view["site_title"], view["site_icon"])
	}
	title := strings.Repeat("x", 100)
	icon := "data:image/png;base64,iVBORw0KGgo="
	saved, err := service.Save(context.Background(), map[string]any{
		"site_title": "  My Gateway  ",
		"site_icon":  icon,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := saved["site_title"]; got != "My Gateway" {
		t.Fatalf("saved title = %#v", got)
	}
	view, err = service.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view["site_title"] != "My Gateway" || view["site_icon"] != icon {
		t.Fatalf("branding view = %#v / %#v", view["site_title"], view["site_icon"])
	}
	saved, err = service.Save(context.Background(), map[string]any{"site_title": title})
	if err != nil {
		t.Fatal(err)
	}
	if got := []rune(saved["site_title"].(string)); len(got) != siteTitleMaxRunes {
		t.Fatalf("clamped title length = %d", len(got))
	}
	saved, err = service.Save(context.Background(), map[string]any{"site_icon": "javascript:alert(1)"})
	if err != nil {
		t.Fatal(err)
	}
	if saved["site_icon"] != "" {
		t.Fatalf("unsafe icon = %#v", saved["site_icon"])
	}
	for _, tc := range []struct {
		name  string
		patch map[string]any
		want  string
	}{
		{"title type", map[string]any{"site_title": 1}, "site_title must be a string"},
		{"icon type", map[string]any{"site_icon": true}, "site_icon must be a string"},
	} {
		if _, err := service.Save(context.Background(), tc.patch); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s error = %v, want %q", tc.name, err, tc.want)
		}
	}
}
