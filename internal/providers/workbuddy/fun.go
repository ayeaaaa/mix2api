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

// BuddyResult is one Buddy blind-box opening attempt (v1 do_buddy_open).
type BuddyResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
	OK       bool   `json:"ok"`
	Skipped  bool   `json:"skipped,omitempty"`
	Busy     bool   `json:"busy,omitempty"`
	Action   string `json:"action,omitempty"`
	Opened   int    `json:"opened"`
	Name     string `json:"name,omitempty"`
	Msg      string `json:"msg,omitempty"`
	Error    string `json:"error,omitempty"`
}

// CatResult is one cat-travel lifecycle result (v1 do_cat_travel).
type CatResult struct {
	UID      string  `json:"uid"`
	Nickname string  `json:"nickname,omitempty"`
	Realm    string  `json:"realm,omitempty"`
	OK       bool    `json:"ok"`
	Skipped  bool    `json:"skipped,omitempty"`
	Busy     bool    `json:"busy,omitempty"`
	Action   string  `json:"action,omitempty"`
	State    string  `json:"state,omitempty"`
	Credit   float64 `json:"credit,omitempty"`
	Location string  `json:"location,omitempty"`
	Msg      string  `json:"msg,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// StreakResult is one streak-manager run (v1 run_streak_bonus).
type StreakResult struct {
	UID        string   `json:"uid"`
	Nickname   string   `json:"nickname,omitempty"`
	Realm      string   `json:"realm,omitempty"`
	OK         bool     `json:"ok"`
	Skipped    bool     `json:"skipped,omitempty"`
	Busy       bool     `json:"busy,omitempty"`
	Msg        string   `json:"msg,omitempty"`
	Error      string   `json:"error,omitempty"`
	Logs       []string `json:"logs"`
	MakeupUsed bool     `json:"makeup_used,omitempty"`
	Redeemed   []string `json:"redeemed"`
	Drawn      int      `json:"drawn,omitempty"`
}

// BuddyCheckin opens affordable Buddy blind boxes. The upstream quota/daily
// limits are authoritative; one account lock keeps a duplicate scheduler tick
// from opening twice from the same stale quota.
func (p *Provider) BuddyCheckin(ctx context.Context, uid string) (BuddyResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return BuddyResult{}, err
	}
	return p.runBuddyCheckin(ctx, account)
}

// BuddyCheckinAll runs the blind-box check for every enabled, task-participating,
// non-restricted CN account in this provider realm.
func (p *Provider) BuddyCheckinAll(ctx context.Context) ([]BuddyResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: Buddy check requires a context")
	}
	results := make([]BuddyResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if auth := taskAuthSnapshot(account); !growthTaskEligible(auth) {
			continue
		}
		result, err := p.runBuddyCheckin(ctx, account)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *Provider) runBuddyCheckin(ctx context.Context, account *Account) (BuddyResult, error) {
	auth := taskAuthSnapshot(account)
	result := BuddyResult{UID: auth.UID, Nickname: auth.Nickname, Realm: auth.Realm}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
		result.Name = p.scrubTaskText(account, result.Name)
	}()
	if auth.Realm != realmCN {
		result.Error = "Buddy growth is only available for CN accounts"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no growth programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.OK = true
		result.Skipped = true
		result.Action = "skip"
		result.Msg = "企业版账号无成长体系，跳过"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	headers := p.taskChatHeaders(account, false)
	quotaResponse, err := p.taskDo(ctx, account, http.MethodGet,
		p.taskChatBase()+tasksBuddyQuotaPath, headers, nil, tasksSummaryTimeout)
	if err != nil {
		result.Error = p.taskRedactError(account, err)
		return result, nil
	}
	if quotaResponse.Status < 200 || quotaResponse.Status >= 300 {
		result.Error = fmt.Sprintf("Buddy 盲盒额度查询失败: HTTP %d", quotaResponse.Status)
		return result, nil
	}
	if taskPayloadError(quotaResponse.Payload) != nil {
		// v1 treats a business-level quota rejection as "nothing affordable"
		// and reports idle rather than a hard error; HTTP/transport failures
		// above remain errors.
		result.OK = true
		result.Action = "idle"
		result.Msg = "成长能量不足以开启 Buddy 盲盒"
		return result, nil
	}
	quota := creditObject(quotaResponse.Payload["data"])
	affordable := int(creditNumber(quota["affordable"]))
	maxOpen := int(creditNumber(quota["max_open_count"]))
	if maxOpen < 1 {
		maxOpen = 1
	}
	if affordable <= 0 {
		result.OK = true
		result.Action = "idle"
		result.Msg = "成长能量不足以开启 Buddy 盲盒"
		return result, nil
	}
	count := affordable
	if count > maxOpen {
		count = maxOpen
	}
	body, _ := json.Marshal(map[string]any{"count": count, "client_token": taskUUID()})
	openResponse, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskChatBase()+tasksBuddyOpenPath, headers, body, tasksDefaultTimeout)
	if err != nil {
		result.Error = p.taskRedactError(account, err)
		return result, nil
	}
	if openResponse.Status < 200 || openResponse.Status >= 300 {
		result.Error = fmt.Sprintf("开盲盒失败: HTTP %d: %s", openResponse.Status, taskPayloadMessage(openResponse.Payload))
		return result, nil
	}
	if err := taskPayloadError(openResponse.Payload); err != nil {
		result.Error = "开盲盒失败: " + p.taskRedactError(account, err)
		return result, nil
	}
	data := creditObject(openResponse.Payload["data"])
	name := creditText(data["buddy"])
	if name == "" {
		name = creditText(data["name"])
	}
	if name == "" {
		if buddies, ok := anyList(data["buddies"]); ok && len(buddies) > 0 {
			parts := make([]string, 0, len(buddies))
			for _, item := range buddies {
				parts = append(parts, creditText(item))
			}
			name = strings.Join(parts, ", ")
		}
	}
	if name == "" {
		name = "新 Buddy"
	}
	result.OK = true
	result.Action = "open"
	result.Opened = count
	result.Name = p.scrubTaskText(account, name)
	result.Msg = p.scrubTaskText(account, fmt.Sprintf("开启 Buddy 盲盒 ×%d 成功 (%s)", count, name))
	return result, nil
}

// CatTravel performs v1's cat-travel lifecycle: claim an arrived trip, or
// depart when idle. A missing Buddy is repaired through first_buddy and the
// departure is retried exactly once.
func (p *Provider) CatTravel(ctx context.Context, uid string) (CatResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return CatResult{}, err
	}
	return p.runCatTravel(ctx, account)
}

// CatTravelAll runs the cat lifecycle for every enabled, task-participating,
// non-restricted CN account in this provider realm.
func (p *Provider) CatTravelAll(ctx context.Context) ([]CatResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: cat travel requires a context")
	}
	results := make([]CatResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if auth := taskAuthSnapshot(account); !growthTaskEligible(auth) {
			continue
		}
		result, err := p.runCatTravel(ctx, account)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *Provider) runCatTravel(ctx context.Context, account *Account) (CatResult, error) {
	auth := taskAuthSnapshot(account)
	result := CatResult{UID: auth.UID, Nickname: auth.Nickname, Realm: auth.Realm}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
		result.Location = p.scrubTaskText(account, result.Location)
	}()
	if auth.Realm != realmCN {
		result.Error = "cat travel is only available for CN accounts"
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no growth programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.OK = true
		result.Skipped = true
		result.Action = "skip"
		result.Msg = "企业版账号无成长体系，跳过"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	status, err := p.fetchTravelStatus(ctx, account)
	if err != nil {
		result.Error = "查询旅行状态失败: " + p.taskRedactError(account, err)
		return result, nil
	}
	state := strings.ToLower(strings.TrimSpace(creditText(status["state"])))
	result.State = state
	switch state {
	case "arrived":
		headers := p.taskChatHeaders(account, false)
		response, claimErr := p.taskDo(ctx, account, http.MethodPost,
			p.taskChatBase()+tasksTravelClaimPath, headers, []byte("{}"), tasksDefaultTimeout)
		if claimErr != nil {
			result.Error = "领奖失败: " + p.taskRedactError(account, claimErr)
			return result, nil
		}
		if response.Status < 200 || response.Status >= 300 {
			result.Error = fmt.Sprintf("领奖失败: HTTP %d", response.Status)
			return result, nil
		}
		if err := taskPayloadError(response.Payload); err != nil {
			result.Error = "领奖失败: " + p.taskRedactError(account, err)
			return result, nil
		}
		credit := creditNumber(creditObject(response.Payload["data"])["reward_credit"])
		_ = p.refreshCreditAccount(ctx, account)
		result.OK = true
		result.Action = "claim"
		result.Credit = credit
		result.Msg = p.scrubTaskText(account, fmt.Sprintf("旅行归来领奖成功！获得 %g 积分", credit))
		return result, nil
	case "idle":
		if creditTruthy(status["daily_limit_reached"]) {
			result.OK = true
			result.Action = "idle"
			result.Msg = "猫猫今日已完成旅行，明日 00:00 刷新"
			return result, nil
		}
		locationID := p.firstTravelLocation(ctx, account)
		data, departErr := p.departTravel(ctx, account, locationID)
		if departErr != nil && strings.Contains(strings.ToLower(departErr.Error()), "no active buddy") {
			if p.ensureGrowthBuddy(ctx, account) {
				data, departErr = p.departTravel(ctx, account, locationID)
			} else {
				result.Error = "还没有 Buddy：需先完成并领取「领取一只 Buddy」任务（新建任务并发起对话）"
				return result, nil
			}
		}
		if departErr != nil {
			result.Error = "派出旅行失败: " + p.taskRedactError(account, departErr)
			return result, nil
		}
		location := creditText(creditObject(data["location"])["name"])
		result.OK = true
		result.Action = "depart"
		result.Location = p.scrubTaskText(account, location)
		result.Msg = p.scrubTaskText(account, fmt.Sprintf("猫猫已出发前往「%s」，预计数小时后归来！", location))
		return result, nil
	case "traveling":
		result.OK = true
		result.Action = "traveling"
		result.Msg = "猫猫正在旅行途中，请稍后再来查看！"
		return result, nil
	default:
		result.OK = true
		result.Action = state
		result.Msg = fmt.Sprintf("当前状态: %s", firstNonEmpty(state, "unknown"))
		return result, nil
	}
}

func (p *Provider) fetchTravelStatus(ctx context.Context, account *Account) (map[string]any, error) {
	headers := p.taskChatHeaders(account, false)
	response, err := p.taskDo(ctx, account, http.MethodGet,
		p.taskChatBase()+tasksTravelStatusPath, headers, nil, tasksSummaryTimeout)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, fmt.Errorf("HTTP %d", response.Status)
	}
	if err := taskPayloadError(response.Payload); err != nil {
		return nil, err
	}
	return creditObject(response.Payload["data"]), nil
}

func (p *Provider) firstTravelLocation(ctx context.Context, account *Account) any {
	headers := p.taskChatHeaders(account, false)
	response, err := p.taskDo(ctx, account, http.MethodGet,
		p.taskChatBase()+tasksTravelConfigPath, headers, nil, tasksSummaryTimeout)
	if err == nil && response.Status >= 200 && response.Status < 300 {
		if locations, ok := anyList(creditObject(response.Payload["data"])["locations"]); ok && len(locations) > 0 {
			if location := creditObject(locations[0]); location != nil {
				if id, exists := location["id"]; exists && id != nil {
					return taskNumberOrText(id)
				}
			}
		}
	}
	return 1
}

func (p *Provider) departTravel(ctx context.Context, account *Account, locationID any) (map[string]any, error) {
	headers := p.taskChatHeaders(account, false)
	body, _ := json.Marshal(map[string]any{"location_id": locationID})
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskChatBase()+tasksTravelDepartPath, headers, body, tasksSummaryTimeout)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		message := taskPayloadMessage(response.Payload)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", response.Status)
		}
		return nil, errors.New(message)
	}
	if err := taskPayloadError(response.Payload); err != nil {
		return nil, err
	}
	return creditObject(response.Payload["data"]), nil
}

// ensureGrowthBuddy completes and claims first_buddy when the account does not
// yet have a Buddy. It is called while the cat-travel task lock is already held.
func (p *Provider) ensureGrowthBuddy(ctx context.Context, account *Account) bool {
	auth := taskAuthSnapshot(account)
	now := p.taskNowTime()
	run := &growthRun{
		provider: p, account: account, auth: auth, now: now, autoClaim: true,
		claims: loadGrowthClaims(auth), tasks: []GrowthTask{},
		summary: cachedGrowthSummary(auth),
	}
	tasks, err := p.fetchGrowthTasks(ctx, account, false)
	if err != nil {
		return false
	}
	run.tasks = tasks
	var buddy *GrowthTask
	for index := range run.tasks {
		if run.tasks[index].TaskCode == "first_buddy" {
			candidate := run.tasks[index]
			buddy = &candidate
			break
		}
	}
	if buddy == nil {
		return false
	}
	if buddy.Status == "claimed" || growthClaimedToday(run.claims, "first_buddy", now) {
		return true
	}
	p.progressGrowthTask(ctx, run, *buddy)
	// Persist the partial attempt even when the claim has not landed yet.
	_ = p.persistGrowthState(account, run)
	refreshed, err := p.fetchGrowthTasks(ctx, account, false)
	if err != nil {
		return false
	}
	run.tasks = mergeGrowthTasks(run.tasks, refreshed)
	claimed := growthClaimedToday(run.claims, "first_buddy", now)
	for _, task := range refreshed {
		if task.TaskCode == "first_buddy" && task.Status == "claimed" {
			claimed = true
			break
		}
	}
	_ = p.persistGrowthState(account, run)
	return claimed
}

// StreakBonus performs v1's idempotent streak loop: yesterday's makeup card,
// one-time gift/compensation, 7d/14d/28d redemption, and every lottery chance.
func (p *Provider) StreakBonus(ctx context.Context, uid string) (StreakResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return StreakResult{}, err
	}
	return p.runStreakBonus(ctx, account)
}

// StreakBonusAll runs the streak loop for every enabled, task-participating,
// non-restricted CN account in this provider realm.
func (p *Provider) StreakBonusAll(ctx context.Context) ([]StreakResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: streak bonus requires a context")
	}
	results := make([]StreakResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if auth := taskAuthSnapshot(account); !growthTaskEligible(auth) {
			continue
		}
		result, err := p.runStreakBonus(ctx, account)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *Provider) runStreakBonus(ctx context.Context, account *Account) (StreakResult, error) {
	auth := taskAuthSnapshot(account)
	result := StreakResult{
		UID: auth.UID, Nickname: auth.Nickname, Realm: auth.Realm,
		Logs: []string{}, Redeemed: []string{},
	}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
		result.Logs = p.scrubTaskLogs(account, result.Logs)
	}()
	if auth.Realm != realmCN {
		result.Error = "连登管家仅限国内版账号"
		result.Msg = result.Error
		return result, nil
	}
	if taskIsAPIKey(auth) {
		result.Error = "API key accounts have no growth programme"
		return result, nil
	}
	if taskEnterpriseID(auth) != "" {
		result.OK = true
		result.Skipped = true
		result.Msg = "企业版账号无成长体系，跳过"
		return result, nil
	}
	if !account.taskMu.TryLock() {
		result.Busy = true
		result.Msg = taskBusyMessage
		return result, nil
	}
	defer account.taskMu.Unlock()

	logs := make([]string, 0)
	headers := p.taskChatHeaders(account, false)
	base := p.taskChatBase()
	now := p.taskNowTime()

	// 0. Make up yesterday when a card exists and the cell was empty.
	if p.makeupYesterday(ctx, account, headers, base, now) {
		result.MakeupUsed = true
		logs = append(logs, fmt.Sprintf("已消耗 1 张补签卡补签昨日 (%s)", now.Add(-24*time.Hour).Format(tasksDayLayout)))
	}

	// 1. One-time gift and compensation (business errors are silent in v1).
	billingHeaders := p.taskBillingHeaders(account)
	for _, item := range []struct{ path, name string }{
		{tasksGiftPath, "新手礼包"},
		{tasksCompensationPath, "活动补偿"},
	} {
		response, err := p.taskDo(ctx, account, http.MethodPost,
			p.taskBillingBase()+item.path, billingHeaders, []byte("{}"), tasksSummaryTimeout)
		if err != nil || response.Status < 200 || response.Status >= 300 {
			continue
		}
		if taskPayloadError(response.Payload) == nil && taskCodeIsZero(response.Payload) {
			credit := creditNumber(creditObject(response.Payload["data"])["credit"])
			logs = append(logs, fmt.Sprintf("领取%s +%g积分", item.name, credit))
		}
	}

	// 2. Redeem unlocked 7d/14d/28d streaks. Already claimed/locked tiers skip.
	if streakData, err := p.fetchStreakData(ctx, account, headers, base); err == nil {
		redemption := creditObject(streakData["redemption_status"])
		if tiers, ok := anyList(redemption["tiers"]); ok {
			for _, rawTier := range tiers {
				tier := creditObject(rawTier)
				name := strings.TrimSpace(creditText(tier["tier"]))
				if name == "" {
					continue
				}
				status := strings.TrimSpace(creditText(redemption["tier_"+name+"_status"]))
				if status == "" {
					status = strings.TrimSpace(creditText(tier["status"]))
				}
				if status == "locked" || status == "claimed" {
					continue
				}
				body, _ := json.Marshal(map[string]any{"tier": name, "client_token": taskUUID()})
				response, err := p.taskDo(ctx, account, http.MethodPost,
					base+tasksGrowthRedeemPath, headers, body, tasksSummaryTimeout)
				if err == nil && response.Status >= 200 && response.Status < 300 &&
					taskPayloadError(response.Payload) == nil && taskCodeIsZero(response.Payload) {
					result.Redeemed = append(result.Redeemed, name)
					logs = append(logs, fmt.Sprintf("成功兑换连登 %s 档位", name))
				}
			}
		}
	}

	// 3. Draw every lottery chance.
	if lotteryResponse, err := p.taskDo(ctx, account, http.MethodGet,
		base+tasksGrowthLotteryPath, headers, nil, tasksSummaryTimeout); err == nil &&
		lotteryResponse.Status >= 200 && lotteryResponse.Status < 300 {
		chances := int(creditNumber(creditObject(lotteryResponse.Payload["data"])["chances"]))
		for index := 0; index < chances; index++ {
			body, _ := json.Marshal(map[string]any{"client_token": taskUUID()})
			response, err := p.taskDo(ctx, account, http.MethodPost,
				base+tasksGrowthLotteryDraw, headers, body, tasksSummaryTimeout)
			if err != nil || response.Status < 200 || response.Status >= 300 ||
				taskPayloadError(response.Payload) != nil || !taskCodeIsZero(response.Payload) {
				break
			}
			result.Drawn++
			p.taskSleepFor(time.Second)
		}
		if result.Drawn > 0 {
			logs = append(logs, fmt.Sprintf("完成成长抽奖 %d 次", result.Drawn))
		}
	}

	result.OK = true
	result.Logs = p.scrubTaskLogs(account, logs)
	if len(logs) == 0 {
		result.Msg = "连登状态正常（无需兑换/无抽奖次数）"
	} else {
		result.Msg = p.scrubTaskText(account, strings.Join(logs, "; "))
	}
	return result, nil
}

func (p *Provider) fetchStreakData(ctx context.Context, account *Account, headers http.Header, base string) (map[string]any, error) {
	response, err := p.taskDo(ctx, account, http.MethodGet, base+tasksGrowthStreakPath, headers, nil, tasksSummaryTimeout)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, fmt.Errorf("HTTP %d", response.Status)
	}
	if err := taskPayloadError(response.Payload); err != nil {
		return nil, err
	}
	return creditObject(response.Payload["data"]), nil
}

func (p *Provider) makeupYesterday(ctx context.Context, account *Account, headers http.Header, base string, now time.Time) bool {
	response, err := p.taskDo(ctx, account, http.MethodGet, base+tasksGrowthHeatmapPath, headers, nil, tasksSummaryTimeout)
	if err != nil || response.Status < 200 || response.Status >= 300 {
		return false
	}
	cells, ok := anyList(creditObject(response.Payload["data"])["cells"])
	if !ok {
		return false
	}
	yesterday := now.Add(-24 * time.Hour).Format(tasksDayLayout)
	empty := false
	for _, rawCell := range cells {
		cell := creditObject(rawCell)
		if cell == nil || creditText(cell["date"]) != yesterday {
			continue
		}
		empty = creditNumber(cell["score"]) == 0
		break
	}
	if !empty {
		return false
	}
	streakData, err := p.fetchStreakData(ctx, account, headers, base)
	if err != nil {
		return false
	}
	cards := creditNumber(creditObject(streakData["makeup_cards"])["balance"])
	if cards <= 0 {
		return false
	}
	body, _ := json.Marshal(map[string]any{"target_date": yesterday})
	useResponse, err := p.taskDo(ctx, account, http.MethodPost,
		base+tasksGrowthMakeupPath, headers, body, tasksSummaryTimeout)
	if err != nil || useResponse.Status < 200 || useResponse.Status >= 300 {
		return false
	}
	return taskPayloadError(useResponse.Payload) == nil
}
