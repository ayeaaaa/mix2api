package workbuddy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Task endpoints. Check-in and daily activity are v1's billing/chat
	// endpoints; growth, Buddy, cat travel, and streak are the CN growth-centre
	// endpoints.
	tasksCheckinPath          = "/v2/billing/meter/daily-checkin"
	tasksCheckinStatusPath    = "/v2/billing/meter/checkin-activity-status"
	tasksGrowthTasksPath      = "/v2/activity/growth/tasks"
	tasksGrowthAcceptPath     = "/v2/activity/growth/tasks/accept"
	tasksGrowthClaimPath      = "/activity/growth/tasks/%s/claim"
	tasksGrowthEnergyPath     = "/v2/activity/growth/energy"
	tasksGrowthStreakPath     = "/activity/growth/streak"
	tasksGrowthV2StreakPath   = "/v2/activity/growth/streak"
	tasksGrowthLotteryPath    = "/activity/growth/lottery/summary"
	tasksGrowthLotteryDraw    = "/activity/growth/lottery/draw"
	tasksGrowthRedeemPath     = "/activity/growth/redeem"
	tasksGrowthHeatmapPath    = "/activity/growth/heatmap"
	tasksGrowthMakeupPath     = "/activity/growth/makeup-cards/use"
	tasksTravelStatusPath     = "/activity/growth/buddy/travel/status"
	tasksTravelConfigPath     = "/activity/growth/buddy/travel/config"
	tasksTravelDepartPath     = "/activity/growth/buddy/travel/depart"
	tasksTravelClaimPath      = "/activity/growth/buddy/travel/claim"
	tasksBuddyQuotaPath       = "/v2/activity/growth/buddy/quota"
	tasksBuddyOpenPath        = "/v2/activity/growth/buddy/open"
	tasksReportPath           = "/v2/report"
	tasksGiftPath             = "/billing/meter/claim-gift"
	tasksCompensationPath     = "/billing/meter/claim-compensation"
	tasksWebConversationsPath = "/console/as/conversations/"

	tasksTimeLayout     = "2006-01-02 15:04:05"
	tasksDayLayout      = "2006-01-02"
	tasksMaxBodyBytes   = 4 << 20
	tasksDefaultTimeout = 15 * time.Second
	tasksSummaryTimeout = 10 * time.Second
	tasksGapDefault     = time.Second
	tasksBackoffDefault = 5 * time.Second
	tasksBackoffMax     = 30 * time.Second
	tasksBudgetDefault  = 600 * time.Second
	tasksTurnWait       = 120 * time.Second
	tasksPollDefault    = 3 * time.Second

	taskTriggerManual        = "manual"
	taskTriggerScheduler     = "scheduler"
	taskTriggerAccountAdd    = "account_add"
	taskTriggerAccountImport = "account_import"

	taskCheckinTask       = "checkin"
	taskDailyChatTask     = "daily_chat"
	taskBusyMessage       = "任务正在执行，请稍后"
	taskDailyDefaultModel = "deepseek-v4.1-flash"

	// taskRestrictedCooldown mirrors v1's fixed 2-hour account restriction.
	taskRestrictedCooldown = 2 * time.Hour
)

// ActivityEvent is one v1-compatible activity-history row attached to a task
// result. Task 9/10 persist it in activity_history; it never carries a token or
// raw request body.
type ActivityEvent struct {
	TS       string `json:"ts"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Realm    string `json:"realm"`
	Task     string `json:"task"`
	Trigger  string `json:"trigger"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// CheckinResult mirrors v1's check-in response plus the account row added by
// the panel route. State is checked/already/pending/failed; Code keeps the
// upstream scalar (number or string) without coercion.
type CheckinResult struct {
	UID      string          `json:"uid"`
	Nickname string          `json:"nickname,omitempty"`
	Realm    string          `json:"realm,omitempty"`
	OK       bool            `json:"ok"`
	Skipped  bool            `json:"skipped,omitempty"`
	Busy     bool            `json:"busy,omitempty"`
	State    string          `json:"state,omitempty"`
	Code     any             `json:"code,omitempty"`
	Msg      string          `json:"msg,omitempty"`
	Error    string          `json:"error,omitempty"`
	Data     any             `json:"data,omitempty"`
	Credits  *CreditSnapshot `json:"credits,omitempty"`
	Checkin  *CheckinStatus  `json:"checkin,omitempty"`
	Activity *ActivityEvent  `json:"activity,omitempty"`
}

// CheckinStatusResult is the read-only checkin-activity-status payload used by
// the panel and the pre-check that prevents a duplicate daily sign-in.
type CheckinStatusResult struct {
	UID      string         `json:"uid"`
	Nickname string         `json:"nickname,omitempty"`
	Realm    string         `json:"realm,omitempty"`
	OK       bool           `json:"ok"`
	Busy     bool           `json:"busy,omitempty"`
	Msg      string         `json:"msg,omitempty"`
	Error    string         `json:"error,omitempty"`
	Checkin  *CheckinStatus `json:"checkin,omitempty"`
	Data     any            `json:"data,omitempty"`
}

// Checkin runs one manual CN personal-account sign-in. An explicit uid is the
// v1 manual action and deliberately forces operator-disabled and task-opted-out
// accounts. The daily timestamp and a best-effort checkin-activity-status probe
// prevent a duplicate sign-in across scheduler restarts. Scheduled callers use
// CheckinAll, which applies enabled/task/restriction filters.
func (p *Provider) Checkin(ctx context.Context, uid string) (CheckinResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return CheckinResult{}, err
	}
	return p.runCheckin(ctx, account, taskTriggerManual)
}

// CheckinAll runs the scheduled/batch sign-in selection: enabled accounts that
// participate in tasks and are not inside a persisted restrictedUntil window.
// Disabled, task-opted-out, and restricted accounts are skipped; manual calls
// use Checkin with an explicit uid instead.
func (p *Provider) CheckinAll(ctx context.Context) ([]CheckinResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: check-in requires a context")
	}
	results := make([]CheckinResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		auth := taskAuthSnapshot(account)
		if auth.Realm != realmCN || taskIsAPIKey(auth) || taskEnterpriseID(auth) != "" {
			continue
		}
		result, err := p.runCheckin(ctx, account, taskTriggerScheduler)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *Provider) runCheckin(ctx context.Context, account *Account, trigger string) (CheckinResult, error) {
	auth := taskAuthSnapshot(account)
	uid := strings.TrimSpace(auth.UID)
	result := CheckinResult{UID: uid, Nickname: strings.TrimSpace(auth.Nickname), Realm: auth.Realm}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
	}()
	if auth.Realm != realmCN {
		result.Error = "checkin is only available for CN realm accounts"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no check-in programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.Error = "enterprise accounts do not support check-in"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	now := p.taskNowTime()
	if last := strings.TrimSpace(auth.LastCheckin); last != "" && strings.HasPrefix(last, now.Format(tasksDayLayout)) {
		result.OK = true
		result.Skipped = true
		result.State = "checked"
		result.Msg = "今日已签到，跳过本轮"
		return result, nil
	}

	// A concurrent process may have signed in without writing the local
	// timestamp (or the file was restored from an older backup). The status
	// endpoint is an authoritative, read-only guard.
	status, statusData, statusErr := p.fetchCheckinStatus(ctx, account)
	if statusErr == nil && status != nil && status.TodayCheckedIn {
		p.persistLastCheckin(account, now)
		p.mergeCheckinStatus(account, status)
		result.OK = true
		result.Skipped = true
		result.State = "already"
		result.Msg = "今日已签到，跳过本轮"
		result.Checkin = status
		result.Data = statusData
		result.Credits = creditSnapshotOf(account)
		return result, nil
	}

	headers := p.taskBillingHeaders(account)
	response, err := p.taskDo(ctx, account, http.MethodPost, p.taskBillingBase()+tasksCheckinPath, headers, []byte("{}"), tasksDefaultTimeout)
	if err != nil {
		result.Error = p.taskRedactError(account, err)
		result.Activity = p.newActivity(account, taskCheckinTask, trigger, false, result.Error)
		return result, nil
	}
	code := taskNumberOrText(response.Payload["code"])
	msg := taskPayloadMessage(response.Payload)
	state := checkinStateValue(code, msg)
	success := state == "checked" || state == "already"
	if response.Status < 200 || response.Status >= 300 {
		// v1 only treats an HTTP error as success when its body says "already";
		// a transport/HTTP failure carrying code 0 is still a failed attempt.
		if state != "already" {
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", response.Status)
			}
			msg = p.taskRedactError(account, errors.New(msg))
			result.State = state
			result.Code = code
			result.Msg = msg
			result.Error = msg
			result.Activity = p.newActivity(account, taskCheckinTask, trigger, false, msg)
			return result, nil
		}
	}
	if !success {
		result.State = state
		result.Code = code
		if msg == "" {
			msg = "check-in failed"
		}
		msg = p.taskRedactError(account, errors.New(msg))
		result.Msg = msg
		result.Error = msg
		result.Activity = p.newActivity(account, taskCheckinTask, trigger, false, msg)
		return result, nil
	}

	// Only a checked/already outcome writes the day stamp.
	if err := p.persistLastCheckin(account, now); err != nil {
		result.Error = p.taskRedactError(account, err)
	}
	data := p.taskRedactValue(account, response.Payload["data"])
	beforeRefresh := previousCheckinStatus(account)
	// Refresh the full balance so the panel sees the new credits immediately.
	// Failure is soft: the sign-in itself already succeeded.
	_ = p.refreshCreditAccount(ctx, account)
	// The refreshed snapshot may carry a newer streak/credit status; use it as
	// the merge baseline while retaining fields the refresh dropped.
	checkin := mergeCheckinStatusValue(mergeCheckinStatuses(beforeRefresh, previousCheckinStatus(account)), data, true)
	p.mergeCheckinStatus(account, checkin)

	result.OK = true
	result.State = state
	result.Code = code
	result.Msg = p.taskRedactError(account, errors.New(msg))
	if result.Msg == "" {
		result.Msg = "ok"
	}
	result.Data = data
	result.Credits = creditSnapshotOf(account)
	if result.Credits != nil {
		result.Checkin = cloneCheckin(result.Credits.Checkin)
	} else {
		result.Checkin = cloneCheckin(checkin)
	}
	result.Activity = p.newActivity(account, taskCheckinTask, trigger, true, result.Msg)
	return result, nil
}

// CheckinStatus reads the billing checkin/activity status without writing any
// local state. It is a panel/diagnostic helper.
func (p *Provider) CheckinStatus(ctx context.Context, uid string) (CheckinStatusResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return CheckinStatusResult{}, err
	}
	auth := taskAuthSnapshot(account)
	result := CheckinStatusResult{
		UID:      strings.TrimSpace(auth.UID),
		Nickname: strings.TrimSpace(auth.Nickname),
		Realm:    auth.Realm,
	}
	if auth.Realm != realmCN {
		result.Error = "checkin is only available for CN realm accounts"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no checkin programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.Error = "enterprise accounts do not support checkin"
		return result, nil
	}
	status, data, err := p.fetchCheckinStatus(ctx, account)
	if err != nil {
		result.Error = p.taskRedactError(account, err)
		return result, nil
	}
	if status == nil {
		result.Error = "checkin status returned no data"
		return result, nil
	}
	result.OK = true
	result.Checkin = status
	result.Data = data
	return result, nil
}

func (p *Provider) fetchCheckinStatus(ctx context.Context, account *Account) (*CheckinStatus, any, error) {
	headers := p.taskBillingHeaders(account)
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskChatBase()+tasksCheckinStatusPath, headers, []byte("{}"), tasksSummaryTimeout)
	if err != nil {
		return nil, nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, nil, fmt.Errorf("workbuddy: checkin status HTTP %d", response.Status)
	}
	if err := taskPayloadError(response.Payload); err != nil {
		return nil, nil, err
	}
	data := p.taskRedactValue(account, response.Payload["data"])
	status := checkinStatusFromData(data)
	return status, data, nil
}

func checkinStatusFromData(data any) *CheckinStatus {
	object, ok := data.(map[string]any)
	if !ok || len(object) == 0 {
		return nil
	}
	return mergeCheckinStatusValue(nil, object, false)
}

// mergeCheckinStatusValue overlays only fields the upstream actually returned.
// A partial status response must not zero the cached streak/credits (e.g. a
// checkin-activity-status timeout followed by a successful checkin).
func mergeCheckinStatusValue(previous *CheckinStatus, data any, checked bool) *CheckinStatus {
	out := cloneCheckin(previous)
	if out == nil {
		out = &CheckinStatus{Active: true}
	}
	object, _ := data.(map[string]any)
	if value, exists := object["today_checked_in"]; exists {
		out.TodayCheckedIn = creditTruthy(value)
	} else if checked {
		out.TodayCheckedIn = true
	}
	if value, exists := object["streak_days"]; exists {
		out.StreakDays = int(creditNumber(value))
	}
	if value, exists := object["daily_credit"]; exists {
		out.DailyCredit = creditNumber(value)
	}
	if value, exists := object["today_credit"]; exists {
		out.TodayCredit = creditNumber(value)
	}
	if value, exists := object["active"]; exists {
		out.Active = creditTruthy(value)
	}
	return out
}

func mergeCheckinStatuses(previous, current *CheckinStatus) *CheckinStatus {
	if current == nil {
		return cloneCheckin(previous)
	}
	out := cloneCheckin(current)
	if previous == nil {
		return out
	}
	// Presence is not carried by the struct; zeros are the only safe signal
	// that the refreshed snapshot omitted a numeric field.
	if out.StreakDays == 0 {
		out.StreakDays = previous.StreakDays
	}
	if out.DailyCredit == 0 {
		out.DailyCredit = previous.DailyCredit
	}
	if out.TodayCredit == 0 {
		out.TodayCredit = previous.TodayCredit
	}
	if !out.Active && previous.Active {
		out.Active = true
	}
	return out
}

func previousCheckinStatus(account *Account) *CheckinStatus {
	if account == nil {
		return nil
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.Credits == nil {
		return nil
	}
	return cloneCheckin(account.Credits.Checkin)
}

// ---------------------------------------------------------------------------
// Persisted 403/11140 restriction
// ---------------------------------------------------------------------------

func taskRestrictedUntil(auth Auth) float64 {
	return authExtraFloat(auth, "restrictedUntil")
}

func (p *Provider) taskRestricted(auth Auth, now time.Time) bool {
	return taskRestrictedUntil(auth) > float64(now.Unix())
}

func (p *Provider) persistTaskRestriction(account *Account, now time.Time, detail string) {
	if p == nil || account == nil {
		return
	}
	if pool := p.Pool(); pool != nil && account.pool != nil {
		// Pool owns the unified runtime + persisted restriction state.
		_ = pool.NoteRestricted(account, detail)
		return
	}
	auth := taskAuthSnapshot(account)
	if taskRestrictedUntil(auth) > float64(now.Unix()) {
		return
	}
	until := now.Add(taskRestrictedCooldown)
	raw, _ := json.Marshal(float64(until.Unix()))
	account.mu.Lock()
	if account.Auth.Extra == nil {
		account.Auth.Extra = make(map[string]json.RawMessage)
	}
	account.Auth.Extra["restrictedUntil"] = raw
	if until.After(account.State.CoolUntil) {
		account.State.CoolUntil = until
	}
	account.mu.Unlock()
}

func (p *Provider) clearTaskRestriction(account *Account) {
	if p == nil || account == nil {
		return
	}
	if pool := p.Pool(); pool != nil && account.pool != nil {
		// A successful request proves the vendor lifted the restriction; clear
		// only the restriction and its account cooldown, preserving model
		// windows, manual disablement, and terminal-token state.
		_ = pool.ClearRestriction(account)
		return
	}
	account.mu.Lock()
	delete(account.Auth.Extra, "restrictedUntil")
	account.State.CoolUntil = time.Time{}
	account.mu.Unlock()
}

func (p *Provider) persistLastCheckin(account *Account, now time.Time) error {
	uid := accountUID(account)
	stamp := now.Format(tasksTimeLayout)
	if p == nil || p.Pool() == nil || uid == "" {
		account.mu.Lock()
		account.Auth.LastCheckin = stamp
		account.mu.Unlock()
		return nil
	}
	return p.Pool().mutateAccount(uid, func(account *Account) {
		account.Auth.LastCheckin = stamp
	})
}

func (p *Provider) mergeCheckinStatus(account *Account, status *CheckinStatus) {
	if p == nil || account == nil || status == nil {
		return
	}
	uid := accountUID(account)
	snapshot := creditSnapshotOf(account)
	if snapshot == nil {
		now := p.creditsNowTime()
		snapshot = &CreditSnapshot{
			Checkin:    cloneCheckin(status),
			Packages:   []CreditPackage{},
			UpdatedAt:  float64(now.UnixNano()) / float64(time.Second),
			UpdatedISO: now.Format(creditsTimeLayout),
		}
	} else {
		snapshot.Checkin = cloneCheckin(status)
	}
	_ = p.Pool().setCreditsAndTier(uid, snapshot, nil)
}

// checkinStateValue is v1's checkin_state classifier. A pending receipt never
// writes the day stamp, and code 10001 is only "already" when the text says so.
func checkinStateValue(code any, msg string) string {
	text := strings.TrimSpace(msg)
	pendingHints := []string{"处理中", "请勿重复操作", "系统繁忙", "稍后再试"}
	for _, hint := range pendingHints {
		if strings.Contains(text, hint) {
			return "pending"
		}
	}
	number := anyCodeNumber(code)
	if number == 0 {
		if strings.Contains(text, "失败") {
			return "failed"
		}
		return "checked"
	}
	if number == 10001 {
		if strings.Contains(text, "已签到") {
			return "already"
		}
		return "failed"
	}
	return "failed"
}

// ---------------------------------------------------------------------------
// Shared task HTTP / persistence helpers
// ---------------------------------------------------------------------------

func (p *Provider) taskAccount(ctx context.Context, uid string) (*Account, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	pool := p.Pool()
	if pool == nil {
		return nil, errors.New("workbuddy: account pool is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: task operation requires a context")
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, ErrAccountNotFound
	}
	account := pool.Get(uid)
	if account == nil {
		return nil, ErrAccountNotFound
	}
	return account, nil
}

func (p *Provider) allTaskAccounts() []*Account {
	if p == nil {
		return nil
	}
	pool := p.Pool()
	if pool == nil {
		return nil
	}
	out := make([]*Account, 0)
	now := pool.nowTime()
	for _, account := range pool.Accounts() {
		account.mu.RLock()
		active := !account.removed && account.Auth.Enabled && !account.State.Disabled &&
			account.Auth.TasksEnabled && !account.State.CoolUntil.After(now)
		auth := cloneAuth(account.Auth)
		account.mu.RUnlock()
		if active && taskRestrictedUntil(auth) > float64(now.Unix()) {
			active = false
		}
		if active {
			out = append(out, account)
		}
	}
	return out
}

func taskProxyURL(account *Account) string {
	if account == nil {
		return ""
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return strings.TrimSpace(account.Auth.ProxyURL)
}

func taskAuthSnapshot(account *Account) Auth {
	if account == nil {
		return Auth{}
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return cloneAuth(account.Auth)
}

func taskIsAPIKey(auth Auth) bool {
	return auth.APIKey || IsAPIKeyToken(auth.AccessToken)
}

func taskEnterpriseID(auth Auth) string {
	if value := strings.TrimSpace(auth.EnterpriseID); value != "" {
		return value
	}
	return strings.TrimSpace(parseJWTClaims(auth.AccessToken).EnterpriseID)
}

func (p *Provider) taskClient() ContextHTTPDoer {
	if p == nil {
		return defaultProfileTransport
	}
	if p.tasksDoer != nil {
		return p.tasksDoer
	}
	// Reuse the existing same-package seams so a test that only knows the chat
	// or credits fake still keeps task requests off the real network.
	if p.chatDoer != nil {
		return p.chatDoer
	}
	if p.creditsDoer != nil {
		return p.creditsDoer
	}
	if p.transport != nil {
		return p.transport
	}
	return defaultProfileTransport
}

func (p *Provider) taskTimeout(defaultTimeout time.Duration) time.Duration {
	if p != nil && p.tasksTimeout > 0 {
		return p.tasksTimeout
	}
	return defaultTimeout
}

func (p *Provider) taskNowTime() time.Time {
	if p != nil && p.tasksNow != nil {
		return p.tasksNow()
	}
	return time.Now()
}

func (p *Provider) taskSleepFor(delay time.Duration) {
	if delay <= 0 {
		return
	}
	if p != nil && p.tasksSleep != nil {
		p.tasksSleep(delay)
		return
	}
	time.Sleep(delay)
}

func (p *Provider) taskGapDelay() time.Duration {
	if p == nil || p.tasksGap <= 0 {
		return 0
	}
	return p.tasksGap
}

func (p *Provider) taskBackoffDelay(round int) time.Duration {
	if p == nil || p.tasksBackoff <= 0 {
		return 0
	}
	base := p.tasksBackoff
	capValue := p.tasksBackoffMax
	if capValue <= 0 {
		capValue = tasksBackoffMax
	}
	if capValue < base {
		capValue = base
	}
	delay := base
	for i := 0; i < round; i++ {
		delay *= 2
		if delay >= capValue {
			return capValue
		}
	}
	if delay > capValue {
		return capValue
	}
	return delay
}

func (p *Provider) taskChatBase() string {
	if p == nil {
		return ""
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tasksChatBaseURL), "/"); base != "" {
		return base
	}
	if base := strings.TrimRight(strings.TrimSpace(p.chatBaseURL), "/"); base != "" {
		return base
	}
	if base := strings.TrimRight(strings.TrimSpace(p.creditsCheckinBaseURL), "/"); base != "" {
		return base
	}
	return strings.TrimRight(p.EndpointFor(productDesktop), "/")
}

func (p *Provider) taskBillingBase() string {
	if p == nil {
		return ""
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tasksBillingBaseURL), "/"); base != "" {
		return base
	}
	if base := strings.TrimRight(strings.TrimSpace(p.creditsBillingBaseURL), "/"); base != "" {
		return base
	}
	// A test or a single-origin deployment may point the whole task slice at
	// one server; never let a task request escape to the real billing host.
	if base := strings.TrimRight(strings.TrimSpace(p.tasksChatBaseURL), "/"); base != "" {
		return base
	}
	return p.creditsBillingBase()
}

func (p *Provider) taskWebBase() string {
	if p == nil {
		return ""
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tasksWebBaseURL), "/"); base != "" {
		return base
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tasksChatBaseURL), "/"); base != "" {
		return base
	}
	if base := strings.TrimRight(strings.TrimSpace(p.chatBaseURL), "/"); base != "" {
		return base
	}
	if p.realm == realmCN {
		return upstreamBaseOverride("WB_TASK_WEB_UPSTREAM", "https://www.workbuddy.cn")
	}
	return upstreamBaseOverride("WB_TASK_WEB_UPSTREAM", "https://www.workbuddy.ai")
}

type taskHTTPResponse struct {
	Status  int
	Payload map[string]any
	Body    string
}

func (p *Provider) taskDo(ctx context.Context, account *Account, method, endpoint string, headers http.Header, body []byte, timeout time.Duration) (taskHTTPResponse, error) {
	status, text, err := p.taskDoRaw(ctx, account, method, endpoint, headers, body, timeout)
	if err != nil {
		return taskHTTPResponse{}, err
	}
	result := taskHTTPResponse{Status: status, Body: text}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return result, nil
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&result.Payload); err != nil {
		if status >= 200 && status < 300 {
			return result, errors.New("workbuddy: task response is not a JSON object")
		}
		result.Payload = nil
		return result, nil
	}
	if status >= 200 && status < 300 && result.Payload == nil {
		return result, errors.New("workbuddy: task response is not a JSON object")
	}
	return result, nil
}

// taskDoRaw performs the request without assuming a JSON response. The daily
// desktop chat is text/event-stream even in v1's urllib flow (it only reads the
// body), so it must not be forced through taskDo's JSON decoder.
func (p *Provider) taskDoRaw(ctx context.Context, account *Account, method, endpoint string, headers http.Header, body []byte, timeout time.Duration) (int, string, error) {
	client := p.taskClient()
	if client == nil {
		return 0, "", errors.New("workbuddy: task HTTP client is nil")
	}
	requestCtx := nonNilContext(ctx)
	if timeout <= 0 {
		timeout = tasksDefaultTimeout
	}
	if p != nil {
		timeout = p.taskTimeout(timeout)
	}
	requestCtx, cancel := context.WithTimeout(requestCtx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint, reader)
	if err != nil {
		return 0, "", errors.New("workbuddy: task request is invalid")
	}
	request.Header = headers.Clone()
	proxyURL := ""
	if account != nil {
		account.mu.RLock()
		proxyURL = strings.TrimSpace(account.Auth.ProxyURL)
		account.mu.RUnlock()
	}
	response, err := client.Do(requestCtx, request, proxyURL)
	if err != nil {
		return 0, "", errors.New("workbuddy: task request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, tasksMaxBodyBytes+1))
	if err != nil {
		return response.StatusCode, "", errors.New("workbuddy: task response read failed")
	}
	if len(raw) > tasksMaxBodyBytes {
		return response.StatusCode, "", errors.New("workbuddy: task response too large")
	}
	return response.StatusCode, string(raw), nil
}

func taskPayloadMessage(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	return strings.TrimSpace(creditText(payload["msg"]))
}

func taskCodeIsZero(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	code, exists := payload["code"]
	if !exists {
		return false
	}
	return anyCodeNumber(code) == 0
}

func taskPayloadError(payload map[string]any) error {
	if payload == nil {
		return nil
	}
	code := payload["code"]
	if code == nil {
		return nil
	}
	number := anyCodeNumber(code)
	if number == 0 {
		return nil
	}
	message := taskPayloadMessage(payload)
	if message == "" {
		message = fmt.Sprintf("code=%v", code)
	}
	return fmt.Errorf("workbuddy: %s", message)
}

func taskNumberOrText(value any) any {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return number
		}
		if number, err := typed.Float64(); err == nil {
			return number
		}
		return typed.String()
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed)
		}
		return typed
	case float32:
		return taskNumberOrText(float64(typed))
	default:
		return value
	}
}

func anyCodeNumber(value any) float64 {
	if value == nil {
		return -1
	}
	number, ok := numericValue(value)
	if !ok {
		return -1
	}
	return number
}

func (p *Provider) taskBillingHeaders(account *Account) http.Header {
	state := creditAccountStateOf(account)
	headers := p.creditHeaders(state)
	return headers
}

func (p *Provider) taskChatHeaders(account *Account, mp bool) http.Header {
	auth := taskAuthSnapshot(account)
	product := normalizeProduct(auth.Product)
	deviceToken, deviceTokenError := p.chatDeviceToken(auth, product)
	headers := p.chatHeaders(auth, product, "", "", "", deviceToken, deviceTokenError, p.Config())
	if mp {
		headers.Set("X-Client-Platform", "miniprogram")
		headers.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14; MicroMessenger/8.0.49 WeChat/0.8.0 MiniProgramEnv/android; wkbrowser xweb)")
	}
	return headers
}

func (p *Provider) taskWebHeaders(account *Account) http.Header {
	auth := taskAuthSnapshot(account)
	origins := p.taskWebBase()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+auth.AccessToken)
	headers.Set("X-User-Id", strings.TrimSpace(auth.UID))
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("X-Request-ID", randomHex(16))
	headers.Set("Origin", origins)
	headers.Set("Referer", origins+"/app")
	for key, values := range webBrowserHeaders(auth.UID, auth.Realm) {
		for _, value := range values {
			headers.Set(key, value)
		}
	}
	return headers
}

const taskWebUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"

// taskWebClaimHeaders is v1's explicit web-domain growth-centre claim request:
// unlike the generic web session headers it carries X-Domain, the web client
// platform, and the growth-centre referer.
func (p *Provider) taskWebClaimHeaders(account *Account) http.Header {
	auth := taskAuthSnapshot(account)
	origin := p.taskWebBase()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+auth.AccessToken)
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("Content-Type", "application/json")
	headers.Set("Origin", origin)
	headers.Set("Referer", origin+"/profile/growth-center")
	headers.Set("x-client-platform", "web")
	headers.Set("User-Agent", taskWebUserAgent)
	headers.Set("X-User-Id", strings.TrimSpace(auth.UID))
	headers.Set("X-Domain", origin)
	return headers
}

// taskRedactError removes this account's known credentials and proxy userinfo
// from an upstream-derived error string.
func (p *Provider) taskRedactError(account *Account, err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if account == nil {
		return text
	}
	account.mu.RLock()
	values := accountSecretValuesLocked(account)
	account.mu.RUnlock()
	return redactAccountText(text, values)
}

func (p *Provider) scrubTaskText(account *Account, text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	return p.taskRedactError(account, errors.New(text))
}

func (p *Provider) scrubTaskLogs(account *Account, logs []string) []string {
	if logs == nil {
		return nil
	}
	out := make([]string, len(logs))
	for index, line := range logs {
		out[index] = p.scrubTaskText(account, line)
	}
	return out
}

func (p *Provider) taskRedactValue(account *Account, value any) any {
	if account == nil {
		return value
	}
	account.mu.RLock()
	values := accountSecretValuesLocked(account)
	account.mu.RUnlock()
	return redactTaskValue(value, values)
}

var taskSecretKeys = map[string]bool{
	"accesstoken": true, "access_token": true, "access-token": true,
	"refreshtoken": true, "refresh_token": true, "refresh-token": true,
	"authorization": true, "cookie": true, "set-cookie": true,
	"password": true, "secret": true,
	"apikey": true, "api_key": true, "api-key": true,
	"token": true, "x-api-key": true,
	"devicetoken": true, "device_token": true, "device-token": true,
	"x-device-token": true,
}

func redactTaskValue(value any, secrets []string) any {
	switch typed := value.(type) {
	case string:
		return secretsRedactText(typed, secrets)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = redactTaskValue(item, secrets)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if taskSecretKeys[strings.ToLower(strings.TrimSpace(key))] {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactTaskValue(item, secrets)
		}
		return out
	default:
		return value
	}
}

func secretsRedactText(text string, secrets []string) string {
	if len(secrets) == 0 {
		return text
	}
	return redactAccountText(text, secrets)
}

func (p *Provider) newActivity(account *Account, task, trigger string, ok bool, message string) *ActivityEvent {
	if account == nil {
		return nil
	}
	auth := taskAuthSnapshot(account)
	numericUID := strings.TrimSpace(auth.UID)
	if numericUID == "" {
		return nil
	}
	if strings.TrimSpace(trigger) == "" {
		trigger = "unknown"
	}
	message = p.taskRedactError(account, errors.New(strings.TrimSpace(message)))
	message = strings.Join(strings.Fields(message), " ")
	if len([]rune(message)) > 200 {
		message = string([]rune(message)[:200])
	}
	return &ActivityEvent{
		TS:       p.taskNowTime().Format(time.RFC3339),
		UID:      numericUID,
		Nickname: strings.TrimSpace(auth.Nickname),
		Realm:    auth.Realm,
		Task:     task,
		Trigger:  trigger,
		OK:       ok,
		Message:  message,
	}
}

func authExtraString(auth Auth, key string) string {
	if len(auth.Extra) == 0 {
		return ""
	}
	raw, exists := auth.Extra[key]
	if !exists {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ""
	}
	return text
}

func authExtraJSON(auth Auth, key string) (json.RawMessage, bool) {
	if len(auth.Extra) == 0 {
		return nil, false
	}
	raw, exists := auth.Extra[key]
	if !exists || len(strings.TrimSpace(string(raw))) == 0 {
		return nil, false
	}
	return raw, true
}

func authExtraFloat(auth Auth, key string) float64 {
	raw, exists := authExtraJSON(auth, key)
	if !exists {
		return 0
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0
	}
	return number
}

func authExtraBool(auth Auth, key string) bool {
	raw, exists := authExtraJSON(auth, key)
	if !exists {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}

func taskUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32]
}
