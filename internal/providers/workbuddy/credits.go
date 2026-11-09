package workbuddy

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// Billing HTTP budgets mirror v1: 15s for the summary/package/enterprise
	// requests, 30s for the intl fallback, and 10s for checkin status.
	creditsBillingTimeout  = 15 * time.Second
	creditsFallbackTimeout = 30 * time.Second
	creditsCheckinTimeout  = 10 * time.Second

	// creditsTierTTL is v1's TIER_TTL_SECONDS.
	creditsTierTTL = 24 * time.Hour

	// creditsMaxBodyBytes caps one billing response. The endpoints answer a few
	// hundred KiB at most; anything larger is not a legitimate snapshot.
	creditsMaxBodyBytes = 4 << 20

	// expirySentinelDays mirrors v1's EXPIRY_SENTINEL_DAYS: DeductionEndTime
	// further out than this is upstream's way of saying "does not expire".
	expirySentinelDays = 730

	creditsSummaryPath         = "/billing/meter/get-user-resource-summary"
	creditsFreePackagesPath    = "/billing/meter/get-user-resource-free-packages"
	creditsPaidPackagesPath    = "/billing/meter/get-user-resource-paid-packages"
	creditsCheckinPath         = "/v2/billing/meter/checkin-activity-status"
	creditsGetUserResourcePath = "/v2/billing/meter/get-user-resource"
	creditsEnterprisePath      = "/billing/meter/get-enterprise-user-usage"
	creditsEnterprisePathV2    = "/v2/billing/meter/get-enterprise-user-usage"

	creditsTimeLayout = "2006-01-02 15:04:05"
)

var (
	// errCreditAuth marks a terminal 401 after the one allowed ForceRefresh
	// retry. The raw path uses it to avoid retrying the same dead token on the
	// fallback endpoint.
	errCreditAuth = errors.New("workbuddy: credits authorization failed")

	creditLimitKeys = []string{
		"reserve_credits", "daily_token_limit", "daily_credit_limit",
		"model_daily_token_limit", "expiring_window_days",
	}
	creditLimitDefaults = map[string]int{"expiring_window_days": 7}
)

// CreditRefreshRow is one account's v1-shaped credits sweep result.
type CreditRefreshRow struct {
	UID     string          `json:"uid"`
	OK      bool            `json:"ok"`
	Credits *CreditSnapshot `json:"credits"`
	Error   string          `json:"error"`
}

// CreditsRefreshResult is the panel payload returned by RefreshCredits.
type CreditsRefreshResult struct {
	Results  []CreditRefreshRow `json:"results"`
	Accounts []AccountView      `json:"accounts,omitempty"`
	Busy     bool               `json:"busy,omitempty"`
}

// ApplyLimits publishes the shared guard settings and today's usage for this
// realm's accounts. values and usage keep v1's shapes:
//
//	values: {"reserve_credits": {"global": 10, "intl": 20, "cn": 5}, ...}
//	usage:  {"tokens": {uid: 12}, "credits": {uid: 1.5},
//	         "models": {uid: {model: 8}}}
//
// A nil usage keeps the last known counters (v1's usage=None contract); a
// non-nil fold treats a missing uid as zero. The expiring-credits window is the
// only guard with a non-zero default (7 days).
func (p *Provider) ApplyLimits(values, usage map[string]any) {
	if p == nil {
		return
	}
	pool := p.Pool()
	if pool == nil {
		return
	}
	// The live catalog is the authoritative free-model set; the pool also has
	// a static fallback for callers that only touch the pool.
	pool.SetFreeModels(p.FreeModelIDs())
	pool.ApplyLimits(values, usage)
}

// ApplyLimits implements Provider.ApplyLimits at the pool level for callers
// that own the pool directly.
func (p *Pool) ApplyLimits(values, usage map[string]any) {
	if p == nil {
		return
	}
	storedValues := cloneAnyMap(values)
	storedUsage := cloneAnyMap(usage)
	p.mu.Lock()
	p.limitValues = storedValues
	p.usageValues = storedUsage
	p.limitsApplied = true
	free := cloneBoolMap(p.freeModels)
	p.mu.Unlock()

	for _, account := range p.Accounts() {
		applyLimitsToAccount(account, storedValues, storedUsage, free)
	}

	p.mu.Lock()
	p.expiringWindowDays = limitForRealm(storedValues, "expiring_window_days", p.realm)
	p.mu.Unlock()
	p.SetGuardCheck(defaultGuardCheck)
}

func defaultGuardCheck(account *Account, model string) bool {
	return account.GuardBlocked(model) != ""
}

// SetFreeModels replaces the realm's free-model view. Every loaded account is
// updated so the reserve and daily-credit guards can exempt those models.
func (p *Pool) SetFreeModels(models []string) {
	if p == nil {
		return
	}
	set := boolSet(models)
	p.mu.Lock()
	p.freeModels = set
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, account := range accounts {
		account.mu.Lock()
		account.FreeModels = cloneBoolMap(set)
		account.mu.Unlock()
	}
}

// applyStoredLimits gives accounts added after ApplyLimits the same guard
// projection as the accounts already in the pool.
func (p *Pool) applyStoredLimits(account *Account) {
	if p == nil || account == nil {
		return
	}
	p.mu.RLock()
	applied := p.limitsApplied
	values := p.limitValues
	usage := p.usageValues
	free := cloneBoolMap(p.freeModels)
	p.mu.RUnlock()
	if !applied {
		return
	}
	applyLimitsToAccount(account, values, usage, free)
}

func applyLimitsToAccount(account *Account, values, usage map[string]any, free map[string]bool) {
	if account == nil {
		return
	}
	account.mu.Lock()
	realm := account.Auth.Realm
	account.ReserveCredits = limitForRealm(values, "reserve_credits", realm)
	account.DailyTokenLimit = limitForRealm(values, "daily_token_limit", realm)
	account.DailyCreditLimit = limitForRealm(values, "daily_credit_limit", realm)
	account.ModelDailyTokenLimit = limitForRealm(values, "model_daily_token_limit", realm)
	account.ExpiringWindowDays = limitForRealm(values, "expiring_window_days", realm)
	account.FreeModels = cloneBoolMap(free)

	if usage != nil {
		uid := strings.TrimSpace(account.Auth.UID)
		if uid == "" {
			uid = strings.TrimSpace(account.State.UID)
		}
		tokens := int64(0)
		if value, ok := usageNumber(usage["tokens"], uid); ok {
			tokens = int64(value)
		}
		credits := 0.0
		if value, ok := usageNumber(usage["credits"], uid); ok {
			credits = value
		}
		modelTokens := usageModelTokens(usage["models"], uid)
		account.DailyTokensToday = &tokens
		account.DailyCreditsToday = &credits
		account.ModelDailyTokens = modelTokens
	}
	account.mu.Unlock()
}

func usageNumber(container any, uid string) (float64, bool) {
	switch values := container.(type) {
	case map[string]any:
		return numericValue(values[uid])
	case map[string]float64:
		value, ok := values[uid]
		return value, ok
	case map[string]int64:
		value, ok := values[uid]
		return float64(value), ok
	case map[string]int:
		value, ok := values[uid]
		return float64(value), ok
	case map[string]json.Number:
		value, ok := values[uid]
		if !ok {
			return 0, false
		}
		parsed, err := value.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func usageModelTokens(container any, uid string) map[string]int64 {
	values, ok := container.(map[string]any)
	if !ok {
		return nil
	}
	raw, exists := values[uid]
	if !exists {
		return map[string]int64{}
	}
	switch perModel := raw.(type) {
	case map[string]any:
		out := make(map[string]int64, len(perModel))
		for model, value := range perModel {
			number, ok := numericValue(value)
			if !ok {
				return nil
			}
			out[model] = int64(number)
		}
		return out
	case map[string]int64:
		out := make(map[string]int64, len(perModel))
		for model, value := range perModel {
			out[model] = value
		}
		return out
	case map[string]int:
		out := make(map[string]int64, len(perModel))
		for model, value := range perModel {
			out[model] = int64(value)
		}
		return out
	default:
		return nil
	}
}

// limitsFromSettings extracts v1's grouped `limits` map, folding the legacy
// flat keys when the grouped object is absent. Missing entries keep their
// per-key defaults (the expiring window defaults to 7 days).
func limitsFromSettings(settings map[string]any) map[string]any {
	out := make(map[string]any, len(creditLimitKeys))
	if grouped, ok := settings["limits"].(map[string]any); ok {
		for _, key := range creditLimitKeys {
			if value, exists := grouped[key]; exists {
				out[key] = value
			}
		}
		return out
	}
	for _, key := range creditLimitKeys {
		if value, exists := settings[key]; exists {
			out[key] = value
		}
	}
	return out
}

func limitForRealm(values map[string]any, key, realm string) int {
	entry, exists := values[key]
	if !exists || entry == nil {
		return defaultLimit(key)
	}
	if grouped, ok := entry.(map[string]any); ok {
		if realm == realmIntl || realm == realmCN {
			if override, exists := grouped[realm]; exists {
				// A malformed override means "inherit", exactly like a
				// blank/nil slot; only an explicit numeric value (including a
				// literal 0 = off) wins over the global setting.
				if value, ok := limitOverrideValue(override); ok {
					return value
				}
			}
		}
		global, exists := grouped["global"]
		if !exists || global == nil {
			return defaultLimit(key)
		}
		return coerceLimitValue(global, 0)
	}
	return coerceLimitValue(entry, 0)
}

// limitOverrideValue accepts only explicit numeric overrides. Anything else
// (nil, blank, junk strings, unsupported types) returns false so the caller
// inherits the global value instead of treating corruption as "off".
func limitOverrideValue(value any) (int, bool) {
	if value == nil || value == "" {
		return 0, false
	}
	number, ok := numericValue(value)
	if !ok {
		return 0, false
	}
	if number < 0 {
		return 0, true
	}
	return int(number), true
}

func defaultLimit(key string) int {
	if value, ok := creditLimitDefaults[key]; ok {
		return value
	}
	return 0
}

func coerceLimitValue(value any, fallback int) int {
	number, ok := numericValue(value)
	if !ok {
		if fallback > 0 {
			return fallback
		}
		return 0
	}
	if number < 0 {
		return 0
	}
	return int(number)
}

func boolSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = true
		}
	}
	return out
}

func staticFreeModelSet(realm string) map[string]bool {
	out := make(map[string]bool)
	for _, entry := range staticCatalogForRealm(realm) {
		if creditsIsFree(entry.Meta["credits"]) {
			out[entry.ID] = true
		}
	}
	return out
}

func cloneBoolMap(values map[string]bool) map[string]bool {
	if values == nil {
		return nil
	}
	out := make(map[string]bool, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = cloneAnyValue(value)
	}
	return out
}

func cloneAnyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneAnyValue(item)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func roundedFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := roundCredit(*value)
	return &copied
}

func cloneModelTokenMap(values map[string]int64) map[string]int64 {
	if values == nil {
		return nil
	}
	out := make(map[string]int64, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// SetFreeModels replaces the account's free-model exemption set.
func (a *Account) SetFreeModels(models []string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.FreeModels = boolSet(models)
	a.mu.Unlock()
}

// ModelIsFree reports whether the realm catalog marks model as free.
func (a *Account) ModelIsFree(model string) bool {
	if a == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.modelIsFreeLocked(model)
}

func (a *Account) modelIsFreeLocked(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if a.FreeModels[model] {
		return true
	}
	// Hand-built accounts without an ApplyLimits projection still get the
	// static catalogue's answer; a loaded pool always carries an explicit set.
	if a.FreeModels == nil {
		return isFreeModel(model, a.Auth.Realm)
	}
	return false
}

// ReserveBlocked reports whether the low-credit reserve should keep this
// account off model. Free models are exempt, and an unknown balance never
// blocks.
func (a *Account) ReserveBlocked(model string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reserveBlockedLocked(model)
}

func (a *Account) reserveBlockedLocked(model string) bool {
	if a.ReserveCredits <= 0 {
		return false
	}
	if model != "" && a.modelIsFreeLocked(model) {
		return false
	}
	if a.Credits == nil {
		return false
	}
	return int64(a.Credits.Remain) <= int64(a.ReserveCredits)
}

// DailyLimitBlocked reports whether today's token count reached the configured
// daily token limit. An unknown count never blocks.
func (a *Account) DailyLimitBlocked() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.dailyLimitBlockedLocked()
}

func (a *Account) dailyLimitBlockedLocked() bool {
	if a.DailyTokenLimit <= 0 || a.DailyTokensToday == nil {
		return false
	}
	return *a.DailyTokensToday >= int64(a.DailyTokenLimit)
}

// CreditLimitReached reports whether today's credit spend reached the daily
// credit limit. It is account-level; CreditLimitBlocked applies the free-model
// exemption.
func (a *Account) CreditLimitReached() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.creditLimitReachedLocked()
}

func (a *Account) creditLimitReachedLocked() bool {
	if a.DailyCreditLimit <= 0 || a.DailyCreditsToday == nil {
		return false
	}
	return *a.DailyCreditsToday >= float64(a.DailyCreditLimit)
}

// CreditLimitBlocked applies v1's daily-credit guard to one model. Free models
// keep serving after the cap; a model-less call is not judged here because the
// caller cannot know whether it would spend credits.
func (a *Account) CreditLimitBlocked(model string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.creditLimitBlockedLocked(model)
}

func (a *Account) creditLimitBlockedLocked(model string) bool {
	if model == "" || a.modelIsFreeLocked(model) {
		return false
	}
	return a.creditLimitReachedLocked()
}

// ModelTokenLimitBlocked reports whether model already burned its per-model
// daily token budget. Other models keep working.
func (a *Account) ModelTokenLimitBlocked(model string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.modelTokenLimitBlockedLocked(model)
}

func (a *Account) modelTokenLimitBlockedLocked(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" || a.ModelDailyTokenLimit <= 0 || a.ModelDailyTokens == nil {
		return false
	}
	used, ok := a.ModelDailyTokens[model]
	return ok && used >= int64(a.ModelDailyTokenLimit)
}

// GuardBlocked returns the first active guard name in v1's order:
// reserve, daily_token, daily_credit, model_daily_token. Free models escape
// reserve and daily_credit only.
func (a *Account) GuardBlocked(model string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.reserveBlockedLocked(model) {
		return "reserve"
	}
	if a.dailyLimitBlockedLocked() {
		return "daily_token"
	}
	if a.creditLimitBlockedLocked(model) {
		return "daily_credit"
	}
	if a.modelTokenLimitBlockedLocked(model) {
		return "model_daily_token"
	}
	return ""
}

// BlockedModelNames returns the models currently out of per-model token
// budget, sorted. It is a diagnostics helper.
func (a *Account) BlockedModelNames() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0)
	for model := range a.ModelDailyTokens {
		if a.modelTokenLimitBlockedLocked(model) {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}

// ConsumeCredit subtracts a finished request's actual cost from the cached
// balance. The next refresh overwrites the estimate. Values never go negative
// or non-finite.
func (a *Account) ConsumeCredit(amount float64) {
	if a == nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Credits == nil {
		return
	}
	remain := math.Max(0, a.Credits.Remain-amount)
	used := a.Credits.Used + amount
	a.Credits.Remain = roundCredit(remain)
	a.Credits.Used = roundCredit(used)
	if a.Credits.Size > 0 {
		a.Credits.RemainPercent = fmt.Sprintf("%.1f%%", a.Credits.Remain/a.Credits.Size*100)
		a.Credits.UsedPercent = fmt.Sprintf("%.1f%%",
			math.Min(a.Credits.Used, a.Credits.Size)/a.Credits.Size*100)
		return
	}
	a.Credits.UsedPercent = "0.0%"
	a.Credits.RemainPercent = "100.0%"
}

// SetCredits persists and publishes a refreshed balance snapshot. Passing nil
// removes the stored snapshot; the runtime tier mirror is left untouched.
func (p *Pool) SetCredits(uid string, snapshot *CreditSnapshot) error {
	return p.setCreditsAndTier(uid, snapshot, nil)
}

func (p *Pool) setCreditsAndTier(uid string, snapshot *CreditSnapshot, tier any) error {
	if p == nil {
		return ErrAccountNotFound
	}
	var marshalErr error
	err := p.mutateAccount(uid, func(account *Account) {
		account.setCreditsLocked(snapshot)
		if snapshot == nil {
			delete(account.Auth.Extra, "credits")
		} else {
			raw, err := json.Marshal(snapshot)
			if err != nil {
				marshalErr = err
			} else {
				if account.Auth.Extra == nil {
					account.Auth.Extra = make(map[string]json.RawMessage)
				}
				account.Auth.Extra["credits"] = raw
			}
		}
		if creditTierPresent(tier) {
			applyTierLocked(account, tier)
		}
	})
	if err != nil {
		return err
	}
	return marshalErr
}

// creditTierPresent rejects both a nil interface and a typed nil/empty tier
// map. Without it, a nil map converted to `any` would marshal as JSON null and
// erase a persisted tier.
func creditTierPresent(tier any) bool {
	if tier == nil {
		return false
	}
	if values, ok := tier.(map[string]any); ok {
		return len(values) > 0
	}
	return true
}

func creditSnapshotOf(account *Account) *CreditSnapshot {
	if account == nil {
		return nil
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return cloneCreditSnapshot(account.Credits)
}

// CreditsRefreshTargets selects the accounts for RefreshCreditsFor. Leaving
// every field empty means "all accounts in this provider realm". UID wins over
// UIDs; an explicitly empty (non-nil) UIDs slice means "none", matching v1's
// payload-presence contract. Realm "all" (or empty) means every account in this
// provider; a different realm selects nothing because the two realms are
// separate provider instances.
type CreditsRefreshTargets struct {
	UID   string
	UIDs  []string
	Realm string
	Group string
}

// RefreshCredits refreshes every loaded account in this realm's pool. A failed
// account is reported in its own row and never stops the rest of the sweep.
// Disabled accounts are refreshed too, matching v1's all-pool sweep; requests
// and persistence remain provider-private. Concurrent calls are coalesced per
// account so one credential is never fetched or written out of order.
func (p *Provider) RefreshCredits(ctx context.Context) (CreditsRefreshResult, error) {
	result := CreditsRefreshResult{Results: []CreditRefreshRow{}}
	if p == nil {
		return result, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return result, errors.New("workbuddy: credits refresh requires a context")
	}
	pool := p.Pool()
	if pool == nil {
		return result, errors.New("workbuddy: account pool is nil")
	}
	return p.refreshCreditAccounts(ctx, pool, pool.Accounts())
}

// RefreshCreditsFor refreshes a v1-shaped target selection (uid/uids/realm/
// group). It is the Task 9 panel entry point and shares the same per-account
// single-flight and row semantics as RefreshCredits.
func (p *Provider) RefreshCreditsFor(ctx context.Context, targets CreditsRefreshTargets) (CreditsRefreshResult, error) {
	result := CreditsRefreshResult{Results: []CreditRefreshRow{}}
	if p == nil {
		return result, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return result, errors.New("workbuddy: credits refresh requires a context")
	}
	pool := p.Pool()
	if pool == nil {
		return result, errors.New("workbuddy: account pool is nil")
	}
	return p.refreshCreditAccounts(ctx, pool, p.creditTargetAccounts(pool, targets))
}

func (p *Provider) creditTargetAccounts(pool *Pool, targets CreditsRefreshTargets) []*Account {
	if pool == nil {
		return nil
	}
	var accounts []*Account
	if uid := strings.TrimSpace(targets.UID); uid != "" {
		if account := pool.Get(uid); account != nil {
			accounts = []*Account{account}
		}
	} else if targets.UIDs != nil {
		seen := make(map[string]bool)
		for _, rawUID := range targets.UIDs {
			uid := strings.TrimSpace(rawUID)
			if uid == "" || seen[uid] {
				continue
			}
			seen[uid] = true
			if account := pool.Get(uid); account != nil {
				accounts = append(accounts, account)
			}
		}
	} else if realm := strings.TrimSpace(targets.Realm); realm != "" && !strings.EqualFold(realm, "all") {
		if normalized, ok := normalizeRealm(realm); ok && normalized == p.realm {
			accounts = pool.Accounts()
		}
	} else {
		accounts = pool.Accounts()
	}
	if group := strings.TrimSpace(targets.Group); group != "" {
		filtered := accounts[:0]
		for _, account := range accounts {
			account.mu.RLock()
			accountGroup := strings.TrimSpace(account.Group)
			account.mu.RUnlock()
			if accountGroup == group {
				filtered = append(filtered, account)
			}
		}
		accounts = filtered
	}
	return accounts
}

func (p *Provider) refreshCreditAccounts(ctx context.Context, pool *Pool, accounts []*Account) (CreditsRefreshResult, error) {
	result := CreditsRefreshResult{Results: make([]CreditRefreshRow, 0, len(accounts))}
	for _, account := range accounts {
		result.Results = append(result.Results, p.refreshCreditAccount(ctx, account))
	}
	result.Accounts = pool.List()
	return result, nil
}

type creditRefreshFlight struct {
	done chan struct{}
	row  CreditRefreshRow
}

// refreshCreditAccount coalesces concurrent refreshes of one account. The
// first caller performs the upstream fetch and credential write; later callers
// wait for that result instead of spending another request or racing the file.
func (p *Provider) refreshCreditAccount(ctx context.Context, account *Account) CreditRefreshRow {
	uid := accountUID(account)
	p.creditsMu.Lock()
	if p.creditFlights == nil {
		p.creditFlights = make(map[*Account]*creditRefreshFlight)
	}
	if flight := p.creditFlights[account]; flight != nil {
		p.creditsMu.Unlock()
		if p.creditJoinHook != nil {
			p.creditJoinHook()
		}
		select {
		case <-flight.done:
			p.creditsMu.Lock()
			row := cloneCreditRefreshRow(flight.row)
			p.creditsMu.Unlock()
			return row
		case <-ctx.Done():
			return CreditRefreshRow{
				UID:     uid,
				Credits: creditSnapshotOf(account),
				Error:   p.redactCreditError(account, ctx.Err()),
			}
		}
	}
	flight := &creditRefreshFlight{done: make(chan struct{})}
	p.creditFlights[account] = flight
	p.creditsMu.Unlock()

	row := p.fetchAndPersistCreditAccount(ctx, account)

	p.creditsMu.Lock()
	flight.row = cloneCreditRefreshRow(row)
	delete(p.creditFlights, account)
	close(flight.done)
	p.creditsMu.Unlock()
	return row
}

func (p *Provider) fetchAndPersistCreditAccount(ctx context.Context, account *Account) CreditRefreshRow {
	row := CreditRefreshRow{UID: accountUID(account)}
	if err := ctx.Err(); err != nil {
		row.Error = p.redactCreditError(account, err)
		row.Credits = creditSnapshotOf(account)
		return row
	}

	snapshot, tier, err := p.fetchRawCredits(ctx, account)
	if err == nil {
		if creditTierPresent(tier) {
			if paid, ok := creditBool(tier["paid"]); ok {
				snapshot.IsPaidUser = paid
			}
		}
		uid := accountUID(account)
		if uid == "" {
			err = errors.New("workbuddy: credit account has no uid")
		} else {
			err = p.Pool().setCreditsAndTier(uid, snapshot, tier)
		}
	}
	if err != nil {
		row.Error = p.redactCreditError(account, err)
		row.Credits = creditSnapshotOf(account)
	} else {
		row.OK = true
		row.Credits = cloneCreditSnapshot(snapshot)
	}
	return row
}

func cloneCreditRefreshRow(row CreditRefreshRow) CreditRefreshRow {
	row.Credits = cloneCreditSnapshot(row.Credits)
	return row
}

func (p *Provider) fetchRawCredits(ctx context.Context, account *Account) (*CreditSnapshot, map[string]any, error) {
	session := newCreditsSession(p, account)
	if session.state.token == "" {
		return nil, nil, errors.New("workbuddy: account has no access token")
	}

	// Static API keys have no personal packages; v1 sends them straight to the
	// enterprise usage endpoint (with the /v2 prefix).
	if session.state.apiKey {
		snapshot, err := p.fetchEnterpriseCredits(ctx, session)
		return snapshot, nil, err
	}

	if p.realm == realmCN {
		snapshot, tier, err := p.fetchCNCredits(ctx, session)
		enterprise := creditsUsesEnterprise(session.state)
		if err == nil {
			if snapshot.Size == 0 && enterprise {
				if enterpriseSnapshot, enterpriseErr := p.fetchEnterpriseCredits(ctx, session); enterpriseErr == nil {
					return enterpriseSnapshot, nil, nil
				}
			}
			return snapshot, tier, nil
		}
		if errors.Is(err, errCreditAuth) {
			session.account.SetTokenExpired(true)
			return nil, nil, err
		}
		if enterprise {
			if enterpriseSnapshot, enterpriseErr := p.fetchEnterpriseCredits(ctx, session); enterpriseErr == nil {
				return enterpriseSnapshot, nil, nil
			}
		}
		snapshot, err = p.fetchFallbackCredits(ctx, session)
		return snapshot, nil, err
	}

	snapshot, err := p.fetchFallbackCredits(ctx, session)
	if err != nil {
		return nil, nil, err
	}
	// v1's fetch_credits refreshes the intl tier from the billing summary at
	// most once per TIER_TTL_SECONDS. Failure is soft: the balance still lands
	// and the previous tier/timestamp stay untouched.
	return snapshot, p.refreshIntlTierIfDue(ctx, session), nil
}

type creditAccountState struct {
	uid          string
	token        string
	domain       string
	realm        string
	enterpriseID string
	proxyURL     string
	product      string
	apiKey       bool
	canRefresh   bool
}

func creditAccountStateOf(account *Account) creditAccountState {
	if account == nil {
		return creditAccountState{}
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	enterprise := strings.TrimSpace(account.Auth.EnterpriseID)
	if enterprise == "" {
		enterprise = strings.TrimSpace(parseJWTClaims(account.Auth.AccessToken).EnterpriseID)
	}
	uid := strings.TrimSpace(account.Auth.UID)
	if uid == "" {
		uid = strings.TrimSpace(account.State.UID)
	}
	realm := realmOrDefault(account.Auth.Realm)
	return creditAccountState{
		uid:          uid,
		token:        strings.TrimSpace(account.Auth.AccessToken),
		domain:       strings.TrimSpace(account.Auth.Domain),
		realm:        realm,
		enterpriseID: enterprise,
		proxyURL:     strings.TrimSpace(account.Auth.ProxyURL),
		product:      normalizeProduct(account.Auth.Product),
		apiKey:       tokenAuthIsAPIKey(account.Auth),
		canRefresh:   !tokenAuthIsAPIKey(account.Auth) && strings.TrimSpace(account.Auth.RefreshToken) != "",
	}
}

func creditsUsesEnterprise(state creditAccountState) bool {
	return state.apiKey || state.enterpriseID != ""
}

type creditsSession struct {
	provider  *Provider
	account   *Account
	state     creditAccountState
	refreshed bool
}

func newCreditsSession(provider *Provider, account *Account) *creditsSession {
	return &creditsSession{
		provider: provider,
		account:  account,
		state:    creditAccountStateOf(account),
	}
}

func (s *creditsSession) postJSON(ctx context.Context, endpoint string, body []byte, refreshOn401 bool, timeout time.Duration) (int, map[string]any, error) {
	status, payload, err := s.postOnce(ctx, endpoint, body, timeout)
	if err != nil {
		return status, payload, err
	}
	if status != http.StatusUnauthorized || !refreshOn401 || s.state.apiKey {
		return status, payload, nil
	}
	if !s.refreshed {
		s.refreshed = true
		if !s.state.canRefresh {
			s.account.SetTokenExpired(true)
			return status, payload, errCreditAuth
		}
		// A 401 is authoritative: the local expiry may still be in the future
		// (upstream revocation / clock skew), so EnsureToken's due check is not
		// enough. ForceRefresh always rotates the credential before the retry.
		if refreshErr := s.provider.ForceRefresh(ctx, s.account); refreshErr != nil {
			return status, payload, refreshErr
		}
		s.state = creditAccountStateOf(s.account)
		status, payload, err = s.postOnce(ctx, endpoint, body, timeout)
		if err != nil {
			return status, payload, err
		}
	}
	if status == http.StatusUnauthorized {
		s.account.SetTokenExpired(true)
		return status, payload, errCreditAuth
	}
	return status, payload, nil
}

func (s *creditsSession) postOnce(ctx context.Context, endpoint string, body []byte, defaultTimeout time.Duration) (int, map[string]any, error) {
	client := s.provider.creditHTTPClient()
	if client == nil {
		return 0, nil, errors.New("workbuddy: credits HTTP client is nil")
	}
	timeout := s.provider.creditTimeout(defaultTimeout)
	requestCtx := nonNilContext(ctx)
	if timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, errors.New("workbuddy: credits request is invalid")
	}
	request.Header = s.provider.creditHeaders(s.state)
	response, err := client.Do(requestCtx, request, s.state.proxyURL)
	if err != nil {
		return 0, nil, errors.New("workbuddy: credits request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, creditsMaxBodyBytes+1))
	if err != nil {
		return response.StatusCode, nil, errors.New("workbuddy: credits response read failed")
	}
	if len(raw) > creditsMaxBodyBytes {
		return response.StatusCode, nil, errors.New("workbuddy: credits response too large")
	}
	var payload map[string]any
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return response.StatusCode, nil, errors.New("workbuddy: credits response is not a JSON object")
			}
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, payload, nil
	}
	if payload == nil {
		return response.StatusCode, nil, errors.New("workbuddy: credits response is not a JSON object")
	}
	return response.StatusCode, payload, nil
}

func (p *Provider) creditHTTPClient() ContextHTTPDoer {
	if p == nil {
		return nil
	}
	if p.creditsDoer != nil {
		return p.creditsDoer
	}
	// Task 8's tests point one fake at the task seams; share that client so the
	// post-check-in balance refresh never reaches the real billing host.
	if p.tasksDoer != nil {
		return p.tasksDoer
	}
	if p.transport != nil {
		return p.transport
	}
	return defaultProfileTransport
}

func (p *Provider) creditTimeout(defaultTimeout time.Duration) time.Duration {
	if p != nil && p.creditsTimeout > 0 {
		return p.creditsTimeout
	}
	return defaultTimeout
}

func (p *Provider) creditsNowTime() time.Time {
	if p != nil && p.creditsNow != nil {
		return p.creditsNow()
	}
	return time.Now()
}

func creditBillingBaseForRealm(realm string) string {
	if realmOrDefault(realm) == realmCN {
		return "https://www.codebuddy.cn"
	}
	return "https://www.workbuddy.ai"
}

func creditBillingOriginForRealm(realm string) string {
	return creditBillingBaseForRealm(realm)
}

func (p *Provider) creditsBillingBase() string {
	if p != nil {
		if base := strings.TrimRight(strings.TrimSpace(p.creditsBillingBaseURL), "/"); base != "" {
			return base
		}
		if base := strings.TrimRight(strings.TrimSpace(p.tasksBillingBaseURL), "/"); base != "" {
			return base
		}
		if base := strings.TrimRight(strings.TrimSpace(p.tasksChatBaseURL), "/"); base != "" {
			return base
		}
	}
	return creditBillingBaseForRealm(p.realm)
}

func (p *Provider) creditsCheckinBase() string {
	if p != nil {
		if base := strings.TrimRight(strings.TrimSpace(p.creditsCheckinBaseURL), "/"); base != "" {
			return base
		}
		if base := strings.TrimRight(strings.TrimSpace(p.tasksChatBaseURL), "/"); base != "" {
			return base
		}
	}
	if p == nil {
		return ""
	}
	return strings.TrimRight(p.EndpointFor(productDesktop), "/")
}

func (p *Provider) creditHeaders(state creditAccountState) http.Header {
	origin := creditBillingOriginForRealm(p.realm)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("X-Client-Platform", "web")
	headers.Set("User-Agent", "WorkBuddy/"+p.DesktopVersion())
	headers.Set("Origin", origin)
	headers.Set("Referer", origin+"/")
	headers.Set("Authorization", "Bearer "+state.token)
	headers.Set("X-User-Id", state.uid)
	domain := state.domain
	if domain == "" {
		domain = domainForRealm(p.realm)
	}
	headers.Set("X-Domain", domain)
	headers.Set("X-CodeBuddy-Request", "1")
	language := "en-US"
	if p.realm == realmCN {
		language = "zh-CN"
		headers.Set("X-Product", "SaaS")
	}
	headers.Set("Accept-Language", language)
	if state.apiKey {
		headers.Set("X-API-Key", state.token)
	}
	if requestID := creditRequestID(); requestID != "" {
		headers.Set("X-Request-ID", requestID)
	}
	if state.uid != "" {
		headers.Set("X-Machine-ID", creditDerivedID(state.uid, "machine"))
		headers.Set("X-Session-ID", creditDerivedID(state.uid, "session"))
	}
	// v1's billing headers carry the configured device token for desktop
	// identities. Keep it CN-only here for the same #448 reason as chat: the
	// intl client does not have this header and it links accounts to one device.
	if p.realm == realmCN && state.product != productCLI {
		if token, err := ResolveDeviceToken(p.accountsDir); err == nil && token != "" {
			headers.Set("X-Device-Token", token)
		}
	}
	if state.enterpriseID != "" {
		headers.Set("X-Enterprise-Id", state.enterpriseID)
		headers.Set("X-Tenant-Id", state.enterpriseID)
	} else {
		headers.Set("X-No-Enterprise-Id", "1")
	}
	return headers
}

func (p *Provider) redactCreditError(account *Account, err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if account != nil {
		account.mu.RLock()
		values := accountSecretValuesLocked(account)
		account.mu.RUnlock()
		text = redactAccountText(text, values)
	}
	return text
}

// creditDerivedID mirrors v1 wb_fingerprint.derive_id: a stable UUID-shaped id
// per (uid, salt), so billing requests from one account look like one device
// without sharing an identifier across accounts.
func creditDerivedID(uid, salt string) string {
	seed := salt + ":" + uid
	if uid == "" {
		seed = salt + ":anonymous"
	}
	sum := md5.Sum([]byte(seed))
	h := hex.EncodeToString(sum[:])
	h = h[:12] + "4" + h[13:16] + "a" + h[17:]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// creditRequestID mirrors v1's per-request 32-hex id.
func creditRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(raw[:])
}

func creditEndpoint(base, path string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + path
}

func creditHTTPError(status int, payload map[string]any) error {
	if status == http.StatusUnauthorized {
		return errCreditAuth
	}
	if message := creditPayloadMessage(payload); message != "" {
		if creditAuthFailureText(message) {
			return fmt.Errorf("%w: HTTP %d: %s", errCreditAuth, status, message)
		}
		return fmt.Errorf("workbuddy: credits HTTP %d: %s", status, message)
	}
	return fmt.Errorf("workbuddy: credits HTTP %d", status)
}

func creditPayloadMessage(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	return creditText(payload["msg"])
}

func creditCodeError(payload map[string]any) error {
	code, ok := numericValue(payload["code"])
	if ok && code == 0 {
		return nil
	}
	message := creditPayloadMessage(payload)
	if creditAuthFailureText(message) {
		return fmt.Errorf("%w: %s", errCreditAuth, message)
	}
	if message != "" {
		return fmt.Errorf("workbuddy: credits code %v: %s", payload["code"], message)
	}
	return fmt.Errorf("workbuddy: credits code %v", payload["code"])
}

// creditAuthFailureText is v1's _auth_failure_text. A credential-shaped error
// names authorization; a bare HTML 401 from an intermediate proxy does not.
// The Chinese login-expired message is the extra marker v1 checks on the CN
// detailed path.
func creditAuthFailureText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "invalid_token") || strings.Contains(lower, "invalid-token") {
		return true
	}
	if strings.Contains(lower, "<html") {
		return false
	}
	for _, marker := range []string{"401", "unauthorized", "authorization", "token expired", "登录"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func creditObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func creditText(value any) string {
	return metaText(value)
}

// creditFieldText mirrors v1's `acc.get("Field") or ""`: falsy values
// (nil, false, numeric zero, empty string) all collapse to empty instead of
// becoming the string "0".
func creditFieldText(value any) string {
	if !creditTruthy(value) {
		return ""
	}
	return creditText(value)
}

func creditNumber(value any) float64 {
	number, _ := numericValue(value)
	return number
}

func creditTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		// Python truthiness (v1 uses bool(...)): any non-empty string is true.
		return typed != ""
	case float64:
		return typed != 0
	case float32:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		if number, ok := numericValue(typed); ok {
			return number != 0
		}
		return false
	}
}

func creditTruthyDefault(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return creditTruthy(value)
}

func creditBool(value any) (bool, bool) {
	boolean, ok := value.(bool)
	if ok {
		return boolean, true
	}
	if value == nil {
		return false, false
	}
	return creditTruthy(value), true
}

func roundCredit(value float64) float64 {
	return math.Round(value*100) / 100
}

func updateCreditPercentages(snapshot *CreditSnapshot) {
	if snapshot == nil {
		return
	}
	if snapshot.Size > 0 {
		snapshot.UsedPercent = fmt.Sprintf("%.1f%%", snapshot.Used/snapshot.Size*100)
		snapshot.RemainPercent = fmt.Sprintf("%.1f%%", snapshot.Remain/snapshot.Size*100)
		return
	}
	snapshot.UsedPercent = "0.0%"
	snapshot.RemainPercent = "100.0%"
}

func newCreditSnapshot(packages []CreditPackage, paid bool, checkin *CheckinStatus, now time.Time) *CreditSnapshot {
	if packages == nil {
		packages = []CreditPackage{}
	}
	sortCreditPackages(packages)
	remain, used, size := 0.0, 0.0, 0.0
	for _, pkg := range packages {
		remain += pkg.Remain
		used += pkg.Used
		size += pkg.Size
	}
	snapshot := &CreditSnapshot{
		Remain:     roundCredit(remain),
		Used:       roundCredit(used),
		Size:       roundCredit(size),
		IsPaidUser: paid,
		Checkin:    cloneCheckin(checkin),
		Packages:   packages,
		UpdatedAt:  float64(now.UnixNano()) / float64(time.Second),
		UpdatedISO: now.Format(creditsTimeLayout),
	}
	updateCreditPercentages(snapshot)
	snapshot.EarliestExpiring = earliestExpiring(packages)
	return snapshot
}

func cloneCheckin(checkin *CheckinStatus) *CheckinStatus {
	if checkin == nil {
		return nil
	}
	copied := *checkin
	return &copied
}

func earliestExpiring(packages []CreditPackage) *EarliestExpiring {
	var best *CreditPackage
	for i := range packages {
		pkg := &packages[i]
		if pkg.Remain <= 0 || pkg.IsExpired || pkg.DaysLeft == nil {
			continue
		}
		if best == nil || *pkg.DaysLeft < *best.DaysLeft {
			best = pkg
		}
	}
	if best == nil {
		return nil
	}
	return &EarliestExpiring{
		Name:         best.Name,
		PackageCode:  best.PackageCode,
		Remain:       best.Remain,
		CycleEndTime: best.CycleEndTime,
		DaysLeft:     *best.DaysLeft,
	}
}

func sortCreditPackages(packages []CreditPackage) {
	sort.SliceStable(packages, func(i, j int) bool {
		left, right := packages[i], packages[j]
		leftInUse, rightInUse := 0, 0
		if !left.InUsage {
			leftInUse = 1
		}
		if !right.InUsage {
			rightInUse = 1
		}
		if leftInUse != rightInUse {
			return leftInUse < rightInUse
		}
		leftExpired, rightExpired := 0, 0
		if left.IsExpired {
			leftExpired = 1
		}
		if right.IsExpired {
			rightExpired = 1
		}
		if leftExpired != rightExpired {
			return leftExpired < rightExpired
		}
		leftDays := creditSortDays(left)
		rightDays := creditSortDays(right)
		if leftDays != rightDays {
			return leftDays < rightDays
		}
		return left.Remain > right.Remain
	})
}

func creditSortDays(pkg CreditPackage) float64 {
	if pkg.DaysLeft == nil {
		return 99999
	}
	days := *pkg.DaysLeft
	if days < 0 {
		return 99999 + math.Abs(days)
	}
	return days
}

func parseCreditPackage(raw map[string]any, now time.Time) CreditPackage {
	name := creditFieldText(raw["PackageName"])
	if name == "" {
		name = "Package"
	}
	size := creditPackageNumber(raw,
		"CycleCapacitySizePrecise", "CycleCapacitySize",
		"CapacitySizePrecise", "CapacitySize")
	remain := creditPackageNumber(raw,
		"CycleCapacityRemainPrecise", "CycleCapacityRemain",
		"CapacityRemainPrecise", "CapacityRemain")
	used := creditPackageNumber(raw,
		"CycleCapacityUsedPrecise", "CycleCapacityUsed",
		"CapacityUsedPrecise", "CapacityUsed")
	if size > 0 && used <= 0 && remain <= size {
		used = math.Max(0, size-remain)
	} else if size > 0 && remain <= 0 && used < size {
		remain = math.Max(0, size-used)
	}

	grantReason := ""
	if attributes, ok := anyList(raw["AccountAttributes"]); ok {
		for _, item := range attributes {
			attribute, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if creditText(attribute["Key"]) == "grantReason" {
				grantReason = creditFieldText(attribute["Value"])
				break
			}
		}
	}

	createTime := creditPackageCreateTime(raw["CreateTime"])
	endTime := creditFieldText(raw["CycleEndTime"])
	if endTime == "" {
		endTime = creditFieldText(raw["ExpiredTime"])
	}
	expireTime := deductionEndText(raw)
	if expireTime == "" {
		expireTime = endTime
	}
	daysLeft, noExpiry, isExpired := creditPackageExpiry(expireTime, now)

	inUsage := creditTruthy(raw["InUsage"])
	if !inUsage && !isExpired && remain > 0 && used > 0 {
		inUsage = true
	}
	autoRenew := creditTruthy(raw["AutoRenewFlag"]) || creditTruthy(raw["SupportAutoRenew"])
	unit := creditFieldText(raw["CapacityUnit"])
	if unit == "" {
		unit = "credits"
	}
	status := creditStatusValue(raw["Status"])

	return CreditPackage{
		Name:           name,
		PackageCode:    creditFieldText(raw["PackageCode"]),
		ProductName:    creditFieldText(raw["ProductName"]),
		SubProductName: creditFieldText(raw["SubProductName"]),
		GrantReason:    grantReason,
		ResourceID:     creditFieldText(raw["ResourceId"]),
		DealName:       creditFieldText(raw["DealName"]),
		CreateTime:     createTime,
		Remain:         roundCredit(remain),
		Used:           roundCredit(used),
		Size:           roundCredit(size),
		Unit:           unit,
		InUsage:        inUsage,
		AutoRenew:      autoRenew,
		CycleStartTime: creditFieldText(raw["CycleStartTime"]),
		CycleEndTime:   endTime,
		ExpireTime:     expireTime,
		NoExpiry:       noExpiry,
		DaysLeft:       daysLeft,
		IsExpired:      isExpired,
		Status:         status,
	}
}

// creditStatusValue preserves v1's raw Status: numeric statuses become ints
// (the shape every current upstream sends) while hand-edited/string values are
// kept as-is instead of being silently coerced to 0.
func creditStatusValue(value any) any {
	if value == nil {
		return 0
	}
	switch value.(type) {
	case float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		if number, ok := numericValue(value); ok {
			return int(number)
		}
	}
	return value
}

func creditPackageNumber(raw map[string]any, keys ...string) float64 {
	for _, key := range keys {
		value, exists := raw[key]
		if !exists || value == nil {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		if number, ok := numericValue(value); ok {
			return number
		}
	}
	return 0
}

func deductionEndText(raw map[string]any) string {
	value, exists := raw["DeductionEndTime"]
	if !exists || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" || text == "0" {
			return ""
		}
	}
	if number, ok := numericValue(value); ok && number == 0 {
		return ""
	}
	stamp, ok := numericValue(value)
	if !ok {
		return ""
	}
	if stamp > 1e11 {
		stamp /= 1000
	}
	seconds := int64(stamp)
	nanos := int64((stamp - float64(seconds)) * 1e9)
	return time.Unix(seconds, nanos).In(time.Local).Format(creditsTimeLayout)
}

func creditPackageCreateTime(value any) string {
	if value == nil || !creditTruthy(value) {
		return ""
	}
	if number, ok := numericValue(value); ok {
		if number > 1e11 {
			number /= 1000
		}
		seconds := int64(number)
		nanos := int64((number - float64(seconds)) * 1e9)
		return time.Unix(seconds, nanos).In(time.Local).Format(creditsTimeLayout)
	}
	return creditText(value)
}

func creditPackageExpiry(expire string, now time.Time) (*float64, bool, bool) {
	if strings.TrimSpace(expire) == "" {
		return nil, false, false
	}
	clean := strings.ReplaceAll(expire, "T", " ")
	if len(clean) > 19 {
		clean = clean[:19]
	}
	parsed, err := time.ParseInLocation(creditsTimeLayout, clean, time.Local)
	if err != nil {
		return nil, false, false
	}
	diff := parsed.Sub(now).Seconds()
	if diff > float64(expirySentinelDays*86400) {
		return nil, true, false
	}
	days := math.Round(diff/86400*10) / 10
	return &days, false, diff < 0
}

func creditSummaryTotals(packages []any) (remain, used, size float64) {
	for _, item := range packages {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		size += creditNumber(raw["CycleTotalCapacity"])
		remain += creditNumber(raw["CycleRemainCapacity"])
		used += creditNumber(raw["CycleUsedCapacity"])
	}
	return remain, used, size
}

func creditAccountPaidKnown(account *Account) (bool, bool) {
	if account == nil {
		return false, false
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.paidKnown {
		return account.paidUser, true
	}
	return false, false
}

func (s *creditsSession) previousCheckin() *CheckinStatus {
	if s == nil || s.account == nil {
		return nil
	}
	s.account.mu.RLock()
	defer s.account.mu.RUnlock()
	if s.account.Credits == nil {
		return nil
	}
	return cloneCheckin(s.account.Credits.Checkin)
}

func (s *creditsSession) paidKnown() (bool, bool) {
	if s == nil {
		return false, false
	}
	return creditAccountPaidKnown(s.account)
}

func (p *Provider) fetchCNCredits(ctx context.Context, session *creditsSession) (*CreditSnapshot, map[string]any, error) {
	now := p.creditsNowTime()
	base := p.creditsBillingBase()
	status, payload, err := session.postJSON(ctx, creditEndpoint(base, creditsSummaryPath), []byte("{}"), true, creditsBillingTimeout)
	if err != nil {
		return nil, nil, err
	}
	if status < 200 || status >= 300 {
		return nil, nil, creditHTTPError(status, payload)
	}
	if err := creditCodeError(payload); err != nil {
		return nil, nil, err
	}
	data := creditObject(payload["data"])
	enterprise := creditsUsesEnterprise(session.state)

	var tier map[string]any
	if !enterprise {
		tier = tierFromCreditSummary(data, now)
	}
	paid := false
	if tier != nil {
		paid, _ = creditBool(tier["paid"])
	} else if known, value := session.paidKnown(); known {
		paid = value
	} else {
		paid = creditTruthy(data["IsPaidUser"])
	}

	var summaryPackages []any
	if packages, ok := anyList(data["Packages"]); ok {
		summaryPackages = packages
	}
	packageCodes := make([]string, 0, len(summaryPackages))
	for _, item := range summaryPackages {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if code := creditText(raw["PackageCode"]); code != "" {
			packageCodes = append(packageCodes, code)
		}
	}

	freeAccounts := []any{}
	if len(packageCodes) > 0 {
		body, _ := json.Marshal(map[string]any{
			"PackageCodes": packageCodes,
			"PageNumber":   1,
			"PageSize":     200,
			"Status":       []int{0},
		})
		freeStatus, freePayload, freeErr := session.postJSON(ctx,
			creditEndpoint(base, creditsFreePackagesPath), body, false, creditsBillingTimeout)
		if freeErr == nil && freeStatus >= 200 && freeStatus < 300 {
			if accounts, ok := anyList(creditObject(freePayload["data"])["Accounts"]); ok {
				freeAccounts = accounts
			}
		}
	}

	paidAccounts := []any{}
	if len(packageCodes) > 0 {
		body, _ := json.Marshal(map[string]any{
			"PackageCodes":  packageCodes,
			"PageNumber":    1,
			"PageSize":      200,
			"Status":        []int{0, 3},
			"NeedRenewInfo": true,
		})
		paidStatus, paidPayload, paidErr := session.postJSON(ctx,
			creditEndpoint(base, creditsPaidPackagesPath), body, false, creditsBillingTimeout)
		if paidErr == nil && paidStatus >= 200 && paidStatus < 300 {
			if accounts, ok := anyList(creditObject(paidPayload["data"])["Accounts"]); ok {
				paidAccounts = accounts
			}
		}
	}

	var checkin *CheckinStatus
	checkinStatus, checkinPayload, checkinErr := session.postJSON(ctx,
		creditEndpoint(p.creditsCheckinBase(), creditsCheckinPath), []byte("{}"), false, creditsCheckinTimeout)
	if checkinErr == nil && checkinStatus >= 200 && checkinStatus < 300 {
		checkinData := creditObject(checkinPayload["data"])
		if len(checkinData) > 0 {
			checkin = &CheckinStatus{
				TodayCheckedIn: creditTruthy(checkinData["today_checked_in"]),
				StreakDays:     int(creditNumber(checkinData["streak_days"])),
				DailyCredit:    creditNumber(checkinData["daily_credit"]),
				TodayCredit:    creditNumber(checkinData["today_credit"]),
				Active:         creditTruthyDefault(checkinData["active"], true),
			}
		}
	}

	packages := make([]CreditPackage, 0, len(freeAccounts)+len(paidAccounts))
	seen := make(map[string]bool)
	for _, item := range append(append([]any{}, freeAccounts...), paidAccounts...) {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if accountID := creditText(raw["AccountId"]); accountID != "" {
			if seen[accountID] {
				continue
			}
			seen[accountID] = true
		}
		packages = append(packages, parseCreditPackage(raw, now))
	}

	snapshot := newCreditSnapshot(packages, paid, checkin, now)
	// v1 uses the summary totals only when no concrete package rows exist.
	if len(packages) == 0 && len(summaryPackages) > 0 {
		summaryRemain, summaryUsed, summarySize := creditSummaryTotals(summaryPackages)
		snapshot.Remain = roundCredit(snapshot.Remain + summaryRemain)
		snapshot.Used = roundCredit(snapshot.Used + summaryUsed)
		snapshot.Size = roundCredit(snapshot.Size + summarySize)
		updateCreditPercentages(snapshot)
	}
	return snapshot, tier, nil
}

func (p *Provider) refreshIntlTierIfDue(ctx context.Context, session *creditsSession) map[string]any {
	if p == nil || session == nil || p.realm != realmIntl || session.state.apiKey ||
		creditsUsesEnterprise(session.state) {
		return nil
	}
	now := p.creditsNowTime()
	if creditTierFresh(session.account, now) {
		return nil
	}
	status, payload, err := session.postJSON(ctx,
		creditEndpoint(p.creditsBillingBase(), creditsSummaryPath),
		[]byte("{}"), false, creditsBillingTimeout)
	if err != nil || status < 200 || status >= 300 || creditCodeError(payload) != nil {
		return nil
	}
	return tierFromCreditSummary(creditObject(payload["data"]), now)
}

func creditTierFresh(account *Account, now time.Time) bool {
	if account == nil {
		return false
	}
	account.mu.RLock()
	raw := append([]byte(nil), account.Auth.Tier...)
	account.mu.RUnlock()
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var tier map[string]any
	if err := json.Unmarshal(raw, &tier); err != nil {
		return false
	}
	checked, ok := numericValue(tier["checked_at"])
	if !ok || checked <= 0 {
		return false
	}
	checkedAt := time.Unix(int64(checked), 0)
	return now.Sub(checkedAt) < creditsTierTTL
}

func tierFromCreditSummary(data map[string]any, now time.Time) map[string]any {
	if data == nil {
		return nil
	}
	if _, exists := data["IsPaidUser"]; !exists {
		return nil
	}
	return map[string]any{
		"paid":            creditTruthy(data["IsPaidUser"]),
		"package":         creditText(data["SubscriptionPackageName"]),
		"package_code":    creditText(data["SubscriptionPackageCode"]),
		"protected_price": creditTruthy(data["IsProtectedPriceUser"]),
		"trial":           int(creditNumber(data["ProTrialStatus"])),
		"checked_at":      float64(now.UnixNano()) / float64(time.Second),
		"source":          "billing",
	}
}

func (p *Provider) fetchFallbackCredits(ctx context.Context, session *creditsSession) (*CreditSnapshot, error) {
	now := p.creditsNowTime()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(creditsTimeLayout),
		"PackageEndTimeRangeEnd":   "2036-01-01 00:00:00",
	})
	status, payload, err := session.postJSON(ctx,
		creditEndpoint(p.creditsBillingBase(), creditsGetUserResourcePath), body, true, creditsFallbackTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, creditHTTPError(status, payload)
	}
	data := creditObject(payload["data"])
	response := creditObject(data["Response"])
	inner := creditObject(response["Data"])
	accounts, _ := anyList(inner["Accounts"])
	if len(accounts) == 0 {
		if direct, ok := anyList(data["Accounts"]); ok {
			accounts = direct
		}
	}
	packages := make([]CreditPackage, 0, len(accounts))
	for _, item := range accounts {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		packages = append(packages, parseCreditPackage(raw, now))
	}
	paid := false
	if known, value := session.paidKnown(); known {
		paid = value
	}
	return newCreditSnapshot(packages, paid, session.previousCheckin(), now), nil
}

func (p *Provider) fetchEnterpriseCredits(ctx context.Context, session *creditsSession) (*CreditSnapshot, error) {
	now := p.creditsNowTime()
	path := creditsEnterprisePath
	if session.state.apiKey {
		path = creditsEnterprisePathV2
	}
	status, payload, err := session.postJSON(ctx,
		creditEndpoint(p.creditsBillingBase(), path), []byte("{}"), true, creditsBillingTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, creditHTTPError(status, payload)
	}
	data := creditObject(payload["data"])
	used := roundCredit(creditNumber(data["credit"]))
	size := roundCredit(creditNumber(data["limitNum"]))
	remain := 0.0
	if size > 0 {
		remain = roundCredit(math.Max(0, size-used))
	}
	cycleStart := creditText(data["cycleStartTime"])
	cycleEnd := creditText(data["cycleEndTime"])
	if cycleEnd == "" {
		cycleEnd = creditText(data["cycleResetTime"])
	}

	packages := []CreditPackage{}
	if size > 0 {
		packages = append(packages, CreditPackage{
			Name:           "企业额度",
			PackageCode:    "enterprise",
			GrantReason:    "企业空间发放",
			CreateTime:     cycleStart,
			Remain:         remain,
			Used:           used,
			Size:           size,
			Unit:           "credits",
			InUsage:        used > 0,
			AutoRenew:      true,
			CycleStartTime: cycleStart,
			CycleEndTime:   cycleEnd,
			NoExpiry:       true,
			Status:         0,
		})
	}
	snapshot := newCreditSnapshot(packages, true, nil, now)
	snapshot.IsEnterprise = true
	snapshot.EarliestExpiring = nil
	return snapshot, nil
}
