package raccoon

import (
	"context"
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
		f.dailyGrantOnChat = true
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
	state := fixture.p.Get(fixture.uid).View().DailyActive
	m, _ := state.(map[string]any)
	if m == nil || m["ok"] != true || testInt(m["points"]) != 300 {
		t.Fatalf("daily_active view = %+v", state)
	}
}

func TestDailyActivePingsAndPersists(t *testing.T) {
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
	if len(summary.Results) != 1 || !summary.Results[0].Chat {
		t.Fatalf("result = %+v, want chat", summary.Results)
	}
	if fixture.fake.chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1", fixture.fake.chatCalls)
	}
	if summary.Results[0].Model == "" {
		t.Fatal("model was not recorded")
	}

	states, err := accountstate.LoadStore(fixture.p.statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state := dailyActiveFromState(states[fixture.uid])
	if state["ok"] != true || testInt(state["points"]) != 300 || testInt(state["attempts"]) != 1 {
		t.Fatalf("persisted daily_active = %+v", state)
	}
}

func TestDailyActiveThrottleAndForce(t *testing.T) {
	oldDelay := dailyActivePollDelay
	dailyActivePollDelay = time.Millisecond
	t.Cleanup(func() { dailyActivePollDelay = oldDelay })

	fixture := newPointsFixture(t)
	clearDailyGrantBills(fixture.fake)
	fixture.fake.update(func(f *pointsFake) { f.dailyGrantOnChat = false })

	first, err := fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("first DailyActive: %v", err)
	}
	if first.OK || first.Results[0].Skipped || !first.Results[0].Chat {
		t.Fatalf("first = %+v, want a failed chat attempt", first)
	}
	second, err := fixture.p.DailyActive(context.Background(), fixture.uid, false)
	if err != nil {
		t.Fatalf("second DailyActive: %v", err)
	}
	if !second.Results[0].Skipped {
		t.Fatalf("second = %+v, want throttle skip", second)
	}
	if fixture.fake.chatCalls != 1 {
		t.Fatalf("chat calls after throttle = %d, want 1", fixture.fake.chatCalls)
	}
	forced, err := fixture.p.DailyActive(context.Background(), fixture.uid, true)
	if err != nil {
		t.Fatalf("forced DailyActive: %v", err)
	}
	if forced.Results[0].Skipped || !forced.Results[0].Chat {
		t.Fatalf("forced = %+v, want forced chat", forced)
	}
	if fixture.fake.chatCalls != 2 {
		t.Fatalf("chat calls after force = %d, want 2", fixture.fake.chatCalls)
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
