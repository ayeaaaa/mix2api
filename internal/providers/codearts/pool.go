package codearts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	// maxAffinityEntries bounds session affinity so a long-running process with
	// many conversations cannot grow the map without limit. The oldest batch is
	// evicted, matching v1's simple cap.
	maxAffinityEntries = 4096
	affinityEvictCount = 1024

	// noteSoftCooldown, noteErrThreshold, and noteErrCooldown mirror v1
	// note_error: soft failures cool for 60s, while three ordinary failures in
	// a row trigger a 600s cooldown and reset the counter.
	noteSoftCooldown = 60 * time.Second
	noteErrThreshold = 3
	noteErrCooldown  = 600 * time.Second
)

var (
	// ErrNoAccount means every account was excluded, unhealthy, cooling,
	// credit-exhausted, or at its concurrency limit.
	ErrNoAccount = errors.New("codearts: no account available")

	// ErrAccountNotFound means the requested UID is not in the pool.
	ErrAccountNotFound = errors.New("codearts: account not found")

	// ErrAuthPathCollision means a write would replace a token document whose
	// embedded user_id belongs to a different account (for example "a/b" and
	// "a_b" both sanitize to codearts-a_b.json).
	ErrAuthPathCollision = errors.New("codearts: auth path belongs to a different uid")
)

// PoolStats is the v1 CodeArts overview counter shape.
type PoolStats struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Disabled int `json:"disabled"`
	Cooling  int `json:"cooling"`
}

// Pool is the provider-owned CodeArts account pool. It owns auth loading,
// v1 list-state persistence, scheduler guards, session affinity, and proxy slot
// resolution.
//
// Pool locks are instance-local. Task 9 must ensure exactly one Pool exists per
// accounts directory (per process); two Provider instances pointed at the same
// directory would still race on state.json even though each Pool is internally
// consistent.
//
// Lock order:
//   - authOpsMu may be held across runtimeMu, pool.mu, account.mu, stateMu, and
//     auth-file I/O.
//   - runtimeMu may be held across a brief pool.mu access (released before
//     account locks) and account.mu.
//   - stateMu may be held across a brief pool.mu access and account.mu.
//   - pool.mu is never held while account.mu is taken; account.mu is never held
//     while pool.mu or stateMu is taken.
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

	// authOpsMu serializes Add/Remove/SetProxySlot auth-file operations.
	authOpsMu sync.Mutex

	// runtimeMu serializes runtime projections of config/proxy settings onto
	// accounts, so an Add/re-import final reapply cannot lose a concurrent
	// SetConfig/ApplyProxySlots update (or vice versa).
	runtimeMu sync.Mutex

	// stateMu serializes every full-file state snapshot and write so an older
	// snapshot cannot overwrite a later manual disable, note, or check-in.
	stateMu sync.Mutex

	// stateWriteHook is a test seam invoked after the snapshot is built and
	// before the state file write, while stateMu is held.
	stateWriteHook func()

	// addAccountHook is a test seam invoked after a new account is constructed
	// (or an existing account re-imported) but before the final runtime
	// projection. Tests use it to simulate a settings change in that window.
	addAccountHook func(*Account)

	// preserved keeps state entries whose auth file is absent or malformed. A
	// later save must not silently wipe their notes, manual disables, check-in
	// results, or unknown fields.
	preserved map[string]AccountState

	// now is the scheduler clock. Production keeps time.Now; tests may set it.
	now func() time.Time
}

// NewPool loads every valid auth document under dir/codearts, merges
// dir/codearts/state.json, and applies cfg.MaxConcurrent. Auth files are
// independent: a malformed document is skipped. An unreadable, malformed, or
// unsupported state document is an error, because overwriting it would destroy
// manual disables and notes.
func NewPool(dir string, cfg Config) (*Pool, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("codearts: accounts directory is empty")
	}
	dir = filepath.Clean(dir)

	p := &Pool{
		dir:       dir,
		folder:    filepath.Join(dir, "codearts"),
		statePath: filepath.Join(dir, "codearts", "state.json"),
		cfg:       cfg,
		affinity:  make(map[string]string),
		preserved: make(map[string]AccountState),
		now:       time.Now,
	}

	states, err := LoadState(p.statePath)
	if err != nil {
		return nil, fmt.Errorf("codearts: load state: %w", err)
	}

	auths, err := p.loadAuthFiles()
	if err != nil {
		return nil, err
	}

	maxConcurrent := maxConcurrentFor(cfg)
	accounts := make([]*Account, 0, len(auths))
	loaded := make(map[string]bool, len(auths))
	for _, loadedAuth := range auths {
		uid := strings.TrimSpace(loadedAuth.auth.UserID)
		state, ok := states[uid]
		if !ok {
			state = AccountState{Name: uid, TasksEnabled: true}
		}
		state.Name = uid
		account := p.newAccount(loadedAuth.auth, loadedAuth.path, state, maxConcurrent)
		accounts = append(accounts, account)
		loaded[uid] = true
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		return strings.TrimSpace(accounts[i].Auth.UserID) < strings.TrimSpace(accounts[j].Auth.UserID)
	})

	// Keep state for accounts whose auth file is currently absent or malformed.
	for uid, state := range states {
		uid = strings.TrimSpace(uid)
		if uid == "" || loaded[uid] {
			continue
		}
		state.Name = uid
		p.preserved[uid] = state
	}

	p.accounts = accounts
	return p, nil
}

func maxConcurrentFor(cfg Config) int {
	return boundedInt(cfg.MaxConcurrent, defaultMaxConcurrent, minMaxConcurrent, maxMaxConcurrent)
}

func (p *Pool) newAccount(auth Auth, path string, state AccountState, maxConcurrent int) *Account {
	uid := strings.TrimSpace(auth.UserID)
	state.Name = uid
	account := &Account{
		Auth:           auth,
		State:          state,
		Path:           path,
		proxySlot:      strings.TrimSpace(auth.ProxySlot),
		pool:           p,
		maxConcurrent:  maxConcurrent,
		authGeneration: 1,
	}
	if state.Checkin != nil {
		account.Checkin = cloneCheckin(CheckinState(state.Checkin))
	}
	account.proxy = p.proxyURLForSlot(account.proxySlot)
	return account
}

type loadedAuth struct {
	auth      Auth
	path      string
	canonical bool
}

// loadAuthFiles reads codearts-*.json documents deterministically. The embedded
// user_id is authoritative; a canonical AuthPath file wins over a duplicate
// alias for the same UID.
func (p *Pool) loadAuthFiles() ([]loadedAuth, error) {
	entries, err := os.ReadDir(p.folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codearts: read accounts directory %s: %w", p.folder, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "codearts-") || !strings.HasSuffix(name, ".json") {
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
		uid := strings.TrimSpace(auth.UserID)
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
		leftUID := strings.TrimSpace(loaded[i].auth.UserID)
		rightUID := strings.TrimSpace(loaded[j].auth.UserID)
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
		uid := strings.TrimSpace(item.auth.UserID)
		if seen[uid] {
			continue
		}
		seen[uid] = true
		unique = append(unique, item)
	}
	return unique, nil
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

// SaveState writes accounts/codearts/state.json as the v1 bare list, preserving
// state for accounts whose auth files are not currently loaded and every
// unknown field carried by AccountState. Snapshot and write are serialized so
// an older full-file snapshot cannot overwrite a later mutation.
func (p *Pool) SaveState() error {
	if p == nil {
		return errors.New("codearts: pool is nil")
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.saveStateLocked()
}

// mutateState serializes a state mutation with its snapshot and write under
// stateMu. Callers must not call SaveState from fn.
func (p *Pool) mutateState(fn func()) error {
	if p == nil {
		return errors.New("codearts: pool is nil")
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	fn()
	return p.saveStateLocked()
}

// saveStateLocked builds and writes one full v1 state snapshot. The caller holds
// stateMu.
func (p *Pool) saveStateLocked() error {
	p.mu.RLock()
	accounts := append([]*Account(nil), p.accounts...)
	preserved := make(map[string]AccountState, len(p.preserved))
	for uid, state := range p.preserved {
		preserved[uid] = state
	}
	statePath := p.statePath
	p.mu.RUnlock()

	states := make(map[string]AccountState, len(accounts)+len(preserved))
	for uid, state := range preserved {
		states[uid] = state
	}
	for _, account := range accounts {
		account.mu.Lock()
		uid := account.uidLocked()
		state := account.State
		state.Name = uid
		if account.Checkin != nil {
			state.Checkin = map[string]any(cloneCheckin(account.Checkin))
		} else if account.State.Checkin != nil {
			state.Checkin = map[string]any(cloneCheckin(CheckinState(account.State.Checkin)))
		} else {
			state.Checkin = nil
		}
		account.mu.Unlock()
		if uid == "" {
			continue
		}
		states[uid] = state
	}

	if p.stateWriteHook != nil {
		p.stateWriteHook()
	}
	if err := SaveState(statePath, states); err != nil {
		return fmt.Errorf("codearts: save state: %w", err)
	}
	return nil
}

// Accounts returns a snapshot of account pointers sorted by their current UID.
func (p *Pool) Accounts() []*Account {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.RUnlock()
	sort.SliceStable(accounts, func(i, j int) bool {
		left, _ := accounts[i].currentUID()
		right, _ := accounts[j].currentUID()
		return left < right
	})
	return accounts
}

// Get returns the live account whose embedded auth UID or state UID matches
// uid.
func (p *Pool) Get(uid string) *Account {
	if p == nil {
		return nil
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil
	}
	for _, account := range p.Accounts() {
		accountUID, live := account.currentUID()
		if live && accountUID == uid {
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

// Stats returns the v1 overview counters. A known credit-exhausted account is
// still counted healthy, matching v1's stats (the scheduler separately parks
// it).
func (p *Pool) Stats() PoolStats {
	stats := PoolStats{}
	if p == nil {
		return stats
	}
	now := p.nowTime()
	for _, account := range p.Accounts() {
		account.mu.Lock()
		disabled := account.State.Disabled || account.State.ManualDisabled
		cooling := !disabled && account.State.CoolUntil > unixSeconds(now)
		account.mu.Unlock()

		stats.Total++
		switch {
		case disabled:
			stats.Disabled++
		case cooling:
			stats.Cooling++
		default:
			stats.Healthy++
		}
	}
	return stats
}

// Pick chooses a healthy account under its concurrency limit: session affinity
// first, then model capability, then v1's rank
// (active_concurrent, urgent, last_used, uid).
//
// Model capability follows v1's `capable or candidates` fallback: if at least
// one eligible account claims the requested model, only those capable accounts
// are considered; if none claim it, every candidate (including accounts with an
// unknown/stale catalog) remains eligible.
//
// Expiring-credit priority is the "urgent" rank field only. It breaks ties
// within the same active-concurrency tier; a busy expiring account never
// outranks a less busy non-expiring account, matching v1 exactly.
func (p *Pool) Pick(model, sessionKey string, exclude map[string]bool) (*Account, error) {
	if p == nil {
		return nil, ErrNoAccount
	}
	now := p.nowTime()

	expiringFirst := p.expiringPriorityEnabled()
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
			selection := account.selectionSnapshot(now)
			if selection.removed || selection.uid != affinityUID ||
				!selection.healthy || selection.active >= selection.max {
				continue
			}
			if account.HasModel(model) {
				return account, nil
			}
		}
	}

	type candidate struct {
		account  *Account
		uid      string
		active   int
		lastUsed time.Time
		urgent   int
	}
	candidates := make([]candidate, 0, len(accounts))
	for _, account := range accounts {
		selection := account.selectionSnapshot(now)
		if selection.removed || exclude[selection.uid] ||
			!selection.healthy || selection.active >= selection.max {
			continue
		}
		urgent := 1
		if expiringFirst && selection.expiring > 0 {
			urgent = 0
		}
		candidates = append(candidates, candidate{
			account:  account,
			uid:      selection.uid,
			active:   selection.active,
			lastUsed: selection.lastUsed,
			urgent:   urgent,
		})
	}
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}

	// v1 uses `capable or candidates`: capability narrows the set only when at
	// least one eligible candidate claims the model. Otherwise every candidate is
	// eligible, keeping an unknown or stale catalog from taking the pool offline.
	selected := candidates
	capable := make([]candidate, 0, len(candidates))
	for _, item := range candidates {
		if item.account.HasModel(model) {
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
		if selected[i].urgent != selected[j].urgent {
			return selected[i].urgent < selected[j].urgent
		}
		if !selected[i].lastUsed.Equal(selected[j].lastUsed) {
			return selected[i].lastUsed.Before(selected[j].lastUsed)
		}
		return selected[i].uid < selected[j].uid
	})
	return selected[0].account, nil
}

func (p *Pool) expiringPriorityEnabled() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	enabled := p.cfg.CreditsExpiringPriority
	p.mu.RUnlock()
	return enabled
}

// BindSession records session affinity for the next Pick. The map is bounded;
// the oldest entries are evicted when it grows.
func (p *Pool) BindSession(sessionKey, uid string) {
	if p == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	uid = strings.TrimSpace(uid)
	if sessionKey == "" || uid == "" {
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

// Acquire reserves one concurrency slot. It does not re-check health; Pick
// already did. The tombstone check still applies so Remove wins a race.
func (p *Pool) Acquire(a *Account) bool {
	if p == nil || a == nil {
		return false
	}
	now := p.nowTime()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed {
		return false
	}
	max := normalizedMaxConcurrent(a.maxConcurrent)
	if a.activeConcurrent >= max {
		return false
	}
	a.activeConcurrent++
	a.lastUsed = now
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

// NoteSuccess clears transient failure state after a successful request:
// err_count, cool_until, and last_error. Matching v1, a manual disable and its
// reason are untouched. State is written only when something changed.
func (p *Pool) NoteSuccess(a *Account) error {
	if a == nil {
		return nil
	}
	if p == nil {
		return errors.New("codearts: pool is nil")
	}

	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	a.mu.Lock()
	changed := a.State.ErrCount != 0 || a.State.CoolUntil != 0 || a.State.LastError != ""
	if changed {
		a.State.ErrCount = 0
		a.State.CoolUntil = 0
		a.State.LastError = ""
	}
	a.mu.Unlock()
	if !changed {
		return nil
	}
	return p.saveStateLocked()
}

// NoteError records a request failure with v1 semantics. A terminal failure
// disables the account unless it is manually disabled, in which case manual
// state and reason are never overwritten. A soft failure sets a 60s cooldown.
// Ordinary failures increment err_count and, on the third consecutive failure,
// set a 600s cooldown and reset the counter. The message is redacted and capped
// at 200 runes, and state is persisted on every call.
func (p *Pool) NoteError(a *Account, message string, soft, terminal bool) error {
	if a == nil {
		return nil
	}
	if p == nil {
		return errors.New("codearts: pool is nil")
	}
	now := p.nowTime()

	return p.mutateState(func() {
		a.mu.Lock()
		redacted := truncateRunes(redactText(message, codeartsSecretValues(a.Auth)), maxStateNoteRunes)
		a.State.LastError = redacted
		switch {
		case terminal:
			if !a.State.ManualDisabled {
				a.State.Disabled = true
				a.State.ManualDisabled = false
				a.State.Reason = redacted
			}
		case soft:
			a.State.CoolUntil = unixSeconds(now.Add(noteSoftCooldown))
		default:
			a.State.ErrCount++
			if a.State.ErrCount >= noteErrThreshold {
				a.State.CoolUntil = unixSeconds(now.Add(noteErrCooldown))
				a.State.ErrCount = 0
			}
		}
		a.mu.Unlock()
	})
}

// Disable applies a manual disable. The manual marker keeps a later credits or
// token refresh from clearing it. Mutation and state write share stateMu.
func (p *Pool) Disable(uid, reason string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "manual disable"
	}
	return p.mutateState(func() {
		account.mu.Lock()
		account.State.Disabled = true
		account.State.ManualDisabled = true
		account.State.Reason = reason
		account.State.LastError = reason
		account.mu.Unlock()
	})
}

// Enable clears a manual disable, cooldown, and error counters.
func (p *Pool) Enable(uid string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	return p.mutateState(func() {
		account.mu.Lock()
		account.State.Disabled = false
		account.State.ManualDisabled = false
		account.State.Reason = ""
		account.State.LastError = ""
		account.State.ErrCount = 0
		account.State.CoolUntil = 0
		account.mu.Unlock()
	})
}

// ClearCooldown clears the cooldown and consecutive-error count.
func (p *Pool) ClearCooldown(uid string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	return p.mutateState(func() {
		account.mu.Lock()
		account.State.CoolUntil = 0
		account.State.ErrCount = 0
		account.mu.Unlock()
	})
}

// SetNote stores a trimmed, 200-character note.
func (p *Pool) SetNote(uid, note string) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	return p.mutateState(func() {
		account.mu.Lock()
		account.State.Note = truncateRunes(strings.TrimSpace(note), maxStateNoteRunes)
		account.mu.Unlock()
	})
}

// SetTasksEnabled toggles automatic task/reward behavior for one account.
func (p *Pool) SetTasksEnabled(uid string, enabled bool) error {
	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	return p.mutateState(func() {
		account.mu.Lock()
		account.State.TasksEnabled = enabled
		account.mu.Unlock()
	})
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
// it against the current slot list. An empty slot id clears the binding. The
// target file is verified to belong to this account before any write.
func (p *Pool) SetProxySlot(uid, slotID string) error {
	if p == nil {
		return ErrAccountNotFound
	}
	p.authOpsMu.Lock()
	defer p.authOpsMu.Unlock()

	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}
	slotID = strings.TrimSpace(slotID)

	account.mu.Lock()
	if account.removed {
		account.mu.Unlock()
		return ErrAccountNotFound
	}
	accountUID := account.uidLocked()
	path := strings.TrimSpace(account.Path)
	auth := account.Auth
	account.mu.Unlock()

	if path == "" {
		path, err = AuthPath(p.dir, accountUID)
		if err != nil {
			return err
		}
	}
	if err := p.verifyWritableAuth(path, accountUID); err != nil {
		return err
	}

	auth.ProxySlot = slotID
	if err := SaveAuth(path, auth); err != nil {
		return fmt.Errorf("codearts: save proxy slot: %w", err)
	}
	saved, err := LoadAuth(path)
	if err != nil {
		return fmt.Errorf("codearts: reload proxy slot: %w", err)
	}
	proxy := p.proxyURLForSlot(slotID)

	account.mu.Lock()
	account.Auth = saved
	account.Path = path
	account.proxySlot = slotID
	account.proxy = proxy
	account.mu.Unlock()
	return nil
}

// ApplyProxySlots replaces the slot list and resolves every account binding.
// Missing or disabled slots resolve to direct while the persisted binding
// stays on disk for re-enabling. runtimeMu serializes the projection with
// concurrent Add/re-import so a new account cannot keep a stale URL.
func (p *Pool) ApplyProxySlots(slots transport.Slots) {
	if p == nil {
		return
	}
	copied := append(transport.Slots(nil), slots...)

	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()

	p.mu.Lock()
	p.slots = copied
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()

	for _, account := range accounts {
		account.mu.Lock()
		slot := account.proxySlotLocked()
		account.proxy = copied.URL(slot)
		account.mu.Unlock()
	}
}

// SetConfig replaces the pool configuration and applies max_concurrent to
// every account. runtimeMu serializes the projection with Add/re-import.
func (p *Pool) SetConfig(cfg Config) {
	if p == nil {
		return
	}
	max := maxConcurrentFor(cfg)

	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()

	p.mu.Lock()
	p.cfg = cfg
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, account := range accounts {
		account.setMaxConcurrent(max)
	}
}

// reapplyRuntimeSettings projects the latest cfg and proxy slots onto one
// account. Add and re-import call it after membership/auth changes so a
// settings update racing the import cannot leave the account with stale
// max_concurrent or proxy values.
func (p *Pool) reapplyRuntimeSettings(account *Account) {
	if p == nil || account == nil {
		return
	}

	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()

	p.mu.RLock()
	cfg := p.cfg
	slots := append(transport.Slots(nil), p.slots...)
	p.mu.RUnlock()

	account.mu.Lock()
	account.maxConcurrent = normalizedMaxConcurrent(maxConcurrentFor(cfg))
	account.proxy = slots.URL(account.proxySlotLocked())
	account.mu.Unlock()
}

// ApplyLimits is reserved for provider-wide scheduling limits. CodeArts v1 has
// no shared reserve/daily guards, so this intentionally does nothing yet.
func (p *Pool) ApplyLimits(values, usage map[string]any) {}

// Add imports or replaces one account under the canonical AuthPath. It refuses
// to overwrite a file whose embedded user_id differs (sanitized filename
// collisions). Re-import preserves manual state, notes, check-in results, tasks,
// and the previously selected proxy slot.
func (p *Pool) Add(auth Auth) (*Account, error) {
	if p == nil {
		return nil, errors.New("codearts: pool is nil")
	}
	auth.UserID = strings.TrimSpace(auth.UserID)
	if auth.UserID == "" {
		return nil, errors.New("codearts: auth requires user_id")
	}
	if strings.TrimSpace(auth.CloudDragonToken) == "" {
		return nil, errors.New("codearts: auth requires cloud_dragon_token")
	}

	path, err := AuthPath(p.dir, auth.UserID)
	if err != nil {
		return nil, err
	}

	p.authOpsMu.Lock()
	defer p.authOpsMu.Unlock()

	if account := p.Get(auth.UserID); account != nil {
		return p.reimportAccount(account, path, auth)
	}
	return p.addNewAccount(path, auth)
}

func (p *Pool) addNewAccount(path string, auth Auth) (*Account, error) {
	uid := strings.TrimSpace(auth.UserID)
	if err := p.verifyWritableAuth(path, uid); err != nil {
		return nil, err
	}
	if err := SaveAuth(path, auth); err != nil {
		return nil, err
	}
	saved, err := LoadAuth(path)
	if err != nil {
		return nil, err
	}

	p.mu.RLock()
	maxConcurrent := maxConcurrentFor(p.cfg)
	state, hadPreserved := p.preserved[uid]
	p.mu.RUnlock()
	if !hadPreserved {
		state = AccountState{Name: uid, TasksEnabled: true}
	}
	account := p.newAccount(saved, path, state, maxConcurrent)
	if p.addAccountHook != nil {
		p.addAccountHook(account)
	}

	p.mu.Lock()
	delete(p.preserved, uid)
	p.accounts = append(p.accounts, account)
	p.mu.Unlock()
	p.reapplyRuntimeSettings(account)

	if err := p.SaveState(); err != nil {
		account.mu.Lock()
		account.removed = true
		account.mu.Unlock()
		p.mu.Lock()
		kept := p.accounts[:0]
		for _, candidate := range p.accounts {
			if candidate != account {
				kept = append(kept, candidate)
			}
		}
		p.accounts = kept
		if hadPreserved {
			p.preserved[uid] = state
		}
		p.mu.Unlock()
		return nil, err
	}
	return account, nil
}

// reimportAccount replaces credentials on the canonical *Account while
// preserving manual state, notes, check-in results, tasks, and an existing
// proxy slot. Matching v1, a fresh login document does not erase the operator's
// panel-side choices.
func (p *Pool) reimportAccount(account *Account, path string, auth Auth) (*Account, error) {
	account.mu.Lock()
	if account.removed {
		account.mu.Unlock()
		return nil, ErrAccountNotFound
	}
	existingSlot := strings.TrimSpace(account.proxySlot)
	if existingSlot == "" {
		existingSlot = strings.TrimSpace(account.Auth.ProxySlot)
	}
	account.mu.Unlock()
	if strings.TrimSpace(auth.ProxySlot) == "" {
		auth.ProxySlot = existingSlot
	}
	uid := strings.TrimSpace(auth.UserID)

	if err := p.verifyWritableAuth(path, uid); err != nil {
		return nil, err
	}
	if err := SaveAuth(path, auth); err != nil {
		return nil, err
	}
	saved, err := LoadAuth(path)
	if err != nil {
		return nil, err
	}
	proxy := p.proxyURLForSlot(strings.TrimSpace(saved.ProxySlot))

	account.mu.Lock()
	account.Auth = saved
	account.Path = path
	account.proxySlot = strings.TrimSpace(saved.ProxySlot)
	account.proxy = proxy
	account.authGeneration++
	account.Models = nil
	account.modelBenefit = nil
	account.State.Name = uid
	if !account.State.ManualDisabled {
		account.State.Disabled = false
		account.State.Reason = ""
		account.State.LastError = ""
		account.State.ErrCount = 0
		account.State.CoolUntil = 0
	}
	account.mu.Unlock()

	if p.addAccountHook != nil {
		p.addAccountHook(account)
	}
	p.reapplyRuntimeSettings(account)

	p.mu.Lock()
	delete(p.preserved, uid)
	p.mu.Unlock()
	if err := p.SaveState(); err != nil {
		return nil, err
	}
	return account, nil
}

// Remove deletes the account's auth document(s), state entry, and affinity
// bindings. A sanitized canonical filename is only deleted when its embedded
// UID verifiably belongs to this account.
func (p *Pool) Remove(uid string) error {
	if p == nil {
		return ErrAccountNotFound
	}
	p.authOpsMu.Lock()
	defer p.authOpsMu.Unlock()

	account, err := p.accountForMutation(uid)
	if err != nil {
		return err
	}

	account.mu.Lock()
	if account.removed {
		account.mu.Unlock()
		return ErrAccountNotFound
	}
	account.removed = true
	accountUID := account.uidLocked()
	account.mu.Unlock()

	paths, err := p.authPathsForUID(accountUID)
	if err != nil {
		account.mu.Lock()
		account.removed = false
		account.mu.Unlock()
		return err
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			account.mu.Lock()
			account.removed = false
			account.mu.Unlock()
			return fmt.Errorf("codearts: remove auth file %s: %w", path, err)
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

	uid = strings.TrimSpace(uid)
	if uid == "" {
		return paths, nil
	}
	if canonical, err := AuthPath(p.dir, uid); err == nil {
		// The canonical filename can collide with a different sanitized UID.
		// Delete it only when its embedded UID is verifiably this account.
		if auth, loadErr := LoadAuth(canonical); loadErr == nil && strings.TrimSpace(auth.UserID) == uid {
			add(canonical)
		}
	}

	entries, err := os.ReadDir(p.folder)
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codearts: read accounts directory %s: %w", p.folder, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "codearts-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(p.folder, name)
		auth, err := LoadAuth(path)
		if err != nil {
			continue
		}
		if strings.TrimSpace(auth.UserID) == uid {
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

func (p *Pool) proxyURLForSlot(slotID string) string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.slots.URL(slotID)
}

// verifyWritableAuth refuses to overwrite a file whose embedded UID differs or
// whose contents cannot be verified. A missing file is fine.
func (p *Pool) verifyWritableAuth(path, uid string) error {
	return verifyAuthOwnership(path, uid)
}
