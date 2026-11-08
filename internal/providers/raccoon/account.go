package raccoon

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// accountRealm is the v1 wire value for every Raccoon account view.
	accountRealm = "raccoon"

	// maxNoteRunes mirrors v1's note[:200] truncation.
	maxNoteRunes = 200
)

// CreditsSnapshot is the provider-private public credit snapshot. Task 12 owns
// fetching; this minimal shape carries the v1 view fields until then.
type CreditsSnapshot struct {
	Available       any              `json:"available"`
	Remain          any              `json:"remain"`
	Amount          any              `json:"amount"`
	Used            any              `json:"used"`
	Daily           any              `json:"daily"`
	Monthly         any              `json:"monthly"`
	Reward          any              `json:"reward"`
	Topup           any              `json:"topup"`
	EnterpriseTopup any              `json:"enterprise_topup"`
	Total           any              `json:"total"`
	TodayUsed       any              `json:"today_used"`
	WeekUsed        any              `json:"week_used"`
	Unit            any              `json:"unit"`
	FetchedAt       any              `json:"fetched_at"`
	Recent          []map[string]any `json:"recent"`
}

// PointsTaskState is one point task's public key/status pair.
type PointsTaskState struct {
	Key    string `json:"key"`
	Status string `json:"status"`
}

// PointsInvite is the non-sensitive invite summary. The invite code itself is
// never part of this type.
type PointsInvite struct {
	GrantedPoints           any `json:"granted_points"`
	RegisterRewardPoints    any `json:"register_reward_points"`
	RegisteredInviteeCount  any `json:"registered_invitee_count"`
	RemainingRewardInvitees any `json:"remaining_reward_invitees"`
}

// PointsTasks is the provider-private public task snapshot. Task 12 owns
// fetching and the invite-code stripping rules.
type PointsTasks struct {
	NewUser   []PointsTaskState `json:"new_user"`
	Invitee   []PointsTaskState `json:"invitee"`
	Invite    PointsInvite      `json:"invite"`
	FetchedAt float64           `json:"fetched_at"`
}

// Account is one Raccoon account's credentials, generic state, provider-owned
// snapshots, and runtime scheduler fields.
//
// Auth and State are the v1-shaped persistent documents. The guard fields are
// shared settings projected per account by the pool; they are not persisted.
type Account struct {
	Auth        Auth
	State       accountstate.State
	Path        string
	Credits     *CreditsSnapshot
	PointsTasks *PointsTasks
	Models      []providers.Model

	ReserveCredits       int64
	DailyCreditLimit     int64
	DailyTokenLimit      int64
	ModelDailyTokenLimit int64
	DailyCreditsToday    *float64
	DailyTokensToday     *int64
	ModelDailyTokens     map[string]int64

	// proxy_slot is persisted in the auth document; proxy is the currently
	// resolved URL and is never exported raw through View.
	proxySlot string
	proxy     string

	// pool is the owner, used to remap UID-keyed runtime data after a refresh
	// rotates the JWT subject. It is nil for standalone test accounts.
	pool *Pool

	mu               sync.Mutex
	activeConcurrent int
	maxConcurrent    int
	lastUsed         time.Time
}

// AccountView is the v1-compatible public account row. It intentionally
// contains no credentials and no raw proxy URL.
type AccountView struct {
	Name               string           `json:"name"`
	UID                string           `json:"uid"`
	Nickname           string           `json:"nickname"`
	Realm              string           `json:"realm"`
	Disabled           bool             `json:"disabled"`
	Cooling            bool             `json:"cooling"`
	Until              string           `json:"until"`
	ErrCount           int              `json:"err_count"`
	Reason             string           `json:"reason"`
	LastError          string           `json:"last_error"`
	ActiveConcurrent   int              `json:"active_concurrent"`
	MaxConcurrent      int              `json:"max_concurrent"`
	TokenRemaining     string           `json:"token_remaining"`
	ExpiresAt          string           `json:"expires_at"`
	Models             int              `json:"models"`
	Credits            *CreditsSnapshot `json:"credits"`
	PointsTasks        *PointsTasks     `json:"points_tasks"`
	Guard              string           `json:"guard"`
	ReserveCredits     int64            `json:"reserve_credits"`
	ReserveBlocked     bool             `json:"reserve_blocked"`
	DailyTokenLimit    int64            `json:"daily_token_limit"`
	DailyTokensToday   *int64           `json:"daily_tokens_today"`
	DailyLimitBlocked  bool             `json:"daily_limit_blocked"`
	DailyCreditLimit   int64            `json:"daily_credit_limit"`
	DailyCreditsToday  *float64         `json:"daily_credits_today"`
	CreditLimitReached bool             `json:"credit_limit_reached"`
	ProxySlot          string           `json:"proxy_slot"`
	Proxy              string           `json:"proxy"`
	Note               string           `json:"note"`
	TasksEnabled       bool             `json:"tasks_enabled"`
}

// View returns the public v1-compatible snapshot. It redacts known credential
// values from reason/last_error and masks proxy userinfo.
func (a *Account) View() AccountView {
	return a.viewAt(time.Now())
}

// viewAt is View with an injectable clock for tests and deterministic callers.
func (a *Account) viewAt(now time.Time) AccountView {
	if a == nil {
		return AccountView{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	uid := strings.TrimSpace(a.Auth.UID)
	if uid == "" {
		uid = strings.TrimSpace(a.State.UID)
	}
	nickname := strings.TrimSpace(a.Auth.OfficeOrgName)
	if nickname == "" {
		nickname = uid
	}
	reason := a.State.Reason
	if reason == "" {
		reason = a.State.LastError
	}
	reason = secrets.Redact(reason, authSecretValues(a.Auth)...)
	lastError := secrets.Redact(a.State.LastError, authSecretValues(a.Auth)...)

	disabled := a.State.Disabled || a.State.ManualDisabled
	cooling := !disabled && now.Before(a.State.CoolUntil)
	until := ""
	if cooling && !a.State.CoolUntil.IsZero() {
		until = formatWireTime(a.State.CoolUntil)
	}

	expiry := effectiveExpiry(a.Auth)
	expiresAt := ""
	remaining := int64(0)
	if !expiry.IsZero() {
		expiresAt = formatWireTime(expiry)
		remaining = int64(expiry.Sub(now).Seconds())
		if remaining < 0 {
			remaining = 0
		}
	}

	credits := publicCredits(a.Credits)
	pointsTasks := publicPointsTasks(a.PointsTasks)

	view := AccountView{
		Name:               uid,
		UID:                uid,
		Nickname:           nickname,
		Realm:              accountRealm,
		Disabled:           disabled,
		Cooling:            cooling,
		Until:              until,
		ErrCount:           a.State.ErrCount,
		Reason:             reason,
		LastError:          lastError,
		ActiveConcurrent:   a.activeConcurrent,
		MaxConcurrent:      normalizedMaxConcurrent(a.maxConcurrent),
		TokenRemaining:     formatTokenRemaining(remaining),
		ExpiresAt:          expiresAt,
		Models:             len(a.Models),
		Credits:            credits,
		PointsTasks:        pointsTasks,
		Guard:              a.guardBlockedLocked(""),
		ReserveCredits:     a.ReserveCredits,
		ReserveBlocked:     a.reserveBlockedLocked(),
		DailyTokenLimit:    a.DailyTokenLimit,
		DailyTokensToday:   copyInt64Pointer(a.DailyTokensToday),
		DailyLimitBlocked:  a.dailyLimitBlockedLocked(),
		DailyCreditLimit:   a.DailyCreditLimit,
		DailyCreditsToday:  copyFloat64Pointer(a.DailyCreditsToday),
		CreditLimitReached: a.creditLimitReachedLocked(),
		ProxySlot:          a.proxySlot,
		Proxy:              maskProxyURL(a.proxy),
		Note:               a.State.Note,
		TasksEnabled:       a.State.TasksEnabled,
	}
	return view
}

// Healthy reports whether the account may serve a request at now. Manual
// disable is checked directly as well as through Disabled so an inconsistent
// persisted pair cannot re-enable an account. Expired tokens stay selectable
// so TokenManager.Ensure can refresh them.
func (a *Account) Healthy(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthyLocked(now)
}

func (a *Account) healthyLocked(now time.Time) bool {
	return !a.State.Disabled && !a.State.ManualDisabled && !a.State.CoolUntil.After(now)
}

// Ready reports whether the account is healthy and every active guard allows
// model.
func (a *Account) Ready(now time.Time, model string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthyLocked(now) && a.guardBlockedLocked(model) == ""
}

// ReserveBlocked reports the v1 low-credit guard. An unknown balance never
// blocks.
func (a *Account) ReserveBlocked() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserveBlockedLocked()
}

func (a *Account) reserveBlockedLocked() bool {
	if a.ReserveCredits <= 0 || a.Credits == nil {
		return false
	}
	remain, ok := numericValue(a.Credits.Remain)
	if !ok {
		return false
	}
	return int64(remain) <= a.ReserveCredits
}

// DailyLimitBlocked reports whether today's token usage reached the daily
// token limit.
func (a *Account) DailyLimitBlocked() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dailyLimitBlockedLocked()
}

func (a *Account) dailyLimitBlockedLocked() bool {
	return a.DailyTokenLimit > 0 &&
		a.DailyTokensToday != nil &&
		*a.DailyTokensToday >= a.DailyTokenLimit
}

// ModelDailyLimitBlocked reports whether model's daily token usage reached
// the per-model limit.
func (a *Account) ModelDailyLimitBlocked(model string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.modelDailyLimitBlockedLocked(model)
}

func (a *Account) modelDailyLimitBlockedLocked(model string) bool {
	model = strings.TrimSpace(model)
	if a.ModelDailyTokenLimit <= 0 || model == "" {
		return false
	}
	used, ok := a.ModelDailyTokens[model]
	if !ok {
		return false
	}
	return used >= a.ModelDailyTokenLimit
}

// CreditLimitReached reports whether today's credit usage reached the daily
// credit limit.
func (a *Account) CreditLimitReached() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.creditLimitReachedLocked()
}

func (a *Account) creditLimitReachedLocked() bool {
	return a.DailyCreditLimit > 0 &&
		a.DailyCreditsToday != nil &&
		*a.DailyCreditsToday >= float64(a.DailyCreditLimit)
}

// CreditLimitBlocked mirrors v1: Raccoon has no free-model exemption, so a
// reached daily credit cap blocks every model.
func (a *Account) CreditLimitBlocked(model string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.creditLimitReachedLocked()
}

// GuardBlocked returns the first active guard name ("reserve", "daily_token",
// "model_daily_token", "daily_credit") or "".
func (a *Account) GuardBlocked(model string) string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.guardBlockedLocked(model)
}

func (a *Account) guardBlockedLocked(model string) string {
	if a.reserveBlockedLocked() {
		return "reserve"
	}
	if a.dailyLimitBlockedLocked() {
		return "daily_token"
	}
	if a.modelDailyLimitBlockedLocked(model) {
		return "model_daily_token"
	}
	if a.creditLimitReachedLocked() {
		return "daily_credit"
	}
	return ""
}

// HasModel mirrors v1: an unknown/empty catalog or empty requested model is
// accepted, otherwise the model id must be present.
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
	return a.hasModelLocked(model)
}

func (a *Account) hasModelLocked(model string) bool {
	mid := strings.ToLower(strings.TrimSpace(model))
	if mid == "" {
		return true
	}
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

// ExpiresAt returns the effective access-token expiry (JWT exp first).
func (a *Account) ExpiresAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return effectiveExpiry(a.Auth)
}

// TokenRemaining returns the non-negative time until ExpiresAt, or zero when
// the expiry is unknown.
func (a *Account) TokenRemaining(now time.Time) time.Duration {
	expiry := a.ExpiresAt()
	if expiry.IsZero() || !expiry.After(now) {
		return 0
	}
	return expiry.Sub(now)
}

// ProxySlot returns the persisted proxy slot id.
func (a *Account) ProxySlot() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxySlot
}

// authSnapshot returns a copy of the credentials for a token operation. The
// Auth value's extra-field map is immutable after load.
func (a *Account) authSnapshot() Auth {
	if a == nil {
		return Auth{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Auth
}

// authPathAndUID returns the refresh path and embedded UID snapshot.
func (a *Account) authPathAndUID() (string, string) {
	if a == nil {
		return "", ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.Path), strings.TrimSpace(a.Auth.UID)
}

// refreshDue reports whether the access token is inside the v1 refresh window.
func (a *Account) refreshDue(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return tokenExpiringSoon(a.Auth, now)
}

// tokenIsExpired reports whether the access token has expired.
func (a *Account) tokenIsExpired(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return tokenExpired(a.Auth, now)
}

// setRefreshedAuth publishes a successful refresh and clears non-manual
// disable state. It returns whether persistent state changed.
func (a *Account) setRefreshedAuth(updated Auth) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	oldUID := strings.TrimSpace(a.State.UID)
	if oldUID == "" {
		oldUID = strings.TrimSpace(a.Auth.UID)
	}
	a.Auth = updated
	newUID := strings.TrimSpace(updated.UID)
	if newUID == "" {
		newUID = oldUID
	}
	a.State.UID = newUID
	uidChanged := oldUID != "" && newUID != "" && oldUID != newUID

	stateChanged := uidChanged
	if !a.State.ManualDisabled {
		if a.State.Disabled || a.State.Reason != "" || !a.State.CoolUntil.IsZero() {
			a.State.Disabled = false
			a.State.Reason = ""
			a.State.CoolUntil = time.Time{}
			stateChanged = true
		}
	}
	owner := a.pool
	a.mu.Unlock()

	if owner != nil && uidChanged {
		owner.renameAffinity(oldUID, newUID)
	}
	return stateChanged
}

// markAuthFailure marks a terminal auth failure unless the account is manually
// disabled. It returns whether persistent state changed.
func (a *Account) markAuthFailure(message string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.State.ManualDisabled {
		return false
	}
	a.State.Disabled = true
	a.State.Reason = message
	a.State.CoolUntil = time.Time{}
	return true
}

// hasSelectionState snapshots the scheduler fields used by Pick.
func (a *Account) hasSelectionState() (uid string, active, max int, lastUsed time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	uid = strings.TrimSpace(a.Auth.UID)
	if uid == "" {
		uid = strings.TrimSpace(a.State.UID)
	}
	return uid, a.activeConcurrent, normalizedMaxConcurrent(a.maxConcurrent), a.lastUsed
}

func (a *Account) setMaxConcurrent(max int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.maxConcurrent = normalizedMaxConcurrent(max)
	a.State.MaxConcurrent = a.maxConcurrent
	a.mu.Unlock()
}

func normalizedMaxConcurrent(max int) int {
	if max < 1 {
		return 1
	}
	return max
}

func copyInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// publicCredits copies only the v1 public keys and guarantees recent is a list,
// never null.
func publicCredits(raw *CreditsSnapshot) *CreditsSnapshot {
	if raw == nil {
		return nil
	}
	copy := *raw
	copy.Recent = append([]map[string]any{}, raw.Recent...)
	return &copy
}

// publicPointsTasks copies the public task snapshot and guarantees list
// fields, never null.
func publicPointsTasks(raw *PointsTasks) *PointsTasks {
	if raw == nil {
		return nil
	}
	copy := *raw
	copy.NewUser = append([]PointsTaskState{}, raw.NewUser...)
	copy.Invitee = append([]PointsTaskState{}, raw.Invitee...)
	// v1's _as_points maps missing invite counters to 0, never null.
	if copy.Invite.GrantedPoints == nil {
		copy.Invite.GrantedPoints = 0
	}
	if copy.Invite.RegisterRewardPoints == nil {
		copy.Invite.RegisterRewardPoints = 0
	}
	if copy.Invite.RegisteredInviteeCount == nil {
		copy.Invite.RegisteredInviteeCount = 0
	}
	if copy.Invite.RemainingRewardInvitees == nil {
		copy.Invite.RemainingRewardInvitees = 0
	}
	return &copy
}

// authSecretValues lists the credential values v1 knows how to redact,
// including credential-shaped extras preserved in the auth document.
func authSecretValues(auth Auth) []string {
	values := make([]string, 0, 4)
	if auth.AccessToken != "" {
		values = append(values, auth.AccessToken)
	}
	if auth.RefreshToken != "" {
		values = append(values, auth.RefreshToken)
	}
	for _, key := range []string{"authorization_code", "code"} {
		raw, ok := auth.extraFields[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil && value != "" {
			values = append(values, value)
		}
	}
	return values
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
func formatTokenRemaining(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// formatWireTime mirrors v1's localtime string with no timezone suffix.
func formatWireTime(value time.Time) string {
	return value.In(time.Local).Format("2006-01-02T15:04:05")
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
