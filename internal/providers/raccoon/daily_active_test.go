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

func TestDailyActiveThrottleAndForce(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })

	fixture := newPointsFixture(t)
	clearDailyGrantBills(fixture.fake)
	fixture.fake.update(func(f *pointsFake) { f.dailyGrantOnSetting = false })

	first, err := fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("first DailyActive: %v", err)
	}
	if !first.OK || first.Results[0].Skipped || first.Results[0].Refreshed {
		t.Fatalf("first = %+v, want a web trigger attempt", first)
	}
	second, err := fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("second DailyActive: %v", err)
	}
	if !second.Results[0].Skipped {
		t.Fatalf("second = %+v, want throttle skip", second)
	}
	if fixture.fake.settingCalls != 1 {
		t.Fatalf("setting calls after throttle = %d, want 1", fixture.fake.settingCalls)
	}
	forced, err := fixture.p.DailyActive(context.Background(), fixture.uid, true)
	if err != nil {
		t.Fatalf("forced DailyActive: %v", err)
	}
	if forced.Results[0].Skipped || forced.Results[0].Refreshed {
		t.Fatalf("forced = %+v, want forced web trigger", forced)
	}
	if fixture.fake.settingCalls != 2 {
		t.Fatalf("setting calls after force = %d, want 2", fixture.fake.settingCalls)
	}
	if fixture.fake.chatCalls != 0 || fixture.fake.refreshCalls != 0 {
		t.Fatalf("chat/refresh calls = %d/%d, want 0/0", fixture.fake.chatCalls, fixture.fake.refreshCalls)
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
		hour int
		want int
		ok   bool
	}{
		{8, 0, false},
		{9, 9, true},
		{15, 9, true},
		{21, 21, true},
		{23, 21, true},
	}
	for _, tc := range cases {
		slot, ok := dailyActiveSlot(time.Date(2026, 11, 11, tc.hour, 30, 0, 0, loc))
		if ok != tc.ok || (ok && slot.Hour() != tc.want) {
			t.Fatalf("hour %d: slot=%v ok=%v", tc.hour, slot, ok)
		}
	}
}

func TestDailyActiveJobRunsOncePerSlot(t *testing.T) {
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
		t.Fatalf("daily_active before 09:00 = %#v", points)
	}

	fixture.now = fixture.now.Add(2 * time.Hour) // 10:00
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := fixture.p.pool.Get(fixture.uid).View().DailyActive
	stateMap, _ := state.(map[string]any)
	if stateMap["ok"] != true || testInt(stateMap["points"]) != 300 {
		t.Fatalf("daily_active after slot = %#v", state)
	}
}
