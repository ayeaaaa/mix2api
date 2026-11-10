package providers

import (
	"context"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// SettingsContributor is implemented by providers that own a section of the
// panel settings document. Platform settings services merge the view into the
// base payload and dispatch saves without importing a concrete provider.
type SettingsContributor interface {
	SettingsView(ctx context.Context) (map[string]any, error)
	SaveSettings(ctx context.Context, patch map[string]any) error
}

// RealmSelector is implemented by providers that expose a switchable default
// upstream exit. Only WorkBuddy currently implements it.
type RealmSelector interface {
	ActiveRealm(ctx context.Context) string
	SetActiveRealm(ctx context.Context, realm string) error
	RealmOptions() []string
}

// ProxySlotBinder is implemented by providers with proxy-slot-bound accounts.
// ApplyProxySlots returns how many accounts were unbound because their slot
// disappeared; ProxySlotBindings returns enabled-account counts per slot id.
type ProxySlotBinder interface {
	ApplyProxySlots(slots transport.Slots) int
	ProxySlotBindings() map[string]int
}
