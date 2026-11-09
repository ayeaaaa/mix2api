package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

type jobsFixture struct {
	intl   *Provider
	cn     *Provider
	store  *config.Store
	dir    string
	fake   *taskTestUpstream
	now    time.Time
	nowPtr *time.Time
}

func newJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	intl, err := New(realmIntl, store)
	if err != nil {
		t.Fatalf("New(intl): %v", err)
	}
	cn, err := New(realmCN, store)
	if err != nil {
		t.Fatalf("New(cn): %v", err)
	}
	intl.SetTwin(cn)
	cn.SetTwin(intl)

	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"today_checked_in": false,
				"streak_days":      1,
			}}
		case creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"IsPaidUser": false,
				"Packages":   []any{},
			}}
		case "/v2/billing/meter/daily-checkin":
			return http.StatusOK, map[string]any{"code": 0, "msg": "签到成功", "data": map[string]any{}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	for _, provider := range []*Provider{intl, cn} {
		provider.tasksChatBaseURL = server.URL
		provider.tasksBillingBaseURL = server.URL
		provider.tasksWebBaseURL = server.URL
		provider.creditsBillingBaseURL = server.URL
		provider.creditsCheckinBaseURL = server.URL
		provider.tasksGap = -1
		provider.tasksBackoff = -1
		provider.tasksBudget = time.Second
		provider.tasksTimeout = time.Second
		provider.tasksTurnWait = 10 * time.Millisecond
		provider.tasksPollInterval = time.Millisecond
		provider.tasksSleep = func(time.Duration) {}
	}
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local)
	fixture := &jobsFixture{intl: intl, cn: cn, store: store, dir: dir, fake: fake, now: now}
	fixture.nowPtr = &fixture.now
	for _, provider := range []*Provider{intl, cn} {
		provider.jobsNow = func() time.Time { return fixture.now }
	}
	return fixture
}

func (f *jobsFixture) setNow(value time.Time) {
	f.now = value
}

func (f *jobsFixture) addTaskAccount(t *testing.T, uid string, enabled, tasks bool, restricted bool) *Account {
	t.Helper()
	auth := Auth{
		UID:             uid,
		Nickname:        "Nick " + uid,
		Realm:           realmCN,
		Domain:          "www.workbuddy.cn",
		AccessToken:     wbRouteToken(t, uid, realmCN),
		RefreshToken:    "refresh-" + uid,
		Platform:        "CLI",
		Source:          sourceOAuth,
		Enabled:         enabled,
		EnabledSet:      true,
		TasksEnabled:    tasks,
		TasksEnabledSet: true,
	}
	if restricted {
		auth.Extra = map[string]json.RawMessage{
			"restrictedUntil": json.RawMessage("4102444800"),
		}
	}
	account, err := f.cn.Pool().Add(auth)
	if err != nil {
		t.Fatalf("add task account %s: %v", uid, err)
	}
	return account
}

func (f *jobsFixture) addIntlTaskAccount(t *testing.T, uid string, enabled, tasks bool) *Account {
	t.Helper()
	auth := Auth{
		UID: uid, Nickname: "Nick " + uid, Realm: realmIntl, Domain: "www.workbuddy.ai",
		AccessToken: wbRouteToken(t, uid, realmIntl), RefreshToken: "refresh-" + uid,
		Platform: "CLI", Source: sourceOAuth, Enabled: enabled, EnabledSet: true,
		TasksEnabled: tasks, TasksEnabledSet: true,
	}
	account, err := f.intl.Pool().Add(auth)
	if err != nil {
		t.Fatalf("add intl task account %s: %v", uid, err)
	}
	return account
}

func jobsByName(t *testing.T, provider *Provider) map[string]*scheduledJob {
	t.Helper()
	out := map[string]*scheduledJob{}
	for _, item := range provider.Jobs() {
		job, ok := item.(*scheduledJob)
		if !ok {
			t.Fatalf("job %T does not implement the workbuddy schedule helper", item)
		}
		if _, exists := out[job.Name()]; exists {
			t.Fatalf("duplicate job name %q", job.Name())
		}
		out[job.Name()] = job
	}
	return out
}

func TestJobsRegistrationTwinOwnershipAndNames(t *testing.T) {
	f := newJobsFixture(t)
	names := []string{
		"workbuddy-checkin",
		"workbuddy-daily-activity",
		"workbuddy-report-activity",
		"workbuddy-growth",
		"workbuddy-cat-travel",
		"workbuddy-streak-bonus",
		"workbuddy-keepalive",
		"workbuddy-catalog-refresh",
	}
	jobs := f.intl.Jobs()
	if len(jobs) != len(names) {
		t.Fatalf("jobs = %d, want %d: %#v", len(jobs), len(names), jobs)
	}
	seen := map[string]bool{}
	for _, item := range jobs {
		var _ scheduler.Job = item
		if seen[item.Name()] {
			t.Fatalf("duplicate job %q", item.Name())
		}
		seen[item.Name()] = true
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("missing job %q in %v", name, seen)
		}
	}
	if jobs := f.cn.Jobs(); len(jobs) != 0 {
		t.Fatalf("cn jobs = %d, want none (scheduler owns intl)", len(jobs))
	}
}

func TestJobsDueCatchUpAndNextRun(t *testing.T) {
	f := newJobsFixture(t)
	f.addTaskAccount(t, "cn-due", true, true, false)
	job := jobsByName(t, f.intl)["workbuddy-checkin"]

	f.setNow(time.Date(2026, 11, 9, 8, 59, 0, 0, time.Local))
	if job.Due(f.now) {
		t.Fatal("checkin due before its first slot")
	}
	f.setNow(time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))
	if !job.Due(f.now) {
		t.Fatal("checkin not due at 09:00")
	}
	f.setNow(time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local))
	if !job.Due(f.now) {
		t.Fatal("checkin did not catch up once after the 09:00 slot")
	}
	if next := job.NextRun(f.now); next.Hour() != 21 || next.Day() != 9 {
		t.Fatalf("next run = %s, want 21:00 same day", next)
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("checkin job: %v", err)
	}
	if f.fake.count(http.MethodPost, "/v2/billing/meter/daily-checkin") != 1 {
		t.Fatalf("checkin posts = %d, want 1", f.fake.count(http.MethodPost, "/v2/billing/meter/daily-checkin"))
	}
	if job.Due(f.now) {
		t.Fatal("checkin still due immediately after the catch-up run")
	}
	f.setNow(time.Date(2026, 11, 9, 21, 0, 0, 0, time.Local))
	if !job.Due(f.now) {
		t.Fatal("second checkin slot is not due")
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("second checkin slot: %v", err)
	}
	if f.fake.count(http.MethodPost, "/v2/billing/meter/daily-checkin") != 1 {
		t.Fatal("second slot repeated an already-completed check-in")
	}
	if job.Due(f.now) {
		t.Fatal("checkin still due after the second slot completed")
	}
	f.setNow(time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local))
	if !job.Due(f.now) {
		t.Fatal("checkin is not due on the next day")
	}
}

func TestJobsConfigurableScheduleHours(t *testing.T) {
	f := newJobsFixture(t)
	if _, err := f.store.Merge(map[string]any{
		"schedule": map[string]any{"checkin": []any{10, 22}},
	}); err != nil {
		t.Fatalf("save schedule: %v", err)
	}
	job := jobsByName(t, f.intl)["workbuddy-checkin"]
	f.setNow(time.Date(2026, 11, 9, 9, 30, 0, 0, time.Local))
	if job.Due(f.now) {
		t.Fatal("checkin ignored the configured 10/22 schedule")
	}
	f.setNow(time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local))
	if !job.Due(f.now) {
		t.Fatal("checkin did not fire at configured 10:00")
	}
}

func TestJobsSkipDisabledTaskOffAndRestrictedAccounts(t *testing.T) {
	f := newJobsFixture(t)
	f.addTaskAccount(t, "cn-active", true, true, false)
	f.addTaskAccount(t, "cn-disabled", false, true, false)
	f.addTaskAccount(t, "cn-task-off", true, false, false)
	f.addTaskAccount(t, "cn-restricted", true, true, true)
	job := jobsByName(t, f.intl)["workbuddy-checkin"]
	report, err := job.RunForce(context.Background())
	if err != nil {
		t.Fatalf("RunForce: %v", err)
	}
	if report.Checked != 4 || report.Succeeded != 1 || report.Skipped != 3 {
		t.Fatalf("report = %+v, want four checked, only cn-active succeeded", report)
	}
	for _, uid := range []string{"cn-disabled", "cn-task-off", "cn-restricted"} {
		if f.fake.count(http.MethodPost, "/v2/billing/meter/daily-checkin") != 1 {
			t.Fatalf("ineligible accounts were not skipped before %s", uid)
		}
	}
}

func TestJobsIdempotentAndManualForce(t *testing.T) {
	f := newJobsFixture(t)
	f.addTaskAccount(t, "cn-idem", true, true, false)
	job := jobsByName(t, f.intl)["workbuddy-checkin"]
	f.setNow(time.Date(2026, 11, 9, 9, 5, 0, 0, time.Local))
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("duplicate Run: %v", err)
	}
	if got := f.fake.count(http.MethodPost, "/v2/billing/meter/daily-checkin"); got != 1 {
		t.Fatalf("checkin posts after duplicate Run = %d, want 1", got)
	}
	report, err := job.RunForce(context.Background())
	if err != nil {
		t.Fatalf("RunForce: %v", err)
	}
	if report.Succeeded != 0 || report.Skipped == 0 {
		t.Fatalf("force report = %+v, want account-level skip", report)
	}
	if got := f.fake.count(http.MethodPost, "/v2/billing/meter/daily-checkin"); got != 1 {
		t.Fatalf("manual force repeated a completed checkin: %d posts", got)
	}
}

func TestJobsPerAccountIsolation(t *testing.T) {
	f := newJobsFixture(t)
	f.addTaskAccount(t, "cn-a", true, true, false)
	f.addTaskAccount(t, "cn-b", true, true, false)
	var checkinPosts int
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"today_checked_in": false}}
		case "/v2/billing/meter/daily-checkin":
			checkinPosts++
			if checkinPosts == 1 {
				return http.StatusInternalServerError, map[string]any{"code": 500, "msg": "boom"}
			}
			return http.StatusOK, map[string]any{"code": 0, "msg": "ok"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	}
	job := jobsByName(t, f.intl)["workbuddy-checkin"]
	report, err := job.RunForce(context.Background())
	if err == nil {
		t.Fatalf("job error = nil, want per-account failure reported")
	}
	if report.Checked != 2 || report.Succeeded != 1 || report.Failed != 1 || len(report.Errors) != 1 {
		t.Fatalf("report = %+v, want one success and one failure", report)
	}
}

func TestJobsKeepaliveCatalogNoCrashAndCtxCancel(t *testing.T) {
	f := newJobsFixture(t)
	jobs := jobsByName(t, f.intl)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range []string{"workbuddy-keepalive", "workbuddy-catalog-refresh", "workbuddy-daily-activity", "workbuddy-growth", "workbuddy-cat-travel", "workbuddy-streak-bonus"} {
		result, err := jobs[name].RunForce(ctx)
		if err == nil {
			t.Fatalf("%s ignored a canceled context", name)
		}
		if result.Name != name {
			t.Fatalf("%s report name = %q", name, result.Name)
		}
	}
}

func TestJobsDailyActivityDisabledMasterSwitch(t *testing.T) {
	f := newJobsFixture(t)
	f.addTaskAccount(t, "cn-not-intl", true, true, false)
	if _, err := f.store.Merge(map[string]any{"daily_activity": false}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	job := jobsByName(t, f.intl)["workbuddy-daily-activity"]
	report, err := job.RunForce(context.Background())
	if err != nil {
		t.Fatalf("daily job: %v", err)
	}
	if report.Ran || report.Checked != 0 {
		t.Fatalf("daily job ignored the master switch: %+v", report)
	}
}

func TestJobsReportActivityCN(t *testing.T) {
	f := newJobsFixture(t)
	f.addTaskAccount(t, "cn-report", true, true, false)
	f.addTaskAccount(t, "cn-report-restricted", true, true, true)
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case "/v2/report":
			return http.StatusOK, map[string]any{"code": 0, "msg": "ok"}
		case tasksGrowthStreakPath, tasksGrowthV2StreakPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"streak": map[string]any{"days": 3},
			}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	}
	job := jobsByName(t, f.intl)["workbuddy-report-activity"]
	report, err := job.RunForce(context.Background())
	if err != nil {
		t.Fatalf("report job: %v", err)
	}
	if report.Name != "workbuddy-report-activity" || report.Succeeded != 1 || report.Checked != 2 || report.Skipped != 1 {
		t.Fatalf("report job result = %+v", report)
	}
	if got := f.fake.count(http.MethodPost, "/v2/report"); got != 1 {
		t.Fatalf("activity report POSTs = %d, want 1", got)
	}
}

func TestJobsDailyBusyAndRestrictedSkip(t *testing.T) {
	f := newJobsFixture(t)
	busyAccount := f.addIntlTaskAccount(t, "intl-daily-busy", true, true)
	busyAccount.taskMu.Lock()
	busyReport, err := jobsByName(t, f.intl)["workbuddy-daily-activity"].RunForce(context.Background())
	busyAccount.taskMu.Unlock()
	if err != nil {
		t.Fatalf("busy daily job: %v", err)
	}
	if busyReport.Skipped != 1 || busyReport.Failed != 0 || busyReport.Succeeded != 0 {
		t.Fatalf("busy daily accounting = %+v, want skipped", busyReport)
	}

	f2 := newJobsFixture(t)
	f2.addIntlTaskAccount(t, "intl-daily-restricted", true, true)
	f2.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == chatPath {
			return http.StatusForbidden, map[string]any{"code": 11140, "msg": "账号存在异常行为，已被限制使用"}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	}
	report, err := jobsByName(t, f2.intl)["workbuddy-daily-activity"].RunForce(context.Background())
	if err != nil {
		t.Fatalf("restricted daily job: %v", err)
	}
	if report.Skipped != 1 || report.Failed != 0 || report.Succeeded != 0 {
		t.Fatalf("restricted daily accounting = %+v, want skipped", report)
	}
}

func TestJobsBusyReportCountsSkipped(t *testing.T) {
	f := newJobsFixture(t)
	account := f.addTaskAccount(t, "cn-busy-report", true, true, false)
	account.taskMu.Lock()
	report, err := jobsByName(t, f.intl)["workbuddy-report-activity"].RunForce(context.Background())
	account.taskMu.Unlock()
	if err != nil {
		t.Fatalf("busy report job: %v", err)
	}
	if report.Skipped != 1 || report.Failed != 0 || report.Succeeded != 0 {
		t.Fatalf("busy report accounting = %+v, want skipped", report)
	}
}

func TestJobsPanicContained(t *testing.T) {
	f := newJobsFixture(t)
	job := jobsByName(t, f.intl)["workbuddy-checkin"]
	f.intl.jobsNow = func() time.Time { panic("job boom") }
	report, err := job.RunForce(context.Background())
	if err == nil {
		t.Fatal("panicking job returned nil error")
	}
	if report.Name != "workbuddy-checkin" {
		t.Fatalf("panic report = %+v", report)
	}
}

func TestJobsRunLockAndSchedulerPayload(t *testing.T) {
	f := newJobsFixture(t)
	f.intl.schedulerRunMu.Lock()
	_, err := f.intl.RunJobs(context.Background(), "workbuddy-checkin", true)
	f.intl.schedulerRunMu.Unlock()
	if !errors.Is(err, errSchedulerBusy) {
		t.Fatalf("RunJobs during a cycle err = %v, want busy", err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "model_windows.json"), []byte(`{"windows":[{"uid":"uid-window","model":"m1","reset_at":4102444800,"attempts":2,"last_result":"ok","cap":true,"cap_at":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "quota_scan.json"), []byte(`{"scans":[{"uid":"uid-scan","model":"m2","next_at":4102444800,"last_result":"ok","attempts":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	activityRow, _ := json.Marshal(ActivityEvent{
		TS: f.now.Format(time.RFC3339), UID: "uid-daily", Realm: realmIntl,
		Task: taskDailyChatTask, Trigger: taskTriggerScheduler, OK: true, Message: "ok",
	})
	if err := os.WriteFile(filepath.Join(f.dir, "activity_history.jsonl"), append(activityRow, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	status := f.intl.SchedulerStatus()
	for _, key := range []string{
		"enabled", "mode", "mode_cn", "mode_intl", "last_run_time",
		"next_run_time", "windows", "quota_scans", "daily_chat", "logs", "jobs",
	} {
		if _, exists := status[key]; !exists {
			t.Fatalf("scheduler payload missing %q: %#v", key, status)
		}
	}
	if !strings.Contains(wbRouteText(status["mode"]), "签到") || !strings.Contains(wbRouteText(status["mode_cn"]), "Token保活") {
		t.Fatalf("scheduler modes = %#v", status)
	}
	if windows, ok := status["windows"].([]map[string]any); !ok || len(windows) != 1 || wbRouteText(windows[0]["uid"]) != "uid-window" {
		t.Fatalf("scheduler windows = %#v", status["windows"])
	}
	if scans, ok := status["quota_scans"].([]map[string]any); !ok || len(scans) != 1 || wbRouteText(scans[0]["uid"]) != "uid-scan" {
		t.Fatalf("scheduler quota scans = %#v", status["quota_scans"])
	}
	daily := status["daily_chat"].(map[string]any)
	for _, key := range []string{"times", "random", "done_slots", "next_slot", "live", "budget_s", "abandoned"} {
		if _, exists := daily[key]; !exists {
			t.Fatalf("daily_chat missing %q: %#v", key, daily)
		}
	}
	if done := daily["done_slots"].([]string); len(done) != 1 || done[0] != "09:00" {
		t.Fatalf("daily_chat done_slots = %#v", daily["done_slots"])
	}
	if logs := status["logs"].([]string); len(logs) != 1 || !strings.Contains(logs[0], "daily_chat") {
		t.Fatalf("scheduler logs = %#v", logs)
	}
	f.setNow(time.Date(2026, 11, 9, 9, 1, 0, 0, time.Local))
	if _, err := f.intl.RunJobs(context.Background(), "workbuddy-checkin", true); err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if status := f.intl.SchedulerStatus(); status["last_run_time"] == "尚未运行" {
		t.Fatalf("scheduler last_run_time not updated: %#v", status)
	}
}

func TestJobsStatusAndToggle(t *testing.T) {
	f := newJobsFixture(t)
	status := f.intl.SchedulerStatus()
	if status["enabled"] != true {
		t.Fatalf("scheduler status = %#v", status)
	}
	if jobs, ok := status["jobs"].([]map[string]any); !ok || len(jobs) == 0 {
		t.Fatalf("scheduler status has no jobs: %#v", status)
	}
	f.intl.ToggleScheduler()
	if status := f.intl.SchedulerStatus(); status["enabled"] != false {
		t.Fatalf("scheduler did not toggle off: %#v", status)
	}
}
