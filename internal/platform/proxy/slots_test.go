package proxy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type slotFakeProvider struct {
	realm    string
	bindings map[string]string // uid -> slot
	enabled  map[string]bool
	applied  transport.Slots
	calls    int
}

func (p *slotFakeProvider) ID() string                                  { return "fake-" + p.realm }
func (p *slotFakeProvider) Realm() string                               { return p.realm }
func (p *slotFakeProvider) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *slotFakeProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *slotFakeProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *slotFakeProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *slotFakeProvider) Routes() []providers.Route { return nil }
func (p *slotFakeProvider) Jobs() []scheduler.Job     { return nil }

func (p *slotFakeProvider) ApplyProxySlots(slots transport.Slots) int {
	p.calls++
	p.applied = append(transport.Slots(nil), slots...)
	valid := map[string]bool{}
	for _, slot := range slots {
		valid[slot.ID] = true
	}
	unbound := 0
	for uid, id := range p.bindings {
		if id != "" && !valid[id] {
			p.bindings[uid] = ""
			unbound++
		}
	}
	return unbound
}

func (p *slotFakeProvider) ProxySlotBindings() map[string]int {
	out := map[string]int{}
	for uid, id := range p.bindings {
		if id != "" && p.enabled[uid] {
			out[id]++
		}
	}
	return out
}

type noHookProvider struct{ realm string }

func (p *noHookProvider) ID() string                                  { return "nohook" }
func (p *noHookProvider) Realm() string                               { return p.realm }
func (p *noHookProvider) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *noHookProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *noHookProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *noHookProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *noHookProvider) Routes() []providers.Route { return nil }
func (p *noHookProvider) Jobs() []scheduler.Job     { return nil }

func TestProxySlots(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	workbuddy := &slotFakeProvider{
		realm: "intl", bindings: map[string]string{"u1": "slot-1", "u2": "slot-2"}, enabled: map[string]bool{"u1": true, "u2": true},
	}
	codearts := &slotFakeProvider{
		realm: "codearts", bindings: map[string]string{"c1": "slot-1"}, enabled: map[string]bool{"c1": true},
	}
	raccoon := &slotFakeProvider{
		realm: "raccoon", bindings: map[string]string{"r1": "slot-2"}, enabled: map[string]bool{"r1": false},
	}
	slots := New(store, []providers.Provider{workbuddy, codearts, raccoon, &noHookProvider{realm: "other"}})

	saved, err := slots.Replace([]Slot{
		{URL: "http://127.0.0.1:7890"},
		{URL: "127.0.0.1:7891:user:pass"},
		{URL: "http://127.0.0.1:7890", Name: "duplicate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].ID != "slot-1" || saved[1].ID != "slot-2" {
		t.Fatalf("saved = %#v", saved)
	}
	if saved[1].URL != "http://user:pass@127.0.0.1:7891" {
		t.Fatalf("normalized URL = %q", saved[1].URL)
	}
	if saved[0].Bound != 2 || saved[1].Bound != 1 {
		t.Fatalf("bound counts = %#v", saved)
	}
	if workbuddy.calls != 1 || codearts.calls != 1 || raccoon.calls != 1 {
		t.Fatalf("binder calls = %d/%d/%d", workbuddy.calls, codearts.calls, raccoon.calls)
	}

	// Removing slot-2 unbinds both providers that used it.
	saved, err = slots.Replace([]Slot{{ID: "slot-1", URL: "http://127.0.0.1:7890", Name: "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Bound != 2 {
		t.Fatalf("after remove = %#v", saved)
	}
	if workbuddy.bindings["u2"] != "" || raccoon.bindings["r1"] != "" {
		t.Fatalf("stale bindings = %#v %#v", workbuddy.bindings, raccoon.bindings)
	}
	if got := slots.LastUnbound(); got != 2 {
		t.Fatalf("LastUnbound = %d", got)
	}

	// Ids are never recycled: the next new slot uses slot-3 even though the
	// stored list currently holds only slot-1.
	saved, err = slots.Replace([]Slot{
		{ID: "slot-1", URL: "http://127.0.0.1:7890", Name: "kept"},
		{URL: "http://127.0.0.1:7892"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[1].ID != "slot-3" {
		t.Fatalf("new id = %#v", saved)
	}
	data, _ := store.Load()
	if text(data["proxy_slot_seq"]) != "3" {
		t.Fatalf("proxy_slot_seq = %#v", data["proxy_slot_seq"])
	}

	// Probe metadata survives update and reads back.
	updated, found, err := slots.Update("slot-1", map[string]any{"ip": "203.0.113.5", "country": "US", "country_code": "us", "ip_type": "RESIDENTIAL"}, map[string]any{"name": "auto"})
	if err != nil || !found {
		t.Fatalf("update = %#v found=%v err=%v", updated, found, err)
	}
	if updated.IP != "203.0.113.5" || updated.CountryCode != "US" || updated.IPType != "residential" || updated.Name != "kept" {
		t.Fatalf("updated = %#v", updated)
	}
	list, err := slots.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].IP != "203.0.113.5" || list[0].Label == "" {
		t.Fatalf("list = %#v", list)
	}

	// Empty names stay empty in storage; Label derives from the exit.
	if list[1].Name != "" || list[1].Label == "" {
		t.Fatalf("auto label = %#v", list[1])
	}
	all := slots.Bindings()
	if all["slot-1"] != 2 {
		t.Fatalf("bindings = %#v", all)
	}
}
