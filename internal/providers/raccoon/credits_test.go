package raccoon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// pointsTestNow is a deterministic local-noon clock used by every points test.
func pointsTestNow() time.Time {
	return time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
}

func fakePointsJWT(uid string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"sub":` + strconv.Quote(uid) + `,"exp":` + strconv.FormatInt(expiry.Unix(), 10) + `}`,
	))
	return header + "." + payload + ".signature"
}

func testInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0
		}
		return int(number)
	case string:
		var number float64
		if err := json.Unmarshal([]byte(typed), &number); err == nil {
			return int(number)
		}
		return 0
	case bool:
		if typed {
			return 1
		}
	}
	return 0
}

func fakeBearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) >= 7 && strings.EqualFold(value[:7], "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}

// pointsFake mirrors the v1 _fake_raccoon.py points subset: balance, bills,
// tasks, desktop/mobile grants, and refresh. No test reaches a real upstream.
type pointsFake struct {
	mu  sync.Mutex
	now time.Time

	allowed       map[string]bool
	refreshExpiry time.Time

	refreshCalls     int
	balanceCalls     int
	billsCalls       int
	tasksCalls       int
	desktopGrants    int
	mobileGrants     int
	refreshStatus    int
	balanceStatus    int
	billsStatus      int
	tasksStatus      int
	desktopStatus    int
	mobileStatus     int
	balanceByToken   map[string]int
	desktopGranted   bool
	mobileGranted    bool
	grantPoints      int
	mobileGrantPoint int

	balance       map[string]any
	billsByCursor map[string]map[string]any
	billQueries   []url.Values

	tasks map[string]any

	lastDesktopHeaders http.Header
	lastMobileHeaders  http.Header

	chatCalls        int
	dailyGrantOnChat bool
}

func newPointsFake(now time.Time) *pointsFake {
	return &pointsFake{
		now:              now,
		allowed:          make(map[string]bool),
		refreshExpiry:    now.Add(time.Hour),
		grantPoints:      3000,
		mobileGrantPoint: 3000,
		balanceByToken:   make(map[string]int),
		balance: map[string]any{
			"code":    0,
			"message": "success",
			"data": map[string]any{
				"available_points":        1234,
				"daily_points":            234,
				"monthly_points":          0,
				"reward_points":           1000,
				"topup_points":            0,
				"enterprise_topup_points": 0,
				"total_chargeable_points": 1234,
				"topup_frozen":            false,
				"upstream_balance_id":     "balance-upstream-id",
			},
		},
		billsByCursor: map[string]map[string]any{"": defaultFakeBills(now)},
		tasks:         defaultFakeTasks(),
	}
}

func defaultFakeTasks() map[string]any {
	return map[string]any{
		"code":    0,
		"message": "success",
		"data": map[string]any{
			"invite": map[string]any{
				"granted_points":            0,
				"invite_code":               "fake-invite-code",
				"marketing_text":            "",
				"register_reward_points":    1500,
				"registered_invitee_count":  0,
				"remaining_reward_invitees": 20,
			},
			"invitee_task_states": []any{},
			"new_user_task_states": []any{
				map[string]any{"key": "register_pack", "status": "completed"},
				map[string]any{"key": "desktop_login", "status": "unfinished"},
				map[string]any{"key": "mobile_login", "status": "unfinished"},
			},
		},
	}
}

func defaultFakeBills(now time.Time) map[string]any {
	when := func(days int) string {
		return now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	}
	return map[string]any{
		"code":    0,
		"message": "success",
		"data": map[string]any{
			"items": []any{
				map[string]any{"id": "bill-upstream-id", "event_name": "桌面端 LLM 对话消耗", "points": -1, "biz_type": "desktop", "client_type": "desktop", "created_at": when(0)},
				map[string]any{"event_name": "桌面端 LLM 对话消耗", "points": -2, "biz_type": "desktop", "client_type": "desktop", "created_at": when(0)},
				map[string]any{"event_name": "三天前对话消耗", "points": -4, "biz_type": "desktop", "client_type": "desktop", "created_at": when(3)},
				map[string]any{"event_name": "八天前对话消耗", "points": -5, "biz_type": "desktop", "client_type": "desktop", "created_at": when(8)},
				map[string]any{"event_name": "每日积分发放", "points": 300, "biz_type": "daily_grant", "client_type": "web", "created_at": when(0)},
				map[string]any{"event_name": "奖励积分", "points": 1000, "biz_type": "reward_grant", "client_type": "web", "created_at": when(2)},
			},
			"has_more":    false,
			"next_cursor": "",
		},
	}
}

func (f *pointsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == PointsBalancePath:
		f.serveBalance(w, r)
	case r.Method == http.MethodGet && r.URL.Path == PointsBillsPath:
		f.serveBills(w, r)
	case r.Method == http.MethodGet && r.URL.Path == PointsTasksPath:
		f.serveTasks(w, r)
	case r.Method == http.MethodPost && r.URL.Path == ChatPath:
		f.serveDailyActiveChat(w, r)
	case r.Method == http.MethodPost && r.URL.Path == PointsDesktopGrantPath:
		f.serveDesktopGrant(w, r)
	case r.Method == http.MethodPost && r.URL.Path == PointsMobileGrantPath:
		f.serveMobileGrant(w, r)
	case r.Method == http.MethodPost && r.URL.Path == RefreshPath:
		f.serveRefresh(w, r)
	default:
		writeCatalogJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
	}
}

func (f *pointsFake) serveDailyActiveChat(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.chatCalls++
	grant := f.dailyGrantOnChat
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
	if grant {
		f.mu.Lock()
		f.addDailyGrantBillLocked()
		f.mu.Unlock()
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func (f *pointsFake) addDailyGrantBillLocked() {
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
	items, _ := data["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item != nil && item["biz_type"] == "daily_grant" {
			return
		}
	}
	data["items"] = append(items, map[string]any{
		"event_name": "每日积分发放", "points": 300, "biz_type": "daily_grant",
		"client_type": "web", "created_at": f.now.Format(time.RFC3339),
	})
}
func (f *pointsFake) authStatusLocked(r *http.Request) (string, bool, int) {
	token := fakeBearerToken(r)
	authorized := f.allowed[token]
	status := f.balanceStatus
	if perToken, ok := f.balanceByToken[token]; ok {
		status = perToken
	}
	if status == 0 {
		status = http.StatusOK
	}
	return token, authorized, status
}

func (f *pointsFake) serveBalance(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.balanceCalls++
	token, authorized, status := f.authStatusLocked(r)
	_ = token
	payload := f.balance
	f.mu.Unlock()

	if !authorized {
		writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid access token"})
		return
	}
	if status != http.StatusOK {
		writeCatalogJSON(w, status, map[string]any{"message": "balance failed"})
		return
	}
	writeCatalogJSON(w, http.StatusOK, payload)
}

func (f *pointsFake) serveBills(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.billsCalls++
	f.billQueries = append(f.billQueries, r.URL.Query())
	authorized := f.allowed[fakeBearerToken(r)]
	status := f.billsStatus
	if status == 0 {
		status = http.StatusOK
	}
	payload := f.billsByCursor[r.URL.Query().Get("cursor")]
	if payload == nil {
		payload = f.billsByCursor[""]
	}
	f.mu.Unlock()

	if !authorized {
		writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid access token"})
		return
	}
	if status != http.StatusOK {
		writeCatalogJSON(w, status, map[string]any{"message": "bills failed"})
		return
	}
	writeCatalogJSON(w, http.StatusOK, payload)
}

func (f *pointsFake) serveTasks(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tasksCalls++
	authorized := f.allowed[fakeBearerToken(r)]
	status := f.tasksStatus
	if status == 0 {
		status = http.StatusOK
	}
	payload := f.tasks
	f.mu.Unlock()

	if !authorized {
		writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid access token"})
		return
	}
	if status != http.StatusOK {
		writeCatalogJSON(w, status, map[string]any{"message": "tasks failed"})
		return
	}
	writeCatalogJSON(w, http.StatusOK, payload)
}

func (f *pointsFake) serveDesktopGrant(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.desktopGrants++
	f.lastDesktopHeaders = r.Header.Clone()
	authorized := f.allowed[fakeBearerToken(r)]
	status := f.desktopStatus
	if status == 0 {
		status = http.StatusOK
	}
	if !authorized {
		f.mu.Unlock()
		writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid access token"})
		return
	}
	if status != http.StatusOK {
		f.mu.Unlock()
		writeCatalogJSON(w, status, map[string]any{"message": "desktop grant failed"})
		return
	}
	if f.desktopGranted {
		f.mu.Unlock()
		writeCatalogJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "success", "data": map[string]any{"granted": false},
		})
		return
	}
	f.desktopGranted = true
	points := f.grantPoints
	f.addBalanceLocked(points)
	f.markTaskLocked("desktop_login")
	f.mu.Unlock()
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"code": 0, "message": "success",
		"data": map[string]any{
			"granted": true,
			"popup": map[string]any{
				"source":       "desktop_login",
				"display_name": "桌面端登录奖励",
				"points":       points,
				"expire_at":    "2027-11-07T23:00:39+08:00",
			},
		},
	})
}

func (f *pointsFake) serveMobileGrant(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.mobileGrants++
	f.lastMobileHeaders = r.Header.Clone()
	authorized := f.allowed[fakeBearerToken(r)]
	status := f.mobileStatus
	if status == 0 {
		status = http.StatusOK
	}
	platformOK := r.Header.Get("X-Client-Platform") == "app-android" &&
		r.Header.Get("X-Client-Version") == "v1.1.0"
	if !authorized {
		f.mu.Unlock()
		writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid access token"})
		return
	}
	if !platformOK {
		f.mu.Unlock()
		writeCatalogJSON(w, http.StatusBadRequest, map[string]any{"message": "mobile client required"})
		return
	}
	if status != http.StatusOK {
		f.mu.Unlock()
		writeCatalogJSON(w, status, map[string]any{"message": "mobile grant failed"})
		return
	}
	if f.mobileGranted {
		f.mu.Unlock()
		writeCatalogJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "success", "data": map[string]any{"granted": false},
		})
		return
	}
	f.mobileGranted = true
	points := f.mobileGrantPoint
	f.addBalanceLocked(points)
	f.markTaskLocked("mobile_login")
	f.mu.Unlock()
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"code": 0, "message": "success",
		"data": map[string]any{
			"granted": true,
			"popup": map[string]any{
				"source":       "mobile_login",
				"display_name": "移动端登录奖励",
				"points":       points,
				"expire_at":    "2027-11-07T23:00:39+08:00",
			},
		},
	})
}

func (f *pointsFake) addBalanceLocked(points int) {
	data, _ := f.balance["data"].(map[string]any)
	if data == nil {
		return
	}
	data["available_points"] = testInt(data["available_points"]) + points
	data["reward_points"] = testInt(data["reward_points"]) + points
	data["total_chargeable_points"] = testInt(data["total_chargeable_points"]) + points
}

func (f *pointsFake) markTaskLocked(key string) {
	data, _ := f.tasks["data"].(map[string]any)
	list, _ := data["new_user_task_states"].([]any)
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		if item != nil && item["key"] == key {
			item["status"] = "completed"
		}
	}
}

func (f *pointsFake) serveRefresh(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	refreshToken, _ := body["refresh_token"].(string)

	f.mu.Lock()
	f.refreshCalls++
	status := f.refreshStatus
	if status == 0 {
		status = http.StatusOK
	}
	expiry := f.refreshExpiry
	f.mu.Unlock()

	if status != http.StatusOK {
		writeCatalogJSON(w, status, map[string]any{"message": "refresh rejected"})
		return
	}
	uid := strings.TrimPrefix(refreshToken, "refresh-")
	if uid == "" || uid == refreshToken {
		writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid refresh token"})
		return
	}
	token := fakePointsJWT(uid, expiry)
	f.mu.Lock()
	f.allowed[token] = true
	f.mu.Unlock()
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"data": map[string]any{
			"access_token":  token,
			"refresh_token": "rotated-" + uid,
			"token_type":    "Bearer",
			"expires_at":    expiry.Unix(),
		},
	})
}

func (f *pointsFake) counts() (balance, bills, tasks, desktop, mobile, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balanceCalls, f.billsCalls, f.tasksCalls,
		f.desktopGrants, f.mobileGrants, f.refreshCalls
}

func (f *pointsFake) update(fn func(*pointsFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *pointsFake) billQuerySnapshot() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]url.Values, len(f.billQueries))
	for index, values := range f.billQueries {
		copied := make(url.Values, len(values))
		for key, items := range values {
			copied[key] = append([]string(nil), items...)
		}
		out[index] = copied
	}
	return out
}

func (f *pointsFake) header(name, which string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if which == "mobile" {
		return f.lastMobileHeaders.Get(name)
	}
	return f.lastDesktopHeaders.Get(name)
}

// pointsFixture wires a real pool to the fake upstream. now is mutable so a
// test can advance the provider clock without sleeping.
type pointsFixture struct {
	p      *Pool
	fake   *pointsFake
	server *httptest.Server
	now    time.Time
	uid    string
}

func newPointsFixture(t *testing.T, uids ...string) *pointsFixture {
	t.Helper()
	if len(uids) == 0 {
		uids = []string{"pu-points"}
	}
	fixture := &pointsFixture{now: pointsTestNow(), uid: uids[0]}
	dir := t.TempDir()
	for _, uid := range uids {
		writePoolAuth(t, dir, uid, "", fixture.now.Add(time.Hour), nil)
	}
	fixture.p = mustPool(t, dir, Config{MaxConcurrent: 1})
	fixture.p.now = func() time.Time { return fixture.now }
	fixture.fake = newPointsFake(fixture.now)
	fixture.server = httptest.NewServer(fixture.fake)
	t.Cleanup(fixture.server.Close)

	for _, uid := range uids {
		account := fixture.p.Get(uid)
		account.mu.Lock()
		token := account.Auth.AccessToken
		account.mu.Unlock()
		fixture.fake.allowed[token] = true
	}

	client := transport.NewClient(transport.ClientOptions{})
	fixture.p.SetHTTPClient(NewHTTPClient(client, HTTPClientOptions{BaseURL: fixture.server.URL}))
	fixture.p.SetTokenManager(fixture.p.NewTokenManager(client, TokenManagerOptions{
		BaseURL: fixture.server.URL,
		Now:     func() time.Time { return fixture.now },
		Timeout: 2 * time.Second,
	}))
	return fixture
}

func (f *pointsFixture) account(t *testing.T) *Account {
	t.Helper()
	account := f.p.Get(f.uid)
	if account == nil {
		t.Fatalf("account %q not found", f.uid)
	}
	return account
}

func TestCreditsSnapshotAndBills(t *testing.T) {
	fixture := newPointsFixture(t, "u-credits")
	account := fixture.account(t)

	snapshot, err := fixture.p.FetchCredits(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchCredits: %v", err)
	}
	want := func(label string, value any, expected int) {
		t.Helper()
		if got := testInt(value); got != expected {
			t.Fatalf("%s = %v (%d), want %d", label, value, got, expected)
		}
	}
	want("available", snapshot.Available, 1234)
	want("remain", snapshot.Remain, 1234)
	want("amount", snapshot.Amount, 1246)
	want("used", snapshot.Used, 12)
	want("daily", snapshot.Daily, 234)
	want("monthly", snapshot.Monthly, 0)
	want("reward", snapshot.Reward, 1000)
	want("topup", snapshot.Topup, 0)
	want("enterprise_topup", snapshot.EnterpriseTopup, 0)
	want("total", snapshot.Total, 1234)
	want("today_used", snapshot.TodayUsed, 3)
	want("week_used", snapshot.WeekUsed, 7)
	if snapshot.Unit != "points" {
		t.Fatalf("unit = %v, want points", snapshot.Unit)
	}
	if len(snapshot.Recent) != 6 {
		t.Fatalf("recent = %d items, want 6: %#v", len(snapshot.Recent), snapshot.Recent)
	}
	fetched, ok := numericValue(snapshot.FetchedAt)
	if !ok || math.Abs(fetched-float64(fixture.now.Unix())) > 1 {
		t.Fatalf("fetched_at = %v, want near %d", snapshot.FetchedAt, fixture.now.Unix())
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"balance-upstream-id", "bill-upstream-id", "access_token", "refresh-u-credits"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("credits leaked %q: %s", forbidden, text)
		}
	}

	balanceCalls, billsCalls, _, _, _, _ := fixture.fake.counts()
	if balanceCalls != 1 || billsCalls != 1 {
		t.Fatalf("calls = balance %d bills %d, want 1/1", balanceCalls, billsCalls)
	}

	cached, err := fixture.p.FetchCredits(context.Background(), account, false)
	if err != nil {
		t.Fatalf("cached FetchCredits: %v", err)
	}
	if got := testInt(cached.Available); got != 1234 {
		t.Fatalf("cached available = %d, want 1234", got)
	}
	balanceCalls, billsCalls, _, _, _, _ = fixture.fake.counts()
	if balanceCalls != 1 || billsCalls != 1 {
		t.Fatalf("cache hit made upstream calls: balance %d bills %d", balanceCalls, billsCalls)
	}

	fixture.now = fixture.now.Add(60 * time.Second)
	if _, err := fixture.p.FetchCredits(context.Background(), account, false); err != nil {
		t.Fatalf("expired FetchCredits: %v", err)
	}
	balanceCalls, billsCalls, _, _, _, _ = fixture.fake.counts()
	if balanceCalls != 2 || billsCalls != 2 {
		t.Fatalf("expired cache calls = balance %d bills %d, want 2/2", balanceCalls, billsCalls)
	}

	view := account.View()
	if view.Credits == nil || testInt(view.Credits.Available) != 1234 {
		t.Fatalf("view credits = %#v", view.Credits)
	}
}

func TestCreditsBillsPaginationStopsAtThreePages(t *testing.T) {
	fixture := newPointsFixture(t, "u-pages")
	account := fixture.account(t)

	page := func(points int, cursor string, hasMore bool) map[string]any {
		return map[string]any{
			"code": 0,
			"data": map[string]any{
				"items": []any{
					map[string]any{
						"event_name":  "page spend",
						"points":      points,
						"biz_type":    "desktop",
						"client_type": "desktop",
						"created_at":  fixture.now.Format(time.RFC3339),
						"id":          "hidden-" + cursor,
					},
				},
				"next_cursor": cursor,
				"has_more":    hasMore,
			},
		}
	}
	fixture.fake.update(func(fake *pointsFake) {
		fake.billsByCursor = map[string]map[string]any{
			"":   page(-10, "c1", true),
			"c1": page(-20, "c2", true),
			"c2": page(-30, "c3", true),
			"c3": page(-40, "", false),
		}
	})

	snapshot, err := fixture.p.FetchCredits(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchCredits: %v", err)
	}
	if got := testInt(snapshot.Used); got != 60 {
		t.Fatalf("used = %d, want 60 (three pages, never the fourth)", got)
	}
	if _, bills, _, _, _, _ := fixture.fake.counts(); bills != 3 {
		t.Fatalf("bills calls = %d, want 3 (max pages)", bills)
	}
	queries := fixture.fake.billQuerySnapshot()
	if len(queries) != 3 {
		t.Fatalf("recorded queries = %d, want 3", len(queries))
	}
	wantCursors := []string{"", "c1", "c2"}
	for index, query := range queries {
		if query.Get("paging.limit") != "100" || query.Get("type") != "all" {
			t.Fatalf("bills query %d = %v, want paging.limit=100&type=all", index, query)
		}
		if got := query.Get("cursor"); got != wantCursors[index] {
			t.Fatalf("bills cursor %d = %q, want %q", index, got, wantCursors[index])
		}
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "hidden-") {
		t.Fatalf("credits leaked a bill id: %s", encoded)
	}
}

func TestCredits401RefreshesOnce(t *testing.T) {
	fixture := newPointsFixture(t, "u-credits-401")
	account := fixture.account(t)
	fixture.fake.update(func(fake *pointsFake) {
		fake.allowed = make(map[string]bool)
	})

	snapshot, err := fixture.p.FetchCredits(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchCredits after one 401: %v", err)
	}
	if got := testInt(snapshot.Available); got != 1234 {
		t.Fatalf("available = %d, want 1234", got)
	}
	balanceCalls, _, _, _, _, refreshCalls := fixture.fake.counts()
	if balanceCalls != 2 {
		t.Fatalf("balance calls = %d, want 2 (rejected + retry)", balanceCalls)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls)
	}

	account.mu.Lock()
	rotated := account.Auth.AccessToken
	account.mu.Unlock()
	if rotated == "" || !fixture.fakeAllowed(rotated) {
		t.Fatalf("account token was not rotated: %q", rotated)
	}
}

func TestPointsEndpointPaths(t *testing.T) {
	paths := map[string]string{
		PointsBalancePath:      "/api/web/points/v1/balance",
		PointsBillsPath:        "/api/web/points/v2/bills",
		PointsTasksPath:        "/api/web/points/v1/tasks",
		PointsDesktopGrantPath: "/api/web/desktop/v1/login/points/grant",
		PointsMobileGrantPath:  "/api/web/mobile/v1/login/points/grant",
	}
	for got, want := range paths {
		if got != want {
			t.Fatalf("endpoint = %q, want %q", got, want)
		}
	}
	if MobileClientPlatform != "app-android" || MobileClientVersion != "v1.1.0" {
		t.Fatalf("mobile identity = %q/%q, want app-android/v1.1.0",
			MobileClientPlatform, MobileClientVersion)
	}
}

func (f *pointsFixture) fakeAllowed(token string) bool {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	return f.fake.allowed[token]
}

func TestCreditsAuthFailureIsTerminalAfterRefresh(t *testing.T) {
	fixture := newPointsFixture(t, "u-credits-terminal")
	account := fixture.account(t)
	fixture.fake.update(func(fake *pointsFake) {
		fake.allowed = make(map[string]bool)
		fake.refreshStatus = http.StatusUnauthorized
	})

	_, err := fixture.p.FetchCredits(context.Background(), account, true)
	if err == nil {
		t.Fatal("FetchCredits should fail when the forced refresh is rejected")
	}
	account.mu.Lock()
	token := account.Auth.AccessToken
	account.mu.Unlock()
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked access token: %v", err)
	}
	balanceCalls, _, _, _, _, refreshCalls := fixture.fake.counts()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if balanceCalls != 1 {
		t.Fatalf("balance calls = %d, want 1 (terminal, no retry)", balanceCalls)
	}
}

func TestCreditsConcurrentCacheMissSingleFlight(t *testing.T) {
	fixture := newPointsFixture(t, "u-credits-flight")
	account := fixture.account(t)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fixture.p.FetchCredits(context.Background(), account, false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent FetchCredits: %v", err)
		}
	}
	balanceCalls, billsCalls, _, _, _, _ := fixture.fake.counts()
	if balanceCalls != 1 || billsCalls != 1 {
		t.Fatalf("concurrent cache misses made %d balance / %d bills calls, want 1/1", balanceCalls, billsCalls)
	}
}

func TestRefreshCreditsForcesOneAccount(t *testing.T) {
	fixture := newPointsFixture(t, "u-one", "u-two")
	account := fixture.p.Get("u-one")
	if _, err := fixture.p.FetchCredits(context.Background(), account, false); err != nil {
		t.Fatalf("initial FetchCredits: %v", err)
	}
	if balance, _, _, _, _, _ := fixture.fake.counts(); balance != 1 {
		t.Fatalf("initial balance calls = %d, want 1", balance)
	}

	results, err := fixture.p.RefreshCredits(context.Background(), "u-one")
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if len(results) != 1 || results[0].UID != "u-one" || !results[0].OK {
		t.Fatalf("targeted refresh = %+v", results)
	}
	if balance, _, _, _, _, _ := fixture.fake.counts(); balance != 2 {
		t.Fatalf("targeted refresh did not bypass cache: balance calls = %d, want 2", balance)
	}
}

func TestRefreshCreditsAggregatesPerAccountErrors(t *testing.T) {
	fixture := newPointsFixture(t, "u-good", "u-bad")
	good := fixture.p.Get("u-good")
	bad := fixture.p.Get("u-bad")
	good.mu.Lock()
	goodToken := good.Auth.AccessToken
	good.mu.Unlock()
	bad.mu.Lock()
	badToken := bad.Auth.AccessToken
	bad.mu.Unlock()
	fixture.fake.update(func(fake *pointsFake) {
		fake.balanceByToken[badToken] = http.StatusInternalServerError
		_ = goodToken
	})

	results, err := fixture.p.RefreshCredits(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(results), results)
	}
	byUID := make(map[string]CreditsResult, len(results))
	for _, result := range results {
		byUID[result.UID] = result
	}
	if good := byUID["u-good"]; !good.OK || good.Credits == nil || testInt(good.Credits.Available) != 1234 || good.Message != "" {
		t.Fatalf("good result = %+v", good)
	}
	if bad := byUID["u-bad"]; bad.OK || bad.Credits != nil || bad.Message == "" {
		t.Fatalf("bad result = %+v", bad)
	}

	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, token := range []string{goodToken, badToken, "refresh-u-good", "refresh-u-bad"} {
		if strings.Contains(text, token) {
			t.Fatalf("refresh results leaked token %q: %s", token, text)
		}
	}

	if _, err := fixture.p.RefreshCredits(context.Background(), "missing"); err != ErrAccountNotFound {
		t.Fatalf("missing account error = %v, want ErrAccountNotFound", err)
	}
}
