package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func TestParseCheckinDetail(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"progress": map[string]any{"total_amount": float64(500)},
			"tasks": []any{map[string]any{
				"task_kind": "daily_signin",
				"daily_checkin": map[string]any{
					"checked_in_today":  false,
					"continuous_days":   float64(2),
					"total_days":        float64(2),
					"last_checkin_date": "2026-11-11",
					"reward":            map[string]any{"reward_amount": float64(200)},
				},
			}},
		},
	}
	status, _ := parseCheckinDetail(payload)
	if status.CheckedInToday || !status.HasSigninTask || status.ContinuousDays != 2 || status.TotalDays != 2 || status.RewardAmount != 200 || status.TotalAmount != 500 {
		t.Fatalf("status = %#v", status)
	}
}

func TestCheckinSlotUsesShanghaiTime(t *testing.T) {
	// 01:14 UTC = 09:14 CST: not due; 01:15 UTC = 09:15 CST: due.
	if _, ok := checkinSlot(time.Date(2026, 11, 12, 1, 14, 0, 0, time.UTC), "09:15"); ok {
		t.Fatal("slot was due before 09:15 Asia/Shanghai")
	}
	slot, ok := checkinSlot(time.Date(2026, 11, 12, 1, 15, 0, 0, time.UTC), "09:15")
	if !ok || slot.Format("2006-01-02 15:04") != "2026-11-12 09:15" {
		t.Fatalf("slot = %v, ok=%v", slot, ok)
	}
}

func TestCheckinProbeOverridesQueryAndUA(t *testing.T) {
	var gotQuery, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotUA = r.URL.RawQuery, r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"progress":{"total_amount":0},"tasks":[{"task_kind":"daily_signin","daily_checkin":{"checked_in_today":true}}]}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.pool.Add(map[string]string{"sessionid": "s"}, nil, "uid-probe", "probe", true)
	if err != nil {
		t.Fatal(err)
	}
	provider.cfgMu.Lock()
	provider.cfg.BaseURL = server.URL
	provider.cfgMu.Unlock()
	if err := provider.pool.SetCheckinProbe(account.UID, "?device_id=d&a_bogus=abc", "SamanthaDoubao/test"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CheckinStatus(context.Background(), account.UID); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "device_id=d&a_bogus=abc" || gotUA != "SamanthaDoubao/test" {
		t.Fatalf("query=%q ua=%q", gotQuery, gotUA)
	}
	view := account.view()
	if view["checkin_probe"] != true || strings.Contains(fmt.Sprint(view), "a_bogus") {
		t.Fatalf("view leaked probe or missed flag: %#v", view)
	}
}

func TestClaimCampaignTask(t *testing.T) {
	var detailCalls, claimCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case campaignDetailPath:
			detailCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"progress":{"total_amount":400},"tasks":[{"campaign_code":"select_work_fields","campaign_group_code":"pc_incentive_task_mvp_v3","task_status":2,"title":"选择工作领域领取奖励"}],"milestones":[]}}`))
		case campaignClaimPath:
			claimCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["campaign_item_code"] != "select_work_fields" || body["campaign_group_code"] != "pc_incentive_task_mvp_v3" || body["task_id"] != "" {
				t.Fatalf("claim body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_status":3,"reward_amount":100}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.pool.Add(map[string]string{"sessionid": "s"}, nil, "uid-claim", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	provider.cfgMu.Lock()
	provider.cfg.BaseURL = server.URL
	provider.cfgMu.Unlock()

	result, err := provider.ClaimTask(context.Background(), account.UID, "select_work_fields")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || result.Reward != 100 || detailCalls != 1 || claimCalls != 1 {
		t.Fatalf("result=%#v detailCalls=%d claimCalls=%d", result, detailCalls, claimCalls)
	}
	account.mu.Lock()
	task, _ := campaignTaskByCode(account.Tasks, "select_work_fields")
	account.mu.Unlock()
	if task == nil || intValue(task["task_status"]) != 3 {
		t.Fatalf("cached task = %#v", task)
	}
}

func TestClaimCampaignTaskBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != campaignClaimPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"biz_error":{"code":2,"message":"当前暂不符合领取条件"},"task_status":0}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.pool.Add(map[string]string{"sessionid": "s"}, nil, "uid-claim-error", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	account.Tasks = map[string]any{"tasks": []any{map[string]any{
		"campaign_code": "select_work_fields", "campaign_group_code": campaignScene,
		"task_status": float64(2), "title": "选择工作领域领取奖励",
	}}}
	provider.cfgMu.Lock()
	provider.cfg.BaseURL = server.URL
	provider.cfgMu.Unlock()

	_, err = provider.ClaimTask(context.Background(), account.UID, "select_work_fields")
	if err == nil || !strings.Contains(err.Error(), "当前暂不符合领取条件") {
		t.Fatalf("err = %v", err)
	}
}

func TestCampaignCheckinAndCredits(t *testing.T) {
	var detailCalls, checkinCalls, creditCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("aid") != campaignAID {
			t.Errorf("aid = %q", r.URL.Query().Get("aid"))
		}
		switch r.URL.Path {
		case campaignDetailPath:
			detailCalls++
			if !strings.Contains(r.Header.Get("User-Agent"), "SamanthaDoubao") {
				t.Errorf("detail UA = %q", r.Header.Get("User-Agent"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"progress":{"total_amount":500},"tasks":[{"task_kind":"daily_signin","daily_checkin":{"checked_in_today":false,"continuous_days":2,"total_days":2,"last_checkin_date":"2026-11-11","reward":{"reward_amount":200}}}],"milestones":[]}}`))
		case campaignCheckinPath:
			checkinCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["scene"] != campaignScene {
				t.Errorf("checkin body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":true,"reward_amount":400,"continuous_days":3,"checkin_date":"2026-11-12"}}`))
		case "/commerce/benefit_supply/credit/get_credit_num_optional_tasks":
			creditCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit_info":{"total_credit_num":900,"credit_text":"积分"},"optional_tasks":[{"task_type":2,"title":"每日登录","button_status":1,"button_text":"待领取"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.pool.Add(map[string]string{"sessionid": "s", "ttwid": "t"}, map[string]string{"web_id": "w"}, "uid-checkin", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	provider.cfgMu.Lock()
	provider.cfg.BaseURL = server.URL
	provider.cfgMu.Unlock()

	status, err := provider.CheckinStatus(context.Background(), account.UID)
	if err != nil {
		t.Fatal(err)
	}
	if status.CheckedInToday || status.TotalAmount != 500 || detailCalls != 1 {
		t.Fatalf("status=%#v detailCalls=%d", status, detailCalls)
	}
	result, err := provider.Checkin(context.Background(), account.UID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || result.Reward != 400 || result.ContinuousDays != 3 || checkinCalls != 1 {
		t.Fatalf("result=%#v checkinCalls=%d", result, checkinCalls)
	}
	if account.Credits["checked_in_today"] != true || account.LastCheckin != "2026-11-12" {
		t.Fatalf("cached credits=%#v last=%q", account.Credits, account.LastCheckin)
	}
	if _, err := provider.RefreshCredits(context.Background(), account.UID); err != nil {
		t.Fatal(err)
	}
	if creditCalls != 1 || account.Credits["total_credit_num"] != float64(900) {
		t.Fatalf("creditCalls=%d credits=%#v", creditCalls, account.Credits)
	}
}

func TestCheckinJobRetriesFailuresSameDay(t *testing.T) {
	checkinCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case campaignDetailPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"progress":{"total_amount":0},"tasks":[{"task_kind":"daily_signin","daily_checkin":{"checked_in_today":false}}]}}`))
		case campaignCheckinPath:
			checkinCalls++
			if checkinCalls == 1 {
				_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":false}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":true,"reward_amount":100,"continuous_days":1,"checkin_date":"2026-11-12"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := SaveConfig(store, map[string]any{
		"enabled": true, "checkin_enabled": true, "checkin_time": "09:15",
		"base_url": server.URL,
	}); err != nil {
		t.Fatal(err)
	}
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.pool.Add(map[string]string{"sessionid": "s"}, nil, "uid-retry", "retry", true); err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 11, 12, 9, 15, 0, 0, zone)
	provider.SetNow(func() time.Time { return now })
	job := &checkinJob{provider: provider}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("first checkin: %v", err)
	}
	if checkinCalls != 1 || job.retryAt.IsZero() {
		t.Fatalf("first checkin calls=%d retryAt=%v, want one failure and a retry", checkinCalls, job.retryAt)
	}

	now = time.Date(2026, 11, 12, 9, 44, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("early retry tick: %v", err)
	}
	if checkinCalls != 1 {
		t.Fatalf("retry ran before the interval: calls=%d", checkinCalls)
	}

	now = time.Date(2026, 11, 12, 9, 45, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("retry checkin: %v", err)
	}
	if checkinCalls != 2 || !job.retryAt.IsZero() {
		t.Fatalf("retry checkin calls=%d retryAt=%v, want success and cleared retry", checkinCalls, job.retryAt)
	}

	now = time.Date(2026, 11, 12, 9, 46, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("post-success tick: %v", err)
	}
	if checkinCalls != 2 {
		t.Fatalf("successful day repeated the checkin: calls=%d", checkinCalls)
	}
}

func TestCheckinJobRetryIntervalSetting(t *testing.T) {
	checkinCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case campaignDetailPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"progress":{"total_amount":0},"tasks":[{"task_kind":"daily_signin","daily_checkin":{"checked_in_today":false}}]}}`))
		case campaignCheckinPath:
			checkinCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := SaveConfig(store, map[string]any{
		"enabled": true, "checkin_enabled": true, "checkin_time": "09:15",
		"checkin_retry_minutes": 45, "base_url": server.URL,
	}); err != nil {
		t.Fatal(err)
	}
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.pool.Add(map[string]string{"sessionid": "s"}, nil, "uid-retry-interval", "retry", true); err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 11, 12, 9, 15, 0, 0, zone)
	provider.SetNow(func() time.Time { return now })
	job := &checkinJob{provider: provider}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("first checkin: %v", err)
	}
	if checkinCalls != 1 || !job.retryAt.Equal(time.Date(2026, 11, 12, 10, 0, 0, 0, zone)) {
		t.Fatalf("first checkin calls=%d retryAt=%v, want 10:00", checkinCalls, job.retryAt)
	}

	now = time.Date(2026, 11, 12, 9, 59, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("early tick: %v", err)
	}
	if checkinCalls != 1 {
		t.Fatalf("retry ran before 10:00: calls=%d", checkinCalls)
	}

	now = time.Date(2026, 11, 12, 10, 0, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if checkinCalls != 2 || !job.retryAt.Equal(time.Date(2026, 11, 12, 10, 45, 0, 0, zone)) {
		t.Fatalf("retry calls=%d retryAt=%v, want 10:45", checkinCalls, job.retryAt)
	}

	if _, err := SaveConfig(store, map[string]any{"checkin_retry_minutes": 0}); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 11, 12, 10, 45, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("disabled retry: %v", err)
	}
	if checkinCalls != 3 || !job.retryAt.IsZero() || job.lastKey == "" {
		t.Fatalf("disabled retry calls=%d retryAt=%v lastKey=%q, want exhausted day", checkinCalls, job.retryAt, job.lastKey)
	}
	now = time.Date(2026, 11, 12, 10, 46, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("post-disabled tick: %v", err)
	}
	if checkinCalls != 3 {
		t.Fatalf("disabled retry repeated: calls=%d", checkinCalls)
	}
}

func TestCheckinJobHourlyCatchUp(t *testing.T) {
	checkinCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case campaignDetailPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"progress":{"total_amount":0},"tasks":[{"task_kind":"daily_signin","daily_checkin":{"checked_in_today":false}}]}}`))
		case campaignCheckinPath:
			checkinCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := SaveConfig(store, map[string]any{
		"enabled": true, "checkin_enabled": true, "checkin_time": "09:15", "base_url": server.URL,
	}); err != nil {
		t.Fatal(err)
	}
	provider, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.pool.Add(map[string]string{"sessionid": "s"}, nil, "uid-first", "first", true); err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 11, 12, 9, 15, 0, 0, zone)
	provider.SetNow(func() time.Time { return now })
	job := &checkinJob{provider: provider}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("initial checkin: %v", err)
	}
	first := provider.pool.account("uid-first")
	first.mu.Lock()
	first.LastCheckin = now.Format("2006-01-02")
	first.mu.Unlock()

	if _, err := provider.pool.Add(map[string]string{"sessionid": "s"}, nil, "uid-late", "late", true); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 11, 12, 9, 30, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("same-hour tick: %v", err)
	}
	if checkinCalls != 1 {
		t.Fatalf("same-hour catch-up ran: calls=%d", checkinCalls)
	}
	now = time.Date(2026, 11, 12, 10, 0, 0, 0, zone)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("catch-up checkin: %v", err)
	}
	if checkinCalls != 2 {
		t.Fatalf("catch-up calls=%d, want 2", checkinCalls)
	}
}
