package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestGrowthSkillShareCreatesRealChannelAndVerifiesProgress(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-skill-share", realmCN, nil)
	type state struct {
		mu       sync.Mutex
		reported bool
		polls    int
		claimed  bool
		events   []map[string]any
		channel  map[string]any
	}
	runState := &state{}
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == skillMarketPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"skills": []any{map[string]any{
					"skill_id": "skill-test-1", "display_name_zh": "测试技能",
					"description_zh": "用于测试",
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == skillShareChannelPath:
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			runState.mu.Lock()
			runState.channel = body
			runState.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"share_code": "share-1", "channel_id": "channel-1",
				"ways": []any{map[string]any{
					"share_way": "link", "resource_ref": "https://example.test/share",
				}},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			runState.mu.Lock()
			if runState.reported {
				runState.polls++
			}
			status, current := "accepted", 0
			if runState.polls >= 2 {
				status, current = "completed", 1
			}
			runState.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "skill_share", "title": "分享技能给好友",
					"task_type": "single", "accept_status": status,
					"progress": map[string]any{"current": current, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			runState.mu.Lock()
			runState.reported = true
			runState.events = events
			runState.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/skill_share/claim":
			runState.mu.Lock()
			runState.claimed = true
			runState.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 100}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-skill-share")
	auth := taskAuthSnapshot(account)
	run := &growthRun{
		provider: provider, account: account, auth: auth, now: provider.taskNowTime(),
		autoClaim: true, claims: map[string]growthClaimEntry{},
	}
	earned := provider.progressGrowthTask(context.Background(), run,
		GrowthTask{TaskCode: "skill_share", Name: "分享技能给好友", Status: "accepted", Current: 0, Target: 1})
	if earned != 100 {
		t.Fatalf("earned = %g, want 100; logs=%v", earned, run.logs)
	}
	runState.mu.Lock()
	channel := runState.channel
	events := append([]map[string]any(nil), runState.events...)
	polls, claimed := runState.polls, runState.claimed
	runState.mu.Unlock()
	if !claimed {
		t.Fatal("skill share was not claimed")
	}
	if polls < 2 {
		t.Fatalf("polls = %d, want at least 2 progress checks before claim", polls)
	}
	if channel["asset_kind"] != "skill" || channel["asset_source"] != "market" {
		t.Fatalf("channel body = %#v", channel)
	}
	var assetRef map[string]any
	if err := json.Unmarshal([]byte(creditText(channel["asset_ref"])), &assetRef); err != nil {
		t.Fatalf("asset_ref = %v", channel["asset_ref"])
	}
	if assetRef["asset_id"] != "skill-test-1" {
		t.Fatalf("asset_ref = %#v", assetRef)
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[0]["panelAction"] != "click" || events[1]["panelAction"] != "success" {
		t.Fatalf("panel actions = %v/%v", events[0]["panelAction"], events[1]["panelAction"])
	}
	if events[0]["eventCode"] != "skill_share_channel_action" || events[0]["id"] != "share-1" {
		t.Fatalf("share event = %#v", events[0])
	}
	_ = fake
}

func TestGrowthSkillShareWithoutLinkDoesNotReportOrClaim(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-skill-nolink", realmCN, nil)
	var reported, claimed bool
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == skillMarketPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"skills": []any{map[string]any{"skill_id": "skill-test-1", "display_name_zh": "测试技能"}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == skillShareChannelPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"share_code": "share-1",
				"ways":       []any{map[string]any{"share_way": "link", "resource_ref": ""}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			reported = true
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			claimed = true
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-skill-nolink")
	run := &growthRun{
		provider: provider, account: account, auth: taskAuthSnapshot(account),
		now: provider.taskNowTime(), autoClaim: true, claims: map[string]growthClaimEntry{},
	}
	earned := provider.progressGrowthTask(context.Background(), run,
		GrowthTask{TaskCode: "skill_share", Status: "accepted", Current: 0, Target: 1})
	if earned != 0 {
		t.Fatalf("earned = %g, want 0", earned)
	}
	if reported {
		t.Fatal("skill share reported events without a real link")
	}
	if claimed {
		t.Fatal("skill share claimed without a real link")
	}
	if !strings.Contains(strings.Join(run.logs, "\n"), "share_code") {
		t.Fatalf("logs = %v", run.logs)
	}
}

func TestGrowthTemplateUsesServerScenesAndFourEventGroup(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-template", realmCN, nil)
	type state struct {
		mu       sync.Mutex
		progress int
		groups   [][]map[string]any
		claimed  bool
	}
	runState := &state{}
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == templateScenesPath[:strings.IndexByte(templateScenesPath, '?')]:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"scenes": []any{
					map[string]any{"id": "42", "name": "服务端场景A"},
					map[string]any{"id": "43", "name": "服务端场景B"},
					map[string]any{"id": "44", "name": "服务端场景C"},
					map[string]any{"id": "45", "name": "服务端场景D"},
					map[string]any{"id": "46", "name": "服务端场景E"},
				},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			runState.mu.Lock()
			progress := runState.progress
			runState.mu.Unlock()
			status := "accepted"
			if progress >= 5 {
				status = "completed"
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "template_5", "title": "模板创建任务",
					"task_type": "single", "accept_status": status,
					"progress": map[string]any{"current": progress, "target": 5},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			runState.mu.Lock()
			runState.groups = append(runState.groups, events)
			runState.progress++
			runState.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/template_5/claim":
			runState.mu.Lock()
			runState.claimed = true
			runState.mu.Unlock()
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 200}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-template")
	run := &growthRun{
		provider: provider, account: account, auth: taskAuthSnapshot(account),
		now: provider.taskNowTime(), autoClaim: true, claims: map[string]growthClaimEntry{},
	}
	earned := provider.progressGrowthTask(context.Background(), run,
		GrowthTask{TaskCode: "template_5", Name: "模板创建任务", Status: "accepted", Current: 0, Target: 5})
	if earned != 200 {
		t.Fatalf("earned = %g, want 200; logs=%v", earned, run.logs)
	}
	runState.mu.Lock()
	groups := append([][]map[string]any(nil), runState.groups...)
	claimed := runState.claimed
	runState.mu.Unlock()
	if !claimed {
		t.Fatal("template task was not claimed")
	}
	if len(groups) != 5 {
		t.Fatalf("event groups = %d, want 5", len(groups))
	}
	wantCodes := []string{
		"agent_task_created", "agent_task_created_with_template",
		"template_used", "playbook_prompt_send",
	}
	for index, group := range groups {
		if len(group) != len(wantCodes) {
			t.Fatalf("group %d = %#v", index, group)
		}
		for eventIndex, want := range wantCodes {
			if group[eventIndex]["eventCode"] != want {
				t.Fatalf("group %d event %d = %v, want %s", index, eventIndex, group[eventIndex]["eventCode"], want)
			}
		}
	}
	if groups[0][0]["template_id"] != "42" || groups[1][1]["templateId"] != "43" {
		t.Fatalf("scene ids = %v/%v", groups[0][0]["template_id"], groups[1][1]["templateId"])
	}
}

func TestGrowthTemplateStopsWhenServerProgressDoesNotAdvance(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-template-stuck", realmCN, nil)
	var groups, claimed int
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/support/scenes":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"scenes": []any{map[string]any{"id": "42", "name": "服务端场景A"}},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "template_5", "title": "模板创建任务",
					"task_type": "single", "accept_status": "accepted",
					"progress": map[string]any{"current": 0, "target": 5},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			groups++
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			claimed++
			return http.StatusOK, map[string]any{"code": 0}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-template-stuck")
	run := &growthRun{
		provider: provider, account: account, auth: taskAuthSnapshot(account),
		now: provider.taskNowTime(), autoClaim: true, claims: map[string]growthClaimEntry{},
	}
	earned := provider.progressGrowthTask(context.Background(), run,
		GrowthTask{TaskCode: "template_5", Status: "accepted", Current: 0, Target: 5})
	if earned != 0 {
		t.Fatalf("earned = %g, want 0", earned)
	}
	if groups != 1 {
		t.Fatalf("event groups = %d, want 1 (no duplicate spraying)", groups)
	}
	if claimed != 0 {
		t.Fatalf("claimed = %d, want 0", claimed)
	}
	if !strings.Contains(strings.Join(run.logs, "\n"), "未增长") {
		t.Fatalf("logs = %v", run.logs)
	}
}

func TestGrowthTemplateFallsBackToBuiltInScenes(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "cn-template-fallback", realmCN, nil)
	var firstScene string
	_, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/support/scenes":
			return http.StatusInternalServerError, map[string]any{"code": 1, "msg": "scenes offline"}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			status, current := "accepted", 0
			if firstScene != "" {
				status, current = "completed", 1
			}
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"tasks": []any{map[string]any{
					"task_code": "template_5", "task_type": "single",
					"accept_status": status,
					"progress":      map[string]any{"current": current, "target": 1},
				}},
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.Unmarshal(raw, &events)
			if len(events) > 0 {
				firstScene = creditText(events[0]["template_id"])
			}
			return http.StatusOK, map[string]any{"code": 0}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 200}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	provider, pool := setupTaskProvider(t, realmCN, dir, server.URL)
	account := accountByUID(t, pool, "cn-template-fallback")
	run := &growthRun{
		provider: provider, account: account, auth: taskAuthSnapshot(account),
		now: provider.taskNowTime(), autoClaim: true, claims: map[string]growthClaimEntry{},
	}
	_ = provider.progressGrowthTask(context.Background(), run,
		GrowthTask{TaskCode: "template_5", Status: "accepted", Current: 0, Target: 1})
	if firstScene != "0" {
		t.Fatalf("fallback scene = %q, want built-in 0", firstScene)
	}
}
