package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
