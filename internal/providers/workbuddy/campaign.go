package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Operational campaign tasks (Starbucks collaboration and Reporter's Day) use
// the portal activity contract, not the growth-task list:
//
//	GET  /portal/activity/tasks?activity_code=...
//	POST /portal/activity/tasks/{task}/viewed?activity_code=...
//	POST /portal/activity/tasks/{task}/claim?activity_code=...
//
// The definitions below are intentionally separate from growthTaskSpecs. v1
// runs them even when the growth list is empty or unreadable, and reports the
// two upstream failures separately.

const (
	campaignTasksPath  = "/portal/activity/tasks"
	campaignMPReport   = "/v2/report"
	campaignMPCategory = "16-BackToSchool"

	campaignGap         = 3 * time.Second
	campaignPoll        = 6
	campaignSlowGap     = 10 * time.Second
	campaignSlowPoll    = 6
	campaignClaimGap    = 4 * time.Second
	campaignClaimPoll   = 5
	campaignDesktopWait = 60 * time.Second
	campaignDesktopPoll = 6 * time.Second

	campaignManualAppReason = "需手机端 App 登录后与 AI 对话一次，脚本不伪造"
)

type campaignPageSpec struct {
	PageURL  string
	PageName string
}

type campaignTaskSpec struct {
	Task       string
	Label      string
	Kind       string
	Times      int
	Credit     float64
	ExpertID   string
	ExpertName string
	SkillID    string
	SkillName  string
	CaseID     string
}

type campaignSpec struct {
	Code  string
	Label string
	Page  campaignPageSpec
	Tasks []campaignTaskSpec
}

type campaignCase struct {
	EventID      string
	Name         string
	Type         string
	PromptLength int
}

var campaignCases = map[string]campaignCase{
	"content-calendar": {
		EventID: "cnb-content-calendar", Name: "内容发布排期表",
		Type: "table", PromptLength: 359,
	},
}

var campaignSpecs = []campaignSpec{
	{
		Code:  "campaign_starbucks_2026",
		Label: "星巴克协作",
		Page: campaignPageSpec{
			PageURL:  "subpackages/secondary/starbucks-activity-428/index",
			PageName: "starbucks_activity",
		},
		Tasks: []campaignTaskSpec{
			{Task: "ai_chat", Label: "与 AI 对话3次", Kind: "chat", Times: 3, Credit: 100},
			{Task: "expert", Label: "召唤专家并对话", Kind: "expert", Credit: 100},
			{Task: "app", Label: "解锁 App 强大能力", Kind: "app", Credit: 100},
		},
	},
	{
		Code:  "campaign_reporter_2026",
		Label: "记者节福利活动",
		Page: campaignPageSpec{
			PageURL:  "subpackages/journalist-benefit/index",
			PageName: "journalist_benefit",
		},
		Tasks: []campaignTaskSpec{
			{
				Task: "expert_chat", Label: "体验「媒体工作助理」专家", Kind: "expert",
				ExpertID: "ex_NaiDbO06Adf3", ExpertName: "媒体工作助理", Credit: 50,
			},
			{
				Task: "skill_use", Label: "体验「采访素材整理」技能", Kind: "skill",
				SkillID: "skill_2117491159283752960", SkillName: "interview-material-organizer", Credit: 50,
			},
			{
				Task: "desktop_same", Label: "体验「内容发布排期」灵感", Kind: "desktop",
				CaseID: "content-calendar", Credit: 100,
			},
		},
	},
}

// campaignTaskRow is the upstream state for one issued campaign task.
type campaignTaskRow struct {
	TaskCode    string
	Target      int
	Progress    int
	CompletedAt string
	ClaimedAt   string
	NeedView    bool
	Credit      float64
}

// CampaignTaskStatus is one campaign task rendered by the panel.
type CampaignTaskStatus struct {
	ActivityCode  string  `json:"activity_code"`
	ActivityLabel string  `json:"activity_label"`
	TaskCode      string  `json:"task_code"`
	Label         string  `json:"label"`
	Kind          string  `json:"kind"`
	Target        int     `json:"target"`
	Progress      int     `json:"progress"`
	Credit        float64 `json:"credit"`
	Issued        bool    `json:"issued"`
	Completed     bool    `json:"completed"`
	Claimed       bool    `json:"claimed"`
	Manual        bool    `json:"manual"`
	ManualReason  string  `json:"manual_reason,omitempty"`
	State         string  `json:"state"`
}

// CampaignActivity is one campaign definition plus its issued task states.
type CampaignActivity struct {
	Code      string               `json:"code"`
	Label     string               `json:"label"`
	Available bool                 `json:"available"`
	Error     string               `json:"error,omitempty"`
	Tasks     []CampaignTaskStatus `json:"tasks"`
}

// CampaignSnapshot is a read-only view of both operational campaigns.
type CampaignSnapshot struct {
	UID        string             `json:"uid"`
	Nickname   string             `json:"nickname,omitempty"`
	Realm      string             `json:"realm,omitempty"`
	OK         bool               `json:"ok"`
	Skipped    bool               `json:"skipped,omitempty"`
	Error      string             `json:"error,omitempty"`
	Activities []CampaignActivity `json:"activities"`
	Logs       []string           `json:"logs,omitempty"`
}

// CampaignResult is one account's operational-campaign run.
type CampaignResult struct {
	UID          string             `json:"uid"`
	Nickname     string             `json:"nickname,omitempty"`
	Realm        string             `json:"realm,omitempty"`
	OK           bool               `json:"ok"`
	Skipped      bool               `json:"skipped,omitempty"`
	Busy         bool               `json:"busy,omitempty"`
	Msg          string             `json:"msg,omitempty"`
	Error        string             `json:"error,omitempty"`
	Activities   []CampaignActivity `json:"activities"`
	EarnedCredit float64            `json:"earned_credit"`
	Claimed      []string           `json:"claimed"`
	Logs         []string           `json:"logs"`
	Credits      *CreditSnapshot    `json:"credits,omitempty"`
}

// CampaignClaimResult is one verified manual claim.
type CampaignClaimResult struct {
	UID          string  `json:"uid"`
	ActivityCode string  `json:"activity_code"`
	Task         string  `json:"task"`
	OK           bool    `json:"ok"`
	Msg          string  `json:"msg,omitempty"`
	Credit       float64 `json:"credit"`
}

func (p *Provider) taskCampaignBase() string {
	if p == nil {
		return ""
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tasksCampaignBaseURL), "/"); base != "" {
		return base
	}
	// Tests and single-origin deployments point every task slice at one
	// server. Never let a campaign request escape to the real host in that
	// configuration.
	if base := strings.TrimRight(strings.TrimSpace(p.tasksChatBaseURL), "/"); base != "" {
		return base
	}
	return upstreamBaseOverride("WB_TASK_CAMPAIGN_UPSTREAM", "https://www.codebuddy.cn")
}

func (p *Provider) taskOpenBase() string {
	if p == nil {
		return ""
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tasksOpenBaseURL), "/"); base != "" {
		return base
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tasksChatBaseURL), "/"); base != "" {
		return base
	}
	return upstreamBaseOverride("WB_TASK_OPEN_UPSTREAM", "https://open.workbuddy.cn")
}

func campaignSpecForCode(code string) (campaignSpec, bool) {
	code = strings.TrimSpace(code)
	for _, spec := range campaignSpecs {
		if spec.Code == code {
			return spec, true
		}
	}
	return campaignSpec{}, false
}

func campaignTaskSpecFor(spec campaignSpec, code string) (campaignTaskSpec, bool) {
	for _, task := range spec.Tasks {
		if task.Task == code {
			return task, true
		}
	}
	return campaignTaskSpec{}, false
}

// campaignSuccessCode mirrors v1's GET/view contract: a missing or null code is
// accepted, but an explicit non-zero business code is not.
func campaignSuccessCode(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	code, exists := payload["code"]
	if !exists || code == nil {
		return true
	}
	return anyCodeNumber(code) == 0
}

// campaignShareCode mirrors the Open Platform channel contract, which accepts
// both 0 and 200 as success.
func campaignShareCode(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	code, exists := payload["code"]
	if !exists || code == nil {
		return false
	}
	number := anyCodeNumber(code)
	return number == 0 || number == 200
}

func (p *Provider) campaignHeaders(account *Account) http.Header {
	auth := taskAuthSnapshot(account)
	headers := p.taskChatHeaders(account, true)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("X-Client-Product", "workbuddy-mp")
	headers.Set("X-Client-Version", "2.4.0")
	headers.Set("X-Client-Platform", "mp-weixin")
	headers.Set("X-Platform", "wechatmp")
	if uid := strings.TrimSpace(auth.UID); uid != "" {
		headers.Set("X-User-Id", uid)
	}
	return headers
}

func campaignMPBase(auth Auth, now time.Time) map[string]any {
	uid := strings.TrimSpace(auth.UID)
	return map[string]any{
		"timestamp": now.UnixMilli(), "ideType": "WorkBuddy_MP", "ideVersion": "2.4.0",
		"extName": "workbuddy-mp", "extVersion": "2.4.0", "product": "SaaS",
		"ideName": "wx_app_cloud", "platform": "mini_program", "source": "mini_program",
		"os": "android", "osVersion": "14", "arch": "arm64",
		"machineId": "mp-" + truncateTaskText(uid, 16), "timezone": "Asia/Shanghai",
		"userId": uid, "userNickname": strings.TrimSpace(auth.Nickname),
	}
}

func campaignMessageID(now time.Time) string {
	return fmt.Sprintf("%012x%s", now.UnixMilli(), randomHex(10))
}

func campaignChatEvent(now time.Time) map[string]any {
	conversation := newRequestTraceID()
	requestID := campaignMessageID(now)
	return map[string]any{
		"eventCode": "chat_request_send", "mode": "chat", "inputLength": 12,
		"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false, "maxToken": 0, "maxSteps": 500,
		"temperature": 0, "maxRetries": 0, "mentionContexts": []any{},
		"knowledgeId": []any{}, "knowledgeName": []any{}, "codebaseId": "",
		"mentionContextCount": 0, "command": "", "recommendId": "",
		"skillId": "", "skillCount": 0, "totalCount": 0,
		"requestId": requestID, "traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversation, "conversationId": conversation,
		"messageId": "msg-" + requestID[len(requestID)-8:], "agentName": "mp",
		"agentType": "main", "codebuddy.session_id": conversation,
		"codebuddy.conversation_request_id": requestID,
	}
}

func campaignExpertEvents(expertID, expertName string, now time.Time) []map[string]any {
	conversation := newRequestTraceID()
	requestID := campaignMessageID(now)
	return []map[string]any{
		{
			"eventCode": "expert_summon_click", "id": expertID, "name": expertID,
			"expertTitle": expertName, "type": campaignMPCategory, "position": 0,
		},
		{
			"eventCode": "expert_summoned", "id": expertID, "name": expertID,
			"expertTitle": expertName,
		},
		{
			"eventCode": "expert_actual_use", "id": expertID, "name": expertID,
			"expertTitle": expertName, "type": campaignMPCategory,
			"characterCount": 14, "expertType": "builtin",
		},
		{
			"eventCode": "chat_request_send", "inputLength": 14,
			"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
			"codebaseEnable": false, "maxToken": 0, "maxSteps": 500,
			"temperature": 0, "maxRetries": 0, "mentionContexts": []any{},
			"knowledgeId": []any{}, "knowledgeName": []any{}, "codebaseId": "",
			"mentionContextCount": 0, "command": "", "recommendId": "",
			"skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversation, "conversationId": conversation,
			"messageId": "msg-" + requestID[len(requestID)-8:], "agentName": "mp",
			"agentType": "main", "expertId": expertID, "expertName": expertName,
			"codebuddy.session_id":              conversation,
			"codebuddy.conversation_request_id": requestID,
		},
	}
}

func campaignSkillEvents(skillID, skillName string, now time.Time) []map[string]any {
	conversation := newRequestTraceID()
	requestID := campaignMessageID(now)
	return []map[string]any{
		{
			"eventCode": "skill_info", "id": skillName, "skillId": skillID,
			"skillName": skillName, "skillType": "market", "skillSource": "market",
			"position": 0, "conversationId": conversation,
		},
		{
			"eventCode": "chat_request_send", "inputLength": 14,
			"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
			"codebaseEnable": false, "maxToken": 0, "maxSteps": 500,
			"temperature": 0, "maxRetries": 0, "mentionContexts": []any{},
			"knowledgeId": []any{}, "knowledgeName": []any{}, "codebaseId": "",
			"mentionContextCount": 0, "command": "", "recommendId": "",
			"skillId": skillID, "skillCount": 1, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversation, "conversationId": conversation,
			"messageId": "msg-" + requestID[len(requestID)-8:], "agentName": "mp",
			"agentType": "main", "codebuddy.session_id": conversation,
			"codebuddy.conversation_request_id": requestID,
		},
	}
}

func campaignDesktopEvents(item campaignCase, now time.Time) []map[string]any {
	return []map[string]any{{
		"eventCode": "playbook_prompt_send", "id": item.EventID, "name": item.Name,
		"type": item.Type, "promptLength": item.PromptLength, "isOfficial": 1,
		"source": "discover", "query": "", "ext1": "discover",
		"requestId": campaignMessageID(now), "conversationId": newRequestTraceID(),
	}}
}

func (p *Provider) campaignCall(ctx context.Context, account *Account, method, path string, body any) (taskHTTPResponse, error) {
	var raw []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return taskHTTPResponse{}, err
		}
		raw = encoded
	}
	return p.taskDo(ctx, account, method, p.taskCampaignBase()+path,
		p.campaignHeaders(account), raw, tasksDefaultTimeout)
}

func campaignQueryPath(activity, task, action string) string {
	path := campaignTasksPath
	if task != "" {
		path += "/" + url.PathEscape(task)
	}
	if action != "" {
		path += "/" + action
	}
	return path + "?activity_code=" + url.QueryEscape(activity)
}

func (p *Provider) fetchCampaignTasks(ctx context.Context, account *Account, activity string) (map[string]campaignTaskRow, error) {
	response, err := p.campaignCall(ctx, account, http.MethodGet,
		campaignQueryPath(activity, "", ""), nil)
	if err != nil {
		return nil, err
	}
	if response.Status != http.StatusOK || !campaignSuccessCode(response.Payload) {
		return nil, fmt.Errorf("HTTP %d %s", response.Status,
			firstNonEmpty(taskPayloadMessage(response.Payload), "活动任务读取失败"))
	}
	data := creditObject(response.Payload["data"])
	progress := make(map[string]map[string]any)
	if rows, ok := anyList(data["progress"]); ok {
		for _, rawItem := range rows {
			item := creditObject(rawItem)
			code := strings.TrimSpace(creditText(item["task_code"]))
			if code == "" {
				continue
			}
			progress[code] = item
		}
	}
	result := make(map[string]campaignTaskRow)
	if tasks, ok := anyList(data["tasks"]); ok {
		for _, rawItem := range tasks {
			item := creditObject(rawItem)
			code := strings.TrimSpace(creditText(item["task_code"]))
			if code == "" {
				continue
			}
			state := progress[code]
			needView := true
			if value, exists := item["need_view"]; exists {
				needView = creditTruthy(value)
			}
			target := int(creditNumber(item["target_count"]))
			if target <= 0 {
				target = int(creditNumber(item["target"]))
			}
			result[code] = campaignTaskRow{
				TaskCode:    code,
				Target:      target,
				Progress:    int(creditNumber(state["progress"])),
				CompletedAt: creditFieldText(state["completed_at"]),
				ClaimedAt:   creditFieldText(state["claimed_at"]),
				NeedView:    needView,
				Credit:      creditNumber(item["reward_credit"]),
			}
		}
	}
	return result, nil
}

func (p *Provider) viewCampaignTask(ctx context.Context, account *Account, activity, task string) bool {
	response, err := p.campaignCall(ctx, account, http.MethodPost,
		campaignQueryPath(activity, task, "viewed"), map[string]any{})
	if err != nil {
		return false
	}
	return response.Status == http.StatusOK && campaignSuccessCode(response.Payload)
}

// claimCampaignTask submits a claim and then verifies claimed_at by re-reading
// the progress list. A successful POST alone is not evidence of a credit.
func (p *Provider) claimCampaignTask(ctx context.Context, account *Account, activity, task string, credit float64, tries int, gap time.Duration, log func(string)) bool {
	response, err := p.campaignCall(ctx, account, http.MethodPost,
		campaignQueryPath(activity, task, "claim"), map[string]any{})
	if err != nil {
		log(fmt.Sprintf("      ⚠️ 领奖[%s/%s]: %s", activity, task, p.taskRedactError(account, err)))
		return false
	}
	if response.Status != http.StatusOK || !taskCodeIsZero(response.Payload) {
		log(fmt.Sprintf("      ⚠️ 领奖[%s/%s]: HTTP %d %s", activity, task,
			response.Status, truncateTaskText(taskPayloadMessage(response.Payload), 80)))
		return false
	}
	if tries <= 0 {
		tries = campaignClaimPoll
	}
	if gap < 0 {
		gap = campaignClaimGap
	}
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			p.taskSleepFor(gap)
		}
		rows, readErr := p.fetchCampaignTasks(ctx, account, activity)
		if readErr != nil {
			continue
		}
		if row := rows[task]; row.ClaimedAt != "" {
			log(fmt.Sprintf("      🎁 领奖[%s/%s]: +%g 积分", activity, task, credit))
			return true
		}
	}
	log(fmt.Sprintf("      ⚠️ 领奖[%s/%s]: 已提交但未回读到 claimed_at，下轮重试", activity, task))
	return false
}

func (p *Provider) pollCampaignTask(ctx context.Context, account *Account, activity, task string, target, tries int, gap time.Duration) int {
	if tries <= 0 {
		tries = campaignPoll
	}
	current := 0
	for attempt := 0; attempt < tries; attempt++ {
		p.taskSleepFor(gap)
		rows, err := p.fetchCampaignTasks(ctx, account, activity)
		if err != nil {
			continue
		}
		row := rows[task]
		current = row.Progress
		if row.CompletedAt != "" || current >= target {
			return maxInt(current, target)
		}
	}
	return current
}

func (p *Provider) reportCampaignMPEvents(ctx context.Context, account *Account, spec campaignSpec, events []map[string]any) bool {
	if len(events) == 0 {
		return true
	}
	auth := taskAuthSnapshot(account)
	now := p.taskNowTime()
	base := campaignMPBase(auth, now)
	payload := make([]map[string]any, 0, len(events))
	for _, event := range events {
		item := cloneAnyMap(base)
		item["pageURL"] = spec.Page.PageURL
		item["pageName"] = spec.Page.PageName
		for key, value := range event {
			item[key] = value
		}
		payload = append(payload, item)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskCampaignBase()+campaignMPReport, p.campaignHeaders(account),
		body, tasksDefaultTimeout)
	if err != nil || response.Status < 200 || response.Status >= 300 {
		return false
	}
	return taskCodeIsZero(response.Payload)
}

func (p *Provider) campaignPickExpert(ctx context.Context, account *Account) (string, string) {
	body, _ := json.Marshal(map[string]any{
		"edition_mode": "all,domestic", "page": 1, "page_size": 20,
		"sort_by": "use_count", "sort_order": "desc",
		"categories": []any{campaignMPCategory}, "expert_type": "agent",
	})
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskCampaignBase()+"/v2/operation-platform/market/expert/list",
		p.taskChatHeaders(account, false), body, tasksDefaultTimeout)
	if err == nil && response.Status >= 200 && response.Status < 300 {
		experts, _ := anyList(creditObject(response.Payload["data"])["experts"])
		for _, rawItem := range experts {
			item := creditObject(rawItem)
			expertID := strings.TrimSpace(creditText(item["expert_id"]))
			if expertID == "" {
				continue
			}
			name := creditText(creditObject(item["display_name_zh"])["zh"])
			if name == "" {
				name = creditText(item["display_name_zh"])
			}
			if name == "" {
				name = creditText(item["name"])
			}
			return expertID, firstNonEmpty(name, expertID)
		}
	}
	return "WorkspaceBuilder", "工作台搭建师"
}

// fireCampaignTask emits one real campaign criterion. The App task is
// deliberately not forgeable: it requires a genuine mobile login + chat.
func (p *Provider) fireCampaignTask(ctx context.Context, account *Account, spec campaignSpec, task campaignTaskSpec, log func(string)) bool {
	switch task.Kind {
	case "app":
		log("      ⏳ " + task.Label + ": " + campaignManualAppReason)
		return false
	case "desktop":
		item, ok := campaignCases[task.CaseID]
		if !ok {
			log("      ⚠️ " + task.Label + ": 未内置案例 " + task.CaseID)
			return false
		}
		return p.reportDesktopEvents(ctx, account, campaignDesktopEvents(item, p.taskNowTime()))
	case "chat":
		return p.reportCampaignMPEvents(ctx, account, spec, []map[string]any{campaignChatEvent(p.taskNowTime())})
	case "expert":
		expertID := strings.TrimSpace(task.ExpertID)
		expertName := strings.TrimSpace(task.ExpertName)
		if expertID == "" {
			expertID, expertName = p.campaignPickExpert(ctx, account)
		}
		if expertID == "" {
			log("      ✗ " + task.Label + ": 取不到专家 id，跳过")
			return false
		}
		return p.reportCampaignMPEvents(ctx, account, spec,
			campaignExpertEvents(expertID, expertName, p.taskNowTime()))
	case "skill":
		return p.reportCampaignMPEvents(ctx, account, spec,
			campaignSkillEvents(task.SkillID, task.SkillName, p.taskNowTime()))
	default:
		return false
	}
}

func campaignStateFor(row campaignTaskRow, task campaignTaskSpec, target int) string {
	if row.ClaimedAt != "" {
		return "claimed"
	}
	if row.CompletedAt != "" || (target > 0 && row.Progress >= target) {
		return "completed"
	}
	if task.Kind == "app" {
		return "manual"
	}
	if row.Progress > 0 {
		return "in_progress"
	}
	return "not_started"
}

func campaignStatusFor(spec campaignSpec, task campaignTaskSpec, row campaignTaskRow, issued bool) CampaignTaskStatus {
	status := CampaignTaskStatus{
		ActivityCode: spec.Code, ActivityLabel: spec.Label, TaskCode: task.Task,
		Label: task.Label, Kind: task.Kind, Target: row.Target, Progress: row.Progress,
		Credit: row.Credit, Issued: issued,
		Claimed: row.ClaimedAt != "",
	}
	if status.Target <= 0 {
		status.Target = firstPositive(task.Times, 1)
	}
	status.Completed = row.CompletedAt != "" || (status.Target > 0 && row.Progress >= status.Target)
	if status.Credit == 0 {
		status.Credit = task.Credit
	}
	if task.Kind == "app" {
		status.Manual = true
		status.ManualReason = campaignManualAppReason
	}
	if !issued {
		status.State = "unavailable"
		return status
	}
	status.State = campaignStateFor(row, task, status.Target)
	return status
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

type campaignRun struct {
	provider   *Provider
	account    *Account
	auth       Auth
	now        time.Time
	autoClaim  bool
	logs       []string
	earned     float64
	claimed    []string
	activities []CampaignActivity
}

func (r *campaignRun) log(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *campaignRun) logLine(line string) {
	r.logs = append(r.logs, line)
}

func (p *Provider) executeCampaigns(ctx context.Context, account *Account, autoClaim bool, now time.Time) campaignOutcome {
	outcome := campaignOutcome{
		Claimed: []string{}, Logs: []string{}, Activities: []CampaignActivity{},
	}
	if p == nil || account == nil {
		return outcome
	}
	run := &campaignRun{
		provider: p, account: account, auth: taskAuthSnapshot(account), now: now,
		autoClaim: autoClaim, claimed: []string{}, activities: []CampaignActivity{},
	}
	available := 0
	failed := false
	for _, spec := range campaignSpecs {
		activity := CampaignActivity{Code: spec.Code, Label: spec.Label, Tasks: []CampaignTaskStatus{}}
		rows, err := p.fetchCampaignTasks(ctx, account, spec.Code)
		if err != nil {
			failed = true
			activity.Error = p.taskRedactError(account, err)
			run.log("      ⚠️ 活动任务读取失败[%s]: %s", spec.Code, activity.Error)
		}
		if len(rows) == 0 {
			run.log("🎪 %s: 活动任务未下发/已结束或读取失败", spec.Label)
			for _, task := range spec.Tasks {
				activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, campaignTaskRow{}, false))
			}
			run.activities = append(run.activities, activity)
			continue
		}
		available++
		activity.Available = true
		run.log("🎪 %s（%s）", spec.Label, spec.Code)
		for _, task := range spec.Tasks {
			row, issued := rows[task.Task]
			if !issued {
				run.log("   ⏭ %s: 未下发/已下线", task.Label)
				activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, campaignTaskRow{}, false))
				continue
			}
			target := row.Target
			if target <= 0 {
				target = firstPositive(task.Times, 1)
			}
			credit := row.Credit
			if credit == 0 {
				credit = task.Credit
			}
			if row.ClaimedAt != "" {
				run.log("   ✅ %s: 已领奖（%d/%d）", task.Label, target, target)
				row.Target, row.Progress = target, target
				activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, row, true))
				continue
			}
			if row.CompletedAt != "" {
				if run.autoClaim {
					if p.claimCampaignTask(ctx, account, spec.Code, task.Task, credit, campaignClaimPoll, campaignClaimGap, run.logLine) {
						run.earned += credit
						run.claimed = appendUnique(run.claimed, spec.Code+"/"+task.Task)
						row.ClaimedAt = now.Format(time.RFC3339)
					} else {
						failed = true
					}
				} else {
					run.log("   ⏸ %s: 已达成，等待手动领取", task.Label)
				}
				row.Target, row.Progress = target, target
				activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, row, true))
				continue
			}
			if row.NeedView {
				if !p.viewCampaignTask(ctx, account, spec.Code, task.Task) {
					run.log("   ⚠️ %s: 浏览上报未确认，继续尝试任务判据", task.Label)
				}
			}
			current := row.Progress
			if task.Kind == "app" {
				p.fireCampaignTask(ctx, account, spec, task, run.logLine)
				run.log("   ⏳ %s: 尚未完成（%d/%d），真机完成后重跑会自动领奖", task.Label, current, target)
				row.Target, row.Progress = target, current
				activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, row, true))
				continue
			}
			if task.Kind == "desktop" {
				if !p.fireCampaignTask(ctx, account, spec, task, run.logLine) {
					run.log("   ⚠️ %s: 上报失败", task.Label)
				} else {
					current = p.pollCampaignTask(ctx, account, spec.Code, task.Task, target,
						maxInt(2, int(campaignDesktopWait/campaignDesktopPoll)), campaignDesktopPoll)
				}
			} else {
				need := target - current
				if need < 1 {
					need = 1
				}
				for index := 0; index < need; index++ {
					if current >= target {
						break
					}
					if !p.fireCampaignTask(ctx, account, spec, task, run.logLine) {
						break
					}
					current = p.pollCampaignTask(ctx, account, spec.Code, task.Task, target, campaignPoll, campaignGap)
				}
				if current < target {
					current = p.pollCampaignTask(ctx, account, spec.Code, task.Task, target, campaignSlowPoll, campaignSlowGap)
				}
			}
			fresh, readErr := p.fetchCampaignTasks(ctx, account, spec.Code)
			if readErr == nil {
				if updated, ok := fresh[task.Task]; ok {
					row = updated
					if row.Credit == 0 {
						row.Credit = credit
					}
					if row.Progress > current {
						current = row.Progress
					}
				}
			}
			if row.CompletedAt == "" && current < target {
				run.log("   ⏳ %s: 判据延迟未落库（%d/%d），下轮自动补领", task.Label, current, target)
				row.Target, row.Progress = target, current
				activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, row, true))
				continue
			}
			run.log("   ✅ %s: completed %d/%d", task.Label, target, target)
			row.Target, row.Progress = target, target
			row.CompletedAt = firstNonEmpty(row.CompletedAt, now.Format(time.RFC3339))
			if run.autoClaim {
				if p.claimCampaignTask(ctx, account, spec.Code, task.Task, credit, campaignClaimPoll, campaignClaimGap, run.logLine) {
					run.earned += credit
					run.claimed = appendUnique(run.claimed, spec.Code+"/"+task.Task)
					row.ClaimedAt = now.Format(time.RFC3339)
				} else {
					failed = true
				}
			} else {
				run.log("   ⏸ %s: 已达成，等待手动领取", task.Label)
			}
			activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, row, true))
		}
		run.activities = append(run.activities, activity)
	}
	if run.earned > 0 {
		run.log("🎪 活动任务本轮入账 %g 积分", run.earned)
	}
	outcome.Available = available
	outcome.Failed = failed
	outcome.Earned = run.earned
	outcome.Claimed = append([]string(nil), run.claimed...)
	outcome.Logs = p.scrubTaskLogs(account, run.logs)
	outcome.Activities = run.activities
	return outcome
}

type campaignOutcome struct {
	Available  int
	Failed     bool
	Earned     float64
	Claimed    []string
	Logs       []string
	Activities []CampaignActivity
}

func (p *Provider) readCampaignSnapshot(ctx context.Context, account *Account) (CampaignSnapshot, error) {
	auth := taskAuthSnapshot(account)
	result := CampaignSnapshot{
		UID: strings.TrimSpace(auth.UID), Nickname: strings.TrimSpace(auth.Nickname),
		Realm: auth.Realm, OK: true, Activities: []CampaignActivity{}, Logs: []string{},
	}
	if auth.Realm != realmCN {
		result.OK = false
		result.Error = "国际版不适用国内运营活动任务"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.OK = false
		result.Error = "API key accounts have no operational campaigns"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.Skipped = true
		result.Error = "企业版账号无运营活动任务，跳过"
		return result, nil
	}
	for _, spec := range campaignSpecs {
		activity := CampaignActivity{Code: spec.Code, Label: spec.Label, Tasks: []CampaignTaskStatus{}}
		rows, err := p.fetchCampaignTasks(ctx, account, spec.Code)
		if err != nil {
			result.OK = false
			activity.Error = p.taskRedactError(account, err)
			result.Logs = append(result.Logs, fmt.Sprintf("⚠️ 活动任务读取失败[%s]: %s", spec.Code, activity.Error))
		}
		if len(rows) > 0 {
			activity.Available = true
		}
		for _, task := range spec.Tasks {
			row, issued := rows[task.Task]
			activity.Tasks = append(activity.Tasks, campaignStatusFor(spec, task, row, issued))
		}
		result.Activities = append(result.Activities, activity)
	}
	return result, nil
}

// CampaignSnapshot reads both operational campaigns without mutating state.
func (p *Provider) CampaignSnapshot(ctx context.Context, uid string) (CampaignSnapshot, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return CampaignSnapshot{}, err
	}
	return p.readCampaignSnapshot(ctx, account)
}

// CampaignTasks runs the operational campaigns for one account. It is
// deliberately independent of the growth-task list: a failed or empty growth
// fetch must not suppress campaign progress.
func (p *Provider) CampaignTasks(ctx context.Context, uid string, autoClaim bool) (CampaignResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return CampaignResult{}, err
	}
	auth := taskAuthSnapshot(account)
	result := CampaignResult{
		UID: strings.TrimSpace(auth.UID), Nickname: strings.TrimSpace(auth.Nickname),
		Realm: auth.Realm, Activities: []CampaignActivity{}, Claimed: []string{}, Logs: []string{},
	}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
		result.Logs = p.scrubTaskLogs(account, result.Logs)
	}()
	if auth.Realm != realmCN {
		result.Error = "国际版不适用国内运营活动任务"
		result.Msg = result.Error
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no operational campaigns"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.OK = true
		result.Skipped = true
		result.Msg = "企业版账号无运营活动任务，跳过"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	now := p.taskNowTime()
	outcome := p.executeCampaigns(ctx, account, autoClaim, now)
	_ = p.refreshCreditAccount(ctx, account)
	result.Activities = outcome.Activities
	result.EarnedCredit = outcome.Earned
	result.Claimed = outcome.Claimed
	result.Logs = outcome.Logs
	result.Credits = creditSnapshotOf(account)
	result.OK = outcome.Available > 0 && !outcome.Failed
	if outcome.Available == 0 {
		result.Msg = "运营活动任务未下发或已结束"
	} else {
		result.Msg = fmt.Sprintf("运营活动执行完成: %d 个活动, 领奖 %d, 新增 %g 积分",
			outcome.Available, len(outcome.Claimed), outcome.Earned)
	}
	return result, nil
}

// CampaignClaim manually claims one completed campaign task. It verifies
// claimed_at by re-reading progress before reporting success.
func (p *Provider) CampaignClaim(ctx context.Context, uid, activityCode, taskCode string) (CampaignClaimResult, error) {
	result := CampaignClaimResult{
		UID: strings.TrimSpace(uid), ActivityCode: strings.TrimSpace(activityCode),
		Task: strings.TrimSpace(taskCode),
	}
	spec, ok := campaignSpecForCode(result.ActivityCode)
	if !ok {
		return result, errors.New("unknown activity_code")
	}
	task, ok := campaignTaskSpecFor(spec, result.Task)
	if !ok {
		return result, errors.New("unknown campaign task")
	}
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return result, err
	}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
	}()
	auth := taskAuthSnapshot(account)
	if auth.Realm != realmCN || taskIsAPIKey(auth) || taskEnterpriseID(auth) != "" {
		result.Msg = "账号不参与国内运营活动"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	rows, err := p.fetchCampaignTasks(ctx, account, spec.Code)
	if err != nil {
		return result, fmt.Errorf("workbuddy: read campaign tasks: %s", p.taskRedactError(account, err))
	}
	row, issued := rows[task.Task]
	if !issued {
		result.Msg = "任务未下发或已下线"
		return result, nil
	}
	if row.ClaimedAt != "" {
		result.OK = true
		result.Msg = "任务已领取"
		return result, nil
	}
	target := row.Target
	if target <= 0 {
		target = firstPositive(task.Times, 1)
	}
	if row.CompletedAt == "" && row.Progress < target {
		result.Msg = "任务尚未完成，无法领取"
		return result, nil
	}
	credit := row.Credit
	if credit == 0 {
		credit = task.Credit
	}
	logs := []string{}
	if p.claimCampaignTask(ctx, account, spec.Code, task.Task, credit, campaignClaimPoll, campaignClaimGap, func(line string) {
		logs = append(logs, line)
	}) {
		result.OK = true
		result.Credit = credit
		result.Msg = "领取成功"
		_ = p.refreshCreditAccount(ctx, account)
		return result, nil
	}
	result.Msg = firstNonEmpty(strings.TrimSpace(strings.Join(logs, "；")), "领取失败")
	return result, nil
}
