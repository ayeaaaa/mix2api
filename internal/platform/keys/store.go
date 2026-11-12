// Package keys owns API-key storage, matching, binding, and per-key
// concurrency. It reads and writes the shared accounts/settings.json document
// through core/config so unknown keys survive a save.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

// Entry is one API key row stored under api_keys. Deleted rows keep the id so
// usage history remains attributable, but their secret is cleared.
type Entry struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Key              string   `json:"key"`
	Realm            string   `json:"realm"`
	Models           []string `json:"models"`
	Accounts         []string `json:"accounts"`
	ConcurrencyLimit int      `json:"concurrency_limit"`
	Enabled          bool     `json:"enabled"`
	// EnabledSet marks an explicit enabled/disabled choice. An Entry built
	// without it defaults to enabled, matching v1's missing-key behavior.
	EnabledSet bool   `json:"-"`
	CreatedAt  string `json:"created_at"`
	DeletedAt  string `json:"deleted_at,omitempty"`
}

// Store manages the api_keys list in settings.json.
type Store struct {
	store *config.Store

	// Now is injectable for deterministic created_at values in tests.
	Now func() time.Time
}

// New returns a Store backed by the shared settings document.
func New(store *config.Store) *Store {
	return &Store{store: store}
}

var (
	// ErrNoStore is returned when the settings store is missing.
	ErrNoStore = errors.New("keys: settings store is required")
	// ErrKeyTooShort is returned for a new secret shorter than four characters.
	ErrKeyTooShort = errors.New("api key must be at least 4 characters")
)

// List returns configured keys in stored order. api_keys is authoritative when
// present; a pre-list install falls back to the single api_key/api_key_set pair.
// Deleted rows are hidden unless includeDeleted is true.
func (s *Store) List(includeDeleted bool) ([]Entry, error) {
	if s == nil || s.store == nil {
		return nil, ErrNoStore
	}
	data, err := s.store.Load()
	if err != nil {
		return nil, fmt.Errorf("keys: load settings: %w", err)
	}
	return entriesFromSettings(data, s.now(), includeDeleted), nil
}

func (s *Store) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func entriesFromSettings(data map[string]any, now time.Time, includeDeleted bool) []Entry {
	if stored, ok := mapList(data["api_keys"]); ok {
		out := make([]Entry, 0, len(stored))
		seenSecrets := make(map[string]bool, len(stored))
		seenIDs := make(map[string]bool, len(stored))
		for _, raw := range stored {
			entry, ok := cleanEntry(raw, now)
			if !ok {
				continue
			}
			if entry.Key != "" {
				if seenSecrets[entry.Key] {
					continue
				}
				seenSecrets[entry.Key] = true
			}
			entry.ID = uniqueKeyID(entry.ID, seenIDs)
			seenIDs[entry.ID] = true
			if !includeDeleted && entry.DeletedAt != "" {
				continue
			}
			out = append(out, entry)
		}
		return out
	}

	if value, _ := data["api_key_set"].(bool); value {
		legacy := strings.TrimSpace(stringValue(data["api_key"]))
		if legacy == "" {
			return nil
		}
		return []Entry{{
			ID:        "legacy",
			Name:      "默认（跟随面板切换）",
			Key:       legacy,
			Realm:     "",
			Models:    []string{},
			Accounts:  []string{},
			Enabled:   true,
			CreatedAt: "",
		}}
	}
	return nil
}

func mapList(value any) ([]map[string]any, bool) {
	switch typed := value.(type) {
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if entry, ok := item.(map[string]any); ok {
				out = append(out, entry)
			} else {
				// Keep positional skipping behavior: non-object rows are
				// simply unusable, not a reason to discard the whole list.
				out = append(out, nil)
			}
		}
		return out, true
	case []map[string]any:
		return typed, true
	case nil:
		return nil, false
	default:
		return nil, false
	}
}

func cleanEntry(raw map[string]any, now time.Time) (Entry, bool) {
	if raw == nil {
		return Entry{}, false
	}
	key := strings.TrimSpace(stringValue(raw["key"]))
	deletedAt := strings.TrimSpace(stringValue(raw["deleted_at"]))
	if key == "" && deletedAt == "" {
		return Entry{}, false
	}
	realm := strings.ToLower(strings.TrimSpace(stringValue(raw["realm"])))
	switch realm {
	case "", "intl", "cn", "codearts", "raccoon", "doubao":
	default:
		realm = ""
	}
	createdAt := strings.TrimSpace(stringValue(raw["created_at"]))
	if createdAt == "" {
		createdAt = now.Format("2006/01/02 15:04")
	}
	name := strings.TrimSpace(stringValue(raw["name"]))
	if name == "" {
		name = "未命名"
	}
	enabled := true
	if deletedAt != "" {
		enabled = false
	} else if value, ok := raw["enabled"].(bool); ok && !value {
		enabled = false
	}
	return Entry{
		ID:               strings.TrimSpace(stringValue(raw["id"])),
		Name:             name,
		Key:              key,
		Realm:            realm,
		Models:           normalizeModelPatterns(raw["models"]),
		Accounts:         normalizeAccountRefs(raw["accounts"]),
		ConcurrencyLimit: concurrencyLimit(raw),
		Enabled:          enabled,
		CreatedAt:        createdAt,
		DeletedAt:        deletedAt,
	}, true
}

func concurrencyLimit(raw map[string]any) int {
	value, ok := integerValue(raw["concurrency_limit"])
	if !ok || value == 0 {
		value, ok = integerValue(raw["max_concurrency"])
	}
	if !ok || value < 0 {
		return 0
	}
	return value
}

func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			parsed, ferr := typed.Float64()
			if ferr != nil {
				return 0, false
			}
			return int(parsed), true
		}
		return int(number), true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		return number, err == nil
	default:
		return 0, false
	}
}

func uniqueKeyID(candidate string, used map[string]bool) string {
	candidate = strings.TrimSpace(candidate)
	if candidate != "" && !used[candidate] {
		return candidate
	}
	if candidate != "" {
		for suffix := 2; ; suffix++ {
			alternative := fmt.Sprintf("%s-%d", candidate, suffix)
			if !used[alternative] {
				return alternative
			}
		}
	}
	for {
		alternative := randomID()
		if !used[alternative] {
			return alternative
		}
	}
}

func randomID() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failure is catastrophic for credentials; an empty id is
		// still unique-checked by the caller and never a valid credential.
		return ""
	}
	return hex.EncodeToString(raw)
}

// Save writes the submitted list. A nil deleteIDs uses replace-by-omission
// (v1's legacy panel contract); a non-nil list uses upsert semantics and only
// retires the named ids. The returned slice is the live list.
func (s *Store) Save(entries []Entry, deleteIDs []string) ([]Entry, error) {
	if s == nil || s.store == nil {
		return nil, ErrNoStore
	}
	var live []Entry
	_, err := s.store.Update(func(data map[string]any) error {
		previous := entriesFromSettings(data, s.now(), true)
		upsert := deleteIDs != nil
		drop := make(map[string]bool, len(deleteIDs))
		for _, id := range deleteIDs {
			drop[strings.TrimSpace(id)] = true
		}

		previousByID := make(map[string]Entry, len(previous))
		liveSecretID := make(map[string]string, len(previous))
		liveIDs := make(map[string]bool, len(previous))
		for _, old := range previous {
			previousByID[old.ID] = old
			if old.Key != "" && old.DeletedAt == "" {
				liveSecretID[old.Key] = old.ID
				liveIDs[old.ID] = true
			}
		}

		cleaned := make([]Entry, 0, len(entries)+len(previous))
		seenSecrets := make(map[string]bool, len(entries))
		seenIDs := make(map[string]bool, len(entries))
		for _, submitted := range entries {
			rawID := strings.TrimSpace(submitted.ID)
			if upsert && drop[rawID] {
				continue
			}
			key := strings.TrimSpace(submitted.Key)
			stored, hasStored := previousByID[rawID]
			if key == "" && rawID != "" && hasStored {
				key = stored.Key
			}
			submitted.Key = key
			if !submitted.EnabledSet && submitted.DeletedAt == "" {
				submitted.Enabled = true
			}
			entry, ok := cleanEntry(entryMap(submitted), s.now())
			if !ok {
				continue
			}
			if entry.Key != "" && len(entry.Key) < 4 {
				return ErrKeyTooShort
			}
			if entry.Key != "" {
				if seenSecrets[entry.Key] {
					continue
				}
				seenSecrets[entry.Key] = true
			}
			if upsert && entry.Key != "" && !liveIDs[rawID] {
				if adopted, ok := liveSecretID[entry.Key]; ok {
					entry.ID = adopted
				}
			}
			if entry.Name == "" {
				entry.Name = "未命名"
			}
			entry.ID = uniqueKeyID(entry.ID, seenIDs)
			seenIDs[entry.ID] = true
			cleaned = append(cleaned, entry)
		}

		for _, old := range previous {
			if seenIDs[old.ID] {
				continue
			}
			seenIDs[old.ID] = true
			if upsert && !drop[old.ID] {
				if old.Key != "" && seenSecrets[old.Key] {
					cleaned = append(cleaned, retiredEntry(old, s.now()))
					continue
				}
				if old.Key != "" {
					seenSecrets[old.Key] = true
				}
				cleaned = append(cleaned, old)
				continue
			}
			cleaned = append(cleaned, retiredEntry(old, s.now()))
		}

		data["api_keys"] = entryMaps(cleaned)
		delete(data, "api_key")
		delete(data, "api_key_set")
		live = liveEntries(cleaned)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return live, nil
}

func entryMap(entry Entry) map[string]any {
	models := make([]any, len(entry.Models))
	for i, model := range entry.Models {
		models[i] = model
	}
	accounts := make([]any, len(entry.Accounts))
	for i, account := range entry.Accounts {
		accounts[i] = account
	}
	raw := map[string]any{
		"id":                entry.ID,
		"name":              entry.Name,
		"key":               entry.Key,
		"realm":             entry.Realm,
		"models":            models,
		"accounts":          accounts,
		"concurrency_limit": entry.ConcurrencyLimit,
		"enabled":           entry.Enabled,
		"created_at":        entry.CreatedAt,
	}
	if entry.DeletedAt != "" {
		raw["deleted_at"] = entry.DeletedAt
	}
	return raw
}

func entryMaps(entries []Entry) []any {
	out := make([]any, len(entries))
	for i, entry := range entries {
		out[i] = entryMap(entry)
	}
	return out
}

func retiredEntry(old Entry, now time.Time) Entry {
	deletedAt := old.DeletedAt
	if deletedAt == "" {
		deletedAt = now.Format("2006/01/02 15:04")
	}
	return Entry{
		ID:        old.ID,
		Name:      old.Name,
		Key:       "",
		Realm:     old.Realm,
		Models:    append([]string(nil), old.Models...),
		Enabled:   false,
		CreatedAt: old.CreatedAt,
		DeletedAt: deletedAt,
	}
}

func liveEntries(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.DeletedAt == "" {
			out = append(out, entry)
		}
	}
	return out
}

// Match returns the enabled live entry whose secret equals supplied. Secrets
// are compared with a hash to keep the comparison constant-time.
func Match(entries []Entry, supplied string) (*Entry, bool) {
	supplied = strings.TrimSpace(supplied)
	if supplied == "" {
		return nil, false
	}
	suppliedHash := sha256.Sum256([]byte(supplied))
	for i := range entries {
		entry := entries[i]
		if !entry.Enabled || entry.DeletedAt != "" || entry.Key == "" {
			continue
		}
		entryHash := sha256.Sum256([]byte(strings.TrimSpace(entry.Key)))
		if subtle.ConstantTimeCompare(suppliedHash[:], entryHash[:]) == 1 {
			copy := entry
			return &copy, true
		}
	}
	return nil, false
}

// AuthRequired reports whether /v1 calls must present a key. Setting
// auth_disabled=true turns only the API-key check off; panel management still
// requires a panel session.
func (s *Store) AuthRequired() bool {
	if s == nil || s.store == nil {
		return true
	}
	data, err := s.store.Load()
	if err != nil {
		return true
	}
	if disabled, ok := data["auth_disabled"].(bool); ok && disabled {
		return false
	}
	// A configured key store with no usable rows still fails closed: an empty
	// credential set must not silently turn /v1 into an open endpoint. Legacy
	// launcher keys are accepted by the API composition alongside this.
	return true
}

// SetAuthDisabled persists the API-key bypass switch.
func (s *Store) SetAuthDisabled(disabled bool) error {
	if s == nil || s.store == nil {
		return ErrNoStore
	}
	_, err := s.store.Merge(map[string]any{"auth_disabled": disabled})
	return err
}

// Mask renders the panel-safe form of a secret. Empty stays empty; eight or
// fewer characters are fully masked, matching v1.
func Mask(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if len(secret) > 8 {
		return secret[:4] + "******" + secret[len(secret)-4:]
	}
	return strings.Repeat("*", len(secret))
}

// MaskedSummaries returns the panel-visible API key rows.
func MaskedSummaries(entries []Entry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.DeletedAt != "" {
			continue
		}
		out = append(out, map[string]any{
			"id":         entry.ID,
			"name":       entry.Name,
			"realm":      entry.Realm,
			"models":     append([]string(nil), entry.Models...),
			"accounts":   append([]string(nil), entry.Accounts...),
			"enabled":    entry.Enabled,
			"masked":     Mask(entry.Key),
			"source":     "panel",
			"created_at": entry.CreatedAt,
		})
	}
	return out
}

// DeletedSummaries returns the read-only history rows.
func DeletedSummaries(entries []Entry) []map[string]any {
	out := make([]map[string]any, 0)
	for _, entry := range entries {
		if entry.DeletedAt == "" {
			continue
		}
		out = append(out, map[string]any{
			"id":         entry.ID,
			"name":       entry.Name,
			"realm":      entry.Realm,
			"created_at": entry.CreatedAt,
			"deleted_at": entry.DeletedAt,
		})
	}
	return out
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
