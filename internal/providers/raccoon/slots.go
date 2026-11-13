package raccoon

import (
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// ApplyProxySlots clears disappeared account bindings and re-resolves the rest.
// It returns the number of unbound accounts.
func (p *Provider) ApplyProxySlots(slots transport.Slots) int {
	if p == nil || p.pool == nil {
		return 0
	}
	valid := make(map[string]bool, len(slots))
	for _, slot := range slots {
		valid[slot.ID] = true
	}
	unbound := 0
	for _, account := range p.pool.List() {
		slotID := account.ProxySlot
		if slotID == "" || valid[slotID] {
			continue
		}
		if err := p.pool.SetProxySlot(account.UID, ""); err == nil {
			unbound++
		}
	}
	p.pool.SetProxySlots(slots)
	return unbound
}

// ProxySlotBindings reports enabled-account counts per slot id.
func (p *Provider) ProxySlotBindings() map[string]int {
	out := map[string]int{}
	if p == nil || p.pool == nil {
		return out
	}
	for _, account := range p.pool.List() {
		if account.Disabled || account.ProxySlot == "" {
			continue
		}
		out[account.ProxySlot]++
	}
	return out
}
