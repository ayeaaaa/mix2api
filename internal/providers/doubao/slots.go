package doubao

import "github.com/ayeaaaa/mix2api-v2/internal/core/transport"

// ApplyProxySlots clears account bindings whose slot disappeared or was
// disabled during a panel save. It returns the number of unbound accounts.
func (p *Provider) ApplyProxySlots(slots transport.Slots) int {
	if p == nil || p.pool == nil {
		return 0
	}
	unbound := 0
	for _, account := range p.pool.Accounts() {
		account.mu.Lock()
		slot := account.ProxySlot
		account.mu.Unlock()
		if slot == "" || slots.URL(slot) != "" {
			continue
		}
		if err := p.pool.SetProxySlot(account.UID, ""); err == nil {
			unbound++
		}
	}
	return unbound
}

// ProxySlotBindings reports enabled-account counts per slot id.
func (p *Provider) ProxySlotBindings() map[string]int {
	out := map[string]int{}
	if p == nil || p.pool == nil {
		return out
	}
	for _, account := range p.pool.Accounts() {
		account.mu.Lock()
		enabled, slot := account.Enabled, account.ProxySlot
		account.mu.Unlock()
		if enabled && slot != "" {
			out[slot]++
		}
	}
	return out
}
