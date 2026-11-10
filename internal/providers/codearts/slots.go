package codearts

import (
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// ApplyProxySlots clears account bindings that disappeared during a panel save
// and re-resolves the rest. It returns the number of unbound accounts.
func (p *Provider) ApplyProxySlots(slots transport.Slots) int {
	if p == nil {
		return 0
	}
	pool := p.Pool()
	if pool == nil {
		return 0
	}
	valid := make(map[string]bool, len(slots))
	for _, slot := range slots {
		valid[slot.ID] = true
	}
	unbound := 0
	for _, account := range pool.List() {
		slotID := account.ProxySlot
		if slotID == "" || valid[slotID] {
			continue
		}
		if err := pool.SetProxySlot(account.UID, ""); err == nil {
			unbound++
		}
	}
	pool.ApplyProxySlots(slots)
	return unbound
}

// ProxySlotBindings reports enabled-account counts per slot id.
func (p *Provider) ProxySlotBindings() map[string]int {
	out := map[string]int{}
	if p == nil {
		return out
	}
	pool := p.Pool()
	if pool == nil {
		return out
	}
	for _, account := range pool.List() {
		if account.Disabled || account.ProxySlot == "" {
			continue
		}
		out[account.ProxySlot]++
	}
	return out
}
