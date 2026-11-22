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
	latest, ok := latestCheckinSlot(j.provider.checkinScheduleSlots(), now)
	if !ok {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastSlotKey != latest.key(now)
}

// NextRun returns the next configured hour strictly after now.
func (j *codeartsCheckinJob) NextRun(now time.Time) time.Time {
	if j == nil || j.provider == nil {
		return time.Time{}
	}
	slots := j.provider.checkinScheduleSlots()
	for _, slot := range slots {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), slot.Hour, slot.Minute, 0, 0, now.Location())
		if candidate.After(now) {
			return candidate
		}
	}
	if len(slots) == 0 {
		return time.Time{}
	}
	tomorrow := now.AddDate(0, 0, 1)
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(),
		slots[0].Hour, slots[0].Minute, 0, 0, now.Location())
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
	latest, ok := latestCheckinSlot(j.provider.checkinScheduleSlots(), now)
	if !ok {
		return
	}
	j.mu.Lock()
	j.lastRun = now
	j.lastSlotKey = latest.key(now)
	j.mu.Unlock()
}

type checkinSlot struct {
	Hour   int
	Minute int
}

func (s checkinSlot) String() string {
	return fmt.Sprintf("%02d:%02d", s.Hour, s.Minute)
}

func (s checkinSlot) key(now time.Time) string {
	return now.Format("2006-01-02") + " " + s.String()
}

func latestCheckinSlot(slots []checkinSlot, now time.Time) (checkinSlot, bool) {
	current := now.Hour()*60 + now.Minute()
	best := checkinSlot{}
	found := false
	for _, slot := range slots {
		minute := slot.Hour*60 + slot.Minute
		if minute > current {
			continue
		}
		if !found || minute > best.Hour*60+best.Minute {
			best = slot
			found = true
		}
	}
	return best, found
}

// checkinScheduleSlots prefers the provider's explicit checkin_time; an empty
// value keeps the historical WorkBuddy schedule.checkin hours.
func (p *Provider) checkinScheduleSlots() []checkinSlot {
	if p != nil {
		if cfg := p.Config(); strings.TrimSpace(cfg.CheckinTime) != "" {
			if slot, ok := parseCheckinSlot(cfg.CheckinTime); ok {
				return []checkinSlot{slot}
			}
		}
		// No CodeArts-specific time: follow the WorkBuddy effective time,
		// including its newer explicit checkin_time override.
		if p.store != nil {
			if settings, err := p.store.Load(); err == nil {
				raw, _ := settings["schedule"].(map[string]any)
				if text, _ := raw["checkin_time"].(string); strings.TrimSpace(text) != "" {
					if slot, ok := parseCheckinSlot(text); ok {
						return []checkinSlot{slot}
					}
				}
			}
		}
	}
	hours := p.checkinScheduleHours()
	out := make([]checkinSlot, 0, len(hours))
	for _, hour := range hours {
		out = append(out, checkinSlot{Hour: hour})
	}
	return out
}

func parseCheckinSlot(value string) (checkinSlot, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return checkinSlot{}, false
	}
	hour, errHour := strconv.Atoi(parts[0])
	minute, errMinute := strconv.Atoi(parts[1])
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return checkinSlot{}, false
	}
	return checkinSlot{Hour: hour, Minute: minute}, true
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
