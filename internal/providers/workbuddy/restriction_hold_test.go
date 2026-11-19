package workbuddy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func newRestrictionTestPool(t *testing.T, settings map[string]any) (*Provider, *Pool) {
	t.Helper()
	dir := t.TempDir()
	for _, uid := range []string{"hold-a", "hold-b", "hold-c"} {
		writeChatAccount(t, dir, uid, realmIntl, nil)
	}
	provider := setupChatProvider(t, realmIntl, dir, settings)
	return provider, provider.Pool()
}

func TestPoolRestrictionWaveHoldTriggersOnDistinctAccounts(t *testing.T) {
	_, pool := newRestrictionTestPool(t, map[string]any{"restriction_hold_seconds": 1800})
	if hold := pool.RestrictionHold(); hold != 0 {
		t.Fatalf("fresh pool hold = %s, want 0", hold)
	}
	if err := pool.NoteRestricted(pool.Get("hold-a"), "403 / 11140"); err != nil {
		t.Fatalf("first restriction: %v", err)
	}
	if hold := pool.RestrictionHold(); hold != 0 {
		t.Fatalf("one restricted account triggered a hold: %s", hold)
	}
	if err := pool.NoteRestricted(pool.Get("hold-b"), "403 / 11140"); err != nil {
		t.Fatalf("second restriction: %v", err)
	}
	hold := pool.RestrictionHold()
	if hold < 29*time.Minute || hold > 31*time.Minute {
		t.Fatalf("hold = %s, want about 30m", hold)
	}
	if holds := pool.RestrictionHolds(); holds["intl"] < 1700 {
		t.Fatalf("restriction holds = %#v, want intl remaining seconds", holds)
	}
}

func TestPoolRestrictionWaveHoldIgnoresRepeatedAccount(t *testing.T) {
	_, pool := newRestrictionTestPool(t, map[string]any{"restriction_hold_seconds": 1800})
	account := pool.Get("hold-a")
	for i := 0; i < 3; i++ {
		if err := pool.NoteRestricted(account, "403 / 11140"); err != nil {
			t.Fatalf("restriction %d: %v", i, err)
		}
	}
	if hold := pool.RestrictionHold(); hold != 0 {
		t.Fatalf("one repeated account triggered a hold: %s", hold)
	}
}

func TestPoolRestrictionWaveHoldDisabledByZero(t *testing.T) {
	_, pool := newRestrictionTestPool(t, map[string]any{"restriction_hold_seconds": 0})
	_ = pool.NoteRestricted(pool.Get("hold-a"), "403 / 11140")
	_ = pool.NoteRestricted(pool.Get("hold-b"), "403 / 11140")
	if hold := pool.RestrictionHold(); hold != 0 {
		t.Fatalf("zero hold_seconds still paused: %s", hold)
	}
}

func TestPoolRestrictionWaveHoldWindowExpires(t *testing.T) {
	_, pool := newRestrictionTestPool(t, map[string]any{"restriction_hold_seconds": 1800})
	now := time.Now()
	pool.now = func() time.Time { return now }
	_ = pool.NoteRestricted(pool.Get("hold-a"), "403 / 11140")
	now = now.Add(16 * time.Minute)
	_ = pool.NoteRestricted(pool.Get("hold-b"), "403 / 11140")
	if hold := pool.RestrictionHold(); hold != 0 {
		t.Fatalf("events outside the 15m window triggered a hold: %s", hold)
	}
}

func TestPoolRestrictionHoldClampAndReset(t *testing.T) {
	_, pool := newRestrictionTestPool(t, map[string]any{"restriction_hold_seconds": 1800})
	_ = pool.NoteRestricted(pool.Get("hold-a"), "403 / 11140")
	_ = pool.NoteRestricted(pool.Get("hold-b"), "403 / 11140")

	pool.ClampRestrictionHolds(60)
	if hold := pool.RestrictionHold(); hold <= 0 || hold > 61*time.Second {
		t.Fatalf("clamped hold = %s, want <= 60s", hold)
	}
	pool.ResetRestrictionState()
	if hold := pool.RestrictionHold(); hold != 0 {
		t.Fatalf("reset hold = %s, want 0", hold)
	}
	if holds := pool.RestrictionHolds(); len(holds) != 0 {
		t.Fatalf("reset holds = %#v, want empty", holds)
	}
}

func TestChatRestrictionHoldBlocksDispatch(t *testing.T) {
	provider, pool := newRestrictionTestPool(t, map[string]any{"restriction_hold_seconds": 1800})
	fake := &chatTestFake{body: ssePayload(chatChunk("hello", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	pool.noteRestrictionEvent("hold-a")
	pool.noteRestrictionEvent("hold-b")
	if hold := pool.RestrictionHold(); hold <= 0 {
		t.Fatal("test hold was not armed")
	}

	_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	}))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindRateLimited {
		t.Fatalf("hold error = %v, want KindRateLimited", err)
	}
	if apiErr.RetryAfter <= 0 {
		t.Fatalf("hold error RetryAfter = %s, want positive", apiErr.RetryAfter)
	}
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("hold still sent %d upstream request(s)", len(calls))
	}
}

func TestMediaRestrictionHoldBlocksDispatch(t *testing.T) {
	provider, pool := newRestrictionTestPool(t, map[string]any{"restriction_hold_seconds": 1800})
	pool.noteRestrictionEvent("hold-a")
	pool.noteRestrictionEvent("hold-b")
	if _, err := provider.pickMediaAccount("image-model", nil, 1, nil); err == nil {
		t.Fatal("media pick ignored the restriction hold")
	}
}

func TestJobsSkipDuringRestrictionHold(t *testing.T) {
	f := newJobsFixture(t)
	f.addIntlTaskAccount(t, "intl-hold", true, true)
	if _, err := f.store.Merge(map[string]any{
		"daily_chat_web":           false,
		"restriction_hold_seconds": 1800,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	pool := f.intl.Pool()
	pool.noteRestrictionEvent("uid-a")
	pool.noteRestrictionEvent("uid-b")
	if hold := pool.RestrictionHold(); hold <= 0 {
		t.Fatal("test hold was not armed")
	}

	f.setNow(time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))
	report, err := jobsByName(t, f.intl)["workbuddy-daily-activity"].run(context.Background(), false)
	if err != nil {
		t.Fatalf("daily job during hold: %v", err)
	}
	if !report.Ran || report.Checked != 1 || report.Skipped != 1 || report.Succeeded != 0 || report.Failed != 0 {
		t.Fatalf("daily job during hold = %+v, want one skipped account", report)
	}
}

func TestRoutesRestrictionHoldSettingsAndReset(t *testing.T) {
	f := newWBRoutesFixture(t)
	view, err := f.intl.SettingsView(context.Background())
	if err != nil {
		t.Fatalf("settings view: %v", err)
	}
	if got := view["restriction_hold_seconds"]; got != 1800 {
		t.Fatalf("default restriction_hold_seconds = %#v, want 1800", got)
	}
	if holds, _ := view["restriction_holds"].(map[string]int); len(holds) != 0 {
		t.Fatalf("default restriction_holds = %#v, want empty", view["restriction_holds"])
	}

	if err := f.intl.SaveSettings(context.Background(), map[string]any{"restriction_hold_seconds": 60}); err != nil {
		t.Fatalf("save hold seconds: %v", err)
	}
	view, _ = f.intl.SettingsView(context.Background())
	if got := view["restriction_hold_seconds"]; got != 60 {
		t.Fatalf("saved restriction_hold_seconds = %#v, want 60", got)
	}

	f.intl.Pool().noteRestrictionEvent("uid-a")
	f.intl.Pool().noteRestrictionEvent("uid-b")
	view, _ = f.intl.SettingsView(context.Background())
	holds, _ := view["restriction_holds"].(map[string]int)
	if holds["intl"] <= 0 {
		t.Fatalf("view restriction_holds = %#v, want active intl hold", view["restriction_holds"])
	}

	rec := f.do(http.MethodPost, "/settings/restriction/reset", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("restriction reset status=%d body=%s", rec.Code, rec.Body)
	}
	reset := decodeWBRoutesJSON(t, rec)
	if reset["ok"] != true || len(reset["restriction_holds"].(map[string]any)) != 0 {
		t.Fatalf("restriction reset = %#v", reset)
	}
	if f.intl.Pool().RestrictionHold() != 0 {
		t.Fatal("reset did not clear the live hold")
	}

	if err := f.intl.SaveSettings(context.Background(), map[string]any{"restriction_hold_seconds": -1}); err == nil {
		t.Fatal("negative restriction_hold_seconds was accepted")
	}
}
