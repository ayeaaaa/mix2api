package doubao

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

type checkinJob struct {
	provider *Provider

	mu             sync.Mutex
	lastKey        string
	lastCatchupKey string
	retryAt        time.Time
	retryUntil     time.Time
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
	hourKey := now.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02T15")
	j.mu.Lock()
	if cfg.CheckinRetryMinutes <= 0 && (!j.retryAt.IsZero() || !j.retryUntil.IsZero()) {
		// Cancelling automatic retries must also cancel the pending retry
		// rather than immediately running it on this tick.
		j.retryAt = time.Time{}
		j.retryUntil = time.Time{}
		j.lastKey = key
		j.lastCatchupKey = hourKey
		j.mu.Unlock()
		return nil
	}
	if j.lastKey == key {
		if !j.retryAt.IsZero() {
			if now.Before(j.retryAt) {
				j.mu.Unlock()
				return nil
			}
		} else {
			if cfg.CheckinRetryMinutes <= 0 || j.lastCatchupKey == hourKey ||
				!j.provider.hasPendingCheckins(now) {
				j.mu.Unlock()
				return nil
			}
		}
		j.lastCatchupKey = hourKey
	} else {
		if !j.retryAt.IsZero() && now.Before(j.retryAt) {
			j.mu.Unlock()
			return nil
		}
		j.lastCatchupKey = hourKey
	}
	j.mu.Unlock()

	results, runErr := j.provider.CheckinPending(ctx)
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
		j.lastCatchupKey = hourKey
		j.retryAt = time.Time{}
		j.retryUntil = time.Time{}
		return nil
	}
	if cfg.CheckinRetryMinutes <= 0 {
		// Retries disabled: mark the day exhausted so the next tick does not
		// loop on the same failed batch.
		j.lastKey = key
		j.lastCatchupKey = hourKey
		j.retryAt = time.Time{}
		j.retryUntil = time.Time{}
		return runErr
	}
	until := endOfZoneDay(now, slot.Location())
	next := now.Add(time.Duration(cfg.CheckinRetryMinutes) * time.Minute)
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

// hasPendingCheckins reports whether any enabled account has no successful
// check-in stamped for the provider's local day. It is a local-only gate for
// the hourly catch-up.
func (p *Provider) hasPendingCheckins(now time.Time) bool {
	if p == nil || p.pool == nil {
		return false
	}
	day := now.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02")
	for _, account := range p.pool.Accounts() {
		if account == nil {
			continue
		}
		account.mu.Lock()
		enabled := account.Enabled
		last := strings.TrimSpace(account.LastCheckin)
		account.mu.Unlock()
		if !enabled {
			continue
		}
		if last != "" && strings.HasPrefix(last, day) {
			continue
		}
		return true
	}
	return false
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
