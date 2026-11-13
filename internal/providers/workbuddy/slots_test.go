package workbuddy

import (
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

func TestWorkBuddyProxySlotBinder(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-a", realmIntl, map[string]any{"proxySlot": "slot-a"})
	wbRouteAddAccount(t, f.intl, "uid-b", realmIntl, map[string]any{"proxySlot": "slot-b"})
	wbRouteAddAccount(t, f.intl, "uid-c", realmIntl, map[string]any{"proxySlot": "slot-b"})

	slots := transport.Slots{
		{ID: "slot-a", URL: "http://127.0.0.1:7001", Enabled: true},
		{ID: "slot-b", URL: "http://127.0.0.1:7002", Enabled: true},
	}
	if unbound := f.intl.ApplyProxySlots(slots); unbound != 0 {
		t.Fatalf("initial unbound = %d", unbound)
	}
	bindings := f.intl.ProxySlotBindings()
	if bindings["slot-a"] != 1 || bindings["slot-b"] != 2 {
		t.Fatalf("bindings = %#v", bindings)
	}

	// Disabling an account removes it from the bound count.
	account := f.intl.Pool().Get("uid-b")
	if account == nil {
		t.Fatal("uid-b missing")
	}
	account.Auth.Enabled = false
	if bindings := f.intl.ProxySlotBindings(); bindings["slot-b"] != 1 {
		t.Fatalf("disabled account counted: %#v", bindings)
	}

	// Removing slot-b unbinds both accounts and reports the count.
	if unbound := f.intl.ApplyProxySlots(transport.Slots{{ID: "slot-a", URL: "http://127.0.0.1:7001", Enabled: true}}); unbound != 2 {
		t.Fatalf("unbound after removal = %d", unbound)
	}
	for _, uid := range []string{"uid-b", "uid-c"} {
		if slot := f.intl.Pool().Get(uid).ProxySlot(); slot != "" {
			t.Fatalf("%s slot = %q", uid, slot)
		}
	}
}
