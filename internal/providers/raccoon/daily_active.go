package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var dailyActivePollDelay = 2 * time.Second

const (
	dailyActiveMaxAttempts = 2
	dailyActiveRetryAfter  = time.Hour
	// The grant is booked asynchronously by the login service. In production
	// it landed about 14 seconds after the refresh; poll through that window.
	dailyActivePollTries = 10
)

// DailyActiveResult is one account's daily-activity outcome. It is returned by
// the manual endpoint and stored as a compact map in accountstate.State.Extra.
//
// A daily-activity refresh is a real login-session refresh: it rotates the
// account's tokens but never sends a chat completion or spends points. Chat
// and Model are retained only for wire compatibility with older panels.
type DailyActiveResult struct {
	UID       string `json:"uid"`
	OK        bool   `json:"ok"`
	Skipped   bool   `json:"skipped,omitempty"`
	Granted   bool   `json:"granted,omitempty"`
	Points    int    `json:"points"`
	Chat      bool   `json:"chat"`
	Model     string `json:"model,omitempty"`
	Refreshed bool   `json:"refreshed,omitempty"`
	Message   string `json:"message,omitempty"`
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

// DailyActive checks one account, or every enabled account when uid is empty.
// It first reads today's bills so an already-landed reward is never refreshed
// again. When today's grant is missing it force-refreshes the login session,
// then polls the bill until the upstream's asynchronous daily_grant appears.
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

	summary := DailyActiveSummary{
		OK:       true,
		Accounts: len(targets),
		Results:  make([]DailyActiveResult, 0, len(targets)),
	}
	for _, account := range targets {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		result := p.dailyActiveAccount(ctx, account, force)
		if !result.Skipped {
			summary.Checked++
			summary.Points += result.Points
			if result.Granted {
				summary.Granted++
			}
			if !result.OK {
				summary.OK = false
			}
		}
		summary.Results = append(summary.Results, result)
	}
	summary.Message = fmt.Sprintf("已处理 %d 个账号，到账 %d 个，共 %d 积分", summary.Checked, summary.Granted, summary.Points)
	return summary, nil
}

func (p *Pool) dailyActiveAccount(ctx context.Context, account *Account, force bool) DailyActiveResult {
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
		state = map[string]any{"date": today, "attempts": 0, "ok": false, "granted": false, "points": 0}
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
		state["granted"] = true
		state["points"] = landed
		state["attempts"] = attempts
		state["refreshed"] = false
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
			result.Message = fmt.Sprintf("今日已刷新登录 %d 次，奖励未到账，明天再试", attempts)
			return result
		}
		if last, ok := dailyActiveLastAttempt(state["last_attempt"]); ok && now.Sub(last) < dailyActiveRetryAfter {
			result.Skipped = true
			result.Message = "距上次刷新登录不足 60 分钟，稍后再试"
			return result
		}
	}

	// A refresh with the existing refresh token is a real login-session
	// refresh. It does not send a chat or spend points, and the upstream books
	// daily_grant asynchronously after the refresh succeeds.
	attempts++
	state["attempts"] = attempts
	state["last_attempt"] = now.Format(time.RFC3339)

	refreshErr := p.dailyActiveRefresh(ctx, account)
	if refreshErr != nil {
		result.OK = false
		result.Message = "刷新登录失败：" + redactedPointsError(refreshErr, account, maxPointsErrorRunes)
		state["ok"] = false
		state["granted"] = false
		state["refreshed"] = false
		state["points"] = 0
		state["checked_at"] = p.timeNow().Format(time.RFC3339)
		state["message"] = result.Message
		_ = p.saveDailyActive(account, state)
		return result
	}

	result.Refreshed = true
	state["refreshed"] = true
	granted, verifyErr := p.pollDailyGrant(ctx, account)
	result.OK = true
	if verifyErr != nil {
		result.Message = "登录会话已刷新，但核对每日活跃奖励失败: " + verifyErr.Error()
		state["ok"] = true
		state["granted"] = false
		state["points"] = 0
	} else if granted <= 0 {
		result.Message = "登录会话已刷新，但今日每日活跃奖励尚未到账，稍后自动重试"
		state["ok"] = true
		state["granted"] = false
		state["points"] = 0
	} else {
		result.Granted = true
		result.Points = granted
		result.Message = fmt.Sprintf("登录会话已刷新，今日每日活跃奖励 %d 积分已到账", granted)
		state["ok"] = true
		state["granted"] = true
		state["points"] = granted
	}
	state["checked_at"] = p.timeNow().Format(time.RFC3339)
	state["message"] = result.Message
	if err := p.saveDailyActive(account, state); err != nil && result.Message == "" {
		result.Message = "保存状态失败: " + err.Error()
	}
	return result
}

func (p *Pool) dailyActiveRefresh(ctx context.Context, account *Account) error {
	tokens := p.TokenManager()
	if tokens == nil {
		return errors.New("登录 token 管理器未配置")
	}
	return tokens.Refresh(ctx, account)
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
