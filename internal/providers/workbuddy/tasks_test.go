package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// taskTestCall is one request observed by the fake upstream.
type taskTestCall struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// taskTestUpstream is a small stateful fake for the WorkBuddy task endpoints.
type taskTestUpstream struct {
	mu      sync.Mutex
	calls   []taskTestCall
	handler func(*taskTestUpstream, *http.Request, []byte) (int, any)
}

func (f *taskTestUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw := make([]byte, 0)
	if r.Body != nil {
		raw, _ = io.ReadAll(r.Body)
	}
	body := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	call := taskTestCall{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
		Raw:    append([]byte(nil), raw...),
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	handler := f.handler
	f.mu.Unlock()

	status, payload := http.StatusOK, any(map[string]any{"code": 0, "msg": "ok"})
	if handler != nil {
		status, payload = handler(f, r, raw)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload != nil {
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func (f *taskTestUpstream) snapshot() []taskTestCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]taskTestCall, len(f.calls))
	for index, call := range f.calls {
		call.Header = call.Header.Clone()
		call.Raw = append([]byte(nil), call.Raw...)
		if call.Body != nil {
			copied := make(map[string]any, len(call.Body))
			for key, value := range call.Body {
				copied[key] = value
			}
			out[index] = call
			out[index].Body = copied
			continue
		}
		out[index] = call
	}
	return out
}

func (f *taskTestUpstream) count(method, path string) int {
	count := 0
	for _, call := range f.snapshot() {
		if call.Method == method && call.Path == path {
			count++
		}
	}
	return count
}

func newTaskTestUpstream(t *testing.T, handler func(*taskTestUpstream, *http.Request, []byte) (int, any)) (*taskTestUpstream, *httptest.Server) {
	t.Helper()
	fake := &taskTestUpstream{handler: handler}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func setupTaskProvider(t *testing.T, realm, dir, serverURL string) (*Provider, *Pool) {
	t.Helper()
	provider, pool := newPoolProvider(t, realm, dir)
	provider.tasksChatBaseURL = serverURL
	provider.tasksBillingBaseURL = serverURL
	provider.tasksWebBaseURL = serverURL
	provider.creditsBillingBaseURL = serverURL
	provider.creditsCheckinBaseURL = serverURL
	provider.tasksGap = -1
	provider.tasksBackoff = -1
	provider.tasksTurnWait = 25 * time.Millisecond
	provider.tasksPollInterval = time.Millisecond
	provider.tasksBudget = 5 * time.Second
	provider.tasksTimeout = 2 * time.Second
	provider.tasksSleep = func(time.Duration) {}
	return provider, pool
}

func writeTaskAccount(t *testing.T, dir, uid, realm string, fields map[string]any) {
	t.Helper()
	base := map[string]any{"enabled": true, "tasksEnabled": true}
	for key, value := range fields {
		base[key] = value
	}
	writePoolAccount(t, dir, uid, realm, base)
}

func taskJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func assertNoSecret(t *testing.T, label string, value any, secrets ...string) {
	t.Helper()
	text := taskJSON(t, value)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("%s leaked secret %q: %s", label, secret, text)
		}
	}
}

func taskBodyString(t *testing.T, call taskTestCall) string {
	t.Helper()
	return string(call.Raw)
}

func taskContainsPath(calls []taskTestCall, method, path string) bool {
	for _, call := range calls {
		if call.Method == method && call.Path == path {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Check-in
// ---------------------------------------------------------------------------

func TestTasksCheckinCNIdempotentAndPersists(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-checkin", realmCN, map[string]any{
		"accessToken": "secret-cn-access",
		"nickname":    "CN Tester",
	})
	var checkinCalls int
	fake, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		case "/v2/billing/meter/daily-checkin":
			checkinCalls++
			return http.StatusOK, map[string]any{
				"code": 0,
				"msg":  "签到成功",
				"data": map[string]any{
					"today_checked_in": true,
					"streak_days":      3,
					"daily_credit":     10,
					"today_credit":     10,
				},
			}
		case creditsSummaryPath:
			return http.StatusOK, map[string]any{
				"code": 0,
				"data": map[string]any{"IsPaidUser": true, "Packages": []any{}},
			}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)

	result, err := provider.Checkin(context.Background(), "cn-checkin")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !result.OK || result.State != "checked" || result.Skipped {
		t.Fatalf("checkin result = %+v", result)
	}
	if result.UID != "cn-checkin" || result.Nickname != "CN Tester" {
		t.Fatalf("identity = %q/%q", result.UID, result.Nickname)
	}
	if checkinCalls != 1 {
		t.Fatalf("checkin POSTs = %d, want 1", checkinCalls)
	}
	account := accountByUID(t, pool, "cn-checkin")
	account.mu.RLock()
	lastCheckin := account.Auth.LastCheckin
	credits := cloneCreditSnapshot(account.Credits)
	account.mu.RUnlock()
	if !strings.HasPrefix(lastCheckin, time.Now().Format("2006-01-02")) {
		t.Fatalf("lastCheckin = %q", lastCheckin)
	}
	if credits == nil || credits.Checkin == nil || !credits.Checkin.TodayCheckedIn {
		t.Fatalf("credits/checkin not persisted: %+v", credits)
	}
	if result.Activity == nil || result.Activity.Task != "checkin" || !result.Activity.OK {
		t.Fatalf("activity = %+v", result.Activity)
	}
	assertNoSecret(t, "checkin result", result, "secret-cn-access")

	second, err := provider.Checkin(context.Background(), "cn-checkin")
	if err != nil {
		t.Fatalf("second Checkin: %v", err)
	}
	if !second.OK || !second.Skipped {
		t.Fatalf("second checkin result = %+v, want skipped success", second)
	}
	if checkinCalls != 1 {
		t.Fatalf("checkin POSTs after duplicate = %d, want 1", checkinCalls)
	}
	if len(fake.snapshot()) == 0 {
		t.Fatal("fake saw no requests")
	}
}

func TestTasksCheckinAlreadyFromStatus(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-already", realmCN, nil)
	var checkinCalls int
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"today_checked_in": true,
				"streak_days":      7,
				"daily_credit":     5,
				"active":           true,
			}}
		case "/v2/billing/meter/daily-checkin":
			checkinCalls++
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)

	result, err := provider.Checkin(context.Background(), "cn-already")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !result.OK || result.State != "already" || !result.Skipped {
		t.Fatalf("result = %+v", result)
	}
	if checkinCalls != 0 {
		t.Fatalf("checkin POSTs = %d, want 0 when status says already", checkinCalls)
	}
	if result.Checkin == nil || result.Checkin.StreakDays != 7 {
		t.Fatalf("checkin status = %+v", result.Checkin)
	}
}

func TestTasksCheckinRejectsNonPersonalAccounts(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-ent", realmCN, map[string]any{"enterpriseId": "ent-1"})
	writeTaskAccount(t, dir, "cn-key", realmCN, map[string]any{
		"accessToken": "ck_key.secret",
		"apiKey":      true,
	})
	_, server := newTaskTestUpstream(t, nil)
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	for _, uid := range []string{"cn-ent", "cn-key"} {
		result, err := provider.Checkin(context.Background(), uid)
		if err != nil {
			t.Fatalf("Checkin(%s): %v", uid, err)
		}
		if result.OK || result.Error == "" {
			t.Fatalf("Checkin(%s) = %+v, want rejection", uid, result)
		}
	}
}

func TestTasksCheckinBusyLock(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-busy", realmCN, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath, creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		case "/v2/billing/meter/daily-checkin":
			close(entered)
			<-release
			return http.StatusOK, map[string]any{"code": 0, "msg": "ok"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)

	first := make(chan CheckinResult, 1)
	go func() {
		result, _ := provider.Checkin(context.Background(), "cn-busy")
		first <- result
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first checkin never reached upstream")
	}
	result, err := provider.Checkin(context.Background(), "cn-busy")
	if err != nil {
		t.Fatalf("concurrent Checkin: %v", err)
	}
	if result.OK || !result.Busy {
		t.Fatalf("concurrent result = %+v, want busy", result)
	}
	close(release)
	<-first
}

func TestTasksCheckinStatus(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-status", realmCN, nil)
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == creditsCheckinPath {
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"today_checked_in": true,
				"streak_days":      9,
				"daily_credit":     8,
				"today_credit":     8,
				"active":           true,
			}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.CheckinStatus(context.Background(), "cn-status")
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if !result.OK || result.Checkin == nil || result.Checkin.StreakDays != 9 ||
		!result.Checkin.TodayCheckedIn {
		t.Fatalf("status = %+v", result)
	}
}

// ---------------------------------------------------------------------------
// Daily activity
// ---------------------------------------------------------------------------

func TestTasksDailyChatOncePerDay(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-daily", realmIntl, nil)
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == chatPath {
			return http.StatusOK, map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
			}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, pool := setupTaskProvider(t, realmIntl, dir, server.URL)

	result, err := provider.DailyChat(context.Background(), "intl-daily")
	if err != nil {
		t.Fatalf("DailyChat: %v", err)
	}
	if !result.OK || result.Skipped || result.Activity == nil || result.Activity.Task != "daily_chat" {
		t.Fatalf("daily result = %+v", result)
	}
	if fake.count(http.MethodPost, chatPath) != 1 {
		t.Fatalf("chat calls = %d, want 1", fake.count(http.MethodPost, chatPath))
	}
	account := accountByUID(t, pool, "intl-daily")
	account.mu.RLock()
	last := account.Auth.LastDailyChat
	account.mu.RUnlock()
	if !strings.HasPrefix(last, time.Now().Format("2006-01-02")) {
		t.Fatalf("lastDailyChat = %q", last)
	}

	second, err := provider.DailyChat(context.Background(), "intl-daily")
	if err != nil {
		t.Fatalf("second DailyChat: %v", err)
	}
	if !second.OK || !second.Skipped {
		t.Fatalf("second result = %+v", second)
	}
	if fake.count(http.MethodPost, chatPath) != 1 {
		t.Fatalf("chat calls after duplicate = %d, want 1", fake.count(http.MethodPost, chatPath))
	}
}

func TestTasksDailyChatRejectsCN(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-daily", realmCN, nil)
	_, server := newTaskTestUpstream(t, nil)
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.DailyChat(context.Background(), "cn-daily")
	if err != nil {
		t.Fatalf("DailyChat: %v", err)
	}
	if result.OK || result.Error == "" {
		t.Fatalf("result = %+v, want CN rejection", result)
	}
}

func setupDailyWebFake(t *testing.T, realm string, create func(*taskTestUpstream, int) (int, any)) (*taskTestUpstream, *Provider, *Pool) {
	t.Helper()
	dir := t.TempDir()
	writeTaskAccount(t, dir, "web-daily", realm, nil)
	var createCount int
	fake, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/console/as/conversations/":
			createCount++
			if create != nil {
				return create(fake, createCount)
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "conv-1"}}
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/conversations/conv-1/session":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"link": "https://sandbox.example/acp", "token": "sandbox-token",
				"sessionId": "conv-1", "cwd": "/workspace",
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/conversations/conv-1":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"status": "completed"}}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete"):
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realm, dir, server.URL)
	return fake, provider, pool
}

func TestTasksDailyChatWebRetryBackoffAndOnce(t *testing.T) {
	fake, provider, pool := setupDailyWebFake(t, realmIntl, func(_ *taskTestUpstream, attempt int) (int, any) {
		if attempt == 1 {
			return http.StatusServiceUnavailable, map[string]any{"code": 14220, "msg": "try later"}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "conv-1"}}
	})
	result, err := provider.DailyChatWeb(context.Background(), "web-daily")
	if err != nil {
		t.Fatalf("DailyChatWeb: %v", err)
	}
	if !result.OK || result.Attempts != 2 || result.Conversation != "conv-1" ||
		result.Status != "completed" || result.Error != "" {
		t.Fatalf("web result = %+v", result)
	}
	if fake.count(http.MethodPost, "/console/as/conversations/") != 2 {
		t.Fatalf("create calls = %d, want 2", fake.count(http.MethodPost, "/console/as/conversations/"))
	}
	account := accountByUID(t, pool, "web-daily")
	account.mu.RLock()
	last := authExtraString(account.Auth, "lastDailyChatWeb")
	account.mu.RUnlock()
	if !strings.HasPrefix(last, time.Now().Format("2006-01-02")) {
		t.Fatalf("lastDailyChatWeb = %q", last)
	}

	second, err := provider.DailyChatWeb(context.Background(), "web-daily")
	if err != nil {
		t.Fatalf("second DailyChatWeb: %v", err)
	}
	if !second.OK || !second.Skipped {
		t.Fatalf("second web result = %+v", second)
	}
	if fake.count(http.MethodPost, "/console/as/conversations/") != 2 {
		t.Fatalf("create calls after duplicate = %d, want 2", fake.count(http.MethodPost, "/console/as/conversations/"))
	}
}

func TestTasksDailyChatWebBusinessRejectNotRetried(t *testing.T) {
	fake, provider, _ := setupDailyWebFake(t, realmCN, func(_ *taskTestUpstream, _ int) (int, any) {
		return http.StatusOK, map[string]any{"code": 12302, "msg": "activity is offline"}
	})
	result, err := provider.DailyChatWeb(context.Background(), "web-daily")
	if err != nil {
		t.Fatalf("DailyChatWeb: %v", err)
	}
	if result.OK || result.Attempts != 1 || !strings.Contains(result.Error, "activity is offline") {
		t.Fatalf("web result = %+v", result)
	}
	if fake.count(http.MethodPost, "/console/as/conversations/") != 1 {
		t.Fatalf("create calls = %d, want 1", fake.count(http.MethodPost, "/console/as/conversations/"))
	}
}

func TestTasksReportActivityCN(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-report", realmCN, nil)
	var reportCalls int
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case "/v2/report":
			reportCalls++
			return http.StatusOK, map[string]any{"code": 0}
		case "/activity/growth/streak":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"streak": map[string]any{"days": 4},
			}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)

	result, err := provider.ReportActivity(context.Background(), "cn-report")
	if err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}
	if !result.OK || result.StreakDays == nil || *result.StreakDays != 4 {
		t.Fatalf("report result = %+v", result)
	}
	if result.Activity == nil || result.Activity.Task != "daily_chat" || !result.Activity.OK {
		t.Fatalf("activity = %+v", result.Activity)
	}
	if reportCalls != 1 {
		t.Fatalf("report calls = %d, want 1", reportCalls)
	}
	account := accountByUID(t, pool, "cn-report")
	account.mu.RLock()
	last := account.Auth.LastActivityReport
	account.mu.RUnlock()
	if !strings.HasPrefix(last, time.Now().Format("2006-01-02")) {
		t.Fatalf("lastActivityReport = %q", last)
	}

	second, err := provider.ReportActivity(context.Background(), "cn-report")
	if err != nil {
		t.Fatalf("second ReportActivity: %v", err)
	}
	if !second.OK || !second.Skipped {
		t.Fatalf("second report = %+v", second)
	}
	if reportCalls != 1 {
		t.Fatalf("report calls after duplicate = %d, want 1", reportCalls)
	}
	if !taskContainsPath(fake.snapshot(), http.MethodPost, "/v2/report") {
		t.Fatal("report path not called")
	}
}

// ---------------------------------------------------------------------------
// Growth tasks
// ---------------------------------------------------------------------------

type growthTestState struct {
	mu         sync.Mutex
	accepted   bool
	reported   bool
	claimed    bool
	firstBuddy string // not_accepted|accepted|completed|claimed
}

func growthStatefulFake(t *testing.T) (*taskTestUpstream, *growthTestState) {
	t.Helper()
	state := &growthTestState{firstBuddy: "not_accepted"}
	fake, _ := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			status := "not_accepted"
			current := 0
			if state.accepted {
				status, current = "accepted", 0
			}
			if state.reported {
				status, current = "completed", 1
			}
			if state.claimed {
				status = "claimed"
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{
					map[string]any{
						"task_code": "create_canvas", "title": "创建设计任务",
						"task_type": "single", "accept_status": status,
						"progress": map[string]any{"current": current, "target": 1},
					},
					map[string]any{
						"task_code": "first_buddy", "title": "领取一只 Buddy",
						"task_type": "auto", "accept_status": state.firstBuddy,
						"progress": map[string]any{
							"current": map[bool]int{true: 1, false: 0}[state.firstBuddy != "not_accepted"],
							"target":  1,
						},
					},
				},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			state.accepted = true
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"results": []any{map[string]any{"task_code": "create_canvas", "status": "accepted"}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			for _, event := range events {
				if event["eventCode"] == "chat_request_send" {
					state.reported = true
				}
			}
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/create_canvas/claim":
			state.claimed = true
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"credit": 300, "energy": 10,
			}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	_ = fake
	return fake, state
}

func TestTasksGrowthAcceptReportClaim(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-growth", realmCN, nil)
	fake, state := growthStatefulFake(t)
	_ = state
	provider, pool := setupTaskProvider(t, realmCN, dir, fakeServerURL(t, fake))
	_ = pool

	result, err := provider.GrowthTasks(context.Background(), "cn-growth", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK || result.EarnedCredit != 300 {
		t.Fatalf("growth result = %+v", result)
	}
	if fake.count(http.MethodPost, "/v2/activity/growth/tasks/accept") != 1 {
		t.Fatalf("accept calls = %d, want 1", fake.count(http.MethodPost, "/v2/activity/growth/tasks/accept"))
	}
	if fake.count(http.MethodPost, "/activity/growth/tasks/create_canvas/claim") != 1 {
		t.Fatalf("claim calls = %d, want 1", fake.count(http.MethodPost, "/activity/growth/tasks/create_canvas/claim"))
	}
	if fake.count(http.MethodPost, "/v2/report") == 0 {
		t.Fatal("no events reported")
	}
	if len(result.Tasks) == 0 {
		t.Fatal("growth result has no tasks")
	}
	account := accountByUID(t, pool, "cn-growth")
	account.mu.RLock()
	account.mu.RUnlock()
	snapshotRaw, hasSnapshot := authExtraJSON(account.Auth, "growthTasks")
	claimsRaw, hasClaims := authExtraJSON(account.Auth, "growthClaims")
	if !hasSnapshot || !hasClaims || len(snapshotRaw) == 0 || len(claimsRaw) == 0 {
		t.Fatalf("growth state not persisted: tasks=%v claims=%v logs=%v err=%s", hasSnapshot, hasClaims, result.Logs, result.Error)
	}

	// The second run sees the upstream task as claimed and must not claim again.
	result2, err := provider.GrowthTasks(context.Background(), "cn-growth", true)
	if err != nil {
		t.Fatalf("second GrowthTasks: %v", err)
	}
	if !result2.OK {
		t.Fatalf("second growth result = %+v", result2)
	}
	if fake.count(http.MethodPost, "/activity/growth/tasks/create_canvas/claim") != 1 {
		t.Fatalf("claim calls after duplicate = %d, want 1", fake.count(http.MethodPost, "/activity/growth/tasks/create_canvas/claim"))
	}
}

func growthCompletedFake(t *testing.T) *taskTestUpstream {
	t.Helper()
	fake, _ := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "create_canvas", "title": "创建设计任务",
					"task_type": "single", "accept_status": "completed",
					"progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/create_canvas/claim":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 100}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	return fake
}

func TestTasksGrowthAutoClaimOff(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-noclaim", realmCN, nil)
	fake := growthCompletedFake(t)
	provider, _ := setupTaskProvider(t, realmCN, dir, fakeServerURL(t, fake))

	result, err := provider.GrowthTasks(context.Background(), "cn-noclaim", false)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK || result.EarnedCredit != 0 {
		t.Fatalf("auto-claim off result = %+v", result)
	}
	if fake.count(http.MethodPost, "/activity/growth/tasks/create_canvas/claim") != 0 {
		t.Fatal("auto-claim off still claimed")
	}
	if len(result.Claimable) != 1 || result.Claimable[0] != "create_canvas" {
		t.Fatalf("claimable = %v", result.Claimable)
	}

	result, err = provider.GrowthTasks(context.Background(), "cn-noclaim", true)
	if err != nil {
		t.Fatalf("GrowthTasks auto: %v", err)
	}
	if !result.OK || result.EarnedCredit != 100 {
		t.Fatalf("auto-claim on result = %+v", result)
	}
	if fake.count(http.MethodPost, "/activity/growth/tasks/create_canvas/claim") != 1 {
		t.Fatalf("claim calls = %d, want 1", fake.count(http.MethodPost, "/activity/growth/tasks/create_canvas/claim"))
	}
}

func TestTasksGrowthTaskToggleOffSkipsBatchOnly(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-tasks-off", realmCN, map[string]any{"tasksEnabled": false})
	var calls int
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, _ *http.Request, _ []byte) (int, any) {
		calls++
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	results, err := provider.GrowthTasksAll(context.Background(), true)
	if err != nil {
		t.Fatalf("GrowthTasksAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("GrowthTasksAll = %+v, want empty", results)
	}
	if calls != 0 || len(fake.snapshot()) != 0 {
		t.Fatalf("batch calls = %d, want 0", calls)
	}

	// An explicit uid is v1's manual action and still reaches upstream even
	// when the scheduler task toggle is off.
	manual, err := provider.GrowthTasks(context.Background(), "cn-tasks-off", true)
	if err != nil {
		t.Fatalf("manual GrowthTasks: %v", err)
	}
	if manual.OK {
		t.Fatalf("manual GrowthTasks = %+v, want no-tasks error", manual)
	}
	if calls == 0 {
		t.Fatal("manual task action did not reach upstream")
	}
}

func TestTasksGrowthFirstBuddyPrerequisiteOrder(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-prereq", realmCN, nil)
	state := &growthTestState{firstBuddy: "not_accepted"}
	var order []string
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			status := "not_accepted"
			current := 0
			if state.accepted {
				status = "accepted"
			}
			if state.reported {
				status, current = "completed", 1
			}
			if state.claimed {
				status = "claimed"
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{
					map[string]any{
						"task_code": "first_buddy", "title": "领取一只 Buddy",
						"task_type": "auto", "accept_status": state.firstBuddy,
						"progress": map[string]any{
							"current": map[bool]int{true: 1, false: 0}[state.firstBuddy != "not_accepted"],
							"target":  1,
						},
					},
					map[string]any{
						"task_code": "create_canvas", "title": "创建设计任务",
						"task_type": "single", "accept_status": status,
						"progress": map[string]any{"current": current, "target": 1},
					},
				},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			if len(events) > 0 {
				if conversation, _ := events[0]["conversationId"].(string); conversation != "" {
					if strings.Contains(conversation, "wb-task") {
						// first_buddy uses the chat event builder; create_canvas is a canvas event.
						for _, event := range events {
							if event["eventCode"] == "chat_request_send" {
								state.firstBuddy = "completed"
								order = append(order, "report:first_buddy")
							}
							if event["eventCode"] == "wbx_design_canvas_task_create" {
								state.reported = true
								order = append(order, "report:create_canvas")
							}
						}
					}
				}
			}
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			order = append(order, "accept:create_canvas")
			state.accepted = true
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"results": []any{map[string]any{"task_code": "create_canvas", "status": "accepted"}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/first_buddy/claim":
			order = append(order, "claim:first_buddy")
			state.firstBuddy = "claimed"
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 300}}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/create_canvas/claim":
			order = append(order, "claim:create_canvas")
			state.claimed = true
			state.reported = true
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 100}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-prereq", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	firstClaim := -1
	firstAccept := -1
	for index, step := range order {
		if step == "claim:first_buddy" && firstClaim < 0 {
			firstClaim = index
		}
		if step == "accept:create_canvas" && firstAccept < 0 {
			firstAccept = index
		}
	}
	if firstClaim < 0 || firstAccept < 0 || firstClaim > firstAccept {
		t.Fatalf("prerequisite order = %v", order)
	}
	if result.EarnedCredit != 400 {
		t.Fatalf("earned = %v, want 400; logs=%v claimed=%v", result.EarnedCredit, result.Logs, result.Claimed)
	}
	_ = taskBodyString
	_ = fmt.Sprintf
}

// ---------------------------------------------------------------------------
// Fun: Buddy / cat / streak
// ---------------------------------------------------------------------------

func TestTasksBuddyOpen(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-buddy", realmCN, nil)
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch r.URL.Path {
		case "/v2/activity/growth/buddy/quota":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"affordable": 2, "max_open_count": 1,
			}}
		case "/v2/activity/growth/buddy/open":
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if body["client_token"] == "" || body["client_token"] == nil {
				return http.StatusBadRequest, map[string]any{"code": 1, "msg": "missing client token"}
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"buddy": "猫咪学霸"}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.BuddyCheckin(context.Background(), "cn-buddy")
	if err != nil {
		t.Fatalf("BuddyCheckin: %v", err)
	}
	if !result.OK || result.Action != "open" || result.Opened != 1 || result.Name != "猫咪学霸" {
		t.Fatalf("buddy result = %+v", result)
	}
	if fake.count(http.MethodPost, "/v2/activity/growth/buddy/open") != 1 {
		t.Fatalf("open calls = %d", fake.count(http.MethodPost, "/v2/activity/growth/buddy/open"))
	}
}

func TestTasksCatTravelClaimAndDepart(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-cat", realmCN, nil)
	state := "arrived"
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case "/activity/growth/buddy/travel/status":
			if state == "arrived" {
				return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"state": "arrived"}}
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"state": "idle"}}
		case "/activity/growth/buddy/travel/claim":
			state = "idle"
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"reward_credit": 50}}
		case "/activity/growth/buddy/travel/config":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"locations": []any{map[string]any{"id": 7, "name": "咖啡馆"}},
			}}
		case "/activity/growth/buddy/travel/depart":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"location": map[string]any{"name": "咖啡馆"},
			}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)

	claim, err := provider.CatTravel(context.Background(), "cn-cat")
	if err != nil {
		t.Fatalf("CatTravel claim: %v", err)
	}
	if !claim.OK || claim.Action != "claim" || claim.Credit != 50 {
		t.Fatalf("claim = %+v", claim)
	}
	depart, err := provider.CatTravel(context.Background(), "cn-cat")
	if err != nil {
		t.Fatalf("CatTravel depart: %v", err)
	}
	if !depart.OK || depart.Action != "depart" || depart.Location != "咖啡馆" {
		t.Fatalf("depart = %+v", depart)
	}
	if fake.count(http.MethodPost, "/activity/growth/buddy/travel/claim") != 1 {
		t.Fatalf("claim calls = %d", fake.count(http.MethodPost, "/activity/growth/buddy/travel/claim"))
	}
}

func TestTasksStreakBonus(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-streak", realmCN, nil)
	yesterday := time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	var makeupUsed, redeemed, drawn int
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case "/activity/growth/heatmap":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"cells": []any{map[string]any{"date": yesterday, "score": 0}},
			}}
		case "/activity/growth/streak":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"streak":       map[string]any{"days": 8},
				"makeup_cards": map[string]any{"balance": map[bool]int{true: 0, false: 1}[makeupUsed > 0]},
				"redemption_status": map[string]any{
					"tiers":          []any{map[string]any{"tier": "7d"}},
					"tier_7d_status": map[bool]string{true: "claimed", false: "available"}[redeemed > 0],
				},
			}}
		case "/activity/growth/makeup-cards/use":
			makeupUsed++
			return http.StatusOK, map[string]any{"code": 0}
		case "/billing/meter/claim-gift", "/v2/billing/meter/claim-gift":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 10}}
		case "/billing/meter/claim-compensation", "/v2/billing/meter/claim-compensation":
			return http.StatusOK, map[string]any{"code": 1, "msg": "already"}
		case "/activity/growth/redeem":
			redeemed++
			return http.StatusOK, map[string]any{"code": 0}
		case "/activity/growth/lottery/summary":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"chances": map[bool]int{true: 0, false: 2}[drawn > 0],
			}}
		case "/activity/growth/lottery/draw":
			drawn++
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.StreakBonus(context.Background(), "cn-streak")
	if err != nil {
		t.Fatalf("StreakBonus: %v", err)
	}
	if !result.OK || result.Drawn != 2 || !result.MakeupUsed ||
		len(result.Redeemed) != 1 || result.Redeemed[0] != "7d" {
		t.Fatalf("streak result = %+v", result)
	}
	if makeupUsed != 1 || redeemed != 1 || drawn != 2 {
		t.Fatalf("effects makeup=%d redeemed=%d drawn=%d", makeupUsed, redeemed, drawn)
	}
	if len(result.Logs) == 0 {
		t.Fatal("streak result has no logs")
	}
	if fake.count(http.MethodPost, "/activity/growth/lottery/draw") != 2 {
		t.Fatalf("draw calls = %d", fake.count(http.MethodPost, "/activity/growth/lottery/draw"))
	}
}

// ---------------------------------------------------------------------------
// Eligibility and duplicate prevention
// ---------------------------------------------------------------------------

func TestTasksManualForceVsBatchDisabled(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-disabled", realmCN, map[string]any{"enabled": false})
	writeTaskAccount(t, dir, "cn-tasks-off", realmCN, map[string]any{"tasksEnabled": false})
	writeTaskAccount(t, dir, "cn-on", realmCN, nil)
	var checkinCalls int
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath, creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		case "/v2/billing/meter/daily-checkin":
			checkinCalls++
			return http.StatusOK, map[string]any{"code": 0, "msg": "ok"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)

	// Explicit uid is the v1 manual action: it forces a disabled account but
	// the hard task-participation opt-out still wins.
	result, err := provider.Checkin(context.Background(), "cn-disabled")
	if err != nil {
		t.Fatalf("manual Checkin: %v", err)
	}
	if !result.OK || result.Skipped {
		t.Fatalf("manual disabled checkin = %+v", result)
	}
	if checkinCalls != 1 {
		t.Fatalf("manual checkin calls = %d", checkinCalls)
	}
	off, err := provider.Checkin(context.Background(), "cn-tasks-off")
	if err != nil {
		t.Fatalf("task-off Checkin: %v", err)
	}
	if !off.OK || off.Skipped || checkinCalls != 2 {
		t.Fatalf("manual task-off checkin = %+v calls=%d, want forced run", off, checkinCalls)
	}
	results, err := provider.CheckinAll(context.Background())
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	uids := make([]string, 0, len(results))
	for _, row := range results {
		uids = append(uids, row.UID)
	}
	if len(uids) != 1 || uids[0] != "cn-on" {
		t.Fatalf("CheckinAll targeted %v; want only the enabled/task-on account", uids)
	}
	if checkinCalls != 3 {
		t.Fatalf("batch checkin calls = %d, want 3", checkinCalls)
	}
}

func TestTasksResultShapesRedactSecrets(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-redact", realmCN, map[string]any{
		"accessToken": "secret-access-token",
	})
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == "/v2/billing/meter/daily-checkin" {
			return http.StatusInternalServerError, map[string]any{
				"code": 1, "msg": "rejected Bearer secret-access-token",
			}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.Checkin(context.Background(), "cn-redact")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.OK {
		t.Fatalf("result = %+v, want failure", result)
	}
	if strings.Contains(result.Error, "secret-access-token") {
		t.Fatalf("error leaked token: %q", result.Error)
	}
	assertNoSecret(t, "checkin failure", result, "secret-access-token")

	fields := map[string]any{
		"ok": true, "state": "checked", "msg": "ok",
		"activity": map[string]any{"message": "ok"},
	}
	assertNoSecret(t, "shape", fields, "secret-access-token")
}

// fakeServerURL is a small helper for tests that build the fake separately from
// the provider and need its URL after the provider already exists.
func fakeServerURL(t *testing.T, fake *taskTestUpstream) string {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return server.URL
}

func TestTasksCheckinRestartIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-restart", realmCN, nil)
	var checkinCalls int
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath, creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		case "/v2/billing/meter/daily-checkin":
			checkinCalls++
			return http.StatusOK, map[string]any{"code": 0, "msg": "ok"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	first, err := provider.Checkin(context.Background(), "cn-restart")
	if err != nil || !first.OK {
		t.Fatalf("first Checkin = %+v, %v", first, err)
	}
	// Simulate a scheduler restart by constructing a fresh provider over the
	// same credential directory.
	restarted, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	second, err := restarted.Checkin(context.Background(), "cn-restart")
	if err != nil {
		t.Fatalf("restarted Checkin: %v", err)
	}
	if !second.OK || !second.Skipped {
		t.Fatalf("restarted checkin = %+v, want skipped", second)
	}
	if checkinCalls != 1 {
		t.Fatalf("checkin POSTs after restart = %d, want 1", checkinCalls)
	}
}

func TestTasksDailyRestartIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-restart", realmIntl, nil)
	var chatCalls int
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == chatPath {
			chatCalls++
			return http.StatusOK, map[string]any{"choices": []any{}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	if result, err := provider.DailyChat(context.Background(), "intl-restart"); err != nil || !result.OK {
		t.Fatalf("first DailyChat = %+v, %v", result, err)
	}
	restarted, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	second, err := restarted.DailyChat(context.Background(), "intl-restart")
	if err != nil {
		t.Fatalf("restarted DailyChat: %v", err)
	}
	if !second.OK || !second.Skipped {
		t.Fatalf("restarted DailyChat = %+v, want skipped", second)
	}
	if chatCalls != 1 {
		t.Fatalf("chat calls after restart = %d, want 1", chatCalls)
	}
}

func TestTasksResultShapes(t *testing.T) {
	cases := []struct {
		name   string
		value  any
		fields []string
	}{
		{"checkin", CheckinResult{OK: true, State: "checked", Code: 0, Msg: "ok"}, []string{"ok", "state", "code", "msg"}},
		{"daily", DailyResult{OK: true, Msg: "ok", Activity: &ActivityEvent{Task: "daily_chat"}}, []string{"ok", "msg", "activity"}},
		{"growth", GrowthResult{OK: true, Tasks: []GrowthTask{}, Logs: []string{}, Claimed: []string{}}, []string{"ok", "tasks", "logs", "earned_credit", "claimed"}},
		{"buddy", BuddyResult{OK: true, Action: "open", Opened: 1}, []string{"ok", "action", "opened"}},
		{"cat", CatResult{OK: true, Action: "depart", Credit: 1}, []string{"ok", "action", "credit"}},
		{"streak", StreakResult{OK: true, Msg: "ok", Logs: []string{"x"}, Redeemed: []string{}}, []string{"ok", "logs", "msg", "redeemed"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			text := taskJSON(t, test.value)
			for _, field := range test.fields {
				if !strings.Contains(text, `"`+field+`"`) {
					t.Fatalf("%s result missing %q: %s", test.name, field, text)
				}
			}
			assertNoSecret(t, test.name+" shape", test.value, "access-token", "refresh-token")
		})
	}
}

func TestTasksDailyChatAcceptsSSEBody(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-sse", realmIntl, nil)
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == chatPath {
			chatCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(server.Close)
	provider, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	result, err := provider.DailyChat(context.Background(), "intl-sse")
	if err != nil {
		t.Fatalf("DailyChat: %v", err)
	}
	if !result.OK || result.Error != "" {
		t.Fatalf("SSE daily chat = %+v", result)
	}
	if chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1", chatCalls)
	}
}

func TestTasksCatTravelEnsuresFirstBuddy(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-cat-buddy", realmCN, nil)
	buddyStatus := "not_accepted"
	departs := 0
	order := []string{}
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case "/activity/growth/buddy/travel/status":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"state": "idle"}}
		case "/activity/growth/buddy/travel/config":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"locations": []any{map[string]any{"id": 1, "name": "咖啡馆"}},
			}}
		case "/activity/growth/buddy/travel/depart":
			departs++
			if departs == 1 {
				return http.StatusBadRequest, map[string]any{"code": 400, "msg": "no active buddy"}
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"location": map[string]any{"name": "咖啡馆"},
			}}
		case "/v2/activity/growth/tasks":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "first_buddy", "task_type": "auto",
					"accept_status": buddyStatus,
					"progress": map[string]any{
						"current": map[bool]int{true: 1, false: 0}[buddyStatus != "not_accepted"],
						"target":  1,
					},
				}},
			}}
		case "/v2/report":
			buddyStatus = "completed"
			order = append(order, "report")
			return http.StatusOK, map[string]any{"code": 0}
		case "/activity/growth/tasks/first_buddy/claim":
			buddyStatus = "claimed"
			order = append(order, "claim")
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 300}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.CatTravel(context.Background(), "cn-cat-buddy")
	if err != nil {
		t.Fatalf("CatTravel: %v", err)
	}
	if !result.OK || result.Action != "depart" || result.Location != "咖啡馆" {
		t.Fatalf("CatTravel result = %+v, order=%v", result, order)
	}
	if len(order) != 2 || order[0] != "report" || order[1] != "claim" {
		t.Fatalf("ensure buddy order = %v", order)
	}
	if departs != 2 {
		t.Fatalf("depart calls = %d, want 2 (retry after ensuring Buddy)", departs)
	}
}

func TestTasksGrowthMiniProgramChain(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-mp", realmCN, nil)
	mpAccepted := false
	mpDone := false
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		mp := strings.EqualFold(r.Header.Get("X-Client-Platform"), "miniprogram") ||
			strings.EqualFold(r.Header.Get("X-Client-Platform"), "mp-weixin")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && !mp:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "first_buddy", "task_type": "auto",
					"accept_status": "claimed", "progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && mp:
			status := "not_accepted"
			current := 0
			if mpAccepted {
				status = "accepted"
			}
			if mpDone {
				status, current = "completed", 1
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "Sequential_Tasks_1", "title": "小程序对话",
					"accept_status": status, "progress": map[string]any{"current": current, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			if !mp {
				return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"results": []any{}}}
			}
			mpAccepted = true
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"results": []any{map[string]any{"task_code": "Sequential_Tasks_1", "status": "accepted"}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			if mp {
				mpDone = true
				if !strings.EqualFold(r.Header.Get("X-Client-Platform"), "mp-weixin") ||
					r.Header.Get("X-Client-Product") != "workbuddy-mp" {
					return http.StatusBadRequest, map[string]any{"code": 1, "msg": "missing mp headers"}
				}
			}
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/Sequential_Tasks_1/claim":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 100}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-mp", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK || result.EarnedCredit != 100 {
		t.Fatalf("MP growth result = %+v", result)
	}
	foundMP := false
	for _, task := range result.Tasks {
		if task.TaskCode == "Sequential_Tasks_1" {
			foundMP = true
		}
	}
	if !foundMP {
		t.Fatalf("MP task missing from result: %+v", result.Tasks)
	}
}
