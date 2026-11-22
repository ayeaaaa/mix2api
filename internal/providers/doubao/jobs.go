package doubao

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

// checkinRetryInterval is the same-day retry spacing after a failed batch.
const checkinRetryInterval = 30 * time.Minute

type checkinJob struct {
	provider *Provider

	mu         sync.Mutex
	lastKey    string
	retryAt    time.Time
	retryUntil time.Time
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
	now := j.provider.nowTime()
	slot, ok := checkinSlot(now, cfg.CheckinTime)
	if !ok {
		return nil
	}
	key := slot.Format("2006-01-02")
	j.mu.Lock()
	if j.lastKey == key {
		j.mu.Unlock()
		return nil
	}
	if !j.retryAt.IsZero() && now.Before(j.retryAt) {
		j.mu.Unlock()
		return nil
	}
	j.mu.Unlock()

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
	j.mu.Lock()
	defer j.mu.Unlock()
	if runErr == nil && failed == 0 {
		j.lastKey = key
		j.retryAt = time.Time{}
		j.retryUntil = time.Time{}
		return nil
	}
	until := endOfZoneDay(now, slot.Location())
	next := now.Add(checkinRetryInterval)
	if !next.Before(until) {
		// No room for another attempt today; mark the day exhausted so a
		// restart cannot loop on the same slot.
		j.lastKey = key
		j.retryAt = time.Time{}
		j.retryUntil = time.Time{}
		return runErr
	}
	j.retryAt = next
	j.retryUntil = until
	return runErr
}

func endOfZoneDay(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
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
