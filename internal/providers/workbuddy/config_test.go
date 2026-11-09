package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func TestConfigDefaultsForRealms(t *testing.T) {
	for _, realm := range []string{"intl", "cn"} {
		t.Run(realm, func(t *testing.T) {
			want := Config{
				Enabled:              true,
				Realm:                realm,
				TimeoutSeconds:       120,
				StreamTimeoutSeconds: 300,
				ModelsTTLSeconds:     300,
				MaxConcurrent:        32,
				AccountMaxConcurrent: 0,
				AutoSwitchProduct:    false,
				AutoSwitchProducts:   []string{"workbuddy", "vscode", "cli"},
				IdentityVersions:     Versions{},
				PrivateData:          "auto",
			}
			store := config.New(filepath.Join(t.TempDir(), "settings.json"))
			if got := LoadConfig(store, realm); !reflect.DeepEqual(got, want) {
				t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
			}
			if got := LoadConfig(nil, realm); !reflect.DeepEqual(got, want) {
				t.Fatalf("LoadConfig(nil) = %#v, want %#v", got, want)
			}
		})
	}
}

func TestConfigLoadClampsValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{"workbuddy":{"intl":{"enabled":false,"timeout_seconds":0,"stream_timeout_seconds":999999,"models_ttl_seconds":1,"max_concurrent":9999,"account_max_concurrent":99,"auto_switch_product":true,"auto_switch_products":["cli","nonsense","cli"],"identity_versions":{"desktop_intl":"9.9.9"},"private_data":"false"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadConfig(config.New(path), "intl")
	want := Config{
		Enabled:              false,
		Realm:                "intl",
		TimeoutSeconds:       1,
		StreamTimeoutSeconds: 3600,
		ModelsTTLSeconds:     60,
		MaxConcurrent:        1024,
		AccountMaxConcurrent: 10,
		AutoSwitchProduct:    true,
		AutoSwitchProducts:   []string{"cli"},
		IdentityVersions:     Versions{DesktopIntl: "9.9.9"},
		PrivateData:          "false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}

	body = `{"workbuddy":{"intl":{"timeout_seconds":-5,"stream_timeout_seconds":-2,"models_ttl_seconds":1000000000,"max_concurrent":0,"account_max_concurrent":-8}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got = LoadConfig(config.New(path), "intl")
	if got.TimeoutSeconds != 1 || got.StreamTimeoutSeconds != 1 ||
		got.ModelsTTLSeconds != 86400 || got.MaxConcurrent != 1 ||
		got.AccountMaxConcurrent != 0 {
		t.Fatalf("clamped config = %#v", got)
	}
}

func TestConfigLoadDefaultsInvalidValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	body := `{"workbuddy":{"intl":{"enabled":"yes","timeout_seconds":"not-a-number","stream_timeout_seconds":null,"models_ttl_seconds":{},"max_concurrent":"many","account_max_concurrent":true,"auto_switch_product":"true","auto_switch_products":"cli","identity_versions":{"desktop_intl":"9 9"},"private_data":"maybe"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadConfig(config.New(path), "intl")
	want := Config{
		Enabled:              true,
		Realm:                "intl",
		TimeoutSeconds:       120,
		StreamTimeoutSeconds: 300,
		ModelsTTLSeconds:     300,
		MaxConcurrent:        32,
		AccountMaxConcurrent: 0,
		AutoSwitchProduct:    false,
		AutoSwitchProducts:   []string{"workbuddy", "vscode", "cli"},
		IdentityVersions:     Versions{},
		PrivateData:          "auto",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
}

func TestConfigTimeoutRejectsBooleansAndNonNumericValues(t *testing.T) {
	values := []any{
		true,
		false,
		"120",
		"45.5",
		"",
		"not-a-number",
		map[string]any{"seconds": 30},
		[]any{30},
		nil,
	}
	for i, value := range values {
		t.Run(fmt.Sprintf("%d-%T", i, value), func(t *testing.T) {
			store := config.New(filepath.Join(t.TempDir(), "settings.json"))
			got, err := SaveConfig(store, "intl", map[string]any{
				"timeout_seconds":        value,
				"stream_timeout_seconds": value,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.TimeoutSeconds != defaultTimeoutSeconds {
				t.Fatalf("timeout_seconds from %#v = %v, want %v",
					value, got.TimeoutSeconds, defaultTimeoutSeconds)
			}
			if got.StreamTimeoutSeconds != defaultStreamTimeoutSeconds {
				t.Fatalf("stream_timeout_seconds from %#v = %v, want %v",
					value, got.StreamTimeoutSeconds, defaultStreamTimeoutSeconds)
			}
		})
	}
}

func TestConfigTimeoutAcceptsFiniteNumbersAndClamps(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  float64
	}{
		{"float", 45.5, 45.5},
		{"json number float", json.Number("90.25"), 90.25},
		{"int64", int64(30), 30},
		{"low clamp", 0.5, 1},
		{"high clamp", 999999.5, 600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := config.New(filepath.Join(t.TempDir(), "settings.json"))
			got, err := SaveConfig(store, "intl", map[string]any{
				"timeout_seconds": tc.value,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.TimeoutSeconds != tc.want {
				t.Fatalf("TimeoutSeconds = %v, want %v", got.TimeoutSeconds, tc.want)
			}
			if got.StreamTimeoutSeconds != defaultStreamTimeoutSeconds {
				t.Fatalf("StreamTimeoutSeconds = %v, want default %v",
					got.StreamTimeoutSeconds, defaultStreamTimeoutSeconds)
			}
		})
	}
}

func TestConfigPrivateDataBooleanSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	body := `{"workbuddy":{"intl":{"private_data":false},"cn":{"private_data":true}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.New(path)
	if got := LoadConfig(store, "intl").PrivateData; got != "auto" {
		t.Fatalf("private_data:false = %q, want auto (v1 false is falsy)", got)
	}
	if got := LoadConfig(store, "cn").PrivateData; got != "true" {
		t.Fatalf("private_data:true = %q, want true", got)
	}
	if got, err := SaveConfig(store, "intl", map[string]any{"private_data": false}); err != nil {
		t.Fatal(err)
	} else if got.PrivateData != "auto" {
		t.Fatalf("saved private_data:false = %q, want auto", got.PrivateData)
	}
}

func TestConfigErrorsDoNotLeakStoredSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	secret := "sk-test-SECRET-sentinel"
	body := `{"workbuddy":{"intl":{"future_secret":"` + secret + `"}},"future_top":"` + secret + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.New(path)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	_, err := SaveConfig(store, "intl", map[string]any{"enabled": true})
	if err == nil {
		t.Fatal("SaveConfig succeeded with an unusable store")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("config error leaked a stored credential: %v", err)
	}
}

func TestConfigLegacyV1Fallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	body := `{
		"account_max_concurrent":3,
		"auto_switch_product":true,
		"auto_switch_products":["vscode"],
		"identity_versions":{"desktop_cn":"9.9.9","cli":"2.0.0"},
		"identity":{"private_data":"false"},
		"upstream":{"header_timeout_seconds":45,"idle_timeout_seconds":90}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadConfig(config.New(path), "cn")
	if got.TimeoutSeconds != 45 || got.StreamTimeoutSeconds != 90 {
		t.Fatalf("legacy upstream timeouts = %v/%v", got.TimeoutSeconds, got.StreamTimeoutSeconds)
	}
	if got.AccountMaxConcurrent != 3 || !got.AutoSwitchProduct {
		t.Fatalf("legacy account settings = %#v", got)
	}
	if !reflect.DeepEqual(got.AutoSwitchProducts, []string{"vscode"}) {
		t.Fatalf("AutoSwitchProducts = %#v", got.AutoSwitchProducts)
	}
	if got.PrivateData != "false" {
		t.Fatalf("PrivateData = %q", got.PrivateData)
	}
	versions := got.Versions()
	if versions.DesktopCN != "9.9.9" || versions.CLI != "2.0.0" {
		t.Fatalf("Versions() = %#v", versions)
	}
}

func TestConfigNestedRealmOverridesLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	body := `{
		"identity_versions":{"desktop_cn":"8.8.8","desktop_intl":"8.8.8"},
		"account_max_concurrent":7,
		"workbuddy":{"cn":{"identity_versions":{"desktop_cn":"9.9.9"},"account_max_concurrent":2}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.New(path)

	cn := LoadConfig(store, "cn")
	if cn.IdentityVersions.DesktopCN != "9.9.9" || cn.AccountMaxConcurrent != 2 {
		t.Fatalf("cn config = %#v", cn)
	}
	// The international realm has no nested object, so the legacy values win.
	intl := LoadConfig(store, "intl")
	if intl.IdentityVersions.DesktopIntl != "8.8.8" || intl.AccountMaxConcurrent != 7 {
		t.Fatalf("intl config = %#v", intl)
	}
}

func TestConfigSavePreservesUnknownKeysAndOtherRealm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{
		"unrelated":{"keep":[1,2]},
		"workbuddy":{
			"intl":{"future_intl_flag":{"keep":1},"future_scalar":"keep me"},
			"cn":{"enabled":false,"future_cn_flag":"keep cn"}
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.New(path)

	got, err := SaveConfig(store, "intl", map[string]any{
		"enabled":                true,
		"timeout_seconds":        2,
		"stream_timeout_seconds": 999999,
		"models_ttl_seconds":     1,
		"max_concurrent":         9999,
		"account_max_concurrent": 99,
		"auto_switch_product":    true,
		"auto_switch_products":   []any{"cli", "vscode", "cli"},
		"identity_versions":      map[string]any{"desktop_intl": "7.7.7"},
		"private_data":           "false",
		"unknown_patch_key":      "must not be stored",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Enabled:              true,
		Realm:                "intl",
		TimeoutSeconds:       2,
		StreamTimeoutSeconds: 3600,
		ModelsTTLSeconds:     60,
		MaxConcurrent:        1024,
		AccountMaxConcurrent: 10,
		AutoSwitchProduct:    true,
		AutoSwitchProducts:   []string{"cli", "vscode"},
		IdentityVersions:     Versions{DesktopIntl: "7.7.7"},
		PrivateData:          "false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveConfig() = %#v, want %#v", got, want)
	}

	got, err = SaveConfig(store, "intl", map[string]any{"max_concurrent": 4})
	if err != nil {
		t.Fatal(err)
	}
	want.MaxConcurrent = 4
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("second SaveConfig() = %#v, want %#v", got, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&settings); err != nil {
		t.Fatal(err)
	}
	section, ok := settings["workbuddy"].(map[string]any)
	if !ok {
		t.Fatalf("workbuddy = %#v, want object", settings["workbuddy"])
	}
	intl, ok := section["intl"].(map[string]any)
	if !ok {
		t.Fatalf("workbuddy.intl = %#v, want object", section["intl"])
	}
	if intl["future_scalar"] != "keep me" {
		t.Fatalf("future_scalar = %#v, want keep me", intl["future_scalar"])
	}
	future, ok := intl["future_intl_flag"].(map[string]any)
	if !ok || future["keep"] != json.Number("1") {
		t.Fatalf("future_intl_flag = %#v", intl["future_intl_flag"])
	}
	if intl["unknown_patch_key"] != nil {
		t.Fatalf("unknown patch key was persisted: %#v", intl["unknown_patch_key"])
	}
	cn, ok := section["cn"].(map[string]any)
	if !ok || cn["future_cn_flag"] != "keep cn" || cn["enabled"] != false {
		t.Fatalf("cn config was clobbered: %#v", section["cn"])
	}
	unrelated, ok := settings["unrelated"].(map[string]any)
	if !ok {
		t.Fatalf("unrelated = %#v", settings["unrelated"])
	}
	keep, ok := unrelated["keep"].([]any)
	if !ok || len(keep) != 2 || keep[0] != json.Number("1") || keep[1] != json.Number("2") {
		t.Fatalf("unrelated.keep = %#v", unrelated["keep"])
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestConfigSaveCanClearIdentityVersionsAndProducts(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	if _, err := SaveConfig(store, "cn", map[string]any{
		"identity_versions":    map[string]any{"desktop_cn": "9.9.9", "cli": "2.0.0"},
		"auto_switch_products": []string{"cli"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := SaveConfig(store, "cn", map[string]any{
		"identity_versions":    map[string]any{},
		"auto_switch_products": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.IdentityVersions, Versions{}) {
		t.Fatalf("IdentityVersions = %#v, want cleared", got.IdentityVersions)
	}
	if len(got.AutoSwitchProducts) != 0 {
		t.Fatalf("AutoSwitchProducts = %#v, want empty", got.AutoSwitchProducts)
	}
}

func TestConfigSaveRejectsNilStoreAndInvalidRealm(t *testing.T) {
	if _, err := SaveConfig(nil, "intl", nil); err == nil {
		t.Fatal("SaveConfig(nil) succeeded")
	}
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	if _, err := SaveConfig(store, "eu", nil); err == nil {
		t.Fatal("SaveConfig accepted an invalid realm")
	}
}

func TestSaveConfigConcurrentDifferentKeysDoNotClobber(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))

	var calls atomic.Int32
	firstRead := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondRead := make(chan struct{}, 1)
	saveConfigReadHook = func() {
		if calls.Add(1) == 1 {
			close(firstRead)
			<-releaseFirst
			return
		}
		select {
		case secondRead <- struct{}{}:
		default:
		}
	}
	defer func() { saveConfigReadHook = nil }()

	errs := make(chan error, 2)
	go func() {
		_, err := SaveConfig(store, "intl", map[string]any{"timeout_seconds": 45})
		errs <- err
	}()
	<-firstRead
	go func() {
		_, err := SaveConfig(store, "intl", map[string]any{"stream_timeout_seconds": 120})
		errs <- err
	}()

	select {
	case <-secondRead:
		close(releaseFirst)
		t.Fatal("SaveConfig entered its read-modify-write cycle concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
	}
	got := LoadConfig(store, "intl")
	if got.TimeoutSeconds != 45 || got.StreamTimeoutSeconds != 120 {
		t.Fatalf("concurrent saves lost a key: %#v", got)
	}
}

func TestConfigSaveReturnsStoreErrors(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveConfig(store, "intl", map[string]any{"enabled": true}); err == nil {
		t.Fatal("SaveConfig succeeded with an unusable store")
	}
}

func TestConfigRealmFallbackAndRestrictedRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{
		"realm_fallback_enabled": true,
		"restricted_403_rotate_max": 3,
		"workbuddy": {
			"intl": {"auto_switch_product": true},
			"cn": {"realm_fallback_enabled": false, "restricted_403_rotate_max": 999}
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.New(path)
	intl := LoadConfig(store, realmIntl)
	if !intl.RealmFallbackEnabled || intl.Restricted403RotateMax != 3 {
		t.Fatalf("intl fallback/rotation = %v/%d", intl.RealmFallbackEnabled, intl.Restricted403RotateMax)
	}
	if !intl.AutoSwitchProduct {
		t.Fatal("auto_switch_product should remain a separate setting")
	}
	cn := LoadConfig(store, realmCN)
	if cn.RealmFallbackEnabled {
		t.Fatal("nested realm override did not disable fallback")
	}
	if cn.Restricted403RotateMax != 100 {
		t.Fatalf("cn rotation clamp = %d, want 100", cn.Restricted403RotateMax)
	}

	// SaveConfig toggles only the requested realm and key.
	if _, err := SaveConfig(store, realmCN, map[string]any{
		"realm_fallback_enabled":    true,
		"restricted_403_rotate_max": 2,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	cn = LoadConfig(store, realmCN)
	if !cn.RealmFallbackEnabled || cn.Restricted403RotateMax != 2 {
		t.Fatalf("saved cn fallback/rotation = %v/%d", cn.RealmFallbackEnabled, cn.Restricted403RotateMax)
	}
	intlAgain := LoadConfig(store, realmIntl)
	if !intlAgain.RealmFallbackEnabled || intlAgain.Restricted403RotateMax != 3 {
		t.Fatalf("saving cn changed intl: %+v", intlAgain)
	}

	if _, err := SaveConfig(store, realmIntl, map[string]any{"realm_fallback_enabled": false}); err != nil {
		t.Fatalf("SaveConfig disable: %v", err)
	}
	if LoadConfig(store, realmIntl).RealmFallbackEnabled {
		t.Fatal("fallback disable did not round-trip")
	}
}
