package doubao

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

type checkinJob struct {
	provider *Provider

	mu      sync.Mutex
	lastKey string
}

var _ scheduler.Job = (*checkinJob)(nil)

func (j *checkinJob) Name() string { return "doubao-checkin" }

// Run checks the configured daily slot and performs one idempotent batch when
// due. The provider is disabled by default; the job is a no-op until the
// operator enables both the exit and checkin_enabled.
func (j *checkinJob) Run(ctx context.Context) error {
	if j == nil || j.provider == nil {
		return nil
	}
	cfg, err := j.provider.applySettings()
	if err != nil || !cfg.Enabled || !cfg.CheckinEnabled {
		return err
	}
	slot, ok := checkinSlot(j.provider.nowTime(), cfg.CheckinTime)
	if !ok {
		return nil
	}
	key := slot.Format("2006-01-02")
	j.mu.Lock()
	already := j.lastKey == key
	j.mu.Unlock()
	if already {
		return nil
	}

	results, runErr := j.provider.CheckinAll(ctx)
	success, alreadyCount, failed := 0, 0, 0
	for _, result := range results {
		switch result.Status {
		case "success":
			success++
		case "already":
			alreadyCount++
		default:
			failed++
		}
	}
	if len(results) > 0 {
		log.Printf("doubao: daily checkin success=%d already=%d failed=%d", success, alreadyCount, failed)
	}
	if runErr == nil {
		j.mu.Lock()
		j.lastKey = key
		j.mu.Unlock()
	}
	return runErr
}

// checkinSlot returns the configured Asia/Shanghai slot for the current day
// when it is due. A malformed time falls back to 09:15.
func checkinSlot(now time.Time, clock string) (time.Time, bool) {
	if !validClock(clock) {
		clock = defaultCheckinTime
	}
	zone := time.FixedZone("CST", 8*60*60)
	local := now.In(zone)
	hour, _ := parseInt(clock[:2])
	minute, _ := parseInt(clock[3:])
	slot := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, zone)
	if local.Before(slot) {
		return time.Time{}, false
	}
	return slot, true
}
