package doubao

import (
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNoAccount means no enabled account can serve the request.
var ErrNoAccount = errors.New("doubao: no available account")

// PoolStats is the account-pool summary exposed by the panel.
type PoolStats struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Disabled int `json:"disabled"`
	Cooling  int `json:"cooling"`
}

// Pool owns the Doubao accounts. P1 keeps selection simple: session affinity,
// then least-active/last-used rotation. Concurrency is per account.
type Pool struct {
	dir string

	mu       sync.RWMutex
	cfg      Config
	accounts []*Account
	affinity map[string]string
	order    []string
	now      func() time.Time
}

func NewPool(dir string, cfg Config) (*Pool, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("doubao: accounts directory is empty")
	}
	p := &Pool{dir: dir, cfg: cfg, affinity: map[string]string{}, now: time.Now}
	if err := p.Reload(); err != nil {
		return nil, err
	}
	return p, nil
}

// Reload rereads account files without discarding the pool object.
func (p *Pool) Reload() error {
	accounts, err := LoadAccounts(p.dir)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.accounts = accounts
	p.applyConcurrencyLocked()
	if p.affinity == nil {
		p.affinity = map[string]string{}
	}
	p.mu.Unlock()
	return nil
}

func (p *Pool) SetConfig(cfg Config) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.cfg = cfg
	p.applyConcurrencyLocked()
	p.mu.Unlock()
}

func (p *Pool) applyConcurrencyLocked() {
	max := p.cfg.MaxConcurrent
	if max < 1 {
		max = 1
	}
	for _, account := range p.accounts {
		account.mu.Lock()
		account.maxConcurrent = max
		account.mu.Unlock()
	}
}

func (p *Pool) Accounts() []*Account {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

func (p *Pool) account(uid string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, account := range p.accounts {
		if account.UID == uid {
			return account
		}
	}
	return nil
}

// Pick selects an account. The model argument is accepted for future model
// capability filtering; all Doubao accounts currently expose the same catalog.
func (p *Pool) Pick(model, sessionKey string, exclude map[string]bool) (*Account, error) {
	if p == nil {
		return nil, ErrNoAccount
	}
	now := p.now()
	sessionKey = strings.TrimSpace(sessionKey)
	p.mu.RLock()
	affinity := p.affinity[sessionKey]
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.RUnlock()

	if affinity != "" && !exclude[affinity] {
		if account := p.account(affinity); account != nil && account.ready(now) {
			return account, nil
		}
	}

	candidates := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || exclude[account.UID] || !account.ready(now) {
			continue
		}
		candidates = append(candidates, account)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		left.mu.Lock()
		la, ll := left.active, left.lastUsed
		left.mu.Unlock()
		right.mu.Lock()
		ra, rl := right.active, right.lastUsed
		right.mu.Unlock()
		if la != ra {
			return la < ra
		}
		if !ll.Equal(rl) {
			return ll.Before(rl)
		}
		return left.UID < right.UID
	})
	return candidates[0], nil
}

func (p *Pool) Acquire(account *Account) bool {
	if account == nil {
		return false
	}
	return account.acquire()
}

func (p *Pool) Release(account *Account) {
	if account != nil {
		account.release()
	}
}

func (p *Pool) BindSession(sessionKey, uid string) {
	sessionKey = strings.TrimSpace(sessionKey)
	uid = strings.TrimSpace(uid)
	if p == nil || sessionKey == "" || uid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.affinity == nil {
		p.affinity = map[string]string{}
	}
	if _, exists := p.affinity[sessionKey]; !exists {
		p.order = append(p.order, sessionKey)
	}
	p.affinity[sessionKey] = uid
	const maxAffinity = 2048
	if len(p.affinity) > maxAffinity {
		evict := len(p.affinity) - maxAffinity
		for _, key := range p.order[:evict] {
			delete(p.affinity, key)
		}
		p.order = append([]string(nil), p.order[evict:]...)
	}
}

func (p *Pool) Stats() PoolStats {
	if p == nil {
		return PoolStats{}
	}
	now := p.now()
	stats := PoolStats{}
	for _, account := range p.Accounts() {
		stats.Total++
		account.mu.Lock()
		enabled := account.Enabled
		cooling := !account.coolingUntil.IsZero() && now.Before(account.coolingUntil)
		account.mu.Unlock()
		if !enabled {
			stats.Disabled++
		} else if cooling {
			stats.Cooling++
		} else {
			stats.Healthy++
		}
	}
	return stats
}

func (p *Pool) Add(cookies, device map[string]string, uid, nickname string, enabled bool) (*Account, error) {
	if len(cookies) == 0 {
		return nil, errors.New("doubao: cookies are required")
	}
	if uid == "" {
		uid = deriveUID(cookies)
	}
	// Re-importing the same browser session refreshes credentials in place.
	// Preserve the operator's note/proxy/credits instead of creating a second
	// row that points at the same file.
	if existing := p.account(uid); existing != nil {
		existing.mu.Lock()
		existing.Cookies = cloneStringMap(cookies)
		existing.Device = cloneStringMap(device)
		if existing.Device["web_tab_id"] == "" {
			existing.Device["web_tab_id"] = randomID()
		}
		if nickname != "" {
			existing.Nickname = nickname
		}
		existing.Enabled = enabled
		existing.mu.Unlock()
		if err := existing.save(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	account, err := newImportedAccount(p.dir, cookies, device, uid, nickname, enabled)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	account.maxConcurrent = p.cfg.MaxConcurrent
	if account.maxConcurrent < 1 {
		account.maxConcurrent = 1
	}
	p.accounts = append(p.accounts, account)
	p.mu.Unlock()
	return account, nil
}

func (p *Pool) Remove(uid string) error {
	account := p.account(uid)
	if account == nil {
		return errors.New("doubao: account not found")
	}
	p.mu.Lock()
	for i, item := range p.accounts {
		if item == account {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	if account.Path != "" {
		if err := os.Remove(account.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (p *Pool) Enable(uid string) error {
	account := p.account(uid)
	if account == nil {
		return errors.New("doubao: account not found")
	}
	account.mu.Lock()
	account.Enabled = true
	account.coolingUntil = time.Time{}
	account.lastError = ""
	account.mu.Unlock()
	return account.save()
}

func (p *Pool) Disable(uid string) error {
	account := p.account(uid)
	if account == nil {
		return errors.New("doubao: account not found")
	}
	account.mu.Lock()
	account.Enabled = false
	account.mu.Unlock()
	return account.save()
}

func (p *Pool) SetNote(uid, note string) error {
	account := p.account(uid)
	if account == nil {
		return errors.New("doubao: account not found")
	}
	account.mu.Lock()
	account.Note = strings.TrimSpace(note)
	account.mu.Unlock()
	return account.save()
}

func (p *Pool) SetProxySlot(uid, slot string) error {
	account := p.account(uid)
	if account == nil {
		return errors.New("doubao: account not found")
	}
	account.mu.Lock()
	account.ProxySlot = strings.TrimSpace(slot)
	account.mu.Unlock()
	return account.save()
}

func (p *Pool) AccountSummaries() []AccountSummary {
	accounts := p.Accounts()
	out := make([]AccountSummary, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, account.summary())
	}
	return out
}
