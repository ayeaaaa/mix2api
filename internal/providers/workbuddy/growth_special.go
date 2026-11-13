package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Real-action growth tasks. Both are not plain /v2/report single events:
//   - skill_share first creates a real Open Platform asset-share channel, then
//     reports the channel click/success pair, and only claims after the server
//     confirms progress;
//   - template_5 uses server-issued scene ids and a four-event group per slot.
const (
	skillMarketPath       = "/v2/operation-platform/market/skill/list"
	skillShareChannelPath = "/api/v2/open-platform/asset-shares/c2c/channels"
	templateScenesPath    = "/console/as/support/scenes?locale=zh-CN"

	skillSharePreferredID = "skill_2097350077599879168"
	skillSharePollTries   = 4
	skillSharePollGap     = 3 * time.Second
	templatePollTries     = 4
	templatePollGap       = 2 * time.Second
)

type growthTemplateScene struct {
	ID   string
	Name string
}

var templateFallbackScenes = []growthTemplateScene{
	{ID: "0", Name: "幻灯片"},
	{ID: "4", Name: "深度研究"},
	{ID: "8", Name: "数据分析"},
	{ID: "16", Name: "设计"},
	{ID: "20", Name: "日常开发"},
}

type marketSkill struct {
	ID          string
	Name        string
	Description string
}

type skillShareChannel struct {
	ShareCode string
	ChannelID string
	Link      string
}

func (p *Provider) skillShareMarketSkill(ctx context.Context, account *Account) (*marketSkill, error) {
	body, err := json.Marshal(map[string]any{"page": 1, "page_size": 20})
	if err != nil {
		return nil, err
	}
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskCampaignBase()+skillMarketPath, p.taskChatHeaders(account, false),
		body, tasksDefaultTimeout)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 || !campaignShareCode(response.Payload) {
		return nil, fmt.Errorf("HTTP %d %s", response.Status,
			firstNonEmpty(taskPayloadMessage(response.Payload), "skill market list rejected"))
	}
	skills, _ := anyList(creditObject(response.Payload["data"])["skills"])
	candidates := make([]marketSkill, 0, len(skills))
	for _, rawItem := range skills {
		item := creditObject(rawItem)
		skillID := strings.TrimSpace(creditText(item["skill_id"]))
		if skillID == "" {
			skillID = strings.TrimSpace(creditText(item["id"]))
		}
		name := firstNonEmptyText(
			creditText(item["display_name_zh"]), creditText(item["name"]),
			creditText(item["display_name_en"]))
		if skillID == "" || name == "" {
			continue
		}
		description := firstNonEmptyText(
			creditText(item["description_zh"]), creditText(item["description"]),
			creditText(item["description_en"]))
		candidates = append(candidates, marketSkill{
			ID: skillID, Name: name, Description: truncateTaskText(description, 60),
		})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	for index := range candidates {
		if candidates[index].ID == skillSharePreferredID {
			return &candidates[index], nil
		}
	}
	return &candidates[0], nil
}

func (p *Provider) createSkillShareChannel(ctx context.Context, account *Account, skill marketSkill) (skillShareChannel, error) {
	var result skillShareChannel
	skillID := strings.TrimSpace(skill.ID)
	if skillID == "" {
		return result, errors.New("市场技能 ID 为空")
	}
	assetRef, err := json.Marshal(map[string]any{"asset_id": skillID})
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]any{
		"asset_kind": "skill", "asset_source": "market", "asset_ref": string(assetRef),
		"ways": []any{"link", "wxacode", "wxapath"},
		"extra": map[string]any{
			"name": skill.Name, "description": skill.Description,
			"shareKey": skillID, "marketplaceSource": "builtin-market",
		},
	})
	if err != nil {
		return result, err
	}
	headers := p.taskChatHeaders(account, false)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("User-Agent", p.taskDesktopUserAgent())
	headers.Set("X-User-Id", strings.TrimSpace(taskAuthSnapshot(account).UID))
	headers.Set("X-Domain", taskDesktopReportDomain)
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskOpenBase()+skillShareChannelPath, headers, body, 30*time.Second)
	if err != nil {
		return result, err
	}
	if response.Status < 200 || response.Status >= 300 || !campaignShareCode(response.Payload) {
		return result, fmt.Errorf("HTTP %d %s", response.Status,
			firstNonEmpty(taskPayloadMessage(response.Payload), "创建分享渠道失败"))
	}
	data := creditObject(response.Payload["data"])
	result.ShareCode = strings.TrimSpace(creditText(data["share_code"]))
	result.ChannelID = strings.TrimSpace(creditText(data["channel_id"]))
	if ways, ok := anyList(data["ways"]); ok {
		for _, rawItem := range ways {
			way := creditObject(rawItem)
			if strings.TrimSpace(creditText(way["share_way"])) != "link" {
				continue
			}
			result.Link = strings.TrimSpace(creditText(way["resource_ref"]))
			break
		}
	}
	if result.ShareCode == "" || result.Link == "" {
		return skillShareChannel{}, errors.New("分享接口缺少 share_code 或 link")
	}
	return result, nil
}

func skillShareChannelEvents(skill marketSkill, shareCode string, now time.Time) []map[string]any {
	stamp := now.UnixMilli()
	common := map[string]any{
		"skillId": skill.ID, "skillName": skill.Name, "skillType": "market",
	}
	click := cloneAnyMap(common)
	for key, value := range map[string]any{
		"eventCode": "skill_share_channel_action", "action": "link",
		"panelAction": "click", "panelType": "clipboard", "isSuccessful": true,
		"cost": 0, "id": shareCode, "actionAt": stamp,
	} {
		click[key] = value
	}
	success := cloneAnyMap(common)
	for key, value := range map[string]any{
		"eventCode": "skill_share_channel_action", "action": "link",
		"panelAction": "success", "panelType": "clipboard", "isSuccessful": true,
		"cost": 0, "id": shareCode, "actionAt": stamp,
	} {
		success[key] = value
	}
	return []map[string]any{click, success}
}

func (p *Provider) refreshGrowthTask(ctx context.Context, account *Account, code string) *GrowthTask {
	tasks, err := p.fetchGrowthTasks(ctx, account, false)
	if err != nil {
		return nil
	}
	for index := range tasks {
		if tasks[index].TaskCode == code {
			return &tasks[index]
		}
	}
	return nil
}

func (p *Provider) waitForGrowthTask(ctx context.Context, account *Account, code string, target, tries int, gap time.Duration) *GrowthTask {
	if tries <= 0 {
		tries = 1
	}
	var fresh *GrowthTask
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			p.taskSleepFor(gap)
		}
		fresh = p.refreshGrowthTask(ctx, account, code)
		if fresh == nil {
			continue
		}
		if fresh.Status == "completed" || fresh.Status == "claimed" || fresh.Current >= target {
			return fresh
		}
	}
	return fresh
}

// finishSpecialGrowthTask claims only after the server-side completion is
// visible. A missing fresh row is a deferred claim, never a success.
func (p *Provider) finishSpecialGrowthTask(ctx context.Context, run *growthRun, task GrowthTask, fresh *GrowthTask, spec growthTaskSpec) float64 {
	name := firstNonEmptyText(task.Name, spec.Name, task.TaskCode)
	target := maxInt(task.Target, spec.Target)
	if target <= 0 {
		target = 1
	}
	if fresh == nil || (fresh.Status != "completed" && fresh.Status != "claimed" && fresh.Current < target) {
		current := task.Current
		if fresh != nil {
			current = fresh.Current
		}
		run.log("? 任务 [%s] 进度 %d/%d 未确认，领奖顺延到下次运行", name, current, target)
		return 0
	}
	if fresh.Status == "claimed" {
		run.log("✓ 任务 [%s] 已领奖", name)
		return 0
	}
	if !run.autoClaim {
		run.log("⏸ 任务 [%s] 已达成，等待手动领取", name)
		run.claimable = appendUnique(run.claimable, task.TaskCode)
		return 0
	}
	return p.claimGrowthTask(ctx, run, task.TaskCode, name, spec)
}

// runSkillShareTask creates a real share channel, reports its two events, then
// verifies progress before claiming. It never reports a share that was not
// backed by a real share_code + link.
func (p *Provider) runSkillShareTask(ctx context.Context, run *growthRun, task GrowthTask, spec growthTaskSpec) float64 {
	name := firstNonEmptyText(task.Name, spec.Name, "分享技能给好友")
	skill, err := p.skillShareMarketSkill(ctx, run.account)
	if err != nil {
		run.log("⏭ 任务 [%s] 市场技能列表读取失败，不伪造分享: %s", name, p.taskRedactError(run.account, err))
		return 0
	}
	if skill == nil {
		run.log("⏭ 任务 [%s] 市场未返回可分享技能，不伪造分享", name)
		return 0
	}
	channel, err := p.createSkillShareChannel(ctx, run.account, *skill)
	if err != nil {
		run.log("! 任务 [%s] 创建分享渠道失败: %s", name, p.taskRedactError(run.account, err))
		return 0
	}
	if !p.reportDesktopEvents(ctx, run.account, skillShareChannelEvents(*skill, channel.ShareCode, p.taskNowTime())) {
		run.log("! 任务 [%s] 渠道已创建但事件上报失败，不领奖", name)
		return 0
	}
	run.log("正在等待任务 [%s] 服务端进度落账...", name)
	fresh := p.waitForGrowthTask(ctx, run.account, task.TaskCode, maxInt(task.Target, spec.Target), skillSharePollTries, skillSharePollGap)
	return p.finishSpecialGrowthTask(ctx, run, task, fresh, spec)
}

func (p *Provider) fetchTemplateScenes(ctx context.Context, account *Account) []growthTemplateScene {
	response, err := p.taskDo(ctx, account, http.MethodGet,
		p.taskWebBase()+templateScenesPath, p.taskChatHeaders(account, false),
		nil, tasksDefaultTimeout)
	if err == nil && response.Status >= 200 && response.Status < 300 {
		scenes := make([]growthTemplateScene, 0)
		rows, _ := anyList(creditObject(response.Payload["data"])["scenes"])
		for _, rawItem := range rows {
			item := creditObject(rawItem)
			if item["id"] == nil {
				continue
			}
			id := strings.TrimSpace(creditText(item["id"]))
			if id == "" {
				continue
			}
			scenes = append(scenes, growthTemplateScene{
				ID: id, Name: strings.TrimSpace(creditText(item["name"])),
			})
		}
		if len(scenes) > 0 {
			return scenes
		}
	}
	return append([]growthTemplateScene(nil), templateFallbackScenes...)
}

func templateEvents(scene growthTemplateScene, now time.Time) []map[string]any {
	requestID := campaignMessageID(now)
	conversation := newRequestTraceID()
	return []map[string]any{
		{
			"eventCode": "agent_task_created", "source": "CLOUD", "name": "",
			"mode": "craft", "requestModelId": "default", "action": scene.ID,
			"has_template": true, "template_id": scene.ID, "template_name": scene.Name,
			"requestId": requestID, "conversationId": conversation,
		},
		{
			"eventCode":  "agent_task_created_with_template",
			"templateId": scene.ID, "templateName": scene.Name,
			"template_id": scene.ID, "mode": "working", "isCustomModel": true,
			"id": scene.ID, "name": scene.Name, "requestId": requestID,
			"conversationId": conversation,
		},
		{
			"eventCode": "template_used", "template_id": scene.ID,
			"templateId": scene.ID, "templateName": scene.Name,
			"task_mode": "working", "id": scene.ID, "name": scene.Name,
			"source": "growth-center", "conversationId": conversation,
		},
		{
			"eventCode": "playbook_prompt_send", "ext1": newRequestTraceID(),
			"requestId": requestID, "id": scene.ID, "name": scene.Name,
			"type": "other", "promptLength": 30, "isOfficial": 1,
			"source": "growth-center", "conversationId": conversation,
		},
	}
}

// runTemplateTask advances only the missing template slots, using server scene
// ids when available and the static table only as a transport fallback. One
// event group must advance the server counter before the next group is sent.
func (p *Provider) runTemplateTask(ctx context.Context, run *growthRun, task GrowthTask, spec growthTaskSpec) float64 {
	name := firstNonEmptyText(task.Name, spec.Name, "模板创建任务")
	target := maxInt(task.Target, spec.Target)
	if target <= 0 {
		target = 5
	}
	current := task.Current
	scenes := p.fetchTemplateScenes(ctx, run.account)
	if len(scenes) == 0 {
		run.log("⏭ 任务 [%s] 取不到模板场景，跳过", name)
		return 0
	}
	var fresh *GrowthTask
	for current < target {
		scene := scenes[current%len(scenes)]
		before := current
		if !p.reportDesktopEvents(ctx, run.account, templateEvents(scene, p.taskNowTime())) {
			run.log("! 任务 [%s] 场景 %s 事件上报失败", name, firstNonEmpty(scene.Name, scene.ID))
			return 0
		}
		fresh = p.waitForGrowthTask(ctx, run.account, task.TaskCode, target, templatePollTries, templatePollGap)
		if fresh == nil {
			break
		}
		next := fresh.Current
		if fresh.Status == "completed" || fresh.Status == "claimed" || next >= target {
			current = maxInt(current, next)
			break
		}
		if next <= before {
			run.log("任务 [%s] 服务端进度未增长，停止本轮补发", name)
			break
		}
		current = next
	}
	run.log("任务 [%s] 当前服务端进度 %d/%d", name, current, target)
	if fresh == nil || fresh.Current < current {
		if task.TaskCode != "" {
			fresh = &GrowthTask{TaskCode: task.TaskCode, Status: "accepted", Current: current, Target: target}
		}
	}
	return p.finishSpecialGrowthTask(ctx, run, task, fresh, spec)
}
