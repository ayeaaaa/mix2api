package codearts

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// Model is one CodeArts catalog entry or seed row. The v1 wire shape is an open
// map, so the provider reuses the shared providers.Model alias.
type Model = providers.Model

// CheckinState is the last check-in result stored in state.json. It stays a
// string-keyed map because v1 persists arbitrary result fields and Task 7 only
// adds helpers around the known "ok" and "checked_at" keys.
type CheckinState map[string]any

// CreditTotal is one credit bucket from the CodeArts account-info response.
// Values stay float64 because v1 reports fractional package credits.
type CreditTotal struct {
	Amount   float64 `json:"amount"`
	Used     float64 `json:"used"`
	Remain   float64 `json:"remain"`
	Expiring float64 `json:"expiring"`
}

// AccountCredits groups the v1 statistics/plugin credit buckets. Every bucket
// is optional because upstream may omit it.
type AccountCredits struct {
	Total    *CreditTotal `json:"total,omitempty"`
	Basic    *CreditTotal `json:"basic,omitempty"`
	OnDemand *CreditTotal `json:"on_demand,omitempty"`
	Bonus    *CreditTotal `json:"bonus,omitempty"`
	Unit     string       `json:"unit"`
}

// ModelInfo is one model row from the statistics/plugin model_metrics list.
// It is the v1 panel metadata, not a chat catalogue entry: id/name come from
// model_id/model_name, description from model_desc, and enabled is false only
// for a literal JSON false.
type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Enabled     bool   `json:"enabled"`
	Status      string `json:"status"`
	Luxury      bool   `json:"luxury"`
}

// AccountInfo is the provider-owned account/package snapshot used for
// scheduling and the panel. Task 7 fills it from the upstream statistics
// endpoint; Task 6 only consumes the credit totals.
type AccountInfo struct {
	IsCreditPackage bool           `json:"is_credit_package"`
	IsTokenPackage  bool           `json:"is_token_package"`
	SpecCode        string         `json:"spec_code"`
	PackageName     string         `json:"package_name"`
	PackageStatus   string         `json:"package_status"`
	StartDate       string         `json:"start_date"`
	EndDate         string         `json:"end_date"`
	Credits         AccountCredits `json:"credits"`
	Models          []ModelInfo    `json:"models"`
	FetchedAt       float64        `json:"fetched_at"`
}

// CreditsView is the v1 public credit summary. It intentionally contains only
// normalized counters and package labels.
type CreditsView struct {
	Remain   float64 `json:"remain"`
	Amount   float64 `json:"amount"`
	Used     float64 `json:"used"`
	Expiring float64 `json:"expiring"`
	Unit     string  `json:"unit"`
	Package  string  `json:"package"`
	EndDate  string  `json:"end_date"`
}

// AccountView is the v1-compatible public account row. It contains no
// credentials, no raw proxy URL, and no auth extras.
type AccountView struct {
	Name             string       `json:"name"`
	UID              string       `json:"uid"`
	Nickname         string       `json:"nickname"`
	UserName         string       `json:"user_name"`
	DefaultModel     string       `json:"default_model"`
	Credits          *CreditsView `json:"credits"`
	Package          string       `json:"package"`
	Checkin          CheckinState `json:"checkin"`
	Disabled         bool         `json:"disabled"`
	ManualDisabled   bool         `json:"manual_disabled"`
	Cooling          bool         `json:"cooling"`
	Until            string       `json:"until"`
	ErrCount         int          `json:"err_count"`
	Reason           string       `json:"reason"`
	LastError        string       `json:"last_error"`
	ActiveConcurrent int          `json:"active_concurrent"`
	MaxConcurrent    int          `json:"max_concurrent"`
	TokenRemaining   string       `json:"token_remaining"`
	ExpiresAt        string       `json:"expires_at"`
	Models           int          `json:"models"`
	ProxySlot        string       `json:"proxy_slot"`
	Proxy            string       `json:"proxy"`
	Note             string       `json:"note"`
	TasksEnabled     bool         `json:"tasks_enabled"`
}

// Account is one CodeArts account's v1 credentials, list-state entry,
// provider-owned snapshots, and runtime scheduler fields.
//
// Auth, State, Info, Checkin, Models, the model benefit map, the last model
// discovery timestamp, the proxy binding, and the concurrency counters are
// guarded by mu. Model discovery itself is serialized by modelsMu and may hold
// the network for a long time; it is never acquired while mu is held. Path and
// pool are set once while the account is being loaded/imported and are
// read-only afterwards. Lock order: modelsMu, infoMu, and checkinMu may be held
// across network I/O before briefly taking mu; mu is never held while any of
// them is acquired.
type Account struct {
	Auth    Auth
	State   AccountState
	Info    *AccountInfo
	Checkin CheckinState
	Models  []Model
	Path    string

	modelBenefit map[string]bool

	// proxySlot is persisted in the auth document; proxy is the currently
	// resolved URL and is never exported raw through View.
	proxySlot string
	proxy     string

	// pool is the owner, used to persist state mutations. It is nil for
	// standalone test accounts.
	pool *Pool

	mu               sync.Mutex
	activeConcurrent int
	maxConcurrent    int
	lastUsed         time.Time
	removed          bool

	// infoMu serializes statistics/plugin refreshes for this account. It may
	// be held across network I/O and is never acquired while mu is held.
	infoMu sync.Mutex

	// checkinMu is held across one check-in run. It is intentionally a
	// non-blocking per-account lock (TryLock) so a concurrent panel/manual
	// check-in reports the v1 "already running" result instead of claiming a
	// campaign twice. It is never acquired while mu is held.
	checkinMu sync.Mutex

	// modelsMu serializes live model discovery for this account and guards the
	// in-flight flight pointer. It may be held across network I/O and is never
	// acquired while mu is held, so a concurrent catalog read never waits on
	// upstream. modelsFetchedAt, modelsAttemptedAt, and modelsEpoch are guarded
	// by mu: fetchedAt is the last successful catalogue, attemptedAt is the last
	// discovery attempt (including failures, used for backoff), and epoch is
	// advanced by a re-import so an in-flight fetch cannot republish stale
	// models into the cleared account.
	modelsMu          sync.Mutex
	modelsFetchedAt   time.Time
	modelsAttemptedAt time.Time
	modelsEpoch       uint64
	modelsFlight      *modelFetchFlight

	// authGeneration advances only when the credential document is replaced
	// (re-import or token rotation), never for a proxy-only auth save. Refresh
	// flights capture it so a rotated response cannot overwrite newer
	// credentials.
	authGeneration uint64

	// chatModelIDs caches the accepted upstream spelling for one logical
	// model id. InferHub can report a casing mismatch as a permission error
	// (TM.000010051), so a successful alternate spelling is reused for later
	// requests on this account. Keyed by lowercase canonical id.
	chatModelIDs sync.Map
}

// Healthy reports whether the account may serve a request at now. Manual
// disable is checked directly as well as through Disabled so an inconsistent
// persisted pair cannot re-enable an account. Expired access tokens stay
// selectable so Task 3's refresh manager can renew them. A known zero-credit
// snapshot parks the account until credits are refreshed or check-in adds
// credits.
func (a *Account) Healthy(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthyLocked(now)
}

func (a *Account) healthyLocked(now time.Time) bool {
	if a.removed || a.State.Disabled || a.State.ManualDisabled {
		return false
	}
	if a.State.CoolUntil > unixSeconds(now) {
		return false
	}
	return !a.creditExhaustedLocked()
}

// CreditExhausted reports whether the last account-info snapshot says no
// credits remain. An unknown snapshot never blocks.
func (a *Account) CreditExhausted() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.creditExhaustedLocked()
}

func (a *Account) creditExhaustedLocked() bool {
	if a.Info == nil || a.Info.Credits.Total == nil {
		return false
	}
	return a.Info.Credits.Total.Remain <= 0
}

// ExpiringCreditRemain returns the aggregate expiring-credit amount from the
// last snapshot, clamped at zero.
func (a *Account) ExpiringCreditRemain() float64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expiringCreditRemainLocked()
}

func (a *Account) expiringCreditRemainLocked() float64 {
	if a.Info == nil || a.Info.Credits.Total == nil {
		return 0
	}
	value := a.Info.Credits.Total.Expiring
	if !(value > 0) {
		return 0
	}
	return value
}

// HasModel mirrors v1: an unknown/empty catalog or empty requested model is
// accepted, otherwise the model id must be present case-insensitively.
func (a *Account) HasModel(model string) bool {
	if a == nil {
		return false
	}
	mid := strings.ToLower(strings.TrimSpace(model))
	if mid == "" {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.Models) == 0 {
		return true
	}
	for _, item := range a.Models {
		id, _ := item["id"].(string)
		if strings.ToLower(strings.TrimSpace(id)) == mid {
			return true
		}
	}
	return false
}

// IsBenefit reports whether the discovered catalog marks model as a benefit
// model. Task 5 uses it for the benefit request header.
func (a *Account) IsBenefit(model string) bool {
	if a == nil {
		return false
	}
	mid := strings.ToLower(strings.TrimSpace(model))
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.modelBenefit[mid]
}

// SetModelBenefit replaces the model-benefit map with a normalized copy.
func (a *Account) SetModelBenefit(benefit map[string]bool) {
	if a == nil {
		return
	}
	copied := make(map[string]bool, len(benefit))
	for model, value := range benefit {
		copied[strings.ToLower(strings.TrimSpace(model))] = value
	}
	a.mu.Lock()
	a.modelBenefit = copied
	a.mu.Unlock()
}

// MarkCreditExhausted records that an upstream request reported zero credits.
// It updates the snapshot remain to 0 and last_error, but never clears a
// manual disable or disables the account itself. The parked state comes from
// Healthy so a later credits refresh or check-in resurrects the account.
func (a *Account) MarkCreditExhausted() error {
	if a == nil {
		return errors.New("codearts: account is nil")
	}
	now := time.Now()
	if a.pool != nil {
		now = a.pool.nowTime()
	}

	a.mu.Lock()
	if a.Info == nil {
		a.Info = &AccountInfo{}
	}
	if a.Info.Credits.Total == nil {
		a.Info.Credits.Total = &CreditTotal{}
	}
	a.Info.Credits.Total.Remain = 0
	a.Info.FetchedAt = unixSeconds(now)
	a.State.LastError = "积分已耗尽"
	a.mu.Unlock()

	if a.pool == nil {
		return nil
	}
	return a.pool.SaveState()
}

// SetInfo installs a private copy of the latest account/package snapshot.
// Callers may reuse or mutate info after the call without racing the scheduler
// or admin views. Task 7 refreshes credits through this method.
func (a *Account) SetInfo(info *AccountInfo) {
	if a == nil {
		return
	}
	copied := cloneAccountInfo(info)
	a.mu.Lock()
	a.Info = copied
	a.mu.Unlock()
}

// ClearCreditExhausted drops the last credit snapshot so a successful check-in
// can unpark the account immediately. Matching v1, the next overview/credits
// refresh repopulates the snapshot with positive credits. The snapshot is not
// part of state.json, so no state write is needed. Task 7 owns check-in
// persistence and calls this helper after a successful claim.
func (a *Account) ClearCreditExhausted() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Info = nil
	a.mu.Unlock()
}

// infoSnapshot returns a private copy of the current account-info snapshot.
func (a *Account) infoSnapshot() *AccountInfo {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneAccountInfo(a.Info)
}

// freshInfo returns the cached snapshot when it is younger than ttl. The v1
// check uses the stored fetch timestamp, so a check-in can invalidate the cache
// without discarding the last known display values.
func (a *Account) freshInfo(now time.Time, ttl time.Duration) (*AccountInfo, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Info == nil || a.Info.FetchedAt <= 0 {
		return nil, false
	}
	fetched := timeFromUnix(a.Info.FetchedAt)
	if fetched.IsZero() || now.Sub(fetched) >= ttl {
		return nil, false
	}
	return cloneAccountInfo(a.Info), true
}

// invalidateInfo expires the cached snapshot without clearing the displayed
// values. v1 resets info_fetched_at to zero after every check-in so the next
// overview performs a fresh statistics request.
func (a *Account) invalidateInfo() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.Info != nil {
		a.Info.FetchedAt = 0
	}
	a.mu.Unlock()
}

// setCheckin installs the v1 account check-in document under the account lock.
func (a *Account) setCheckin(ok bool, message string, checkedAt time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Checkin = CheckinState{
		"ok":         ok,
		"message":    message,
		"checked_at": checkedAt.In(time.Local).Format("2006-01-02T15:04:05"),
	}
	a.mu.Unlock()
}

// View returns the public v1-compatible snapshot.
func (a *Account) View() AccountView {
	if a == nil {
		return AccountView{}
	}
	now := time.Now()
	if a.pool != nil {
		now = a.pool.nowTime()
	}
	return a.viewAt(now)
}

// viewAt is View with an injectable clock for tests and deterministic callers.
func (a *Account) viewAt(now time.Time) AccountView {
	if a == nil {
		return AccountView{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	uid := a.uidLocked()
	nickname := strings.TrimSpace(a.Auth.UserName)
	if nickname == "" {
		nickname = uid
	}
	reason := a.State.Reason
	if reason == "" {
		reason = a.State.LastError
	}
	secretValues := codeartsSecretValues(a.Auth)
	reason = redactText(reason, secretValues)
	lastError := redactText(a.State.LastError, secretValues)
	note := redactText(a.State.Note, secretValues)

	disabled := a.State.Disabled || a.State.ManualDisabled
	cooling := !disabled && a.State.CoolUntil > unixSeconds(now)
	until := ""
	if cooling {
		if coolAt := timeFromUnix(a.State.CoolUntil); !coolAt.IsZero() {
			until = formatWireTime(coolAt)
		}
	}

	expiresAt := ""
	remaining := float64(0)
	if !a.Auth.Expiration.IsZero() {
		expiresAt = formatWireTime(a.Auth.Expiration)
		remaining = a.Auth.Expiration.Sub(now).Seconds()
		if remaining < 0 {
			remaining = 0
		}
	}

	checkin := sanitizeCheckin(a.checkinLocked(), secretValues)

	return AccountView{
		Name:             uid,
		UID:              uid,
		Nickname:         nickname,
		UserName:         nickname,
		DefaultModel:     "",
		Credits:          publicCredits(a.Info),
		Package:          packageName(a.Info),
		Checkin:          checkin,
		Disabled:         disabled,
		ManualDisabled:   a.State.ManualDisabled,
		Cooling:          cooling,
		Until:            until,
		ErrCount:         a.State.ErrCount,
		Reason:           reason,
		LastError:        lastError,
		ActiveConcurrent: a.activeConcurrent,
		MaxConcurrent:    normalizedMaxConcurrent(a.maxConcurrent),
		TokenRemaining:   formatTokenRemaining(remaining),
		ExpiresAt:        expiresAt,
		Models:           len(a.Models),
		ProxySlot:        a.proxySlotLocked(),
		Proxy:            maskProxyURL(a.proxy),
		Note:             note,
		TasksEnabled:     a.State.TasksEnabled,
	}
}

// ProxyURL returns the resolved raw proxy URL for outbound calls. It is never
// part of a view and must not be logged.
func (a *Account) ProxyURL() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxy
}

// ProxySlot returns the persisted proxy slot id.
func (a *Account) ProxySlot() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxySlotLocked()
}

func (a *Account) proxySlotLocked() string {
	slot := strings.TrimSpace(a.proxySlot)
	if slot == "" {
		slot = strings.TrimSpace(a.Auth.ProxySlot)
	}
	return slot
}

func (a *Account) uidLocked() string {
	uid := strings.TrimSpace(a.Auth.UserID)
	if uid == "" {
		uid = strings.TrimSpace(a.State.Name)
	}
	return uid
}

func (a *Account) currentUID() (string, bool) {
	if a == nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uidLocked(), !a.removed
}

func (a *Account) checkinLocked() CheckinState {
	if a.Checkin != nil {
		return cloneCheckin(a.Checkin)
	}
	if a.State.Checkin != nil {
		return cloneCheckin(CheckinState(a.State.Checkin))
	}
	return nil
}

// accountSelection is one account's scheduler fields captured under a single
// lock so Pick does not mix values from different moments.
type accountSelection struct {
	uid      string
	active   int
	max      int
	lastUsed time.Time
	healthy  bool
	expiring float64
	removed  bool
}

func (a *Account) selectionSnapshot(now time.Time) accountSelection {
	if a == nil {
		return accountSelection{removed: true}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return accountSelection{
		uid:      a.uidLocked(),
		active:   a.activeConcurrent,
		max:      normalizedMaxConcurrent(a.maxConcurrent),
		lastUsed: a.lastUsed,
		healthy:  a.healthyLocked(now),
		expiring: a.expiringCreditRemainLocked(),
		removed:  a.removed,
	}
}

func (a *Account) setMaxConcurrent(max int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.maxConcurrent = normalizedMaxConcurrent(max)
	a.mu.Unlock()
}

func normalizedMaxConcurrent(max int) int {
	if max < 1 {
		return 1
	}
	return max
}

func publicCredits(info *AccountInfo) *CreditsView {
	if info == nil || info.Credits.Total == nil {
		return nil
	}
	total := *info.Credits.Total
	return &CreditsView{
		Remain:   total.Remain,
		Amount:   total.Amount,
		Used:     total.Used,
		Expiring: total.Expiring,
		Unit:     "credit",
		Package:  info.PackageName,
		EndDate:  info.EndDate,
	}
}

func packageName(info *AccountInfo) string {
	if info == nil {
		return ""
	}
	return info.PackageName
}

func cloneAccountInfo(info *AccountInfo) *AccountInfo {
	if info == nil {
		return nil
	}
	copied := *info
	copied.Credits = AccountCredits{
		Total:    cloneCreditTotal(info.Credits.Total),
		Basic:    cloneCreditTotal(info.Credits.Basic),
		OnDemand: cloneCreditTotal(info.Credits.OnDemand),
		Bonus:    cloneCreditTotal(info.Credits.Bonus),
		Unit:     info.Credits.Unit,
	}
	copied.Models = append([]ModelInfo(nil), info.Models...)
	return &copied
}

func cloneCreditTotal(total *CreditTotal) *CreditTotal {
	if total == nil {
		return nil
	}
	copied := *total
	return &copied
}

func cloneCheckin(checkin CheckinState) CheckinState {
	if checkin == nil {
		return nil
	}
	copied := make(CheckinState, len(checkin))
	for key, value := range checkin {
		copied[key] = value
	}
	return copied
}

const redactedCheckinValue = "<redacted>"

var (
	checkinKeyNormalizer = strings.NewReplacer("_", "", "-", "", " ", "", ".", "")
	checkinSecretParts   = []string{
		"token", "secret", "password", "authorization", "credential",
		"apikey", "privatekey", "cookie", "verifier", "signature",
	}
)

// sanitizeCheckin returns a deep public copy of a check-in document. Values
// under token-like keys are replaced with the redaction marker, while expected
// fields (ok, checked_at, message, credits, campaigns, ...) keep their shape.
// String values elsewhere are scrubbed of known auth secrets.
func sanitizeCheckin(checkin CheckinState, secretValues []string) CheckinState {
	if checkin == nil {
		return nil
	}
	return sanitizeCheckinMap(map[string]any(checkin), secretValues)
}

func sanitizeCheckinMap(input map[string]any, secretValues []string) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		if secretLikeCheckinKey(key) {
			out[key] = redactCheckinSecretValue(value)
			continue
		}
		out[key] = sanitizeCheckinValue(value, secretValues)
	}
	return out
}

func sanitizeCheckinValue(value any, secretValues []string) any {
	switch typed := value.(type) {
	case CheckinState:
		return sanitizeCheckin(typed, secretValues)
	case map[string]any:
		return sanitizeCheckinMap(typed, secretValues)
	case map[string]string:
		out := make(map[string]string, len(typed))
		for key, item := range typed {
			if secretLikeCheckinKey(key) {
				out[key] = redactCheckinSecretValue(item).(string)
				continue
			}
			out[key] = redactText(item, secretValues)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = sanitizeCheckinValue(item, secretValues)
		}
		return out
	case []string:
		out := make([]string, len(typed))
		for index, item := range typed {
			out[index] = redactText(item, secretValues)
		}
		return out
	case string:
		return redactText(typed, secretValues)
	default:
		return value
	}
}

func secretLikeCheckinKey(key string) bool {
	normalized := checkinKeyNormalizer.Replace(strings.ToLower(strings.TrimSpace(key)))
	for _, part := range checkinSecretParts {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}

func redactCheckinSecretValue(value any) any {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok && text == "" {
		return ""
	}
	return redactedCheckinValue
}

// codeartsSecretValues lists credential values that must never surface in a
// reason, error, log, or admin view.
func codeartsSecretValues(auth Auth) []string {
	values := make([]string, 0, 6)
	for _, value := range []string{
		auth.CloudDragonToken,
		auth.AccessKeyID,
		auth.SecretAccessKey,
		auth.RefreshToken,
		auth.CodeVerifier,
	} {
		if value != "" {
			values = append(values, value)
		}
	}
	if len(auth.DPopPrivateKey) > 0 {
		var jwk struct {
			D string `json:"d"`
		}
		if err := json.Unmarshal(auth.DPopPrivateKey, &jwk); err == nil && jwk.D != "" {
			values = append(values, jwk.D)
		}
	}
	return values
}

func redactText(text string, values []string) string {
	if text == "" {
		return ""
	}
	return secrets.Redact(text, values...)
}

var proxyUserinfo = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://)[^/@\s]+@`)

// maskProxyURL hides scheme://user:password@ credentials exactly like v1.
func maskProxyURL(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	return proxyUserinfo.ReplaceAllString(text, "${1}<redacted>@")
}

// formatTokenRemaining mirrors v1 _fmt_duration: hours/minutes, no days, and
// no seconds.
func formatTokenRemaining(seconds float64) string {
	if !(seconds > 0) {
		seconds = 0
	}
	total := int64(seconds)
	hours := total / 3600
	minutes := (total % 3600) / 60
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// formatWireTime mirrors v1's localtime string with no timezone suffix.
func formatWireTime(value time.Time) string {
	return value.In(time.Local).Format("2006-01-02T15:04:05")
}

// unixSeconds converts a time.Time to v1's float Unix-seconds representation.
func unixSeconds(value time.Time) float64 {
	return float64(value.Unix()) + float64(value.Nanosecond())/float64(time.Second)
}

// timeFromUnix converts v1's float Unix seconds to a Time. Non-positive and
// non-finite values yield the zero time.
func timeFromUnix(value float64) time.Time {
	if !(value > 0) {
		return time.Time{}
	}
	seconds := int64(value)
	nanos := int64((value - float64(seconds)) * float64(time.Second))
	return time.Unix(seconds, nanos)
}

// truncateRunes truncates by characters, matching Python's note slicing.
func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
