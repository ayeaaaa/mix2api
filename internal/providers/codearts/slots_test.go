package codearts

import (
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

func TestCodeArtsProxySlotBinder(t *testing.T) {
	provider, accounts := newT5Provider(t, "slot-a", "slot-b")
	if err := provider.Pool().SetProxySlot("slot-a", "slot-a"); err != nil {
		t.Fatal(err)
	}
	if err := provider.Pool().SetProxySlot("slot-b", "slot-b"); err != nil {
		t.Fatal(err)
	}
	slots := transport.Slots{
		{ID: "slot-a", URL: "http://127.0.0.1:7101", Enabled: true},
		{ID: "slot-b", URL: "http://127.0.0.1:7102", Enabled: true},
	}
	if unbound := provider.ApplyProxySlots(slots); unbound != 0 {
		t.Fatalf("initial unbound = %d", unbound)
	}
	if bindings := provider.ProxySlotBindings(); bindings["slot-a"] != 1 || bindings["slot-b"] != 1 {
		t.Fatalf("bindings = %#v", bindings)
	}
	if accounts["slot-a"].ProxySlot() != "slot-a" {
		t.Fatalf("slot-a binding = %q", accounts["slot-a"].ProxySlot())
	}
	if unbound := provider.ApplyProxySlots(transport.Slots{
		{ID: "slot-a", URL: "http://127.0.0.1:7101", Enabled: true},
	}); unbound != 1 {
		t.Fatalf("removed slot unbound = %d", unbound)
	}
	if got := accounts["slot-b"].ProxySlot(); got != "" {
		t.Fatalf("stale binding = %q", got)
	}
}
