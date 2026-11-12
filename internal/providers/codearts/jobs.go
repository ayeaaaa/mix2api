package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

const codeartsCheckinJobName = "codearts-checkin"

// checkinJobSlots is the WorkBuddy-compatible fallback: the same 09:00/21:00
// slots used by the domestic WorkBuddy check-in. The live setting is read from
// the shared schedule.checkin key so both providers stay aligned.
var checkinJobSlots = []int{9, 21}

type codeartsCheckinJob struct {
	provider *Provider

	mu          sync.Mutex
	lastRun     time.Time
	lastSlotKey string
}

var _ scheduler.Job = (*codeartsCheckinJob)(nil)

func (j *codeartsCheckinJob) Name() string {
	return codeartsCheckinJobName
}

// Run executes one due check-in cycle. The core scheduler may call it on every
// tick; the slot key makes each configured hour run at most once per day.
func (j *codeartsCheckinJob) Run(ctx context.Context) error {
	if j == nil || j.provider == nil {
		return errors.New("codearts: check-in job provider is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := j.provider.applySettings()
	if err != nil {
		return err
	}
	if !cfg.Enabled || !cfg.CreditsAutoClaim {
		return nil
	}
	now := j.provider.nowTime()
	if !j.due(now) {
		return nil
	}
	j.markSlot(now)
	result, err := j.provider.CheckinAll(ctx)
	if err != nil {
		return err
	}
	if result.OK {
		return nil
	}
	messages := make([]string, 0, len(result.Results))
	for _, row := range result.Results {
		if row.OK {
			continue
		}
		messages = append(messages, row.UID+": "+row.Message)
	}
	if len(messages) == 0 {
		return errors.New("codearts: check-in failed")
	}
	return errors.New(strings.Join(messages, "; "))
}

// Due reports whether the latest configured slot still needs a run.
func (j *codeartsCheckinJob) Due(now time.Time) bool {
	if j == nil || j.provider == nil {
		return false
	}
	cfg := j.provider.Config()
	if !cfg.Enabled || !cfg.CreditsAutoClaim {
		return false
	}
	return j.due(now)
}

func (j *codeartsCheckinJob) due(now time.Time) bool {
	latest, ok := latestCheckinHour(j.provider.checkinScheduleHours(), now)
	if !ok {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastSlotKey != slotKey(now, latest)
}

// NextRun returns the next configured hour strictly after now.
func (j *codeartsCheckinJob) NextRun(now time.Time) time.Time {
	if j == nil || j.provider == nil {
		return time.Time{}
	}
	hours := j.provider.checkinScheduleHours()
	for _, hour := range hours {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if candidate.After(now) {
			return candidate
		}
	}
	if len(hours) == 0 {
		return time.Time{}
	}
	tomorrow := now.AddDate(0, 0, 1)
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), hours[0], 0, 0, 0, now.Location())
}

// LastRun returns the last attempted slot start, or the zero time.
func (j *codeartsCheckinJob) LastRun() time.Time {
	if j == nil {
		return time.Time{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastRun
}

func (j *codeartsCheckinJob) markSlot(now time.Time) {
	latest, ok := latestCheckinHour(j.provider.checkinScheduleHours(), now)
	if !ok {
		return
	}
	j.mu.Lock()
	j.lastRun = now
	j.lastSlotKey = slotKey(now, latest)
	j.mu.Unlock()
}

func slotKey(now time.Time, hour int) string {
	return fmt.Sprintf("%s %02d:00", now.Format("2006-01-02"), hour)
}

func latestCheckinHour(hours []int, now time.Time) (int, bool) {
	best := 0
	found := false
	for _, hour := range hours {
		if hour > now.Hour() {
			continue
		}
		if !found || hour > best {
			best = hour
			found = true
		}
	}
	return best, found
}

// checkinScheduleHours reads schedule.checkin from the shared settings file.
// Missing or malformed values fall back to the WorkBuddy default slots.
func (p *Provider) checkinScheduleHours() []int {
	if p == nil || p.store == nil {
		return append([]int(nil), checkinJobSlots...)
	}
	settings, err := p.store.Load()
	if err != nil {
		return append([]int(nil), checkinJobSlots...)
	}
	raw, _ := settings["schedule"].(map[string]any)
	hours := normalizeCheckinHours(raw["checkin"])
	if len(hours) == 0 {
		return append([]int(nil), checkinJobSlots...)
	}
	return hours
}

func normalizeCheckinHours(value any) []int {
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
		hour, ok := checkinHour(item)
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

func checkinHour(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err == nil {
			return int(number), true
		}
		float, err := typed.Float64()
		if err == nil {
			return int(float), true
		}
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		return number, err == nil
	}
	return 0, false
}
