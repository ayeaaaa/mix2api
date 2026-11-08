package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestConcurrentMergesPreserveBothKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"raccoon":{"enabled":true},"keep":{"nested":1}}`), 0600); err != nil {
		t.Fatal(err)
	}

	const merges = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, merges)
	for i := 0; i < merges; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			// Alternate Store instances to exercise the shared path lock.
			store := New(path)
			_, err := store.Merge(map[string]any{
				"concurrent": map[string]any{fmt.Sprintf("k%02d", index): index},
			})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Merge: %v", err)
		}
	}

	settings, err := New(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	concurrent, ok := settings["concurrent"].(map[string]any)
	if !ok {
		t.Fatalf("concurrent group = %#v", settings["concurrent"])
	}
	if len(concurrent) != merges {
		t.Fatalf("concurrent keys = %d, want %d: %#v", len(concurrent), merges, concurrent)
	}
	for i := 0; i < merges; i++ {
		key := fmt.Sprintf("k%02d", i)
		if value, ok := concurrent[key].(json.Number); !ok || value.String() != fmt.Sprint(i) {
			t.Fatalf("concurrent[%s] = %#v, want %d", key, concurrent[key], i)
		}
	}
	if settings["raccoon"].(map[string]any)["enabled"] != true {
		t.Fatalf("original settings were lost: %#v", settings)
	}
	if settings["keep"].(map[string]any)["nested"] != json.Number("1") {
		t.Fatalf("unknown settings were lost: %#v", settings)
	}
}

// TestV1SettingsShapeRoundTrip exercises a v1-shaped settings document with
// unknown keys, numeric values, and nested maps. It must remain loadable by v1
// after a v2 merge and atomic rewrite.
func TestV1SettingsShapeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{
		"raccoon": {"enabled": true, "max_concurrent": 2, "future": {"x": 1}},
		"limits": {"reserve_credits": {"global": 5}},
		"proxy_slots": [{"id": "slot-1", "url": "http://127.0.0.1:8080", "enabled": true}],
		"future_top_level": [1, 2, {"keep": true}]
	}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	merged, err := New(path).Merge(map[string]any{
		"raccoon": map[string]any{"max_concurrent": 3, "enabled": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged["raccoon"].(map[string]any)["future"].(map[string]any)["x"] != json.Number("1") {
		t.Fatalf("unknown nested key was lost: %#v", merged)
	}

	// A plain json.Unmarshal is what v1 uses for settings.json.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v1View map[string]any
	if err := json.Unmarshal(raw, &v1View); err != nil {
		t.Fatalf("v1 settings parse failed: %v", err)
	}
	raccoon, ok := v1View["raccoon"].(map[string]any)
	if !ok || raccoon["enabled"] != false || raccoon["max_concurrent"] != float64(3) {
		t.Fatalf("v1 raccoon settings = %#v", v1View["raccoon"])
	}
	if _, ok := v1View["future_top_level"]; !ok {
		t.Fatalf("v1 unknown top-level key was dropped: %#v", v1View)
	}
	if v1View["limits"].(map[string]any)["reserve_credits"].(map[string]any)["global"] != float64(5) {
		t.Fatalf("v1 limits were lost: %#v", v1View["limits"])
	}
}

func TestMergePreservesUnknownNestedKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"raccoon":{"enabled":true,"future":{"x":1}},"other":{"keep":2}}`), 0600)
	s := New(p)
	got, err := s.Merge(map[string]any{"raccoon": map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	if got["raccoon"].(map[string]any)["future"].(map[string]any)["x"] != json.Number("1") {
		t.Fatal(got)
	}
	if got["other"].(map[string]any)["keep"] != json.Number("2") {
		t.Fatal(got)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestMalformedSettingsFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(`{"raccoon":`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := New(p).Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load() = %#v, want empty map", got)
	}
}

func TestLoadMissingFileReturnsEmptyMap(t *testing.T) {
	got, err := New(filepath.Join(t.TempDir(), "settings.json")).Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("Load() = %#v, want empty map", got)
	}
}

func TestMergeReplacesScalarAndArrayValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := []byte(`{"raccoon":{"enabled":true,"limits":{"daily":5},"models":["old"],"note":"keep"},"other":{"keep":2}}`)
	if err := os.WriteFile(p, body, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := New(p).Merge(map[string]any{
		"raccoon": map[string]any{
			"limits": 9,
			"models": []any{"new"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	raccoon := got["raccoon"].(map[string]any)
	if raccoon["limits"] != 9 {
		t.Fatalf("limits = %#v, want 9", raccoon["limits"])
	}
	if !reflect.DeepEqual(raccoon["models"], []any{"new"}) {
		t.Fatalf("models = %#v, want [new]", raccoon["models"])
	}
	if raccoon["enabled"] != true || raccoon["note"] != "keep" {
		t.Fatalf("unknown keys changed: %#v", raccoon)
	}
	if got["other"].(map[string]any)["keep"] != json.Number("2") {
		t.Fatalf("other = %#v, want keep=2", got["other"])
	}

	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "settings.json" {
		t.Fatalf("unexpected leftover files: %v", entries)
	}

	reloaded, err := New(p).Load()
	if err != nil {
		t.Fatal(err)
	}
	reloadedRaccoon := reloaded["raccoon"].(map[string]any)
	if reloadedRaccoon["limits"] != json.Number("9") {
		t.Fatalf("rewritten limits = %#v, want json.Number(9)", reloadedRaccoon["limits"])
	}
	if !reflect.DeepEqual(reloadedRaccoon["models"], []any{"new"}) {
		t.Fatalf("rewritten models = %#v, want [new]", reloadedRaccoon["models"])
	}
}
