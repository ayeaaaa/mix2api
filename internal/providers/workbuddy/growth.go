package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// GrowthTask is one v1 growth-centre task row. The public fields are exactly
// the display/progress fields v1 exposes; no task payload is copied through.
type GrowthTask struct {
	TaskCode     string  `json:"task_code"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	JumpURL      string  `json:"jump_url"`
	Status       string  `json:"status"`
	TaskType     string  `json:"task_type"`
	Current      int     `json:"current"`
	Target       int     `json:"target"`
	RewardCredit float64 `json:"reward_credit"`
	RewardEnergy float64 `json:"reward_energy"`
	Unforgeable  bool    `json:"unforgeable"`
	Reason       string  `json:"reason"`
}

// GrowthSummary is v1's growth overview: energy, streak, makeup cards,
// redemption tiers, lottery chances, and the Buddy travel state.
type GrowthSummary struct {
	Energy         float64           `json:"energy"`
	StreakDays     int               `json:"streak_days"`
	MakeupCards    float64           `json:"makeup_cards"`
	Tiers          []any             `json:"tiers"`
	TierStatuses   map[string]string `json:"tier_statuses"`
	LotteryChances float64           `json:"lottery_chances"`
	Travel         map[string]any    `json:"travel"`
}

// GrowthResult is one account's growth-task run. Tasks carries the final
// observed statuses, Claimed/Accepted/Reported/Claimable give the panel the
// actionable sets, and Credits is the refreshed balance after any claims.
type GrowthResult struct {
	UID          string          `json:"uid"`
	Nickname     string          `json:"nickname,omitempty"`
	Realm        string          `json:"realm,omitempty"`
	OK           bool            `json:"ok"`
	Skipped      bool            `json:"skipped,omitempty"`
	Busy         bool            `json:"busy,omitempty"`
	Msg          string          `json:"msg,omitempty"`
	Error        string          `json:"error,omitempty"`
	Tasks        []GrowthTask    `json:"tasks"`
	Summary      GrowthSummary   `json:"summary"`
	StreakDays   int             `json:"streak_days"`
	EarnedCredit float64         `json:"earned_credit"`
	Accepted     []string        `json:"accepted"`
	Reported     []string        `json:"reported"`
	Claimed      []string        `json:"claimed"`
	Claimable    []string        `json:"claimable"`
	Logs         []string        `json:"logs"`
	Credits      *CreditSnapshot `json:"credits,omitempty"`
}

type growthExpert struct {
	ID   string
	Name string
}

type growthTaskSpec struct {
	Kind        string
	Target      int
	Reward      float64
	Name        string
	Expert      *growthExpert
	Unforgeable bool
	Reason      string
}

var growthTaskSpecs = map[string]growthTaskSpec{
	"create_canvas":                 {Kind: "canvas", Target: 1, Reward: 300, Name: "创建设计任务"},
	"template_5":                    {Kind: "template", Target: 5, Reward: 200, Name: "模板创建任务"},
	"expert_5":                      {Kind: "expert", Target: 5, Reward: 200, Name: "使用专家助手"},
	"Expert_team_use_3":             {Kind: "team", Target: 3, Reward: 150, Name: "使用专家团队"},
	"skill_1":                       {Kind: "skill", Target: 1, Reward: 100, Name: "体验技能"},
	"automation_1":                  {Kind: "automation", Target: 1, Reward: 100, Name: "创建自动化任务"},
	"playbook_prompt":               {Kind: "playbook", Target: 1, Reward: 100, Name: "灵感案例使用"},
	"Expert_lighthouse":             {Kind: "lighthouse", Target: 1, Reward: 100, Name: "轻量云专家使用"},
	"Buddy_App":                     {Kind: "buddy5", Target: 1, Reward: 100, Name: "进入 Buddy 应用"},
	"Buddy_App_QQ":                  {Kind: "buddy5", Target: 1, Reward: 100, Name: "企鹅教师助手"},
	"Hp_Appearance":                 {Kind: "skin", Target: 1, Reward: 100, Name: "应用主题外观"},
	"chat_5":                        {Kind: "chat", Target: 5, Reward: 100, Name: "发起 5 次对话"},
	"Model_chat_GLM5.2":             {Kind: "glmchat", Target: 1, Reward: 100, Name: "体验 GLM-5.2"},
	"black_cat":                     {Kind: "cat", Target: 3, Reward: 100, Name: "夜猫子任务 (23:00-08:00)"},
	"RichMeow_Chat":                 {Kind: "richmeow", Target: 1, Reward: 100, Name: "桌面对话事件链"},
	"Library_read":                  {Kind: "library", Target: 1, Reward: 100, Name: "浏览资料库"},
	"first_buddy":                   {Kind: "chat", Target: 1, Reward: 300, Name: "领取一只 Buddy"},
	"WorkBuddy_Valorant":            {Kind: "expert", Target: 1, Reward: 100, Name: "体验「无畏契约拼豆专家」", Expert: &growthExpert{ID: "ex_xQHHD1u5Jkua", Name: "无畏契约拼豆专家"}},
	"workstation_expert":            {Kind: "expert", Target: 1, Reward: 100, Name: "工作台搭建师", Expert: &growthExpert{ID: "WorkspaceBuilder", Name: "工作台搭建师"}},
	"expert_use_maidehao":           {Kind: "expert", Target: 1, Reward: 100, Name: "中国电商运营专家", Expert: &growthExpert{ID: "ex_GDl31WahyIH0", Name: "中国电商运营专家"}},
	"expert_use_douyinyin":          {Kind: "expert", Target: 1, Reward: 100, Name: "抖音策略师", Expert: &growthExpert{ID: "ex_FbY5nlWTqDod", Name: "抖音策略师"}},
	"expert_use_shangjia_xuanchuan": {Kind: "expert", Target: 1, Reward: 100, Name: "商家宣传页与海报设计师", Expert: &growthExpert{ID: "ex_Weo3x2EZuSDm", Name: "商家宣传页与海报设计师"}},
	"expert_use_bowangwang":         {Kind: "expert", Target: 1, Reward: 100, Name: "直播电商教练", Expert: &growthExpert{ID: "ex_WZY136wV5aMt", Name: "直播电商教练"}},
	"Sequential_Tasks_7":            {Kind: "playbook", Target: 1, Reward: 100, Name: "PC灵感功能"},
	"wb_wechat_oa_subscribe_task":   {Unforgeable: true, Reward: 100, Name: "关注官方公众号", Reason: "需在微信关注「腾讯 WorkBuddy」公众号满 24 小时"},
	"Expert_Philanthropy":           {Unforgeable: true, Reward: 0, Name: "公益爱心捐赠", Reason: "真实捐款动作"},
}

var growthMPSpecs = map[string]growthTaskSpec{
	"Sequential_Tasks_1": {Kind: "mp-chat", Target: 1, Reward: 100, Name: "小程序对话"},
	"Sequential_Tasks_2": {Kind: "mp-expert", Target: 1, Reward: 200, Name: "小程序专家对话"},
	"Sequential_Tasks_3": {Kind: "mp-chat", Target: 5, Reward: 300, Name: "小程序对话5次"},
	"Sequential_Tasks_4": {Kind: "mp-automation", Target: 1, Reward: 100, Name: "小程序定时任务"},
	"Sequential_Tasks_5": {Kind: "mp-glm", Target: 1, Reward: 100, Name: "小程序GLM5.2"},
	"Sequential_Tasks_6": {Kind: "mp-chat", Target: 10, Reward: 400, Name: "小程序对话10次"},
	"school_season":      {Kind: "mp-chat", Target: 1, Reward: 100, Name: "校园日活动"},
}

var growthDesktopOnlyTasks = map[string]string{
	"Buddy_App":    "在桌面端左上角「发现应用」进入任意一个 Buddy 应用",
	"Buddy_App_QQ": "在桌面端「发现应用」进入「企鹅教师助手」",
}

var growthNightTasks = map[string]bool{"black_cat": true}

var growthExpertIDPattern = regexp.MustCompile(`^ex_[A-Za-z0-9]{6,64}$`)

var growthExpertPool = []growthExpert{
	{ID: "ex_PZw8Gu81HfN4", Name: "运维工程师"}, {ID: "ex_ROsDtJbzADFV", Name: "产品经理"},
	{ID: "ex_SMUnl0nJbPix", Name: "UI设计师"}, {ID: "ex_ZTR062oVBOCW", Name: "数据分析师"},
	{ID: "ex_a3sSSFBy8qaC", Name: "后端架构师"}, {ID: "ex_aG1kvKbq8lPx", Name: "文案策划"},
	{ID: "ex_al1vxtUOYQ10", Name: "测试专家"}, {ID: "ex_cZfiyuET9UQP", Name: "安全顾问"},
	{ID: "ex_eggOgQuVP0hq", Name: "算法工程师"}, {ID: "ex_hSwsQjkSKnkX", Name: "前端工程师"},
	{ID: "ex_mMbwwmFA9n9P", Name: "项目管理专家"}, {ID: "ex_uAQE5POfk7Zh", Name: "增长运营专家"},
	{ID: "ex_uZzSAScSy7FZ", Name: "行业研究员"}, {ID: "ex_LHywGrZOtG7G", Name: "数据分析师"},
	{ID: "ex_NX5C8GBciVed", Name: "测试架构师"}, {ID: "ex_DdCsaoq4AtcO", Name: "云端运维专家"},
	{ID: "ex_KzqKQguubrNQ", Name: "内容创作专家"}, {ID: "ex_2cvvUZQhDyeJ", Name: "腾讯轻量云专家"},
}

var growthTeamPool = []growthExpert{
	{ID: "CloudOpsTeam", Name: "运维专家团队"}, {ID: "CloudContentTeam", Name: "内容专家团队"},
	{ID: "CloudDevTeam", Name: "研发专家团队"}, {ID: "ProductStrategyTeam", Name: "产品战略团队"},
	{ID: "MarketingCampaignTeam", Name: "营销活动团队"}, {ID: "SalesBattleTeam", Name: "销售作战团队"},
	{ID: "DesignEngineTeam", Name: "设计引擎团队"}, {ID: "HrOperationsTeam", Name: "人力运营团队"},
}

const growthLibraryDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"

// GrowthTasks runs the v1 CN growth-task engine: auto tasks (first_buddy
// prerequisite) first, batch accept, event report, progress poll, claim, and
// the WeChat mini-program task chain. autoClaim=false still reports progress
// but leaves completed tasks for the operator to claim manually.
func (p *Provider) GrowthTasks(ctx context.Context, uid string, autoClaim bool) (GrowthResult, error) {
	account, err := p.taskAccount(ctx, uid)
	if err != nil {
		return GrowthResult{}, err
	}
	return p.runGrowth(ctx, account, autoClaim, taskTriggerManual)
}

// GrowthTasksAll runs the scheduler's growth selection for this provider realm,
// skipping disabled, task-opted-out, and restrictedUntil accounts.
func (p *Provider) GrowthTasksAll(ctx context.Context, autoClaim bool) ([]GrowthResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: growth tasks require a context")
	}
	results := make([]GrowthResult, 0)
	for _, account := range p.allTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		auth := taskAuthSnapshot(account)
		if !growthTaskEligible(auth) {
			continue
		}
		result, err := p.runGrowth(ctx, account, autoClaim, taskTriggerScheduler)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// GrowthSnapshot returns the last persisted task snapshot without touching the
// upstream. It lets the panel render cached state while a scheduled run is
// still busy.
func (p *Provider) GrowthSnapshot(_ context.Context, uid string) (GrowthResult, error) {
	if p == nil {
		return GrowthResult{}, errors.New("workbuddy: provider is nil")
	}
	pool := p.Pool()
	if pool == nil {
		return GrowthResult{}, errors.New("workbuddy: account pool is nil")
	}
	account := pool.Get(strings.TrimSpace(uid))
	if account == nil {
		return GrowthResult{}, ErrAccountNotFound
	}
	auth := taskAuthSnapshot(account)
	result := GrowthResult{
		UID: auth.UID, Nickname: auth.Nickname, Realm: auth.Realm, OK: true,
		Tasks: []GrowthTask{}, Accepted: []string{}, Reported: []string{},
		Claimed: []string{}, Claimable: []string{}, Logs: []string{},
	}
	raw, exists := authExtraJSON(auth, "growthTasks")
	if exists {
		var snapshot growthSnapshot
		if err := json.Unmarshal(raw, &snapshot); err == nil {
			summary := normalizeGrowthSummary(snapshot.Summary)
			if summary.StreakDays == 0 && snapshot.StreakDays != 0 {
				summary.StreakDays = snapshot.StreakDays
			}
			result.Tasks = snapshot.Tasks
			result.Summary = summary
			result.StreakDays = snapshot.StreakDays
			if result.StreakDays == 0 {
				result.StreakDays = result.Summary.StreakDays
			}
			result.Msg = "cached"
		}
	}
	return result, nil
}

func growthTaskEligible(auth Auth) bool {
	return auth.Realm == realmCN && !taskIsAPIKey(auth) && taskEnterpriseID(auth) == ""
}

func (p *Provider) runGrowth(ctx context.Context, account *Account, autoClaim bool, trigger string) (GrowthResult, error) {
	result, err := p.runGrowthLocked(ctx, account, autoClaim, trigger)
	if err != nil || !result.OK {
		return result, err
	}
	// v1's full growth run ends with the daily cat/Buddy chain. The growth lock
	// has been released by runGrowthLocked, so these public task methods can
	// acquire their own per-account lock without deadlocking.
	p.runGrowthRewardChain(ctx, account, &result)
	return result, nil
}

func (p *Provider) runGrowthRewardChain(ctx context.Context, account *Account, result *GrowthResult) {
	if p == nil || account == nil || result == nil {
		return
	}
	cat, catErr := p.runCatTravel(ctx, account)
	if catErr != nil {
		result.Logs = append(result.Logs, "! 猫猫日常异常: "+p.taskRedactError(account, catErr))
	} else {
		if cat.Error != "" {
			result.Logs = append(result.Logs, "! 猫猫日常: "+cat.Error)
		} else if cat.Msg != "" {
			result.Logs = append(result.Logs, "猫猫日常: "+cat.Msg)
		}
		if cat.OK && cat.Action == "claim" {
			result.EarnedCredit += cat.Credit
		}
	}
	p.taskSleepFor(p.taskGapDelay())
	buddy, buddyErr := p.runBuddyCheckin(ctx, account)
	if buddyErr != nil {
		result.Logs = append(result.Logs, "! Buddy 盲盒异常: "+p.taskRedactError(account, buddyErr))
	} else {
		if buddy.Error != "" {
			result.Logs = append(result.Logs, "! Buddy 盲盒: "+buddy.Error)
		} else if buddy.Action == "open" {
			result.Logs = append(result.Logs, "Buddy 盲盒: "+buddy.Msg)
		}
	}
	if cat.Action == "claim" {
		p.taskSleepFor(p.taskGapDelay())
		if second, secondErr := p.runCatTravel(ctx, account); secondErr != nil {
			result.Logs = append(result.Logs, "! 猫猫日常异常: "+p.taskRedactError(account, secondErr))
		} else {
			if second.Error != "" {
				result.Logs = append(result.Logs, "! 猫猫日常: "+second.Error)
			} else if second.Msg != "" && second.Action == "depart" {
				result.Logs = append(result.Logs, "猫猫日常: "+second.Msg)
			}
		}
	}
	// Cat/Buddy claims add credits after the growth snapshot refresh above.
	_ = p.refreshCreditAccount(ctx, account)
	result.Credits = creditSnapshotOf(account)
	result.Msg = fmt.Sprintf("%s；奖励链完成，本次累计 +%g 积分", result.Msg, result.EarnedCredit)
	result.Logs = p.scrubTaskLogs(account, result.Logs)
	result.Logs = append(result.Logs, fmt.Sprintf("🎉 全部完成！本次累计新增到账: +%g 积分", result.EarnedCredit))
}

func (p *Provider) runGrowthLocked(ctx context.Context, account *Account, autoClaim bool, _ string) (GrowthResult, error) {
	auth := taskAuthSnapshot(account)
	result := GrowthResult{
		UID: strings.TrimSpace(auth.UID), Nickname: strings.TrimSpace(auth.Nickname),
		Realm: auth.Realm, Tasks: []GrowthTask{}, Accepted: []string{},
		Reported: []string{}, Claimed: []string{}, Claimable: []string{},
		Logs: []string{},
	}
	defer func() {
		result.Msg = p.scrubTaskText(account, result.Msg)
		result.Error = p.scrubTaskText(account, result.Error)
		result.Logs = p.scrubTaskLogs(account, result.Logs)
	}()
	if auth.Realm != realmCN {
		result.Error = "国际版不适用国内成长任务中心"
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

	now := p.taskNowTime()
	run := &growthRun{
		provider: p, account: account, auth: auth, now: now,
		autoClaim: autoClaim, claims: loadGrowthClaims(auth),
	}
	run.logs = append(run.logs, fmt.Sprintf("开始为账号 %s 运行成长任务自动化...", firstNonEmpty(auth.Nickname, shortUID(auth.UID))))

	tasks, err := p.fetchGrowthTasks(ctx, account, false)
	if err != nil {
		cached := cachedGrowthSummary(auth)
		result.Error = p.taskRedactError(account, err)
		result.Logs = run.logs
		result.Summary = cached
		result.StreakDays = cached.StreakDays
		return result, nil
	}
	if len(tasks) == 0 {
		cached := cachedGrowthSummary(auth)
		result.Error = "未能获取到任务清单，请检查网络或账号状态"
		result.Logs = run.logs
		result.Tasks = []GrowthTask{}
		result.Summary = cached
		result.StreakDays = cached.StreakDays
		return result, nil
	}
	run.tasks = tasks
	baseline := cachedGrowthSummary(auth)
	run.summary, _ = p.fetchGrowthSummary(ctx, account, baseline)

	// 0. Auto tasks (first_buddy) must be completed and claimed first: the
	// upstream gates single-task acceptance and cat travel on the Buddy
	// instance granted by first_buddy.
	for index := range run.tasks {
		if err := ctx.Err(); err != nil {
			result.Logs = run.logs
			return result, err
		}
		if run.tasks[index].TaskType == "auto" {
			run.earned += p.progressGrowthTask(ctx, run, run.tasks[index])
			run.tasks[index] = run.currentTask(run.tasks[index].TaskCode)
		}
	}

	// 1. Batch accept every unaccepted, forgeable, non-auto task.
	unaccepted := make([]string, 0)
	for _, task := range run.tasks {
		if growthNeedsAccept(task) {
			unaccepted = append(unaccepted, task.TaskCode)
		}
	}
	if len(unaccepted) > 0 {
		run.logs = append(run.logs, fmt.Sprintf("发现 %d 个待接取任务，正在批量接取...", len(unaccepted)))
		accepted, failed, _ := p.acceptGrowthTasks(ctx, account, unaccepted, false)
		run.accepted = append(run.accepted, accepted...)
		if len(accepted) > 0 {
			run.logs = append(run.logs, fmt.Sprintf("✓ 已接取 %d 个任务", len(accepted)))
		}
		if len(failed) > 0 {
			run.logs = append(run.logs, fmt.Sprintf("! 接取未成功 %d 个: %s", len(failed), strings.Join(firstStrings(failed, 5), ", ")))
		}
		p.taskSleepFor(p.taskGapDelay())
		if refreshed, fetchErr := p.fetchGrowthTasks(ctx, account, false); fetchErr == nil {
			run.tasks = mergeGrowthTasks(run.tasks, refreshed)
			still := make([]string, 0)
			for _, task := range run.tasks {
				if growthNeedsAccept(task) {
					still = append(still, task.TaskCode)
				}
			}
			if len(still) > 0 {
				run.logs = append(run.logs, fmt.Sprintf("仍有 %d 个未接取，重试接取一次...", len(still)))
				accepted, _, _ = p.acceptGrowthTasks(ctx, account, still, false)
				run.accepted = append(run.accepted, accepted...)
				p.taskSleepFor(p.taskGapDelay())
				if refreshed, fetchErr = p.fetchGrowthTasks(ctx, account, false); fetchErr == nil {
					run.tasks = mergeGrowthTasks(run.tasks, refreshed)
				}
			}
		}
		still := make([]string, 0)
		for _, task := range run.tasks {
			if growthNeedsAccept(task) {
				still = append(still, task.TaskCode)
			}
		}
		if len(still) > 0 {
			run.logs = append(run.logs, fmt.Sprintf("! 仍有 %d 个任务处于未接取状态，本轮跳过这些任务", len(still)))
		}
	}

	// 2. Process every non-auto task. Report first, then poll for progress.
	for index := range run.tasks {
		if err := ctx.Err(); err != nil {
			result.Logs = run.logs
			return result, err
		}
		if run.tasks[index].TaskType == "auto" {
			continue
		}
		run.earned += p.progressGrowthTask(ctx, run, run.tasks[index])
		run.tasks[index] = run.currentTask(run.tasks[index].TaskCode)
	}

	// 3. The mini-program chain is part of the same v1 run. It mutates
	// run.tasks directly; merging its return value would overwrite refreshed
	// claimed/completed states with the pre-accept snapshot.
	run.earned += p.runMiniProgramGrowth(ctx, run)

	// 4. Refresh credits and persist the final task/claim state.
	_ = p.refreshCreditAccount(ctx, account)
	if err := p.persistGrowthState(account, run); err != nil {
		run.logs = append(run.logs, "! 任务状态持久化失败: "+p.taskRedactError(account, err))
	}
	result.OK = true
	result.Tasks = append([]GrowthTask(nil), run.tasks...)
	result.Summary = run.summary
	result.StreakDays = run.summary.StreakDays
	result.EarnedCredit = run.earned
	result.Accepted = append([]string(nil), run.accepted...)
	result.Reported = append([]string(nil), run.reported...)
	result.Claimed = append([]string(nil), run.claimed...)
	result.Claimable = append([]string(nil), run.claimable...)
	result.Logs = p.scrubTaskLogs(account, run.logs)
	result.Credits = creditSnapshotOf(account)
	result.Msg = fmt.Sprintf("成长任务执行完成: 接取 %d, 上报 %d, 领奖 %d, 新增 %g 积分",
		len(result.Accepted), len(result.Reported), len(result.Claimed), result.EarnedCredit)
	return result, nil
}

type growthRun struct {
	provider  *Provider
	account   *Account
	auth      Auth
	now       time.Time
	autoClaim bool
	logs      []string
	earned    float64
	tasks     []GrowthTask
	summary   GrowthSummary
	accepted  []string
	reported  []string
	claimed   []string
	claimable []string
	claims    map[string]growthClaimEntry
}

func (r *growthRun) log(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *growthRun) currentTask(code string) GrowthTask {
	for _, task := range r.tasks {
		if task.TaskCode == code {
			return task
		}
	}
	return GrowthTask{TaskCode: code}
}

func (r *growthRun) setTaskStatus(code, status string, current int) {
	for index := range r.tasks {
		if r.tasks[index].TaskCode != code {
			continue
		}
		r.tasks[index].Status = status
		if current > r.tasks[index].Current {
			r.tasks[index].Current = current
		}
	}
}

func growthNeedsAccept(task GrowthTask) bool {
	return task.Status == "not_accepted" && !task.Unforgeable && task.TaskType != "auto"
}

func mergeGrowthTasks(current, refreshed []GrowthTask) []GrowthTask {
	if len(refreshed) == 0 {
		return current
	}
	byCode := make(map[string]GrowthTask, len(refreshed))
	for _, task := range refreshed {
		byCode[task.TaskCode] = task
	}
	for index := range current {
		if task, exists := byCode[current[index].TaskCode]; exists {
			current[index] = task
		}
	}
	seen := make(map[string]bool, len(current))
	for _, task := range current {
		seen[task.TaskCode] = true
	}
	for _, task := range refreshed {
		if !seen[task.TaskCode] {
			current = append(current, task)
		}
	}
	return current
}

func firstStrings(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[:limit]
}

// progressGrowthTask is v1's _progress_one. It returns this task's credited
// amount and never lets one task abort the rest of the run.
func (p *Provider) progressGrowthTask(ctx context.Context, run *growthRun, task GrowthTask) float64 {
	code := task.TaskCode
	spec, ok := growthTaskSpecs[code]
	if !ok {
		if dynamic := growthExpertFromTask(task); dynamic != nil {
			spec = growthTaskSpec{Kind: "expert", Target: task.Target, Reward: 100, Expert: dynamic, Name: task.Name}
			ok = true
		}
	}
	if !ok || spec.Unforgeable {
		if ok && spec.Reason != "" {
			run.log("⏭ 任务 [%s] 需真实操作完成: %s, 跳过", firstNonEmpty(spec.Name, code), spec.Reason)
		}
		return 0
	}
	if task.Status == "claimed" {
		return 0
	}
	name := firstNonEmptyText(task.Name, spec.Name, code)
	if task.Status == "completed" || task.Current >= maxInt(task.Target, spec.Target) {
		return p.claimGrowthTask(ctx, run, code, name, spec)
	}
	if description, desktopOnly := growthDesktopOnlyTasks[code]; desktopOnly {
		jump := firstNonEmpty(task.JumpURL, "workbuddy://chat")
		run.log("⏭ 任务 [%s] 需真实操作完成: %s (深链 %s), 跳过事件伪造", name, description, jump)
		return 0
	}
	if growthNightTasks[code] && !inNightWindow(p.taskNowTime()) {
		run.log("🌙 任务 [%s] 仅 23:00-08:00 上报计数, 当前不在窗口, 跳过", name)
		return 0
	}
	if task.Status == "not_accepted" && task.TaskType != "auto" {
		run.log("⏭ 任务 [%s] 仍未接取, 跳过 (先解决接取失败)", name)
		return 0
	}

	target := maxInt(task.Target, spec.Target)
	if target <= 0 {
		target = 1
	}
	need := target - task.Current
	if need < 1 {
		need = 1
	}
	run.log("正在点亮任务 [%s] (需上报 %d 次)...", name, need)
	reportOK := true
	for index := 0; index < need; index++ {
		var expert *growthExpert
		if spec.Expert != nil {
			expert = spec.Expert
		} else if spec.Kind == "team" {
			item := growthTeamPool[(task.Current+index)%len(growthTeamPool)]
			expert = &item
		} else if spec.Kind == "expert" || spec.Kind == "lighthouse" {
			if spec.Kind == "lighthouse" {
				expert = &growthExpert{ID: "ex_2cvvUZQhDyeJ", Name: "腾讯轻量云专家"}
			} else {
				item := growthExpertPool[(task.Current+index)%len(growthExpertPool)]
				expert = &item
			}
		}
		events := buildTaskEventList(run.auth, spec.Kind, index, expert, p.taskNowTime())
		reported, err := p.reportGrowthEvents(ctx, run.account, events, false)
		if err != nil || !reported {
			reportOK = false
		}
		if index < need-1 {
			p.taskSleepFor(p.taskGapDelay())
		}
	}
	if reportOK {
		run.reported = appendUnique(run.reported, code)
	} else {
		run.log("! 任务 [%s] 部分事件上报失败 (上游拒绝), 继续尝试领奖", name)
	}
	p.taskSleepFor(1500 * time.Millisecond)

	progress := task.Current
	for attempt := 0; attempt < 6; attempt++ {
		increased := false
		fresh, err := p.fetchGrowthTasks(ctx, run.account, false)
		if err == nil {
			for _, candidate := range fresh {
				if candidate.TaskCode != code {
					continue
				}
				run.tasks = mergeGrowthTasks(run.tasks, []GrowthTask{candidate})
				progress = candidate.Current
				if progress >= target || candidate.Status == "completed" || candidate.Status == "claimed" {
					goto settled
				}
				increased = progress > task.Current
				break
			}
		}
		if increased {
			// Progress is moving; give the upstream a little longer before
			// deferring the claim to the next run.
			if attempt < 5 {
				p.taskSleepFor(2500 * time.Millisecond)
			}
			continue
		}
		if attempt < 2 {
			p.taskSleepFor(1500 * time.Millisecond)
			continue
		}
		break
	}
settled:
	if progress < target {
		run.log("? 任务 [%s] 已上报但进度 %d/%d 未达成, 领奖顺延到下次运行", name, progress, target)
		p.taskSleepFor(p.taskGapDelay())
		return 0
	}
	if !run.autoClaim {
		if reportOK {
			run.log("⏸ 任务 [%s] 已点亮，等待手动领取", name)
			run.claimable = appendUnique(run.claimable, code)
		} else {
			run.log("! 任务 [%s] 上报未成功，未标记可领取", name)
		}
		p.taskSleepFor(p.taskGapDelay())
		return 0
	}
	return p.claimGrowthTask(ctx, run, code, name, spec)
}

func (p *Provider) claimGrowthTask(ctx context.Context, run *growthRun, code, name string, spec growthTaskSpec) float64 {
	if growthClaimedToday(run.claims, code, run.now) {
		run.log("⏸ 任务 [%s] 今日已领奖，跳过重复领取", name)
		return 0
	}
	if !run.autoClaim {
		task := run.currentTask(code)
		if task.Status == "completed" || task.Current >= maxInt(task.Target, spec.Target) {
			run.log("⏸ 任务 [%s] 已达成，等待手动领取", name)
			run.claimable = appendUnique(run.claimable, code)
		}
		return 0
	}
	credit, energy, ok, message, err := p.claimGrowthTaskUpstream(ctx, run.account, code, false)
	if err != nil {
		run.log("! 任务 [%s] 领奖失败: %s", name, p.taskRedactError(run.account, err))
		p.taskSleepFor(p.taskGapDelay())
		return 0
	}
	if !ok {
		run.log("! 任务 [%s] 领奖失败: %s", name, firstNonEmpty(message, "未知原因"))
		p.taskSleepFor(p.taskGapDelay())
		return 0
	}
	run.claims[code] = growthClaimEntry{
		Date: run.now.Format(tasksDayLayout), TS: run.now.Format(tasksTimeLayout),
		Credit: credit, Energy: energy,
	}
	run.claimed = appendUnique(run.claimed, code)
	run.setTaskStatus(code, "claimed", maxInt(run.currentTask(code).Current, spec.Target))
	run.log("✓ 任务 [%s] 领奖成功: +%g 积分", name, credit)
	p.taskSleepFor(p.taskGapDelay())
	return credit
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func inNightWindow(now time.Time) bool {
	hour := now.Hour()
	return hour >= 23 || hour < 8
}

func growthExpertFromTask(task GrowthTask) *growthExpert {
	jump := strings.TrimSpace(task.JumpURL)
	if jump != "" {
		if parsed, err := url.Parse(jump); err == nil {
			query := parsed.Query()
			if values := query["deeplink"]; len(values) > 0 {
				if inner, err := url.Parse(values[0]); err == nil {
					if id := strings.TrimSpace(inner.Query().Get("expertId")); growthExpertIDPattern.MatchString(id) {
						return &growthExpert{ID: id, Name: firstNonEmpty(task.Name, "专家")}
					}
				}
			}
			if id := strings.TrimSpace(query.Get("expertId")); growthExpertIDPattern.MatchString(id) {
				return &growthExpert{ID: id, Name: firstNonEmpty(task.Name, "专家")}
			}
		}
	}
	code := task.TaskCode
	if strings.HasPrefix(code, "expert_use_") {
		slug := strings.TrimPrefix(code, "expert_use_")
		if slug != "" {
			return &growthExpert{ID: "ex_" + slug, Name: firstNonEmpty(task.Name, slug)}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Upstream fetches
// ---------------------------------------------------------------------------

func (p *Provider) fetchGrowthTasks(ctx context.Context, account *Account, mp bool) ([]GrowthTask, error) {
	headers := p.taskChatHeaders(account, mp)
	response, err := p.taskDo(ctx, account, http.MethodGet,
		p.taskChatBase()+tasksGrowthTasksPath, headers, nil, tasksDefaultTimeout)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, fmt.Errorf("workbuddy: growth tasks HTTP %d", response.Status)
	}
	if err := taskPayloadError(response.Payload); err != nil {
		return nil, err
	}
	rawTasks, _ := anyList(creditObject(response.Payload["data"])["tasks"])
	tasks := make([]GrowthTask, 0, len(rawTasks))
	for _, rawItem := range rawTasks {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		code := strings.TrimSpace(creditText(item["task_code"]))
		if code == "" {
			code = strings.TrimSpace(creditText(item["taskCode"]))
		}
		if code == "" {
			continue
		}
		spec, _ := growthMPSpecForCode(code, mp)
		progress := creditObject(item["progress"])
		current := int(creditNumber(progress["current"]))
		target := int(creditNumber(progress["target"]))
		if target <= 0 {
			target = spec.Target
		}
		if target <= 0 {
			target = 1
		}
		credits := creditNumber(item["reward_credit"])
		if credits == 0 {
			credits = spec.Reward
		}
		tasks = append(tasks, GrowthTask{
			TaskCode:     code,
			Name:         p.scrubTaskText(account, firstNonEmptyText(creditText(item["title"]), spec.Name, code)),
			Description:  p.scrubTaskText(account, firstNonEmpty(creditText(item["description"]), creditText(item["task_desc"]))),
			JumpURL:      p.scrubTaskText(account, creditText(item["jump_url"])),
			Status:       firstNonEmpty(creditText(item["accept_status"]), "not_accepted"),
			TaskType:     creditText(item["task_type"]),
			Current:      current,
			Target:       target,
			RewardCredit: credits,
			RewardEnergy: creditNumber(item["reward_energy"]),
			Unforgeable:  spec.Unforgeable,
			Reason:       spec.Reason,
		})
	}
	return tasks, nil
}

func growthMPSpecForCode(code string, mp bool) (growthTaskSpec, bool) {
	if mp {
		spec, ok := growthMPSpecs[code]
		return spec, ok
	}
	spec, ok := growthTaskSpecs[code]
	return spec, ok
}

func cloneGrowthSummary(summary GrowthSummary) GrowthSummary {
	out := summary
	if summary.Tiers != nil {
		out.Tiers = append([]any(nil), summary.Tiers...)
	}
	if summary.TierStatuses != nil {
		out.TierStatuses = make(map[string]string, len(summary.TierStatuses))
		for key, value := range summary.TierStatuses {
			out.TierStatuses[key] = value
		}
	}
	if summary.Travel != nil {
		out.Travel = cloneAnyMap(summary.Travel)
	}
	return out
}

func normalizeGrowthSummary(summary GrowthSummary) GrowthSummary {
	out := cloneGrowthSummary(summary)
	if out.Tiers == nil {
		out.Tiers = []any{}
	}
	if out.TierStatuses == nil {
		out.TierStatuses = map[string]string{"7d": "locked", "14d": "locked", "28d": "locked"}
	}
	if out.Travel == nil {
		out.Travel = map[string]any{"state": "unknown"}
	}
	return out
}

func cachedGrowthSummary(auth Auth) GrowthSummary {
	if raw, exists := authExtraJSON(auth, "growthTasks"); exists {
		var snapshot growthSnapshot
		if err := json.Unmarshal(raw, &snapshot); err == nil {
			summary := snapshot.Summary
			if summary.StreakDays == 0 && snapshot.StreakDays != 0 {
				summary.StreakDays = snapshot.StreakDays
			}
			return normalizeGrowthSummary(summary)
		}
	}
	return normalizeGrowthSummary(GrowthSummary{})
}

func (p *Provider) fetchGrowthSummary(ctx context.Context, account *Account, baseline GrowthSummary) (GrowthSummary, error) {
	summary := normalizeGrowthSummary(baseline)
	headers := p.taskChatHeaders(account, false)
	base := p.taskChatBase()

	if response, err := p.taskDo(ctx, account, http.MethodGet, base+tasksGrowthEnergyPath, headers, nil, tasksSummaryTimeout); err == nil && response.Status >= 200 && response.Status < 300 {
		data := creditObject(response.Payload["data"])
		if value, exists := data["balance"]; exists {
			summary.Energy = creditNumber(value)
		}
	}
	if response, err := p.taskDo(ctx, account, http.MethodGet, base+tasksGrowthStreakPath, headers, nil, tasksSummaryTimeout); err == nil && response.Status >= 200 && response.Status < 300 {
		if err := taskPayloadError(response.Payload); err == nil {
			data := creditObject(response.Payload["data"])
			if streak := creditObject(data["streak"]); streak != nil {
				if value, exists := streak["days"]; exists {
					summary.StreakDays = int(creditNumber(value))
				}
			}
			if cards := creditObject(data["makeup_cards"]); cards != nil {
				if value, exists := cards["balance"]; exists {
					summary.MakeupCards = creditNumber(value)
				}
			}
			redemption := creditObject(data["redemption_status"])
			if tiers, ok := anyList(redemption["tiers"]); ok {
				summary.Tiers = p.taskRedactValue(account, tiers).([]any)
			}
			for key, field := range map[string]string{"7d": "tier_7d_status", "14d": "tier_14d_status", "28d": "tier_28d_status"} {
				if status := strings.TrimSpace(creditText(redemption[field])); status != "" {
					summary.TierStatuses[key] = status
				}
			}
		}
	}
	if response, err := p.taskDo(ctx, account, http.MethodGet, base+tasksGrowthLotteryPath, headers, nil, tasksSummaryTimeout); err == nil && response.Status >= 200 && response.Status < 300 {
		data := creditObject(response.Payload["data"])
		if value, exists := data["chances"]; exists {
			summary.LotteryChances = creditNumber(value)
		}
	}
	if response, err := p.taskDo(ctx, account, http.MethodGet, base+tasksTravelStatusPath, headers, nil, tasksSummaryTimeout); err == nil && response.Status >= 200 && response.Status < 300 {
		if travel := creditObject(response.Payload["data"]); len(travel) > 0 {
			summary.Travel = p.taskRedactValue(account, travel).(map[string]any)
		}
	}
	return summary, nil
}

func (p *Provider) acceptGrowthTasks(ctx context.Context, account *Account, codes []string, mp bool) (accepted, failed []string, message string) {
	accepted, failed = []string{}, []string{}
	if len(codes) == 0 {
		return accepted, failed, ""
	}
	headers := p.taskChatHeaders(account, mp)
	for start := 0; start < len(codes); start += 20 {
		end := start + 20
		if end > len(codes) {
			end = len(codes)
		}
		part := codes[start:end]
		body, _ := json.Marshal(map[string]any{"task_codes": part})
		response, err := p.taskDo(ctx, account, http.MethodPost,
			p.taskChatBase()+tasksGrowthAcceptPath, headers, body, tasksDefaultTimeout)
		if err != nil {
			failed = append(failed, part...)
			message = p.taskRedactError(account, err)
			continue
		}
		if response.Status < 200 || response.Status >= 300 {
			failed = append(failed, part...)
			message = fmt.Sprintf("HTTP %d: %s", response.Status, taskPayloadMessage(response.Payload))
			continue
		}
		if err := taskPayloadError(response.Payload); err != nil {
			failed = append(failed, part...)
			message = p.taskRedactError(account, err)
			continue
		}
		results, _ := anyList(creditObject(response.Payload["data"])["results"])
		seen := make(map[string]bool, len(results))
		for _, rawItem := range results {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			code := strings.TrimSpace(creditText(item["task_code"]))
			if code == "" {
				code = strings.TrimSpace(creditText(item["taskCode"]))
			}
			seen[code] = true
			status := strings.TrimSpace(creditText(item["status"]))
			if status == "accepted" || status == "already_accepted" {
				accepted = append(accepted, code)
				continue
			}
			failed = append(failed, code)
			if reason := taskPayloadMessage(item); reason != "" {
				message = reason
			}
		}
		for _, code := range part {
			if !seen[code] {
				failed = append(failed, code)
			}
		}
	}
	return accepted, failed, message
}

func (p *Provider) claimGrowthTaskUpstream(ctx context.Context, account *Account, code string, mp bool) (credit, energy float64, ok bool, message string, err error) {
	path := fmt.Sprintf(tasksGrowthClaimPath, url.PathEscape(code))
	headers := p.taskChatHeaders(account, mp)
	response, requestErr := p.taskDo(ctx, account, http.MethodPost,
		p.taskChatBase()+path, headers, []byte("{}"), tasksDefaultTimeout)
	if requestErr != nil {
		return 0, 0, false, "", requestErr
	}
	if response.Status == http.StatusBadRequest {
		// v1 falls back to the web origin when the chat-side claim is rejected
		// with a business 400 (typically eventual-consistency "not completed").
		webHeaders := p.taskWebClaimHeaders(account)
		webResponse, webErr := p.taskDo(ctx, account, http.MethodPost,
			p.taskWebBase()+path, webHeaders, []byte("{}"), tasksDefaultTimeout)
		if webErr == nil {
			response = webResponse
		}
	}
	if response.Status < 200 || response.Status >= 300 {
		return 0, 0, false, fmt.Sprintf("HTTP %d: %s", response.Status, taskPayloadMessage(response.Payload)), nil
	}
	if payloadErr := taskPayloadError(response.Payload); payloadErr != nil {
		return 0, 0, false, p.taskRedactError(account, payloadErr), nil
	}
	if !taskCodeIsZero(response.Payload) {
		return 0, 0, false, "upstream did not return code=0", nil
	}
	data := creditObject(response.Payload["data"])
	return creditNumber(data["credit"]), creditNumber(data["energy"]), true, "", nil
}

func (p *Provider) reportGrowthEvents(ctx context.Context, account *Account, events []map[string]any, mp bool) (bool, error) {
	if len(events) == 0 {
		return true, nil
	}
	body, err := json.Marshal(events)
	if err != nil {
		return false, err
	}
	headers := p.taskChatHeaders(account, mp)
	if mp {
		headers.Set("X-Client-Product", "workbuddy-mp")
		headers.Set("X-Client-Version", "2.4.0")
		headers.Set("X-Client-Platform", "mp-weixin")
		headers.Set("X-Platform", "wechatmp")
	}
	response, err := p.taskDo(ctx, account, http.MethodPost,
		p.taskChatBase()+tasksReportPath, headers, body, tasksDefaultTimeout)
	if err != nil {
		return false, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return false, nil
	}
	if err := taskPayloadError(response.Payload); err != nil {
		return false, nil
	}
	return taskCodeIsZero(response.Payload), nil
}

// ---------------------------------------------------------------------------
// Mini-program chain
// ---------------------------------------------------------------------------

func (p *Provider) runMiniProgramGrowth(ctx context.Context, run *growthRun) float64 {
	if run == nil || run.auth.Realm != realmCN {
		return 0
	}
	mpTasks, err := p.fetchGrowthTasks(ctx, run.account, true)
	if err != nil || len(mpTasks) == 0 {
		return 0
	}
	supported := filterMPGrowthTasks(mpTasks)
	if len(supported) == 0 {
		return 0
	}
	run.tasks = mergeGrowthTasks(run.tasks, supported)
	run.log("📱 发现 %d 个小程序成长任务，开始执行自动化...", len(supported))
	unaccepted := make([]string, 0)
	for _, task := range supported {
		if task.Status == "not_accepted" {
			unaccepted = append(unaccepted, task.TaskCode)
		}
	}
	if len(unaccepted) > 0 {
		accepted, _, _ := p.acceptGrowthTasks(ctx, run.account, unaccepted, true)
		if len(accepted) > 0 {
			run.log("✓ 已接取小程序任务: %s", strings.Join(accepted, ", "))
		}
		run.accepted = append(run.accepted, accepted...)
		p.taskSleepFor(p.taskGapDelay())
		if refreshed, fetchErr := p.fetchGrowthTasks(ctx, run.account, true); fetchErr == nil {
			refreshed = filterMPGrowthTasks(refreshed)
			supported = mergeGrowthTasks(supported, refreshed)
			// Persist the refreshed statuses into run.tasks before the
			// processing loop so claimed/completed skip branches do not fall
			// back to the pre-accept snapshot.
			run.tasks = mergeGrowthTasks(run.tasks, refreshed)
		}
	}
	total := 0.0
	now := p.taskNowTime()
	for _, task := range supported {
		if task.Status == "claimed" {
			continue
		}
		if task.Status == "completed" || task.Current >= task.Target {
			if !run.autoClaim {
				run.claimable = appendUnique(run.claimable, task.TaskCode)
				continue
			}
			if growthClaimedToday(run.claims, task.TaskCode, run.now) {
				continue
			}
			credit, energy, ok, message, claimErr := p.claimGrowthTaskUpstream(ctx, run.account, task.TaskCode, true)
			if claimErr != nil || !ok {
				run.log("! 小程序任务 [%s] 领奖失败: %s", task.Name, firstNonEmpty(message, "未知原因"))
				continue
			}
			run.claims[task.TaskCode] = growthClaimEntry{Date: run.now.Format(tasksDayLayout), TS: run.now.Format(tasksTimeLayout), Credit: credit, Energy: energy}
			run.claimed = appendUnique(run.claimed, task.TaskCode)
			run.setTaskStatus(task.TaskCode, "claimed", maxInt(task.Target, 1))
			run.log("✓ 小程序任务 [%s] 领奖成功: +%g 积分", task.Name, credit)
			total += credit
			continue
		}
		if task.Status == "not_accepted" {
			continue
		}
		need := task.Target - task.Current
		if need < 1 {
			need = 1
		}
		events := buildMPEvents(run.auth, task.TaskCode, need, now)
		if len(events) > 0 {
			run.log("正在点亮小程序任务 [%s] (需上报 %d 次)...", task.Name, need)
			reported, reportErr := p.reportGrowthEvents(ctx, run.account, events, true)
			if reportErr != nil {
				run.log("! 小程序任务 [%s] 上报失败: %s", task.Name, p.taskRedactError(run.account, reportErr))
			} else if !reported {
				run.log("! 小程序任务 [%s] 上报被上游拒绝", task.Name)
			}
			if reported {
				run.reported = appendUnique(run.reported, task.TaskCode)
			}
			reached := false
			if reported {
				if freshTasks, fetchErr := p.fetchGrowthTasks(ctx, run.account, true); fetchErr == nil {
					for _, fresh := range freshTasks {
						if fresh.TaskCode != task.TaskCode {
							continue
						}
						run.tasks = mergeGrowthTasks(run.tasks, []GrowthTask{fresh})
						reached = fresh.Status == "completed" || fresh.Current >= fresh.Target
						task = fresh
						break
					}
				}
			}
			p.taskSleepFor(1500 * time.Millisecond)
			if run.autoClaim {
				if growthClaimedToday(run.claims, task.TaskCode, run.now) {
					continue
				}
				credit, energy, ok, message, claimErr := p.claimGrowthTaskUpstream(ctx, run.account, task.TaskCode, true)
				if claimErr != nil || !ok {
					run.log("? 小程序任务 [%s] 已上报，领奖将在下轮重试: %s", task.Name, firstNonEmpty(message, "未知原因"))
				} else {
					run.claims[task.TaskCode] = growthClaimEntry{Date: run.now.Format(tasksDayLayout), TS: run.now.Format(tasksTimeLayout), Credit: credit, Energy: energy}
					run.claimed = appendUnique(run.claimed, task.TaskCode)
					run.setTaskStatus(task.TaskCode, "claimed", maxInt(task.Target, 1))
					run.log("✓ 小程序任务 [%s] 点亮并领奖成功: +%g 积分", task.Name, credit)
					total += credit
				}
			} else if reported && reached {
				run.setTaskStatus(task.TaskCode, "completed", maxInt(task.Target, task.Current))
				run.claimable = appendUnique(run.claimable, task.TaskCode)
			} else if reported {
				run.log("? 小程序任务 [%s] 已上报但进度 %d/%d 未达成，未标记可领取",
					task.Name, task.Current, task.Target)
			} else {
				run.log("! 小程序任务 [%s] 上报未成功，未标记完成", task.Name)
			}
			p.taskSleepFor(p.taskGapDelay())
		}
	}
	return total
}

func filterMPGrowthTasks(tasks []GrowthTask) []GrowthTask {
	out := make([]GrowthTask, 0, len(tasks))
	for _, task := range tasks {
		if _, ok := growthMPSpecs[task.TaskCode]; ok {
			out = append(out, task)
		}
	}
	return out
}

func buildMPEvents(auth Auth, code string, need int, now time.Time) []map[string]any {
	stamp := now.UnixMilli()
	uid := strings.TrimSpace(auth.UID)
	nickname := strings.TrimSpace(auth.Nickname)
	events := make([]map[string]any, 0, need)
	for index := 0; index < need; index++ {
		conversation := fmt.Sprintf("wb-mp-%d-%s-%d", stamp, code, index)
		requestID := fmt.Sprintf("req-%d-%s-%d", stamp, code, index)
		// v1 merges mp_base first and the event body second, so explicit event
		// fields (source/platform/userId) always win over the base envelope.
		base := map[string]any{
			"timestamp": stamp + int64(index), "ideType": "WorkBuddy_MP",
			"ideVersion": "2.4.0", "extName": "workbuddy-mp", "extVersion": "2.4.0",
			"product": "SaaS", "ideName": "wx_app_cloud", "platform": "mini_program",
			"source": "mini_program", "os": "android", "osVersion": "14", "arch": "arm64",
			"machineId": "mp-" + truncateTaskText(uid, 16), "timezone": "Asia/Shanghai",
			"userId": uid, "userNickname": nickname,
		}
		var event map[string]any
		switch {
		case code == "Sequential_Tasks_2":
			event = map[string]any{
				"eventCode": "expert_actual_use", "reportDelay": 0,
				"id": "WorkspaceBuilder", "name": "WorkspaceBuilder",
				"expertTitle": "工作台搭建师", "type": "send_message", "characterCount": 12,
				"expertType": "agent", "extVersion": "2.2.8", "userId": uid,
			}
		case code == "Sequential_Tasks_4":
			event = map[string]any{
				"eventCode": "automated_task_create_suc", "reportDelay": 0,
				"mode": "CLOUD", "name": "每日任务提醒", "source": "manually",
				"skills": "", "skillCount": 0, "scheduleType": "daily", "userId": uid,
			}
		default:
			event = map[string]any{
				"eventCode": "chat_request_send", "reportDelay": 0, "mode": "chat",
				"inputLength": 12, "isPlan": false, "isAutoExecuteTerminal": false,
				"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
				"maxSteps": 500, "temperature": 0, "maxRetries": 0,
				"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
				"codebaseId": "", "mentionContextCount": 0, "command": "",
				"requestId": requestID, "traceId": requestID, "rootRequestId": requestID,
				"parentConversationId": conversation, "conversationId": conversation,
				"messageId": fmt.Sprintf("msg-%d-%d", stamp, index),
				"agentName": "mp", "agentType": "main",
				"codebuddy.session_id":              conversation,
				"codebuddy.conversation_request_id": requestID, "userId": uid,
			}
			if code == "Sequential_Tasks_5" {
				event["requestModelId"] = "glm-5.2"
				event["requestModelName"] = "GLM-5.2"
			}
		}
		merged := map[string]any{}
		for key, value := range base {
			merged[key] = value
		}
		for key, value := range event {
			merged[key] = value
		}
		events = append(events, merged)
	}
	return events
}

// ---------------------------------------------------------------------------
// Event builders
// ---------------------------------------------------------------------------

func buildTaskEventList(auth Auth, kind string, idx int, expert *growthExpert, now time.Time) []map[string]any {
	if kind == "playbook" {
		return buildPlaybookEvents(auth, idx, now)
	}
	if kind == "richmeow" {
		return buildDesktopChatEvents(auth, idx, now)
	}
	return []map[string]any{buildTaskEvent(auth, kind, idx, expert, now)}
}

func buildTaskEvent(auth Auth, kind string, idx int, expert *growthExpert, now time.Time) map[string]any {
	stamp := now.UnixMilli()
	uid := strings.TrimSpace(auth.UID)
	conversation := fmt.Sprintf("wb-task-%d-%d", stamp, idx)
	requestID := conversation + "-req"
	if kind == "canvas" {
		return map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "timestamp": stamp,
			"reportDelay": 0, "conversationId": conversation, "requestId": requestID,
			"source": "summon_keyword", "isCustomModel": false, "name": "",
			"inputLength": 12, "id": fmt.Sprintf("wbx-canvas-%d", stamp), "cost": 0,
			"isSuccessful": true, "userId": uid,
		}
	}
	if kind == "template" {
		return map[string]any{
			"eventCode": "agent_task_created_with_template", "timestamp": stamp,
			"reportDelay": 0, "isCustomModel": true, "id": fmt.Sprintf("%d", idx),
			"name": "幻灯片", "requestId": requestID, "conversationId": conversation,
			"userId": uid,
		}
	}
	if kind == "expert" || kind == "team" || kind == "lighthouse" {
		expertType := "agent"
		chosen := growthExpert{ID: "ContentCreator", Name: "内容创作专家"}
		if kind == "team" {
			expertType = "team"
			chosen = growthTeamPool[0]
		} else if kind == "lighthouse" {
			chosen = growthExpert{ID: "ex_2cvvUZQhDyeJ", Name: "腾讯轻量云专家"}
		}
		if expert != nil {
			chosen = *expert
		}
		return map[string]any{
			"eventCode": "expert_actual_use", "timestamp": stamp, "reportDelay": 0,
			"mode": "CLOUD", "id": chosen.ID, "name": chosen.Name, "expertTitle": chosen.Name,
			"type": "02-Engineering", "expertType": expertType, "source": "builtin",
			"version": "1.0.2", "cost": 0, "characterCount": 12,
			"conversationId": conversation, "requestId": requestID, "messageId": requestID,
			"requestModelId": "deepseek-v4-flash", "requestModelName": "DeepSeek V4 Flash",
			"userId": uid,
		}
	}
	if kind == "skill" {
		return map[string]any{
			"eventCode": "skill_info", "timestamp": stamp, "reportDelay": 0,
			"skillId": "skill_2096525080079265792", "name": "pptx", "userId": uid,
		}
	}
	if kind == "automation" {
		return map[string]any{
			"eventCode": "automated_task_create_suc", "timestamp": stamp, "reportDelay": 0,
			"name": "每周工作整理", "type": "cron", "source": "manually",
			"modelId": "deepseek-v4-flash", "modelIsThinking": false,
			"conversationId": conversation, "requestId": requestID,
			"schedule": map[string]any{"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
			"prompt":   "每周五自动整理本周工作", "userId": uid,
		}
	}
	if kind == "library" {
		return map[string]any{
			"eventCode": "web_element_click", "timestamp": stamp, "reportDelay": 0,
			"pageURL": growthLibraryDocURL, "elementId": "library_doc_intro_click",
			"elementName": "WorkBuddy资料库介绍", "os": "Win32", "arch": "",
			"osVersion": "10.0", "userAgent": "Mozilla/5.0", "userId": uid,
		}
	}
	if kind == "skin" {
		return map[string]any{
			"eventCode": "appearance_skin_apply", "timestamp": stamp, "reportDelay": 0,
			"action": "apply", "source": "settings_close", "id": "theme-tkmw7j",
			"vipLevel": "free", "series": "craft", "type": "unknown",
			"name": "和平精英激战金秋", "userId": uid,
		}
	}
	if kind == "chat" || kind == "glmchat" || kind == "cat" || kind == "buddy5" {
		modelID := "deepseek-v4-flash"
		modelName := "DeepSeek V4 Flash"
		if kind == "glmchat" || kind == "cat" {
			modelID, modelName = "glm-5.2", "GLM-5.2"
		}
		mode := "craft"
		if kind == "cat" {
			mode = "night"
		}
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": stamp, "reportDelay": 0,
			"mode": mode, "conversationId": conversation, "requestId": requestID,
			"inputLength": 12, "requestModelId": modelID, "requestModelName": modelName,
			"isPlan": false, "agentName": "default", "agentType": "conversation",
			"userId": uid,
		}
	}
	return map[string]any{"eventCode": "heartbeat", "timestamp": stamp, "userId": uid}
}

func buildPlaybookEvents(auth Auth, idx int, now time.Time) []map[string]any {
	stamp := now.UnixMilli()
	uid := strings.TrimSpace(auth.UID)
	conversation := fmt.Sprintf("wb-task-%d-%d", stamp, idx)
	requestID := conversation + "-req"
	return []map[string]any{
		{
			"eventCode": "playbook_cta_click", "timestamp": stamp, "reportDelay": 0,
			"id": "worker-ledger-freedom-dashboard", "name": "打工人小账本",
			"type": "document", "categoryId": "", "categoryName": "", "source": "discover",
			"position": 0, "conversationId": conversation, "requestId": requestID,
			"messageId": requestID, "userId": uid,
		},
		{
			"eventCode": "playbook_prompt_send", "timestamp": stamp + 50, "reportDelay": 0,
			"id": "worker-ledger-freedom-dashboard", "name": "打工人小账本",
			"type": "document", "categoryId": "", "categoryName": "",
			"promptLength": 30, "isOfficial": 1, "source": "discover",
			"conversationId": conversation, "requestId": requestID, "messageId": requestID,
			"skills": "", "skillNames": "", "userId": uid,
		},
	}
}

func buildDesktopChatEvents(auth Auth, idx int, now time.Time) []map[string]any {
	stamp := now.UnixMilli()
	conversation := fmt.Sprintf("wb-richmeow-%d-%d", stamp, idx)
	requestID := conversation + "-req"
	messageID := fmt.Sprintf("msg-%d-%d", stamp, idx)
	uid := strings.TrimSpace(auth.UID)
	nickname := strings.TrimSpace(auth.Nickname)
	events := make([]map[string]any, 0, 6)
	base := map[string]any{"userId": uid}
	if nickname != "" {
		base["userNickname"] = nickname
	}
	created := cloneAnyMap(base)
	for key, value := range map[string]any{
		"eventCode": "agent_task_created", "timestamp": stamp,
		"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"has_repo": false, "repo_type": "none", "workspace_type": "empty",
		"has_connector": false, "connector_types": []any{}, "has_mention": false,
		"mention_types": []any{}, "has_template": false, "action": "", "template_name": "",
		"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
		"has_skill": false, "skill_names": []any{}, "conversationId": conversation,
		"messageId": messageID, "buddyId": "", "buddyName": "",
	} {
		created[key] = value
	}
	events = append(events, created)

	message := cloneAnyMap(base)
	for key, value := range map[string]any{
		"eventCode": "chat_message_send", "timestamp": stamp + 20,
		"messageId": messageID + "-ast", "historyCount": 0, "isContextTruncated": false,
		"currentStepCount": 1, "traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversation, "agentName": "cli", "agentType": "main",
	} {
		message[key] = value
	}
	events = append(events, message)

	request := cloneAnyMap(base)
	for key, value := range map[string]any{
		"eventCode": "chat_request_send", "timestamp": stamp + 40,
		"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0, "maxSteps": 500,
		"temperature": 0, "maxRetries": 0, "mentionContexts": []any{},
		"knowledgeId": []any{}, "knowledgeName": []any{}, "codebaseId": "",
		"mentionContextCount": 0, "command": "", "recommendId": "", "skillId": "",
		"skillCount": 0, "totalCount": 0, "traceId": requestID,
		"rootRequestId": requestID, "parentConversationId": conversation,
		"conversationId": conversation, "messageId": messageID,
		"agentName": "cli", "agentType": "main",
	} {
		request[key] = value
	}
	events = append(events, request)

	responseMessage := cloneAnyMap(base)
	for key, value := range map[string]any{
		"eventCode": "chat_message_response", "timestamp": stamp + 60,
		"messageId": messageID + "-resp", "conversationId": conversation,
		"requestId": requestID, "agentName": "cli",
	} {
		responseMessage[key] = value
	}
	events = append(events, responseMessage)

	responseRequest := cloneAnyMap(base)
	for key, value := range map[string]any{
		"eventCode": "chat_request_response", "timestamp": stamp + 80,
		"conversationId": conversation, "requestId": requestID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"toolCallCount": 0, "inputToken": 6, "outputToken": 12, "duration": 50,
		"agentName": "cli",
	} {
		responseRequest[key] = value
	}
	events = append(events, responseRequest)

	completed := cloneAnyMap(base)
	for key, value := range map[string]any{
		"eventCode": "agent_task_completed", "timestamp": stamp + 100,
		"conversationId": conversation, "messageId": messageID, "status": "success",
	} {
		completed[key] = value
	}
	events = append(events, completed)
	return events
}

// ---------------------------------------------------------------------------
// Growth state persistence
// ---------------------------------------------------------------------------

type growthSnapshot struct {
	FetchedAt  float64       `json:"fetched_at"`
	FetchedISO string        `json:"fetched_iso"`
	Tasks      []GrowthTask  `json:"tasks"`
	Summary    GrowthSummary `json:"summary"`
	StreakDays int           `json:"streak_days"`
}

type growthClaimEntry struct {
	Date   string  `json:"date"`
	TS     string  `json:"ts"`
	Credit float64 `json:"credit"`
	Energy float64 `json:"energy"`
}

func loadGrowthClaims(auth Auth) map[string]growthClaimEntry {
	out := map[string]growthClaimEntry{}
	raw, exists := authExtraJSON(auth, "growthClaims")
	if !exists {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func growthClaimedToday(claims map[string]growthClaimEntry, code string, now time.Time) bool {
	entry, exists := claims[code]
	if !exists {
		return false
	}
	return entry.Date == now.Format(tasksDayLayout)
}

func (p *Provider) persistGrowthState(account *Account, run *growthRun) error {
	if p == nil || account == nil || run == nil {
		return nil
	}
	now := run.now
	snapshot := growthSnapshot{
		FetchedAt:  float64(now.UnixNano()) / float64(time.Second),
		FetchedISO: now.Format(creditsTimeLayout),
		Tasks:      run.tasks,
		Summary:    run.summary,
		StreakDays: run.summary.StreakDays,
	}
	snapshotRaw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	claimsRaw, err := json.Marshal(run.claims)
	if err != nil {
		return err
	}
	uid := accountUID(account)
	if p.Pool() == nil || uid == "" {
		account.mu.Lock()
		if account.Auth.Extra == nil {
			account.Auth.Extra = make(map[string]json.RawMessage)
		}
		account.Auth.Extra["growthTasks"] = snapshotRaw
		account.Auth.Extra["growthClaims"] = claimsRaw
		account.mu.Unlock()
		return nil
	}
	return p.Pool().mutateAccount(uid, func(account *Account) {
		if account.Auth.Extra == nil {
			account.Auth.Extra = make(map[string]json.RawMessage)
		}
		account.Auth.Extra["growthTasks"] = snapshotRaw
		account.Auth.Extra["growthClaims"] = claimsRaw
	})
}
