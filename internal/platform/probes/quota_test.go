package probes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type quotaFakeProvider struct {
	targets []providers.ProbeTarget
	results []providers.ProbeResult
	calls   int
}

func (f *quotaFakeProvider) ProbeTargets(context.Context, providers.ProbeKind, time.Time, int) ([]providers.ProbeTarget, error) {
	return append([]providers.ProbeTarget(nil), f.targets...), nil
}
func (f *quotaFakeProvider) Probe(context.Context, providers.ProbeKind, providers.ProbeTarget) (providers.ProbeResult, error) {
	f.calls++
	if len(f.results) == 0 {
		return providers.ProbeResult{OK: true, Status: 200}, nil
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result, nil
}

func TestQuotaScanState(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000, 0)
	state := NewQuotaState(dir)
	state.Now = func() time.Time { return now }
	if !state.Ensure("u1", "m1", now, QuotaFirstDelay) {
		t.Fatal("first Ensure failed")
	}
	if state.Ensure("u1", "m1", now, QuotaFirstDelay) {
		t.Fatal("duplicate Ensure inserted")
	}
	row, _ := state.Get("u1", "m1")
	if row.NextAt != resetTime(now.Add(QuotaFirstDelay)) {
		t.Fatalf("first delay = %v", row.NextAt)
	}
	if due := state.Due(map[string]bool{"m1": true}, now, 6, false); len(due) != 0 {
		t.Fatalf("first delay not respected: %#v", due)
	}
	if due := state.Due(map[string]bool{"m1": true}, now.Add(QuotaFirstDelay), 6, false); len(due) != 1 {
		t.Fatalf("due = %#v", due)
	}
	state.Record("u1", "m1", now, true, time.Time{}, 0, "ok", "")
	if due := state.Due(map[string]bool{"m1": true}, now.Add(time.Hour), 6, false); len(due) != 0 {
		t.Fatalf("6h interval not respected: %#v", due)
	}
	if due := state.Due(map[string]bool{"m1": true}, now.Add(QuotaInterval), 6, false); len(due) != 1 {
		t.Fatalf("6h due = %#v", due)
	}
	state.Record("u1", "m1", now, false, now.Add(QuotaRetryDelay), 1, "error", "boom")
	row, _ = state.Get("u1", "m1")
	if row.Attempts != 1 || row.LastResult != "error" {
		t.Fatalf("failure row = %#v", row)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, QuotaStateFile))
	if len(raw) == 0 {
		t.Fatal("state not persisted")
	}
}

func TestProbeServicesAndNoDuplicate429Window(t *testing.T) {
	dir := t.TempDir()
	current := time.Unix(3000, 0)
	now := func() time.Time { return current }
	window := NewWindowState(dir)
	window.Now = now
	quota := NewQuotaState(dir)
	quota.Now = now
	provider := &quotaFakeProvider{targets: []providers.ProbeTarget{{UID: "u1", Realm: "cn"}}}
	service := NewService(Service{
		Provider: provider, Window: window, Quota: quota,
		WatchModels: func() []string { return []string{"m1"} },
		Now:         now,
		Sleep:       func(time.Duration) {},
	})
	sent, err := service.RunQuota(context.Background(), true)
	if err != nil || sent != 1 {
		t.Fatalf("run quota = %d err=%v", sent, err)
	}
	if _, ok := quota.Get("u1", "m1"); !ok {
		t.Fatal("quota row missing")
	}
	// A 429 window blocks the next forced scan even though force ignores 6h.
	if !window.Schedule("u1", "m1", current.Add(time.Hour), true, current, current) {
		t.Fatal("window schedule failed")
	}
	sent, err = service.RunQuota(context.Background(), true)
	if err != nil || sent != 0 {
		t.Fatalf("duplicate scan = %d err=%v", sent, err)
	}

	// Window probe success clears the window and provider cooldown was handled
	// by the provider hook; the platform only drops state.
	window.Schedule("u1", "m1", current.Add(time.Second), false, current, current)
	current = current.Add(2 * time.Second)
	provider.results = []providers.ProbeResult{{OK: true, Status: 200}}
	if sent := service.RunWindow(context.Background()); sent != 1 {
		t.Fatalf("window sent = %d", sent)
	}
	if window.HasWindow("u1", "m1") {
		t.Fatal("window survived success")
	}

	// A new 429 re-registers the window.
	provider.results = []providers.ProbeResult{{Status: 429, ResetAt: current.Add(2 * time.Hour), Detail: "throttled"}}
	window.Schedule("u1", "m1", current.Add(time.Second), false, current, current)
	current = current.Add(2 * time.Second)
	service.RunWindow(context.Background())
	if got, ok := window.Get("u1", "m1"); !ok || got.ResetAt <= resetTime(current.Add(time.Hour)) {
		t.Fatalf("new 429 not registered: %#v ok=%v", got, ok)
	}
}

func TestServiceWithoutProvider(t *testing.T) {
	service := NewService(Service{})
	if _, err := service.RunQuota(context.Background(), true); err != ErrNoProvider {
		t.Fatalf("no provider error = %v", err)
	}
	if sent := service.RunWindow(context.Background()); sent != 0 {
		t.Fatalf("window no-op sent = %d", sent)
	}
}

func TestQuotaProbeLegacyStatesAndDisabled(t *testing.T) {
	now := time.Now()
	scan := now.Add(-time.Hour).Unix()
	next := now.Add(time.Hour).Unix()
	for _, payload := range []string{
		`[{"uid":"u1","model":"m1","last_scan":"%d","next_at":"%d"}]`,
		`{"u1\u0000m1":{"uid":"u1","model":"m1","last_scan":%d,"next_at":%d}}`,
	} {
		payload = fmt.Sprintf(payload, scan, next)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, QuotaStateFile), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		state := NewQuotaState(dir)
		state.Now = func() time.Time { return now }
		row, ok := state.Get("u1", "m1")
		if !ok || int64(row.LastScan) != scan || int64(row.NextAt) != next {
			t.Fatalf("legacy state %s = %#v ok=%v", payload, row, ok)
		}
	}
	provider := &quotaFakeProvider{targets: []providers.ProbeTarget{{UID: "u1"}}}
	service := NewService(Service{
		Provider: provider, Window: NewWindowState(t.TempDir()), Quota: NewQuotaState(t.TempDir()),
		WatchModels: func() []string { return []string{"m1"} },
		Enabled:     func() bool { return false },
		Now:         func() time.Time { return now },
	})
	sent, err := service.RunQuota(context.Background(), true)
	if err != nil || sent != 0 || provider.calls != 0 {
		t.Fatalf("disabled quota = sent %d err=%v calls=%d", sent, err, provider.calls)
	}
}

type realmProbeProvider struct {
	realm string
	uid   string
	calls []string
}

func (p *realmProbeProvider) Realm() string { return p.realm }
func (p *realmProbeProvider) ProbeTargets(context.Context, providers.ProbeKind, time.Time, int) ([]providers.ProbeTarget, error) {
	return []providers.ProbeTarget{{UID: p.uid, Realm: p.realm}}, nil
}
func (p *realmProbeProvider) Probe(_ context.Context, _ providers.ProbeKind, target providers.ProbeTarget) (providers.ProbeResult, error) {
	p.calls = append(p.calls, target.UID)
	return providers.ProbeResult{OK: true, Status: 200}, nil
}

func TestRunQuotaDispatchesBothRealmProviders(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	intl := &realmProbeProvider{realm: "intl", uid: "u-intl"}
	cn := &realmProbeProvider{realm: "cn", uid: "u-cn"}
	service := NewService(Service{
		Providers: []providers.ProbeProvider{intl, cn},
		Window:    NewWindowState(t.TempDir()),
		Quota:     NewQuotaState(t.TempDir()),
		WatchModels: func() []string {
			return []string{"m1"}
		},
		Now:   func() time.Time { return now },
		Sleep: func(time.Duration) {},
	})
	sent, err := service.RunQuota(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 2 {
		t.Fatalf("sent = %d, want 2", sent)
	}
	if len(intl.calls) != 1 || intl.calls[0] != "u-intl" {
		t.Fatalf("intl calls = %v", intl.calls)
	}
	if len(cn.calls) != 1 || cn.calls[0] != "u-cn" {
		t.Fatalf("cn calls = %v", cn.calls)
	}
}

func TestRunWindowDispatchesBothRealmProviders(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	intl := &realmProbeProvider{realm: "intl", uid: "u-intl"}
	cn := &realmProbeProvider{realm: "cn", uid: "u-cn"}
	current := now
	window := NewWindowState(t.TempDir())
	window.Now = func() time.Time { return current }
	window.ScheduleRealm("u-intl", "m1", "intl", now.Add(time.Second), false, now, now)
	window.ScheduleRealm("u-cn", "m1", "cn", now.Add(time.Second), false, now, now)
	current = now.Add(2 * time.Second)
	service := NewService(Service{
		Providers: []providers.ProbeProvider{intl, cn},
		Window:    window,
		Quota:     NewQuotaState(t.TempDir()),
		Now:       func() time.Time { return current },
		Sleep:     func(time.Duration) {},
	})
	if sent := service.RunWindow(context.Background()); sent != 2 {
		t.Fatalf("sent = %d, want 2", sent)
	}
	if len(intl.calls) != 1 || intl.calls[0] != "u-intl" {
		t.Fatalf("intl window calls = %v", intl.calls)
	}
	if len(cn.calls) != 1 || cn.calls[0] != "u-cn" {
		t.Fatalf("cn window calls = %v", cn.calls)
	}
}
