package doubao

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func TestProviderDisabledThenEnabled(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Models(context.Background(), false); err == nil {
		t.Fatal("disabled provider returned models")
	}
	if provider.RealmAvailable() {
		t.Fatal("disabled provider is realm-available")
	}
	if err := provider.SaveSettings(context.Background(), map[string]any{
		"doubao": map[string]any{"enabled": true, "max_concurrent": float64(2)},
	}); err != nil {
		t.Fatal(err)
	}
	models, err := provider.Models(context.Background(), false)
	if err != nil || len(models) != 12 {
		t.Fatalf("models = %d, %v", len(models), err)
	}
	if !provider.RealmAvailable() {
		// No accounts yet: models can be served, but /realm should stay hidden.
		t.Log("realm hidden until an account is imported")
	}
	if _, err := provider.pool.Add(map[string]string{"sessionid": "s"}, map[string]string{}, "", "tester", true); err != nil {
		t.Fatal(err)
	}
	if !provider.RealmAvailable() {
		t.Fatal("enabled provider with account is not realm-available")
	}
	health, err := provider.Health(context.Background(), false)
	accounts, _ := numberValue(health["accounts"])
	if err != nil || health["ok"] != true || accounts != 1 {
		t.Fatalf("health = %#v, %v", health, err)
	}
	view, err := provider.SettingsView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	section := view["doubao"].(map[string]any)
	if section["enabled"] != true {
		t.Fatalf("settings view = %#v", section)
	}
}
