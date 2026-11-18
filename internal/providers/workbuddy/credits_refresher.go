package workbuddy

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const (
	defaultCreditsRefreshHours = 12.0
	defaultCreditsRefreshEvery = 30 * time.Minute
	creditsRefreshFailedPark   = 6 * time.Hour
)

// RunBackground keeps credit balances fresh independently of the scheduler.
// It mirrors v1's CreditsRefresher: every tick refreshes only the single
// oldest stale account, so a large pool does not produce a burst of billing
// requests.
func (p *Provider) RunBackground(ctx context.Context) {
	if p == nil {
		return
	}
	interval := p.creditsRefreshInterval
	if interval <= 0 {
		interval = defaultCreditsRefreshEvery
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshStalestCredit(ctx)
		}
	}
}

// refreshStalestCredit refreshes one account and reports whether it attempted
// a request. TTL 0 disables the refresher; failed accounts are parked for six
// hours so a broken credential is not retried on every tick.
func (p *Provider) refreshStalestCredit(ctx context.Context) bool {
	if p == nil {
		return false
	}
	ttl := p.creditsRefreshTTL()
	if ttl <= 0 {
		return false
	}
	pool := p.Pool()
	if pool == nil {
		return false
	}
	now := p.creditsRefreshTime()
	parked := p.activeCreditsRefreshParked(now)
	account := p.stalestCreditAccount(pool.Accounts(), ttl, now, parked)
	if account == nil {
		return false
	}
	uid := accountUID(account)

	var row CreditRefreshRow
	if p.creditsRefreshHook != nil {
		row = p.creditsRefreshHook(ctx, account)
	} else {
		row = p.refreshCreditAccount(ctx, account)
	}

	p.creditsRefreshMu.Lock()
	if p.creditsRefreshParked == nil {
		p.creditsRefreshParked = make(map[string]time.Time)
	}
	if row.OK {
		delete(p.creditsRefreshParked, uid)
	} else {
		p.creditsRefreshParked[uid] = now.Add(creditsRefreshFailedPark)
	}
	p.creditsRefreshMu.Unlock()
	return true
}

// activeCreditsRefreshParked returns the accounts whose failed refresh is
// still parked, and drops expired entries. Parked accounts must be excluded
// before selecting the oldest account: otherwise one broken oldest account
// would block every other stale account until its six-hour park expires.
func (p *Provider) activeCreditsRefreshParked(now time.Time) map[string]bool {
	p.creditsRefreshMu.Lock()
	defer p.creditsRefreshMu.Unlock()
	if len(p.creditsRefreshParked) == 0 {
		return nil
	}
	parked := make(map[string]bool, len(p.creditsRefreshParked))
	for uid, until := range p.creditsRefreshParked {
		if now.Before(until) {
			parked[uid] = true
			continue
		}
		delete(p.creditsRefreshParked, uid)
	}
	return parked
}

func (p *Provider) stalestCreditAccount(accounts []*Account, ttl time.Duration, now time.Time, parked map[string]bool) *Account {
	var selected *Account
	var selectedAge time.Duration
	for _, account := range accounts {
		if account == nil {
			continue
		}
		account.mu.RLock()
		enabled := account.Auth.Enabled && !account.State.Disabled &&
			!account.tokenExpired && strings.TrimSpace(account.Auth.AccessToken) != ""
		credits := cloneCreditSnapshot(account.Credits)
		account.mu.RUnlock()
		if !enabled {
			continue
		}
		if parked != nil && parked[accountUID(account)] {
			continue
		}
		age := time.Duration(1 << 62)
		if credits != nil && credits.UpdatedAt > 0 {
			updated := time.Unix(0, int64(credits.UpdatedAt*float64(time.Second)))
			age = now.Sub(updated)
		}
		if age <= ttl {
			continue
		}
		if selected == nil || age > selectedAge {
			selected = account
			selectedAge = age
		}
	}
	return selected
}

func (p *Provider) creditsRefreshTTL() time.Duration {
	settings := p.sharedSettings()
	raw, present := settings["credits_refresh_hours"]
	if !present {
		return time.Duration(defaultCreditsRefreshHours * float64(time.Hour))
	}
	hours, ok := creditsRefreshNumber(raw)
	if !ok || hours < 0 {
		return time.Duration(defaultCreditsRefreshHours * float64(time.Hour))
	}
	if hours == 0 {
		return 0
	}
	return time.Duration(hours * float64(time.Hour))
}

func (p *Provider) creditsRefreshTime() time.Time {
	if p.creditsRefreshNow != nil {
		return p.creditsRefreshNow()
	}
	return p.creditsNowTime()
}

func creditsRefreshNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}
