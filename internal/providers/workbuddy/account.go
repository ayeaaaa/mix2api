package workbuddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// maxNoteRunes and maxGroupRunes mirror v1's note/group truncation.
	maxNoteRunes  = 200
	maxGroupRunes = 64

	// minWeight and maxWeight are v1's manual scheduler weight bounds. A
	// weight of 1 means "no manual preference".
	minWeight = 1
	maxWeight = 100

	// expiryWeightScale converts the floating urgency/amount score to the
	// integer weights used by the deterministic smooth weighted round-robin.
	// It matches v1's EXPIRY_WEIGHT_SCALE.
	expiryWeightScale = 10.0
)

// CreditPackage is one v1 credit package. The scheduling fields (Remain, Used,
// Size, DaysLeft, NoExpiry, IsExpired) drive expiry preference and guards; the
// display fields are the exact v1 `_parse_package_account` projection so the
// panel can render a package detail without a second shape.
type CreditPackage struct {
	Name           string   `json:"name"`
	PackageCode    string   `json:"package_code"`
	ProductName    string   `json:"product_name"`
	SubProductName string   `json:"sub_product_name"`
	GrantReason    string   `json:"grant_reason"`
	ResourceID     string   `json:"resource_id"`
	DealName       string   `json:"deal_name"`
	CreateTime     string   `json:"create_time"`
	Remain         float64  `json:"remain"`
	Used           float64  `json:"used"`
	Size           float64  `json:"size"`
	Unit           string   `json:"unit"`
	InUsage        bool     `json:"in_usage"`
	AutoRenew      bool     `json:"auto_renew"`
	CycleStartTime string   `json:"cycle_start_time"`
	CycleEndTime   string   `json:"cycle_end_time"`
	ExpireTime     string   `json:"expire_time"`
	NoExpiry       bool     `json:"no_expiry"`
	DaysLeft       *float64 `json:"days_left"`
	IsExpired      bool     `json:"is_expired"`
	Status         any      `json:"status"`
}

// EarliestExpiring is v1's compact "spend these first" package summary.
type EarliestExpiring struct {
	Name         string  `json:"name"`
	PackageCode  string  `json:"package_code"`
	Remain       float64 `json:"remain"`
	CycleEndTime string  `json:"cycle_end_time"`
	DaysLeft     float64 `json:"days_left"`
}

// CheckinStatus is the CN daily-checkin state carried by a credits snapshot.
type CheckinStatus struct {
	TodayCheckedIn bool    `json:"today_checked_in"`
	StreakDays     int     `json:"streak_days"`
	DailyCredit    float64 `json:"daily_credit"`
	TodayCredit    float64 `json:"today_credit"`
	Active         bool    `json:"active"`
}

// CreditSnapshot is the full v1-compatible balance snapshot. The aggregate
// counters and package expiry fields drive scheduling; the remainder is the
// exact v1 panel payload persisted under the account's `credits` extra.
type CreditSnapshot struct {
	Remain           float64           `json:"remain"`
	Used             float64           `json:"used"`
	Size             float64           `json:"size"`
	UsedPercent      string            `json:"used_percent"`
	RemainPercent    string            `json:"remain_percent"`
	IsPaidUser       bool              `json:"is_paid_user"`
	IsEnterprise     bool              `json:"is_enterprise,omitempty"`
	Checkin          *CheckinStatus    `json:"checkin"`
	EarliestExpiring *EarliestExpiring `json:"earliest_expiring"`
	Packages         []CreditPackage   `json:"packages"`
	UpdatedAt        float64           `json:"updated_at"`
	UpdatedISO       string            `json:"updated_iso"`
}

// ModelCooldownView is one active per-model upstream window in the public v1
// payload. Account-level cooldowns remain separate (InCooldown/CooldownFor).
type ModelCooldownView struct {
	Model     string `json:"model"`
	ExpiresAt int64  `json:"expiresAt"`
	StartedAt int64  `json:"startedAt,omitempty"`
	CappedAt  int64  `json:"cappedAt,omitempty"`
}

// AccountView is the v1-compatible public account row. It intentionally has
// no access/refresh token fields, no raw proxy URL, and no unredacted errors.
type AccountView struct {
	UID                  string              `json:"uid"`
	Nickname             string              `json:"nickname"`
	Domain               string              `json:"domain"`
	Realm                string              `json:"realm"`
	Platform             string              `json:"platform"`
	Product              string              `json:"product"`
	EnterpriseID         string              `json:"enterpriseId"`
	Enabled              bool                `json:"enabled"`
	TasksEnabled         bool                `json:"tasksEnabled"`
	Weight               int                 `json:"weight"`
	Note                 string              `json:"note"`
	Group                string              `json:"group"`
	DisabledModels       []string            `json:"disabledModels"`
	Source               string              `json:"source"`
	APIKey               bool                `json:"apiKey"`
	CredentialMask       string              `json:"credentialMask"`
	ProxySlot            string              `json:"proxySlot"`
	Proxy                string              `json:"proxy"`
	ExpiresAt            int64               `json:"expiresAt"`
	ExpiresIn            any                 `json:"expiresIn"`
	TokenExpired         bool                `json:"tokenExpired"`
	NeedRelogin          bool                `json:"needRelogin"`
	HasRefreshToken      bool                `json:"hasRefreshToken"`
	Reason               string              `json:"reason"`
	LastError            string              `json:"lastError"`
	LastErrorDetail      any                 `json:"lastErrorDetail"`
	InCooldown           bool                `json:"inCooldown"`
	CooldownFor          any                 `json:"cooldownFor"`
	ModelCooldowns       []ModelCooldownView `json:"modelCooldowns"`
	ActiveConcurrent     int                 `json:"activeConcurrent"`
	MaxConcurrent        int                 `json:"maxConcurrent"`
	AddedAt              int64               `json:"addedAt"`
	File                 string              `json:"file"`
	Credits              *CreditSnapshot     `json:"credits"`
	Tier                 any                 `json:"tier"`
	ReserveCredits       int                 `json:"reserveCredits"`
	ReserveBlocked       bool                `json:"reserveBlocked"`
	DailyTokenLimit      int                 `json:"dailyTokenLimit"`
	DailyTokensToday     *int64              `json:"dailyTokensToday"`
	DailyLimitBlocked    bool                `json:"dailyLimitBlocked"`
	DailyCreditLimit     int                 `json:"dailyCreditLimit"`
	DailyCreditsToday    *float64            `json:"dailyCreditsToday"`
	CreditLimitReached   bool                `json:"creditLimitReached"`
	ModelDailyTokenLimit int                 `json:"modelDailyTokenLimit"`
	ModelDailyTokens     map[string]int64    `json:"modelDailyTokens"`
	LastCheckin          string              `json:"lastCheckin"`
	LastDailyChat        string              `json:"lastDailyChat"`
	LastActivityReport   string              `json:"lastActivityReport"`
	CanCheckin           bool                `json:"canCheckin"`
	CanDailyChat         bool                `json:"canDailyChat"`
	CanReportActivity    bool                `json:"canReportActivity"`
}

// Account is one WorkBuddy account: its persisted v1 credential document,
// runtime-only scheduler state, provider snapshot fields, and resolved proxy.
//
// Auth is authoritative for persisted operator state (enabled, tasks, weight,
// note, group, disabledModels, tier, credits, timestamps). Group, Weight, Tier,
// and DisabledModels are normalized mirrors kept in step by the pool setters so
// scheduling does not repeatedly decode JSON. State is runtime-only: cooldown,
// error counters, and concurrency are never written back to the account file.
//
// Lock order: Pool.writeMu -> Account.mu, and Pool.mu -> Account.mu is never
// held; a caller may read Pool.mu, take Account.mu, but must release Account.mu
// before taking Pool.mu again. Account.mu is never held across file I/O.
type Account struct {
	Auth               Auth
	State              accountstate.State
	Credits            *CreditSnapshot
	DisabledModels     map[string]bool
	Group              string
	Weight             float64
	Tier               string
	ExpiringWindowDays int

	// Guard limits and today's folded usage. Provider.ApplyLimits projects
	// these from the shared settings; they are runtime-only and never written
	// to the credential file, matching v1.
	ReserveCredits       int
	DailyTokenLimit      int
	DailyTokensToday     *int64
	DailyCreditLimit     int
	DailyCreditsToday    *float64
	ModelDailyTokenLimit int
	ModelDailyTokens     map[string]int64
	FreeModels           map[string]bool

	// Models is the provider-owned per-account catalog discovered in Task 5.
	Models []providers.Model

	// Path is the loaded/written credential file. The pool updates it after
	// every save.
	Path string

	pool *Pool

	mu            sync.RWMutex
	removed       bool
	maxConcurrent int
	tokenExpired  bool
	paidUser      bool
	paidKnown     bool

	// Per-model upstream window cooldowns. They are runtime-only: v1 persisted
	// only the 6004 quota cap (Task 7 keeps the request-path window in memory),
	// while manual account disable remains the only persisted control state.
	modelCoolUntil   map[string]time.Time
	modelCoolStarted map[string]time.Time

	// taskMu serializes one account's scheduled/manual task operations
	// (check-in, daily activity, growth, Buddy/cat/streak). It is acquired
	// non-blockingly so a duplicate trigger reports "busy" instead of issuing a
	// second claim or sign-in.
	taskMu sync.Mutex
}

// UID returns the normalized account identifier.
func (a *Account) UID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.Auth.UID)
}

// Healthy reports whether the account may serve at now. Expiration is not a
// health gate: Task 3's refresh manager renews near-expiry credentials, while
// a terminal token failure is represented by tokenExpired.
func (a *Account) Healthy(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.healthyLocked(now)
}

func (a *Account) healthyLocked(now time.Time) bool {
	if a.removed || !a.Auth.Enabled || a.State.Disabled || a.tokenExpired {
		return false
	}
	if a.State.CoolUntil.After(now) {
		return false
	}
	return strings.TrimSpace(a.Auth.AccessToken) != ""
}

// modelCooling reports whether model is parked by an upstream model window.
// Callers must hold a.mu (read or write).
func (a *Account) modelCoolingLocked(model string, now time.Time) bool {
	if a == nil || model == "" || len(a.modelCoolUntil) == 0 {
		return false
	}
	until, ok := a.modelCoolUntil[strings.ToLower(strings.TrimSpace(model))]
	return ok && until.After(now)
}

// modelCooling is the lock-taking form used by tests and diagnostics.
func (a *Account) modelCooling(model string, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.modelCoolingLocked(model, now)
}

// setModelCooldownLocked records one model window. Callers hold a.mu.
func (a *Account) setModelCooldownLocked(model string, until, started time.Time) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || !until.After(started) {
		return
	}
	if a.modelCoolUntil == nil {
		a.modelCoolUntil = make(map[string]time.Time)
	}
	if a.modelCoolStarted == nil {
		a.modelCoolStarted = make(map[string]time.Time)
	}
	if existing, ok := a.modelCoolUntil[model]; !ok || until.After(existing) {
		a.modelCoolUntil[model] = until
	}
	// v1 refreshes the visible start stamp on every new model-window record.
	a.modelCoolStarted[model] = started
}

// clearModelCooldownsLocked clears one model window, or every window when
// model is empty. Callers hold a.mu.
func (a *Account) clearModelCooldownsLocked(model string) {
	if a == nil {
		return
	}
	if model == "" {
		a.modelCoolUntil = nil
		a.modelCoolStarted = nil
		return
	}
	model = strings.ToLower(strings.TrimSpace(model))
	delete(a.modelCoolUntil, model)
	delete(a.modelCoolStarted, model)
}

// TaskActive reports whether scheduled/manual tasks may use this account. It
// is independent of API dispatch, matching v1's task_active().
func (a *Account) TaskActive() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Auth.TasksEnabled
}

// DisabledFor reports whether the operator manually banned model for this
// account. Matching is exact and case-insensitive; an empty model or empty
// list never matches.
func (a *Account) DisabledFor(model string) bool {
	if a == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.DisabledModels[model] {
		return true
	}
	// Tolerate hand-constructed maps that were not normalized by the loader.
	for blocked := range a.DisabledModels {
		if strings.EqualFold(strings.TrimSpace(blocked), model) {
			return true
		}
	}
	return false
}

// SoonestExpiringDays returns the days until the earliest usable package
// expires. Packages marked no_expiry, the enterprise quota, expired packages,
// and packages with no remaining credits are skipped, matching v1.
func (a *Account) SoonestExpiringDays() (float64, bool) {
	days, _, ok := a.expiringPackage()
	return days, ok
}

// SoonestExpiringRemain returns the remaining credits of the package chosen by
// SoonestExpiringDays.
func (a *Account) SoonestExpiringRemain() (float64, bool) {
	_, remain, ok := a.expiringPackage()
	return remain, ok
}

func (a *Account) expiringPackage() (float64, float64, bool) {
	if a == nil {
		return 0, 0, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.expiringPackageLocked()
}

// expiringPackageLocked is expiringPackage for callers already holding
// Account.mu.
func (a *Account) expiringPackageLocked() (float64, float64, bool) {
	if a.Credits == nil {
		return 0, 0, false
	}
	soonest := math.Inf(1)
	remain := 0.0
	found := false
	for _, pkg := range a.Credits.Packages {
		if pkg.NoExpiry || pkg.PackageCode == "enterprise" || pkg.IsExpired {
			continue
		}
		if pkg.DaysLeft == nil || *pkg.DaysLeft < 0 || pkg.Remain <= 0 {
			continue
		}
		if *pkg.DaysLeft < soonest {
			soonest = *pkg.DaysLeft
			remain = pkg.Remain
			found = true
		}
	}
	if !found {
		return 0, 0, false
	}
	return soonest, remain, true
}

// InExpiringWindow reports whether the account should be served before
// accounts whose credits lapse later. Unknown credit data is never treated as
// urgent.
func (a *Account) InExpiringWindow() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	window := a.ExpiringWindowDays
	a.mu.RUnlock()
	if window <= 0 {
		return false
	}
	days, ok := a.SoonestExpiringDays()
	return ok && days <= float64(window)
}

// IsPaidUser reports whether the last tier/credit snapshot identifies a paid
// (PRO) account. An unknown tier is not paid, matching v1's fail-closed model
// gate.
func (a *Account) IsPaidUser() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.paidUserLocked()
}

func (a *Account) paidUserLocked() bool {
	if a.paidKnown {
		return a.paidUser
	}
	// A tier document without a paid field is unknown. A package label such as
	// "Pro" is display metadata, not evidence of entitlement; only an explicit
	// paid flag (above) or a credits snapshot may decide the gate.
	if a.Credits != nil && a.Credits.IsPaidUser {
		return true
	}
	return false
}

// ProxyURL returns the resolved raw outbound proxy. It is never part of a
// public view and must not be logged.
func (a *Account) ProxyURL() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.Auth.ProxyURL)
}

// ProxySlot returns the persisted proxy slot id.
func (a *Account) ProxySlot() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.Auth.ProxySlot)
}

// SetCredits replaces the runtime balance snapshot. Task 6 owns persistence of
// the underlying raw credits document; this setter is for refreshed runtime
// data.
func (a *Account) SetCredits(snapshot *CreditSnapshot) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.setCreditsLocked(snapshot)
	a.mu.Unlock()
}

func (a *Account) setCreditsLocked(snapshot *CreditSnapshot) {
	a.Credits = cloneCreditSnapshot(snapshot)
	if a.Credits != nil && a.Credits.IsPaidUser && !a.paidKnown && a.Tier == "" {
		// Advisory fallback only while the tier is unknown; an explicit tier
		// paid:false remains authoritative.
		a.Tier = "pro"
	}
}

// SetModels replaces the per-account model list. Task 5 owns catalog refresh.
func (a *Account) SetModels(models []providers.Model) {
	if a == nil {
		return
	}
	copied := make([]providers.Model, 0, len(models))
	for _, model := range models {
		clone := make(providers.Model, len(model))
		for key, value := range model {
			clone[key] = value
		}
		copied = append(copied, clone)
	}
	a.mu.Lock()
	a.Models = copied
	a.mu.Unlock()
}

// SetTokenExpired records the runtime terminal-auth marker used by Task 3.
func (a *Account) SetTokenExpired(expired bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.tokenExpired = expired
	a.mu.Unlock()
}

// Save persists the account's current credential document through its owning
// pool. Standalone test accounts have no pool and return an error.
func (a *Account) Save() error {
	if a == nil || a.pool == nil {
		return errors.New("workbuddy: account has no pool")
	}
	return a.pool.SaveAccount(a)
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

func (a *Account) viewAt(now time.Time) AccountView {
	if a == nil {
		return AccountView{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	uid := strings.TrimSpace(a.Auth.UID)
	if uid == "" {
		uid = strings.TrimSpace(a.State.UID)
	}
	nickname := strings.TrimSpace(a.Auth.Nickname)
	if nickname == "" {
		nickname = shortUID(uid)
	}
	secretsToRedact := accountSecretValuesLocked(a)
	reason := redactAccountText(a.State.Reason, secretsToRedact)
	if reason == "" {
		reason = redactAccountText(a.State.LastError, secretsToRedact)
	}
	lastError := redactAccountText(a.State.LastError, secretsToRedact)
	note := redactAccountText(a.Auth.Note, secretsToRedact)

	enabled := a.Auth.Enabled && !a.State.Disabled
	expiry := a.Auth.ExpiresAt
	expiryUnix := int64(0)
	if !expiry.IsZero() {
		expiryUnix = expiry.Unix()
	}
	expired := false
	if a.tokenExpired || strings.TrimSpace(a.Auth.AccessToken) == "" {
		expired = true
	} else if !expiry.IsZero() && !expiry.After(now) {
		expired = true
	}
	expiresIn := any(nil)
	if expired {
		expiresIn = "expired"
	} else if !expiry.IsZero() {
		expiresIn = humanDelta(expiry.Sub(now))
	}

	cooling := enabled && a.State.CoolUntil.After(now)
	var cooldownFor any
	if cooling {
		seconds := int(math.Round(a.State.CoolUntil.Sub(now).Seconds()))
		if seconds > 0 {
			cooldownFor = seconds
		}
	}

	disabledModels := append([]string(nil), a.Auth.DisabledModels...)
	if len(disabledModels) == 0 && len(a.DisabledModels) > 0 {
		disabledModels = make([]string, 0, len(a.DisabledModels))
		for model := range a.DisabledModels {
			disabledModels = append(disabledModels, model)
		}
		sort.Strings(disabledModels)
	}
	if disabledModels == nil {
		disabledModels = []string{}
	}

	file := ""
	if strings.TrimSpace(a.Auth.Path) != "" {
		file = filepath.Base(a.Auth.Path)
	}

	modelCooldowns := make([]ModelCooldownView, 0, len(a.modelCoolUntil))
	for model, until := range a.modelCoolUntil {
		if !until.After(now) {
			continue
		}
		entry := ModelCooldownView{
			Model:     model,
			ExpiresAt: until.Unix(),
		}
		if started, ok := a.modelCoolStarted[model]; ok && !started.IsZero() {
			entry.StartedAt = started.Unix()
		}
		modelCooldowns = append(modelCooldowns, entry)
	}
	sort.Slice(modelCooldowns, func(i, j int) bool {
		if modelCooldowns[i].ExpiresAt == modelCooldowns[j].ExpiresAt {
			return modelCooldowns[i].Model < modelCooldowns[j].Model
		}
		return modelCooldowns[i].ExpiresAt < modelCooldowns[j].ExpiresAt
	})

	return AccountView{
		UID:                  uid,
		Nickname:             nickname,
		Domain:               a.Auth.Domain,
		Realm:                a.Auth.Realm,
		Platform:             a.Auth.Platform,
		Product:              a.Auth.Product,
		EnterpriseID:         a.Auth.EnterpriseID,
		Enabled:              enabled,
		TasksEnabled:         a.Auth.TasksEnabled,
		Weight:               normalizedWeight(a.Weight),
		Note:                 note,
		Group:                a.Group,
		DisabledModels:       disabledModels,
		Source:               a.Auth.Source,
		APIKey:               a.Auth.APIKey,
		CredentialMask:       credentialMask(a.Auth),
		ProxySlot:            strings.TrimSpace(a.Auth.ProxySlot),
		Proxy:                maskProxyURL(a.Auth.ProxyURL),
		ExpiresAt:            expiryUnix,
		ExpiresIn:            expiresIn,
		TokenExpired:         expired,
		NeedRelogin:          expired,
		HasRefreshToken:      strings.TrimSpace(a.Auth.RefreshToken) != "",
		Reason:               reason,
		LastError:            lastError,
		LastErrorDetail:      nil,
		InCooldown:           cooling,
		CooldownFor:          cooldownFor,
		ModelCooldowns:       modelCooldowns,
		ActiveConcurrent:     a.State.ActiveConcurrent,
		MaxConcurrent:        a.maxConcurrent,
		AddedAt:              unixSeconds(a.Auth.AddedAt),
		File:                 file,
		Credits:              cloneCreditSnapshot(a.Credits),
		Tier:                 tierJSONValue(a.Auth.Tier),
		ReserveCredits:       a.ReserveCredits,
		ReserveBlocked:       a.reserveBlockedLocked(""),
		DailyTokenLimit:      a.DailyTokenLimit,
		DailyTokensToday:     cloneInt64Pointer(a.DailyTokensToday),
		DailyLimitBlocked:    a.dailyLimitBlockedLocked(),
		DailyCreditLimit:     a.DailyCreditLimit,
		DailyCreditsToday:    roundedFloat64Pointer(a.DailyCreditsToday),
		CreditLimitReached:   a.creditLimitReachedLocked(),
		ModelDailyTokenLimit: a.ModelDailyTokenLimit,
		ModelDailyTokens:     cloneModelTokenMap(a.ModelDailyTokens),
		LastCheckin:          a.Auth.LastCheckin,
		LastDailyChat:        a.Auth.LastDailyChat,
		LastActivityReport:   a.Auth.LastActivityReport,
		CanCheckin:           !a.Auth.APIKey && a.Auth.Realm == realmCN && strings.TrimSpace(a.Auth.EnterpriseID) == "",
		CanDailyChat:         !a.Auth.APIKey && a.Auth.Realm == realmIntl,
		CanReportActivity: !a.Auth.APIKey &&
			(a.Auth.Realm == realmCN || a.Auth.Realm == realmIntl) &&
			strings.TrimSpace(a.Auth.EnterpriseID) == "",
	}
}

func shortUID(uid string) string {
	if uid == "" {
		return "?"
	}
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

func accountSecretValuesLocked(a *Account) []string {
	values := []string{
		a.Auth.AccessToken,
		a.Auth.RefreshToken,
		a.Auth.ProxyURL,
	}
	if password := proxyPassword(a.Auth.ProxyURL); password != "" {
		values = append(values, password)
	}
	for _, key := range []string{"deviceToken", "device_token", "device-token"} {
		raw, ok := a.Auth.Extra[key]
		if !ok {
			continue
		}
		var token string
		if json.Unmarshal(raw, &token) == nil && token != "" {
			values = append(values, token)
		}
	}
	return values
}

func proxyPassword(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User == nil {
		return ""
	}
	password, _ := parsed.User.Password()
	return password
}

func redactAccountText(text string, values []string) string {
	if text == "" {
		return ""
	}
	return secrets.Redact(text, values...)
}

func credentialMask(auth Auth) string {
	if !auth.APIKey {
		return ""
	}
	head := strings.SplitN(strings.TrimSpace(auth.AccessToken), ".", 2)[0]
	if head == "" {
		return "***"
	}
	return head + ".***"
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

// humanDelta mirrors v1 _human_delta: whole days, one decimal hour, or whole
// minutes; non-positive durations read "expired".
func humanDelta(delta time.Duration) string {
	seconds := delta.Seconds()
	if !(seconds > 0) {
		return "expired"
	}
	days := seconds / 86400
	if days >= 1 {
		return fmt.Sprintf("%.0f days", days)
	}
	hours := seconds / 3600
	if hours >= 1 {
		return fmt.Sprintf("%.1f hours", hours)
	}
	return fmt.Sprintf("%d min", int(seconds/60))
}

func unixSeconds(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

func normalizedWeight(value float64) int {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < minWeight {
		return minWeight
	}
	if value > maxWeight {
		return maxWeight
	}
	return int(math.Round(value))
}

func truncateText(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}

func disabledModelSet(models []string) map[string]bool {
	if len(models) == 0 {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		out[model] = true
	}
	return out
}

func cloneCreditSnapshot(snapshot *CreditSnapshot) *CreditSnapshot {
	if snapshot == nil {
		return nil
	}
	copied := *snapshot
	if snapshot.Packages != nil {
		copied.Packages = make([]CreditPackage, len(snapshot.Packages))
		for i, pkg := range snapshot.Packages {
			if pkg.DaysLeft != nil {
				value := *pkg.DaysLeft
				pkg.DaysLeft = &value
			}
			copied.Packages[i] = pkg
		}
	}
	if snapshot.Checkin != nil {
		value := *snapshot.Checkin
		copied.Checkin = &value
	}
	if snapshot.EarliestExpiring != nil {
		value := *snapshot.EarliestExpiring
		copied.EarliestExpiring = &value
	}
	return &copied
}

func decodeCreditSnapshot(raw json.RawMessage) *CreditSnapshot {
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var snapshot CreditSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil
	}
	return &snapshot
}

func tierLabel(raw json.RawMessage) (string, bool, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false, false
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err == nil && object != nil {
		label := ""
		for _, key := range []string{"package", "package_name", "packageName", "package_code", "packageCode"} {
			if value, ok := object[key].(string); ok && strings.TrimSpace(value) != "" {
				label = strings.TrimSpace(value)
				break
			}
		}
		paid, known := false, false
		if value, ok := object["paid"]; ok {
			switch typed := value.(type) {
			case bool:
				paid = typed
				known = true
			case string:
				paid = strings.EqualFold(strings.TrimSpace(typed), "true")
				known = strings.TrimSpace(typed) != ""
			case float64:
				paid = typed != 0
				known = true
			}
		}
		if !known {
			return label, false, false
		}
		if label == "" {
			if paid {
				label = "pro"
			} else {
				label = "free"
			}
		}
		return label, paid, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return "", false, false
		}
		lower := strings.ToLower(text)
		return text, lower == "pro" || lower == "paid" || lower == "premium", true
	}
	return "", false, false
}

func tierJSONValue(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}
