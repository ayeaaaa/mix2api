package doubao

import (
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

func TestProxySlotBindingAndUnbind(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.pool.Add(
		map[string]string{"sessionid": "s"}, map[string]string{"web_id": "w"},
		"uid-proxy", "proxy test", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.pool.SetProxySlot(account.UID, "slot-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Merge(map[string]any{"proxy_slots": []any{
		map[string]any{"id": "slot-a", "url": "http://127.0.0.1:18080", "enabled": true},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := provider.proxyURL(account); got != "http://127.0.0.1:18080" {
		t.Fatalf("proxyURL = %q", got)
	}
	if got := provider.ProxySlotBindings()["slot-a"]; got != 1 {
		t.Fatalf("bindings = %d", got)
	}
	if unbound := provider.ApplyProxySlots(transport.Slots{}); unbound != 1 {
		t.Fatalf("unbound = %d, want 1", unbound)
	}
	if account.ProxySlot != "" {
		t.Fatalf("account still bound to %q", account.ProxySlot)
	}
	if len(provider.ProxySlotBindings()) != 0 {
		t.Fatalf("bindings after unbind = %#v", provider.ProxySlotBindings())
	}
}

func TestNewHTTPTransportKeepsProxy(t *testing.T) {
	tr, err := newHTTPTransport("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proxy == nil {
		t.Fatal("proxy resolver is nil")
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 must stay disabled for the Doubao SSE endpoint")
	}
}
