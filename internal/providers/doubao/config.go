package doubao

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

const (
	defaultBaseURL              = "https://www.doubao.com"
	defaultAID                  = "1044603"
	defaultTimeoutSeconds       = 60.0
	defaultStreamTimeoutSeconds = 600.0
	defaultModelsTTLSeconds     = 300.0
	defaultMaxConcurrent        = 1
)

// Config is the persisted doubao settings section.
type Config struct {
	Enabled              bool    `json:"enabled"`
	BaseURL              string  `json:"base_url"`
	AID                  string  `json:"aid"`
	TimeoutSeconds       float64 `json:"timeout_seconds"`
	StreamTimeoutSeconds float64 `json:"stream_timeout_seconds"`
	ModelsTTLSeconds     float64 `json:"models_ttl_seconds"`
	MaxConcurrent        int     `json:"max_concurrent"`
}

func defaultConfig() Config {
	return Config{
		Enabled:              false,
		BaseURL:              defaultBaseURL,
		AID:                  defaultAID,
		TimeoutSeconds:       defaultTimeoutSeconds,
		StreamTimeoutSeconds: defaultStreamTimeoutSeconds,
		ModelsTTLSeconds:     defaultModelsTTLSeconds,
		MaxConcurrent:        defaultMaxConcurrent,
	}
}

// LoadConfig reads the doubao section. Missing or invalid values fall back to
// safe defaults; unknown fields remain untouched in the shared settings file.
func LoadConfig(store *config.Store) Config {
	if store == nil {
		return defaultConfig()
	}
	data, err := store.Load()
	if err != nil {
		return defaultConfig()
	}
	raw, _ := data["doubao"].(map[string]any)
	return normalizeConfig(raw)
}

// SaveConfig merges recognized values under the doubao key.
func SaveConfig(store *config.Store, patch map[string]any) (Config, error) {
	if store == nil {
		return Config{}, errors.New("doubao: config store is nil")
	}
	current := LoadConfig(store)
	raw := configValues(current)
	for key, value := range patch {
		switch key {
		case "enabled", "base_url", "aid", "timeout_seconds",
			"stream_timeout_seconds", "models_ttl_seconds", "max_concurrent":
			raw[key] = value
		}
	}
	merged := normalizeConfig(raw)
	if _, err := store.Merge(map[string]any{"doubao": configValues(merged)}); err != nil {
		return Config{}, fmt.Errorf("doubao: save config: %w", err)
	}
	return LoadConfig(store), nil
}

func configValues(cfg Config) map[string]any {
	return map[string]any{
		"enabled":                cfg.Enabled,
		"base_url":               cfg.BaseURL,
		"aid":                    cfg.AID,
		"timeout_seconds":        cfg.TimeoutSeconds,
		"stream_timeout_seconds": cfg.StreamTimeoutSeconds,
		"models_ttl_seconds":     cfg.ModelsTTLSeconds,
		"max_concurrent":         cfg.MaxConcurrent,
	}
}

func normalizeConfig(raw map[string]any) Config {
	cfg := defaultConfig()
	if raw == nil {
		return cfg
	}
	cfg.Enabled = boolValue(raw["enabled"])
	if value := textValue(raw["base_url"]); value != "" {
		cfg.BaseURL = strings.TrimRight(value, "/")
	}
	if value := textValue(raw["aid"]); value != "" {
		cfg.AID = value
	}
	cfg.TimeoutSeconds = boundedFloat(raw["timeout_seconds"], defaultTimeoutSeconds, 1, 600)
	cfg.StreamTimeoutSeconds = boundedFloat(raw["stream_timeout_seconds"], defaultStreamTimeoutSeconds, 1, 3600)
	cfg.ModelsTTLSeconds = boundedFloat(raw["models_ttl_seconds"], defaultModelsTTLSeconds, 1, 86400)
	cfg.MaxConcurrent = boundedInt(raw["max_concurrent"], defaultMaxConcurrent, 1, 32)
	return cfg
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func textValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func boundedFloat(value any, fallback, min, max float64) float64 {
	number, ok := numberValue(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < min || number > max {
		return fallback
	}
	return number
}

func boundedInt(value any, fallback, min, max int) int {
	number, ok := numberValue(value)
	if !ok {
		return fallback
	}
	n := int(number)
	if n < min || n > max {
		return fallback
	}
	return n
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case json.Number:
		value, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return value, true
	default:
		return 0, false
	}
}
