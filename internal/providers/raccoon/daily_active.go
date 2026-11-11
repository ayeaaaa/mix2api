package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var dailyActivePollDelay = 2 * time.Second

const (
	dailyActiveMaxAttempts = 2
	dailyActiveRetryAfter  = time.Hour
	dailyActivePollTries   = 3

	dailyActivePrompt    = "hi"
	dailyActiveMaxTokens = 16
)

// DailyActiveResult is one account's daily-activity outcome. It is returned by
// the manual endpoint and stored as a compact map in accountstate.State.Extra.
type DailyActiveResult struct {
	UID     string `json:"uid"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Granted bool   `json:"granted,omitempty"`
	Points  int    `json:"points"`
	Chat    bool   `json:"chat"`
	Model   string `json:"model,omitempty"`
	Message string `json:"message,omitempty"`
}

// DailyActiveSummary mirrors v1's aggregate response.
type DailyActiveSummary struct {
	OK       bool                `json:"ok"`
	Accounts int                 `json:"accounts"`
	Checked  int                 `json:"checked"`
	Granted  int                 `json:"granted"`
	Points   int                 `json:"points"`
	Results  []DailyActiveResult `json:"results"`
	Message  string              `json:"message"`
}

func (p *Pool) dailyActiveConfig() Config {
	if p == nil {
		return Config{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// DailyActive checks one account, or every enabled account when uid is empty.
// It first reads today's bills so an already-landed reward is never re-pinged.
// A failed bill read still allows the conversation attempt; verification is
// best effort after the ping, matching v1.
func (p *Pool) DailyActive(ctx context.Context, uid string, force bool) (DailyActiveSummary, error) {
	if p == nil {
		return DailyActiveSummary{}, errors.New("raccoon: pool is nil")
	}
	if ctx == nil {
		return DailyActiveSummary{}, errors.New("raccoon: nil context")
	}

	uid = strings.TrimSpace(uid)
	var targets []*Account
	if uid != "" {
		account := p.Get(uid)
		if account == nil {
			return DailyActiveSummary{}, ErrAccountNotFound
		}
		targets = []*Account{account}
	} else {
		targets = p.Accounts()
	}

	cfg := p.dailyActiveConfig()
	summary := DailyActiveSummary{
		OK:       true,
		Accounts: len(targets),
		Results:  make([]DailyActiveResult, 0, len(targets)),
	}
	for _, account := range targets {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		result := p.dailyActiveAccount(ctx, account, cfg, force)
		if !result.Skipped {
			summary.Checked++
			summary.Points += result.Points
			if result.OK {
				summary.Granted++
			} else {
				summary.OK = false
			}
		}
		summary.Results = append(summary.Results, result)
	}
	summary.Message = fmt.Sprintf("已处理 %d 个账号，到账 %d 个，共 %d 积分", summary.Checked, summary.Granted, summary.Points)
	return summary, nil
}

func (p *Pool) dailyActiveAccount(ctx context.Context, account *Account, cfg Config, force bool) DailyActiveResult {
	if account == nil {
		return DailyActiveResult{Message: "账号不存在"}
	}
	uid, _ := account.currentUID()
	result := DailyActiveResult{UID: uid}

	account.mu.Lock()
	disabled := account.State.Disabled || account.State.ManualDisabled || !account.State.TasksEnabled || account.Deleted
	account.mu.Unlock()
	if disabled {
		result.Skipped = true
		result.Message = "账号未启用任务"
		return result
	}

	account.dailyActiveMu.Lock()
	defer account.dailyActiveMu.Unlock()

	now := p.timeNow()
	today := now.Local().Format("2006-01-02")
	state := p.dailyActiveState(account)
	stateDate, _ := state["date"].(string)
	if stateDate != today {
		state = map[string]any{"date": today, "attempts": 0, "ok": false, "points": 0}
	}
	attempts := intValue(state["attempts"])
	if attempts < 0 {
		attempts = 0
	}

	landed, landedErr := p.DailyGrantToday(ctx, account)
	if landedErr == nil && landed > 0 {
		result.OK = true
		result.Granted = true
		result.Points = landed
		result.Message = "今日每日活跃奖励已到账 " + fmt.Sprintf("%d", landed) + " 积分"
		state["ok"] = true
		state["points"] = landed
		state["attempts"] = attempts
		state["checked_at"] = now.Format(time.RFC3339)
		state["message"] = result.Message
		if err := p.saveDailyActive(account, state); err != nil {
			result.Message += "；保存状态失败: " + err.Error()
		}
		return result
	}

	if !force {
		if attempts >= dailyActiveMaxAttempts {
			result.Skipped = true
			result.Message = fmt.Sprintf("今日已打卡 %d 次，奖励未到账，明天再试", attempts)
			return result
		}
		if last, ok := dailyActiveLastAttempt(state["last_attempt"]); ok && now.Sub(last) < dailyActiveRetryAfter {
			result.Skipped = true
			result.Message = "距上次打卡不足 60 分钟，稍后再试"
			return result
		}
	}

	model := p.dailyActiveModel(account, cfg)
	if model == "" {
		result.Message = "没有可用于每日活跃打卡的模型"
		return result
	}
	result.Model = model
	result.Chat = true
	attempts++
	state["attempts"] = attempts
	state["last_attempt"] = now.Format(time.RFC3339)
	state["model"] = model

	pingErr := p.dailyActivePing(ctx, account, model)
	if pingErr != nil {
		result.Message = "打卡失败：" + pingErr.Error()
		state["ok"] = false
		state["points"] = 0
		state["checked_at"] = p.timeNow().Format(time.RFC3339)
		state["message"] = result.Message
		_ = p.saveDailyActive(account, state)
		return result
	}

	granted, verifyErr := p.pollDailyGrant(ctx, account)
	if verifyErr != nil {
		result.Message = "对话已发送，但核对每日活跃奖励失败: " + verifyErr.Error()
		state["ok"] = false
		state["points"] = 0
	} else if granted <= 0 {
		result.Message = "已打卡，但今日每日活跃奖励尚未到账，稍后自动重试"
		state["ok"] = false
		state["points"] = 0
	} else {
		result.OK = true
		result.Granted = true
		result.Points = granted
		result.Message = fmt.Sprintf("已打卡，今日每日活跃奖励 %d 积分已到账", granted)
		state["ok"] = true
		state["points"] = granted
	}
	state["checked_at"] = p.timeNow().Format(time.RFC3339)
	state["message"] = result.Message
	if err := p.saveDailyActive(account, state); err != nil && result.Message == "" {
		result.Message = "保存状态失败: " + err.Error()
	}
	return result
}

func (p *Pool) dailyActiveModel(account *Account, cfg Config) string {
	if model := strings.TrimSpace(cfg.DailyActiveModel); model != "" {
		return model
	}
	account.mu.Lock()
	models := cloneModels(account.Models)
	account.mu.Unlock()
	for _, item := range models {
		if model := strings.TrimSpace(pointsText(item["id"])); model != "" {
			return model
		}
	}
	p.catalogMu.Lock()
	shared := cloneModels(p.catalog.models)
	p.catalogMu.Unlock()
	for _, item := range shared {
		if model := strings.TrimSpace(pointsText(item["id"])); model != "" {
			return model
		}
	}
	if len(seedModelDefinitions) > 0 {
		return seedModelDefinitions[0].id
	}
	return ""
}

func (p *Pool) dailyActiveState(account *Account) map[string]any {
	account.mu.Lock()
	defer account.mu.Unlock()
	return cloneAnyMap(account.DailyActive)
}

func (p *Pool) saveDailyActive(account *Account, state map[string]any) error {
	account.mu.Lock()
	account.DailyActive = cloneAnyMap(state)
	account.mu.Unlock()
	return p.SaveState()
}

func (p *Pool) dailyActivePing(ctx context.Context, account *Account, model string) error {
	if !p.Acquire(account) {
		return errors.New("账号并发已满")
	}

	modelRaw, err := json.Marshal(model)
	if err != nil {
		return err
	}
	messages, err := json.Marshal([]map[string]any{{
		"role":    "user",
		"content": dailyActivePrompt,
	}})
	if err != nil {
		return err
	}
	body := map[string]json.RawMessage{
		"model":                 modelRaw,
		"messages":              messages,
		"max_completion_tokens": json.RawMessage(fmt.Sprintf("%d", dailyActiveMaxTokens)),
		"stream":                json.RawMessage("true"),
	}

	client := p.HTTPClient()
	tokens := p.TokenManager()
	if client == nil {
		return errors.New("chat http client is not configured")
	}
	stream, err := p.openChatAccount(ctx, account, body, model, client, tokens, p.dailyActiveConfig())
	if err != nil {
		p.Release(account)
		return err
	}
	if stream == nil {
		p.Release(account)
		return errors.New("chat returned an empty stream")
	}
	// The returned stream owns the slot and releases it on Close.
	defer stream.Close()
	read := 0
	for {
		chunk, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return nextErr
		}
		read += len(chunk)
		if bytes.Contains(chunk, []byte("[DONE]")) || read > 1<<20 {
			return nil
		}
	}
}

func (p *Pool) pollDailyGrant(ctx context.Context, account *Account) (int, error) {
	var lastErr error
	for attempt := 0; attempt < dailyActivePollTries; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, dailyActivePollDelay); err != nil {
				return 0, err
			}
		}
		points, err := p.DailyGrantToday(ctx, account)
		if err == nil {
			lastErr = nil
			if points > 0 {
				return points, nil
			}
			continue
		}
		lastErr = err
	}
	if lastErr != nil {
		return 0, lastErr
	}
	return 0, nil
}

func dailyActiveLastAttempt(value any) (time.Time, bool) {
	if parsed, ok := parsePointsTime(value); ok {
		return parsed, true
	}
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return time.Unix(int64(typed), 0), true
		}
	case float32:
		if typed > 0 {
			return time.Unix(int64(typed), 0), true
		}
	case int:
		if typed > 0 {
			return time.Unix(int64(typed), 0), true
		}
	case int64:
		if typed > 0 {
			return time.Unix(typed, 0), true
		}
	case json.Number:
		if number, err := typed.Int64(); err == nil && number > 0 {
			return time.Unix(number, 0), true
		}
	}
	return time.Time{}, false
}

func intValue(value any) int {
	number, ok := numericValue(value)
	if !ok {
		return 0
	}
	return int(number)
}
