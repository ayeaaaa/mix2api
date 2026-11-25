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
	if got := fake.callCountFor("u1", creditDeliveryPath); got != 1 {
		t.Fatalf("delivery calls = %d, want 1 (already checked today)", got)
	}
}

func TestCodeArtsCheckinJobHourlyCatchUp(t *testing.T) {
	fake := newCreditFake()
	provider, job, first := newCheckinJobFixture(t, fake)
	enableCodeArtsAutoCheckin(t, provider, true)
	if _, err := SaveConfig(provider.store, map[string]any{"checkin_time": "00:02"}); err != nil {
		t.Fatalf("SaveConfig checkin_time: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings checkin_time: %v", err)
	}
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 12, hour, minute, 0, 0, time.Local)
	}

	setCatalogNow(t, provider, at(0, 2))
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	if got := fake.callCountFor("u1", creditDeliveryPath); got != 1 {
		t.Fatalf("initial delivery calls = %d, want 1", got)
	}

	second := addCreditsAccount(t, provider, "u2")
	second.mu.Lock()
	second.proxy = first.proxy
	second.Auth.Expiration = time.Now().Add(48 * time.Hour)
	second.State.Disabled = true // auto-disabled, not manual
	second.mu.Unlock()
	fake.setDelivery("u2", []any{})

	setCatalogNow(t, provider, at(0, 30))
	if job.Due(at(0, 30)) {
		t.Fatal("same-hour catch-up must not run twice")
	}
	setCatalogNow(t, provider, at(1, 0))
	if !job.Due(at(1, 0)) {
		t.Fatal("next clock hour should catch up the unchecked account")
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("catch-up Run: %v", err)
	}
	if got := fake.callCountFor("u2", creditDeliveryPath); got != 1 {
		t.Fatalf("catch-up delivery calls = %d, want 1", got)
	}
	if job.Due(at(1, 5)) {
		t.Fatal("checked account must not be retried in the same hour")
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

func TestCodeArtsCheckinTimeOverridesSharedSlots(t *testing.T) {
	fake := newCreditFake()
	provider, job, _ := newCheckinJobFixture(t, fake)
	enableCodeArtsAutoCheckin(t, provider, true)
	if _, err := SaveConfig(provider.store, map[string]any{"checkin_time": "09:15"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 12, hour, minute, 0, 0, time.Local)
	}
	if job.Due(at(9, 14)) {
		t.Fatal("job due before the explicit 09:15 time")
	}
	if !job.Due(at(9, 15)) {
		t.Fatal("job not due at the explicit 09:15 time")
	}
	if got := job.NextRun(at(9, 14)); !got.Equal(at(9, 15)) {
		t.Fatalf("NextRun(09:14) = %v, want 09:15", got)
	}
	if got := provider.Config().CheckinTime; got != "09:15" {
		t.Fatalf("CheckinTime = %q, want 09:15", got)
	}
	slots := provider.checkinScheduleSlots()
	if len(slots) != 1 || slots[0].String() != "09:15" {
		t.Fatalf("checkinScheduleSlots() = %#v, want one 09:15 slot", slots)
	}

	// With no CodeArts-specific time, follow the shared WorkBuddy checkin_time.
	if _, err := SaveConfig(provider.store, map[string]any{"checkin_time": ""}); err != nil {
		t.Fatalf("clear CodeArts checkin time: %v", err)
	}
	if _, err := provider.store.Merge(map[string]any{
		"schedule": map[string]any{"checkin_time": "10:30"},
	}); err != nil {
		t.Fatalf("save shared checkin time: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	slots = provider.checkinScheduleSlots()
	if len(slots) != 1 || slots[0].String() != "10:30" {
		t.Fatalf("shared checkin_time slots = %#v, want one 10:30 slot", slots)
	}
}

func TestNormalizeCheckinTime(t *testing.T) {
	cases := map[string]string{
		"9:5":      "09:05",
		"09:15":    "09:15",
		"23:59":    "23:59",
		"24:00":    "",
		"09:60":    "",
		"nonsense": "",
		"":         "",
	}
	for input, want := range cases {
		if got := normalizeClockTime(input); got != want {
			t.Fatalf("normalizeClockTime(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCodeArtsCheckinRetryState(t *testing.T) {
	fake := newCreditFake()
	provider, job, account := newCheckinJobFixture(t, fake)
	enableCodeArtsAutoCheckin(t, provider, true)
	if _, err := SaveConfig(provider.store, map[string]any{"checkin_time": "09:15"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 12, hour, minute, 0, 0, time.Local)
	}

	account.Checkin = CheckinState{"ok": true, "checked_at": at(9, 15).Format("2006-01-02T15:04:05")}
	job.markSlot(at(9, 15))
	job.updateRetry(at(9, 15), CheckinResult{OK: false}, nil)
	if job.Due(at(9, 44)) {
		t.Fatal("retry ran before the 30-minute interval")
	}
	if !job.Due(at(9, 45)) {
		t.Fatal("retry not due at 09:45")
	}
	if got := job.NextRun(at(9, 16)); !got.Equal(at(9, 45)) {
		t.Fatalf("NextRun(09:16) = %v, want the 09:45 retry", got)
	}

	job.updateRetry(at(9, 45), CheckinResult{OK: true}, nil)
	if job.Due(at(9, 46)) {
		t.Fatal("successful retry left a retry pending")
	}

	// A retry that would fall on the next day is not scheduled.
	job.updateRetry(at(23, 50), CheckinResult{OK: false}, nil)
	if job.Due(at(23, 51)) {
		t.Fatal("retry was scheduled past local midnight")
	}
}

func TestCodeArtsCheckinRetryIntervalSetting(t *testing.T) {
	fake := newCreditFake()
	provider, job, account := newCheckinJobFixture(t, fake)
	enableCodeArtsAutoCheckin(t, provider, true)
	if _, err := SaveConfig(provider.store, map[string]any{
		"checkin_time": "09:15", "checkin_retry_minutes": 45,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 12, hour, minute, 0, 0, time.Local)
	}
	account.Checkin = CheckinState{"ok": true, "checked_at": at(9, 15).Format("2006-01-02T15:04:05")}
	job.markSlot(at(9, 15))
	job.updateRetry(at(9, 15), CheckinResult{OK: false}, nil)
	if job.Due(at(9, 59)) {
		t.Fatal("retry ran before the 45-minute interval")
	}
	if !job.Due(at(10, 0)) {
		t.Fatal("retry not due at 09:15 + 45m")
	}

	if _, err := SaveConfig(provider.store, map[string]any{"checkin_retry_minutes": 0}); err != nil {
		t.Fatalf("disable retry: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	job.updateRetry(at(10, 0), CheckinResult{OK: false}, nil)
	if job.Due(at(10, 1)) {
		t.Fatal("retry ran with checkin_retry_minutes=0")
	}

	if _, err := SaveConfig(provider.store, map[string]any{"checkin_retry_minutes": 999}); err != nil {
		t.Fatalf("clamp retry: %v", err)
	}
	if got := LoadConfig(provider.store).CheckinRetryMinutes; got != maxCheckinRetryMinutes {
		t.Fatalf("clamped retry = %d, want %d", got, maxCheckinRetryMinutes)
	}
}

func TestIntlHasNoCheckinJob(t *testing.T) {
	p, err := NewWithStation(newTestStore(t), International)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range p.Jobs() {
		if strings.Contains(strings.ToLower(job.Name()), "checkin") {
			t.Fatalf("intl job = %#v", job)
		}
	}
}
