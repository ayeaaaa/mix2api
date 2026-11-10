// Package settings owns the panel settings view, merge/save semantics, and the
// active realm file. It merges provider-owned sections through the optional
// providers.SettingsContributor hook and never imports a concrete provider.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	defaultPricingRefreshMinutes = 5.0
	maxPricingRefreshMinutes     = 24 * 30 * 60
	defaultSectionMaxHeight      = 640
	maxSectionMaxHeight          = 4000
	defaultKeepBackups           = 1
	maxKeepBackups               = 3
	defaultCreditsRefreshHours   = 12.0
	defaultAccountMaxConcurrent  = 0
	maxAccountMaxConcurrent      = 10
	defaultLogMaxDays            = 7
	maxLogMaxRows                = 100000000
	maxLogMaxDays                = 36500
	maxLogMaxBytes               = 100 * 1024 * 1024 * 1024
)

var (
	limitKeys   = []string{"reserve_credits", "daily_token_limit", "daily_credit_limit", "model_daily_token_limit", "expiring_window_days"}
	limitRealms = []string{"intl", "cn"}
)

// Service merges platform-owned settings with provider contributors.
type Service struct {
	Store                  *config.Store
	Keys                   *keys.Store
	Providers              []providers.Provider
	PanelPasswordIsDefault func() bool
	SlotViews              func() []map[string]any
	AccountsDir            string
	UsageDir               string
	SettingsFile           string
	Project                string
	Version                string
	Now                    func() time.Time
}

// New returns a settings service.
func New(opts Service) *Service {
	if strings.TrimSpace(opts.Project) == "" {
		opts.Project = "mix2api"
	}
	return &opts
}

// View returns the full panel settings payload. Unknown stored keys are kept;
// provider-owned sections are merged over the platform-owned base.
func (s *Service) View(ctx context.Context) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("settings: store is required")
	}
	data, err := s.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("settings: load: %w", err)
	}
	data, _ = s.migrateLegacyLimits(data)
	view := s.baseView(data)

	for _, provider := range s.Providers {
		contributor, ok := provider.(providers.SettingsContributor)
		if !ok {
			continue
		}
		section, err := contributor.SettingsView(ctx)
		if err != nil || section == nil {
			// A broken optional provider must not take the settings page down.
			continue
		}
		view = mergeMaps(view, section)
	}
	if s.SlotViews != nil {
		view["proxySlots"] = s.SlotViews()
	} else {
		view["proxySlots"] = rawSlotViews(data)
	}
	return view, nil
}

func rawSlotViews(data map[string]any) []map[string]any {
	slots := transport.SlotsFromSettings(data)
	out := make([]map[string]any, 0, len(slots))
	stored, _ := data["proxy_slots"].([]any)
	byID := map[string]map[string]any{}
	for _, raw := range stored {
		entry, _ := raw.(map[string]any)
		if entry != nil {
			byID[textValue(entry["id"])] = entry
		}
	}
	for _, slot := range slots {
		entry := byID[slot.ID]
		label := slot.Name
		if label == "" {
			label = slot.ID
		}
		item := map[string]any{
			"id": slot.ID, "name": slot.Name, "url": slot.URL,
			"enabled": slot.Enabled, "label": label, "bound": 0,
		}
		if entry != nil {
			for _, key := range []string{"ip", "country", "country_code", "ip_type", "isp", "asn"} {
				if value, ok := entry[key]; ok {
					item[key] = value
				}
			}
			if value, ok := entry["probed_at"]; ok {
				item["probed_at"] = value
			}
		}
		out = append(out, item)
	}
	return out
}

func (s *Service) baseView(data map[string]any) map[string]any {
	live := []keys.Entry{}
	deleted := []keys.Entry{}
	if s.Keys != nil {
		live, _ = s.Keys.List(false)
		all, _ := s.Keys.List(true)
		deleted = all
	}
	liveSummaries := keys.MaskedSummaries(live)
	deletedSummaries := keys.DeletedSummaries(deleted)
	masked := ""
	if len(live) > 0 {
		masked = keys.Mask(live[0].Key)
	}
	authRequired := false
	if s.Keys != nil {
		authRequired = s.Keys.AuthRequired()
	}
	_, panelOwnsKeys := data["api_keys"].([]any)

	limits := limitsView(data)
	view := map[string]any{
		"api_keys":                    liveSummaries,
		"deleted_api_keys":            deletedSummaries,
		"api_key_masked":              masked,
		"api_key_set":                 len(live) > 0,
		"api_key_set_by_panel":        panelOwnsKeys,
		"auth_required":               authRequired,
		"panel_password_is_default":   s.panelPasswordIsDefault(),
		"limits":                      limits,
		"reserve_credits":             limitValue(limits, "reserve_credits"),
		"daily_token_limit":           limitValue(limits, "daily_token_limit"),
		"daily_credit_limit":          limitValue(limits, "daily_credit_limit"),
		"model_daily_token_limit":     limitValue(limits, "model_daily_token_limit"),
		"pricing_refresh_minutes":     pricingRefreshMinutes(data),
		"pricing_variant_inherit":     boolDefaultTrue(data["pricing_variant_inherit"]),
		"pricing_enabled":             boolDefaultTrue(data["pricing_enabled"]),
		"credits_refresh_hours":       creditsRefreshHours(data),
		"section_max_height":          sectionMaxHeight(data),
		"media":                       mediaView(data),
		"media_limits":                mediaLimits(),
		"media_minimums":              mediaMinimums(),
		"media_realms":                []string{"cn", "intl"},
		"media_identities":            []string{"desktop", "cli"},
		"log_db_enabled":              boolTrue(data["log_db_enabled"]),
		"log_db_max_rows":             logMaxRows(data),
		"log_db_max_days":             logMaxDays(data),
		"log_db_max_bytes":            logMaxBytes(data),
		"update_check_enabled":        boolTrue(data["update_check_enabled"]),
		"update_check_proxy_slot":     strings.TrimSpace(textValue(data["update_check_proxy_slot"])),
		"update_check_proxy_slots":    s.updateSlotOptions(),
		"keep_backups":                keepBackups(data),
		"keep_backups_max":            maxKeepBackups,
		"usage":                       usageView(data),
		"prompt":                      promptView(data),
		"local_web_tools":             boolTrue(data["local_web_tools"]),
		"window_probe":                boolDefaultTrue(data["window_probe"]),
		"quota_scan_models":           stringSlice(data["quota_scan_models"]),
		"quota_scan_interval_hours":   6,
		"quota_scans":                 []any{},
		"remaining_priority_enabled":  boolTrue(data["remaining_priority_enabled"]),
		"egress_backoff_enabled":      boolTrue(data["egress_backoff_enabled"]),
		"account_max_concurrent":      accountMaxConcurrent(data),
		"account_max_concurrent_max":  maxAccountMaxConcurrent,
		"account_inflight":            map[string]any{},
		"affinity_stable_key_enabled": boolTrue(data["affinity_stable_key_enabled"]),
		"accounts_collapsed":          boolTrue(data["accounts_collapsed"]),
		"accounts_separate_tab":       boolTrue(data["accounts_separate_tab"]),
		"key_before_hidden":           boolTrue(data["key_before_hidden"]),
		"hidden_pages":                hiddenPages(data["hidden_pages"]),
		"ui_language":                 uiLanguage(data),
		"section_max_height_max":      maxSectionMaxHeight,
		"max_payload_mb":              maxPayloadMB(data),
		"enable_anthropic_messages":   boolTrue(data["enable_anthropic_messages"]),
		"filter_models_by_key":        boolTrue(data["filter_models_by_key"]),
		"reasoning_effort_map":        boolDefaultTrue(data["reasoning_effort_map"]),
		"models_only_available":       boolTrue(data["models_only_available"]),
		"free_model_credit_fallback":  boolTrue(data["free_model_credit_fallback"]),
		"accounts_dir":                s.AccountsDir,
		"usage_dir":                   s.UsageDir,
		"settings_file":               s.settingsFile(),
		"project":                     s.Project,
		"version":                     s.Version,
	}
	return view
}

func (s *Service) panelPasswordIsDefault() bool {
	if s != nil && s.PanelPasswordIsDefault != nil {
		return s.PanelPasswordIsDefault()
	}
	return true
}

func (s *Service) settingsFile() string {
	if s != nil && strings.TrimSpace(s.SettingsFile) != "" {
		return s.SettingsFile
	}
	if s != nil && s.Store != nil {
		return s.Store.Path
	}
	return ""
}

func (s *Service) updateSlotOptions() []map[string]any {
	views := s.currentSlotViews()
	out := make([]map[string]any, 0, len(views))
	for _, slot := range views {
		id := textValue(slot["id"])
		label := textValue(slot["label"])
		if label == "" {
			label = textValue(slot["name"])
		}
		if label == "" {
			label = id
		}
		enabled := true
		if value, ok := slot["enabled"].(bool); ok {
			enabled = value
		}
		out = append(out, map[string]any{"id": id, "label": label, "enabled": enabled})
	}
	return out
}

func (s *Service) currentSlotViews() []map[string]any {
	if s == nil {
		return nil
	}
	if s.SlotViews != nil {
		return s.SlotViews()
	}
	if s.Store == nil {
		return nil
	}
	data, err := s.Store.Load()
	if err != nil {
		return nil
	}
	return rawSlotViews(data)
}

// Save applies a panel settings patch. It dispatches API keys to the key store,
// validates platform groups, merges provider sections through contributors,
// and returns the refreshed view plus save counters.
func (s *Service) Save(ctx context.Context, patch map[string]any) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("settings: store is required")
	}
	if patch == nil {
		patch = map[string]any{}
	}
	reply := map[string]any{}

	// Keys are saved first but fully validated before touching disk.
	var keyEntries []keys.Entry
	var deleteIDs []string
	upsert := false
	if raw, present := patch["api_keys"]; present {
		if s.Keys == nil {
			return nil, errors.New("settings: key store is required")
		}
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("api_keys must be a list")
		}
		existingEntries, _ := s.Keys.List(false)
		existing := make(map[string]keys.Entry, len(existingEntries))
		for _, entry := range existingEntries {
			existing[entry.ID] = entry
		}
		for _, item := range list {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("each api key must be an object")
			}
			id := textValue(object["id"])
			stored := existing[id]
			value := strings.TrimSpace(textValue(object["key"]))
			if value == "" && id != "" {
				value = stored.Key
			}
			if value != "" && len(value) < 4 {
				return nil, errors.New("api key must be at least 4 characters")
			}
			if value == "" {
				return nil, errors.New("a key entry is empty - fill it in or remove the row")
			}
			realm := stored.Realm
			if rawRealm, present := object["realm"]; present {
				realm = strings.ToLower(strings.TrimSpace(textValue(rawRealm)))
			}
			if !s.realmAllowed(realm) {
				return nil, errors.New("realm must be intl, cn, codearts, raccoon or empty")
			}
			if realm == "codearts" && !s.providerRealmRegistered("codearts") {
				return nil, errors.New("codearts realm is not configured; enable it in settings first")
			}
			if realm == "raccoon" && !s.providerRealmRegistered("raccoon") {
				return nil, errors.New("raccoon realm is not configured; enable it in settings first")
			}
			models := stored.Models
			if rawModels, present := object["models"]; present {
				models = keys.NormalizeModelPatterns(rawModels)
			}
			accounts := stored.Accounts
			if rawAccounts, present := object["accounts"]; present {
				accounts = keys.NormalizeAccountRefs(rawAccounts)
			}
			name := stored.Name
			if rawName, present := object["name"]; present {
				name = strings.TrimSpace(textValue(rawName))
			}
			enabled := stored.Enabled
			enabledSet := stored.ID != ""
			if rawEnabled, present := object["enabled"]; present {
				if boolean, ok := rawEnabled.(bool); ok {
					enabled = boolean
				} else {
					enabled = true
				}
				enabledSet = true
			}
			concurrency := stored.ConcurrencyLimit
			if rawConcurrency, present := object["concurrency_limit"]; present {
				concurrency = intValue(rawConcurrency)
				if concurrency < 0 {
					concurrency = 0
				}
			}
			createdAt := textValue(object["created_at"])
			if createdAt == "" {
				createdAt = stored.CreatedAt
			}
			if createdAt == "" {
				createdAt = s.now().Format("2006/01/02 15:04")
			}
			keyEntries = append(keyEntries, keys.Entry{
				ID: id, Name: name, Key: value, Realm: realm, Models: models,
				Accounts: accounts, ConcurrencyLimit: concurrency,
				Enabled: enabled, EnabledSet: enabledSet, CreatedAt: createdAt,
			})
		}
		if rawDelete, present := patch["deleted_api_key_ids"]; present {
			if ids, ok := rawDelete.([]any); ok {
				upsert = true
				for _, id := range ids {
					deleteIDs = append(deleteIDs, textValue(id))
				}
			}
		}
		if s.Keys == nil {
			return nil, errors.New("settings: key store is required")
		}
		if upsert {
			if _, err := s.Keys.Save(keyEntries, deleteIDs); err != nil {
				return nil, err
			}
		} else if _, err := s.Keys.Save(keyEntries, nil); err != nil {
			return nil, err
		}
		reply["api_keys_saved"] = len(keyEntries)
	}

	// Validate and build the platform-owned patch before any non-key write.
	platformPatch := map[string]any{}
	if raw, present := patch["auth_disabled"]; present {
		disabled, ok := raw.(bool)
		if !ok {
			return nil, errors.New("auth_disabled must be true or false")
		}
		if s.Keys != nil {
			if err := s.Keys.SetAuthDisabled(disabled); err != nil {
				return nil, err
			}
		} else {
			platformPatch["auth_disabled"] = disabled
		}
		reply["auth_disabled"] = disabled
	}
	limitsPatch, touchedLimits, err := validateLimitsPatch(patch)
	if err != nil {
		return nil, err
	}
	if len(limitsPatch) > 0 {
		// Fold a pre-grouping file first so the scoped patch cannot overwrite
		// the legacy global value with a partial grouped map.
		_ = s.migrateLegacyLimitsData()
		platformPatch["limits"] = limitsPatch
	}

	boolFields := []string{
		"pricing_enabled", "pricing_variant_inherit", "update_check_enabled",
		"local_web_tools", "window_probe", "remaining_priority_enabled",
		"egress_backoff_enabled", "enable_anthropic_messages", "filter_models_by_key",
		"reasoning_effort_map", "models_only_available", "free_model_credit_fallback",
		"accounts_collapsed", "accounts_separate_tab", "key_before_hidden",
		"affinity_stable_key_enabled",
	}
	for _, key := range boolFields {
		raw, present := patch[key]
		if !present {
			continue
		}
		value, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("%s must be true or false", key)
		}
		platformPatch[key] = value
		reply[key] = value
	}
	if raw, present := patch["pricing_refresh_minutes"]; present {
		minutes, err := nonNegativeFloat(raw, false, "pricing_refresh_minutes")
		if err != nil {
			return nil, err
		}
		if minutes > maxPricingRefreshMinutes {
			minutes = maxPricingRefreshMinutes
		}
		platformPatch["pricing_refresh_minutes"] = minutes
		reply["pricing_refresh_minutes"] = minutes
	} else if raw, present := patch["pricing_refresh_hours"]; present {
		hours, err := nonNegativeFloat(raw, false, "pricing_refresh_hours")
		if err != nil {
			return nil, err
		}
		minutes := hours * 60
		if minutes > maxPricingRefreshMinutes {
			minutes = maxPricingRefreshMinutes
		}
		platformPatch["pricing_refresh_minutes"] = minutes
		reply["pricing_refresh_minutes"] = minutes
	}
	if raw, present := patch["credits_refresh_hours"]; present {
		hours, err := nonNegativeFloat(raw, false, "credits_refresh_hours")
		if err != nil {
			return nil, err
		}
		platformPatch["credits_refresh_hours"] = hours
		reply["credits_refresh_hours"] = hours
	}
	if raw, present := patch["section_max_height"]; present {
		value, err := wholeNumber(raw, "section_max_height")
		if err != nil {
			return nil, err
		}
		if value < 0 {
			return nil, errors.New("section_max_height cannot be negative")
		}
		if value > maxSectionMaxHeight {
			value = maxSectionMaxHeight
		}
		platformPatch["section_max_height"] = value
		reply["section_max_height"] = value
	}
	if raw, present := patch["keep_backups"]; present {
		value, ok := strictInt(raw)
		if !ok {
			return nil, errors.New("keep_backups must be an integer 0-3")
		}
		if value < 0 {
			value = 0
		}
		if value > maxKeepBackups {
			value = maxKeepBackups
		}
		platformPatch["keep_backups"] = value
		reply["keep_backups"] = value
	}
	if raw, present := patch["account_max_concurrent"]; present {
		value, err := wholeNumber(raw, "account_max_concurrent")
		if err != nil {
			return nil, err
		}
		if value < 0 {
			return nil, errors.New("account_max_concurrent cannot be less than 0")
		}
		if value > maxAccountMaxConcurrent {
			return nil, fmt.Errorf("account_max_concurrent cannot be more than %d", maxAccountMaxConcurrent)
		}
		platformPatch["account_max_concurrent"] = value
		reply["account_max_concurrent"] = value
	}
	if raw, present := patch["max_payload_mb"]; present {
		value, err := wholeNumber(raw, "max_payload_mb")
		if err != nil {
			return nil, err
		}
		if value < 1 || value > 256 {
			return nil, errors.New("max_payload_mb must be an integer between 1 and 256")
		}
		platformPatch["max_payload_mb"] = value
		reply["max_payload_mb"] = value
	}
	if raw, present := patch["ui_language"]; present {
		value, ok := raw.(string)
		if !ok || (value != "zh" && value != "zh-Hant" && value != "en") {
			return nil, errors.New("ui_language must be zh, zh-Hant or en")
		}
		platformPatch["ui_language"] = value
		reply["ui_language"] = value
	}
	if raw, present := patch["hidden_pages"]; present {
		values, ok := stringList(raw)
		if !ok {
			return nil, errors.New("hidden_pages must be a list of page keys")
		}
		pages := cleanHiddenPages(values)
		platformPatch["hidden_pages"] = pages
		reply["hidden_pages"] = pages
	}
	if raw, present := patch["update_check_proxy_slot"]; present {
		text, ok := raw.(string)
		if !ok {
			return nil, errors.New("update_check_proxy_slot must be a string")
		}
		value := strings.TrimSpace(text)
		if len(value) > 64 {
			value = value[:64]
		}
		if value != "" {
			found := false
			for _, slot := range s.currentSlotViews() {
				if textValue(slot["id"]) == value {
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("update_check_proxy_slot does not exist")
			}
		}
		platformPatch["update_check_proxy_slot"] = value
		reply["update_check_proxy_slot"] = value
	}
	for _, group := range []struct {
		key      string
		validate func(map[string]any) (map[string]any, error)
	}{
		{"media", validateMedia},
		{"usage", validateUsage},
		{"prompt", validatePrompt},
	} {
		raw, present := patch[group.key]
		if !present {
			continue
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", group.key)
		}
		clean, err := group.validate(object)
		if err != nil {
			return nil, err
		}
		platformPatch[group.key] = clean
		reply[group.key] = clean
	}
	for _, key := range []string{"log_db_enabled", "log_db_max_rows", "log_db_max_days", "log_db_max_bytes"} {
		raw, present := patch[key]
		if !present {
			continue
		}
		switch key {
		case "log_db_enabled":
			value, ok := raw.(bool)
			if !ok {
				return nil, errors.New("log_db_enabled must be true or false")
			}
			platformPatch[key] = value
			reply[key] = value
		default:
			value, err := clearableCap(raw, key)
			if err != nil {
				return nil, err
			}
			platformPatch[key] = value
			reply[key] = value
		}
	}
	if raw, present := patch["quota_scan_models"]; present {
		models := stringSlice(raw)
		platformPatch["quota_scan_models"] = models
		reply["quota_scan_models"] = models
	}

	if len(platformPatch) > 0 {
		if _, err := s.Store.Merge(platformPatch); err != nil {
			return nil, fmt.Errorf("settings: save: %w", err)
		}
	}

	// Provider contributors own their sections and ignore unknown patch keys.
	for _, provider := range s.Providers {
		contributor, ok := provider.(providers.SettingsContributor)
		if !ok {
			continue
		}
		if err := contributor.SaveSettings(ctx, patch); err != nil {
			return nil, err
		}
	}

	view, err := s.View(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := reply["limits"]; ok {
		reply["limits"] = view["limits"]
	}
	if len(touchedLimits) > 0 {
		full, _ := view["limits"].(map[string]any)
		reply["limits"] = full
		for _, key := range touchedLimits {
			reply[key] = limitValue(full, key)
		}
	}
	for key, value := range reply {
		view[key] = value
	}
	view["ok"] = true
	return view, nil
}

// RevealKey returns the clear-text secret for a live key id. Deleted ids never
// resolve.
func (s *Service) RevealKey(id string) (string, bool) {
	if s == nil || s.Keys == nil {
		return "", false
	}
	entries, err := s.Keys.List(false)
	if err != nil {
		return "", false
	}
	id = strings.TrimSpace(id)
	for _, entry := range entries {
		if entry.ID == id {
			return entry.Key, true
		}
	}
	return "", false
}

func (s *Service) realmAllowed(realm string) bool {
	switch realm {
	case "", "intl", "cn", "codearts", "raccoon":
		return true
	default:
		return false
	}
}

func (s *Service) providerRealmRegistered(realm string) bool {
	for _, provider := range s.Providers {
		if strings.EqualFold(provider.Realm(), realm) {
			return true
		}
	}
	return false
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func mergeMaps(base, patch map[string]any) map[string]any {
	if base == nil {
		return patch
	}
	if patch == nil {
		return base
	}
	out := make(map[string]any, len(base)+len(patch))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range patch {
		if patchMap, ok := value.(map[string]any); ok {
			if baseMap, ok := out[key].(map[string]any); ok {
				out[key] = mergeMaps(baseMap, patchMap)
				continue
			}
		}
		out[key] = value
	}
	return out
}

func boolTrue(value any) bool {
	result, _ := value.(bool)
	return result
}

func boolDefaultTrue(value any) bool {
	if value == nil {
		return true
	}
	result, ok := value.(bool)
	if !ok {
		return true
	}
	return result
}

func textValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	default:
		return ""
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return int(number)
		}
		if number, err := typed.Float64(); err == nil {
			return int(number)
		}
	case string:
		if number, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return number
		}
	}
	return 0
}

func strictInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		number, err := typed.Int64()
		return int(number), err == nil
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

func wholeNumber(value any, name string) (int, error) {
	switch typed := value.(type) {
	case bool, nil:
		return 0, fmt.Errorf("%s must be a whole number", name)
	case int:
		return typed, nil
	case int64:
		return int(typed), nil
	case float64:
		if typed != float64(int(typed)) {
			return 0, fmt.Errorf("%s must be a whole number", name)
		}
		return int(typed), nil
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return int(number), nil
		}
		return 0, fmt.Errorf("%s must be a whole number", name)
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, fmt.Errorf("%s must be a whole number", name)
		}
		return number, nil
	default:
		return 0, fmt.Errorf("%s must be a whole number", name)
	}
}

func nonNegativeFloat(value any, allowBool bool, name string) (float64, error) {
	if _, ok := value.(bool); ok && !allowBool {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	if value == nil {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case int:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s must be a number", name)
		}
		number = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number", name)
		}
		number = parsed
	default:
		return 0, fmt.Errorf("%s must be a number", name)
	}
	if number < 0 {
		return 0, fmt.Errorf("%s cannot be negative", name)
	}
	return number, nil
}

func stringList(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), true
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	default:
		return nil, false
	}
}

func stringSlice(value any) []string {
	var raw []string
	switch typed := value.(type) {
	case []string:
		raw = typed
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				raw = append(raw, text)
			}
		}
	case string:
		raw = strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == ';' || r == '\n' })
	default:
		return []string{}
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func cleanHiddenPages(value any) []string {
	pages := stringSlice(value)
	out := make([]string, 0, len(pages))
	for _, page := range pages {
		if page == "settings" || len(page) == 0 || len(page) > 32 {
			continue
		}
		valid := page[0] >= 'a' && page[0] <= 'z'
		for _, r := range page {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				valid = false
				break
			}
		}
		if valid {
			out = append(out, page)
		}
		if len(out) >= 32 {
			break
		}
	}
	return out
}

func hiddenPages(value any) []string {
	return cleanHiddenPages(value)
}

func limitsView(data map[string]any) map[string]any {
	raw, grouped := data["limits"].(map[string]any)
	out := map[string]any{}
	for _, key := range limitKeys {
		def := 0
		if key == "expiring_window_days" {
			def = 7
		}
		entry := map[string]any{"global": def, "intl": nil, "cn": nil}
		if grouped {
			if stored, ok := raw[key].(map[string]any); ok {
				if value, ok := stored["global"]; ok && value != nil {
					entry["global"] = nonNegativeInt(value, def)
				}
				for _, realm := range limitRealms {
					if value, ok := stored[realm]; ok && value != nil && textValue(value) != "" {
						entry[realm] = nonNegativeInt(value, 0)
					}
				}
			}
		} else if flat, present := data[key]; present {
			// Legacy pre-grouping install: the top-level value is the global
			// default and there are no realm overrides yet.
			entry["global"] = nonNegativeInt(flat, def)
		}
		out[key] = entry
	}
	return out
}

// migrateLegacyLimits folds v1 pre-grouping top-level guards into `limits`,
// drops the flat keys, and returns the updated document. Read-only accounts
// directories fall back to the in-memory shape.
func (s *Service) migrateLegacyLimits(data map[string]any) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return data, nil
	}
	if _, grouped := data["limits"].(map[string]any); grouped {
		return data, nil
	}
	hasFlat := false
	for _, key := range limitKeys {
		if _, present := data[key]; present {
			hasFlat = true
			break
		}
	}
	if !hasFlat {
		return data, nil
	}
	updated, err := s.Store.Update(func(current map[string]any) error {
		if _, grouped := current["limits"].(map[string]any); grouped {
			return nil
		}
		current["limits"] = limitsView(current)
		for _, key := range limitKeys {
			delete(current, key)
		}
		return nil
	})
	if err != nil {
		return data, err
	}
	return updated, nil
}

func nonNegativeInt(value any, fallback int) int {
	number, err := wholeNumber(value, "")
	if err != nil || number < 0 {
		return fallback
	}
	return number
}

func limitValue(limits map[string]any, key string) int {
	entry, _ := limits[key].(map[string]any)
	if entry == nil {
		return 0
	}
	return intValue(entry["global"])
}

func (s *Service) migrateLegacyLimitsData() error {
	if s == nil || s.Store == nil {
		return nil
	}
	data, err := s.Store.Load()
	if err != nil {
		return err
	}
	_, err = s.migrateLegacyLimits(data)
	return err
}

// validateLimitsPatch returns only the scopes the request actually names. A
// later deep merge then preserves the untouched global value and realm
// overrides, matching v1 set_limit semantics.
func validateLimitsPatch(patch map[string]any) (map[string]any, []string, error) {
	out := map[string]any{}
	touched := []string{}
	rawLimits, hasLimits := patch["limits"]
	if hasLimits {
		if _, ok := rawLimits.(map[string]any); !ok {
			return nil, nil, errors.New("limits must be an object")
		}
	}
	raw, _ := rawLimits.(map[string]any)
	for _, key := range limitKeys {
		entry := map[string]any{}
		if grouped, ok := raw[key].(map[string]any); ok {
			if value, present := grouped["global"]; present {
				number, err := wholeNumber(value, key)
				if err != nil {
					return nil, nil, err
				}
				if number < 0 {
					return nil, nil, fmt.Errorf("%s cannot be negative", key)
				}
				entry["global"] = number
			}
			for _, realm := range limitRealms {
				value, present := grouped[realm]
				if !present {
					continue
				}
				if value == nil || textValue(value) == "" {
					entry[realm] = nil
					continue
				}
				number, err := wholeNumber(value, key)
				if err != nil {
					return nil, nil, err
				}
				if number < 0 {
					return nil, nil, fmt.Errorf("%s cannot be negative", key)
				}
				entry[realm] = number
			}
		} else if flat, present := patch[key]; present {
			number, err := wholeNumber(flat, key)
			if err != nil {
				return nil, nil, err
			}
			if number < 0 {
				return nil, nil, fmt.Errorf("%s cannot be negative", key)
			}
			entry["global"] = number
		}
		if len(entry) == 0 {
			continue
		}
		out[key] = entry
		touched = append(touched, key)
	}
	return out, touched, nil
}

// LogCaps returns the normalized log-database switch and retention caps. A
// zero cap means unlimited; an absent max-days key uses v1's seven-day default.
// Composition maps these values onto logs.Caps without importing the log store
// into the settings package.
func (s *Service) LogCaps() (enabled bool, maxRows, maxDays, maxBytes int64) {
	if s == nil || s.Store == nil {
		return false, 0, defaultLogMaxDays, 0
	}
	data, err := s.Store.Load()
	if err != nil {
		return false, 0, defaultLogMaxDays, 0
	}
	enabled = boolTrue(data["log_db_enabled"])
	maxRows = capValue(logMaxRows(data))
	maxDays = capValue(logMaxDays(data))
	maxBytes = capValue(logMaxBytes(data))
	return enabled, maxRows, maxDays, maxBytes
}

func capValue(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		number, err := typed.Int64()
		if err == nil {
			return number
		}
	case float64:
		return int64(typed)
	}
	return 0
}

func pricingRefreshMinutes(data map[string]any) float64 {
	if raw, present := data["pricing_refresh_minutes"]; present {
		if value, err := nonNegativeFloat(raw, false, "pricing_refresh_minutes"); err == nil {
			if value > maxPricingRefreshMinutes {
				return maxPricingRefreshMinutes
			}
			return value
		}
	}
	if raw, present := data["pricing_refresh_hours"]; present {
		if value, err := nonNegativeFloat(raw, false, "pricing_refresh_hours"); err == nil {
			value *= 60
			if value > maxPricingRefreshMinutes {
				return maxPricingRefreshMinutes
			}
			return value
		}
	}
	return defaultPricingRefreshMinutes
}

func creditsRefreshHours(data map[string]any) float64 {
	if raw, present := data["credits_refresh_hours"]; present {
		if value, err := nonNegativeFloat(raw, false, "credits_refresh_hours"); err == nil {
			return value
		}
	}
	return defaultCreditsRefreshHours
}

func sectionMaxHeight(data map[string]any) int {
	if raw, present := data["section_max_height"]; present {
		if value, err := wholeNumber(raw, "section_max_height"); err == nil {
			if value < 0 {
				return defaultSectionMaxHeight
			}
			if value > maxSectionMaxHeight {
				return maxSectionMaxHeight
			}
			return value
		}
	}
	return defaultSectionMaxHeight
}

func keepBackups(data map[string]any) int {
	if raw, present := data["keep_backups"]; present {
		if value, err := wholeNumber(raw, "keep_backups"); err == nil && value >= 0 && value <= maxKeepBackups {
			return value
		}
	}
	return defaultKeepBackups
}

func accountMaxConcurrent(data map[string]any) int {
	if raw, present := data["account_max_concurrent"]; present {
		if value, err := wholeNumber(raw, "account_max_concurrent"); err == nil && value >= 0 && value <= maxAccountMaxConcurrent {
			return value
		}
	}
	return defaultAccountMaxConcurrent
}

func uiLanguage(data map[string]any) string {
	value, _ := data["ui_language"].(string)
	switch value {
	case "zh", "zh-Hant", "en":
		return value
	default:
		return "zh"
	}
}

func maxPayloadMB(data map[string]any) any {
	if raw, present := data["max_payload_mb"]; present {
		if value, err := wholeNumber(raw, "max_payload_mb"); err == nil && value >= 1 && value <= 256 {
			return value
		}
	}
	return nil
}

func logMaxRows(data map[string]any) any {
	value, present := data["log_db_max_rows"]
	if !present || value == nil {
		return nil
	}
	number, err := wholeNumber(value, "log_db_max_rows")
	if err != nil || number <= 0 {
		return nil
	}
	if number > maxLogMaxRows {
		number = maxLogMaxRows
	}
	return number
}

func logMaxDays(data map[string]any) any {
	value, present := data["log_db_max_days"]
	if !present {
		return defaultLogMaxDays
	}
	if value == nil {
		return nil
	}
	number, err := wholeNumber(value, "log_db_max_days")
	if err != nil || number <= 0 {
		if err != nil {
			return defaultLogMaxDays
		}
		return nil
	}
	if number > maxLogMaxDays {
		number = maxLogMaxDays
	}
	return number
}

func logMaxBytes(data map[string]any) any {
	value, present := data["log_db_max_bytes"]
	if !present || value == nil {
		return nil
	}
	number, err := wholeNumber(value, "log_db_max_bytes")
	if err != nil || number <= 0 {
		return nil
	}
	if number > maxLogMaxBytes {
		number = maxLogMaxBytes
	}
	return number
}

func clearableCap(value any, name string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if _, isBool := value.(bool); isBool {
		return nil, nil
	}
	if text, isText := value.(string); isText && strings.TrimSpace(text) == "" {
		return nil, nil
	}
	number, err := wholeNumber(value, name)
	if err != nil {
		number = intValue(value)
	}
	if number <= 0 {
		return nil, nil
	}
	limit := maxLogMaxRows
	switch name {
	case "log_db_max_days":
		limit = maxLogMaxDays
	case "log_db_max_bytes":
		limit = maxLogMaxBytes
	}
	if number > limit {
		number = limit
	}
	return number, nil
}

func usageView(data map[string]any) map[string]any {
	out := map[string]any{"hour_keep_hours": 48, "day_keep_days": 32}
	raw, _ := data["usage"].(map[string]any)
	if raw == nil {
		return out
	}
	for key, value := range raw {
		out[key] = value
	}
	if value, present := raw["hour_keep_hours"]; present {
		if number, err := wholeNumber(value, "hour_keep_hours"); err == nil && number >= 1 && number <= 24*14 {
			out["hour_keep_hours"] = number
		}
	}
	if value, present := raw["day_keep_days"]; present {
		if number, err := wholeNumber(value, "day_keep_days"); err == nil && number >= 1 && number <= 366 {
			out["day_keep_days"] = number
		}
	}
	return out
}

func validateUsage(patch map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range patch {
		switch key {
		case "hour_keep_hours":
			number, err := wholeNumber(value, key)
			if err != nil {
				return nil, err
			}
			if number < 1 {
				return nil, fmt.Errorf("%s cannot be less than 1", key)
			}
			if number > 24*14 {
				return nil, fmt.Errorf("%s cannot be more than %d", key, 24*14)
			}
			out[key] = number
		case "day_keep_days":
			number, err := wholeNumber(value, key)
			if err != nil {
				return nil, err
			}
			if number < 1 {
				return nil, fmt.Errorf("%s cannot be less than 1", key)
			}
			if number > 366 {
				return nil, fmt.Errorf("%s cannot be more than %d", key, 366)
			}
			out[key] = number
		default:
			return nil, fmt.Errorf("unknown usage setting %q", key)
		}
	}
	return out, nil
}

func promptView(data map[string]any) map[string]any {
	out := map[string]any{"mode": "passthrough", "file": "", "text": ""}
	raw, _ := data["prompt"].(map[string]any)
	if raw == nil {
		return out
	}
	for key, value := range raw {
		out[key] = value
	}
	if value, ok := raw["mode"].(string); ok {
		mode := strings.ToLower(strings.TrimSpace(value))
		if mode == "passthrough" || mode == "custom" || mode == "append" {
			out["mode"] = mode
		}
	}
	if value, ok := raw["file"].(string); ok {
		out["file"] = strings.TrimSpace(value)
	}
	if value, ok := raw["text"].(string); ok {
		out["text"] = value
	}
	return out
}

func validatePrompt(patch map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range patch {
		switch key {
		case "mode":
			mode, ok := value.(string)
			if !ok {
				return nil, errors.New("prompt mode must be a string")
			}
			mode = strings.ToLower(strings.TrimSpace(mode))
			if mode != "passthrough" && mode != "custom" && mode != "append" {
				return nil, errors.New("prompt mode must be passthrough, custom or append")
			}
			out[key] = mode
		case "file":
			text, ok := value.(string)
			if !ok {
				return nil, errors.New("prompt file must be a string")
			}
			out[key] = strings.TrimSpace(text)
		case "text":
			text, ok := value.(string)
			if !ok {
				return nil, errors.New("prompt text must be a string")
			}
			if len([]byte(text)) > 64*1024 {
				return nil, errors.New("prompt text cannot be more than 65536 bytes")
			}
			out[key] = text
		default:
			return nil, fmt.Errorf("unknown prompt setting %q", key)
		}
	}
	return out, nil
}

func mediaView(data map[string]any) map[string]any {
	defaults := map[string]any{
		"enabled": false, "realm": "cn", "identity": "desktop",
		"image_max_per_minute": 6, "video_max_seconds": 15,
		"daily_credits_per_key": 0, "video_concurrency": 3,
	}
	stored, _ := data["media"].(map[string]any)
	out := map[string]any{}
	for key, value := range stored {
		out[key] = value
	}
	for key, def := range defaults {
		value, present := stored[key]
		if !present {
			out[key] = def
			continue
		}
		switch key {
		case "enabled":
			if boolean, ok := value.(bool); ok {
				out[key] = boolean
			} else {
				out[key] = def
			}
		case "realm":
			text, _ := value.(string)
			if text == "cn" || text == "intl" {
				out[key] = text
			} else {
				out[key] = def
			}
		case "identity":
			text, _ := value.(string)
			if text == "desktop" || text == "cli" {
				out[key] = text
			} else {
				out[key] = def
			}
		default:
			number, err := wholeNumber(value, key)
			if err != nil || number < mediaMinimum(key) || number > mediaLimit(key) {
				out[key] = def
			} else {
				out[key] = number
			}
		}
	}
	return out
}

func validateMedia(patch map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range patch {
		switch key {
		case "enabled":
			boolean, ok := value.(bool)
			if !ok {
				return nil, errors.New("enabled must be true or false")
			}
			out[key] = boolean
		case "realm":
			text, _ := value.(string)
			if text != "cn" && text != "intl" {
				return nil, errors.New("realm must be one of cn, intl")
			}
			out[key] = text
		case "identity":
			text, _ := value.(string)
			if text != "desktop" && text != "cli" {
				return nil, errors.New("identity must be one of desktop, cli")
			}
			out[key] = text
		case "image_max_per_minute", "video_max_seconds", "daily_credits_per_key", "video_concurrency":
			number, err := wholeNumber(value, key)
			if err != nil {
				return nil, err
			}
			if number < mediaMinimum(key) {
				return nil, fmt.Errorf("%s cannot be less than %d", key, mediaMinimum(key))
			}
			if number > mediaLimit(key) {
				return nil, fmt.Errorf("%s cannot be more than %d", key, mediaLimit(key))
			}
			out[key] = number
		default:
			return nil, fmt.Errorf("unknown media setting %q", key)
		}
	}
	return out, nil
}

func mediaLimit(key string) int {
	switch key {
	case "image_max_per_minute":
		return 60
	case "video_max_seconds":
		return 60
	case "daily_credits_per_key":
		return 100000
	case "video_concurrency":
		return 8
	default:
		return 0
	}
}

func mediaMinimum(key string) int {
	switch key {
	case "daily_credits_per_key":
		return 0
	default:
		return 1
	}
}

func mediaLimits() map[string]any {
	return map[string]any{
		"image_max_per_minute":  60,
		"video_max_seconds":     60,
		"daily_credits_per_key": 100000,
		"video_concurrency":     8,
	}
}

func mediaMinimums() map[string]any {
	return map[string]any{
		"image_max_per_minute":  1,
		"video_max_seconds":     1,
		"daily_credits_per_key": 0,
		"video_concurrency":     1,
	}
}

// SortedContributorKeys is a small diagnostic helper used by tests to keep
// provider merge order deterministic.
func SortedContributorKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
