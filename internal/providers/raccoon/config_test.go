package raccoon

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func TestConfigDefaults(t *testing.T) {
	want := Config{
		Enabled:              false,
		TimeoutSeconds:       60,
		StreamTimeoutSeconds: 900,
		ModelsTTLSeconds:     600,
		MaxConcurrent:        1,
		AutoClaimLogin:       false,
	}

	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	if got := LoadConfig(store); !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
	if got := LoadConfig(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig(nil) = %#v, want %#v", got, want)
	}
}

func TestConfigLoadClampsValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := `{"raccoon":{"enabled":"yes","timeout_seconds":0,"stream_timeout_seconds":999999,"models_ttl_seconds":1,"max_concurrent":99,"auto_claim_login":1}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got := LoadConfig(config.New(p))
	want := Config{
		Enabled:              false,
		TimeoutSeconds:       1,
		StreamTimeoutSeconds: 3600,
		ModelsTTLSeconds:     60,
		MaxConcurrent:        10,
		AutoClaimLogin:       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}

	body = `{"raccoon":{"timeout_seconds":-5,"stream_timeout_seconds":-2,"models_ttl_seconds":1000000000,"max_concurrent":0}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got = LoadConfig(config.New(p))
	want = Config{
		Enabled:              false,
		TimeoutSeconds:       1,
		StreamTimeoutSeconds: 1,
		ModelsTTLSeconds:     86400,
		MaxConcurrent:        1,
		AutoClaimLogin:       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
}

func TestConfigLoadDefaultsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := `{"raccoon":{"enabled":1,"timeout_seconds":"not-a-number","stream_timeout_seconds":null,"models_ttl_seconds":{},"max_concurrent":"many","auto_claim_login":"true"}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got := LoadConfig(config.New(p))
	want := Config{
		Enabled:              false,
		TimeoutSeconds:       60,
		StreamTimeoutSeconds: 900,
		ModelsTTLSeconds:     600,
		MaxConcurrent:        1,
		AutoClaimLogin:       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
}

func TestConfigAcceptsNumericTypes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  float64
	}{
		{"int", 120, 120},
		{"int64", int64(120), 120},
		{"uint", uint(120), 120},
		{"uint32", uint32(120), 120},
		{"float32", float32(120.5), 120.5},
		{"json.Number", json.Number("120.5"), 120.5},
		{"numeric string", " 120.5 ", 120.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := config.New(filepath.Join(t.TempDir(), "settings.json"))
			got, err := SaveConfig(store, map[string]any{"timeout_seconds": tc.value})
			if err != nil {
				t.Fatal(err)
			}
			if got.TimeoutSeconds != tc.want {
				t.Fatalf("TimeoutSeconds = %v, want %v", got.TimeoutSeconds, tc.want)
			}
		})
	}
}

func TestConfigSaveClampsAndPreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := `{"unrelated":{"keep":[1,2]},"raccoon":{"enabled":false,"timeout_seconds":30,"future_raccoon_flag":{"keep":1}}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	store := config.New(p)

	got, err := SaveConfig(store, map[string]any{
		"enabled":                true,
		"timeout_seconds":        2,
		"stream_timeout_seconds": 999999,
		"models_ttl_seconds":     1,
		"max_concurrent":         99,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Enabled:              true,
		TimeoutSeconds:       2,
		StreamTimeoutSeconds: 3600,
		ModelsTTLSeconds:     60,
		MaxConcurrent:        10,
		AutoClaimLogin:       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveConfig() = %#v, want %#v", got, want)
	}

	// A second save only changes the patched known keys.
	got, err = SaveConfig(store, map[string]any{
		"max_concurrent":   2,
		"auto_claim_login": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want.MaxConcurrent = 2
	want.AutoClaimLogin = true
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveConfig() = %#v, want %#v", got, want)
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&settings); err != nil {
		t.Fatal(err)
	}

	raw, ok := settings["raccoon"].(map[string]any)
	if !ok {
		t.Fatalf("raccoon = %#v, want object", settings["raccoon"])
	}
	future, ok := raw["future_raccoon_flag"].(map[string]any)
	if !ok || future["keep"] != json.Number("1") {
		t.Fatalf("future_raccoon_flag = %#v, want keep=1", raw["future_raccoon_flag"])
	}
	unrelated, ok := settings["unrelated"].(map[string]any)
	if !ok {
		t.Fatalf("unrelated = %#v, want object", settings["unrelated"])
	}
	keep, ok := unrelated["keep"].([]any)
	if !ok || len(keep) != 2 || keep[0] != json.Number("1") || keep[1] != json.Number("2") {
		t.Fatalf("unrelated.keep = %#v, want [1,2]", unrelated["keep"])
	}
	if raw["enabled"] != true || raw["timeout_seconds"] != json.Number("2") {
		t.Fatalf("persisted config = %#v, want enabled=true timeout_seconds=2", raw)
	}

	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("settings mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestConfigSaveInvalidValuesFallBackToDefaults(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))

	got, err := SaveConfig(store, map[string]any{
		"timeout_seconds":        "not-a-number",
		"stream_timeout_seconds": nil,
		"models_ttl_seconds":     map[string]any{},
		"max_concurrent":         "many",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Enabled:              false,
		TimeoutSeconds:       60,
		StreamTimeoutSeconds: 900,
		ModelsTTLSeconds:     600,
		MaxConcurrent:        1,
		AutoClaimLogin:       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveConfig() = %#v, want %#v", got, want)
	}

	got, err = SaveConfig(store, map[string]any{
		"timeout_seconds":        math.NaN(),
		"stream_timeout_seconds": math.Inf(1),
		"models_ttl_seconds":     math.Inf(-1),
		"max_concurrent":         math.Inf(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveConfig() = %#v, want %#v", got, want)
	}
}

func TestSaveConfigReturnsStoreErrors(t *testing.T) {
	if _, err := SaveConfig(nil, nil); err == nil {
		t.Fatal("SaveConfig(nil) error = nil, want error")
	}

	store := config.New(filepath.Join(t.TempDir(), "missing", "settings.json"))
	if _, err := SaveConfig(store, map[string]any{"enabled": true}); err == nil {
		t.Fatal("SaveConfig() error = nil, want store error")
	}
}
