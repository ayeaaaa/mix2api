package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// LimitKeys are the five v1 guard settings. expiring_window_days has a
// non-zero default (7); every other guard reads 0 as off.
var LimitKeys = []string{
	"reserve_credits", "daily_token_limit", "daily_credit_limit",
	"model_daily_token_limit", "expiring_window_days",
}

const defaultExpiringWindowDays = 7

// DefaultTTL matches v1's WB_STATS_TTL: a hot request reuses the folded day
// counters for a few seconds, while a settings save forces a fresh fold.
const DefaultTTL = 15 * time.Second

// Guard publishes the shared daily guards to every provider that implements
// providers.LimitApplier. Providers without the hook are never touched.
type Guard struct {
	Store     *config.Store
	Providers []providers.Provider
	LogPath   string

	TTL    time.Duration
	Now    func() time.Time
	Logger func(string)

	// LoadRows is a test seam and an embedding seam. Nil reads LogPath through
	// usage.Store.ReadRows.
	LoadRows func(ctx context.Context) ([]usage.Row, error)

	mu        sync.Mutex
	cached    map[string]Folds
	cachedAt  time.Time
	cachedDay string
	haveCache bool
	lastErr   string
}

// NewGuard builds a guard. No file is created; the ledger is read lazily.
func NewGuard(store *config.Store, list []providers.Provider, logPath string) *Guard {
	return &Guard{Store: store, Providers: list, LogPath: logPath}
}

func (g *Guard) now() time.Time {
	if g != nil && g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Guard) ttl() time.Duration {
	if g != nil && g.TTL > 0 {
		return g.TTL
	}
	return DefaultTTL
}

func (g *Guard) logf(format string, args ...any) {
	if g != nil && g.Logger != nil {
		g.Logger(fmt.Sprintf(format, args...))
	}
}

// Apply reads the guard settings once, folds at most one day of rows, and
// publishes values plus this provider realm's counters. refresh bypasses the
// fold TTL (the settings-save path uses it so a limit change is immediate).
// The returned count is the number of providers that received the hook call.
//
// A fold failure passes nil usage, which keeps every provider's last known
// counters (v1 usage=None); it never fabricates zeros that could park an
// account.
func (g *Guard) Apply(ctx context.Context, refresh bool) int {
	if g == nil {
		return 0
	}
	data, err := g.settings()
	if err != nil {
		g.logf("limits: settings read failed: %v", err)
		return 0
	}
	values, active, scanWanted := guardValues(data)
	if !active && !refresh {
		return 0
	}

	var folds map[string]Folds
	if scanWanted {
		if folded, ok := g.loadFolds(ctx, refresh); ok {
			folds = folded
		}
	}

	applied := 0
	for _, provider := range g.Providers {
		applier, ok := provider.(providers.LimitApplier)
		if !ok || provider == nil {
			continue
		}
		var realmUsage map[string]any
		if folds != nil {
			realmFolds, exists := folds[provider.Realm()]
			if !exists {
				realmFolds = newFolds()
			}
			realmUsage = foldPayload(realmFolds)
		}
		applier.ApplyLimits(values, realmUsage)
		applied++
	}
	if applied > 0 && refresh {
		g.logf("limits: applied to %d provider(s), scan=%t refresh=true", applied, folds != nil)
	}
	return applied
}

func (g *Guard) settings() (map[string]any, error) {
	if g == nil || g.Store == nil {
		return nil, errors.New("limits: settings store is required")
	}
	return g.Store.Load()
}

func (g *Guard) loadFolds(ctx context.Context, refresh bool) (map[string]Folds, bool) {
	now := g.now()
	day := now.Format("2006-01-02")
	g.mu.Lock()
	if !refresh && g.haveCache && g.cachedDay == day && now.Sub(g.cachedAt) < g.ttl() && g.cached != nil {
		cached := g.cached
		g.mu.Unlock()
		return cached, true
	}
	g.mu.Unlock()

	rows, err := g.readRows(ctx)
	if err != nil {
		g.mu.Lock()
		g.lastErr = err.Error()
		g.mu.Unlock()
		g.logf("limits: daily fold failed: %v", err)
		return nil, false
	}
	start, end := localDay(now)
	folds := FoldAll(rows, start, end)

	g.mu.Lock()
	g.cached = folds
	g.cachedAt = now
	g.cachedDay = day
	g.haveCache = true
	g.lastErr = ""
	g.mu.Unlock()
	return folds, true
}

func (g *Guard) readRows(ctx context.Context) ([]usage.Row, error) {
	if g.LoadRows != nil {
		return g.LoadRows(ctx)
	}
	path := strings.TrimSpace(g.LogPath)
	if path == "" {
		return nil, errors.New("limits: usage log path is required")
	}
	// The usage store owns the exact v1 line discipline (complete lines only,
	// json.Number precision, malformed rows skipped).
	return usage.New(usage.Paths{Log: path}).ReadRows()
}

// Invalidate drops the cached fold so the next Apply re-reads the ledger.
func (g *Guard) Invalidate() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.haveCache = false
	g.cached = nil
	g.mu.Unlock()
}

// LastError returns the most recent fold error, or "" when the last read
// succeeded. It is safe to call concurrently.
func (g *Guard) LastError() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastErr
}

func localDay(now time.Time) (time.Time, time.Time) {
	year, month, day := now.Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	return start, start.AddDate(0, 0, 1)
}

// guardValues returns the v1-shaped values map, whether any guard is on, and
// whether the three counters need a fold. expiring_window_days alone does not
// require reading the ledger.
func guardValues(data map[string]any) (map[string]any, bool, bool) {
	grouped, hasGrouped := data["limits"].(map[string]any)
	values := make(map[string]any, len(LimitKeys))
	active := false
	scanWanted := false
	for _, key := range LimitKeys {
		entry, hasEntry := limitEntry(grouped, hasGrouped, data, key)
		global := defaultLimit(key)
		if hasEntry {
			if raw, present := entry["global"]; present && raw != nil {
				global = coerceLimit(raw)
			}
		}
		realmValues := map[string]any{"global": global, "intl": global, "cn": global}
		for _, realm := range []string{"intl", "cn"} {
			if hasEntry {
				if raw, present := entry[realm]; present && raw != nil && raw != "" {
					if number, ok := limitNumber(raw); ok {
						realmValues[realm] = number
					}
				}
			}
		}
		values[key] = realmValues
		if key != "expiring_window_days" {
			for _, realm := range []string{"global", "intl", "cn"} {
				if number, _ := limitNumber(realmValues[realm]); number > 0 {
					active = true
					scanWanted = true
				}
			}
		} else {
			for _, realm := range []string{"global", "intl", "cn"} {
				if number, _ := limitNumber(realmValues[realm]); number > 0 {
					active = true
				}
			}
		}
	}
	return values, active, scanWanted
}

func limitEntry(grouped map[string]any, hasGrouped bool, data map[string]any, key string) (map[string]any, bool) {
	if hasGrouped {
		raw, ok := grouped[key].(map[string]any)
		return raw, ok
	}
	// Legacy pre-grouping files stored each guard as a flat top-level number.
	if raw, ok := data[key]; ok && raw != nil {
		return map[string]any{"global": raw}, true
	}
	return nil, false
}

func defaultLimit(key string) int64 {
	if key == "expiring_window_days" {
		return defaultExpiringWindowDays
	}
	return 0
}

func coerceLimit(value any) int64 {
	number, ok := limitNumber(value)
	if !ok || number < 0 {
		return 0
	}
	return number
}

func limitNumber(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return number, true
		}
		if number, err := typed.Float64(); err == nil {
			return int64(number), true
		}
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0, false
		}
		number, err := strconv.ParseInt(text, 10, 64)
		if err == nil {
			return number, true
		}
		if parsed, err := strconv.ParseFloat(text, 64); err == nil {
			return int64(parsed), true
		}
	}
	return 0, false
}

func foldPayload(folds Folds) map[string]any {
	tokens := make(map[string]any, len(folds.Tokens))
	for uid, value := range folds.Tokens {
		tokens[uid] = value
	}
	credits := make(map[string]any, len(folds.Credits))
	for uid, value := range folds.Credits {
		credits[uid] = value
	}
	models := make(map[string]any, len(folds.Models))
	for uid, perModel := range folds.Models {
		copied := make(map[string]any, len(perModel))
		for model, value := range perModel {
			copied[model] = value
		}
		models[uid] = copied
	}
	return map[string]any{
		"tokens":  tokens,
		"credits": credits,
		"models":  models,
	}
}
