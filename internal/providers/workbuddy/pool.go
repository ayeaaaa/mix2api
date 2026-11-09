package workbuddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// affinityTTL and affinityHistory mirror v1's SessionAffinity. A
	// conversation remembers a short most-recent list so a failed front entry
	// can fall back to another account that already warmed its prefix.
	affinityTTL     = 2 * time.Hour
	affinityHistory = 3
	maxAffinityKeys = 5000
)

var (
	// ErrNoAccount means every account was excluded, unhealthy, cooling, at
	// its concurrency cap, model-disabled, guarded, or not paid enough.
	ErrNoAccount = errors.New("workbuddy: no account available")

	// ErrAccountNotFound means the requested UID is not in this realm's pool.
	ErrAccountNotFound = errors.New("workbuddy: account not found")

	// ErrRealmMismatch means Add was asked to store an account belonging to
	// the other WorkBuddy realm. The two provider instances own disjoint pools.
	ErrRealmMismatch = errors.New("workbuddy: account belongs to a different realm")
)

// PoolStats is the v1 overview counter shape.
type PoolStats struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Disabled int `json:"disabled"`
	Cooling  int `json:"cooling"`
}

// weightEntry is one participant in a smooth weighted round-robin.
type weightEntry struct {
	account *Account
	key     string
	weight  int
}

// Pool is the provider-owned WorkBuddy account pool for one realm. It owns
// account loading, v1 credential persistence, scheduler filters, smooth
// weighted round-robin state, session affinity, and proxy resolution.
//
// Lock order: writeMu -> Account.mu; Pool.mu may be taken briefly to snapshot
// membership or settings before Account.mu, never while Account.mu is held.
// Account.mu is never held across file I/O; writeMu serializes credential
// mutations so an older full-document save cannot overwrite a later note,
// weight, or token refresh.
type Pool struct {
	dir   string
	realm string
	store *config.Store

	mu       sync.RWMutex
	cfg      Config
	accounts []*Account
	slots    transport.Slots
	now      func() time.Time

	// remainingWeights is the provider-computed (uid, model) hook for the
	// remaining-usage preference. Keys are uid + "\x00" + model.
	remainingWeights   map[string]int
	proOnly            map[string]bool
	guardCheck         func(*Account, string) bool
	freeModels         map[string]bool
	limitsApplied      bool
	limitValues        map[string]any
	usageValues        map[string]any
	expiringWindowDays int
	loadErr            error

	// writeMu serializes mutations and credential-file writes.
	writeMu sync.Mutex

	pickMu       sync.Mutex
	weightStates map[string]map[string]int

	affinity *sessionAffinity
}

// NewPool returns the provider's cached pool. New calls it eagerly; subsequent
// callers receive the same instance. Loading is tolerant for individual
// files: malformed account documents are skipped by LoadAccounts, while an
// unreadable directory is recorded for Pool.LoadError rather than panicking.
func (p *Provider) NewPool() *Pool {
	if p == nil {
		return nil
	}
	p.poolOnce.Do(func() {
		p.pool, p.poolErr = newPool(p)
	})
	return p.pool
}

// Pool returns the provider's account pool.
func (p *Provider) Pool() *Pool {
	return p.NewPool()
}

// PoolLoadError returns the error recorded while constructing the pool, if
// any. Malformed individual account files are not errors.
func (p *Provider) PoolLoadError() error {
	if p == nil {
		return nil
	}
	p.NewPool()
	return p.poolErr
}

func newPool(provider *Provider) (*Pool, error) {
	if provider == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	dir := strings.TrimSpace(provider.AccountsDir())
	if dir == "" {
		return nil, errors.New("workbuddy: accounts directory is empty")
	}
	realm := provider.Realm()
	if !validRealm(realm) {
		return nil, fmt.Errorf("workbuddy: invalid realm %q", realm)
	}
	cfg := provider.Config()
	p := &Pool{
		dir:              dir,
		realm:            realm,
		store:            provider.store,
		cfg:              cfg,
		now:              time.Now,
		remainingWeights: make(map[string]int),
		proOnly:          make(map[string]bool),
		freeModels:       staticFreeModelSet(realm),
		weightStates:     make(map[string]map[string]int),
		affinity:         newSessionAffinity(affinityTTL, maxAffinityKeys, affinityHistory),
	}
	p.guardCheck = defaultGuardCheck
	p.affinity.now = p.nowTime
	settings := map[string]any{}
	if raw, err := p.loadSettings(); err == nil {
		settings = raw
		p.slots = transport.SlotsFromSettings(raw)
	} else {
		p.loadErr = err
	}
	// v1 reads the shared limits at startup. The expiring window is the one
	// non-zero default (7 days); an explicit per-realm override wins. Applying
	// before loading accounts also gives newAccount the stored projection.
	p.ApplyLimits(limitsFromSettings(settings), nil)

	auths, err := LoadAccounts(dir)
	if err != nil {
		p.loadErr = err
		return p, nil
	}

	maxConcurrent := p.accountMaxConcurrentLocked(cfg)
	byUID := make(map[string]Auth, len(auths))
	for _, auth := range auths {
		if auth.Realm != realm {
			continue
		}
		uid := strings.TrimSpace(auth.UID)
		if uid == "" {
			continue
		}
		if existing, ok := byUID[uid]; ok {
			// A canonical uid.json wins over an alias for the same UID.
			canonical, pathErr := AccountPath(dir, uid)
			if pathErr == nil && filepath.Clean(auth.Path) == filepath.Clean(canonical) &&
				filepath.Clean(existing.Path) != filepath.Clean(canonical) {
				byUID[uid] = auth
			}
			continue
		}
		byUID[uid] = auth
	}

	accounts := make([]*Account, 0, len(byUID))
	for _, auth := range byUID {
		accounts = append(accounts, p.newAccount(auth, maxConcurrent))
	}
	sort.Slice(accounts, func(i, j int) bool {
		return strings.TrimSpace(accounts[i].Auth.UID) < strings.TrimSpace(accounts[j].Auth.UID)
	})
	p.accounts = accounts
	return p, nil
}

func (p *Pool) loadSettings() (map[string]any, error) {
	if p == nil || p.store == nil {
		return map[string]any{}, nil
	}
	return p.store.Load()
}

func (p *Pool) accountMaxConcurrentLocked(cfg Config) int {
	value := cfg.AccountMaxConcurrent
	if value < minAccountMaxConcurrent {
		return minAccountMaxConcurrent
	}
	if value > maxAccountMaxConcurrent {
		return maxAccountMaxConcurrent
	}
	return value
}

func (p *Pool) newAccount(auth Auth, maxConcurrent int) *Account {
	credits := decodeCreditSnapshot(auth.Extra["credits"])
	tier, paid, known := tierLabel(auth.Tier)
	if !known && credits != nil && credits.IsPaidUser {
		tier = "pro"
		paid = true
	}
	state := accountstate.State{
		UID:            strings.TrimSpace(auth.UID),
		Disabled:       !auth.Enabled,
		ManualDisabled: !auth.Enabled,
		TasksEnabled:   auth.TasksEnabled,
		Note:           auth.Note,
		MaxConcurrent:  maxConcurrent,
	}
	account := &Account{
		Auth:               auth,
		State:              state,
		Credits:            credits,
		DisabledModels:     disabledModelSet(auth.DisabledModels),
		Group:              auth.Group,
		Weight:             float64(auth.Weight),
		Tier:               tier,
		ExpiringWindowDays: p.expiringWindow(),
		FreeModels:         p.freeModelSnapshot(),
		Path:               auth.Path,
		pool:               p,
		maxConcurrent:      maxConcurrent,
		paidUser:           paid,
		paidKnown:          known,
	}
	if account.Weight < minWeight {
		account.Weight = minWeight
	}
	account.Auth.ProxyURL = p.resolveProxySlot(auth.ProxySlot, auth.Extra)
	p.applyStoredLimits(account)
	return account
}

// LoadError returns the construction error recorded by the pool. An empty
// accounts directory and individual malformed files are not errors.
func (p *Pool) LoadError() error {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.loadErr
}

// Dir returns the pool's accounts directory.
func (p *Pool) Dir() string {
	if p == nil {
		return ""
	}
	return p.dir
}

// Realm returns the realm this pool serves.
func (p *Pool) Realm() string {
	if p == nil {
		return ""
	}
	return p.realm
}

// Config returns the pool's scheduling configuration snapshot.
func (p *Pool) Config() Config {
	if p == nil {
		return Config{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return clonePoolConfig(p.cfg)
}

func clonePoolConfig(cfg Config) Config {
	cfg.AutoSwitchProducts = append([]string(nil), cfg.AutoSwitchProducts...)
	return cfg
}

// SetConfig replaces the scheduling configuration and applies the account
// concurrency cap to every loaded account. Existing in-flight acquisitions are
// not revoked; the new cap is enforced on the next Acquire/Pick.
func (p *Pool) SetConfig(cfg Config) {
	if p == nil {
		return
	}
	maxConcurrent := p.accountMaxConcurrentLocked(cfg)
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	p.cfg = clonePoolConfig(cfg)
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, account := range accounts {
		account.mu.Lock()
		account.maxConcurrent = maxConcurrent
		account.State.MaxConcurrent = maxConcurrent
		account.mu.Unlock()
	}
}

func (p *Pool) nowTime() time.Time {
	if p == nil {
		return time.Now()
	}
	p.mu.RLock()
	now := p.now
	p.mu.RUnlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// Accounts returns a snapshot of the live account pointers, sorted by UID.
func (p *Pool) Accounts() []*Account {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.RUnlock()
	sort.SliceStable(accounts, func(i, j int) bool {
		return accountUID(accounts[i]) < accountUID(accounts[j])
	})
	return accounts
}

// Get returns the live account for uid, or nil.
func (p *Pool) Get(uid string) *Account {
	if p == nil {
		return nil
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil
	}
	for _, account := range p.Accounts() {
		uidNow, live := accountUIDLive(account)
		if live && uidNow == uid {
			return account
		}
	}
	return nil
}

// List returns public account views sorted by UID.
func (p *Pool) List() []AccountView {
	if p == nil {
		return nil
	}
	accounts := p.Accounts()
	views := make([]AccountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, account.View())
	}
	sort.Slice(views, func(i, j int) bool { return views[i].UID < views[j].UID })
	return views
}

// Stats returns the v1 overview counters. A disabled account is never counted
// as cooling or healthy.
func (p *Pool) Stats() PoolStats {
	stats := PoolStats{}
	if p == nil {
		return stats
	}
	now := p.nowTime()
	for _, account := range p.Accounts() {
		account.mu.RLock()
		disabled := !account.Auth.Enabled || account.State.Disabled
		cooling := !disabled && account.State.CoolUntil.After(now)
		// Healthy uses the same predicate as Pick, including runtime token
		// expiry and credential presence. A token-expired account is neither
		// disabled nor cooling, but it is not healthy either.
		healthy := !disabled && !cooling && account.healthyLocked(now)
		account.mu.RUnlock()
		stats.Total++
		switch {
		case disabled:
			stats.Disabled++
		case cooling:
			stats.Cooling++
		case healthy:
			stats.Healthy++
		}
	}
	return stats
}

// TaskAccounts returns accounts that participate in scheduled/manual tasks.
// It is independent of API enabled state, matching v1's task_active(), and
// excludes accounts carrying a persisted 403/11140 restrictionUntil window.
func (p *Pool) TaskAccounts() []*Account {
	if p == nil {
		return nil
	}
	now := p.nowTime()
	out := make([]*Account, 0)
	for _, account := range p.Accounts() {
		account.mu.RLock()
		active := !account.removed && account.Auth.TasksEnabled
		auth := cloneAuth(account.Auth)
		account.mu.RUnlock()
		if active && taskRestrictedUntil(auth) > float64(now.Unix()) {
			active = false
		}
		if active {
			out = append(out, account)
		}
	}
	return out
}

// ReadyAccounts returns the accounts that can serve model right now, sorted by
// UID. It is the read-only "test all"/diagnostics helper: no concurrency slot
// is acquired and session affinity is not modified.
func (p *Pool) ReadyAccounts(model string) []*Account {
	if p == nil {
		return nil
	}
	now := p.nowTime()
	proOnly, guard := p.schedulingHooks()
	out := make([]*Account, 0)
	for _, account := range p.Accounts() {
		if p.accountUsable(account, model, nil, now, proOnly, guard) {
			out = append(out, account)
		}
	}
	return out
}

// CountReady reports how many accounts can serve model right now.
func (p *Pool) CountReady(model string) int {
	return len(p.ReadyAccounts(model))
}

// Acquire reserves one concurrency slot. Pick already checked the cap; this
// call closes the race between selection and dispatch. A nil account, removed
// account, or full account returns false.
func (p *Pool) Acquire(a *Account) bool {
	if a == nil {
		return false
	}
	now := time.Now()
	if p != nil {
		now = p.nowTime()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed {
		return false
	}
	if a.maxConcurrent > 0 && a.State.ActiveConcurrent >= a.maxConcurrent {
		return false
	}
	a.State.ActiveConcurrent++
	a.State.LastUsed = now
	return true
}

// Release returns a concurrency slot, clamped at zero.
func (p *Pool) Release(a *Account) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.State.ActiveConcurrent > 0 {
		a.State.ActiveConcurrent--
	}
	a.mu.Unlock()
}

// BindSession records which account served a conversation's latest turn.
func (p *Pool) BindSession(sessionKey, uid string) {
	if p == nil || p.affinity == nil {
		return
	}
	p.affinity.bind(sessionKey, uid)
}

// DemoteSession drops one account from a conversation's recent list while
// keeping the accounts behind it. It is used by failure paths so the next turn
// tries another warm account before a cold one.
func (p *Pool) DemoteSession(sessionKey, uid string) {
	if p == nil || p.affinity == nil {
		return
	}
	p.affinity.demote(sessionKey, uid)
}

// UnbindSession forgets a conversation's affinity entirely.
func (p *Pool) UnbindSession(sessionKey string) {
	if p == nil || p.affinity == nil {
		return
	}
	p.affinity.unbind(sessionKey)
}

// Pick chooses one usable account for model.
//
// Filter order: removed/realm, Auth.Enabled + runtime disable, cooldown,
// credential presence, manual model ban, guard hook, Pro-only tier, account
// concurrency cap, and exclude. Selection order: session affinity recent list,
// manual weight > 1, expiring credits inside the account window, remaining
// usage hook, then deterministic equal-weight rotation. All weighted layers
// use smooth weighted round-robin with per-layer state.
func (p *Pool) Pick(model, sessionKey string, exclude map[string]bool) (*Account, error) {
	if p == nil {
		return nil, ErrNoAccount
	}
	model = strings.TrimSpace(model)
	sessionKey = strings.TrimSpace(sessionKey)
	now := p.nowTime()
	proOnly, guard := p.schedulingHooks()

	eligible := make([]*Account, 0)
	byUID := make(map[string]*Account)
	for _, account := range p.Accounts() {
		if !p.accountUsable(account, model, exclude, now, proOnly, guard) {
			continue
		}
		eligible = append(eligible, account)
		byUID[accountUID(account)] = account
	}
	if len(eligible) == 0 {
		return nil, ErrNoAccount
	}

	if sessionKey != "" && p.affinity != nil {
		for _, uid := range p.affinity.recent(sessionKey) {
			if exclude != nil && exclude[uid] {
				continue
			}
			if account := byUID[uid]; account != nil {
				p.affinity.bind(sessionKey, uid)
				return account, nil
			}
		}
	}

	selected := p.pickManual(eligible)
	if selected == nil {
		selected = p.pickExpiring(eligible)
	}
	if selected == nil {
		selected = p.pickRemaining(eligible, model)
	}
	if selected == nil {
		selected = p.pickFallback(eligible)
	}
	if selected == nil {
		return nil, ErrNoAccount
	}
	if sessionKey != "" && p.affinity != nil {
		p.affinity.bind(sessionKey, accountUID(selected))
	}
	return selected, nil
}

func (p *Pool) schedulingHooks() (map[string]bool, func(*Account, string) bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	proOnly := make(map[string]bool, len(p.proOnly))
	for model, value := range p.proOnly {
		proOnly[model] = value
	}
	return proOnly, p.guardCheck
}

func (p *Pool) accountUsable(account *Account, model string, exclude map[string]bool,
	now time.Time, proOnly map[string]bool, guard func(*Account, string) bool) bool {
	if account == nil {
		return false
	}
	uid := accountUID(account)
	if uid == "" || (exclude != nil && exclude[uid]) {
		return false
	}

	account.mu.RLock()
	usable := !account.removed &&
		account.Auth.Realm == p.realm &&
		account.healthyLocked(now) &&
		!(account.maxConcurrent > 0 && account.State.ActiveConcurrent >= account.maxConcurrent)
	if usable && model != "" && proOnly[strings.ToLower(model)] && !account.paidUserLocked() {
		usable = false
	}
	if usable && model != "" {
		lower := strings.ToLower(model)
		if account.modelCoolingLocked(lower, now) {
			usable = false
		} else if account.DisabledModels[lower] {
			usable = false
		} else {
			for blocked := range account.DisabledModels {
				if strings.EqualFold(strings.TrimSpace(blocked), lower) {
					usable = false
					break
				}
			}
		}
	}
	account.mu.RUnlock()

	if !usable {
		return false
	}
	if guard != nil && guard(account, model) {
		return false
	}
	return true
}

func (p *Pool) pickManual(eligible []*Account) *Account {
	entries := make([]weightEntry, 0)
	for _, account := range eligible {
		account.mu.RLock()
		weight := normalizedWeight(account.Weight)
		uid := strings.TrimSpace(account.Auth.UID)
		account.mu.RUnlock()
		if weight <= minWeight {
			continue
		}
		entries = append(entries, weightEntry{account: account, key: uid, weight: weight})
	}
	if len(entries) == 0 {
		return nil
	}
	return p.smoothWeightedPick("manual", entries, nil)
}

func (p *Pool) pickExpiring(eligible []*Account) *Account {
	entries := make([]weightEntry, 0)
	for _, account := range eligible {
		account.mu.RLock()
		window := account.ExpiringWindowDays
		days, remain, hasCredits := account.expiringPackageLocked()
		uid := strings.TrimSpace(account.Auth.UID)
		account.mu.RUnlock()
		if window <= 0 || !hasCredits || days > float64(window) {
			continue
		}
		remainingFraction := 1 - (days / float64(window))
		if remainingFraction < 0 {
			remainingFraction = 0
		}
		if remainingFraction > 1 {
			remainingFraction = 1
		}
		urgency := 1 + remainingFraction
		amountWeight := 1.0
		if remain > 0 {
			amountWeight = 1 + math.Log10(1+remain)
		}
		weight := int(math.Round(urgency * amountWeight * expiryWeightScale))
		if weight < minWeight {
			weight = minWeight
		}
		entries = append(entries, weightEntry{account: account, key: uid, weight: weight})
	}
	if len(entries) == 0 {
		return nil
	}
	return p.smoothWeightedPick("expiring", entries, nil)
}

func (p *Pool) pickRemaining(eligible []*Account, model string) *Account {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	entries := make([]weightEntry, 0)
	for _, account := range eligible {
		uid := accountUID(account)
		weight := p.remainingWeight(uid, model)
		if weight <= minWeight {
			continue
		}
		entries = append(entries, weightEntry{
			account: account,
			key:     uid + "\x00" + model,
			weight:  weight,
		})
	}
	if len(entries) == 0 {
		return nil
	}
	// Match v1 keep_uids: preserve rotation state for every account still in
	// the pool, not only the entries taking part in this model's pick. Pruning
	// by the current entries would reset another model's progress on every
	// interleaved request and collapse the weighted share.
	keepUIDs := make(map[string]bool)
	for _, account := range p.Accounts() {
		if uid := accountUID(account); uid != "" {
			keepUIDs[uid] = true
		}
	}
	keep := func(key string) bool {
		uid, _, ok := strings.Cut(key, "\x00")
		return ok && keepUIDs[uid]
	}
	return p.smoothWeightedPick("remaining", entries, keep)
}

func (p *Pool) pickFallback(eligible []*Account) *Account {
	entries := make([]weightEntry, 0, len(eligible))
	for _, account := range eligible {
		uid := accountUID(account)
		entries = append(entries, weightEntry{account: account, key: uid, weight: 1})
	}
	return p.smoothWeightedPick("rotation", entries, nil)
}

// smoothWeightedPick implements nginx's smooth weighted round-robin. Every
// entry accrues its weight each pick; the largest running total wins and pays
// back the whole round's weight. Equal weights therefore degenerate to a plain
// rotation. A nil keep function prunes the state to the entries participating
// in this pick; otherwise keep decides which stored keys survive.
func (p *Pool) smoothWeightedPick(stateName string, entries []weightEntry, keep func(string) bool) *Account {
	if p == nil || len(entries) == 0 {
		return nil
	}
	p.pickMu.Lock()
	defer p.pickMu.Unlock()
	state := p.weightStates[stateName]
	if state == nil {
		state = make(map[string]int)
		p.weightStates[stateName] = state
	}
	current := make(map[string]bool, len(entries))
	for _, entry := range entries {
		current[entry.key] = true
	}
	for key := range state {
		preserve := current[key]
		if keep != nil {
			preserve = keep(key)
		}
		if !preserve {
			delete(state, key)
		}
	}
	total := 0
	chosen := -1
	for index, entry := range entries {
		state[entry.key] += entry.weight
		total += entry.weight
		if chosen < 0 || state[entry.key] > state[entries[chosen].key] {
			chosen = index
		}
	}
	if chosen < 0 || total <= 0 {
		return nil
	}
	state[entries[chosen].key] -= total
	return entries[chosen].account
}

// ApplyRemainingWeights installs the provider-computed (account, model)
// remaining-usage preference. Only weights greater than 1 are retained; an
// empty map disables the preference. The pool never derives this data itself.
func (p *Pool) ApplyRemainingWeights(weights map[string]map[string]int) map[string]map[string]int {
	if p == nil {
		return nil
	}
	table := make(map[string]int)
	for uid, models := range weights {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		for model, weight := range models {
			model = strings.TrimSpace(model)
			if model == "" || weight <= minWeight {
				continue
			}
			table[uid+"\x00"+model] = weight
		}
	}
	p.mu.Lock()
	p.remainingWeights = table
	p.mu.Unlock()
	return remainingWeightMap(table)
}

// ApplyExpiringWindow sets the expiring-credits preference window for every
// loaded account. A non-positive value disables the preference. Task 6 reads
// the per-realm setting and calls this; the pool does not interpret settings
// itself.
func (p *Pool) ApplyExpiringWindow(days int) {
	if p == nil {
		return
	}
	if days < 0 {
		days = 0
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	p.expiringWindowDays = days
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, account := range accounts {
		account.mu.Lock()
		account.ExpiringWindowDays = days
		account.mu.Unlock()
	}
}

// expiringWindow returns the pool's default preference window. New accounts
// inherit it so an ApplyExpiringWindow call is not lost on a later Add.
func (p *Pool) expiringWindow() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.expiringWindowDays
}

func (p *Pool) freeModelSnapshot() map[string]bool {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneBoolMap(p.freeModels)
}

func remainingWeightMap(table map[string]int) map[string]map[string]int {
	out := make(map[string]map[string]int)
	for key, weight := range table {
		uid, model, ok := strings.Cut(key, "\x00")
		if !ok {
			continue
		}
		if out[uid] == nil {
			out[uid] = make(map[string]int)
		}
		out[uid][model] = weight
	}
	return out
}

func (p *Pool) remainingWeight(uid, model string) int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.remainingWeights[uid+"\x00"+model]
}

// SetProOnlyModels installs the catalog's Pro-only model set. A nil/empty list
// disables the gate. Task 5 wires the catalog result here.
func (p *Pool) SetProOnlyModels(models []string) {
	if p == nil {
		return
	}
	table := make(map[string]bool, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model != "" {
			table[model] = true
		}
	}
	p.mu.Lock()
	p.proOnly = table
	p.mu.Unlock()
}

// SetGuardCheck installs the provider guard hook. Task 6 owns reserve/daily
// limits and supplies the real predicate; nil means no guard.
func (p *Pool) SetGuardCheck(check func(*Account, string) bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.guardCheck = check
	p.mu.Unlock()
}

func accountUID(account *Account) string {
	if account == nil {
		return ""
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return strings.TrimSpace(account.Auth.UID)
}

func accountUIDLive(account *Account) (string, bool) {
	if account == nil {
		return "", false
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return strings.TrimSpace(account.Auth.UID), !account.removed
}

// SaveAccount writes the account's current credential document, preserving
// unknown/state extras and excluding runtime error/cooldown fields.
func (p *Pool) SaveAccount(account *Account) error {
	if p == nil || account == nil {
		return errors.New("workbuddy: account is nil")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	account.mu.RLock()
	if account.removed {
		account.mu.RUnlock()
		return ErrAccountNotFound
	}
	auth := authSnapshotLocked(account)
	runtimeProxy := account.Auth.ProxyURL
	account.mu.RUnlock()

	saved, err := SaveAccount(p.dir, auth)
	if err != nil {
		return err
	}
	saved.ProxyURL = runtimeProxy
	account.mu.Lock()
	account.Auth = saved
	account.Path = saved.Path
	account.mu.Unlock()
	return nil
}

// mutateAccount serializes a mutation with its credential write. The callback
// runs under Account.mu and must not perform I/O or call back into the pool.
func (p *Pool) mutateAccount(uid string, mutate func(*Account)) error {
	if p == nil {
		return ErrAccountNotFound
	}
	account := p.Get(uid)
	if account == nil {
		return ErrAccountNotFound
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	account.mu.Lock()
	if account.removed {
		account.mu.Unlock()
		return ErrAccountNotFound
	}
	mutate(account)
	auth := authSnapshotLocked(account)
	runtimeProxy := account.Auth.ProxyURL
	account.mu.Unlock()

	saved, err := SaveAccount(p.dir, auth)
	if err != nil {
		return err
	}
	saved.ProxyURL = runtimeProxy
	account.mu.Lock()
	account.Auth = saved
	account.Path = saved.Path
	account.mu.Unlock()
	return nil
}

func cloneAuth(auth Auth) Auth {
	copied := auth
	if auth.Extra != nil {
		copied.Extra = make(map[string]json.RawMessage, len(auth.Extra))
		for key, value := range auth.Extra {
			copied.Extra[key] = append(json.RawMessage(nil), value...)
		}
	}
	if auth.Tier != nil {
		copied.Tier = append(json.RawMessage(nil), auth.Tier...)
	}
	if auth.DisabledModels != nil {
		copied.DisabledModels = append([]string(nil), auth.DisabledModels...)
	}
	return copied
}

// authSnapshotLocked projects the Account scheduler mirrors onto a private
// copy of Auth before a credential write. The pool setters keep both sides in
// step; this also makes a direct field edit followed by Save() persist, which
// is the natural expectation for the exported Account fields.
func authSnapshotLocked(account *Account) Auth {
	auth := cloneAuth(account.Auth)
	auth.Group = truncateText(strings.TrimSpace(account.Group), maxGroupRunes)
	auth.Weight = normalizedWeight(account.Weight)
	if models, changed := disabledModelsForAuth(account.Auth.DisabledModels, account.DisabledModels); changed {
		auth.DisabledModels = models
	}
	return auth
}

// disabledModelsForAuth returns Auth's ordered list when the map describes the
// same set, otherwise a deterministic sorted projection. The second result is
// false when no rewrite is needed.
func disabledModelsForAuth(current []string, set map[string]bool) ([]string, bool) {
	existing := make(map[string]bool, len(current))
	for _, model := range current {
		model = strings.ToLower(strings.TrimSpace(model))
		if model != "" {
			existing[model] = true
		}
	}
	if len(existing) == len(set) {
		same := true
		for model := range set {
			model = strings.ToLower(strings.TrimSpace(model))
			if model == "" || !existing[model] {
				same = false
				break
			}
		}
		if same {
			return current, false
		}
	}
	models := make([]string, 0, len(set))
	for model := range set {
		model = strings.ToLower(strings.TrimSpace(model))
		if model != "" {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models, true
}

// Disable applies a manual disable. Task participation is disabled too unless
// SetTasks later opts the account back in, matching v1's set_enabled default.
func (p *Pool) Disable(uid, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual disable"
	}
	return p.mutateAccount(uid, func(account *Account) {
		account.Auth.SetEnabled(false)
		account.Auth.SetTasksEnabled(false)
		account.State.Disabled = true
		account.State.ManualDisabled = true
		account.State.DisabledBy = "manual"
		account.State.TasksEnabled = false
		account.State.Reason = reason
		account.State.LastError = reason
	})
}

// Enable clears a manual disable, the account cooldown, and error counters. It
// restores task participation as well; SetTasks can opt out afterwards.
func (p *Pool) Enable(uid string) error {
	return p.mutateAccount(uid, func(account *Account) {
		account.Auth.SetEnabled(true)
		account.Auth.SetTasksEnabled(true)
		account.State.Disabled = false
		account.State.ManualDisabled = false
		account.State.DisabledBy = ""
		account.State.TasksEnabled = true
		account.State.Reason = ""
		account.State.LastError = ""
		account.State.ErrCount = 0
		account.State.CoolUntil = time.Time{}
		account.clearModelCooldownsLocked("")
	})
}

const (
	// softErrorCooldown mirrors v1's short single-account 429/ordinary-error
	// park. The account breaker is deliberately simpler here: three ordinary
	// failures in a row park it for ten minutes, then the count restarts.
	softErrorCooldown = 60 * time.Second
	errorThreshold    = 3
	errorCooldown     = 10 * time.Minute

	// restrictedCooldown mirrors v1 RESTRICTED_ACCOUNT_COOLDOWN (403/11140).
	restrictedCooldown = 2 * time.Hour
)

// NoteError records a request failure with v1 runtime semantics: terminal
// marks the credential expired and auto-disables the account unless the
// operator already disabled it; soft starts a short account cooldown; an
// ordinary error advances the consecutive-failure counter. Runtime state is
// never persisted, matching v1; only a manual disable writes the credential
// file.
func (p *Pool) NoteError(a *Account, message string, soft, terminal bool) error {
	if a == nil {
		return nil
	}
	if p == nil {
		return errors.New("workbuddy: pool is nil")
	}
	now := p.nowTime()
	a.mu.Lock()
	redacted := truncateText(redactAccountText(message, accountSecretValuesLocked(a)), tokenErrorLimit)
	a.State.LastError = redacted
	switch {
	case terminal:
		a.tokenExpired = true
		if !a.State.ManualDisabled {
			a.State.Disabled = true
			a.State.DisabledBy = "auth"
			a.State.Reason = redacted
		}
	case soft:
		if until := now.Add(softErrorCooldown); until.After(a.State.CoolUntil) {
			a.State.CoolUntil = until
		}
	default:
		a.State.ErrCount++
		if a.State.ErrCount >= errorThreshold {
			if until := now.Add(errorCooldown); until.After(a.State.CoolUntil) {
				a.State.CoolUntil = until
			}
			a.State.ErrCount = 0
		}
	}
	a.mu.Unlock()
	return nil
}

// NoteCooldown parks the whole account for wait with a redacted last error.
// Rate-limit handling uses this for credential-scoped 429s so the v1 600s
// first-tier window is kept instead of the generic 60s soft park.
func (p *Pool) NoteCooldown(a *Account, message string, wait time.Duration) error {
	if a == nil {
		return nil
	}
	if p == nil {
		return errors.New("workbuddy: pool is nil")
	}
	if wait <= 0 {
		wait = softErrorCooldown
	}
	until := p.nowTime().Add(wait)
	a.mu.Lock()
	a.State.LastError = truncateText(redactAccountText(message, accountSecretValuesLocked(a)), tokenErrorLimit)
	if until.After(a.State.CoolUntil) {
		a.State.CoolUntil = until
	}
	a.mu.Unlock()
	return nil
}

// NoteModelError records a model-scoped upstream window without parking the
// whole credential. Sibling models stay serviceable on the same account.
func (p *Pool) NoteModelError(a *Account, model, message string, wait time.Duration) error {
	if a == nil {
		return nil
	}
	if p == nil {
		return errors.New("workbuddy: pool is nil")
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return nil
	}
	if wait <= 0 {
		wait = softErrorCooldown
	}
	started := p.nowTime()
	until := started.Add(wait)
	a.mu.Lock()
	a.State.LastError = truncateText(redactAccountText(message, accountSecretValuesLocked(a)), tokenErrorLimit)
	a.setModelCooldownLocked(model, until, started)
	a.mu.Unlock()
	return nil
}

// NoteRestricted records v1's account-level 403/11140 restriction. The
// cooldown is runtime-only in this slice; the account stays out of rotation
// until it expires or a later successful request proves the vendor lifted it.
func (p *Pool) NoteRestricted(a *Account, message string) error {
	return p.NoteCooldown(a, message, restrictedCooldown)
}

// NoteSuccess clears every account-level penalty and the named model window
// (or all model windows when no model is supplied). A served response is v1's
// evidence that a persisted upstream restriction is gone and that a locally
// marked token still works.
func (p *Pool) NoteSuccess(a *Account, model ...string) error {
	if a == nil {
		return nil
	}
	if p == nil {
		return errors.New("workbuddy: pool is nil")
	}
	a.mu.Lock()
	for _, item := range model {
		a.clearModelCooldownsLocked(item)
	}
	if len(model) == 0 {
		a.clearModelCooldownsLocked("")
	}
	a.State.ErrCount = 0
	a.State.LastError = ""
	a.State.CoolUntil = time.Time{}
	a.tokenExpired = false
	if !a.State.ManualDisabled {
		a.State.Disabled = false
		a.State.DisabledBy = ""
		a.State.Reason = ""
	}
	a.mu.Unlock()
	return nil
}

// AdmissionWait explains why Pick found no candidate and, when the reason is a
// temporary local admission limit, returns the shortest wait until one could
// become usable. Guard-blocked and concurrency-busy pools are rate-limited
// (429) in the chat layer, not reported as an unavailable pool (503). A pool
// with genuinely disabled/cooling/expired accounts returns 0 and stays 503.
func (p *Pool) AdmissionWait(model string, exclude map[string]bool) (time.Duration, string) {
	if p == nil {
		return 0, ""
	}
	model = strings.TrimSpace(model)
	lower := strings.ToLower(model)
	p.mu.RLock()
	proOnly := make(map[string]bool, len(p.proOnly))
	for key, value := range p.proOnly {
		proOnly[key] = value
	}
	p.mu.RUnlock()

	now := p.nowTime()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	candidates := 0
	usable := false
	var best time.Duration
	reason := ""
	consider := func(wait time.Duration, why string) {
		if wait <= 0 || why == "" {
			return
		}
		if wait < time.Second {
			wait = time.Second
		}
		if best == 0 || wait < best {
			best = wait
			reason = why
		}
	}

	for _, account := range p.Accounts() {
		uid := strings.TrimSpace(account.UID())
		if uid == "" || (exclude != nil && exclude[uid]) {
			continue
		}
		account.mu.RLock()
		eligible := !account.removed &&
			account.Auth.Realm == p.realm &&
			account.Auth.Enabled &&
			!account.State.Disabled &&
			!account.tokenExpired &&
			!account.State.CoolUntil.After(now) &&
			strings.TrimSpace(account.Auth.AccessToken) != ""
		if eligible && model != "" && proOnly[lower] && !account.paidUserLocked() {
			eligible = false
		}
		if eligible && model != "" {
			if account.DisabledModels[lower] {
				eligible = false
			} else {
				for blocked := range account.DisabledModels {
					if strings.EqualFold(strings.TrimSpace(blocked), lower) {
						eligible = false
						break
					}
				}
			}
		}
		if !eligible {
			account.mu.RUnlock()
			continue
		}
		candidates++
		modelWait := time.Duration(0)
		if model != "" && account.modelCoolingLocked(lower, now) {
			if until, ok := account.modelCoolUntil[lower]; ok {
				modelWait = until.Sub(now)
			}
		}
		busy := account.maxConcurrent > 0 && account.State.ActiveConcurrent >= account.maxConcurrent
		account.mu.RUnlock()

		if modelWait > 0 {
			consider(modelWait, "every usable WorkBuddy account is waiting for the upstream model window to reset")
			continue
		}
		if busy {
			consider(time.Second, "every usable WorkBuddy account is already serving its concurrency cap")
			continue
		}
		if guard := account.GuardBlocked(model); guard != "" {
			consider(guardAdmissionWait(guard, now, midnight), guardAdmissionReason(guard))
			continue
		}
		usable = true
	}
	if usable || candidates == 0 {
		return 0, ""
	}
	return best, reason
}

func guardAdmissionWait(guard string, now, midnight time.Time) time.Duration {
	switch guard {
	case "daily_token", "daily_credit", "model_daily_token":
		if wait := midnight.Sub(now); wait > 0 {
			return wait
		}
		return 24 * time.Hour
	case "reserve":
		return 60 * time.Second
	default:
		return 60 * time.Second
	}
}

func guardAdmissionReason(guard string) string {
	switch guard {
	case "reserve":
		return "every usable WorkBuddy account is below the reserve credit floor"
	case "daily_token":
		return "every usable WorkBuddy account reached today's token limit; the pool resumes after local midnight"
	case "daily_credit":
		return "every usable WorkBuddy account reached today's credit limit; the pool resumes after local midnight"
	case "model_daily_token":
		return "every usable WorkBuddy account reached today's model token limit; the pool resumes after local midnight"
	default:
		return "WorkBuddy account guards are blocking every usable account"
	}
}

// ClearCooldown clears the account cooldown and consecutive-error count. The
// credential file is untouched because these fields are runtime-only.
func (p *Pool) ClearCooldown(uid string) error {
	account := p.Get(uid)
	if account == nil {
		return ErrAccountNotFound
	}
	account.mu.Lock()
	account.State.CoolUntil = time.Time{}
	account.State.ErrCount = 0
	account.State.LastError = ""
	account.clearModelCooldownsLocked("")
	account.mu.Unlock()
	return nil
}

// SetNote stores a trimmed 200-character operator note.
func (p *Pool) SetNote(uid, note string) error {
	return p.mutateAccount(uid, func(account *Account) {
		note = truncateText(strings.TrimSpace(note), maxNoteRunes)
		account.Auth.Note = note
		account.State.Note = note
	})
}

// SetTasks toggles scheduled/manual task participation without changing API
// dispatch. SetTasksEnabled is the v1-shaped alias.
func (p *Pool) SetTasks(uid string, enabled bool) error {
	return p.mutateAccount(uid, func(account *Account) {
		account.Auth.SetTasksEnabled(enabled)
		account.State.TasksEnabled = enabled
	})
}

// SetTasksEnabled is an alias for SetTasks.
func (p *Pool) SetTasksEnabled(uid string, enabled bool) error {
	return p.SetTasks(uid, enabled)
}

// SetWeight sets the manual scheduler weight in [1, 100]. Weights above 1 take
// priority over all automatic preferences.
func (p *Pool) SetWeight(uid string, weight float64) error {
	return p.mutateAccount(uid, func(account *Account) {
		value := normalizedWeight(weight)
		account.Weight = float64(value)
		account.Auth.Weight = value
	})
}

// SetDisabledModels replaces the operator's per-account model ban list. Ids
// are trimmed, lowercased, deduplicated, and keep their input order.
func (p *Pool) SetDisabledModels(uid string, models []string) error {
	return p.mutateAccount(uid, func(account *Account) {
		values := make([]any, 0, len(models))
		for _, model := range models {
			values = append(values, model)
		}
		normalized := normalizeDisabledModels(values)
		if normalized == nil {
			normalized = []string{}
		}
		account.Auth.DisabledModels = normalized
		account.DisabledModels = disabledModelSet(normalized)
	})
}

// SetTier stores the panel/billing tier document. A nil value clears it; a
// string and a map are both accepted and persisted as JSON. The account's
// scheduling mirror and PRO flag are refreshed from the stored value.
func (p *Pool) SetTier(uid string, tier any) error {
	return p.mutateAccount(uid, func(account *Account) {
		applyTierLocked(account, tier)
	})
}

func applyTierLocked(account *Account, tier any) {
	raw := rawTierValue(tier)
	account.Auth.Tier = raw
	label, paid, known := tierLabel(raw)
	if !known && account.Credits != nil && account.Credits.IsPaidUser {
		label, paid = "pro", true
	}
	account.Tier = label
	account.paidUser = paid
	account.paidKnown = known
}

func rawTierValue(tier any) json.RawMessage {
	switch value := tier.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return copyRawJSON(value)
	case []byte:
		return copyRawJSON(json.RawMessage(value))
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		return encoded
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		return encoded
	}
}

// SetProxySlot persists one account's proxy-slot binding and resolves the
// runtime URL under the same write lock, so a concurrent slot update cannot
// leave the account pointing at a different proxy than the one it stored.
//
// An empty slot id is an explicit "direct" request: it clears the persisted
// slot and v1's legacy proxy extra, and sets the runtime proxy to direct. A
// non-empty id that is missing or disabled still falls back to the legacy
// proxy, matching v1's apply_proxy_slots.
func (p *Pool) SetProxySlot(uid, slotID string) error {
	if p == nil {
		return ErrAccountNotFound
	}
	slotID = strings.TrimSpace(slotID)

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	account := p.Get(uid)
	if account == nil {
		return ErrAccountNotFound
	}
	p.mu.RLock()
	slots := append(transport.Slots(nil), p.slots...)
	p.mu.RUnlock()

	account.mu.Lock()
	if account.removed {
		account.mu.Unlock()
		return ErrAccountNotFound
	}
	before := cloneAuth(account.Auth)
	account.Auth.ProxySlot = slotID
	runtimeProxy := ""
	if slotID == "" {
		delete(account.Auth.Extra, "proxy")
	} else {
		runtimeProxy = resolveProxyForSlots(slots, slotID, account.Auth.Extra)
	}
	auth := authSnapshotLocked(account)
	account.mu.Unlock()

	saved, err := SaveAccount(p.dir, auth)
	if err != nil {
		account.mu.Lock()
		account.Auth = before
		account.mu.Unlock()
		return err
	}

	account.mu.Lock()
	account.Auth = saved
	account.Path = saved.Path
	account.Auth.ProxyURL = runtimeProxy
	account.mu.Unlock()
	return nil
}

func (p *Pool) resolveProxySlot(slotID string, extra map[string]json.RawMessage) string {
	if p != nil {
		p.mu.RLock()
		slots := append(transport.Slots(nil), p.slots...)
		p.mu.RUnlock()
		if url := slots.URL(slotID); url != "" {
			return url
		}
	}
	return legacyProxy(extra)
}

func legacyProxy(extra map[string]json.RawMessage) string {
	raw, ok := extra["proxy"]
	if !ok {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ""
	}
	return strings.TrimSpace(text)
}

// ApplyProxySlots replaces the slot list and re-resolves every account's
// runtime proxy. A missing/disabled slot falls back to the account's legacy
// proxy value; the persisted slot id is untouched.
func (p *Pool) ApplyProxySlots(slots transport.Slots) {
	if p == nil {
		return
	}
	copied := append(transport.Slots(nil), slots...)
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	p.slots = copied
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, account := range accounts {
		account.mu.Lock()
		account.Auth.ProxyURL = resolveProxyForSlots(copied, account.Auth.ProxySlot, account.Auth.Extra)
		account.mu.Unlock()
	}
}

func resolveProxyForSlots(slots transport.Slots, slotID string, extra map[string]json.RawMessage) string {
	if url := slots.URL(slotID); url != "" {
		return url
	}
	return legacyProxy(extra)
}

// ApplyLimits projects the shared guard values and today's usage onto every
// account. The implementation lives in credits.go with the rest of Task 6's
// credit/guard logic.

// Add imports or replaces one account in this realm's pool. Re-import keeps
// the operator's stable choices (manual enable state, tasks, note, group,
// weight, disabled models, proxy slot, tier, timestamps, and unknown extras)
// while accepting rotated credentials.
func (p *Pool) Add(auth Auth) (*Account, error) {
	if p == nil {
		return nil, errors.New("workbuddy: pool is nil")
	}
	normalized, err := normalizeAuth(auth)
	if err != nil {
		return nil, err
	}
	if normalized.Realm != p.realm {
		return nil, fmt.Errorf("%w: got %s want %s", ErrRealmMismatch, normalized.Realm, p.realm)
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	existing := p.Get(normalized.UID)
	if existing != nil {
		existing.mu.RLock()
		old := cloneAuth(existing.Auth)
		oldCredits := cloneCreditSnapshot(existing.Credits)
		oldModels := append([]providers.Model(nil), existing.Models...)
		oldActive := existing.State.ActiveConcurrent
		oldLastUsed := existing.State.LastUsed
		existing.mu.RUnlock()

		preserveStableAuth(&normalized, old)
		saved, err := SaveAccount(p.dir, normalized)
		if err != nil {
			return nil, err
		}

		credits := decodeCreditSnapshot(saved.Extra["credits"])
		if credits == nil {
			credits = oldCredits
		}
		label, paid, known := tierLabel(saved.Tier)
		if !known && credits != nil && credits.IsPaidUser {
			label, paid = "pro", true
		}
		maxConcurrent := p.accountMaxConcurrentLocked(p.Config())
		expiringWindow := p.expiringWindow()
		existing.mu.Lock()
		if existing.removed {
			existing.mu.Unlock()
			return nil, ErrAccountNotFound
		}
		existing.Auth = saved
		existing.Auth.ProxyURL = p.resolveProxySlot(saved.ProxySlot, saved.Extra)
		existing.Path = saved.Path
		existing.Credits = credits
		existing.DisabledModels = disabledModelSet(saved.DisabledModels)
		existing.Group = saved.Group
		existing.Weight = float64(saved.Weight)
		existing.Tier = label
		existing.paidUser = paid
		existing.paidKnown = known
		existing.Models = oldModels
		existing.tokenExpired = false
		existing.State.Disabled = !saved.Enabled
		existing.State.ManualDisabled = !saved.Enabled
		existing.State.TasksEnabled = saved.TasksEnabled
		existing.State.Note = saved.Note
		existing.State.Reason = ""
		existing.State.LastError = ""
		existing.State.ErrCount = 0
		existing.State.CoolUntil = time.Time{}
		existing.State.ActiveConcurrent = oldActive
		existing.State.LastUsed = oldLastUsed
		existing.maxConcurrent = maxConcurrent
		existing.State.MaxConcurrent = maxConcurrent
		existing.ExpiringWindowDays = expiringWindow
		existing.mu.Unlock()
		return existing, nil
	}

	saved, err := SaveAccount(p.dir, normalized)
	if err != nil {
		return nil, err
	}
	maxConcurrent := p.accountMaxConcurrentLocked(p.Config())
	account := p.newAccount(saved, maxConcurrent)
	p.mu.Lock()
	p.accounts = append(p.accounts, account)
	sort.Slice(p.accounts, func(i, j int) bool {
		return strings.TrimSpace(p.accounts[i].Auth.UID) < strings.TrimSpace(p.accounts[j].Auth.UID)
	})
	p.mu.Unlock()
	return account, nil
}

func preserveStableAuth(auth *Auth, old Auth) {
	if auth == nil {
		return
	}
	if !old.AddedAt.IsZero() {
		auth.AddedAt = old.AddedAt
	}
	if !auth.EnabledSet {
		auth.Enabled = old.Enabled
		auth.EnabledSet = true
	}
	if !auth.TasksEnabledSet {
		auth.TasksEnabled = old.TasksEnabled
		auth.TasksEnabledSet = true
	}
	if strings.TrimSpace(auth.Note) == "" {
		auth.Note = old.Note
	}
	if strings.TrimSpace(auth.Group) == "" {
		auth.Group = old.Group
	}
	if auth.Weight <= minWeight && old.Weight > minWeight {
		auth.Weight = old.Weight
	}
	if len(auth.DisabledModels) == 0 && len(old.DisabledModels) > 0 {
		auth.DisabledModels = append([]string(nil), old.DisabledModels...)
	}
	if strings.TrimSpace(auth.ProxySlot) == "" {
		auth.ProxySlot = old.ProxySlot
	}
	if len(strings.TrimSpace(string(auth.Tier))) == 0 && len(old.Tier) > 0 {
		auth.Tier = append(json.RawMessage(nil), old.Tier...)
	}
	if auth.LastCheckin == "" {
		auth.LastCheckin = old.LastCheckin
	}
	if auth.LastDailyChat == "" {
		auth.LastDailyChat = old.LastDailyChat
	}
	if auth.LastActivityReport == "" {
		auth.LastActivityReport = old.LastActivityReport
	}
	if auth.Extra == nil {
		auth.Extra = make(map[string]json.RawMessage)
	}
	for key, value := range old.Extra {
		if _, exists := auth.Extra[key]; exists {
			continue
		}
		auth.Extra[key] = append(json.RawMessage(nil), value...)
	}
}

// Remove deletes an account's credential file(s), affinity bindings, and pool
// membership. A sanitized filename is removed only when its embedded UID
// verifiably belongs to this account.
func (p *Pool) Remove(uid string) error {
	if p == nil {
		return ErrAccountNotFound
	}
	account := p.Get(uid)
	if account == nil {
		return ErrAccountNotFound
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	account.mu.Lock()
	if account.removed {
		account.mu.Unlock()
		return ErrAccountNotFound
	}
	account.removed = true
	accountUID := strings.TrimSpace(account.Auth.UID)
	account.mu.Unlock()

	paths, err := p.authPathsForUID(accountUID)
	if err != nil {
		account.mu.Lock()
		account.removed = false
		account.mu.Unlock()
		return err
	}
	for _, path := range paths {
		if err := removeAccountFile(path); err != nil {
			account.mu.Lock()
			account.removed = false
			account.mu.Unlock()
			return err
		}
	}

	p.mu.Lock()
	remaining := p.accounts[:0]
	for _, candidate := range p.accounts {
		if candidate != account {
			remaining = append(remaining, candidate)
		}
	}
	p.accounts = remaining
	p.mu.Unlock()
	if p.affinity != nil {
		p.affinity.dropUID(accountUID)
	}
	return nil
}

func (p *Pool) authPathsForUID(uid string) ([]string, error) {
	uid = strings.TrimSpace(uid)
	paths := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	add := func(path string) {
		if path == "" {
			return
		}
		clean := filepath.Clean(path)
		if seen[clean] {
			return
		}
		seen[clean] = true
		paths = append(paths, path)
	}

	entries, err := os.ReadDir(p.dir)
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workbuddy: read accounts dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || isReservedAccountFile(name) {
			continue
		}
		path := filepath.Join(p.dir, name)
		auth, loadErr := LoadAccount(path)
		if loadErr != nil {
			continue
		}
		if strings.TrimSpace(auth.UID) == uid && auth.Realm == p.realm {
			add(path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

type affinityEntry struct {
	uid     string
	expires time.Time
}

// sessionAffinity is v1's short per-conversation most-recent list. The front
// entry is the account the last turn used; the entries behind it are earlier
// warm accounts, so a cooling front entry can fall back without rebuilding the
// upstream prompt cache from scratch.
type sessionAffinity struct {
	ttl        time.Duration
	maxEntries int
	history    int
	now        func() time.Time

	mu       sync.Mutex
	bindings map[string][]affinityEntry
	order    []string
}

func newSessionAffinity(ttl time.Duration, maxEntries, history int) *sessionAffinity {
	if ttl <= 0 {
		ttl = affinityTTL
	}
	if maxEntries <= 0 {
		maxEntries = maxAffinityKeys
	}
	if history < 1 {
		history = 1
	}
	return &sessionAffinity{
		ttl:        ttl,
		maxEntries: maxEntries,
		history:    history,
		now:        time.Now,
		bindings:   make(map[string][]affinityEntry),
	}
}

func (s *sessionAffinity) clock() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *sessionAffinity) liveLocked(key string, now time.Time) []affinityEntry {
	entries := s.bindings[key]
	if len(entries) == 0 {
		return nil
	}
	live := entries[:0]
	for _, entry := range entries {
		if entry.expires.After(now) {
			live = append(live, entry)
		}
	}
	if len(live) == 0 {
		delete(s.bindings, key)
		return nil
	}
	s.bindings[key] = live
	return live
}

func (s *sessionAffinity) recent(key string) []string {
	key = strings.TrimSpace(key)
	if s == nil || key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveLocked(key, s.clock())
	uids := make([]string, 0, len(live))
	for _, entry := range live {
		uids = append(uids, entry.uid)
	}
	return uids
}

func (s *sessionAffinity) bind(key, uid string) {
	key = strings.TrimSpace(key)
	uid = strings.TrimSpace(uid)
	if s == nil || key == "" || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	s.gcLocked(now)
	entries := make([]affinityEntry, 0, s.history)
	entries = append(entries, affinityEntry{uid: uid, expires: now.Add(s.ttl)})
	for _, entry := range s.liveLocked(key, now) {
		if entry.uid == uid || len(entries) >= s.history {
			continue
		}
		entries = append(entries, entry)
	}
	if _, exists := s.bindings[key]; !exists {
		s.order = append(s.order, key)
	}
	s.bindings[key] = entries
}

func (s *sessionAffinity) gcLocked(now time.Time) {
	if len(s.bindings) < s.maxEntries {
		return
	}
	for key := range s.bindings {
		s.liveLocked(key, now)
	}
	kept := s.order[:0]
	for _, key := range s.order {
		if _, ok := s.bindings[key]; ok {
			kept = append(kept, key)
		}
	}
	s.order = kept
	for len(s.bindings) >= s.maxEntries && len(s.order) > 0 {
		delete(s.bindings, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *sessionAffinity) demote(key, uid string) {
	key = strings.TrimSpace(key)
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.liveLocked(key, s.clock())
	if len(entries) == 0 {
		return
	}
	kept := make([]affinityEntry, 0, len(entries))
	for index, entry := range entries {
		if uid == "" && index == 0 {
			continue
		}
		if uid != "" && entry.uid == uid {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		delete(s.bindings, key)
		return
	}
	s.bindings[key] = kept
}

func (s *sessionAffinity) unbind(key string) {
	key = strings.TrimSpace(key)
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	delete(s.bindings, key)
	s.mu.Unlock()
}

func (s *sessionAffinity) dropUID(uid string) {
	uid = strings.TrimSpace(uid)
	if s == nil || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entries := range s.bindings {
		kept := make([]affinityEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.uid != uid {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(s.bindings, key)
			continue
		}
		s.bindings[key] = kept
	}
}
