package keys

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func testStore(t *testing.T) (*Store, *config.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	raw := config.New(path)
	store := New(raw)
	store.Now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local) }
	return store, raw
}

func TestKeyStoreLegacyAndList(t *testing.T) {
	store, raw := testStore(t)
	if _, err := raw.Merge(map[string]any{"api_key": "legacy-secret", "api_key_set": true}); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "legacy" || entries[0].Key != "legacy-secret" {
		t.Fatalf("legacy entries = %#v", entries)
	}
	if entries[0].Realm != "" || len(entries[0].Models) != 0 || len(entries[0].Accounts) != 0 {
		t.Fatalf("legacy restrictions = %#v", entries[0])
	}

	if _, err := raw.Merge(map[string]any{"api_keys": []any{
		map[string]any{"id": "k1", "name": "first", "key": "key-1111", "realm": "intl", "models": " DeepSeek/*,deepseek/* "},
		map[string]any{"id": "k1", "name": "second", "key": "key-2222", "realm": "cn", "accounts": []any{"u1", "u1"}, "concurrency_limit": "-2"},
		map[string]any{"id": "k2", "name": "duplicate secret", "key": "key-1111"},
		map[string]any{"id": "k3", "name": "deleted", "key": "", "deleted_at": "2026/11/01 00:00"},
	}}); err != nil {
		t.Fatal(err)
	}
	entries, err = store.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("live entries = %#v", entries)
	}
	if entries[0].ID != "k1" || entries[1].ID != "k1-2" {
		t.Fatalf("ids = %q %q", entries[0].ID, entries[1].ID)
	}
	if got := entries[0].Models; len(got) != 1 || got[0] != "deepseek/*" {
		t.Fatalf("models = %#v", got)
	}
	if entries[1].ConcurrencyLimit != 0 || len(entries[1].Accounts) != 1 {
		t.Fatalf("second entry = %#v", entries[1])
	}
	all, err := store.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[2].ID != "k3" || all[2].DeletedAt == "" || all[2].Key != "" {
		t.Fatalf("all entries = %#v", all)
	}

	// An authoritative empty api_keys list must hide the legacy pair.
	if _, err := raw.Merge(map[string]any{"api_keys": []any{}}); err != nil {
		t.Fatal(err)
	}
	entries, _ = store.List(false)
	if len(entries) != 0 {
		t.Fatalf("empty authoritative list fell back to legacy: %#v", entries)
	}
}

func TestKeyStoreSaveSemantics(t *testing.T) {
	store, raw := testStore(t)
	if _, err := store.Save([]Entry{
		{ID: "k1", Name: "one", Key: "key-one", Realm: "intl"},
		{ID: "k2", Name: "two", Key: "key-two", Realm: "cn"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// Replace-by-omission retires k2.
	live, err := store.Save([]Entry{{ID: "k1", Name: "one", Key: "key-one", Realm: "intl"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "k1" {
		t.Fatalf("live = %#v", live)
	}
	all, _ := store.List(true)
	retired := findEntry(all, "k2")
	if retired == nil || retired.Key != "" || retired.DeletedAt == "" || retired.Enabled {
		t.Fatalf("k2 not retired: %#v", retired)
	}

	// Upsert mode keeps an unmentioned key.
	if _, err := store.Save([]Entry{
		{ID: "k1", Name: "one", Key: "key-one", Realm: "intl"},
		{ID: "k2", Name: "two", Key: "key-two", Realm: "cn"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	live, err = store.Save([]Entry{{ID: "k1", Name: "one", Key: "", Realm: "intl"}}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || findEntry(live, "k1").Key != "key-one" {
		t.Fatalf("upsert live = %#v", live)
	}

	// Explicit delete retires exactly the named id and wipes its secret.
	live, err = store.Save([]Entry{
		{ID: "k1", Name: "one", Key: "key-one"},
		{ID: "k2", Name: "two", Key: ""},
	}, []string{"k2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("live after delete = %#v", live)
	}
	all, _ = store.List(true)
	retired = findEntry(all, "k2")
	if retired == nil || retired.Key != "" || retired.DeletedAt == "" {
		t.Fatalf("k2 delete = %#v", retired)
	}

	// A new secret shorter than four characters is rejected.
	if _, err := store.Save([]Entry{{ID: "short", Key: "abc"}}, nil); err == nil {
		t.Fatal("short key accepted")
	}

	// Upsert with a stale id must not retire an unmentioned key.
	if _, err := store.Save([]Entry{
		{ID: "a", Name: "a", Key: "key-aaaa"},
		{ID: "b", Name: "b", Key: "key-bbbb"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save([]Entry{{ID: "b", Name: "b", Key: ""}}, []string{}); err != nil {
		t.Fatal(err)
	}
	all, _ = store.List(true)
	if findEntry(all, "a") == nil || findEntry(all, "a").DeletedAt != "" {
		t.Fatalf("stale upsert retired unmentioned key: %#v", all)
	}

	// The legacy single-key fields disappear after the first list save.
	data, _ := raw.Load()
	if _, exists := data["api_key"]; exists {
		t.Fatalf("legacy api_key survived save: %#v", data)
	}
}

func TestKeyStoreGatewayRealmPersists(t *testing.T) {
	store, _ := testStore(t)
	live, err := store.Save([]Entry{{
		ID: "all-providers", Name: "all-providers", Key: "key-all", Realm: "gateway",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Realm != "gateway" {
		t.Fatalf("live = %#v", live)
	}
	entries, err := store.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Realm != "gateway" {
		t.Fatalf("entries = %#v", entries)
	}
}

func findEntry(entries []Entry, id string) *Entry {
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i]
		}
	}
	return nil
}

func TestKeyStoreDailyTokenLimit(t *testing.T) {
	store, raw := testStore(t)
	if _, err := raw.Merge(map[string]any{"api_keys": []any{
		map[string]any{"id": "k1", "name": "daily", "key": "key-daily", "daily_token_limit": 2},
		map[string]any{"id": "k2", "name": "invalid", "key": "key-invalid", "daily_token_limit": -1},
	}}); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].DailyTokenLimit != 2 || entries[1].DailyTokenLimit != 0 {
		t.Fatalf("daily limits = %#v", entries)
	}
	if _, err := store.Save([]Entry{{
		ID: "k3", Name: "saved", Key: "key-saved", DailyTokenLimit: 5, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	entries, err = store.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].DailyTokenLimit != 5 {
		t.Fatalf("saved daily limit = %#v", entries)
	}
}
