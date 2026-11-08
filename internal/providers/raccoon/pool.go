package raccoon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	// softCooldown and errCooldown mirror v1's 429/5xx and repeated-error
	// backoff. errThreshold is the number of ordinary failures that triggers a
	// cooldown.
	softCooldown = 60 * time.Second
	errThreshold = 3
	errCooldown  = 600 * time.Second

	maxAffinityEntries = 4096
	affinityEvictCount = 1024
)

var (
	// ErrNoAccount means every account was excluded, unhealthy, guarded, or at
	// its concurrency limit.
	ErrNoAccount = errors.New("raccoon: no account available")

	// ErrAccountNotFound means the requested UID is not in the pool.
	ErrAccountNotFound = errors.New("raccoon: account not found")

	// ErrAuthPathCollision means a save would overwrite an auth file belonging
	// to a different embedded UID.
	ErrAuthPathCollision = errors.New("raccoon: auth path belongs to a different uid")
)

// Pool is the provider-owned Raccoon account pool. It owns auth loading,
// generic state persistence, scheduler guards, session affinity, and proxy
// slot resolution.
type Pool struct {
	dir       string
	folder    string
	statePath string

	mu       sync.RWMutex
	cfg      Config
	accounts []*Account
	slots    transport.Slots
	affinity map[string]string
	order    []string

	// preserved keeps state entries whose auth file is temporarily missing or
	// malformed. A later save must not silently wipe notes, manual disables,
	// task toggles, or unknown fields for those accounts.
	preserved map[string]accountstate.State
}

// PoolStats is the v1 overview counter shape.
type PoolStats struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Disabled int `json:"disabled"`
	Cooling  int `json:"cooling"`
	Limited  int `json:"limited"`
}

// NewPool loads every valid auth document under dir/raccoon, merges
// dir/raccoon/state.json, and applies cfg.MaxConcurrent.
//
// Auth files are independent: a malformed document is skipped. The state file
// is not independent, because silently replacing an unrecognized shape would
// destroy manual disables and notes; an unreadable, malformed, list-shaped, or
// non-object state document is therefore an error and is never written.
func NewPool(dir string, cfg Config) (*Pool, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("raccoon: accounts directory is empty")
	}
	dir = filepath.Clean(dir)

	p := &Pool{
		dir:       dir,
		folder:    filepath.Join(dir, "raccoon"),
		statePath: filepath.Join(dir, "raccoon", "state.json"),
		cfg:       cfg,
		affinity:  make(map[string]string),
		preserved: make(map[string]accountstate.State),
	}

	rawState, hasState, err := readStateDocument(p.statePath)
	if err != nil {
		return nil, err
	}
	states := make(map[string]accountstate.State)
	if hasState {
		states, err = accountstate.LoadStore(p.statePath)
		if err != nil {
			return nil, fmt.Errorf("raccoon: load state: %w", err)
		}
		// LoadStore skips an entry it cannot decode. Refuse to continue rather
		// than saving over data that was not understood.
		for uid := range rawState {
			if strings.TrimSpace(uid) == "" {
				continue
			}
			if _, ok := states[uid]; !ok {
				return nil, fmt.Errorf("raccoon: state entry %q is not a supported object", uid)
			}
		}
	}

	auths, err := p.loadAuthFiles()
	if err != nil {
		return nil, err
	}

	maxConcurrent := maxConcurrentFor(cfg)
	accounts := make([]*Account, 0, len(auths))
	loaded := make(map[string]bool, len(auths))
	for _, loadedAuth := range auths {
		uid := strings.TrimSpace(loadedAuth.auth.UID)
		state, ok := states[uid]
		if !ok {
			state = accountstate.State{UID: uid, TasksEnabled: true}
		} else {
			migrateState(&state, stateEntryFields(rawState[uid]))
		}
		state.UID = uid
		state.ActiveConcurrent = 0
		state.MaxConcurrent = maxConcurrent

		account := &Account{
			Auth:          loadedAuth.auth,
			State:         state,
			Path:          loadedAuth.path,
			proxySlot:     strings.TrimSpace(loadedAuth.auth.ProxySlot),
			pool:          p,
			maxConcurrent: maxConcurrent,
		}
		accounts = append(accounts, account)
		loaded[uid] = true
	}
	sort.Slice(accounts, func(i, j int) bool {
		return strings.TrimSpace(accounts[i].Auth.UID) < strings.TrimSpace(accounts[j].Auth.UID)
	})

	// Keep state for accounts whose auth file is currently absent/malformed.
	if hasState {
		for uid, entry := range rawState {
			uid = strings.TrimSpace(uid)
			if uid == "" || loaded[uid] {
				continue
			}
			state := states[uid]
			migrateState(&state, stateEntryFields(entry))
			state.UID = uid
			state.ActiveConcurrent = 0
			p.preserved[uid] = state
		}
	}

	p.accounts = accounts
	return p, nil
}

func maxConcurrentFor(cfg Config) int {
	return boundedInt(cfg.MaxConcurrent, defaultMaxConcurrent, minMaxConcurrent, maxMaxConcurrent)
}

// readStateDocument validates the top-level state shape without interpreting
// account fields. It returns hasFile=false for a missing file.
func readStateDocument(path string) (map[string]json.RawMessage, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("raccoon: read state file %s: %w", path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("raccoon: state file %s is not a JSON object: %w", path, err)
	}
	if raw == nil {
		return nil, false, fmt.Errorf("raccoon: state file %s is not a JSON object", path)
	}
	for uid, entry := range raw {
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, false, fmt.Errorf("raccoon: state entry %q is not a JSON object", uid)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			return nil, false, fmt.Errorf("raccoon: decode state entry %q: %w", uid, err)
		}
	}
	return raw, true, nil
}

func stateEntryFields(raw json.RawMessage) map[string]json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return fields
}

// migrateState applies v1 load semantics that the generic store cannot know:
// tasks_enabled defaults to true, and pre-manual_disabled state tagged manual
// disables through disabled_by/reason.
func migrateState(state *accountstate.State, fields map[string]json.RawMessage) {
	if state == nil {
		return
	}
	if _, ok := fields["tasks_enabled"]; !ok {
		state.TasksEnabled = true
	}
	if state.Disabled && !state.ManualDisabled {
		switch {
		case state.DisabledBy == "manual" || state.Reason == "manual disable":
			state.ManualDisabled = true
			state.DisabledBy = "manual"
		case state.DisabledBy == "":
			state.DisabledBy = "auth"
		}
	}
	// A manual marker without Disabled would make the panel say enabled while
	// the scheduler refuses the account. Repair the inconsistent pair so a
	// later token refresh cannot leave it half-cleared.
	if state.ManualDisabled && !state.Disabled {
		state.Disabled = true
		if state.DisabledBy == "" {
			state.DisabledBy = "manual"
		}
		if state.Reason == "" {
			state.Reason = "manual disable"
		}
	}
	state.Note = truncateRunes(state.Note, maxNoteRunes)
}

type loadedAuth struct {
	auth      Auth
	path      string
	canonical bool
}

// loadAuthFiles reads raccoon-*.json documents deterministically. The embedded
// UID (stored or derived from the token) is authoritative; a canonical
// AuthPath file wins over a duplicate alias for the same UID.
func (p *Pool) loadAuthFiles() ([]loadedAuth, error) {
	entries, err := os.ReadDir(p.folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("raccoon: read accounts directory %s: %w", p.folder, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "raccoon-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	loaded := make([]loadedAuth, 0, len(names))
	for _, name := range names {
		path := filepath.Join(p.folder, name)
		auth, err := LoadAuth(path)
		if err != nil {
			continue
		}
		uid := strings.TrimSpace(auth.UID)
		if uid == "" {
			continue
		}
		canonicalPath, err := AuthPath(p.dir, uid)
		if err != nil {
			continue
		}
		loaded = append(loaded, loadedAuth{
			auth:      auth,
			path:      path,
			canonical: filepath.Clean(canonicalPath) == filepath.Clean(path),
		})
	}

	sort.Slice(loaded, func(i, j int) bool {
		leftUID := strings.TrimSpace(loaded[i].auth.UID)
		rightUID := strings.TrimSpace(loaded[j].auth.UID)
		if leftUID != rightUID {
			return leftUID < rightUID
		}
		if loaded[i].canonical != loaded[j].canonical {
			return loaded[i].canonical
		}
		return loaded[i].path < loaded[j].path
	})

	unique := loaded[:0]
	seen := make(map[string]bool, len(loaded))
	for _, item := range loaded {
		uid := strings.TrimSpace(item.auth.UID)
		if seen[uid] {
			continue
		}
		seen[uid] = true
		unique = append(unique, item)
	}
	return unique, nil
}

// SaveState writes dir/raccoon/state.json atomically, preserving state for
// accounts whose auth files are not currently loaded and every unknown field
// carried by accountstate.State.
func (p *Pool) SaveState() error {
	if p == nil {
		return errors.New("raccoon: pool is nil")
	}

	p.mu.RLock()
	accounts := append([]*Account(nil), p.accounts...)
	preserved := make(map[string]accountstate.State, len(p.preserved))
	for uid, state := range p.preserved {
		preserved[uid] = state
	}
	statePath := p.statePath
	p.mu.RUnlock()

	// Re-validate the on-disk shape before replacing it. A file changed to a
	// list or malformed document while the pool was running belongs to another
	// writer and must not be silently destroyed.
	if _, _, err := readStateDocument(statePath); err != nil {
		return err
	}

	states := make(map[string]accountstate.State, len(accounts)+len(preserved))
	for uid, state := range preserved {
		states[uid] = state
	}
	for _, account := range accounts {
		account.mu.Lock()
		uid := strings.TrimSpace(account.Auth.UID)
		if uid == "" {
			uid = strings.TrimSpace(account.State.UID)
		}
		state := account.State
		state.UID = uid
		state.ActiveConcurrent = 0
		state.MaxConcurrent = normalizedMaxConcurrent(account.maxConcurrent)
		account.mu.Unlock()
		if uid == "" {
			continue
		}
		states[uid] = state
	}

	if err := accountstate.SaveStore(statePath, states); err != nil {
		return fmt.Errorf("raccoon: save state: %w", err)
	}
	return nil
}

// Accounts returns a snapshot of account pointers.
func (p *Pool) Accounts() []*Account {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Account(nil), p.accounts...)
}

// Get returns the account whose embedded auth UID or state UID matches uid.
func (p *Pool) Get(uid string) *Account {
	if p == nil {
		return nil
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil
	}
	for _, account := range p.Accounts() {
		account.mu.Lock()
		match := strings.TrimSpace(account.Auth.UID) == uid ||
			strings.TrimSpace(account.State.UID) == uid
		account.mu.Unlock()
		if match {
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

// Stats returns v1 overview counters.
func (p *Pool) Stats() PoolStats {
	stats := PoolStats{}
	if p == nil {
		return stats
	}
	now := time.Now()
	for _, account := range p.Accounts() {
		account.mu.Lock()
		disabled := account.State.Disabled || account.State.ManualDisabled
		cooling := !disabled && now.Before(account.State.CoolUntil)
		guard := account.guardBlockedLocked("")
		account.mu.Unlock()

		stats.Total++
		switch {
		case disabled:
			stats.Disabled++
		case cooling:
			stats.Cooling++
		case guard != "":
			stats.Limited++
		default:
			stats.Healthy++
		}
	}
	return stats
}

// Pick chooses a ready account under its concurrency limit: session affinity
// first, then model-capable candidates, then least busy and least recently
// used with UID as the deterministic tie breaker.
//
// Matching v1, when no ready account claims the model, non-capable accounts
// remain eligible. This keeps a missing/stale catalog from taking the provider
// offline; HasModel is still consulted whenever any candidate knows the model.
func (p *Pool) Pick(model, sessionKey string, exclude map[string]bool) (*Account, error) {
	if p == nil {
		return nil, ErrNoAccount
	}
	now := time.Now()

	affinityUID := ""
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey != "" {
		p.mu.RLock()
		affinityUID = p.affinity[sessionKey]
		p.mu.RUnlock()
	}

	accounts := p.Accounts()
	if affinityUID != "" && !exclude[affinityUID] {
		for _, account := range accounts {
			uid, active, max, _ := account.hasSelectionState()
			if uid != affinityUID || active >= max {
				continue
			}
			if account.Ready(now, model) && account.HasModel(model) {
				return account, nil
			}
		}
	}

	type candidate struct {
		account  *Account
		uid      string
		active   int
		lastUsed time.Time
		capable  bool
	}
	candidates := make([]candidate, 0, len(accounts))
	for _, account := range accounts {
		uid, active, max, lastUsed := account.hasSelectionState()
		if exclude[uid] || active >= max {
			continue
		}
		if !account.Ready(now, model) {
			continue
		}
		candidates = append(candidates, candidate{
			account:  account,
			uid:      uid,
			active:   active,
			lastUsed: lastUsed,
			capable:  account.HasModel(model),
		})
	}
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}

	selected := candidates
	capable := make([]candidate, 0, len(candidates))
	for _, item := range candidates {
		if item.capable {
			capable = append(capable, item)
		}
	}
	if len(capable) > 0 {
		selected = capable
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].active != selected[j].active {
			return selected[i].active < selected[j].active
		}
		if !selected[i].lastUsed.Equal(selected[j].lastUsed) {
			return selected[i].lastUsed.Before(selected[j].lastUsed)
		}
		return selected[i].uid < selected[j].uid
	})
	return selected[0].account, nil
}

// BindSession records session affinity for the next Pick. The map is bounded;
// the oldest entries are evicted when it grows.
func (p *Pool) BindSession(sessionKey, uid string) {
	sessionKey = strings.TrimSpace(sessionKey)
	uid = strings.TrimSpace(uid)
	if p == nil || sessionKey == "" || uid == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.affinity == nil {
		p.affinity = make(map[string]string)
	}
	if _, exists := p.affinity[sessionKey]; !exists {
		p.order = append(p.order, sessionKey)
	}
	p.affinity[sessionKey] = uid
	if len(p.affinity) > maxAffinityEntries {
		evict := affinityEvictCount
		if evict > len(p.order) {
			evict = len(p.order)
		}
		for _, key := range p.order[:evict] {
			delete(p.affinity, key)
		}
		p.order = append([]string(nil), p.order[evict:]...)
	}
}

// renameAffinity remaps session bindings after a token refresh rotates the
// account UID. The canonical *Account pointer is unchanged.
func (p *Pool) renameAffinity(oldUID, newUID string) {
	if p == nil || oldUID == "" || newUID == "" || oldUID == newUID {
		return
	}
	p.mu.Lock()
	for key, bound := range p.affinity {
		if bound == oldUID {
			p.affinity[key] = newUID
		}
	}
	delete(p.preserved, oldUID)
	p.mu.Unlock()
}

// Acquire reserves one concurrency slot. It does not re-check health; Pick
// already did.
func (p *Pool) Acquire(a *Account) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	max := normalizedMaxConcurrent(a.maxConcurrent)
	if a.activeConcurrent >= max {
		return false
	}
	a.activeConcurrent++
	a.lastUsed = time.Now()
	return true
}

// Release returns a concurrency slot, clamped at zero.
func (p *Pool) Release(a *Account) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.activeConcurrent > 0 {
		a.activeConcurrent--
	}
	a.mu.Unlock()
}

// Disable applies a manual disable. The manual marker keeps a token refresh
// from clearing it.
func (p *Pool) Disable(uid, reason string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "manual disable"
	}
	account.mu.Lock()
	account.State.Disabled = true
	account.State.ManualDisabled = true
	account.State.DisabledBy = "manual"
	account.State.Reason = reason
	account.State.LastError = reason
	account.mu.Unlock()
	return p.SaveState()
}

// Enable clears a manual disable, cooldown, and error counters.
func (p *Pool) Enable(uid string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	account.mu.Lock()
	account.State.Disabled = false
	account.State.ManualDisabled = false
	account.State.DisabledBy = ""
	account.State.Reason = ""
	account.State.LastError = ""
	account.State.ErrCount = 0
	account.State.CoolUntil = time.Time{}
	account.mu.Unlock()
	return p.SaveState()
}

// ClearCooldown clears the cooldown and consecutive-error count.
func (p *Pool) ClearCooldown(uid string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	account.mu.Lock()
	account.State.CoolUntil = time.Time{}
	account.State.ErrCount = 0
	account.mu.Unlock()
	return p.SaveState()
}

// SetNote stores a trimmed, 200-character note.
func (p *Pool) SetNote(uid, note string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	account.mu.Lock()
	account.State.Note = truncateRunes(strings.TrimSpace(note), maxNoteRunes)
	account.mu.Unlock()
	return p.SaveState()
}

// SetTasksEnabled toggles auto task/reward behavior for one account.
func (p *Pool) SetTasksEnabled(uid string, enabled bool) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	account.mu.Lock()
	account.State.TasksEnabled = enabled
	account.mu.Unlock()
	return p.SaveState()
}

func (p *Pool) accountForMutation(uid string) (*Account, error) {
	if p == nil {
		return nil, ErrAccountNotFound
	}
	account := p.Get(uid)
	if account == nil {
		return nil, ErrAccountNotFound
	}
	return account, nil
}

// SetProxySlot persists proxySlot in the account's auth document and resolves
// it against the current slot list. An empty slot id clears the binding.
func (p *Pool) SetProxySlot(uid, slotID string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	slotID = strings.TrimSpace(slotID)

	account.mu.Lock()
	path := account.Path
	currentUID := strings.TrimSpace(account.Auth.UID)
	auth := account.Auth
	account.mu.Unlock()
	if path == "" {
		return errors.New("raccoon: account has no auth path")
	}
	auth.ProxySlot = slotID
	if err := p.verifyWritableAuth(path, currentUID); err != nil {
		return err
	}
	if err := SaveAuth(path, auth); err != nil {
		return fmt.Errorf("raccoon: save proxy slot: %w", err)
	}
	saved, err := LoadAuth(path)
	if err != nil {
		return fmt.Errorf("raccoon: reload proxy slot: %w", err)
	}

	p.mu.RLock()
	proxy := p.slots.URL(slotID)
	p.mu.RUnlock()

	account.mu.Lock()
	account.Auth = saved
	account.proxySlot = slotID
	account.proxy = proxy
	account.mu.Unlock()
	return nil
}

// SetProxySlots replaces the slot list and resolves every account binding.
// Disabled or unknown slots resolve to direct, matching v1.
func (p *Pool) SetProxySlots(slots []transport.Slot) {
	if p == nil {
		return
	}
	copied := append(transport.Slots(nil), slots...)
	p.mu.Lock()
	p.slots = copied
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()

	for _, account := range accounts {
		account.mu.Lock()
		account.proxy = copied.URL(account.proxySlot)
		account.mu.Unlock()
	}
}

// ProxyURL returns the resolved raw proxy URL for the token HTTP client. It is
// never part of a view; callers must not log it.
func (p *Pool) ProxyURL(a *Account) string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxy
}

// NewTokenManager builds a refresh manager wired to this pool. When opts does
// not name a proxy resolver, refresh requests follow each account's resolved
// proxy slot. Account state transitions (terminal auth failure, non-manual
// recovery, and a rotated JWT subject) are persisted to state.json
// automatically. BaseURL, Timeout, and Now come from opts; the default timeout
// remains v1's 30-second refresh timeout.
func (p *Pool) NewTokenManager(client *transport.Client, opts TokenManagerOptions) *TokenManager {
	if opts.ProxyURL == nil && p != nil {
		opts.ProxyURL = p.ProxyURL
	}
	manager := NewTokenManager(client, opts)
	if p != nil {
		manager.OnStateChange = func(*Account) { _ = p.SaveState() }
	}
	return manager
}

// SetConfig replaces the pool configuration and applies max_concurrent to
// every account.
func (p *Pool) SetConfig(cfg Config) {
	if p == nil {
		return
	}
	max := maxConcurrentFor(cfg)
	p.mu.Lock()
	p.cfg = cfg
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, account := range accounts {
		account.setMaxConcurrent(max)
	}
}

// ApplyLimits projects the shared guard values onto every account.
//
// values mirrors v1's grouped limit values:
//
//	{"reserve_credits": {"global": 10}, ...}
//
// usage mirrors v1's daily fold:
//
//	{"tokens": {uid: 12}, "credits": {uid: 1.5}, "models": {uid: {model: 8}}}
//
// Missing usage is zero, exactly as v1's dict.get(uid, 0). No network calls are
// made here.
func (p *Pool) ApplyLimits(values, usage map[string]any) {
	if p == nil {
		return
	}
	reserveCredits := limitGlobal(values, "reserve_credits")
	dailyCreditLimit := limitGlobal(values, "daily_credit_limit")
	dailyTokenLimit := limitGlobal(values, "daily_token_limit")
	modelDailyTokenLimit := limitGlobal(values, "model_daily_token_limit")

	tokenUsage := usageGroup(usage, "tokens")
	creditUsage := usageGroup(usage, "credits")
	modelUsage := usageGroup(usage, "models")

	for _, account := range p.Accounts() {
		account.mu.Lock()
		uid := strings.TrimSpace(account.Auth.UID)
		if uid == "" {
			uid = strings.TrimSpace(account.State.UID)
		}
		tokens := int64(0)
		if value, ok := lookupNumber(tokenUsage, uid); ok {
			tokens = int64(value)
		}
		credits := float64(0)
		if value, ok := lookupNumber(creditUsage, uid); ok {
			credits = value
		}
		modelCounts := lookupModelCounts(modelUsage, uid)

		account.ReserveCredits = reserveCredits
		account.DailyCreditLimit = dailyCreditLimit
		account.DailyTokenLimit = dailyTokenLimit
		account.ModelDailyTokenLimit = modelDailyTokenLimit
		account.DailyTokensToday = &tokens
		account.DailyCreditsToday = &credits
		account.ModelDailyTokens = modelCounts
		account.mu.Unlock()
	}
}

func limitGlobal(values map[string]any, key string) int64 {
	if values == nil {
		return 0
	}
	entry := values[key]
	if grouped, ok := entry.(map[string]any); ok {
		entry = grouped["global"]
	}
	value, ok := numericValue(entry)
	if !ok {
		return 0
	}
	return int64(value)
}

func usageGroup(usage map[string]any, key string) any {
	if usage == nil {
		return nil
	}
	return usage[key]
}

func lookupNumber(container any, uid string) (float64, bool) {
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

func lookupModelCounts(container any, uid string) map[string]int64 {
	switch values := container.(type) {
	case map[string]any:
		return modelCounts(values[uid])
	case map[string]map[string]int64:
		return copyModelCounts(values[uid])
	case map[string]map[string]any:
		return modelCounts(values[uid])
	default:
		return map[string]int64{}
	}
}

func modelCounts(value any) map[string]int64 {
	counts := make(map[string]int64)
	switch values := value.(type) {
	case map[string]any:
		for model, count := range values {
			if number, ok := numericValue(count); ok {
				counts[model] = int64(number)
			}
		}
	case map[string]int64:
		return copyModelCounts(values)
	case map[string]int:
		for model, count := range values {
			counts[model] = int64(count)
		}
	case map[string]float64:
		for model, count := range values {
			counts[model] = int64(count)
		}
	}
	return counts
}

func copyModelCounts(values map[string]int64) map[string]int64 {
	copied := make(map[string]int64, len(values))
	for model, count := range values {
		copied[model] = count
	}
	return copied
}

// NoteSuccess clears transient error/cooldown state after a successful request.
func (p *Pool) NoteSuccess(a *Account) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	changed := a.State.ErrCount != 0 || a.State.LastError != "" || !a.State.CoolUntil.IsZero()
	if changed {
		a.State.ErrCount = 0
		a.State.LastError = ""
		a.State.CoolUntil = time.Time{}
	}
	a.mu.Unlock()
	if !changed {
		return nil
	}
	return p.SaveState()
}

// NoteError records a request failure with v1 semantics: terminal disables the
// account unless it is manually disabled, soft sets a short cooldown, and
// ordinary errors cooldown after errThreshold consecutive failures.
func (p *Pool) NoteError(a *Account, message string, soft, terminal bool) error {
	if a == nil {
		return nil
	}

	a.mu.Lock()
	redacted := secrets.Redact(message, authSecretValues(a.Auth)...)
	redacted = truncateRunes(redacted, maxNoteRunes)
	a.State.LastError = redacted
	switch {
	case terminal:
		if !a.State.ManualDisabled {
			a.State.Disabled = true
			a.State.DisabledBy = "auth"
			a.State.Reason = redacted
		}
	case soft:
		a.State.CoolUntil = time.Now().Add(softCooldown)
	default:
		a.State.ErrCount++
		if a.State.ErrCount >= errThreshold {
			a.State.CoolUntil = time.Now().Add(errCooldown)
			a.State.ErrCount = 0
		}
	}
	a.mu.Unlock()
	return p.SaveState()
}

// Add imports or replaces one account. It uses the canonical AuthPath and
// refuses to overwrite a file whose embedded UID differs (sanitized filename
// collisions). Existing runtime/manual state is preserved on re-import.
func (p *Pool) Add(auth Auth) (*Account, error) {
	if p == nil {
		return nil, errors.New("raccoon: pool is nil")
	}
	cleanAuth(&auth)
	if auth.AccessToken == "" {
		return nil, errors.New("raccoon: auth requires access_token")
	}
	if auth.UID == "" {
		auth.UID = auth.uidFromCredentials()
	}
	if auth.UID == "" {
		return nil, errors.New("raccoon: auth requires uid")
	}

	path, err := AuthPath(p.dir, auth.UID)
	if err != nil {
		return nil, err
	}
	if err := p.verifyWritableAuth(path, auth.UID); err != nil {
		return nil, err
	}
	if err := SaveAuth(path, auth); err != nil {
		return nil, err
	}
	saved, err := LoadAuth(path)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	max := maxConcurrentFor(p.cfg)
	for _, account := range p.accounts {
		account.mu.Lock()
		match := strings.TrimSpace(account.Auth.UID) == strings.TrimSpace(saved.UID)
		account.mu.Unlock()
		if !match {
			continue
		}
		account.mu.Lock()
		account.Auth = saved
		account.Path = path
		account.proxySlot = strings.TrimSpace(saved.ProxySlot)
		account.proxy = p.slots.URL(account.proxySlot)
		account.Models = nil
		account.maxConcurrent = max
		account.State.MaxConcurrent = max
		if !account.State.ManualDisabled {
			account.State.Disabled = false
			account.State.DisabledBy = ""
			account.State.Reason = ""
			account.State.LastError = ""
			account.State.ErrCount = 0
			account.State.CoolUntil = time.Time{}
		}
		account.mu.Unlock()
		delete(p.preserved, saved.UID)
		p.mu.Unlock()
		if err := p.SaveState(); err != nil {
			return nil, err
		}
		return account, nil
	}

	state, ok := p.preserved[saved.UID]
	delete(p.preserved, saved.UID)
	if !ok {
		state = accountstate.State{UID: saved.UID, TasksEnabled: true}
	}
	state.UID = saved.UID
	state.ActiveConcurrent = 0
	state.MaxConcurrent = max

	account := &Account{
		Auth:          saved,
		State:         state,
		Path:          path,
		proxySlot:     strings.TrimSpace(saved.ProxySlot),
		proxy:         p.slots.URL(strings.TrimSpace(saved.ProxySlot)),
		pool:          p,
		maxConcurrent: max,
	}
	p.accounts = append(p.accounts, account)
	sort.Slice(p.accounts, func(i, j int) bool {
		return strings.TrimSpace(p.accounts[i].Auth.UID) < strings.TrimSpace(p.accounts[j].Auth.UID)
	})
	p.mu.Unlock()

	if err := p.SaveState(); err != nil {
		return nil, err
	}
	return account, nil
}

// Remove deletes the account's auth document(s), state entry, and affinity
// bindings. Matching v1, a UID may have a canonical file plus duplicate aliases.
func (p *Pool) Remove(uid string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	account.mu.Lock()
	accountUID := strings.TrimSpace(account.Auth.UID)
	if accountUID == "" {
		accountUID = strings.TrimSpace(account.State.UID)
	}
	account.mu.Unlock()

	paths, err := p.authPathsForUID(accountUID)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("raccoon: remove auth file %s: %w", path, err)
		}
	}

	p.mu.Lock()
	remaining := p.accounts[:0]
	for _, candidate := range p.accounts {
		if candidate == account {
			continue
		}
		remaining = append(remaining, candidate)
	}
	p.accounts = remaining
	delete(p.preserved, accountUID)
	p.dropAffinityLocked(accountUID)
	p.mu.Unlock()

	return p.SaveState()
}

// authPathsForUID returns the canonical file and any alias whose embedded UID
// matches.
func (p *Pool) authPathsForUID(uid string) ([]string, error) {
	paths := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	add := func(path string) {
		clean := filepath.Clean(path)
		if path == "" || seen[clean] {
			return
		}
		seen[clean] = true
		paths = append(paths, path)
	}

	if canonical, err := AuthPath(p.dir, uid); err == nil {
		// The canonical filename can collide with a different sanitized UID.
		// Delete it only when its embedded UID is verifiably this account.
		if auth, loadErr := LoadAuth(canonical); loadErr == nil && strings.TrimSpace(auth.UID) == uid {
			add(canonical)
		}
	}

	entries, err := os.ReadDir(p.folder)
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, fmt.Errorf("raccoon: read accounts directory %s: %w", p.folder, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "raccoon-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(p.folder, name)
		auth, err := LoadAuth(path)
		if err != nil {
			continue
		}
		if strings.TrimSpace(auth.UID) == uid {
			add(path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (p *Pool) dropAffinityLocked(uid string) {
	for key, bound := range p.affinity {
		if bound == uid {
			delete(p.affinity, key)
		}
	}
	kept := p.order[:0]
	for _, key := range p.order {
		if _, ok := p.affinity[key]; ok {
			kept = append(kept, key)
		}
	}
	p.order = kept
}

// verifyWritableAuth refuses to overwrite a file whose embedded UID differs or
// whose contents cannot be verified. A missing file is fine.
func (p *Pool) verifyWritableAuth(path, uid string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("raccoon: auth path is empty")
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("raccoon: auth uid is empty")
	}

	existing, err := LoadAuth(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("raccoon: refusing to overwrite unreadable auth file %s: %w", path, err)
	}
	if strings.TrimSpace(existing.UID) != uid {
		return fmt.Errorf("%w: %s", ErrAuthPathCollision, path)
	}
	return nil
}
