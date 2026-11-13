package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	campaignScene       = "pc_incentive_task_mvp_v3"
	campaignAID         = "497858"
	campaignPCVersion   = "2.33.5"
	campaignDetailPath  = "/alice/ug/campaign/v3/detail/"
	campaignCheckinPath = "/alice/ug/campaign/landing/checkin"
	campaignReferer     = "https://www.doubao.com/pc/agent-explore"
	campaignUA          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 SamanthaDoubao/2.33.5"
)

// CheckinStatus is the provider-private view of one account's daily campaign
// state. It is safe to expose through the panel; it contains no credentials.
type CheckinStatus struct {
	CheckedInToday  bool   `json:"checked_in_today"`
	HasSigninTask   bool   `json:"has_signin_task"`
	ContinuousDays  int    `json:"continuous_days"`
	TotalDays       int    `json:"total_days"`
	LastCheckinDate string `json:"last_checkin_date"`
	RewardAmount    int    `json:"reward_amount"`
	TotalAmount     int    `json:"total_amount"`
}

// CheckinResult is the outcome of one check-in attempt.
type CheckinResult struct {
	UID             string `json:"uid"`
	Name            string `json:"name"`
	Status          string `json:"status"` // success | already | fail
	Message         string `json:"message"`
	Reward          int    `json:"reward"`
	ContinuousDays  int    `json:"continuous_days"`
	TotalAmount     int    `json:"total_amount"`
	LastCheckinDate string `json:"last_checkin_date"`
}

func (p *Provider) campaignQuery() string {
	values := url.Values{}
	values.Set("version_code", "20800")
	values.Set("language", "zh")
	values.Set("device_platform", "web")
	values.Set("aid", campaignAID)
	values.Set("real_aid", campaignAID)
	values.Set("pc_version", campaignPCVersion)
	values.Set("samantha_web", "1")
	values.Set("web_platform", "desktop")
	values.Set("use-olympus-account", "1")
	return values.Encode()
}

func (p *Provider) postCampaignJSON(ctx context.Context, account *Account, path string, body map[string]any) (map[string]any, error) {
	if p == nil || account == nil {
		return nil, errors.New("doubao: account is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cfg := p.currentConfig()
	base := strings.TrimRight(cfg.BaseURL, "/")
	account.mu.Lock()
	query, ua := account.CheckinQuery, account.CheckinUA
	account.mu.Unlock()
	query = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query), "?"))
	if query == "" {
		query = p.campaignQuery()
	}
	if strings.TrimSpace(ua) == "" {
		ua = campaignUA
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path+"?"+query, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("agw-js-conv", "str")
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9")
	req.Header.Set("cookie", account.cookieHeader())
	req.Header.Set("origin", base)
	req.Header.Set("referer", campaignReferer)
	req.Header.Set("user-agent", ua)

	client, err := p.clientForProxy(p.proxyURL(account))
	if err != nil {
		return nil, &APIError{Kind: KindTransient, Message: "proxy slot is misconfigured"}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &APIError{Kind: KindTransient, Message: "campaign request failed"}
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, &APIError{Kind: KindTransient, Message: "campaign response read failed"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Kind: KindUpstream, Status: resp.StatusCode, Message: "campaign endpoint rejected the request"}
	}
	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return nil, &APIError{Kind: KindUpstream, Message: "campaign response is not JSON"}
	}
	code := intValue(payload["code"])
	if code != 0 {
		message := firstNonEmpty(stringValue(payload["message"]), stringValue(payload["msg"]), "campaign request failed")
		kind := KindUpstream
		switch code {
		case 710012001:
			kind = KindAuth
			message = "豆包登录态已失效，请重新导入 cookie"
		case 712010702:
			kind = KindBadRequest
			message = "豆包风控拒绝了设备参数，请重新导入 cookie/device"
		case 710010702:
			kind = KindBadRequest
			message = "豆包签到仅支持电脑端活动接口，当前 Web 会话被风控拒绝 (710010702)；请先在豆包电脑版手动签到"
		case 710022004:
			kind = KindRateLimited
			message = "豆包要求完成一次人工风控验证"
		}
		return nil, &APIError{Kind: kind, Message: message, Code: code}
	}
	return payload, nil
}

func parseCheckinDetail(payload map[string]any) (CheckinStatus, map[string]any) {
	status := CheckinStatus{}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return status, nil
	}
	if progress, ok := data["progress"].(map[string]any); ok {
		status.TotalAmount = intValue(progress["total_amount"])
	}
	tasks, _ := data["tasks"].([]any)
	for _, item := range tasks {
		task, _ := item.(map[string]any)
		if task == nil || stringValue(task["task_kind"]) != "daily_signin" {
			continue
		}
		status.HasSigninTask = true
		daily, _ := task["daily_checkin"].(map[string]any)
		if daily != nil {
			status.CheckedInToday, _ = daily["checked_in_today"].(bool)
			status.ContinuousDays = intValue(daily["continuous_days"])
			status.TotalDays = intValue(daily["total_days"])
			status.LastCheckinDate = stringValue(daily["last_checkin_date"])
			if reward, ok := daily["reward"].(map[string]any); ok {
				status.RewardAmount = intValue(reward["reward_amount"])
			}
		}
		break
	}
	return status, data
}

func sanitizeCampaignTasks(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	out := map[string]any{
		"campaign_code": stringValue(data["campaign_code"]),
		"progress":      data["progress"],
		"updated_at":    time.Now().Format("2006-01-02 15:04:05"),
	}
	if tasks, ok := data["tasks"].([]any); ok {
		kept := make([]any, 0, len(tasks))
		for _, item := range tasks {
			task, _ := item.(map[string]any)
			if task == nil {
				continue
			}
			entry := map[string]any{
				"campaign_code":       task["campaign_code"],
				"campaign_group_code": task["campaign_group_code"],
				"campaign_type":       task["campaign_type"],
				"task_kind":           task["task_kind"],
				"task_status":         task["task_status"],
				"title":               task["title"],
				"subtitle":            task["subtitle"],
				"description":         task["description"],
				"reward_mode":         task["reward_mode"],
				"reward_summaries":    task["reward_summaries"],
			}
			if daily, ok := task["daily_checkin"]; ok {
				entry["daily_checkin"] = daily
			}
			kept = append(kept, entry)
		}
		out["tasks"] = kept
	}
	if milestones, ok := data["milestones"].([]any); ok {
		kept := make([]any, 0, len(milestones))
		for _, item := range milestones {
			milestone, _ := item.(map[string]any)
			if milestone == nil {
				continue
			}
			kept = append(kept, map[string]any{
				"campaign_code":       milestone["campaign_code"],
				"campaign_group_code": milestone["campaign_group_code"],
				"milestone_threshold": milestone["milestone_threshold"],
				"task_kind":           milestone["task_kind"],
				"task_status":         milestone["task_status"],
				"title":               milestone["title"],
				"subtitle":            milestone["subtitle"],
				"reward_summaries":    milestone["reward_summaries"],
			})
		}
		out["milestones"] = kept
	}
	return out
}

func (p *Provider) updateCampaignCache(account *Account, status CheckinStatus, data map[string]any) {
	if account == nil {
		return
	}
	account.mu.Lock()
	credits := cloneAnyMap(account.Credits)
	credits["checked_in_today"] = status.CheckedInToday
	credits["has_signin_task"] = status.HasSigninTask
	credits["continuous_days"] = status.ContinuousDays
	credits["total_days"] = status.TotalDays
	credits["last_checkin_date"] = status.LastCheckinDate
	credits["reward_amount"] = status.RewardAmount
	credits["total_amount"] = status.TotalAmount
	credits["updated_at"] = time.Now().Format("2006-01-02 15:04:05")
	account.Credits = credits
	account.Tasks = sanitizeCampaignTasks(data)
	if status.CheckedInToday && strings.TrimSpace(status.LastCheckinDate) != "" {
		account.LastCheckin = status.LastCheckinDate
	}
	account.mu.Unlock()
	_ = account.save()
}

func (p *Provider) updateCreditCache(account *Account, payload map[string]any) {
	if account == nil {
		return
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return
	}
	creditInfo, _ := data["credit_info"].(map[string]any)
	tasks, _ := data["optional_tasks"].([]any)
	kept := make([]any, 0, len(tasks))
	for _, item := range tasks {
		task, _ := item.(map[string]any)
		if task == nil {
			continue
		}
		kept = append(kept, map[string]any{
			"task_type":     task["task_type"],
			"title":         task["title"],
			"description":   task["description"],
			"button_text":   task["button_text"],
			"button_status": task["button_status"],
			"button_schema": task["button_schema"],
		})
	}
	account.mu.Lock()
	credits := cloneAnyMap(account.Credits)
	if creditInfo != nil {
		credits["total_credit_num"] = creditInfo["total_credit_num"]
		credits["credit_text"] = creditInfo["credit_text"]
		credits["credit_desc_text"] = creditInfo["credit_desc_text"]
	}
	credits["optional_tasks"] = kept
	credits["credits_updated_at"] = time.Now().Format("2006-01-02 15:04:05")
	account.Credits = credits
	account.mu.Unlock()
	_ = account.save()
}

// CheckinStatus reads the current daily campaign state and caches it.
func (p *Provider) CheckinStatus(ctx context.Context, uid string) (CheckinStatus, error) {
	account := p.pool.account(uid)
	if account == nil {
		return CheckinStatus{}, ErrNoAccount
	}
	payload, err := p.postCampaignJSON(ctx, account, campaignDetailPath, map[string]any{"campaign_group_code": campaignScene})
	if err != nil {
		return CheckinStatus{}, err
	}
	status, data := parseCheckinDetail(payload)
	p.updateCampaignCache(account, status, data)
	return status, nil
}

// Checkin performs one daily check-in. It reads detail first so an already
// checked-in account is idempotent and never posts a second reward request.
func (p *Provider) Checkin(ctx context.Context, uid string) (CheckinResult, error) {
	account := p.pool.account(uid)
	if account == nil {
		return CheckinResult{}, ErrNoAccount
	}
	status, err := p.CheckinStatus(ctx, uid)
	if err != nil {
		return CheckinResult{}, err
	}
	result := CheckinResult{
		UID: account.UID, Name: account.Nickname, Reward: status.RewardAmount,
		ContinuousDays: status.ContinuousDays, TotalAmount: status.TotalAmount,
		LastCheckinDate: status.LastCheckinDate,
	}
	if status.CheckedInToday {
		result.Status = "already"
		result.Message = "今日已签到"
		return result, nil
	}
	if !status.HasSigninTask {
		result.Status = "fail"
		result.Message = "当前账号没有可用的每日签到任务"
		return result, nil
	}
	payload, err := p.postCampaignJSON(ctx, account, campaignCheckinPath, map[string]any{"scene": campaignScene})
	if err != nil {
		return result, err
	}
	data, _ := payload["data"].(map[string]any)
	checked, _ := data["checked_in"].(bool)
	idempotent, _ := data["idempotent"].(bool)
	reward := intValue(data["reward_amount"])
	continuous := intValue(data["continuous_days"])
	date := stringValue(data["checkin_date"])
	if idempotent || (!checked && date != "") {
		result.Status = "already"
		result.Message = "今日已签到（幂等）"
		result.LastCheckinDate = firstNonEmpty(date, result.LastCheckinDate)
		result.ContinuousDays = continuous
		return result, nil
	}
	if !checked {
		result.Status = "fail"
		result.Message = "签到未生效"
		return result, nil
	}
	result.Status = "success"
	result.Reward = reward
	result.ContinuousDays = continuous
	result.LastCheckinDate = firstNonEmpty(date, result.LastCheckinDate)
	result.TotalAmount = status.TotalAmount + reward
	result.Message = fmt.Sprintf("签到成功 +%d 额度", reward)
	account.mu.Lock()
	credits := cloneAnyMap(account.Credits)
	credits["checked_in_today"] = true
	credits["continuous_days"] = continuous
	credits["last_checkin_date"] = result.LastCheckinDate
	credits["reward_amount"] = reward
	credits["total_amount"] = result.TotalAmount
	credits["updated_at"] = time.Now().Format("2006-01-02 15:04:05")
	account.Credits = credits
	account.Tasks = markCheckinTask(account.Tasks, result)
	account.LastCheckin = result.LastCheckinDate
	account.mu.Unlock()
	_ = account.save()
	return result, nil
}

// CheckinAll performs a sequential daily check-in across enabled accounts.
func (p *Provider) CheckinAll(ctx context.Context) ([]CheckinResult, error) {
	accounts := p.pool.Accounts()
	out := make([]CheckinResult, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || !account.Enabled {
			continue
		}
		result, err := p.Checkin(ctx, account.UID)
		if err != nil {
			out = append(out, CheckinResult{
				UID: account.UID, Name: account.Nickname, Status: "fail", Message: errorMessage(err),
			})
		} else {
			out = append(out, result)
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(800 * time.Millisecond):
		}
	}
	return out, nil
}

// RefreshCredits reads the credits/optional-task endpoint and caches the
// result. It is read-only apart from the local account file write.
func (p *Provider) RefreshCredits(ctx context.Context, uid string) (map[string]any, error) {
	account := p.pool.account(uid)
	if account == nil {
		return nil, ErrNoAccount
	}
	payload, err := p.postCampaignJSON(ctx, account, "/commerce/benefit_supply/credit/get_credit_num_optional_tasks", map[string]any{"need_tasks": true})
	if err != nil {
		return nil, err
	}
	p.updateCreditCache(account, payload)
	data, _ := payload["data"].(map[string]any)
	return data, nil
}

func markCheckinTask(tasks map[string]any, result CheckinResult) map[string]any {
	if tasks == nil {
		return nil
	}
	out := cloneAnyMap(tasks)
	list, _ := out["tasks"].([]any)
	kept := make([]any, 0, len(list))
	for _, item := range list {
		task, _ := item.(map[string]any)
		if task == nil || stringValue(task["task_kind"]) != "daily_signin" {
			kept = append(kept, item)
			continue
		}
		updated := cloneAnyMap(task)
		daily, _ := updated["daily_checkin"].(map[string]any)
		daily = cloneAnyMap(daily)
		daily["checked_in_today"] = true
		if result.ContinuousDays > 0 {
			daily["continuous_days"] = result.ContinuousDays
			daily["total_days"] = result.ContinuousDays
		}
		if result.LastCheckinDate != "" {
			daily["last_checkin_date"] = result.LastCheckinDate
		}
		updated["daily_checkin"] = daily
		kept = append(kept, updated)
	}
	out["tasks"] = kept
	return out
}

func cloneAnyMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value)+1)
	for key, item := range value {
		out[key] = item
	}
	return out
}
