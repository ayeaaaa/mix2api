package codearts

import (
	"context"
	"strings"
	"testing"
	"time"
)

func enableCodeArtsAutoCheckin(t *testing.T, provider *Provider, creditsAutoClaim bool) {
	t.Helper()
	if _, err := SaveConfig(provider.store, map[string]any{
		"enabled":            true,
		"credits_auto_claim": creditsAutoClaim,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
}

func newCheckinJobFixture(t *testing.T, fake *creditFake) (*Provider, *codeartsCheckinJob, *Account) {
	t.Helper()
	provider, serverURL := newCreditsProvider(t, fake)
	account := addCreditsAccount(t, provider, "u1")
	account.mu.Lock()
	account.proxy = serverURL
	// Keep the token valid across the test slots without exercising the
	// separate token-refresh path.
	account.Auth.Expiration = time.Now().Add(48 * time.Hour)
	account.mu.Unlock()
	fake.setDelivery("u1", []any{})
	return provider, provider.Jobs()[0].(*codeartsCheckinJob), account
}

func TestCodeArtsCheckinJobRegistered(t *testing.T) {
	provider, _ := newCreditsProvider(t, newCreditFake())

	jobs := provider.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("Jobs() = %d jobs, want 1", len(jobs))
	}
	if got := jobs[0].Name(); got != codeartsCheckinJobName {
		t.Fatalf("job name = %q, want %q", got, codeartsCheckinJobName)
	}
}

func TestCodeArtsCheckinJobFollowsSharedCheckinSlots(t *testing.T) {
	fake := newCreditFake()
	provider, job, _ := newCheckinJobFixture(t, fake)
	enableCodeArtsAutoCheckin(t, provider, true)
	if _, err := provider.store.Merge(map[string]any{
		"schedule": map[string]any{"checkin": []any{0, 12}},
	}); err != nil {
		t.Fatalf("Merge schedule: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}

	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 12, hour, minute, 0, 0, time.Local)
	}

	if !job.Due(at(8, 0)) {
		t.Fatal("job should catch up the missed 00:00 slot at 08:00")
	}
	if got := job.NextRun(at(8, 0)); !got.Equal(at(12, 0)) {
		t.Fatalf("NextRun(08:00) = %v, want 12:00", got)
	}

	setCatalogNow(t, provider, at(8, 0))
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run(08:00): %v", err)
	}
	if job.Due(at(8, 5)) {
		t.Fatal("job must not run twice in the same slot")
	}
	if got := job.NextRun(at(8, 5)); !got.Equal(at(12, 0)) {
		t.Fatalf("NextRun(08:05) = %v, want 12:00", got)
	}
	if got := job.LastRun(); !got.Equal(at(8, 0)) {
		t.Fatalf("LastRun = %v, want 08:00", got)
	}

	setCatalogNow(t, provider, at(12, 1))
	if !job.Due(at(12, 1)) {
		t.Fatal("job should run at the 12:00 slot")
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run(12:01): %v", err)
	}
	if job.Due(at(12, 5)) {
		t.Fatal("job must not run twice in the 12:00 slot")
	}
	if got := job.NextRun(at(12, 5)); !got.Equal(at(0, 0).AddDate(0, 0, 1)) {
		t.Fatalf("NextRun(12:05) = %v, want tomorrow 00:00", got)
	}
	if got := fake.callCountFor("u1", creditDeliveryPath); got != 2 {
		t.Fatalf("delivery calls = %d, want 2 (one per slot)", got)
	}
}

func TestCodeArtsCheckinJobHonoursAutoCheckinSwitch(t *testing.T) {
	fake := newCreditFake()
	provider, job, _ := newCheckinJobFixture(t, fake)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)

	// Default CodeArts config is disabled, so no check-in is attempted.
	if job.Due(now) {
		t.Fatal("disabled provider must not be due")
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run(disabled): %v", err)
	}
	if got := fake.callCountFor("u1", creditDeliveryPath); got != 0 {
		t.Fatalf("disabled provider delivery calls = %d, want 0", got)
	}

	enableCodeArtsAutoCheckin(t, provider, false)
	if job.Due(now) {
		t.Fatal("credits_auto_claim=false must not be due")
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run(manual only): %v", err)
	}
	if got := fake.callCountFor("u1", creditDeliveryPath); got != 0 {
		t.Fatalf("manual-only delivery calls = %d, want 0", got)
	}

	enableCodeArtsAutoCheckin(t, provider, true)
	if !job.Due(now) {
		t.Fatal("enabled auto check-in should be due at the 09:00 slot")
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run(auto): %v", err)
	}
	if got := fake.callCountFor("u1", creditDeliveryPath); got != 1 {
		t.Fatalf("auto delivery calls = %d, want 1", got)
	}
}

func TestCodeArtsCheckinJobReportsPerAccountFailures(t *testing.T) {
	fake := newCreditFake()
	provider, job, _ := newCheckinJobFixture(t, fake)
	enableCodeArtsAutoCheckin(t, provider, true)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	fake.failDelivery("u1", 500, `{"error_msg":"upstream down"}`)

	err := job.Run(context.Background())
	if err == nil {
		t.Fatal("check-in failure must be reported to the scheduler")
	}
	if !strings.Contains(err.Error(), "u1") {
		t.Fatalf("failure error = %q, want account uid", err)
	}
	if !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("failure error = %q, want safe upstream reason", err)
	}
	if job.Due(now.Add(5 * time.Minute)) {
		t.Fatal("a failed slot must not rerun until the next slot")
	}
}
