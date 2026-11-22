package raccoon

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
)

func clearDailyGrantBills(f *pointsFake) {
	f.update(func(f *pointsFake) {
		payload := f.billsByCursor[""]
		if payload == nil {
			payload = defaultFakeBills(f.now)
			f.billsByCursor[""] = payload
		}
		data, _ := payload["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
			payload["data"] = data
		}
		data["items"] = []any{}
		data["has_more"] = false
		data["next_cursor"] = ""
		f.dailyGrantOnSetting = true
	})
}

func TestDailyGrantToday(t *testing.T) {
	fixture := newPointsFixture(t)
	account := fixture.p.Get(fixture.uid)
	if account == nil {
		t.Fatal("account missing")
	}
	points, err := fixture.p.DailyGrantToday(context.Background(), account)
	if err != nil {
		t.Fatalf("DailyGrantToday: %v", err)
	}
	if points != 300 {
		t.Fatalf("DailyGrantToday = %d, want 300", points)
	}
}

func TestDailyActiveSkipsAlreadyGranted(t *testing.T) {
	fixture := newPointsFixture(t)
	summary, err := fixture.p.DailyActive(context.Background(), fixture.uid, true)
	if err != nil {
		t.Fatalf("DailyActive: %v", err)
	}
	if !summary.OK || summary.Granted != 1 || summary.Points != 300 {
		t.Fatalf("summary = %+v, want ok/granted/300", summary)
	}
	if len(summary.Results) != 1 || summary.Results[0].Chat {
		t.Fatalf("result = %+v, want no chat", summary.Results)
	}
	if fixture.fake.chatCalls != 0 {
		t.Fatalf("chat calls = %d, want 0", fixture.fake.chatCalls)
	}
	if fixture.fake.refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0 when already granted", fixture.fake.refreshCalls)
	}
	state := fixture.p.Get(fixture.uid).View().DailyActive
	m, _ := state.(map[string]any)
	if m == nil || m["ok"] != true || testInt(m["points"]) != 300 {
		t.Fatalf("daily_active view = %+v", state)
	}
}

func TestDailyActiveTriggersAndPersists(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })

	fixture := newPointsFixture(t)
	clearDailyGrantBills(fixture.fake)

	summary, err := fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("DailyActive: %v", err)
	}
	if !summary.OK || summary.Granted != 1 || summary.Points != 300 {
		t.Fatalf("summary = %+v, want ok/granted/300", summary)
	}
	if len(summary.Results) != 1 {
		t.Fatalf("results = %+v", summary.Results)
	}
	result := summary.Results[0]
	if result.Chat || result.Refreshed || result.Model != "" {
		t.Fatalf("result = %+v, want web trigger without refresh/chat/model", result)
	}
	if fixture.fake.chatCalls != 0 {
		t.Fatalf("chat calls = %d, want 0", fixture.fake.chatCalls)
	}
	if fixture.fake.settingCalls != 1 {
		t.Fatalf("setting calls = %d, want 1", fixture.fake.settingCalls)
	}
	if fixture.fake.refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0", fixture.fake.refreshCalls)
	}

	states, err := accountstate.LoadStore(fixture.p.statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state := dailyActiveFromState(states[fixture.uid])
	if state["ok"] != true || state["granted"] != true ||
		testInt(state["points"]) != 300 || testInt(state["attempts"]) != 1 ||
		state["refreshed"] != false {
		t.Fatalf("persisted daily_active = %+v", state)
	}
}

func TestDailyActiveTriggerUsesWebSession(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })

	fixture := newPointsFixture(t)
	clearDailyGrantBills(fixture.fake)
	account := fixture.account(t)
	account.mu.Lock()
	token := account.Auth.AccessToken
	refresh := account.Auth.RefreshToken
	account.mu.Unlock()

	if _, err := fixture.p.DailyActive(context.Background(), fixture.uid, true); err != nil {
		t.Fatalf("DailyActive: %v", err)
	}

	fixture.fake.mu.Lock()
	headers := fixture.fake.lastSettingHeaders.Clone()
	fixture.fake.mu.Unlock()
	if headers.Get("X-Client-Platform") != "web" {
		t.Fatalf("platform = %q, want web", headers.Get("X-Client-Platform"))
	}
	if headers.Get("Referer") != dailyActiveWorkHome || headers.Get("Origin") != dailyActiveOrigin {
		t.Fatalf("web origin headers = %q/%q", headers.Get("Referer"), headers.Get("Origin"))
	}
	if headers.Get("Authorization") != "Bearer "+token {
		t.Fatal("setting request did not carry the account bearer token")
	}
	cookie := headers.Get("Cookie")
	for _, wanted := range []string{
		"xhx_token=" + token,
		"raccoon_refresh_token=" + refresh,
		"user_id=" + fixture.uid,
	} {
		if !strings.Contains(cookie, wanted) {
			t.Fatalf("setting cookie missing %q", wanted[:strings.Index(wanted, "=")])
		}
	}
}

func TestDailyActiveRefreshesRejectedWebSession(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })

	fixture := newPointsFixture(t)
	clearDailyGrantBills(fixture.fake)
	fixture.fake.update(func(f *pointsFake) { f.settingRejectFirst = true })

	summary, err := fixture.p.DailyActive(context.Background(), fixture.uid, true)
	if err != nil {
		t.Fatalf("DailyActive: %v", err)
	}
	if !summary.OK || summary.Granted != 1 || summary.Points != 300 {
		t.Fatalf("summary = %+v, want ok/granted/300 after token refresh", summary)
	}
	result := summary.Results[0]
	if !result.Refreshed || result.Chat || result.Granted != true {
		t.Fatalf("result = %+v, want refreshed web trigger", result)
	}
	if fixture.fake.settingCalls != 2 || fixture.fake.refreshCalls != 1 {
		t.Fatalf("setting/refresh calls = %d/%d, want 2/1",
			fixture.fake.settingCalls, fixture.fake.refreshCalls)
	}
}

func TestDailyActiveTriggerWithoutGrant(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })

	fixture := newPointsFixture(t)
	clearDailyGrantBills(fixture.fake)
	fixture.fake.update(func(f *pointsFake) { f.dailyGrantOnSetting = false })

	summary, err := fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("DailyActive: %v", err)
	}
	if !summary.OK || summary.Granted != 0 || summary.Points != 0 {
		t.Fatalf("summary = %+v, want trigger but no grant", summary)
	}
	result := summary.Results[0]
	if !result.OK || result.Granted || result.Refreshed || result.Chat {
		t.Fatalf("result = %+v, want trigger without grant", result)
	}
	if fixture.fake.chatCalls != 0 || fixture.fake.settingCalls != 1 || fixture.fake.refreshCalls != 0 {
		t.Fatalf("chat/setting/refresh calls = %d/%d/%d, want 0/1/0",
			fixture.fake.chatCalls, fixture.fake.settingCalls, fixture.fake.refreshCalls)
	}

	states, err := accountstate.LoadStore(fixture.p.statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state := dailyActiveFromState(states[fixture.uid])
	if state["ok"] != true || state["granted"] != false ||
		testInt(state["points"]) != 0 || state["refreshed"] != false {
		t.Fatalf("persisted daily_active = %+v", state)
	}
}

func TestDailyActiveTriggerFailure(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })

	fixture := newPointsFixture(t)
	clearDailyGrantBills(fixture.fake)
	fixture.fake.update(func(f *pointsFake) { f.settingStatus = http.StatusInternalServerError })

	summary, err := fixture.p.DailyActive(context.Background(), fixture.uid, true)
	if err != nil {
		t.Fatalf("DailyActive: %v", err)
	}
	if summary.OK || summary.Granted != 0 || summary.Points != 0 {
		t.Fatalf("summary = %+v, want trigger failure", summary)
	}
	if len(summary.Results) != 1 {
		t.Fatalf("results = %+v", summary.Results)
	}
	result := summary.Results[0]
	if result.OK || result.Granted || result.Refreshed || result.Chat {
		t.Fatalf("result = %+v, want failed trigger without chat", result)
	}
	if !strings.Contains(result.Message, "触发每日活跃失败") {
		t.Fatalf("message = %q, want trigger failure", result.Message)
	}
	if fixture.fake.chatCalls != 0 || fixture.fake.settingCalls != 1 || fixture.fake.refreshCalls != 0 {
		t.Fatalf("chat/setting/refresh calls = %d/%d/%d, want 0/1/0",
			fixture.fake.chatCalls, fixture.fake.settingCalls, fixture.fake.refreshCalls)
	}

	states, err := accountstate.LoadStore(fixture.p.statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state := dailyActiveFromState(states[fixture.uid])
	if state["ok"] != false || state["granted"] != false ||
		state["refreshed"] != false || testInt(state["attempts"]) != 1 {
		t.Fatalf("persisted daily_active = %+v", state)
	}
}

func TestDailyActiveSkipsDisabledOrTasksOff(t *testing.T) {
	fixture := newPointsFixture(t)
	account := fixture.p.Get(fixture.uid)
	if err := fixture.p.SetTasksEnabled(fixture.uid, false); err != nil {
		t.Fatalf("SetTasksEnabled: %v", err)
	}
	summary, err := fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("DailyActive tasks-off: %v", err)
	}
	if !summary.Results[0].Skipped || summary.Checked != 0 {
		t.Fatalf("tasks-off summary = %+v", summary)
	}
	if fixture.fake.chatCalls != 0 {
		t.Fatalf("chat calls = %d, want 0", fixture.fake.chatCalls)
	}
	if err := fixture.p.Enable(fixture.uid); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := fixture.p.Disable(fixture.uid, "test"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	summary, err = fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("DailyActive disabled: %v", err)
	}
	if !summary.Results[0].Skipped {
		t.Fatalf("disabled summary = %+v", summary)
	}
	if account == nil || account.View().DailyActive == nil {
		t.Fatal("account view missing daily_active")
	}
}

func TestDailyActiveSlot(t *testing.T) {
	loc := time.Local
	cases := []struct {
		hour, minute int
		random       int
		wantHour     int
		wantMinute   int
		ok           bool
	}{
		{0, 0, 30, 0, 30, false},
		{0, 30, 30, 0, 30, true},
		{3, 59, 239, 3, 59, true},
		{4, 0, 30, 4, 30, false},
		{8, 30, 30, 8, 30, true},
		{12, 0, 0, 12, 0, true},
		{20, 0, 239, 23, 59, false},
		{23, 59, 239, 23, 59, true},
	}
	for _, tc := range cases {
		now := time.Date(2026, 11, 11, tc.hour, tc.minute, 0, 0, loc)
		slot, ok := dailyActiveSlot(now, tc.random)
		want := time.Date(2026, 11, 11, tc.wantHour, tc.wantMinute, 0, 0, loc)
		if ok != tc.ok || !slot.Equal(want) {
			t.Fatalf("%02d:%02d random=%d: slot=%v ok=%v, want %v/%v",
				tc.hour, tc.minute, tc.random, slot, ok, want, tc.ok)
		}
	}
}

func TestDailyActiveJobRunsOncePerSlot(t *testing.T) {
	oldRandom := dailyActiveRandomMinute
	dailyActiveRandomMinute = func() int { return 30 }
	t.Cleanup(func() { dailyActiveRandomMinute = oldRandom })

	fixture := newRoutesFixture(t)
	fixture.importFlat(t, fixture.uid, fixture.token, nil)
	if _, err := SaveConfig(fixture.store, map[string]any{
		"enabled": true, "daily_active": true,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.now = time.Date(fixture.now.Year(), fixture.now.Month(), fixture.now.Day(), 8, 0, 0, 0, fixture.now.Location())
	job := &dailyActiveJob{provider: fixture.p}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if points, _ := fixture.p.pool.Get(fixture.uid).View().DailyActive.(map[string]any); len(points) != 0 {
		t.Fatalf("daily_active before random slot = %#v", points)
	}

	fixture.now = fixture.now.Add(29 * time.Minute) // 08:29
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if points, _ := fixture.p.pool.Get(fixture.uid).View().DailyActive.(map[string]any); len(points) != 0 {
		t.Fatalf("daily_active before 08:30 = %#v", points)
	}

	fixture.now = fixture.now.Add(time.Minute) // 08:30
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := fixture.p.pool.Get(fixture.uid).View().DailyActive
	stateMap, _ := state.(map[string]any)
	if stateMap["ok"] != true || testInt(stateMap["points"]) != 300 {
		t.Fatalf("daily_active at random slot = %#v", state)
	}

	// A second tick in the same four-hour window must be a no-op.
	fixture.now = fixture.now.Add(time.Minute)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fixture.p.pool.dailyActiveState(fixture.p.pool.Get(fixture.uid)); testInt(got["attempts"]) != 0 {
		t.Fatalf("second tick in window repeated the attempt: %#v", got)
	}
}

func TestDailyActiveJobRetriesNextFourHourWindow(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })
	oldRandom := dailyActiveRandomMinute
	dailyActiveRandomMinute = func() int { return 30 }
	t.Cleanup(func() { dailyActiveRandomMinute = oldRandom })

	fixture := newRoutesFixture(t)
	fixture.importFlat(t, fixture.uid, fixture.token, nil)
	if _, err := SaveConfig(fixture.store, map[string]any{
		"enabled": true, "daily_active": true,
	}); err != nil {
		t.Fatal(err)
	}
	clearDailyGrantBills(fixture.fake.points)
	fixture.fake.points.update(func(f *pointsFake) { f.dailyGrantOnSetting = false })
	fixture.now = time.Date(fixture.now.Year(), fixture.now.Month(), fixture.now.Day(), 8, 0, 0, 0, fixture.now.Location())
	job := &dailyActiveJob{provider: fixture.p}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(30 * time.Minute)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := fixture.p.pool.dailyActiveState(fixture.p.pool.Get(fixture.uid))
	if state["granted"] != false || testInt(state["attempts"]) != 1 {
		t.Fatalf("first failed window state = %#v", state)
	}
	if fixture.fake.points.settingCalls != 1 {
		t.Fatalf("setting calls after first window = %d, want 1", fixture.fake.points.settingCalls)
	}

	// Same window after failure must not retry on the next scheduler tick.
	fixture.now = fixture.now.Add(time.Minute)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.points.settingCalls != 1 {
		t.Fatalf("same-window retry calls = %d, want 1", fixture.fake.points.settingCalls)
	}

	// The next four-hour window retries and succeeds.
	fixture.fake.points.update(func(f *pointsFake) { f.dailyGrantOnSetting = true })
	fixture.now = time.Date(fixture.now.Year(), fixture.now.Month(), fixture.now.Day(), 12, 0, 0, 0, fixture.now.Location())
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(30 * time.Minute)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state = fixture.p.pool.dailyActiveState(fixture.p.pool.Get(fixture.uid))
	if state["granted"] != true || testInt(state["points"]) != 300 || testInt(state["attempts"]) != 2 {
		t.Fatalf("second window state = %#v", state)
	}
	if fixture.fake.points.settingCalls != 2 {
		t.Fatalf("setting calls after second window = %d, want 2", fixture.fake.points.settingCalls)
	}

	// Once today's grant is present, later windows must not call setting_info.
	fixture.now = time.Date(fixture.now.Year(), fixture.now.Month(), fixture.now.Day(), 16, 0, 0, 0, fixture.now.Location())
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(30 * time.Minute)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.points.settingCalls != 2 {
		t.Fatalf("post-grant setting calls = %d, want 2", fixture.fake.points.settingCalls)
	}
}

func TestDailyActiveJobFixedTime(t *testing.T) {
	oldRandom := dailyActiveRandomMinute
	dailyActiveRandomMinute = func() int { return 30 }
	t.Cleanup(func() { dailyActiveRandomMinute = oldRandom })

	fixture := newRoutesFixture(t)
	fixture.importFlat(t, fixture.uid, fixture.token, nil)
	if _, err := SaveConfig(fixture.store, map[string]any{
		"enabled": true, "daily_active": true, "daily_active_time": "09:15",
	}); err != nil {
		t.Fatal(err)
	}
	job := &dailyActiveJob{provider: fixture.p}
	base := fixture.now
	day := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, base.Location())

	fixture.now = day.Add(9 * time.Hour)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if points, _ := fixture.p.pool.Get(fixture.uid).View().DailyActive.(map[string]any); len(points) != 0 {
		t.Fatalf("daily_active before 09:15 = %#v", points)
	}

	fixture.now = day.Add(9*time.Hour + 15*time.Minute)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := fixture.p.pool.Get(fixture.uid).View().DailyActive
	stateMap, _ := state.(map[string]any)
	if stateMap["ok"] != true || testInt(stateMap["points"]) != 300 {
		t.Fatalf("daily_active at fixed 09:15 = %#v", state)
	}

	fixture.now = day.Add(9*time.Hour + 16*time.Minute)
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fixture.p.pool.dailyActiveState(fixture.p.pool.Get(fixture.uid)); testInt(got["attempts"]) != 0 {
		t.Fatalf("second tick in the fixed window repeated the attempt: %#v", got)
	}
}
