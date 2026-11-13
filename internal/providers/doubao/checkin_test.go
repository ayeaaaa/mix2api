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
