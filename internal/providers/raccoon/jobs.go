package raccoon

import (
	"context"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

// dailyActiveRandomMinute picks a minute in the four-hour window. It is a
// variable so tests can make the schedule deterministic.
var dailyActiveRandomMinute = func() int {
	return rand.IntN(4 * 60)
}

// dailyActiveJob runs the opt-in daily-activity check-in at one random minute
// per four-hour window. A failed window is retried in the next window; once
// today's grant is present, DailyActive verifies the bill and performs no
// additional trigger. The job itself is always registered; Run is a no-op until
// the provider and daily_active setting are enabled.
type dailyActiveJob struct {
	provider *Provider

	mu        sync.Mutex
	lastKey   string
	windowKey string
	slotAt    time.Time
}

var _ scheduler.Job = (*dailyActiveJob)(nil)

func (j *dailyActiveJob) Name() string { return "raccoon-daily-active" }

func (j *dailyActiveJob) Run(ctx context.Context) error {
	if j == nil || j.provider == nil {
		return nil
	}
	cfg, err := j.provider.applySettings()
	if err != nil {
		return err
	}
	if !cfg.Enabled || !cfg.DailyActive {
		return nil
	}

	now := j.provider.nowTime()
	window := dailyActiveWindow(now)
	key := window.Format("2006-01-02T15")
	j.mu.Lock()
	if j.windowKey != key {
		j.windowKey = key
		j.slotAt = dailyActiveSlotAt(window, dailyActiveRandomMinute())
	}
	slotAt := j.slotAt
	already := j.lastKey == key
	j.mu.Unlock()
	if already || now.Before(slotAt) {
		return nil
	}

	summary, err := j.provider.pool.DailyActive(ctx, "", false)
	// Mark the window as attempted even on failure. A transient upstream error
	// must wait for the next four-hour window rather than retrying every tick.
	j.mu.Lock()
	j.lastKey = key
	j.mu.Unlock()
	if err != nil {
		return err
	}
	if summary.Checked > 0 {
		log.Printf("raccoon: daily active checked=%d granted=%d points=%d",
			summary.Checked, summary.Granted, summary.Points)
	}
	return nil
}

// dailyActiveWindow returns the start of the current four-hour local window.
func dailyActiveWindow(now time.Time) time.Time {
	local := now.Local()
	hour := local.Hour() - local.Hour()%4
	return time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, local.Location())
}

// dailyActiveSlot returns the random-minute trigger inside the current
// four-hour window and whether that minute has arrived.
func dailyActiveSlot(now time.Time, randomMinute int) (time.Time, bool) {
	slot := dailyActiveSlotAt(dailyActiveWindow(now), randomMinute)
	return slot, !now.Before(slot)
}

func dailyActiveSlotAt(window time.Time, randomMinute int) time.Time {
	if randomMinute < 0 {
		randomMinute = 0
	}
	if randomMinute >= 4*60 {
		randomMinute = 4*60 - 1
	}
	return window.Add(time.Duration(randomMinute) * time.Minute)
}
