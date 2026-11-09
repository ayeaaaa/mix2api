// Package workbuddy implements the WorkBuddy international and China
// providers. One Provider instance serves exactly one realm.
package workbuddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

// WorkBuddy settings live under "workbuddy" as a per-realm object:
//
//	{"workbuddy":{"intl":{...},"cn":{...}}}
//
// v1 stored identity_versions, identity.private_data, auto_switch_product(s),
// account_max_concurrent, and the upstream header/idle timeouts as top-level
// keys. Those keys are still read as fallbacks when the nested realm object
// does not define the corresponding key. SaveConfig only writes the nested
// realm object, so saving the intl realm can never clobber the cn realm.
//
// Unknown top-level keys, unknown "workbuddy" keys, and unknown per-realm keys
// are preserved by config.Store.Merge.
const (
	realmIntl = "intl"
	realmCN   = "cn"

	defaultTimeoutSeconds       = 120.0
	minTimeoutSeconds           = 1.0
	maxTimeoutSeconds           = 600.0
	defaultStreamTimeoutSeconds = 300.0
	minStreamTimeoutSeconds     = 1.0
	maxStreamTimeoutSeconds     = 3600.0
	defaultModelsTTLSeconds     = 300.0
	minModelsTTLSeconds         = 60.0
	maxModelsTTLSeconds         = 86400.0

	// v1's gateway defaults to 32 in-flight chat slots (WB_MAX_CONCURRENT_CHAT).
	defaultMaxConcurrent = 32
	minMaxConcurrent     = 1
	maxMaxConcurrent     = 1024

	// v1's account cap is 0 (unlimited) by default and caps at 10.
	defaultAccountMaxConcurrent = 0
	minAccountMaxConcurrent     = 0
	maxAccountMaxConcurrent     = 10
)

// Config is the WorkBuddy provider settings for one realm. IdentityVersions
// holds only panel overrides; Config.Versions resolves them against the
// verified v1 defaults.
type Config struct {
	Enabled              bool    `json:"enabled"`
	Realm                string  `json:"realm"`
	TimeoutSeconds       float64 `json:"timeout_seconds"`
	StreamTimeoutSeconds float64 `json:"stream_timeout_seconds"`
	ModelsTTLSeconds     float64 `json:"models_ttl_seconds"`
	MaxConcurrent        int     `json:"max_concurrent"`
	AccountMaxConcurrent int     `json:"account_max_concurrent"`
	AutoSwitchProduct    bool    `json:"auto_switch_product"`

	AutoSwitchProducts []string `json:"auto_switch_products"`
	IdentityVersions   Versions `json:"identity_versions"`
	PrivateData        string   `json:"private_data"`
}

// Versions returns the effective identity version set: stored overrides first,
// verified defaults for every field the panel left empty.
func (c Config) Versions() Versions {
	return c.IdentityVersions.withDefaults()
}

var defaultAutoSwitchProducts = []string{"workbuddy", "vscode", "cli"}

// saveConfigMu serializes SaveConfig's load-modify-merge cycle. Store.Merge is
// atomic on its own, but SaveConfig also reads the normalized config first;
// without this lock two saves of different keys could both read the old value
// and the later Merge would clobber the earlier key with stale values.
var saveConfigMu sync.Mutex

// saveConfigReadHook is a test seam invoked after LoadConfig and before the
// patch is applied, while saveConfigMu is held.
var saveConfigReadHook func()

func defaultConfig(realm string) Config {
	return Config{
		Enabled:              true,
		Realm:                realm,
		TimeoutSeconds:       defaultTimeoutSeconds,
		StreamTimeoutSeconds: defaultStreamTimeoutSeconds,
		ModelsTTLSeconds:     defaultModelsTTLSeconds,
		MaxConcurrent:        defaultMaxConcurrent,
		AccountMaxConcurrent: defaultAccountMaxConcurrent,
		AutoSwitchProduct:    false,
		AutoSwitchProducts:   append([]string(nil), defaultAutoSwitchProducts...),
		IdentityVersions:     Versions{},
		PrivateData:          "auto",
	}
}

func normalizeRealm(realm string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case realmIntl:
		return realmIntl, true
	case realmCN:
		return realmCN, true
	default:
		return "", false
	}
}

func realmOrDefault(realm string) string {
	if normalized, ok := normalizeRealm(realm); ok {
		return normalized
	}
	return realmIntl
}

// LoadConfig reads and clamps the WorkBuddy settings for realm. Missing
// settings use v1 defaults; malformed or non-finite numeric values fall back
// to their defaults while missing sections or unreadable files yield the full
// defaults.
func LoadConfig(store *config.Store, realm string) Config {
	realm = realmOrDefault(realm)
	if store == nil {
		return defaultConfig(realm)
	}
	settings, err := store.Load()
	if err != nil {
		return defaultConfig(realm)
	}
	return normalizeConfig(rawRealmSettings(settings, realm), realm)
}

// rawRealmSettings returns the nested per-realm object, with v1 top-level keys
// filled in only where the nested object does not define the same key.
func rawRealmSettings(settings map[string]any, realm string) map[string]any {
	raw := map[string]any{}
	if section, ok := settings["workbuddy"].(map[string]any); ok {
		if nested, ok := section[realm].(map[string]any); ok {
			for key, value := range nested {
				raw[key] = value
			}
		}
	}

	inherit := func(key string, value any, exists bool) {
		if !exists {
			return
		}
		if _, set := raw[key]; !set {
			raw[key] = value
		}
	}

	value, exists := settings["account_max_concurrent"]
	inherit("account_max_concurrent", value, exists)
	value, exists = settings["auto_switch_product"]
	inherit("auto_switch_product", value, exists)
	value, exists = settings["auto_switch_products"]
	inherit("auto_switch_products", value, exists)
	value, exists = settings["identity_versions"]
	inherit("identity_versions", value, exists)

	if identity, ok := settings["identity"].(map[string]any); ok {
		value, exists = identity["private_data"]
		inherit("private_data", value, exists)
	}
	if upstream, ok := settings["upstream"].(map[string]any); ok {
		value, exists = upstream["header_timeout_seconds"]
		inherit("timeout_seconds", value, exists)
		value, exists = upstream["idle_timeout_seconds"]
		inherit("stream_timeout_seconds", value, exists)
	}
	return raw
}

// SaveConfig merges the recognized keys in patch under
// "workbuddy".<realm>, clamps the resulting values, and returns the stored
// configuration. Unknown keys in the settings file and its workbuddy sections
// are preserved.
func SaveConfig(store *config.Store, realm string, patch map[string]any) (Config, error) {
	normalized, ok := normalizeRealm(realm)
	if !ok {
		return Config{}, fmt.Errorf("workbuddy: invalid realm %q", realm)
	}
	if store == nil {
		return Config{}, errors.New("workbuddy: config store is nil")
	}

	saveConfigMu.Lock()
	defer saveConfigMu.Unlock()

	current := LoadConfig(store, normalized)
	if saveConfigReadHook != nil {
		saveConfigReadHook()
	}
	raw := configValues(current)
	for key, value := range patch {
		switch key {
		case "enabled", "timeout_seconds", "stream_timeout_seconds",
			"models_ttl_seconds", "max_concurrent", "account_max_concurrent",
			"auto_switch_product", "auto_switch_products",
			"identity_versions", "private_data":
			raw[key] = value
		}
	}
	merged := normalizeConfig(raw, normalized)

	if _, err := store.Merge(map[string]any{
		"workbuddy": map[string]any{normalized: configValues(merged)},
	}); err != nil {
		return Config{}, fmt.Errorf("workbuddy: save config: %w", err)
	}
	return LoadConfig(store, normalized), nil
}

func configValues(cfg Config) map[string]any {
	return map[string]any{
		"enabled":                cfg.Enabled,
		"realm":                  cfg.Realm,
		"timeout_seconds":        cfg.TimeoutSeconds,
		"stream_timeout_seconds": cfg.StreamTimeoutSeconds,
		"models_ttl_seconds":     cfg.ModelsTTLSeconds,
		"max_concurrent":         cfg.MaxConcurrent,
		"account_max_concurrent": cfg.AccountMaxConcurrent,
		"auto_switch_product":    cfg.AutoSwitchProduct,
		"auto_switch_products":   cfg.AutoSwitchProducts,
		"identity_versions":      versionsValues(cfg.IdentityVersions),
		"private_data":           cfg.PrivateData,
	}
}

func normalizeConfig(raw map[string]any, realm string) Config {
	cfg := defaultConfig(realm)
	if raw == nil {
		return cfg
	}
	cfg.Enabled = boolOrDefault(raw["enabled"], true)
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
	cfg.AccountMaxConcurrent = accountMaxConcurrentValue(raw["account_max_concurrent"])
	cfg.AutoSwitchProduct = isTrue(raw["auto_switch_product"])
	cfg.AutoSwitchProducts = autoSwitchProductsValue(raw["auto_switch_products"])
	if value, exists := raw["identity_versions"]; exists {
		cfg.IdentityVersions = versionsFromAny(value)
	}
	cfg.PrivateData = privateDataValue(raw["private_data"])
	return cfg
}

func boolOrDefault(value any, def bool) bool {
	if boolean, ok := value.(bool); ok {
		return boolean
	}
	return def
}

func isTrue(value any) bool {
	enabled, ok := value.(bool)
	return ok && enabled
}

func privateDataValue(value any) string {
	if boolean, ok := value.(bool); ok {
		if boolean {
			return "true"
		}
		return "false"
	}
	text := strings.ToLower(strings.TrimSpace(stringValue(value)))
	switch text {
	case "true", "false":
		return text
	default:
		return "auto"
	}
}

func autoSwitchProductsValue(value any) []string {
	out := make([]string, 0, len(defaultAutoSwitchProducts))
	seen := make(map[string]bool)
	add := func(text string) {
		if text != "workbuddy" && text != "vscode" && text != "cli" {
			return
		}
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			add(strings.TrimSpace(stringValue(item)))
		}
	case []string:
		for _, item := range values {
			add(strings.TrimSpace(item))
		}
	case string:
		// v1 treats a non-list value as "use the default list".
		for _, item := range defaultAutoSwitchProducts {
			add(item)
		}
	case nil:
		for _, item := range defaultAutoSwitchProducts {
			add(item)
		}
	default:
		for _, item := range defaultAutoSwitchProducts {
			add(item)
		}
	}
	return out
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int:
		return strconv.Itoa(v)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	default:
		return ""
	}
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

// accountMaxConcurrentValue follows v1's safe defaults: a boolean or
// non-number reads as 0 (unlimited), negative values clamp to 0, and oversized
// values clamp to 10 so a hand-edited value can never silently remove the cap.
func accountMaxConcurrentValue(value any) int {
	if _, isBoolean := value.(bool); isBoolean {
		return defaultAccountMaxConcurrent
	}
	number, ok := numericValue(value)
	if !ok {
		return defaultAccountMaxConcurrent
	}
	if number < float64(minAccountMaxConcurrent) {
		return minAccountMaxConcurrent
	}
	if number > float64(maxAccountMaxConcurrent) {
		return maxAccountMaxConcurrent
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
	case bool:
		// v1 used Python float()/int(), which accept booleans.
		if v {
			number = 1
		}
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}
