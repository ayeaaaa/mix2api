package workbuddy

import (
	"context"
	"testing"
	"time"
)

func setTestAccountCredits(t *testing.T, account *Account, updated time.Time, remain float64) {
	t.Helper()
	account.mu.Lock()
	account.Credits = &CreditSnapshot{Remain: remain, UpdatedAt: float64(updated.Unix()), UpdatedISO: updated.Format("2006-01-02 15:04:05")}
	account.mu.Unlock()
}

func TestCreditsRefresherRefreshesOnlyOldestStaleEnabledAccount(t *testing.T) {
	dir := t.TempDir()
	provider, pool := newPoolProvider(t, realmIntl, dir)
	writePoolSettings(t, dir, map[string]any{"credits_refresh_hours": 12})
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	provider.creditsRefreshNow = func() time.Time { return now }

	// A is stale, B is older, C is fresh, D is disabled.
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{"enabled": true})
	writePoolAccount(t, dir, "b", realmIntl, map[string]any{"enabled": true})
	writePoolAccount(t, dir, "c", realmIntl, map[string]any{"enabled": true})
	writePoolAccount(t, dir, "d", realmIntl, map[string]any{"enabled": false})
	// Re-load after writing the account files.
	provider, pool = newPoolProvider(t, realmIntl, dir)
	provider.creditsRefreshNow = func() time.Time { return now }
	setTestAccountCredits(t, pool.Get("a"), now.Add(-13*time.Hour), 10)
	setTestAccountCredits(t, pool.Get("b"), now.Add(-20*time.Hour), 20)
	setTestAccountCredits(t, pool.Get("c"), now.Add(-time.Hour), 30)
	setTestAccountCredits(t, pool.Get("d"), now.Add(-48*time.Hour), 40)

	var refreshed []string
	provider.creditsRefreshHook = func(_ context.Context, account *Account) CreditRefreshRow {
		refreshed = append(refreshed, accountUID(account))
		account.mu.Lock()
		account.Credits.UpdatedAt = float64(now.Unix())
		account.mu.Unlock()
		return CreditRefreshRow{UID: accountUID(account), OK: true, Credits: creditSnapshotOf(account)}
	}

	if !provider.refreshStalestCredit(context.Background()) {
		t.Fatal("refreshStalestCredit returned false, want true")
	}
	if len(refreshed) != 1 || refreshed[0] != "b" {
		t.Fatalf("refreshed = %v, want only b", refreshed)
	}
}

func TestCreditsRefresherHonoursTTLAndSkipsFreshAccounts(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{"enabled": true})
	writePoolSettings(t, dir, map[string]any{"credits_refresh_hours": 12})
	provider, pool := newPoolProvider(t, realmIntl, dir)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	provider.creditsRefreshNow = func() time.Time { return now }
	setTestAccountCredits(t, pool.Get("a"), now.Add(-2*time.Hour), 10)

	called := 0
	provider.creditsRefreshHook = func(context.Context, *Account) CreditRefreshRow {
		called++
		return CreditRefreshRow{OK: true}
	}
	if provider.refreshStalestCredit(context.Background()) {
		t.Fatal("fresh account was refreshed")
	}
	if called != 0 {
		t.Fatalf("hook called %d times, want 0", called)
	}

	writePoolSettings(t, dir, map[string]any{"credits_refresh_hours": 0})
	setTestAccountCredits(t, pool.Get("a"), now.Add(-48*time.Hour), 10)
	if provider.refreshStalestCredit(context.Background()) {
		t.Fatal("credits refresh ran with TTL 0")
	}
	if called != 0 {
		t.Fatalf("hook called %d times with TTL 0, want 0", called)
	}
}

func TestCreditsRefresherParksFailedAccountForSixHours(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{"enabled": true})
	writePoolSettings(t, dir, map[string]any{"credits_refresh_hours": 12})
	provider, pool := newPoolProvider(t, realmIntl, dir)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	provider.creditsRefreshNow = func() time.Time { return now }
	setTestAccountCredits(t, pool.Get("a"), now.Add(-48*time.Hour), 10)

	called := 0
	provider.creditsRefreshHook = func(context.Context, *Account) CreditRefreshRow {
		called++
		return CreditRefreshRow{UID: "a", OK: false, Error: "billing unavailable"}
	}
	if !provider.refreshStalestCredit(context.Background()) {
		t.Fatal("first failed refresh was not attempted")
	}
	if called != 1 {
		t.Fatalf("hook called %d times, want 1", called)
	}

	now = now.Add(time.Hour)
	if provider.refreshStalestCredit(context.Background()) {
		t.Fatal("parked account was retried after one hour")
	}
	if called != 1 {
		t.Fatalf("hook called %d times after parking, want 1", called)
	}

	now = now.Add(5*time.Hour + time.Second)
	if !provider.refreshStalestCredit(context.Background()) {
		t.Fatal("account was not retried after six-hour park")
	}
	if called != 2 {
		t.Fatalf("hook called %d times after park expiry, want 2", called)
	}
}
