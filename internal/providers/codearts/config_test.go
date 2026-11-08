package codearts

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
		Enabled:                 false,
		TimeoutSeconds:          30,
		StreamTimeoutSeconds:    600,
		ModelsTTLSeconds:        300,
		MaxConcurrent:           1,
		BenefitAutoClaim:        true,
		CreditsAutoClaim:        true,
		CreditsExpiringPriority: true,
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
	body := `{"codearts":{"enabled":true,"timeout_seconds":0,"stream_timeout_seconds":999999,"models_ttl_seconds":1,"max_concurrent":99,"benefit_auto_claim":false,"credits_auto_claim":false,"credits_expiring_priority":false}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got := LoadConfig(config.New(p))
	want := Config{
		Enabled:                 true,
		TimeoutSeconds:          1,
		StreamTimeoutSeconds:    3600,
		ModelsTTLSeconds:        60,
		MaxConcurrent:           20,
		BenefitAutoClaim:        false,
		CreditsAutoClaim:        false,
		CreditsExpiringPriority: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}

	body = `{"codearts":{"enabled":false,"timeout_seconds":-5,"stream_timeout_seconds":-2,"models_ttl_seconds":1000000000,"max_concurrent":0,"benefit_auto_claim":true,"credits_auto_claim":true,"credits_expiring_priority":true}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got = LoadConfig(config.New(p))
	want = Config{
		Enabled:                 false,
		TimeoutSeconds:          1,
		StreamTimeoutSeconds:    1,
		ModelsTTLSeconds:        86400,
		MaxConcurrent:           1,
		BenefitAutoClaim:        true,
		CreditsAutoClaim:        true,
		CreditsExpiringPriority: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
}

func TestConfigLoadDefaultsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	// Auto-claim switches only turn off for the literal JSON false, matching
	// v1's `value is not False`. Strings, zero, and null keep the default on.
	body := `{"codearts":{"enabled":1,"timeout_seconds":"not-a-number","stream_timeout_seconds":null,"models_ttl_seconds":{},"max_concurrent":"many","benefit_auto_claim":"false","credits_auto_claim":0,"credits_expiring_priority":null}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got := LoadConfig(config.New(p))
	want := Config{
		Enabled:                 false,
		TimeoutSeconds:          30,
		StreamTimeoutSeconds:    600,
		ModelsTTLSeconds:        300,
		MaxConcurrent:           1,
		BenefitAutoClaim:        true,
		CreditsAutoClaim:        true,
		CreditsExpiringPriority: true,
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
			got, err := SaveConfig(store, map[string]any{
				"timeout_seconds": tc.value,
				"max_concurrent":  tc.value,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.TimeoutSeconds != tc.want {
				t.Fatalf("TimeoutSeconds = %v, want %v", got.TimeoutSeconds, tc.want)
			}
			if got.MaxConcurrent != 20 {
				t.Fatalf("MaxConcurrent = %d, want 20", got.MaxConcurrent)
			}
		})
	}
}

func TestConfigAcceptsV1BooleanNumbers(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	got, err := SaveConfig(store, map[string]any{
		"timeout_seconds": true,
		"max_concurrent":  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSeconds != 1 || got.MaxConcurrent != 1 {
		t.Fatalf("boolean numeric values = timeout %v max %d, want 1 and 1",
			got.TimeoutSeconds, got.MaxConcurrent)
	}
}

func TestConfigSaveClampsAndPreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := `{"unrelated":{"keep":[1,2]},"codearts":{"enabled":false,"timeout_seconds":30,"future_codearts_flag":{"keep":1},"future_scalar":"keep me"}}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	store := config.New(p)

	got, err := SaveConfig(store, map[string]any{
		"enabled":                   true,
		"timeout_seconds":           2,
		"stream_timeout_seconds":    999999,
		"models_ttl_seconds":        1,
		"max_concurrent":            99,
		"benefit_auto_claim":        false,
		"credits_auto_claim":        false,
		"credits_expiring_priority": false,
		"unknown_patch_key":         "must not be stored",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Enabled:                 true,
		TimeoutSeconds:          2,
		StreamTimeoutSeconds:    3600,
		ModelsTTLSeconds:        60,
		MaxConcurrent:           20,
		BenefitAutoClaim:        false,
		CreditsAutoClaim:        false,
		CreditsExpiringPriority: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveConfig() = %#v, want %#v", got, want)
	}

	// A second save only changes the patched known keys.
	got, err = SaveConfig(store, map[string]any{"max_concurrent": 2})
	if err != nil {
		t.Fatal(err)
	}
	want.MaxConcurrent = 2
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

	raw, ok := settings["codearts"].(map[string]any)
	if !ok {
		t.Fatalf("codearts = %#v, want object", settings["codearts"])
	}
	future, ok := raw["future_codearts_flag"].(map[string]any)
	if !ok || future["keep"] != json.Number("1") {
		t.Fatalf("future_codearts_flag = %#v, want keep=1", raw["future_codearts_flag"])
	}
	if raw["future_scalar"] != "keep me" {
		t.Fatalf("future_scalar = %#v, want keep me", raw["future_scalar"])
	}
	unrelated, ok := settings["unrelated"].(map[string]any)
	if !ok {
		t.Fatalf("unrelated = %#v, want object", settings["unrelated"])
	}
	keep, ok := unrelated["keep"].([]any)
	if !ok || len(keep) != 2 || keep[0] != json.Number("1") || keep[1] != json.Number("2") {
		t.Fatalf("unrelated.keep = %#v, want [1,2]", unrelated["keep"])
	}
	if _, exists := raw["unknown_patch_key"]; exists {
		t.Fatalf("unknown patch key was persisted: %#v", raw)
	}
	if raw["enabled"] != true || raw["timeout_seconds"] != json.Number("2") {
		t.Fatalf("persisted config = %#v, want enabled=true timeout_seconds=2", raw)
	}
	if raw["benefit_auto_claim"] != false || raw["credits_auto_claim"] != false ||
		raw["credits_expiring_priority"] != false {
		t.Fatalf("persisted auto-claim switches = %#v, want false", raw)
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
		Enabled:                 false,
		TimeoutSeconds:          30,
		StreamTimeoutSeconds:    600,
		ModelsTTLSeconds:        300,
		MaxConcurrent:           1,
		BenefitAutoClaim:        true,
		CreditsAutoClaim:        true,
		CreditsExpiringPriority: true,
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
