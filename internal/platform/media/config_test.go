package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMediaConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"media":{"enabled":"yes","realm":"bogus","identity":"robot","image_max_per_minute":0,"video_max_seconds":999,"daily_credits_per_key":-1,"video_concurrency":100}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Load(dir)
	want := DefaultConfig()
	if cfg != want {
		t.Fatalf("malformed config = %#v, want %#v", cfg, want)
	}

	stored := ConfigFromSettings(map[string]any{"media": map[string]any{
		"enabled": true, "realm": "intl", "identity": "cli",
		"image_max_per_minute": 12, "video_max_seconds": 30,
		"daily_credits_per_key": 500, "video_concurrency": 4,
	}}, nil)
	if !stored.Enabled || stored.Realm != "intl" || stored.Identity != "cli" || stored.ImageMaxPerMinute != 12 ||
		stored.VideoMaxSeconds != 30 || stored.DailyCreditsPerKey != 500 || stored.VideoConcurrency != 4 {
		t.Fatalf("stored config = %#v", stored)
	}

	if _, err := ValidatePatch(map[string]any{"unknown": true}); err == nil {
		t.Fatal("unknown key accepted")
	}
	for _, patch := range []map[string]any{
		{"enabled": "yes"},
		{"realm": "eu"},
		{"identity": "robot"},
		{"image_max_per_minute": 0},
		{"image_max_per_minute": 61},
		{"video_max_seconds": 0},
		{"video_concurrency": 9},
		{"daily_credits_per_key": 100001},
		{"video_concurrency": 1.5},
	} {
		if _, err := ValidatePatch(patch); err == nil {
			t.Fatalf("patch %#v accepted", patch)
		}
	}
	clean, err := ValidatePatch(map[string]any{"enabled": true, "realm": "intl", "video_concurrency": 8})
	if err != nil || clean["video_concurrency"] != 8 {
		t.Fatalf("valid patch = %#v err=%v", clean, err)
	}

	// The environment may force only the master switch.
	t.Setenv(MediaEnabledEnv, "1")
	envCfg := Load(dir)
	if !envCfg.Enabled {
		t.Fatal("WB_MEDIA_ENABLED did not force enabled")
	}
	if envCfg.ImageMaxPerMinute != DefaultImageMaxPerMinute {
		t.Fatalf("env changed a limit: %#v", envCfg)
	}
}

func TestMediaSaveAndRealmMismatch(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Save(dir, map[string]any{"enabled": true, "realm": "cn", "identity": "desktop"})
	if err != nil || !cfg.Enabled || cfg.Realm != "cn" {
		t.Fatalf("Save = %#v err=%v", cfg, err)
	}
	service := &Service{ConfigFunc: func() Config { return cfg }}
	if realm, err := service.Realm("cn"); err != nil || realm != "cn" {
		t.Fatalf("matching realm = %q err=%v", realm, err)
	}
	if _, err := service.Realm("intl"); err == nil {
		t.Fatal("mismatched key realm accepted")
	} else if got := err.Error(); got != "本 Key 绑定在 intl 出口，而媒体能力配置在 cn 出口；请换对应出口的 Key，或在设置页把媒体出口改成 intl" {
		t.Fatalf("mismatch message = %q", got)
	}
	_ = context.Background()
}
