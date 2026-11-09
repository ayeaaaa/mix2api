package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

const (
	jobNameCheckin        = "workbuddy-checkin"
	jobNameDailyActivity  = "workbuddy-daily-activity"
	jobNameReportActivity = "workbuddy-report-activity"
	jobNameGrowth         = "workbuddy-growth"
	jobNameCatTravel      = "workbuddy-cat-travel"
	jobNameStreakBonus    = "workbuddy-streak-bonus"
	jobNameKeepalive      = "workbuddy-keepalive"
	jobNameCatalog        = "workbuddy-catalog-refresh"
)

type jobKind uint8

const (
	jobKindCheckin jobKind = iota
	jobKindDaily
	jobKindReport
	jobKindGrowth
	jobKindCat
	jobKindStreak
	jobKindKeepalive
	jobKindCatalog
)

type jobRealmMode uint8

const (
	jobRealmIntl jobRealmMode = iota
	jobRealmCN
	jobRealmAll
)

// jobSlot is one local-time HH:MM trigger.
type jobSlot struct {
	Hour   int
	Minute int
}

func (s jobSlot) String() string {
	return fmt.Sprintf("%02d:%02d", s.Hour, s.Minute)
}

func (s jobSlot) key(now time.Time) string {
	return now.Format("2006-01-02") + " " + s.String()
}

// JobRunResult is the per-job report returned by RunJobs and the scheduler
// panel trigger. Errors are redacted and per-account; one failed account never
// aborts the rest of the job.
type JobRunResult struct {
	Name      string   `json:"name"`
	Ran       bool     `json:"ran"`
	Realms    []string `json:"realms,omitempty"`
	Checked   int      `json:"checked"`
	Succeeded int      `json:"succeeded"`
	Skipped   int      `json:"skipped"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"`
}

type scheduledJobState struct {
	mu          sync.Mutex
	lastRun     time.Time
	lastSlotKey string
}

type scheduledJob struct {
	provider *Provider
	name     string
	kind     jobKind
	realm    jobRealmMode
	state    scheduledJobState
}

var _ scheduler.Job = (*scheduledJob)(nil)

var errSchedulerBusy = errors.New("workbuddy: scheduler cycle already running")

func (j *scheduledJob) Name() string {
	if j == nil {
		return ""
	}
	return j.name
}

// Run executes only when the job is due and the scheduler is enabled. The
// core scheduler may call it on every tick; a missed slot is caught up once per
// local day, and later ticks in the same slot are no-ops.
func (j *scheduledJob) Run(ctx context.Context) error {
	if j == nil || j.provider == nil {
		_, err := j.run(ctx, false)
		return err
	}
	if !j.provider.schedulerRunMu.TryLock() {
		return errSchedulerBusy
	}
	defer j.provider.schedulerRunMu.Unlock()
	_, err := j.run(ctx, false)
	return err
}

// RunForce is the manual panel trigger. It bypasses the enabled switch and the
// slot clock; account-level daily idempotency still prevents duplicate
// check-ins/activity claims.
func (j *scheduledJob) RunForce(ctx context.Context) (JobRunResult, error) {
	return j.run(ctx, true)
}

func (j *scheduledJob) Due(now time.Time) bool {
	if j == nil || j.provider == nil {
		return false
	}
	if !j.provider.schedulerIsEnabled() {
		return false
	}
	slots := j.schedule(now)
	latest, ok := latestJobSlot(slots, now)
	if !ok {
		return false
	}
	j.state.mu.Lock()
	defer j.state.mu.Unlock()
	return j.state.lastSlotKey != latest.key(now)
}

func (j *scheduledJob) NextRun(now time.Time) time.Time {
	if j == nil {
		return time.Time{}
	}
	slots := j.schedule(now)
	for _, slot := range slots {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), slot.Hour, slot.Minute, 0, 0, now.Location())
		if candidate.After(now) {
			return candidate
		}
	}
	if len(slots) == 0 {
		return time.Time{}
	}
	tomorrow := now.Add(24 * time.Hour)
	first := slots[0]
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), first.Hour, first.Minute, 0, 0, now.Location())
}

func (j *scheduledJob) LastRun() time.Time {
	if j == nil {
		return time.Time{}
	}
	j.state.mu.Lock()
	defer j.state.mu.Unlock()
	return j.state.lastRun
}

func (j *scheduledJob) schedule(now time.Time) []jobSlot {
	if j == nil || j.provider == nil {
		return nil
	}
	switch j.kind {
	case jobKindCheckin:
		return jobSlotsFromHours(j.provider.scheduleHours("checkin", defaultHours(9, 21)))
	case jobKindDaily:
		if times := normalizeScheduleTimes(j.provider.scheduleRaw("daily_chat")); len(times) > 0 {
			return jobSlotsFromTimes(times)
		}
		return jobSlotsFromHours(j.provider.scheduleHours("checkin", defaultHours(9, 21)))
	case jobKindReport:
		return jobSlotsFromHours(j.provider.scheduleHours("checkin", defaultHours(9, 21)))
	case jobKindGrowth:
		return jobSlotsFromHours(j.provider.scheduleHours("cat", defaultHours(1)))
	case jobKindCat, jobKindStreak:
		return jobSlotsFromHours(j.provider.scheduleHours("travel", defaultHours(9, 21)))
	case jobKindKeepalive:
		return jobSlotsFromHours(j.provider.scheduleHours("keepalive", defaultHours(22)))
	case jobKindCatalog:
		return []jobSlot{{Hour: 0, Minute: 0}}
	default:
		return nil
	}
}

func latestJobSlot(slots []jobSlot, now time.Time) (jobSlot, bool) {
	current := now.Hour()*60 + now.Minute()
	best := jobSlot{}
	found := false
	for _, slot := range slots {
		if slot.Hour*60+slot.Minute > current {
			continue
		}
		if !found || slot.Hour*60+slot.Minute > best.Hour*60+best.Minute {
			best = slot
			found = true
		}
	}
	return best, found
}

func jobSlotsFromHours(hours []int) []jobSlot {
	out := make([]jobSlot, 0, len(hours))
	for _, hour := range hours {
		if hour < 0 || hour > 23 {
			continue
		}
		out = append(out, jobSlot{Hour: hour})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
	return out
}

func jobSlotsFromTimes(times []string) []jobSlot {
	out := make([]jobSlot, 0, len(times))
	for _, value := range times {
		parts := strings.Split(value, ":")
		if len(parts) != 2 {
			continue
		}
		hour, errHour := strconv.Atoi(parts[0])
		minute, errMinute := strconv.Atoi(parts[1])
		if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			continue
		}
		out = append(out, jobSlot{Hour: hour, Minute: minute})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hour == out[j].Hour {
			return out[i].Minute < out[j].Minute
		}
		return out[i].Hour < out[j].Hour
	})
	return out
}

func defaultHours(values ...int) []int {
	return append([]int(nil), values...)
}

func (j *scheduledJob) run(ctx context.Context, force bool) (result JobRunResult, err error) {
	if j != nil {
		result = JobRunResult{Name: j.name}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Ran = true
			err = errors.New("workbuddy: job panicked")
		}
	}()
	if j == nil || j.provider == nil {
		return result, errors.New("workbuddy: job provider is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	now := j.provider.jobsNowTime()
	if !force && !j.provider.schedulerIsEnabled() {
		return result, nil
	}
	if !force && !j.Due(now) {
		return result, nil
	}
	if j.kind == jobKindDaily && !j.provider.dailyActivityEnabled() {
		return result, nil
	}
	slots := j.schedule(now)
	if latest, ok := latestJobSlot(slots, now); ok {
		j.state.mu.Lock()
		j.state.lastSlotKey = latest.key(now)
		j.state.lastRun = now
		j.state.mu.Unlock()
	} else if force {
		j.state.mu.Lock()
		j.state.lastRun = now
		j.state.mu.Unlock()
	}
	j.provider.markSchedulerRun(now)

	result.Ran = true
	targets := j.realmProviders()
	for _, provider := range targets {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		provider.refreshSettings()
		result.Realms = append(result.Realms, provider.realm)
		if j.kind == jobKindKeepalive || j.kind == jobKindCatalog {
			j.runProviderWide(ctx, provider, &result)
		} else {
			j.runAccounts(ctx, provider, &result)
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	if len(result.Errors) > 0 {
		return result, errors.New(strings.Join(result.Errors, "; "))
	}
	return result, nil
}

func (j *scheduledJob) realmProviders() []*Provider {
	switch j.realm {
	case jobRealmIntl:
		if provider := j.provider.providerForRealm(realmIntl); provider != nil {
			return []*Provider{provider}
		}
	case jobRealmCN:
		if provider := j.provider.providerForRealm(realmCN); provider != nil {
			return []*Provider{provider}
		}
	case jobRealmAll:
		return j.provider.allProviders()
	}
	return nil
}

func (j *scheduledJob) runAccounts(ctx context.Context, provider *Provider, result *JobRunResult) {
	for index, account := range provider.jobTaskAccounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		auth := taskAuthSnapshot(account)
		if auth.Realm != provider.realm {
			continue
		}
		result.Checked++
		if !jobAccountActive(account, provider.jobsNowTime()) {
			result.Skipped++
			continue
		}
		if taskIsAPIKey(auth) {
			result.Skipped++
			continue
		}
		switch j.kind {
		case jobKindCheckin:
			if taskEnterpriseID(auth) != "" {
				result.Skipped++
				continue
			}
			checkin, err := safeJobCall(func() (CheckinResult, error) {
				return provider.runCheckin(ctx, account, taskTriggerScheduler)
			})
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, auth.UID+": "+safeJobError(err))
				continue
			}
			provider.recordActivity(checkin.Activity)
			jobCountCheckin(checkin, result)
		case jobKindDaily:
			if auth.Realm != realmIntl || taskEnterpriseID(auth) != "" {
				result.Skipped++
				continue
			}
			daily, err := safeJobCall(func() (DailyResult, error) {
				return provider.runDailyActivity(ctx, account, taskTriggerScheduler)
			})
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, auth.UID+": "+safeJobError(err))
				continue
			}
			success := daily.OK
			skipped := daily.Skipped
			if !success {
				result.Failed++
				if daily.Error != "" {
					result.Errors = append(result.Errors, auth.UID+": "+daily.Error)
				}
			} else if skipped {
				result.Skipped++
			} else {
				result.Succeeded++
			}
		case jobKindReport:
			if auth.Realm != realmCN || taskEnterpriseID(auth) != "" {
				result.Skipped++
				continue
			}
			report, err := safeJobCall(func() (DailyResult, error) {
				return provider.runReportActivity(ctx, account, taskTriggerScheduler)
			})
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, auth.UID+": "+safeJobError(err))
				continue
			}
			provider.recordActivity(report.Activity)
			if report.Error != "" && !report.OK {
				result.Failed++
				result.Errors = append(result.Errors, auth.UID+": "+report.Error)
			} else if report.Skipped {
				result.Skipped++
			} else if report.OK {
				result.Succeeded++
			} else {
				result.Failed++
			}
		case jobKindGrowth:
			if !growthTaskEligible(auth) {
				result.Skipped++
				continue
			}
			growth, err := safeJobCall(func() (GrowthResult, error) {
				return provider.runGrowth(ctx, account, true, taskTriggerScheduler)
			})
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, auth.UID+": "+safeJobError(err))
				continue
			}
			if growth.OK && !growth.Skipped {
				result.Succeeded++
			} else if growth.Skipped {
				result.Skipped++
			} else {
				result.Failed++
				if growth.Error != "" {
					result.Errors = append(result.Errors, auth.UID+": "+growth.Error)
				}
			}
		case jobKindCat:
			if !growthTaskEligible(auth) {
				result.Skipped++
				continue
			}
			cat, err := safeJobCall(func() (CatResult, error) {
				return provider.runCatTravel(ctx, account)
			})
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, auth.UID+": "+safeJobError(err))
				continue
			}
			jobCountTaskResult(cat.OK, cat.Skipped, cat.Error, auth.UID, result)
			provider.invalidateGrowthSnapshot(auth.UID)
		case jobKindStreak:
			if !growthTaskEligible(auth) {
				result.Skipped++
				continue
			}
			streak, err := safeJobCall(func() (StreakResult, error) {
				return provider.runStreakBonus(ctx, account)
			})
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, auth.UID+": "+safeJobError(err))
				continue
			}
			jobCountTaskResult(streak.OK, streak.Skipped, streak.Error, auth.UID, result)
			provider.invalidateGrowthSnapshot(auth.UID)
		}
	}
}

func (j *scheduledJob) runProviderWide(ctx context.Context, provider *Provider, result *JobRunResult) {
	switch j.kind {
	case jobKindKeepalive:
		keepalive, err := safeJobCall(func() (KeepaliveResult, error) {
			return provider.Keepalive(ctx)
		})
		result.Checked += keepalive.Accounts
		result.Succeeded += keepalive.Refreshed
		result.Skipped += keepalive.Skipped
		result.Failed += keepalive.Failed
		result.Errors = append(result.Errors, keepalive.Errors...)
		if err != nil && len(keepalive.Errors) == 0 {
			result.Failed++
			result.Errors = append(result.Errors, safeJobError(err))
		}
	case jobKindCatalog:
		catalog, done, err := safeJobCall3(func() (CatalogResult, bool, error) {
			return provider.CatalogDailyRefresh(ctx)
		})
		_ = catalog
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, safeJobError(err))
		} else if done {
			result.Succeeded++
		} else {
			result.Skipped++
		}
	}
}

func jobCountCheckin(result CheckinResult, report *JobRunResult) {
	if result.Error != "" && !result.OK {
		report.Failed++
		report.Errors = append(report.Errors, result.UID+": "+result.Error)
		return
	}
	if result.Skipped || result.Busy {
		report.Skipped++
		return
	}
	if result.OK {
		report.Succeeded++
		return
	}
	report.Failed++
}

func jobCountTaskResult(ok, skipped bool, message, uid string, report *JobRunResult) {
	if skipped {
		report.Skipped++
		return
	}
	if ok {
		report.Succeeded++
		return
	}
	report.Failed++
	if message != "" {
		report.Errors = append(report.Errors, uid+": "+message)
	}
}

// jobTaskAccounts returns every non-removed account in this realm's pool. The
// per-job loop applies the enabled/tasks/cooling/restriction filters so skipped
// accounts still appear in the report instead of vanishing from it.
func (p *Provider) jobTaskAccounts() []*Account {
	if p == nil {
		return nil
	}
	pool := p.Pool()
	if pool == nil {
		return nil
	}
	return pool.Accounts()
}

func jobAccountActive(account *Account, now time.Time) bool {
	if account == nil {
		return false
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.removed || !account.Auth.Enabled || account.State.Disabled ||
		account.State.ManualDisabled || !account.Auth.TasksEnabled {
		return false
	}
	if account.State.CoolUntil.After(now) {
		return false
	}
	return taskRestrictedUntil(account.Auth) <= float64(now.Unix())
}

func safeJobCall[T any](call func() (T, error)) (result T, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			var zero T
			result = zero
			err = errors.New("workbuddy: job panicked")
		}
	}()
	return call()
}

func safeJobCall3[A any, B any](call func() (A, B, error)) (first A, second B, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			var zeroA A
			var zeroB B
			first = zeroA
			second = zeroB
			err = errors.New("workbuddy: job panicked")
		}
	}()
	return call()
}

func safeJobError(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.TrimSpace(apiErr.SafeMessage()) != "" {
		return apiErr.SafeMessage()
	}
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timed out"
	}
	return "request failed"
}

// Jobs returns the WorkBuddy scheduler jobs on the intl provider only. Each
// job resolves its realm targets at run time, so the twin may be wired after
// construction.
func (p *Provider) Jobs() []scheduler.Job {
	if p == nil || p.realm != realmIntl {
		return nil
	}
	p.jobsOnce.Do(func() {
		p.jobs = []*scheduledJob{
			{provider: p, name: jobNameCheckin, kind: jobKindCheckin, realm: jobRealmCN},
			{provider: p, name: jobNameDailyActivity, kind: jobKindDaily, realm: jobRealmIntl},
			{provider: p, name: jobNameReportActivity, kind: jobKindReport, realm: jobRealmCN},
			{provider: p, name: jobNameGrowth, kind: jobKindGrowth, realm: jobRealmCN},
			{provider: p, name: jobNameCatTravel, kind: jobKindCat, realm: jobRealmCN},
			{provider: p, name: jobNameStreakBonus, kind: jobKindStreak, realm: jobRealmCN},
			{provider: p, name: jobNameKeepalive, kind: jobKindKeepalive, realm: jobRealmAll},
			{provider: p, name: jobNameCatalog, kind: jobKindCatalog, realm: jobRealmAll},
		}
	})
	out := make([]scheduler.Job, 0, len(p.jobs))
	for _, job := range p.jobs {
		out = append(out, job)
	}
	return out
}

func (p *Provider) workbuddyJobs() []*scheduledJob {
	if p == nil {
		return nil
	}
	p.Jobs()
	return p.jobs
}

// RunJobs runs one named job or every job. force is the manual trigger path.
func (p *Provider) RunJobs(ctx context.Context, name string, force bool) ([]JobRunResult, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if !p.schedulerRunMu.TryLock() {
		return nil, errSchedulerBusy
	}
	defer p.schedulerRunMu.Unlock()
	name = strings.TrimSpace(name)
	results := make([]JobRunResult, 0)
	errs := make([]error, 0)
	for _, job := range p.workbuddyJobs() {
		if name != "" && job.Name() != name {
			continue
		}
		var result JobRunResult
		var err error
		if force {
			result, err = job.run(ctx, true)
		} else {
			result, err = job.run(ctx, false)
		}
		results = append(results, result)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if name != "" && len(results) == 0 {
		return results, fmt.Errorf("workbuddy: unknown job %q", name)
	}
	return results, errors.Join(errs...)
}

func (p *Provider) markSchedulerRun(at time.Time) {
	if p == nil {
		return
	}
	p.schedulerMu.Lock()
	p.schedulerLastRun = at
	p.schedulerMu.Unlock()
}

func (p *Provider) schedulerLastRunTime() time.Time {
	if p == nil {
		return time.Time{}
	}
	p.schedulerMu.Lock()
	defer p.schedulerMu.Unlock()
	return p.schedulerLastRun
}

func (p *Provider) jobsNowTime() time.Time {
	if p != nil && p.jobsNow != nil {
		return p.jobsNow()
	}
	return time.Now()
}

func (p *Provider) schedulerIsEnabled() bool {
	if p == nil {
		return false
	}
	p.schedulerMu.Lock()
	defer p.schedulerMu.Unlock()
	if p.schedulerEnabled == nil {
		value := true
		p.schedulerEnabled = &value
	}
	return *p.schedulerEnabled
}

// ToggleScheduler flips the provider's scheduler switch and returns its new
// state.
func (p *Provider) ToggleScheduler() bool {
	if p == nil {
		return false
	}
	p.schedulerMu.Lock()
	if p.schedulerEnabled == nil {
		value := true
		p.schedulerEnabled = &value
	}
	*p.schedulerEnabled = !*p.schedulerEnabled
	value := *p.schedulerEnabled
	p.schedulerMu.Unlock()
	return value
}

// SchedulerStatus is the read-only v1 /scheduler panel payload.
func (p *Provider) SchedulerStatus() map[string]any {
	rawJobs := p.workbuddyJobs()
	jobs := make([]map[string]any, 0, len(rawJobs))
	now := p.jobsNowTime()
	next := time.Time{}
	for _, job := range rawJobs {
		row := map[string]any{"name": job.Name()}
		if candidate := job.NextRun(now); !candidate.IsZero() {
			row["next_run"] = candidate.Format("2006-01-02 15:04:05")
			if next.IsZero() || candidate.Before(next) {
				next = candidate
			}
		}
		if last := job.LastRun(); !last.IsZero() {
			row["last_run"] = last.Format("2006-01-02 15:04:05")
		}
		jobs = append(jobs, row)
	}
	lastRun := p.schedulerLastRunTime()
	lastText := "尚未运行"
	if !lastRun.IsZero() {
		lastText = lastRun.Format("2006-01-02 15:04:05")
	}
	nextText := "待调度"
	if !next.IsZero() {
		nextText = next.Format("2006-01-02 15:04:05")
	}
	checkin := formatScheduleHours(p.scheduleHours("checkin", defaultHours(9, 21)))
	travel := formatScheduleHours(p.scheduleHours("travel", defaultHours(9, 21)))
	keepalive := formatScheduleHours(p.scheduleHours("keepalive", defaultHours(22)))
	cat := formatScheduleHours(p.scheduleHours("cat", defaultHours(1)))
	modeCN := fmt.Sprintf("整点排程 (签到 %s · 猫猫旅行 %s · 每日活跃上报 · Token保活 %s · 夜猫 %s) + 模型额度窗口探针",
		checkin, travel, keepalive, cat)
	modeIntl := fmt.Sprintf("账号 Token 自动保活与凭证常驻 (每日 %s 集中巡检) + 模型额度窗口探针", keepalive)

	times := normalizeScheduleTimes(p.scheduleRaw("daily_chat"))
	randomSlots := len(times) == 0
	if len(times) == 0 {
		for _, hour := range p.scheduleHours("checkin", defaultHours(9, 21)) {
			times = append(times, fmt.Sprintf("%02d:00", hour))
		}
	}
	nextSlot := ""
	if daily := p.workbuddyJobByName(jobNameDailyActivity); daily != nil {
		if candidate := daily.NextRun(now); !candidate.IsZero() {
			nextSlot = candidate.Format("15:04")
		}
	}
	budgetSeconds := int(p.tasksBudget.Seconds())
	if budgetSeconds <= 0 {
		budgetSeconds = int(tasksBudgetDefault.Seconds())
	}
	dailyChat := map[string]any{
		"times": times, "random": randomSlots, "done_slots": []string{},
		"next_slot": nextSlot, "live": []any{},
		"budget_s": budgetSeconds, "abandoned": 0,
	}
	return map[string]any{
		"ok":            true,
		"enabled":       p.schedulerIsEnabled(),
		"realm":         p.realm,
		"mode":          modeCN,
		"mode_cn":       modeCN,
		"mode_intl":     modeIntl,
		"last_run_time": lastText,
		"next_run_time": nextText,
		"windows":       p.schedulerWindows(now),
		"quota_scans":   p.schedulerQuotaScans(now),
		"daily_chat":    dailyChat,
		"jobs":          jobs,
		"logs":          []string{},
	}
}

func (p *Provider) schedulerWindows(now time.Time) []map[string]any {
	path := filepath.Join(p.AccountsDir(), "model_windows.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	rows := schedulerStateRows(data, "windows")
	out := make([]map[string]any, 0, len(rows))
	for _, item := range rows {
		row := creditObject(item)
		uid := creditText(row["uid"])
		model := creditText(row["model"])
		if uid == "" || model == "" {
			continue
		}
		resetAt := creditNumber(row["reset_at"])
		resetIn := int(resetAt - float64(now.Unix()))
		if resetIn < 0 {
			resetIn = 0
		}
		out = append(out, map[string]any{
			"uid": uid, "uid8": shortUID(uid), "model": model,
			"reset_at": resetAt, "reset_in": resetIn,
			"attempts":    int(creditNumber(row["attempts"])),
			"last_result": creditText(row["last_result"]),
			"cap":         parseLooseBool(row["cap"]), "cap_at": creditNumber(row["cap_at"]),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["reset_at"].(float64) < out[j]["reset_at"].(float64)
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func (p *Provider) schedulerQuotaScans(now time.Time) []map[string]any {
	path := filepath.Join(p.AccountsDir(), "quota_scan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	rows := schedulerStateRows(data, "scans")
	out := make([]map[string]any, 0, len(rows))
	for _, item := range rows {
		row := creditObject(item)
		uid := creditText(row["uid"])
		model := creditText(row["model"])
		if uid == "" || model == "" {
			continue
		}
		nextAt := creditNumber(row["next_at"])
		dueIn := int(nextAt - float64(now.Unix()))
		if dueIn < 0 {
			dueIn = 0
		}
		out = append(out, map[string]any{
			"uid": uid, "uid8": shortUID(uid), "model": model,
			"watched": true, "pending": false,
			"last_scan":   creditNumber(row["last_scan"]),
			"last_result": creditText(row["last_result"]),
			"last_detail": creditText(row["last_detail"]),
			"due_in":      dueIn, "next_at": nextAt, "seen_at": creditNumber(row["seen_at"]),
			"attempts": int(creditNumber(row["attempts"])),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["due_in"].(int) < out[j]["due_in"].(int)
	})
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func schedulerStateRows(data []byte, key string) []any {
	var root any
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	switch typed := root.(type) {
	case map[string]any:
		rows, _ := anyList(typed[key])
		return rows
	case []any:
		return typed
	default:
		return nil
	}
}

func parseLooseBool(value any) bool {
	if boolean, ok := value.(bool); ok {
		return boolean
	}
	if number, ok := numericValue(value); ok {
		return number != 0
	}
	switch strings.ToLower(strings.TrimSpace(creditText(value))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func formatScheduleHours(hours []int) string {
	parts := make([]string, 0, len(hours))
	for _, hour := range hours {
		parts = append(parts, fmt.Sprintf("%02d:00", hour))
	}
	return strings.Join(parts, "/")
}

func (p *Provider) workbuddyJobByName(name string) *scheduledJob {
	for _, job := range p.workbuddyJobs() {
		if job.Name() == name {
			return job
		}
	}
	return nil
}

func (p *Provider) settingsSnapshot() map[string]any {
	if p == nil || p.store == nil {
		return map[string]any{}
	}
	settings, err := p.store.Load()
	if err != nil {
		return map[string]any{}
	}
	return settings
}

func (p *Provider) scheduleRaw(key string) any {
	settings := p.settingsSnapshot()
	raw, _ := settings["schedule"].(map[string]any)
	if raw == nil {
		return nil
	}
	return raw[key]
}

func (p *Provider) scheduleHours(key string, fallback []int) []int {
	if hours := normalizeScheduleHours(p.scheduleRaw(key)); len(hours) > 0 {
		return hours
	}
	return append([]int(nil), fallback...)
}

func (p *Provider) scheduleView() map[string]any {
	view := map[string]any{
		"checkin":   p.scheduleHours("checkin", defaultHours(9, 21)),
		"travel":    p.scheduleHours("travel", defaultHours(9, 21)),
		"keepalive": p.scheduleHours("keepalive", defaultHours(22)),
		"cat":       p.scheduleHours("cat", defaultHours(1)),
	}
	times := normalizeScheduleTimes(p.scheduleRaw("daily_chat"))
	if times == nil {
		times = []string{}
	}
	view["daily_chat"] = times
	if len(times) == 0 {
		effective := make([]string, 0)
		for _, hour := range p.scheduleHours("checkin", defaultHours(9, 21)) {
			effective = append(effective, fmt.Sprintf("%02d:00", hour))
		}
		view["daily_chat_effective"] = effective
		view["daily_chat_random"] = true
	} else {
		view["daily_chat_effective"] = times
		view["daily_chat_random"] = false
	}
	return view
}

func normalizeScheduleHours(value any) []int {
	items := []any{}
	switch typed := value.(type) {
	case []any:
		items = typed
	case []int:
		for _, item := range typed {
			items = append(items, item)
		}
	case []string:
		for _, item := range typed {
			items = append(items, item)
		}
	case string:
		for _, part := range strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\t'
		}) {
			items = append(items, part)
		}
	default:
		if typed == nil {
			return nil
		}
		items = []any{typed}
	}
	set := map[int]bool{}
	for _, item := range items {
		hour, ok := wbAdminInt(item)
		if !ok || hour < 0 || hour > 23 {
			continue
		}
		set[hour] = true
	}
	out := make([]int, 0, len(set))
	for hour := range set {
		out = append(out, hour)
	}
	sort.Ints(out)
	return out
}

func normalizeScheduleTimes(value any) []string {
	items := []any{}
	switch typed := value.(type) {
	case []any:
		items = typed
	case []string:
		for _, item := range typed {
			items = append(items, item)
		}
	case string:
		for _, part := range strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\t'
		}) {
			items = append(items, part)
		}
	case json.Number:
		items = []any{typed}
	}
	set := map[string]bool{}
	for _, item := range items {
		text := strings.ReplaceAll(strings.TrimSpace(wbAdminText(item)), "：", ":")
		parts := strings.Split(text, ":")
		if len(parts) != 2 {
			continue
		}
		hour, errHour := strconv.Atoi(parts[0])
		minute, errMinute := strconv.Atoi(parts[1])
		if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			continue
		}
		set[fmt.Sprintf("%02d:%02d", hour, minute)] = true
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (p *Provider) settingsBool(key string, fallback bool) bool {
	value, exists := p.settingsSnapshot()[key]
	if !exists || value == nil {
		return fallback
	}
	boolean, ok := value.(bool)
	if !ok {
		return false
	}
	return boolean
}

func (p *Provider) dailyActivityEnabled() bool {
	return p.settingsBool("daily_activity", true)
}

func (p *Provider) dailyChatWebEnabled() bool {
	return p.settingsBool("daily_chat_web", true)
}
