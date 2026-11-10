package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	platformproxy "github.com/ayeaaaa/mix2api-v2/internal/platform/proxy"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type binderStub struct {
	realm    string
	bindings map[string]string
	enabled  map[string]bool
}

func (p *binderStub) ID() string                                  { return "binder" }
func (p *binderStub) Realm() string                               { return p.realm }
func (p *binderStub) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *binderStub) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *binderStub) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *binderStub) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *binderStub) Routes() []providers.Route { return nil }
func (p *binderStub) Jobs() []scheduler.Job     { return nil }
func (p *binderStub) ApplyProxySlots(slots transport.Slots) int {
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
func (p *binderStub) ProxySlotBindings() map[string]int {
	out := map[string]int{}
	for uid, id := range p.bindings {
		if id != "" && p.enabled[uid] {
			out[id]++
		}
	}
	return out
}

func TestProxySlotsAPI(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	binder := &binderStub{
		realm: "intl", bindings: map[string]string{"u1": "slot-1", "u2": "slot-2"},
		enabled: map[string]bool{"u1": true, "u2": true},
	}
	slots := platformproxy.New(store, []providers.Provider{binder})
	server := New(ServerOptions{ProxySlots: slots, PanelAuth: passThroughAuth})

	rec := doRequest(server, http.MethodPost, "/proxy/slots/save", `{"slots":[
		{"url":"127.0.0.1:7890"},
		{"url":"http://127.0.0.1:7891","name":"second"}
	]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	list, _ := body["slots"].([]any)
	if len(list) != 2 {
		t.Fatalf("saved slots = %#v", body)
	}
	first, _ := list[0].(map[string]any)
	second, _ := list[1].(map[string]any)
	if first["id"] != "slot-1" || second["id"] != "slot-2" {
		t.Fatalf("ids = %#v", list)
	}
	if textField(first["bound"]) != "1" || textField(second["bound"]) != "1" {
		t.Fatalf("bound = %#v", list)
	}

	rec = doRequest(server, http.MethodGet, "/proxy/slots", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d", rec.Code)
	}
	if list, _ := decodeBody(t, rec)["slots"].([]any); len(list) != 2 {
		t.Fatalf("list = %#v", list)
	}

	rec = doRequest(server, http.MethodPost, "/proxy/slots/save", `{"slots":[{"id":"slot-1","url":"http://127.0.0.1:7890"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second save status=%d body=%s", rec.Code, rec.Body)
	}
	if slots.LastUnbound() != 1 || binder.bindings["u2"] != "" {
		t.Fatalf("unbound = %d bindings=%#v", slots.LastUnbound(), binder.bindings)
	}

	rec = doRequest(server, http.MethodPost, "/proxy/discover", `{}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover status=%d", rec.Code)
	}
	rec = doRequest(server, http.MethodPost, "/proxy/slots/test", `{"id":"slot-1"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("test without probe status=%d body=%s", rec.Code, rec.Body)
	}
}
