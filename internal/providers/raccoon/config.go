// Package raccoon implements the Raccoon provider's configuration and
// account-file model.
package raccoon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

// Defaults and bounds match v1's Raccoon settings.
const (
	defaultTimeoutSeconds       = 60.0
	minTimeoutSeconds           = 1.0
	maxTimeoutSeconds           = 600.0
	defaultStreamTimeoutSeconds = 900.0
	minStreamTimeoutSeconds     = 1.0
	maxStreamTimeoutSeconds     = 3600.0
	defaultModelsTTLSeconds     = 600.0
	minModelsTTLSeconds         = 60.0
	maxModelsTTLSeconds         = 86400.0
	defaultMaxConcurrent        = 1
	minMaxConcurrent            = 1
	maxMaxConcurrent            = 10
)

// Config is the Raccoon provider settings stored under the "raccoon" key of
// the shared settings file.
type Config struct {
	Enabled              bool    `json:"enabled"`
	TimeoutSeconds       float64 `json:"timeout_seconds"`
	StreamTimeoutSeconds float64 `json:"stream_timeout_seconds"`
	ModelsTTLSeconds     float64 `json:"models_ttl_seconds"`
	MaxConcurrent        int     `json:"max_concurrent"`
	AutoClaimLogin       bool    `json:"auto_claim_login"`
}

func defaultConfig() Config {
	return Config{
		Enabled:              false,
		TimeoutSeconds:       defaultTimeoutSeconds,
		StreamTimeoutSeconds: defaultStreamTimeoutSeconds,
		ModelsTTLSeconds:     defaultModelsTTLSeconds,
		MaxConcurrent:        defaultMaxConcurrent,
		AutoClaimLogin:       false,
	}
}

// LoadConfig reads and clamps the Raccoon settings. Missing settings use v1
// defaults; malformed or non-finite values fall back to their defaults while
// missing sections or unreadable files yield the full defaults.
func LoadConfig(store *config.Store) Config {
	if store == nil {
		return defaultConfig()
	}
	settings, err := store.Load()
	if err != nil {
		return defaultConfig()
	}
	raw, _ := settings["raccoon"].(map[string]any)
	return normalizeConfig(raw)
}

// SaveConfig merges the recognized keys in patch under the "raccoon" key,
// clamps the resulting values, and returns the stored configuration. Existing
// unknown keys in the settings file and its "raccoon" section are preserved.
func SaveConfig(store *config.Store, patch map[string]any) (Config, error) {
	if store == nil {
		return Config{}, errors.New("raccoon: config store is nil")
	}

	current := LoadConfig(store)
	raw := configValues(current)
	for key, value := range patch {
		switch key {
		case "enabled", "timeout_seconds", "stream_timeout_seconds",
			"models_ttl_seconds", "max_concurrent", "auto_claim_login":
			raw[key] = value
		}
	}
	merged := normalizeConfig(raw)

	if _, err := store.Merge(map[string]any{"raccoon": configValues(merged)}); err != nil {
		return Config{}, fmt.Errorf("raccoon: save config: %w", err)
	}
	return LoadConfig(store), nil
}

func configValues(cfg Config) map[string]any {
	return map[string]any{
		"enabled":                cfg.Enabled,
		"timeout_seconds":        cfg.TimeoutSeconds,
		"stream_timeout_seconds": cfg.StreamTimeoutSeconds,
		"models_ttl_seconds":     cfg.ModelsTTLSeconds,
		"max_concurrent":         cfg.MaxConcurrent,
		"auto_claim_login":       cfg.AutoClaimLogin,
	}
}

func normalizeConfig(raw map[string]any) Config {
	cfg := defaultConfig()
	if raw == nil {
		return cfg
	}
	cfg.Enabled = isTrue(raw["enabled"])
	cfg.TimeoutSeconds = boundedFloat(
		raw["timeout_seconds"], defaultTimeoutSeconds, minTimeoutSeconds, maxTimeoutSeconds,
	)
	cfg.StreamTimeoutSeconds = boundedFloat(
		raw["stream_timeout_seconds"], defaultStreamTimeoutSeconds,
		minStreamTimeoutSeconds, maxStreamTimeoutSeconds,
	)
	cfg.ModelsTTLSeconds = boundedFloat(
		raw["models_ttl_seconds"], defaultModelsTTLSeconds,
		minModelsTTLSeconds, maxModelsTTLSeconds,
	)
	cfg.MaxConcurrent = boundedInt(
		raw["max_concurrent"], defaultMaxConcurrent, minMaxConcurrent, maxMaxConcurrent,
	)
	cfg.AutoClaimLogin = isTrue(raw["auto_claim_login"])
	return cfg
}

func isTrue(value any) bool {
	enabled, ok := value.(bool)
	return ok && enabled
}

// boundedFloat mirrors v1 clamping: non-numeric, NaN, and infinite values use
// the default, while finite values outside the bounds are clamped.
func boundedFloat(value any, def, low, high float64) float64 {
	number, ok := numericValue(value)
	if !ok {
		return def
	}
	if number < low {
		return low
	}
	if number > high {
		return high
	}
	return number
}

// boundedInt mirrors v1's int() conversion: finite values are truncated toward
// zero and then clamped. Invalid or non-finite values use the default.
func boundedInt(value any, def, low, high int) int {
	number, ok := numericValue(value)
	if !ok {
		return def
	}
	if number < float64(low) {
		return low
	}
	if number > float64(high) {
		return high
	}
	return int(number)
}

func numericValue(value any) (float64, bool) {
	var number float64
	switch v := value.(type) {
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	case float64:
		number = v
	case float32:
		number = float64(v)
	case int:
		number = float64(v)
	case int8:
		number = float64(v)
	case int16:
		number = float64(v)
	case int32:
		number = float64(v)
	case int64:
		number = float64(v)
	case uint:
		number = float64(v)
	case uint8:
		number = float64(v)
	case uint16:
		number = float64(v)
	case uint32:
		number = float64(v)
	case uint64:
		number = float64(v)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}
