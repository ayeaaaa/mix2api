// Package media owns the provider-neutral image/video surface: settings,
// pricing evidence, asynchronous video tasks, and signed artifact downloads.
package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

// Config is the v1-compatible media settings section.
type Config struct {
	Enabled            bool   `json:"enabled"`
	Realm              string `json:"realm"`
	Identity           string `json:"identity"`
	ImageMaxPerMinute  int    `json:"image_max_per_minute"`
	VideoMaxSeconds    int    `json:"video_max_seconds"`
	DailyCreditsPerKey int    `json:"daily_credits_per_key"`
	VideoConcurrency   int    `json:"video_concurrency"`
}

// The v1 defaults and bounds are part of the wire contract; the panel shows
// them and the settings validator enforces the same numbers.
const (
	DefaultImageMaxPerMinute = 6
	DefaultVideoMaxSeconds   = 15
	DefaultVideoConcurrency  = 3
	MaxImageMaxPerMinute     = 60
	MaxVideoMaxSeconds       = 60
	MaxDailyCreditsPerKey    = 100000
	MaxVideoConcurrency      = 8
	MinImageMaxPerMinute     = 1
	MinVideoMaxSeconds       = 1
	MinDailyCreditsPerKey    = 0
	MinVideoConcurrency      = 1
	DefaultRealm             = "cn"
	DefaultIdentity          = "desktop"
	MediaEnabledEnv          = "WB_MEDIA_ENABLED"
)

// DefaultConfig returns the exact v1 defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:            false,
		Realm:              DefaultRealm,
		Identity:           DefaultIdentity,
		ImageMaxPerMinute:  DefaultImageMaxPerMinute,
		VideoMaxSeconds:    DefaultVideoMaxSeconds,
		DailyCreditsPerKey: 0,
		VideoConcurrency:   DefaultVideoConcurrency,
	}
}

// Limit returns the v1 upper bound for a numeric key.
func Limit(key string) int {
	switch key {
	case "image_max_per_minute":
		return MaxImageMaxPerMinute
	case "video_max_seconds":
		return MaxVideoMaxSeconds
	case "daily_credits_per_key":
		return MaxDailyCreditsPerKey
	case "video_concurrency":
		return MaxVideoConcurrency
	default:
		return 0
	}
}

// Minimum returns the v1 lower bound for a numeric key.
func Minimum(key string) int {
	if key == "daily_credits_per_key" {
		return 0
	}
	return 1
}

// ConfigFromSettings projects a stored settings document. Malformed stored
// values fall back to the default rather than failing the whole config.
func ConfigFromSettings(data map[string]any, environ func(string) string) Config {
	cfg := DefaultConfig()
	stored, _ := data["media"].(map[string]any)
	if stored != nil {
		if value, ok := stored["enabled"].(bool); ok {
			cfg.Enabled = value
		}
		if value, ok := stored["realm"].(string); ok && (value == "cn" || value == "intl") {
			cfg.Realm = value
		}
		if value, ok := stored["identity"].(string); ok && (value == "desktop" || value == "cli") {
			cfg.Identity = value
		}
		for key, target := range map[string]*int{
			"image_max_per_minute":  &cfg.ImageMaxPerMinute,
			"video_max_seconds":     &cfg.VideoMaxSeconds,
			"daily_credits_per_key": &cfg.DailyCreditsPerKey,
			"video_concurrency":     &cfg.VideoConcurrency,
		} {
			value, present := stored[key]
			if !present {
				continue
			}
			number, ok := wholeNumber(value)
			if ok && number >= Minimum(key) && number <= Limit(key) {
				*target = number
			}
		}
	}
	if environ == nil {
		environ = os.Getenv
	}
	if strings.TrimSpace(environ(MediaEnabledEnv)) == "1" {
		// Only the master switch is environment-controlled; limits stay in
		// the operator-visible settings file.
		cfg.Enabled = true
	}
	return cfg
}

// Load reads the media section from accounts/settings.json.
func Load(accountsDir string) Config {
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	return LoadFromStore(store)
}

// LoadFromStore reads the media section through an existing settings store.
func LoadFromStore(store *config.Store) Config {
	if store == nil {
		return ConfigFromSettings(nil, nil)
	}
	data, err := store.Load()
	if err != nil {
		data = map[string]any{}
	}
	return ConfigFromSettings(data, nil)
}

// ValidatePatch applies strict v1 validation to a panel-submitted media patch.
// Unknown keys, wrong types, and out-of-range numbers are errors.
func ValidatePatch(patch map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(patch))
	for key, value := range patch {
		switch key {
		case "enabled":
			boolean, ok := value.(bool)
			if !ok {
				return nil, errors.New("enabled must be true or false")
			}
			out[key] = boolean
		case "realm":
			text, ok := value.(string)
			if !ok || (text != "cn" && text != "intl") {
				return nil, errors.New("realm must be one of cn, intl")
			}
			out[key] = text
		case "identity":
			text, ok := value.(string)
			if !ok || (text != "desktop" && text != "cli") {
				return nil, errors.New("identity must be one of desktop, cli")
			}
			out[key] = text
		case "image_max_per_minute", "video_max_seconds", "daily_credits_per_key", "video_concurrency":
			number, ok := wholeNumber(value)
			if !ok {
				return nil, fmt.Errorf("%s must be a whole number", key)
			}
			if number < Minimum(key) {
				return nil, fmt.Errorf("%s cannot be less than %d", key, Minimum(key))
			}
			if number > Limit(key) {
				return nil, fmt.Errorf("%s cannot be more than %d", key, Limit(key))
			}
			out[key] = number
		default:
			return nil, fmt.Errorf("unknown media setting %q", key)
		}
	}
	return out, nil
}

// Save validates and persists a media patch, returning the merged config.
func Save(accountsDir string, patch map[string]any) (Config, error) {
	clean, err := ValidatePatch(patch)
	if err != nil {
		return Config{}, err
	}
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	return SaveStore(store, clean)
}

// SaveStore validates and persists a media patch through an existing store.
func SaveStore(store *config.Store, patch map[string]any) (Config, error) {
	clean, err := ValidatePatch(patch)
	if err != nil {
		return Config{}, err
	}
	if store == nil {
		return Config{}, errors.New("media: settings store is required")
	}
	if _, err := store.Merge(map[string]any{"media": clean}); err != nil {
		return Config{}, err
	}
	return LoadFromStore(store), nil
}

func wholeNumber(value any) (int, bool) {
	switch typed := value.(type) {
	case bool:
		return 0, false
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case uint:
		return int(typed), true
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		return int(typed), true
	case uint64:
		if typed > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case float32:
		if typed != float32(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		number, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		return int(number), true
	default:
		return 0, false
	}
}
