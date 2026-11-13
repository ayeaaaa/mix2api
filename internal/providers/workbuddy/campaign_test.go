package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Campaign fixtures
// ---------------------------------------------------------------------------

type campaignReportRecord struct {
	PageURL string
	Events  []map[string]any
}

type campaignFakeState struct {
	mu                  sync.Mutex
	progress            map[string]int
	claimed             map[string]bool
	reports             []campaignReportRecord
	claims              []string
	views               []string
	claimWithoutReceipt bool
}

func newCampaignFakeState() *campaignFakeState {
	return &campaignFakeState{progress: map[string]int{}, claimed: map[string]bool{}}
}

func campaignTaskDefinitions(activity string) []map[string]any {
	switch activity {
	case "campaign_starbucks_2026":
		return []map[string]any{
			{"task_code": "ai_chat", "target_count": 3, "reward_credit": 100, "need_view": true},
			{"task_code": "expert", "target_count": 1, "reward_credit": 100, "need_view": true},
			{"task_code": "app", "target_count": 1, "reward_credit": 100, "need_view": true},
		}
	case "campaign_reporter_2026":
		return []map[string]any{
			{"task_code": "expert_chat", "target_count": 1, "reward_credit": 50, "need_view": true},
			{"task_code": "skill_use", "target_count": 1, "reward_credit": 50, "need_view": true},
			{"task_code": "desktop_same", "target_count": 1, "reward_credit": 100, "need_view": true},
		}
	default:
		return nil
	}
}

func (s *campaignFakeState) campaignPayload(activity string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := campaignTaskDefinitions(activity)
	progressRows := make([]any, 0, len(tasks))
	for _, task := range tasks {
		code := task["task_code"].(string)
		target := task["target_count"].(int)
		current := s.progress[code]
		row := map[string]any{"task_code": code, "progress": current}
		if current >= target {
			row["completed_at"] = "2026-11-13T00:00:00Z"
		}
		if s.claimed[code] {
			row["claimed_at"] = "2026-11-13T00:00:01Z"
		}
		progressRows = append(progressRows, row)
	}
	return map[string]any{"code": 0, "data": map[string]any{
		"tasks": tasks, "progress": progressRows,
	}}
}

func (s *campaignFakeState) recordReport(events []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pageURL := ""
	if len(events) > 0 {
		pageURL, _ = events[0]["pageURL"].(string)
	}
	s.reports = append(s.reports, campaignReportRecord{PageURL: pageURL, Events: events})
	for _, event := range events {
		code, _ := event["eventCode"].(string)
		page, _ := event["pageURL"].(string)
		switch code {
		case "chat_request_send":
			switch {
			case strings.Contains(page, "starbucks") && creditText(event["expertId"]) == "":
				s.progress["ai_chat"]++
			case strings.Contains(page, "journalist") && creditText(event["skillId"]) != "":
				s.progress["skill_use"] = 1
			case strings.Contains(page, "journalist") && creditText(event["expertId"]) != "":
				s.progress["expert_chat"] = 1
			}
		case "expert_actual_use":
			if strings.Contains(page, "starbucks") {
				s.progress["expert"] = 1
			} else if strings.Contains(page, "journalist") {
				s.progress["expert_chat"] = 1
			}
		case "skill_info":
			if strings.Contains(page, "journalist") {
				s.progress["skill_use"] = 1
			}
		case "playbook_prompt_send":
			if creditText(event["id"]) == "cnb-content-calendar" {
				s.progress["desktop_same"] = 1
			}
		}
	}
}

func campaignTaskFromPath(path string) string {
	rest := strings.TrimPrefix(path, "/portal/activity/tasks/")
	if index := strings.IndexByte(rest, '/'); index >= 0 {
		return rest[:index]
	}
	return rest
}

func newCampaignFake(t *testing.T, state *campaignFakeState) *taskTestUpstream {
	t.Helper()
	fake, _ := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/portal/activity/tasks":
			return http.StatusOK, state.campaignPayload(r.URL.Query().Get("activity_code"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/viewed"):
			state.mu.Lock()
			state.views = append(state.views, campaignTaskFromPath(r.URL.Path))
			state.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			task := campaignTaskFromPath(r.URL.Path)
			state.mu.Lock()
			state.claims = append(state.claims, task)
			if !state.claimWithoutReceipt {
				state.claimed[task] = true
			}
			state.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			state.recordReport(events)
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/operation-platform/market/expert/list":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"experts": []any{map[string]any{
					"expert_id": "ex_campaign01", "display_name_zh": map[string]any{"zh": "活动专家"},
				}},
			}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	return fake
}

func campaignFakeServerURL(t *testing.T, fake *taskTestUpstream) string {
	t.Helper()
	return fakeServerURL(t, fake)
}

// ---------------------------------------------------------------------------
// Contract tests
// ---------------------------------------------------------------------------

func TestCampaignSpecsMatchPortalContract(t *testing.T) {
	codes := make([]string, 0, len(campaignSpecs))
	for _, spec := range campaignSpecs {
		codes = append(codes, spec.Code)
	}
	if strings.Join(codes, ",") != "campaign_starbucks_2026,campaign_reporter_2026" {
		t.Fatalf("campaign codes = %v", codes)
	}
	starbucks := campaignSpecs[0]
	starbucksTasks := make([]string, 0, len(starbucks.Tasks))
	for _, task := range starbucks.Tasks {
		starbucksTasks = append(starbucksTasks, task.Task)
	}
	if strings.Join(starbucksTasks, ",") != "ai_chat,expert,app" {
		t.Fatalf("starbucks tasks = %v", starbucksTasks)
	}
	reporter := campaignSpecs[1]
	if reporter.Tasks[0].ExpertID != "ex_NaiDbO06Adf3" {
		t.Fatalf("reporter expert = %q", reporter.Tasks[0].ExpertID)
	}
	if reporter.Tasks[1].SkillID != "skill_2117491159283752960" ||
		reporter.Tasks[1].SkillName != "interview-material-organizer" {
		t.Fatalf("reporter skill = %+v", reporter.Tasks[1])
	}
	if reporter.Tasks[2].CaseID != "content-calendar" {
		t.Fatalf("reporter desktop case = %q", reporter.Tasks[2].CaseID)
	}
	if campaignCases["content-calendar"].EventID != "cnb-content-calendar" {
		t.Fatalf("content-calendar event = %+v", campaignCases["content-calendar"])
	}
}

func TestCampaignFetchViewAndClaimRequireReceipt(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-campaign", realmCN, nil)
	state := newCampaignFakeState()
	fake := newCampaignFake(t, state)
	provider, pool := setupTaskProvider(t, realmCN, dir, campaignFakeServerURL(t, fake))
	account := accountByUID(t, pool, "cn-campaign")

	snapshot, err := provider.CampaignSnapshot(context.Background(), "cn-campaign")
	if err != nil {
		t.Fatalf("CampaignSnapshot: %v", err)
	}
	if !snapshot.OK || len(snapshot.Activities) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	var aiChat CampaignTaskStatus
	for _, activity := range snapshot.Activities {
		if activity.Code != "campaign_starbucks_2026" {
			continue
		}
		for _, task := range activity.Tasks {
			if task.TaskCode == "ai_chat" {
				aiChat = task
			}
		}
	}
	if !aiChat.Issued || aiChat.Target != 3 || aiChat.State != "not_started" {
		t.Fatalf("ai_chat status = %+v", aiChat)
	}
	if !provider.viewCampaignTask(context.Background(), account, "campaign_starbucks_2026", "ai_chat") {
		t.Fatal("viewCampaignTask returned false")
	}

	// A completed task is required before claim; an incomplete one is rejected
	// locally without a claim POST.
	claim, err := provider.CampaignClaim(context.Background(), "cn-campaign", "campaign_starbucks_2026", "ai_chat")
	if err != nil {
		t.Fatalf("CampaignClaim incomplete: %v", err)
	}
	if claim.OK || !strings.Contains(claim.Msg, "尚未完成") {
		t.Fatalf("incomplete claim = %+v", claim)
	}
	if len(state.claims) != 0 {
		t.Fatalf("claims = %v, want none", state.claims)
	}

	// A successful POST without claimed_at must not be reported as success.
	state.mu.Lock()
	state.progress["ai_chat"] = 3
	state.claimWithoutReceipt = true
	state.mu.Unlock()
	logs := []string{}
	ok := provider.claimCampaignTask(context.Background(), account,
		"campaign_starbucks_2026", "ai_chat", 100, 1, 0,
		func(line string) { logs = append(logs, line) })
	if ok {
		t.Fatal("claim without claimed_at reported success")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "claimed_at") {
		t.Fatalf("logs = %v, want claimed_at warning", logs)
	}
}

func TestCampaignStarbucksAndReporterAutomation(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-campaign-run", realmCN, nil)
	state := newCampaignFakeState()
	fake := newCampaignFake(t, state)
	provider, _ := setupTaskProvider(t, realmCN, dir, campaignFakeServerURL(t, fake))

	result, err := provider.CampaignTasks(context.Background(), "cn-campaign-run", true)
	if err != nil {
		t.Fatalf("CampaignTasks: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if result.EarnedCredit != 400 {
		t.Fatalf("earned = %g, want 400 (Starbucks 200 + Reporter 200; app is manual)", result.EarnedCredit)
	}
	state.mu.Lock()
	progress := map[string]int{}
	for key, value := range state.progress {
		progress[key] = value
	}
	claims := append([]string(nil), state.claims...)
	state.mu.Unlock()
	if _, forged := progress["app"]; forged {
		t.Fatalf("app task was forged: %+v", progress)
	}
	if progress["ai_chat"] != 3 || progress["expert"] != 1 ||
		progress["expert_chat"] != 1 || progress["skill_use"] != 1 || progress["desktop_same"] != 1 {
		t.Fatalf("progress = %+v", progress)
	}
	wantClaims := map[string]bool{"ai_chat": true, "expert": true, "expert_chat": true, "skill_use": true, "desktop_same": true}
	for _, code := range claims {
		if code == "app" {
			t.Fatal("app task was claimed")
		}
		if !wantClaims[code] {
			t.Fatalf("unexpected claim %q in %v", code, claims)
		}
	}
	if len(claims) != len(wantClaims) {
		t.Fatalf("claims = %v, want %d", claims, len(wantClaims))
	}
	logs := strings.Join(result.Logs, "\n")
	if !strings.Contains(logs, "手机端 App") {
		t.Fatalf("missing manual App reason: %s", logs)
	}
	appManual := false
	for _, activity := range result.Activities {
		for _, task := range activity.Tasks {
			if task.TaskCode == "app" {
				appManual = task.Manual && task.State == "manual" && strings.Contains(task.ManualReason, "手机端 App")
			}
		}
	}
	if !appManual {
		t.Fatalf("app task not reported as manual: %+v", result.Activities)
	}

	// The report stream must carry the real reporter ids and page context.
	var reporterExpert, reporterSkill, desktop bool
	for _, report := range state.reports {
		for _, event := range report.Events {
			switch creditText(event["eventCode"]) {
			case "expert_actual_use":
				if creditText(event["id"]) == "ex_NaiDbO06Adf3" &&
					strings.Contains(report.PageURL, "journalist") {
					reporterExpert = true
				}
			case "skill_info":
				if creditText(event["skillId"]) == "skill_2117491159283752960" &&
					strings.Contains(report.PageURL, "journalist") {
					reporterSkill = true
				}
			case "playbook_prompt_send":
				if creditText(event["id"]) == "cnb-content-calendar" {
					desktop = true
				}
			}
		}
	}
	if !reporterExpert || !reporterSkill || !desktop {
		t.Fatalf("report shapes expert=%v skill=%v desktop=%v reports=%+v",
			reporterExpert, reporterSkill, desktop, state.reports)
	}
}

func TestCampaignRunsWhenGrowthListFails(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-campaign-only", realmCN, nil)
	state := newCampaignFakeState()
	state.mu.Lock()
	state.progress["expert"] = 1
	state.mu.Unlock()
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			return http.StatusInternalServerError, map[string]any{"code": 1, "msg": "growth offline"}
		case r.Method == http.MethodGet && r.URL.Path == "/portal/activity/tasks":
			activity := r.URL.Query().Get("activity_code")
			if activity != "campaign_starbucks_2026" {
				return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
			}
			state.mu.Lock()
			claimedAt := ""
			if state.claimed["expert"] {
				claimedAt = "2026-11-13T00:00:01Z"
			}
			state.mu.Unlock()
			progress := map[string]any{
				"task_code": "expert", "progress": 1,
				"completed_at": "2026-11-13T00:00:00Z",
			}
			if claimedAt != "" {
				progress["claimed_at"] = claimedAt
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "expert", "target_count": 1,
					"reward_credit": 100, "need_view": true,
				}},
				"progress": []any{progress},
			}}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			task := campaignTaskFromPath(r.URL.Path)
			state.mu.Lock()
			state.claims = append(state.claims, task)
			state.claimed[task] = true
			state.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			state.recordReport(events)
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-campaign-only", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK {
		t.Fatalf("campaign-only growth result = %+v", result)
	}
	if result.EarnedCredit != 100 {
		t.Fatalf("earned = %g, want 100 from the completed campaign task", result.EarnedCredit)
	}
	text := strings.Join(result.Logs, "\n")
	if !strings.Contains(text, "成长任务清单读取失败") || !strings.Contains(text, "运营活动") {
		t.Fatalf("logs did not record separate reasons: %s", text)
	}
	state.mu.Lock()
	claims := append([]string(nil), state.claims...)
	state.mu.Unlock()
	if len(claims) != 1 || claims[0] != "expert" {
		t.Fatalf("claims = %v, want [expert]", claims)
	}
	if calls := fake.count(http.MethodPost, "/activity/growth/buddy/travel/depart"); calls != 0 {
		t.Fatalf("campaign-only run spent the daily cat chain: %d travel depart calls", calls)
	}
}

func TestCampaignRunsWhenGrowthListIsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-campaign-empty", realmCN, nil)
	state := newCampaignFakeState()
	state.mu.Lock()
	state.progress["expert"] = 1
	state.mu.Unlock()
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"tasks": []any{}}}
		case r.Method == http.MethodGet && r.URL.Path == "/portal/activity/tasks":
			activity := r.URL.Query().Get("activity_code")
			if activity != "campaign_starbucks_2026" {
				return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
			}
			state.mu.Lock()
			claimedAt := ""
			if state.claimed["expert"] {
				claimedAt = "2026-11-13T00:00:01Z"
			}
			state.mu.Unlock()
			progress := map[string]any{
				"task_code": "expert", "progress": 1,
				"completed_at": "2026-11-13T00:00:00Z",
			}
			if claimedAt != "" {
				progress["claimed_at"] = claimedAt
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "expert", "target_count": 1,
					"reward_credit": 100, "need_view": false,
				}},
				"progress": []any{progress},
			}}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			task := campaignTaskFromPath(r.URL.Path)
			state.mu.Lock()
			state.claims = append(state.claims, task)
			state.claimed[task] = true
			state.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	result, err := provider.GrowthTasks(context.Background(), "cn-campaign-empty", true)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if !result.OK || result.EarnedCredit != 100 {
		t.Fatalf("empty-growth campaign result = %+v", result)
	}
	if !strings.Contains(strings.Join(result.Logs, "\n"), "未能获取到成长任务清单") {
		t.Fatalf("logs = %v", result.Logs)
	}
	if calls := fake.count(http.MethodPost, "/activity/growth/buddy/travel/depart"); calls != 0 {
		t.Fatalf("empty-growth campaign run spent the daily cat chain: %d travel depart calls", calls)
	}
}

func TestCampaignRedactsUpstreamSecrets(t *testing.T) {
	dir := t.TempDir()
	secret := "secret-campaign-access-token"
	writeTaskAccount(t, dir, "cn-campaign-secret", realmCN, map[string]any{
		"accessToken": secret,
	})
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == "/portal/activity/tasks" {
			return http.StatusInternalServerError, map[string]any{
				"code": 1, "msg": "upstream rejected " + secret,
			}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	snapshot, err := provider.CampaignSnapshot(context.Background(), "cn-campaign-secret")
	if err != nil {
		t.Fatalf("CampaignSnapshot: %v", err)
	}
	assertNoSecret(t, "campaign snapshot", snapshot, secret)
	if snapshot.OK {
		t.Fatalf("snapshot should report the read failure: %+v", snapshot)
	}
	_ = fake
}

// TestCampaignClaimRejectsUnknownCodes guards the route contract from arbitrary
// upstream paths.
func TestCampaignClaimRejectsUnknownCodes(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-campaign-unknown", realmCN, nil)
	fake, server := newTaskTestUpstream(t, nil)
	provider, _ := setupTaskProvider(t, realmCN, dir, server.URL)
	if _, err := provider.CampaignClaim(context.Background(), "cn-campaign-unknown", "campaign_nope", "ai_chat"); err == nil {
		t.Fatal("unknown activity code was accepted")
	}
	if _, err := provider.CampaignClaim(context.Background(), "cn-campaign-unknown", "campaign_starbucks_2026", "nope"); err == nil {
		t.Fatal("unknown campaign task was accepted")
	}
	if len(fake.snapshot()) != 0 {
		t.Fatalf("unknown codes reached upstream: %+v", fake.snapshot())
	}
}

// TestCampaignClaimSuccessRequiresClaimedAt is the positive counterpart: a
// completed task with a verified receipt returns the credit.
func TestCampaignClaimSuccessRequiresClaimedAt(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-campaign-claim", realmCN, nil)
	state := newCampaignFakeState()
	state.mu.Lock()
	state.progress["expert"] = 1
	state.mu.Unlock()
	fake := newCampaignFake(t, state)
	provider, _ := setupTaskProvider(t, realmCN, dir, campaignFakeServerURL(t, fake))
	result, err := provider.CampaignClaim(context.Background(), "cn-campaign-claim", "campaign_starbucks_2026", "expert")
	if err != nil {
		t.Fatalf("CampaignClaim: %v", err)
	}
	if !result.OK || result.Credit != 100 {
		t.Fatalf("claim = %+v", result)
	}
	state.mu.Lock()
	claimed := state.claimed["expert"]
	state.mu.Unlock()
	if !claimed {
		t.Fatal("claim did not reach upstream")
	}
}

func TestCampaignRouteStatusRunAndClaim(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.cn, "uid-campaign", realmCN, nil)
	state := newCampaignFakeState()
	state.mu.Lock()
	state.progress["expert"] = 1
	state.mu.Unlock()
	f.fake.mu.Lock()
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/portal/activity/tasks":
			return http.StatusOK, state.campaignPayload(r.URL.Query().Get("activity_code"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			task := campaignTaskFromPath(r.URL.Path)
			state.mu.Lock()
			state.claims = append(state.claims, task)
			state.claimed[task] = true
			state.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/viewed"):
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			state.recordReport(events)
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	}
	f.fake.mu.Unlock()

	rec := f.do(http.MethodGet, "/activity/campaigns?uid=uid-campaign", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	if activities, ok := body["activities"].([]any); !ok || len(activities) != 2 {
		t.Fatalf("status activities = %#v", body["activities"])
	}

	rec = f.do(http.MethodPost, "/activity/campaigns/claim", map[string]any{
		"uid": "uid-campaign", "activity_code": "campaign_starbucks_2026", "task": "expert",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("claim = %d %s", rec.Code, rec.Body)
	}
	claim := decodeWBRoutesJSON(t, rec)
	if claim["ok"] != true || creditNumber(claim["credit"]) != 100 {
		t.Fatalf("claim body = %#v", claim)
	}

	rec = f.do(http.MethodPost, "/activity/campaigns/run", map[string]any{"uid": "uid-campaign", "claim": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	run := decodeWBRoutesJSON(t, rec)
	if run["ok"] != true || !strings.Contains(fmt.Sprint(run["logs"]), "运营活动") {
		t.Fatalf("run body = %#v", run)
	}
}
