package raccoon

import (
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

func TestRaccoonProxySlotBinder(t *testing.T) {
	fixture := newRoutesFixture(t)
	fixture.importFlat(t, fixture.uid, fixture.token, nil)
	if err := fixture.p.pool.SetProxySlot(fixture.uid, "slot-a"); err != nil {
		t.Fatal(err)
	}
	slots := transport.Slots{{ID: "slot-a", URL: "http://127.0.0.1:7201", Enabled: true}}
	if unbound := fixture.p.ApplyProxySlots(slots); unbound != 0 {
		t.Fatalf("initial unbound = %d", unbound)
	}
	if bindings := fixture.p.ProxySlotBindings(); bindings["slot-a"] != 1 {
		t.Fatalf("bindings = %#v", bindings)
	}
	if unbound := fixture.p.ApplyProxySlots(nil); unbound != 1 {
		t.Fatalf("removed slot unbound = %d", unbound)
	}
	account := fixture.p.pool.Get(fixture.uid)
	if account == nil {
		t.Fatal("account missing")
	}
	if got := account.ProxySlot(); got != "" {
		t.Fatalf("stale binding = %q", got)
	}
}
