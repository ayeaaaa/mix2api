package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	dailyChatPrompt = "Hi"
	dailyChatPlugin = "weixinpay"
)

// DailyWebResult is one web-channel daily activity result. The fields mirror
// v1's daily_chat_web: phase is create/session/turn, attempts counts rounds, and
// retryable tells the caller whether another round is worth trying.
type DailyWebResult struct {
	OK              bool     `json:"ok"`
	Skipped         bool     `json:"skipped,omitempty"`
	Conversation    string   `json:"conversation,omitempty"`
	Status          string   `json:"status,omitempty"`
	Model           string   `json:"model,omitempty"`
	Chunks          int      `json:"chunks,omitempty"`
	ElapsedMS       int64    `json:"elapsed_ms,omitempty"`
	Attempts        int      `json:"attempts,omitempty"`
	ModelsTried     []string `json:"models_tried,omitempty"`
	Phase           string   `json:"phase,omitempty"`
	BudgetExhausted bool     `json:"budget_exhausted,omitempty"`
	Retryable       bool     `json:"retryable,omitempty"`
	Msg             string   `json:"msg,omitempty"`
	Error           string   `json:"error,omitempty"`
}

// DailyResult is the v1-compatible daily-activity row. DailyChatWeb returns the
// web fields flattened at the top level; DailyChat wraps any embedded web leg
// under "web" so the two panel envelopes stay distinguishable. StreakDays is a
// pointer because zero is meaningful for the activity report.
type DailyResult struct {
	UID             string          `json:"uid"`
	Nickname        string          `json:"nickname,omitempty"`
	Realm           string          `json:"realm,omitempty"`
	OK              bool            `json:"ok"`
	Skipped         bool            `json:"skipped,omitempty"`
	Restricted      bool            `json:"restricted,omitempty"`
	Busy            bool            `json:"busy,omitempty"`
	Msg             string          `json:"msg,omitempty"`
	Error           string          `json:"error,omitempty"`
	Web             *DailyWebResult `json:"web,omitempty"`
	Conversation    string          `json:"conversation,omitempty"`
	Status          string          `json:"status,omitempty"`
	Model           string          `json:"model,omitempty"`
	Chunks          int             `json:"chunks,omitempty"`
	ElapsedMS       int64           `json:"elapsed_ms,omitempty"`
	Attempts        int             `json:"attempts,omitempty"`
	ModelsTried     []string        `json:"models_tried,omitempty"`
	Phase           string          `json:"phase,omitempty"`
	BudgetExhausted bool            `json:"budget_exhausted,omitempty"`
	Retryable       bool            `json:"retryable,omitempty"`
	StreakDays      *int            `json:"streak_days,omitempty"`
	Activity        *ActivityEvent  `json:"activity,omitempty"`
}

// DailyChat runs the international desktop-identity daily conversation. The
// web conversation is a separate method (DailyChatWeb) because v1 exposes it as
// its own panel action; a scheduled caller that wants the official web count
// should run both. Success stamps lastDailyChat so repeated scheduler slots are
// idempotent across restarts.
func (p *Provider) DailyChat(ctx context.Context, uid string) (DailyResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return DailyResult{}, err
	}
	return p.runDailyChat(ctx, account, taskTriggerManual)
}

// DailyChatAll runs DailyChat for every enabled, task-participating
// international account.
func (p *Provider) DailyChatAll(ctx context.Context) ([]DailyResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: daily activity requires a context")
	}
	results := make([]DailyResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		auth := taskAuthSnapshot(account)
		if auth.Realm != realmIntl || taskIsAPIKey(auth) || taskEnterpriseID(auth) != "" {
			continue
		}
		result, err := p.runDailyChat(ctx, account, taskTriggerScheduler)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *Provider) runDailyChat(ctx context.Context, account *Account, trigger string) (DailyResult, error) {
	auth := taskAuthSnapshot(account)
	result := DailyResult{
		UID: strings.TrimSpace(auth.UID), Nickname: strings.TrimSpace(auth.Nickname),
		Realm: auth.Realm,
	}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
	}()
	if auth.Realm != realmIntl {
		result.Error = "daily chat is only for international accounts"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no activity programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.Error = "enterprise accounts do not support daily activity"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	now := p.taskNowTime()
	if taskDayDone(auth.LastDailyChat, now) {
		result.OK = true
		result.Skipped = true
		result.Msg = "今日活跃已完成，跳过本轮"
		return result, nil
	}
	if restricted := authExtraFloat(auth, "restrictedUntil"); restricted > 0 && restricted > float64(now.Unix()) {
		result.Skipped = true
		result.Restricted = true
		result.Error = "账号处于上游风控限制窗口，跳过每日活跃"
		return result, nil
	}

	model := p.dailyChatModel(auth)
	if err := p.runDailyChatDesktop(ctx, account, auth, model); err != nil {
		result.Error = p.taskRedactError(account, err)
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, false, result.Error)
		return result, nil
	}
	if err := p.persistLastDailyChat(account, now); err != nil {
		result.Error = p.taskRedactError(account, err)
	}
	_ = p.refreshCreditAccount(ctx, account)
	result.OK = true
	result.Msg = "每日活跃对话成功完成"
	result.Activity = p.newActivity(account, taskDailyChatTask, trigger, true, result.Msg)
	return result, nil
}

func (p *Provider) runDailyChatDesktop(ctx context.Context, account *Account, auth Auth, model string) error {
	body, err := json.Marshal(map[string]any{
		"model":            model,
		"messages":         []any{map[string]any{"role": "user", "content": dailyChatPrompt}},
		"stream":           true,
		"max_tokens":       10,
		"reasoning_effort": "none",
	})
	if err != nil {
		return err
	}
	headers := p.taskChatHeaders(account, false)
	status, responseBody, err := p.taskDoRaw(ctx, account, http.MethodPost,
		p.taskChatBase()+chatPath, headers, body, tasksDefaultTimeout)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		message := strings.TrimSpace(responseBody)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", status)
		}
		return fmt.Errorf("workbuddy: daily chat HTTP %d: %s", status, truncateTaskText(message, 160))
	}
	// The upstream is always SSE here; a 2xx response is the v1 success signal
	// and the body is intentionally not parsed.
	return nil
}

// DailyChatWeb runs one web-conversation daily activity attempt. It works for
// both realms (the web origin follows the account realm), is idempotent per day
// via the lastDailyChatWeb extra, and retries transient create/session/turn
// failures with v1's exponential backoff.
func (p *Provider) DailyChatWeb(ctx context.Context, uid string) (DailyResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return DailyResult{}, err
	}
	return p.runDailyChatWeb(ctx, account, taskTriggerManual)
}

// DailyChatWebAll runs the web activity for every enabled, task-participating
// account in this provider realm.
func (p *Provider) DailyChatWebAll(ctx context.Context) ([]DailyResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: daily activity requires a context")
	}
	results := make([]DailyResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		auth := taskAuthSnapshot(account)
		if (auth.Realm != realmCN && auth.Realm != realmIntl) ||
			taskIsAPIKey(auth) || taskEnterpriseID(auth) != "" {
			continue
		}
		result, err := p.runDailyChatWeb(ctx, account, taskTriggerScheduler)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *Provider) runDailyChatWeb(ctx context.Context, account *Account, trigger string) (DailyResult, error) {
	auth := taskAuthSnapshot(account)
	result := DailyResult{
		UID: strings.TrimSpace(auth.UID), Nickname: strings.TrimSpace(auth.Nickname),
		Realm: auth.Realm,
	}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
	}()
	if auth.Realm != realmCN && auth.Realm != realmIntl {
		result.Error = "web daily activity is only for CN/Intl realm accounts"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no activity programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.Error = "enterprise accounts do not support daily activity"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	now := p.taskNowTime()
	if taskDayDone(authExtraString(auth, "lastDailyChatWeb"), now) {
		result.OK = true
		result.Skipped = true
		result.Msg = "今日网页活跃已完成，跳过本轮"
		return result, nil
	}

	web := p.runDailyWebRounds(ctx, account, auth)
	result.OK = web.OK
	result.Skipped = web.Skipped
	result.Msg = web.Msg
	result.Error = web.Error
	result.Conversation = web.Conversation
	result.Status = web.Status
	result.Model = web.Model
	result.Chunks = web.Chunks
	result.ElapsedMS = web.ElapsedMS
	result.Attempts = web.Attempts
	result.ModelsTried = append([]string(nil), web.ModelsTried...)
	result.Phase = web.Phase
	result.BudgetExhausted = web.BudgetExhausted
	result.Retryable = web.Retryable
	result.Error = p.scrubTaskText(account, result.Error)
	result.Msg = p.scrubTaskText(account, result.Msg)
	if web.OK {
		_ = p.persistDailyWebDay(account, now)
		// The web leg is the officially counted daily activity in v1; also
		// stamp the shared field so a later DailyChat/other scheduler slot on
		// the same day cannot double-run the activity.
		_ = p.persistLastDailyChat(account, now)
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, true, web.Msg)
	} else {
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, false, result.Error)
	}
	return result, nil
}

func (p *Provider) runDailyWebRounds(ctx context.Context, account *Account, auth Auth) DailyWebResult {
	models := p.dailyChatModels(auth)
	if len(models) == 0 {
		models = []string{p.dailyChatModel(auth)}
	}
	rounds := len(models)
	if rounds < 2 {
		rounds = 2
	}
	budget := time.Duration(0)
	if p != nil {
		budget = p.tasksBudget
	}
	turnWait := tasksTurnWait
	if p != nil && p.tasksTurnWait > 0 {
		turnWait = p.tasksTurnWait
	}
	var deadline time.Time
	if budget > 0 {
		deadline = p.taskNowTime().Add(budget)
	}
	tried := make([]string, 0, rounds)
	var last DailyWebResult
	for index := 0; index < rounds; index++ {
		if err := ctx.Err(); err != nil {
			last.BudgetExhausted = true
			last.Error = err.Error()
			return last
		}
		wait := turnWait
		if !deadline.IsZero() {
			remaining := deadline.Sub(p.taskNowTime())
			if remaining <= 0 {
				last.BudgetExhausted = true
				if last.Error == "" {
					last.Error = fmt.Sprintf("单账号预算 %ds 用尽", int(budget/time.Second))
				}
				last.Phase = "budget"
				return last
			}
			if remaining < wait {
				wait = remaining
			}
		}
		model := models[index%len(models)]
		tried = append(tried, model)
		round := p.dailyWebRound(ctx, account, auth, model, wait, index)
		round.Attempts = index + 1
		round.ModelsTried = append([]string(nil), tried...)
		round.Model = model
		last = round
		if round.OK || !round.Retryable {
			return round
		}
		if index+1 >= rounds {
			break
		}
		p.taskSleepFor(p.taskBackoffDelay(index))
	}
	if last.Attempts == 0 {
		last.Phase = "budget"
		last.BudgetExhausted = true
		last.Error = "单账号预算不足一轮"
	}
	return last
}

func (p *Provider) dailyWebRound(ctx context.Context, account *Account, auth Auth, model string, wait time.Duration, round int) DailyWebResult {
	result := DailyWebResult{Model: model}
	headers := p.taskWebHeaders(account)
	payload, err := json.Marshal(map[string]any{
		"prompt":             dailyChatPrompt,
		"model":              model,
		"conversationOrigin": "workbuddy-app",
		"plugins": []any{map[string]any{
			"name": dailyChatPlugin, "marketplace": "codebuddy-builtin",
		}},
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	create, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskWebBase()+tasksWebConversationsPath, headers, payload, 30*time.Second)
	if err != nil {
		result.Phase = "create"
		result.Retryable = true
		result.Error = p.taskRedactError(account, err)
		return result
	}
	if create.Status < 200 || create.Status >= 300 {
		result.Phase = "create"
		result.Retryable = create.Status >= 500 || create.Status == http.StatusRequestTimeout || create.Status == http.StatusTooManyRequests
		result.Error = fmt.Sprintf("HTTP %d: %s", create.Status, truncateTaskText(taskPayloadMessage(create.Payload), 160))
		return result
	}
	if err := taskPayloadError(create.Payload); err != nil {
		result.Phase = "create"
		result.Error = p.taskRedactError(account, err)
		return result
	}
	conversation := strings.TrimSpace(creditText(creditObject(create.Payload["data"])["id"]))
	if conversation == "" {
		result.Phase = "create"
		result.Retryable = true
		result.Error = "create conversation returned no id"
		return result
	}
	result.Conversation = conversation

	// Fetching the sandbox proves the conversation has a running agent. v1
	// drives the ACP turn here; v2 polls the conversation status because the ACP
	// client belongs to a later slice.
	session, err := p.taskDo(ctx, account, http.MethodGet,
		p.taskWebBase()+tasksWebConversationsPath+conversation+"/session", headers, nil, 30*time.Second)
	if err != nil {
		result.Phase = "session"
		result.Retryable = true
		result.Error = p.taskRedactError(account, err)
		p.releaseWebConversation(ctx, account, conversation, true)
		return result
	}
	sessionData := creditObject(session.Payload["data"])
	if session.Status < 200 || session.Status >= 300 ||
		strings.TrimSpace(creditText(sessionData["link"])) == "" ||
		strings.TrimSpace(creditText(sessionData["token"])) == "" {
		result.Phase = "session"
		result.Retryable = true
		result.Error = "sandbox is not ready (missing link/token)"
		p.releaseWebConversation(ctx, account, conversation, true)
		return result
	}

	statusResult := p.pollWebConversation(ctx, account, headers, conversation, wait)
	statusResult.Model = model
	statusResult.Attempts = result.Attempts
	statusResult.ModelsTried = result.ModelsTried
	statusResult.Conversation = conversation
	statusResult.Phase = "turn"
	if statusResult.OK {
		p.releaseWebConversation(ctx, account, conversation, false)
		return statusResult
	}
	p.releaseWebConversation(ctx, account, conversation, true)
	return statusResult
}

func (p *Provider) pollWebConversation(ctx context.Context, account *Account, headers http.Header, conversation string, wait time.Duration) DailyWebResult {
	result := DailyWebResult{Conversation: conversation}
	started := time.Now()
	interval := tasksPollDefault
	if p != nil && p.tasksPollInterval > 0 {
		interval = p.tasksPollInterval
	}
	maxPolls := int(wait/interval) + 2
	if maxPolls < 2 {
		maxPolls = 2
	}
	lastStatus := ""
	for index := 0; index < maxPolls; index++ {
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			result.Retryable = false
			return result
		}
		response, err := p.taskDo(ctx, account, http.MethodGet,
			p.taskWebBase()+tasksWebConversationsPath+conversation, headers, nil, 15*time.Second)
		if err != nil {
			result.Error = p.taskRedactError(account, err)
			result.Retryable = true
		} else if response.Status >= 200 && response.Status < 300 {
			data := creditObject(response.Payload["data"])
			lastStatus = strings.ToLower(strings.TrimSpace(creditText(data["status"])))
			switch lastStatus {
			case "completed":
				result.OK = true
				result.Status = lastStatus
				result.Chunks = int(creditNumber(data["chunks"]))
				result.ElapsedMS = time.Since(started).Milliseconds()
				result.Msg = "网页通道会话跑完"
				return result
			case "failed", "error", "cancelled", "canceled", "deleted":
				result.Status = lastStatus
				result.Error = "web conversation ended with status " + lastStatus
				result.Retryable = true
				return result
			}
		} else {
			result.Error = fmt.Sprintf("HTTP %d", response.Status)
			result.Retryable = true
		}
		if index+1 < maxPolls {
			p.taskSleepFor(interval)
		}
	}
	result.Status = lastStatus
	result.Error = fmt.Sprintf("会话在 %s 内没有跑完（状态=%s）", wait, firstNonEmpty(lastStatus, "unknown"))
	result.Retryable = true
	return result
}

func (p *Provider) releaseWebConversation(ctx context.Context, account *Account, conversation string, deleteConversation bool) {
	if p == nil || account == nil || strings.TrimSpace(conversation) == "" {
		return
	}
	headers := p.taskWebHeaders(account)
	base := p.taskWebBase() + tasksWebConversationsPath + conversation
	// Cleanup must outlive a canceled round: an orphaned web conversation eats
	// the account's active-session quota until it is stopped.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.taskDo(cleanupCtx, account, http.MethodPost, base+"/stop", headers, []byte("{}"), 10*time.Second)
	if deleteConversation {
		_, _ = p.taskDo(cleanupCtx, account, http.MethodPost, base+"/delete", headers, []byte("{}"), 10*time.Second)
	}
}

// ReportActivity posts v1's chat_request_send event and records lastActivityReport.
// It is valid for both CN and Intl realms; each realm posts to its own chat base.
func (p *Provider) ReportActivity(ctx context.Context, uid string) (DailyResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return DailyResult{}, err
	}
	return p.runReportActivity(ctx, account, taskTriggerManual)
}

// ReportActivityAll runs the once-a-day activity report for every enabled,
// task-participating CN/Intl account in this provider.
func (p *Provider) ReportActivityAll(ctx context.Context) ([]DailyResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: activity report requires a context")
	}
	results := make([]DailyResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		auth := taskAuthSnapshot(account)
		if (auth.Realm != realmCN && auth.Realm != realmIntl) ||
			taskIsAPIKey(auth) || taskEnterpriseID(auth) != "" {
			continue
		}
		result, err := p.runReportActivity(ctx, account, taskTriggerScheduler)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *Provider) runReportActivity(ctx context.Context, account *Account, trigger string) (DailyResult, error) {
	auth := taskAuthSnapshot(account)
	result := DailyResult{
		UID: strings.TrimSpace(auth.UID), Nickname: strings.TrimSpace(auth.Nickname),
		Realm: auth.Realm,
	}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
	}()
	if auth.Realm != realmCN && auth.Realm != realmIntl {
		result.Error = "activity report is only for CN/Intl realm accounts"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no activity programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.Error = "enterprise accounts do not support activity report"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	now := p.taskNowTime()
	if taskDayDone(auth.LastActivityReport, now) {
		result.OK = true
		result.Skipped = true
		result.Msg = "今日活跃上报已完成，跳过本轮"
		return result, nil
	}

	event := buildTaskEvent(auth, "chat", 0, nil, now)
	body, err := json.Marshal([]any{event})
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	headers := p.taskChatHeaders(account, false)
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskChatBase()+tasksReportPath, headers, body, tasksDefaultTimeout)
	if err != nil {
		result.Error = p.taskRedactError(account, err)
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, false, result.Error)
		return result, nil
	}
	if response.Status < 200 || response.Status >= 300 {
		result.Error = fmt.Sprintf("活跃上报失败 HTTP %d", response.Status)
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, false, result.Error)
		return result, nil
	}
	if err := taskPayloadError(response.Payload); err != nil {
		result.Error = "活跃上报被上游拒绝（未返回 code=0）: " + p.taskRedactError(account, err)
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, false, result.Error)
		return result, nil
	}
	if !taskCodeIsZero(response.Payload) {
		result.Error = "活跃上报被上游拒绝（未返回 code=0）"
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, false, result.Error)
		return result, nil
	}

	if err := p.persistLastActivityReport(account, now); err != nil {
		result.Error = p.taskRedactError(account, err)
	}
	days, streakErr := p.fetchStreakDays(ctx, account)
	if streakErr == nil && days != nil {
		result.StreakDays = days
	}
	result.OK = true
	switch {
	case days == nil:
		result.Msg = "对话活跃上报成功（连登天数未知）"
	case *days <= 0:
		result.Msg = "对话活跃上报成功，但连登仍是 0 天（可能被上游静默丢弃）"
	default:
		result.Msg = fmt.Sprintf("对话活跃上报成功（连续打卡 %d 天）", *days)
	}
	result.Activity = p.newActivity(account, taskDailyChatTask, trigger, true, result.Msg)
	return result, nil
}

func (p *Provider) fetchStreakDays(ctx context.Context, account *Account) (*int, error) {
	headers := p.taskChatHeaders(account, false)
	response, err := p.taskDo(ctx, account, http.MethodGet,
		p.taskChatBase()+tasksGrowthStreakPath, headers, nil, tasksSummaryTimeout)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, fmt.Errorf("workbuddy: streak HTTP %d", response.Status)
	}
	if err := taskPayloadError(response.Payload); err != nil {
		return nil, err
	}
	data := creditObject(response.Payload["data"])
	streak := creditObject(data["streak"])
	if streak == nil {
		return nil, nil
	}
	value, exists := streak["days"]
	if !exists || value == nil {
		return nil, nil
	}
	days := int(creditNumber(value))
	return &days, nil
}

func (p *Provider) persistLastDailyChat(account *Account, now time.Time) error {
	uid := accountUID(account)
	stamp := now.Format(tasksTimeLayout)
	if p == nil || p.Pool() == nil || uid == "" {
		account.mu.Lock()
		account.Auth.LastDailyChat = stamp
		account.mu.Unlock()
		return nil
	}
	return p.Pool().mutateAccount(uid, func(account *Account) {
		account.Auth.LastDailyChat = stamp
	})
}

func (p *Provider) persistDailyWebDay(account *Account, now time.Time) error {
	return p.persistTaskExtra(account, "lastDailyChatWeb", now.Format(tasksTimeLayout))
}

func (p *Provider) persistLastActivityReport(account *Account, now time.Time) error {
	uid := accountUID(account)
	stamp := now.Format(tasksTimeLayout)
	if p == nil || p.Pool() == nil || uid == "" {
		account.mu.Lock()
		account.Auth.LastActivityReport = stamp
		account.mu.Unlock()
		return nil
	}
	return p.Pool().mutateAccount(uid, func(account *Account) {
		account.Auth.LastActivityReport = stamp
	})
}

func (p *Provider) persistTaskExtra(account *Account, key string, value any) error {
	if p == nil || account == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	uid := accountUID(account)
	if p.Pool() == nil || uid == "" {
		account.mu.Lock()
		if account.Auth.Extra == nil {
			account.Auth.Extra = make(map[string]json.RawMessage)
		}
		account.Auth.Extra[key] = raw
		account.mu.Unlock()
		return nil
	}
	return p.Pool().mutateAccount(uid, func(account *Account) {
		if account.Auth.Extra == nil {
			account.Auth.Extra = make(map[string]json.RawMessage)
		}
		account.Auth.Extra[key] = raw
	})
}

func taskDayDone(stamp string, now time.Time) bool {
	stamp = strings.TrimSpace(stamp)
	if stamp == "" {
		return false
	}
	return strings.HasPrefix(stamp, now.Format(tasksDayLayout))
}

func (p *Provider) dailyChatModel(auth Auth) string {
	if value := strings.TrimSpace(authExtraString(auth, "dailyChatModel")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("WB_DAILY_CHAT_MODEL")); value != "" {
		return value
	}
	return taskDailyDefaultModel
}

func (p *Provider) dailyChatModels(auth Auth) []string {
	models := make([]string, 0)
	if raw, exists := authExtraJSON(auth, "dailyChatModels"); exists {
		var values []string
		if err := json.Unmarshal(raw, &values); err == nil {
			for _, value := range values {
				value = strings.TrimSpace(value)
				if value != "" {
					models = append(models, value)
				}
			}
		}
	}
	if len(models) == 0 {
		for _, value := range strings.Split(os.Getenv("WB_DAILY_CHAT_MODELS"), ",") {
			value = strings.TrimSpace(value)
			if value != "" {
				models = append(models, value)
			}
		}
	}
	if len(models) == 0 {
		models = []string{p.dailyChatModel(auth)}
	}
	return models
}

func truncateTaskText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len([]rune(text)) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}
