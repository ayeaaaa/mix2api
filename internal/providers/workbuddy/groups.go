package workbuddy

import (
	"sort"
	"strings"
	"time"
)

// GroupInfo is one operator-defined account group summary. Group is display
// and selection metadata only; it never changes routing by itself.
type GroupInfo struct {
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Enabled int    `json:"enabled"`
}

// Groups lists non-empty account groups with their total and currently
// enabled account counts, sorted by group name.
func (p *Pool) Groups() []GroupInfo {
	if p == nil {
		return []GroupInfo{}
	}
	byName := make(map[string]*GroupInfo)
	for _, account := range p.Accounts() {
		account.mu.RLock()
		name := strings.TrimSpace(account.Group)
		enabled := account.Auth.Enabled && !account.State.Disabled
		account.mu.RUnlock()
		if name == "" {
			continue
		}
		info := byName[name]
		if info == nil {
			info = &GroupInfo{Name: name}
			byName[name] = info
		}
		info.Count++
		if enabled {
			info.Enabled++
		}
	}
	groups := make([]GroupInfo, 0, len(byName))
	for _, info := range byName {
		groups = append(groups, *info)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups
}

// GroupAccounts returns the accounts carrying exactly group, sorted by UID.
// An empty group matches nothing.
func (p *Pool) GroupAccounts(group string) []*Account {
	if p == nil {
		return []*Account{}
	}
	group = strings.TrimSpace(group)
	if group == "" {
		return []*Account{}
	}
	out := make([]*Account, 0)
	for _, account := range p.Accounts() {
		account.mu.RLock()
		match := strings.TrimSpace(account.Group) == group && !account.removed
		account.mu.RUnlock()
		if match {
			out = append(out, account)
		}
	}
	return out
}

// SetGroup sets the operator label used for filtering and batch actions. The
// value is trimmed and capped at 64 characters; an empty string clears it.
func (p *Pool) SetGroup(uid, group string) error {
	return p.mutateAccount(uid, func(account *Account) {
		group = truncateText(strings.TrimSpace(group), maxGroupRunes)
		account.Group = group
		account.Auth.Group = group
	})
}

// SetProduct switches one account's outbound identity. It reports whether the
// value actually changed, matching v1's set_product() contract.
func (p *Pool) SetProduct(uid, product string) (bool, error) {
	changed := false
	target := normalizeProduct(product)
	err := p.mutateAccount(uid, func(account *Account) {
		if account.Auth.Product == target {
			return
		}
		account.Auth.Product = target
		changed = true
	})
	return changed, err
}

// SetGroupEnabled enables or disables exactly the accounts carrying a
// non-empty group, including task participation. It returns the changed UIDs
// in account order. An empty group never matches every account.
func (p *Pool) SetGroupEnabled(group string, enabled bool) ([]string, error) {
	group = strings.TrimSpace(group)
	if e := []string{}; group == "" {
		return e, nil
	}
	accounts := p.GroupAccounts(group)
	changed := make([]string, 0, len(accounts))
	for _, account := range accounts {
		uid := accountUID(account)
		if uid == "" {
			continue
		}
		if err := p.mutateAccount(uid, func(account *Account) {
			account.Auth.SetEnabled(enabled)
			account.Auth.SetTasksEnabled(enabled)
			account.State.Disabled = !enabled
			account.State.ManualDisabled = !enabled
			account.State.TasksEnabled = enabled
			account.State.DisabledBy = ""
			if enabled {
				account.State.Reason = ""
				account.State.LastError = ""
				account.State.ErrCount = 0
				account.State.CoolUntil = time.Time{}
				// A persisted 403/11140 window survives a group enable; seed
				// the runtime cooldown so the operator cannot bypass it.
				seedRuntimeRestrictionLocked(account)
			} else {
				account.State.DisabledBy = "manual"
			}
		}); err != nil {
			return changed, err
		}
		changed = append(changed, uid)
	}
	sort.Strings(changed)
	return changed, nil
}
