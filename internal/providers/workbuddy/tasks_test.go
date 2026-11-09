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

// taskSSEResponse lets a fake return a real event stream: the handler result is
// written verbatim and the connection stays open until the client disconnects.
type taskSSEResponse struct {
	Status int
	Header http.Header
	Body   string
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
	if stream, ok := payload.(taskSSEResponse); ok {
		for name, values := range stream.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		streamStatus := stream.Status
		if streamStatus == 0 {
			streamStatus = http.StatusOK
		}
		w.WriteHeader(streamStatus)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = io.WriteString(w, stream.Body)
			flusher.Flush()
		} else {
			_, _ = io.WriteString(w, stream.Body)
		}
		<-r.Context().Done()
		return
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

type dailyWebFakeState struct {
	mu          sync.Mutex
	acpMethods  []string
	acpPrompted bool
}

func (s *dailyWebFakeState) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acpMethods...)
}

func setupDailyWebFake(t *testing.T, realm string, create func(*taskTestUpstream, int) (int, any)) (*taskTestUpstream, *Provider, *Pool, *dailyWebFakeState) {
	t.Helper()
	dir := t.TempDir()
	writeTaskAccount(t, dir, "web-daily", realm, nil)
	state := &dailyWebFakeState{}
	var createCount int
	var serverURL string
	fake, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/acp":
			return http.StatusOK, taskSSEResponse{
				Header: http.Header{
					"Content-Type":      []string{"text/event-stream"},
					"Acp-Connection-Id": []string{"acp-conn-1"},
					"Cache-Control":     []string{"no-cache"},
				},
				Body: "data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n",
			}
		case r.Method == http.MethodPost && r.URL.Path == "/acp":
			var request map[string]any
			_ = json.Unmarshal(raw, &request)
			method, _ := request["method"].(string)
			state.mu.Lock()
			state.acpMethods = append(state.acpMethods, method)
			if method == "session/prompt" {
				state.acpPrompted = true
			}
			state.mu.Unlock()
			if r.Header.Get("Acp-Connection-Id") != "acp-conn-1" ||
				!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sandbox-token") {
				return http.StatusBadRequest, map[string]any{"error": "bad ACP headers"}
			}
			return http.StatusAccepted, map[string]any{"jsonrpc": "2.0"}
		case r.Method == http.MethodPost && r.URL.Path == "/console/as/conversations/":
			createCount++
			if create != nil {
				return create(fake, createCount)
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "conv-1"}}
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/conversations/conv-1/session":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"link": serverURL + "/acp", "token": "sandbox-token",
				"sessionId": "conv-1", "cwd": "/workspace",
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/conversations/conv-1":
			state.mu.Lock()
			prompted := state.acpPrompted
			state.mu.Unlock()
			status := "creating"
			if prompted {
				status = "completed"
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"status": status}}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete"):
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	serverURL = server.URL
	provider, pool := setupTaskProvider(t, realm, dir, server.URL)
	return fake, provider, pool, state
}

func TestTasksDailyChatWebRetryBackoffAndOnce(t *testing.T) {
	fake, provider, pool, acp := setupDailyWebFake(t, realmIntl, func(_ *taskTestUpstream, attempt int) (int, any) {
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
	if !strings.Contains(result.Msg, "网页通道会话跑完") || !strings.Contains(result.Msg, "重试 1 次后") {
		t.Fatalf("web success Msg = %q", result.Msg)
	}
	if fake.count(http.MethodPost, "/console/as/conversations/") != 2 {
		t.Fatalf("create calls = %d, want 2", fake.count(http.MethodPost, "/console/as/conversations/"))
	}
	if methods := acp.methods(); strings.Join(methods, ",") != "initialize,session/load,session/set_model,session/prompt" {
		t.Fatalf("ACP methods = %v", methods)
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
	fake, provider, _, _ := setupDailyWebFake(t, realmCN, func(_ *taskTestUpstream, _ int) (int, any) {
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
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	seed, _ := json.Marshal(growthSnapshot{
		Summary: GrowthSummary{Energy: 4, StreakDays: 9,
			TierStatuses: map[string]string{"7d": "available"},
			Travel:       map[string]any{"state": "idle"}},
		StreakDays: 9,
	})
	account := accountByUID(t, pool, "cn-cat-buddy")
	account.mu.Lock()
	account.Auth.Extra = map[string]json.RawMessage{"growthTasks": seed}
	account.mu.Unlock()
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
	account.mu.RLock()
	persisted, _ := authExtraJSON(account.Auth, "growthTasks")
	account.mu.RUnlock()
	var snapshot growthSnapshot
	if err := json.Unmarshal(persisted, &snapshot); err != nil {
		t.Fatalf("decode ensureBuddy snapshot: %v", err)
	}
	if snapshot.Summary.StreakDays != 9 || snapshot.Summary.Energy != 4 {
		t.Fatalf("ensureBuddy clobbered cached summary: %+v", snapshot.Summary)
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
			tasks := []any{map[string]any{
				"task_code": "Sequential_Tasks_1", "title": "小程序对话",
				"accept_status": status, "progress": map[string]any{"current": current, "target": 1},
			}}
			if mpAccepted {
				// A refresh after accept must not re-admit a non-MP code.
				tasks = append(tasks, map[string]any{
					"task_code": "Sequential_Tasks_99", "accept_status": "accepted",
					"progress": map[string]any{"current": 0, "target": 1},
				})
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"tasks": tasks}}
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
		if task.TaskCode == "Sequential_Tasks_99" {
			t.Fatalf("unknown MP task re-entered after accept: %+v", result.Tasks)
		}
	}
	if !foundMP {
		t.Fatalf("MP task missing from result: %+v", result.Tasks)
	}
}

func TestTasksCheckinHTTPErrorWithCodeZeroFails(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-http-zero", realmCN, nil)
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath, creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		case "/v2/billing/meter/daily-checkin":
			return http.StatusInternalServerError, map[string]any{"code": 0, "msg": "ok"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.Checkin(context.Background(), "cn-http-zero")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.OK || result.Error == "" {
		t.Fatalf("HTTP 500/code 0 result = %+v, want failure", result)
	}
	account := accountByUID(t, pool, "cn-http-zero")
	account.mu.RLock()
	last := account.Auth.LastCheckin
	account.mu.RUnlock()
	if last != "" {
		t.Fatalf("failed HTTP/code0 checkin wrote lastCheckin = %q", last)
	}
}

func TestTasksCheckinNetworkFailureActivity(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-net", realmCN, nil)
	_, server := newTaskTestUpstream(t, nil)
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	provider.tasksDoer = chatDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, fmt.Errorf("network down")
	})
	result, err := provider.Checkin(context.Background(), "cn-net")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.OK || result.Error == "" {
		t.Fatalf("result = %+v, want failure", result)
	}
	if result.Activity == nil || result.Activity.OK || result.Activity.Task != "checkin" {
		t.Fatalf("activity = %+v, want failed checkin event", result.Activity)
	}
}

func TestTasksCheckinStatusMergePreservesPartialFields(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-merge", realmCN, nil)
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		case "/v2/billing/meter/daily-checkin":
			return http.StatusOK, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{
				"today_checked_in": true,
			}}
		case creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"IsPaidUser": false, "Packages": []any{},
			}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-merge")
	account.SetCredits(&CreditSnapshot{
		Checkin:  &CheckinStatus{StreakDays: 5, DailyCredit: 3, TodayCredit: 3, Active: true},
		Packages: []CreditPackage{},
	})
	result, err := provider.Checkin(context.Background(), "cn-merge")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.Checkin == nil || result.Checkin.StreakDays != 5 || result.Checkin.DailyCredit != 3 {
		t.Fatalf("merged checkin = %+v, want cached streak/credit preserved", result.Checkin)
	}
	account.mu.RLock()
	cached := cloneCheckin(account.Credits.Checkin)
	account.mu.RUnlock()
	if cached == nil || cached.StreakDays != 5 || !cached.TodayCheckedIn {
		t.Fatalf("persisted merged checkin = %+v", cached)
	}
}

func TestTasksRestrictedAllSelectorsAndDailyPersistence(t *testing.T) {
	dir := t.TempDir()
	future := float64(time.Now().Add(time.Hour).Unix())
	writeTaskAccount(t, dir, "cn-restricted", realmCN, map[string]any{"restrictedUntil": future})
	writeTaskAccount(t, dir, "cn-cool", realmCN, nil)
	writeTaskAccount(t, dir, "intl-restricted", realmIntl, map[string]any{"restrictedUntil": future})
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
	cn, cnPool := setupTaskProvider(t, realmCN, dir, server.URL)
	cool := accountByUID(t, cnPool, "cn-cool")
	cool.mu.Lock()
	cool.State.CoolUntil = time.Now().Add(time.Hour)
	cool.mu.Unlock()
	if rows, err := cn.CheckinAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("CheckinAll restricted = %+v, %v", rows, err)
	}
	if rows, err := cn.GrowthTasksAll(context.Background(), true); err != nil || len(rows) != 0 {
		t.Fatalf("GrowthTasksAll restricted = %+v, %v", rows, err)
	}
	if rows, err := cn.BuddyCheckinAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("BuddyCheckinAll restricted = %+v, %v", rows, err)
	}
	if rows, err := cn.CatTravelAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("CatTravelAll restricted = %+v, %v", rows, err)
	}
	if rows, err := cn.StreakBonusAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("StreakBonusAll restricted = %+v, %v", rows, err)
	}
	if checkinCalls != 0 {
		t.Fatalf("restricted batch made %d upstream calls", checkinCalls)
	}
	if accounts := cn.Pool().TaskAccounts(); len(accounts) != 0 {
		t.Fatalf("Pool.TaskAccounts returned restricted account(s): %+v", accounts)
	}
	manual, err := cn.Checkin(context.Background(), "cn-restricted")
	if err != nil || !manual.OK {
		t.Fatalf("manual restricted checkin = %+v, %v", manual, err)
	}
	if checkinCalls != 1 {
		t.Fatalf("manual checkin calls = %d, want 1", checkinCalls)
	}

	intl, intlPool := setupTaskProvider(t, realmIntl, dir, server.URL)
	if rows, err := intl.DailyChatAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("DailyChatAll restricted = %+v, %v", rows, err)
	}
	manualDaily, err := intl.DailyChat(context.Background(), "intl-restricted")
	if err != nil {
		t.Fatalf("manual restricted DailyChat: %v", err)
	}
	if !manualDaily.OK {
		t.Fatalf("manual restricted DailyChat = %+v, want forced run", manualDaily)
	}
	account := accountByUID(t, intlPool, "intl-restricted")
	account.mu.RLock()
	until := taskRestrictedUntil(account.Auth)
	account.mu.RUnlock()
	if until != 0 {
		t.Fatalf("successful manual DailyChat did not clear restrictedUntil: %v", until)
	}
}

func TestTasksDailyChatPersists403Restriction(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-403", realmIntl, nil)
	blocked := true
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == chatPath && blocked {
			return http.StatusForbidden, map[string]any{"code": 11140, "msg": "request illegal"}
		}
		if r.URL.Path == chatPath {
			return http.StatusOK, map[string]any{"choices": []any{}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, pool := setupTaskProvider(t, realmIntl, dir, server.URL)
	result, err := provider.DailyChat(context.Background(), "intl-403")
	if err != nil {
		t.Fatalf("DailyChat: %v", err)
	}
	if result.OK || !result.Restricted {
		t.Fatalf("403 daily result = %+v", result)
	}
	account := accountByUID(t, pool, "intl-403")
	account.mu.RLock()
	until := taskRestrictedUntil(account.Auth)
	coolUntil := account.State.CoolUntil
	account.mu.RUnlock()
	if until <= float64(time.Now().Unix()) {
		t.Fatalf("restrictedUntil = %v, want future", until)
	}
	if !coolUntil.After(time.Now()) {
		t.Fatalf("runtime coolUntil = %v, want future", coolUntil)
	}
	restarted, restartedPool := setupTaskProvider(t, realmIntl, dir, server.URL)
	if rows, err := restarted.DailyChatAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("DailyChatAll after 403 = %+v, %v", rows, err)
	}
	blocked = false
	manual, err := restarted.DailyChat(context.Background(), "intl-403")
	if err != nil {
		t.Fatalf("manual restricted DailyChat: %v", err)
	}
	if !manual.OK {
		t.Fatalf("manual restricted DailyChat = %+v, want forced run", manual)
	}
	account = accountByUID(t, restartedPool, "intl-403")
	account.mu.RLock()
	until = taskRestrictedUntil(account.Auth)
	coolUntil = account.State.CoolUntil
	account.mu.RUnlock()
	if until != 0 || !coolUntil.IsZero() {
		t.Fatalf("successful forced DailyChat did not clear restriction state: extra=%v runtime=%v", until, coolUntil)
	}
}

func TestTasksACPChannelCountsEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/acp" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Acp-Connection-Id", "conn-events")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	channel := newACPTurnChannel(chatDoerFunc(func(ctx context.Context, req *http.Request, _ string) (*http.Response, error) {
		return http.DefaultClient.Do(req.WithContext(ctx))
	}), server.URL+"/acp", "sandbox-token", "test-agent", "")
	if err := channel.open(context.Background()); err != nil {
		t.Fatalf("open ACP channel: %v", err)
	}
	defer channel.close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		updates, chunks := channel.counts()
		if updates >= 1 && chunks >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	updates, chunks := channel.counts()
	t.Fatalf("ACP event counts = %d/%d, want >=1/1", updates, chunks)
}

func TestTasksDailyChatBodyNormalizationAndGzip(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-cli", realmIntl, map[string]any{"product": productCLI})
	fake := &chatTestFake{body: "data: {\"choices\":[]}\n\n"}
	server := newChatTestServer(t, fake)
	provider, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	result, err := provider.DailyChat(context.Background(), "intl-cli")
	if err != nil || !result.OK {
		t.Fatalf("DailyChat = %+v, %v", result, err)
	}
	var chatCall *chatTestCall
	for _, call := range fake.snapshot() {
		if call.Method == http.MethodPost && call.Path == chatPath {
			copy := call
			chatCall = &copy
			break
		}
	}
	if chatCall == nil {
		t.Fatal("no daily chat call")
	}
	if !strings.EqualFold(chatCall.Header.Get("Content-Encoding"), "gzip") {
		t.Fatalf("CLI daily chat Content-Encoding = %q, want gzip", chatCall.Header.Get("Content-Encoding"))
	}
	var body map[string]any
	if err := json.Unmarshal(chatCall.Body, &body); err != nil {
		t.Fatalf("decode daily body: %v", err)
	}
	if body["cache_retention"] != "long" {
		t.Fatalf("cache_retention = %v", body["cache_retention"])
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("messages = %v", messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != defaultChatSystemPrompt {
		t.Fatalf("first daily message = %+v", first)
	}
	options, _ := body["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("stream_options = %+v", options)
	}
}

func TestTasksGrowthSummaryPreservedOnPartialFetch(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-cache", realmCN, nil)
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" {
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "first_buddy", "task_type": "auto",
					"accept_status": "claimed", "progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		}
		return http.StatusInternalServerError, map[string]any{"code": 1, "msg": "summary offline"}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-cache")
	cached := GrowthSummary{
		Energy: 5, StreakDays: 7, MakeupCards: 2,
		Tiers:          []any{map[string]any{"tier": "7d"}},
		TierStatuses:   map[string]string{"7d": "available", "14d": "locked", "28d": "locked"},
		LotteryChances: 3,
		Travel:         map[string]any{"state": "idle"},
	}
	raw, _ := json.Marshal(growthSnapshot{Summary: cached, StreakDays: 7})
	account.mu.Lock()
	if account.Auth.Extra == nil {
		account.Auth.Extra = map[string]json.RawMessage{}
	}
	account.Auth.Extra["growthTasks"] = raw
	account.mu.Unlock()

	result, err := provider.GrowthTasks(context.Background(), "cn-cache", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if result.StreakDays != 7 || result.Summary.Energy != 5 ||
		result.Summary.LotteryChances != 3 || result.Summary.TierStatuses["7d"] != "available" ||
		result.Summary.Travel["state"] != "idle" {
		t.Fatalf("partial summary clobbered cache: %+v", result.Summary)
	}
	account.mu.RLock()
	persisted, _ := authExtraJSON(account.Auth, "growthTasks")
	account.mu.RUnlock()
	var snapshot growthSnapshot
	if err := json.Unmarshal(persisted, &snapshot); err != nil {
		t.Fatalf("decode persisted snapshot: %v", err)
	}
	if snapshot.Summary.StreakDays != 7 || snapshot.Summary.Energy != 5 {
		t.Fatalf("persisted summary = %+v", snapshot.Summary)
	}
	_ = fake
}

func TestTasksReportedAndClaimableOnlyOnReportSuccess(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-reportfail", realmCN, nil)
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "create_canvas", "task_type": "single",
					"accept_status": "accepted", "progress": map[string]any{"current": 0, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			return http.StatusOK, map[string]any{"code": 1, "msg": "rejected"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-reportfail", false)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if len(result.Reported) != 0 {
		t.Fatalf("failed reports marked reported: %v", result.Reported)
	}
	if len(result.Claimable) != 0 {
		t.Fatalf("failed report marked claimable: %v", result.Claimable)
	}
	if fake.count(http.MethodPost, "/v2/report") == 0 {
		t.Fatal("no report attempted")
	}
}

func TestTasksMPFailedReportNotCompleted(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-mpfail", realmCN, nil)
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		mp := strings.EqualFold(r.Header.Get("X-Client-Platform"), "miniprogram") ||
			strings.EqualFold(r.Header.Get("X-Client-Platform"), "mp-weixin")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && !mp:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "first_buddy", "task_type": "auto", "accept_status": "claimed",
					"progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && mp:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "Sequential_Tasks_1", "accept_status": "accepted",
					"progress": map[string]any{"current": 0, "target": 1},
				}, map[string]any{
					"task_code": "Sequential_Tasks_99", "accept_status": "accepted",
					"progress": map[string]any{"current": 0, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			return http.StatusOK, map[string]any{"code": 1, "msg": "rejected"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-mpfail", false)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if len(result.Reported) != 0 || len(result.Claimable) != 0 {
		t.Fatalf("MP failed report marked reported/claimable: %+v", result)
	}
	for _, task := range result.Tasks {
		if task.TaskCode == "Sequential_Tasks_1" && task.Status == "completed" {
			t.Fatalf("MP failed report marked task completed: %+v", task)
		}
		if task.TaskCode == "Sequential_Tasks_99" {
			t.Fatalf("unknown Sequential task was processed: %+v", task)
		}
	}
}

func TestTasksWebClaimHeaders(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-webclaim", realmCN, nil)
	var serverURL string
	webCalls := 0
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "create_canvas", "task_type": "single",
					"accept_status": "completed", "progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/create_canvas/claim":
			if !strings.EqualFold(r.Header.Get("x-client-platform"), "web") {
				return http.StatusBadRequest, map[string]any{"code": 400, "msg": "chat claim rejected"}
			}
			webCalls++
			if r.Header.Get("X-Domain") != serverURL ||
				r.Header.Get("Referer") != serverURL+"/profile/growth-center" {
				return http.StatusBadRequest, map[string]any{"code": 1, "msg": "missing web claim headers"}
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 100}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	serverURL = server.URL
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-webclaim", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK || result.EarnedCredit != 100 || webCalls != 1 {
		t.Fatalf("web claim result = %+v, webCalls=%d", result, webCalls)
	}
}

func TestTasksGrowthRewardChain(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-chain", realmCN, nil)
	catState := "arrived"
	claimCalls, departCalls, openCalls := 0, 0, 0
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "first_buddy", "task_type": "auto", "accept_status": "claimed",
					"progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		case "/activity/growth/buddy/travel/status":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"state": catState}}
		case "/activity/growth/buddy/travel/claim":
			claimCalls++
			catState = "idle"
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"reward_credit": 50}}
		case "/activity/growth/buddy/travel/config":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"locations": []any{map[string]any{"id": 1, "name": "咖啡馆"}},
			}}
		case "/activity/growth/buddy/travel/depart":
			departCalls++
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"location": map[string]any{"name": "咖啡馆"},
			}}
		case "/v2/activity/growth/buddy/quota":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"affordable": 1, "max_open_count": 1,
			}}
		case "/v2/activity/growth/buddy/open":
			openCalls++
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"buddy": "猫咪学霸"}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-chain", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	text := strings.Join(result.Logs, "\n")
	if !result.OK || result.EarnedCredit != 50 ||
		!strings.Contains(text, "猫猫日常") || !strings.Contains(text, "Buddy 盲盒") {
		t.Fatalf("chain result = %+v logs=%s", result, text)
	}
	if claimCalls != 1 || departCalls != 1 || openCalls != 1 {
		t.Fatalf("chain calls claim=%d depart=%d open=%d", claimCalls, departCalls, openCalls)
	}
}

func TestTasksMPEventBaseThenEventAndUnknownSequential(t *testing.T) {
	auth := Auth{UID: "mp-user", Nickname: "MP Tester", Realm: realmCN}
	events := buildMPEvents(auth, "Sequential_Tasks_4", 1, time.Now())
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	event := events[0]
	if event["eventCode"] != "automated_task_create_suc" || event["source"] != "manually" {
		t.Fatalf("event source lost to base: %+v", event)
	}
	if event["platform"] != "mini_program" || event["userId"] != "mp-user" {
		t.Fatalf("event base fields = %+v", event)
	}
	tasks := filterMPGrowthTasks([]GrowthTask{
		{TaskCode: "Sequential_Tasks_99"}, {TaskCode: "Sequential_Tasks_1"},
	})
	if len(tasks) != 1 || tasks[0].TaskCode != "Sequential_Tasks_1" {
		t.Fatalf("unknown sequential kept: %+v", tasks)
	}
}

func TestTasksWebBackoffSchedule(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-backoff", realmIntl, nil)
	_, server := newTaskTestUpstream(t, nil)
	provider, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	provider.tasksBackoff = 5 * time.Millisecond
	provider.tasksBackoffMax = 12 * time.Millisecond
	if got := provider.taskBackoffDelay(0); got != 5*time.Millisecond {
		t.Fatalf("backoff(0) = %v", got)
	}
	if got := provider.taskBackoffDelay(1); got != 10*time.Millisecond {
		t.Fatalf("backoff(1) = %v", got)
	}
	if got := provider.taskBackoffDelay(2); got != 12*time.Millisecond {
		t.Fatalf("backoff(2) = %v", got)
	}
	if got := provider.taskBackoffDelay(9); got != 12*time.Millisecond {
		t.Fatalf("backoff cap = %v", got)
	}
}

func TestTasksDailyActivityEventNotDuplicated(t *testing.T) {
	_, provider, _, _ := setupDailyWebFake(t, realmIntl, nil)
	daily, err := provider.DailyChat(context.Background(), "web-daily")
	if err != nil || !daily.OK {
		t.Fatalf("DailyChat = %+v, %v", daily, err)
	}
	if daily.Activity == nil {
		t.Fatal("first daily activity emitted no event")
	}
	web, err := provider.DailyChatWeb(context.Background(), "web-daily")
	if err != nil || !web.OK {
		t.Fatalf("DailyChatWeb = %+v, %v", web, err)
	}
	if web.Activity != nil {
		t.Fatalf("second daily activity emitted a duplicate event: %+v", web.Activity)
	}
	report, err := provider.ReportActivity(context.Background(), "web-daily")
	if err != nil || !report.OK {
		t.Fatalf("ReportActivity = %+v, %v", report, err)
	}
	if report.Activity != nil {
		t.Fatalf("activity report duplicated the day's event: %+v", report.Activity)
	}
}

func TestTasksRedactValueSecretSpellings(t *testing.T) {
	value := map[string]any{
		"access_token": "secret-value",
		"deviceToken":  "device-secret",
		"nested": map[string]any{
			"Authorization": "Bearer secret-value",
			"safe":          "visible",
		},
	}
	out := redactTaskValue(value, []string{"secret-value"})
	text := taskJSON(t, out)
	if strings.Contains(text, "secret-value") || strings.Contains(text, "device-secret") {
		t.Fatalf("redaction leaked: %s", text)
	}
	if !strings.Contains(text, "visible") {
		t.Fatalf("redaction removed safe data: %s", text)
	}
}

func TestTasksBuddyQuotaBusinessIdle(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-buddy-idle", realmCN, nil)
	var openCalls int
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == "/v2/activity/growth/buddy/quota" {
			return http.StatusOK, map[string]any{"code": 123, "msg": "quota unavailable"}
		}
		if r.URL.Path == "/v2/activity/growth/buddy/open" {
			openCalls++
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.BuddyCheckin(context.Background(), "cn-buddy-idle")
	if err != nil {
		t.Fatalf("BuddyCheckin: %v", err)
	}
	if !result.OK || result.Action != "idle" || result.Opened != 0 || openCalls != 0 {
		t.Fatalf("quota business idle = %+v, openCalls=%d", result, openCalls)
	}
}

func TestTasksDesktopEventShapes(t *testing.T) {
	auth := Auth{UID: "event-user", Nickname: "Event Tester", Realm: realmCN}
	now := time.Now()
	expert := buildTaskEvent(auth, "expert", 0, &growthExpert{ID: "ex_xQHHD1u5Jkua", Name: "无畏契约拼豆专家"}, now)
	if expert["eventCode"] != "expert_actual_use" || expert["id"] != "ex_xQHHD1u5Jkua" ||
		expert["userId"] != "event-user" || expert["expertType"] != "agent" {
		t.Fatalf("expert event = %+v", expert)
	}
	canvas := buildTaskEvent(auth, "canvas", 1, nil, now)
	if canvas["eventCode"] != "wbx_design_canvas_task_create" || canvas["userId"] != "event-user" {
		t.Fatalf("canvas event = %+v", canvas)
	}
	rich := buildDesktopChatEvents(auth, 0, now)
	if len(rich) != 6 || rich[0]["eventCode"] != "agent_task_created" ||
		rich[5]["eventCode"] != "agent_task_completed" {
		t.Fatalf("rich meow events = %+v", rich)
	}
	playbook := buildPlaybookEvents(auth, 0, now)
	if len(playbook) != 2 || playbook[1]["eventCode"] != "playbook_prompt_send" {
		t.Fatalf("playbook events = %+v", playbook)
	}
}

func TestTasksChat403RestrictionSharedWithTaskSelectors(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "cn-chat403", realmCN, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmCN, dir, map[string]any{})
	fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
		return http.StatusForbidden, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`)
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	account := accountByUID(t, provider.Pool(), "cn-chat403")
	account.mu.RLock()
	until := taskRestrictedUntil(account.Auth)
	coolUntil := account.State.CoolUntil
	account.mu.RUnlock()
	if until <= float64(time.Now().Unix()) || !coolUntil.After(time.Now()) {
		t.Fatalf("chat 403 restriction not unified: extra=%v runtime=%v", until, coolUntil)
	}
	// A repeated probe refreshes runtime from the same persisted window but
	// must not extend either clock.
	if err := provider.Pool().NoteRestricted(account, "repeated 403"); err != nil {
		t.Fatalf("repeated NoteRestricted: %v", err)
	}
	account.mu.RLock()
	repeatedUntil := taskRestrictedUntil(account.Auth)
	repeatedCool := account.State.CoolUntil
	account.mu.RUnlock()
	if repeatedUntil != until || !repeatedCool.Equal(coolUntil) {
		t.Fatalf("repeated 403 extended restriction: first=%v/%v repeated=%v/%v",
			until, coolUntil, repeatedUntil, repeatedCool)
	}
	if rows, err := provider.CheckinAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("CheckinAll after chat 403 = %+v, %v", rows, err)
	}
	if rows, err := provider.GrowthTasksAll(context.Background(), true); err != nil || len(rows) != 0 {
		t.Fatalf("GrowthTasksAll after chat 403 = %+v, %v", rows, err)
	}
	if accounts := provider.Pool().TaskAccounts(); len(accounts) != 0 {
		t.Fatalf("TaskAccounts after chat 403 = %+v", accounts)
	}
	restarted, restartedPool := setupTaskProvider(t, realmCN, dir, server.URL)
	restartedAccount := accountByUID(t, restartedPool, "cn-chat403")
	restartedAccount.mu.RLock()
	persistedUntil := taskRestrictedUntil(restartedAccount.Auth)
	seededCool := restartedAccount.State.CoolUntil
	restartedAccount.mu.RUnlock()
	if persistedUntil <= float64(time.Now().Unix()) {
		t.Fatalf("chat 403 restriction not persisted across restart: %v", persistedUntil)
	}
	if !seededCool.After(time.Now()) {
		t.Fatalf("restart did not seed runtime restriction: %v", seededCool)
	}
	// Even if the runtime cooldown is emptied, a repeated restricted-account
	// probe must refresh it from the persisted window, not extend the clock.
	restartedAccount.mu.Lock()
	restartedAccount.State.CoolUntil = time.Time{}
	restartedAccount.mu.Unlock()
	if err := restarted.Pool().NoteRestricted(restartedAccount, "reseed"); err != nil {
		t.Fatalf("reseed NoteRestricted: %v", err)
	}
	restartedAccount.mu.RLock()
	reseededCool := restartedAccount.State.CoolUntil
	reseededUntil := taskRestrictedUntil(restartedAccount.Auth)
	restartedAccount.mu.RUnlock()
	if !reseededCool.After(time.Now()) || reseededUntil != persistedUntil {
		t.Fatalf("restriction reseed failed: cool=%v until=%v want-%v", reseededCool, reseededUntil, persistedUntil)
	}
	if rows, err := restarted.CheckinAll(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("CheckinAll after chat 403 restart = %+v, %v", rows, err)
	}
	restarted.chatBaseURL = server.URL
	beforeCalls := len(fake.snapshot())
	if _, err := restarted.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})); err == nil {
		t.Fatal("restarted restricted account served a chat request")
	}
	if len(fake.snapshot()) != beforeCalls {
		t.Fatal("restarted restricted account hot-probed upstream")
	}
	// NoteSuccess is the chat-path proof that the restriction is gone; it must
	// clear both runtime and persisted state.
	if err := provider.Pool().NoteSuccess(account); err != nil {
		t.Fatalf("NoteSuccess: %v", err)
	}
	account.mu.RLock()
	until = taskRestrictedUntil(account.Auth)
	coolUntil = account.State.CoolUntil
	account.mu.RUnlock()
	if until != 0 || !coolUntil.IsZero() {
		t.Fatalf("NoteSuccess did not clear restriction: extra=%v runtime=%v", until, coolUntil)
	}
	cleared, clearedPool := setupTaskProvider(t, realmCN, dir, server.URL)
	clearedAccount := accountByUID(t, clearedPool, "cn-chat403")
	clearedAccount.mu.RLock()
	until = taskRestrictedUntil(clearedAccount.Auth)
	clearedAccount.mu.RUnlock()
	if until != 0 {
		t.Fatalf("NoteSuccess did not persist cleared restriction: %v", until)
	}
	if accounts := cleared.Pool().TaskAccounts(); len(accounts) != 1 {
		t.Fatalf("TaskAccounts after NoteSuccess = %+v", accounts)
	}
}

func TestTasksMPClaimableRequiresTargetProgress(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-mptruth", realmCN, nil)
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		mp := strings.EqualFold(r.Header.Get("X-Client-Platform"), "miniprogram") ||
			strings.EqualFold(r.Header.Get("X-Client-Platform"), "mp-weixin")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && !mp:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "first_buddy", "task_type": "auto", "accept_status": "claimed",
					"progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && mp:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "Sequential_Tasks_1", "accept_status": "accepted",
					"progress": map[string]any{"current": 0, "target": 2},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-mptruth", false)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	foundReported := false
	for _, code := range result.Reported {
		if code == "Sequential_Tasks_1" {
			foundReported = true
		}
	}
	if !foundReported {
		t.Fatalf("successful MP report not tracked: %+v", result.Reported)
	}
	for _, code := range result.Claimable {
		if code == "Sequential_Tasks_1" {
			t.Fatalf("unreached MP target marked claimable: %+v", result.Claimable)
		}
	}
	for _, task := range result.Tasks {
		if task.TaskCode == "Sequential_Tasks_1" && task.Status == "completed" {
			t.Fatalf("unreached MP target marked completed: %+v", task)
		}
	}
}

func TestTasksDailyFailureEventDeduplicatedAcrossLegs(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-faildedupe", realmIntl, nil)
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch {
		case r.URL.Path == chatPath:
			return http.StatusInternalServerError, map[string]any{"code": 1, "msg": "chat broken"}
		case r.Method == http.MethodPost && r.URL.Path == "/console/as/conversations/":
			return http.StatusInternalServerError, map[string]any{"code": 14220, "msg": "web broken"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	daily, err := provider.DailyChat(context.Background(), "intl-faildedupe")
	if err != nil {
		t.Fatalf("DailyChat: %v", err)
	}
	if daily.OK || daily.Activity == nil || daily.Activity.OK {
		t.Fatalf("first failed leg activity = %+v", daily)
	}
	web, err := provider.DailyChatWeb(context.Background(), "intl-faildedupe")
	if err != nil {
		t.Fatalf("DailyChatWeb: %v", err)
	}
	if web.OK {
		t.Fatalf("web unexpectedly succeeded: %+v", web)
	}
	if web.Activity != nil {
		t.Fatalf("second failed leg duplicated the daily event: %+v", web.Activity)
	}
}

func TestTasksWebRetryBackoffClampedToBudget(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-budget", realmIntl, nil)
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.Method == http.MethodPost && r.URL.Path == "/console/as/conversations/" {
			return http.StatusServiceUnavailable, map[string]any{"code": 14220, "msg": "down"}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	var sleeps []time.Duration
	provider.tasksSleep = func(delay time.Duration) { sleeps = append(sleeps, delay) }
	provider.tasksBudget = 10 * time.Millisecond
	provider.tasksBackoff = 500 * time.Millisecond
	provider.tasksBackoffMax = time.Second
	_, _ = provider.DailyChatWeb(context.Background(), "intl-budget")
	if len(sleeps) == 0 {
		t.Fatal("no retry backoff observed")
	}
	for _, delay := range sleeps {
		if delay > 10*time.Millisecond {
			t.Fatalf("retry backoff %v exceeded budget", delay)
		}
	}
}

func TestTasksACPStatusTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/acp" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Acp-Connection-Id", "conn-terminal")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	result := runACPTurn(context.Background(),
		chatDoerFunc(func(ctx context.Context, req *http.Request, _ string) (*http.Response, error) {
			return http.DefaultClient.Do(req.WithContext(ctx))
		}),
		server.URL+"/acp", "sandbox-token", "session-1", "/workspace", "Hi", "test-agent", "model",
		"", 50*time.Millisecond, time.Millisecond,
		func() string { return "cancelled" })
	if result.OK || !result.Terminal || !strings.Contains(result.Error, "cancelled") {
		t.Fatalf("cancelled ACP turn = %+v", result)
	}
}

func TestTasksCachedSummaryNilMaps(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-nilsummary", realmCN, nil)
	_, server := newTaskTestUpstream(t, nil)
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-nilsummary")
	raw, _ := json.Marshal(growthSnapshot{Tasks: []GrowthTask{}})
	account.mu.Lock()
	account.Auth.Extra = map[string]json.RawMessage{"growthTasks": raw}
	account.mu.Unlock()
	snapshot, err := provider.GrowthSnapshot(context.Background(), "cn-nilsummary")
	if err != nil {
		t.Fatalf("GrowthSnapshot: %v", err)
	}
	if snapshot.Summary.Tiers == nil || snapshot.Summary.TierStatuses == nil || snapshot.Summary.Travel == nil {
		t.Fatalf("cached summary nil maps: %+v", snapshot.Summary)
	}
	if snapshot.Summary.TierStatuses["7d"] != "locked" || snapshot.Summary.Travel["state"] != "unknown" {
		t.Fatalf("cached summary defaults = %+v", snapshot.Summary)
	}
}

func TestTasksEnsureBuddyPersistsPartialAttempt(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-partial-buddy", realmCN, nil)
	buddyStatus := "not_accepted"
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case "/activity/growth/buddy/travel/status":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"state": "idle"}}
		case "/activity/growth/buddy/travel/config":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"locations": []any{map[string]any{"id": 1, "name": "咖啡馆"}},
			}}
		case "/activity/growth/buddy/travel/depart":
			return http.StatusBadRequest, map[string]any{"code": 400, "msg": "no active buddy"}
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
			return http.StatusOK, map[string]any{"code": 0}
		case "/activity/growth/tasks/first_buddy/claim":
			return http.StatusInternalServerError, map[string]any{"code": 1, "msg": "claim down"}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.CatTravel(context.Background(), "cn-partial-buddy")
	if err != nil {
		t.Fatalf("CatTravel: %v", err)
	}
	if result.OK {
		t.Fatalf("CatTravel unexpectedly succeeded without Buddy: %+v", result)
	}
	account := accountByUID(t, pool, "cn-partial-buddy")
	account.mu.RLock()
	raw, exists := authExtraJSON(account.Auth, "growthTasks")
	account.mu.RUnlock()
	if !exists {
		t.Fatal("ensureBuddy did not persist partial task state")
	}
	var snapshot growthSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode partial snapshot: %v", err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].TaskCode != "first_buddy" ||
		snapshot.Tasks[0].Status != "completed" {
		t.Fatalf("partial snapshot = %+v", snapshot.Tasks)
	}
}

func TestTasksACPStatusFailedErrorRetryable(t *testing.T) {
	for _, status := range []string{"failed", "error"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/acp" {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Acp-Connection-Id", "conn-"+status)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}))
		result := runACPTurn(context.Background(),
			chatDoerFunc(func(ctx context.Context, req *http.Request, _ string) (*http.Response, error) {
				return http.DefaultClient.Do(req.WithContext(ctx))
			}),
			server.URL+"/acp", "sandbox-token", "session-1", "/workspace", "Hi", "test-agent", "model",
			"", 50*time.Millisecond, time.Millisecond,
			func() string { return status })
		server.Close()
		if result.OK || result.Terminal || !strings.Contains(result.Error, status) {
			t.Fatalf("%s ACP turn = %+v, want retryable non-terminal failure", status, result)
		}
	}
}

func TestTasksDailyEventPrefersLaterSuccess(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-eventretry", realmIntl, nil)
	fail := true
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == chatPath {
			if fail {
				return http.StatusInternalServerError, map[string]any{"code": 1, "msg": "chat down"}
			}
			return http.StatusOK, map[string]any{"choices": []any{}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmIntl, dir, server.URL)
	first, err := provider.DailyChat(context.Background(), "intl-eventretry")
	if err != nil || first.OK || first.Activity == nil || first.Activity.OK {
		t.Fatalf("first failed daily = %+v, %v", first, err)
	}
	fail = false
	second, err := provider.DailyChat(context.Background(), "intl-eventretry")
	if err != nil || !second.OK || second.Activity == nil || !second.Activity.OK {
		t.Fatalf("later successful daily = %+v, %v", second, err)
	}
	report, err := provider.ReportActivity(context.Background(), "intl-eventretry")
	if err != nil || !report.OK {
		t.Fatalf("ReportActivity = %+v, %v", report, err)
	}
	if report.Activity != nil {
		t.Fatalf("duplicate event after preferred success row: %+v", report.Activity)
	}
}

func TestTasksClearRestrictionPreservesUnrelatedState(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-clearpreserve", realmCN, map[string]any{
		"restrictedUntil": float64(time.Now().Add(time.Hour).Unix()),
	})
	_, server := newTaskTestUpstream(t, nil)
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-clearpreserve")
	modelUntil := time.Now().Add(30 * time.Minute)
	account.mu.Lock()
	account.State.CoolUntil = time.Now().Add(2 * time.Hour)
	account.State.Disabled = true
	account.State.ManualDisabled = true
	account.modelCoolUntil = map[string]time.Time{"deepseek-test": modelUntil}
	account.mu.Unlock()
	account.SetTokenExpired(true)
	provider.clearTaskRestriction(account)
	account.mu.RLock()
	until := taskRestrictedUntil(account.Auth)
	coolUntil := account.State.CoolUntil
	disabled := account.State.Disabled
	manualDisabled := account.State.ManualDisabled
	modelWindow := account.modelCoolUntil["deepseek-test"]
	tokenExpired := account.tokenExpired
	account.mu.RUnlock()
	if until != 0 || !coolUntil.IsZero() {
		t.Fatalf("restriction not cleared: extra=%v runtime=%v", until, coolUntil)
	}
	if !disabled || !manualDisabled || !tokenExpired {
		t.Fatalf("unrelated account flags changed: disabled=%v manual=%v token=%v", disabled, manualDisabled, tokenExpired)
	}
	if !modelWindow.Equal(modelUntil) {
		t.Fatalf("model window changed: %v want %v", modelWindow, modelUntil)
	}
}

func TestTasksMPReachedTargetAutoClaimOffKeepsStatus(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-mpreached", realmCN, nil)
	reported := false
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		mp := strings.EqualFold(r.Header.Get("X-Client-Platform"), "miniprogram") ||
			strings.EqualFold(r.Header.Get("X-Client-Platform"), "mp-weixin")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && !mp:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "first_buddy", "task_type": "auto", "accept_status": "claimed",
					"progress": map[string]any{"current": 1, "target": 1},
				}},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks" && mp:
			status, current := "accepted", 0
			if reported {
				status, current = "completed", 1
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "Sequential_Tasks_1", "accept_status": status,
					"progress": map[string]any{"current": current, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			reported = true
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-mpreached", false)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	claimable := false
	for _, code := range result.Claimable {
		if code == "Sequential_Tasks_1" {
			claimable = true
		}
	}
	if !claimable {
		t.Fatalf("reached MP target not claimable: %+v", result.Claimable)
	}
	status := ""
	for _, task := range result.Tasks {
		if task.TaskCode == "Sequential_Tasks_1" {
			status = task.Status
		}
	}
	if status != "completed" {
		t.Fatalf("reached MP task status reverted to %q, tasks=%+v", status, result.Tasks)
	}
}
