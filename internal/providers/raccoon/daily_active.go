package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var dailyActivePollDelay = 2 * time.Second

const (
	// The upstream books daily_grant shortly after the authenticated web work
	// startup request. Poll through that asynchronous window.
	dailyActivePollTries = 10

	// dailyActiveSettingPath is the authenticated web work startup request that
	// triggers the daily activity reward. A refresh-token rotation alone does
	// not trigger it.
	dailyActiveSettingPath = "/api/web/office/v3/setting_info"
	dailyActiveWorkHome    = "https://xiaohuanxiong.com/work/home"
	dailyActiveOrigin      = "https://xiaohuanxiong.com"
	dailyActivePlatform    = "web"
)

// DailyActiveResult is one account's daily-activity outcome. It is returned by
// the manual endpoint and stored as a compact map in accountstate.State.Extra.
//
// Daily activity is triggered by the same authenticated web settings request
// that the Raccoon work page sends on startup. It never sends a chat completion
// or spends points. Chat and Model are retained only for wire compatibility
// with older panels.
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
// It first reads today's bills so an already-landed reward is never triggered
// again. When today's grant is missing it sends the authenticated web work
// settings request, then polls the bill until the upstream's asynchronous
// daily_grant appears.
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
		result := p.dailyActiveAccount(ctx, account)
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

func (p *Pool) dailyActiveAccount(ctx context.Context, account *Account) DailyActiveResult {
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

	// Attempts are paced by the scheduler: one attempt per four-hour window,
	// with a random minute inside each window. Do not add a separate hourly
	// throttle here; a failed window must retry at the next window.
	attempts++
	state["attempts"] = attempts
	state["last_attempt"] = now.Format(time.RFC3339)

	refreshed, triggerErr := p.dailyActiveTrigger(ctx, account)
	if triggerErr != nil {
		result.OK = false
		result.Message = "触发每日活跃失败：" + redactedPointsError(triggerErr, account, maxPointsErrorRunes)
		state["ok"] = false
		state["granted"] = false
		state["refreshed"] = refreshed
		state["points"] = 0
		state["checked_at"] = p.timeNow().Format(time.RFC3339)
		state["message"] = result.Message
		_ = p.saveDailyActive(account, state)
		return result
	}

	result.Refreshed = refreshed
	state["refreshed"] = refreshed
	granted, verifyErr := p.pollDailyGrant(ctx, account)
	result.OK = true
	if verifyErr != nil {
		result.Message = "已触发 web 每日活跃，但核对每日活跃奖励失败: " + verifyErr.Error()
		state["ok"] = true
		state["granted"] = false
		state["points"] = 0
	} else if granted <= 0 {
		result.Message = "已触发 web 每日活跃，但今日每日活跃奖励尚未到账，稍后自动重试"
		state["ok"] = true
		state["granted"] = false
		state["points"] = 0
	} else {
		result.Granted = true
		result.Points = granted
		result.Message = fmt.Sprintf("已触发 web 每日活跃，今日每日活跃奖励 %d 积分已到账", granted)
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

// dailyActiveTrigger sends the web work settings request that causes the
// upstream to book daily_grant. The request carries both Bearer auth and the
// web cookies used by the browser session. It retries once after a forced
// token refresh when the first request is rejected.
func (p *Pool) dailyActiveTrigger(ctx context.Context, account *Account) (bool, error) {
	client := p.HTTPClient()
	if client == nil {
		return false, errors.New("raccoon: points http client is not configured")
	}
	tokens := p.TokenManager()
	if err := ensurePointsToken(ctx, tokens, account); err != nil {
		return false, err
	}

	payload, generation, err := p.requestDailyActiveSetting(ctx, account, client)
	refreshed := false
	if err != nil && IsAuthError(err) && tokens != nil {
		if refreshErr := tokens.RefreshIfGeneration(ctx, account, generation); refreshErr != nil {
			return false, refreshErr
		}
		refreshed = true
		payload, _, err = p.requestDailyActiveSetting(ctx, account, client)
	}
	if err != nil {
		return refreshed, err
	}
	if !dailyActiveResponseOK(payload) {
		return refreshed, errors.New("raccoon: daily-active settings request was rejected")
	}
	return refreshed, nil
}

func (p *Pool) requestDailyActiveSetting(
	ctx context.Context,
	account *Account,
	client *HTTPClient,
) (map[string]any, uint64, error) {
	generation := uint64(0)
	if tokens := p.TokenManager(); tokens != nil {
		generation = tokens.Generation(account)
	}
	snapshot := account.refreshCredentials()
	if snapshot.Deleted {
		return nil, 0, ErrAccountDeleted
	}
	if snapshot.Generation != generation {
		generation = snapshot.Generation
	}
	payload, err := client.DoJSON(
		ctx, http.MethodGet, dailyActiveSettingPath, dailyActiveWebHeaders(snapshot.Auth),
		nil, p.ProxyURL(account), authSecretValues(snapshot.Auth)...,
	)
	if err != nil {
		return nil, generation, err
	}
	return payload, generation, nil
}

func dailyActiveWebHeaders(auth Auth) http.Header {
	headers := make(http.Header)
	headers.Set("Accept", "*/*")
	headers.Set("Content-Type", "application/json")
	headers.Set("Referer", dailyActiveWorkHome)
	headers.Set("Origin", dailyActiveOrigin)
	headers.Set("X-Client-Platform", dailyActivePlatform)
	headers.Set("X-Raccoon-Language", ClientLanguage)
	if token := cleanToken(auth.AccessToken); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if cookie := dailyActiveCookie(auth); cookie != "" {
		headers.Set("Cookie", cookie)
	}
	identity := strings.TrimSpace(auth.OfficeIdentity)
	if identity != "" &&
		!strings.EqualFold(identity, "personal") &&
		!strings.EqualFold(identity, "none") {
		headers.Set("X-Org-Code", identity)
	}
	return headers
}

func dailyActiveCookie(auth Auth) string {
	parts := make([]string, 0, 6)
	add := func(name, value string) {
		value = strings.TrimSpace(value)
		value = strings.NewReplacer("\r", "", "\n", "", ";", "").Replace(value)
		if value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	access := cleanToken(auth.AccessToken)
	add("xhx_token", access)
	add("raccoon_token", access)
	add("raccoon_refresh_token", cleanToken(auth.RefreshToken))
	add("user_id", auth.UID)
	add("user_name", auth.UID)
	add("office_identity", auth.OfficeIdentity)
	return strings.Join(parts, "; ")
}

func dailyActiveResponseOK(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	code, ok := payload["code"]
	if !ok || code == nil {
		return true
	}
	if text, ok := code.(string); ok {
		text = strings.TrimSpace(text)
		return text == "" || text == "0" ||
			strings.EqualFold(text, "ok") || strings.EqualFold(text, "success")
	}
	return pointsInt(code) == 0
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

func intValue(value any) int {
	number, ok := numericValue(value)
	if !ok {
		return 0
	}
	return int(number)
}
