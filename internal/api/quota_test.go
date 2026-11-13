package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/probes"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type quotaAPIProvider struct {
	calls int
}

func (p *quotaAPIProvider) ProbeTargets(context.Context, providers.ProbeKind, time.Time, int) ([]providers.ProbeTarget, error) {
	return []providers.ProbeTarget{{UID: "u1", Realm: "cn"}}, nil
}
func (p *quotaAPIProvider) Probe(context.Context, providers.ProbeKind, providers.ProbeTarget) (providers.ProbeResult, error) {
	p.calls++
	return providers.ProbeResult{OK: true, Status: 200}, nil
}

func TestQuotaScanRunRoute(t *testing.T) {
	dir := t.TempDir()
	service := probes.NewService(probes.Service{
		Window:      probes.NewWindowState(dir),
		Quota:       probes.NewQuotaState(dir),
		WatchModels: func() []string { return []string{"m1"} },
		Now:         time.Now,
		Sleep:       func(time.Duration) {},
	})
	server := New(ServerOptions{Probes: service, PanelAuth: passThroughAuth})
	rec := doRequest(server, http.MethodPost, "/quota-scan/run", "{}", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no provider = %d %s", rec.Code, rec.Body)
	}

	provider := &quotaAPIProvider{}
	service.Provider = provider
	rec = doRequest(server, http.MethodPost, "/quota-scan/run", "{}", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["ok"] != true || numberText(body["sent"]) != "1" {
		t.Fatalf("run body = %#v", body)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d", provider.calls)
	}
}

func TestQuotaScanRequiresPanel(t *testing.T) {
	dir := t.TempDir()
	sessions := panelplatform.NewSessions(time.Hour)
	service := probes.NewService(probes.Service{
		Provider: &quotaAPIProvider{}, Window: probes.NewWindowState(dir),
		Quota: probes.NewQuotaState(dir), Now: time.Now,
	})
	server := New(ServerOptions{Probes: service, PanelSessions: sessions})
	rec := doRequest(server, http.MethodPost, "/quota-scan/run", "{}", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session = %d %s", rec.Code, rec.Body)
	}
	token := sessions.Create()
	rec = doRequest(server, http.MethodPost, "/quota-scan/run", "{}", map[string]string{"X-Panel-Token": token})
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("with session = %d %s", rec.Code, rec.Body)
	}
}
