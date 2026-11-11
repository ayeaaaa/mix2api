package raccoon

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

// dailyActiveJob runs the opt-in daily-activity check-in at the v1 default
// slots. The job itself is always registered; Run is a no-op until the
// provider and daily_active setting are enabled.
type dailyActiveJob struct {
	provider *Provider

	mu      sync.Mutex
	lastKey string
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
	slot, ok := dailyActiveSlot(j.provider.nowTime())
	if !ok {
		return nil
	}
	key := slot.Format("2006-01-02T15")
	j.mu.Lock()
	already := j.lastKey == key
	j.mu.Unlock()
	if already {
		return nil
	}

	summary, err := j.provider.pool.DailyActive(ctx, "", false)
	if err != nil {
		return err
	}
	if summary.Checked > 0 {
		log.Printf("raccoon: daily active checked=%d granted=%d points=%d",
			summary.Checked, summary.Granted, summary.Points)
	}

	j.mu.Lock()
	j.lastKey = key
	j.mu.Unlock()
	return nil
}

// dailyActiveSlot returns the most recent 09:00/21:00 local slot at or before
// now. Before 09:00 no slot is due for the current day.
func dailyActiveSlot(now time.Time) (time.Time, bool) {
	local := now.Local()
	for _, hour := range []int{21, 9} {
		slot := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, local.Location())
		if !local.Before(slot) {
			return slot, true
		}
	}
	return time.Time{}, false
}
