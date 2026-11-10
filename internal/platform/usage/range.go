package usage

import (
	"strconv"
	"strings"
	"time"
)

// Range is a resolved inclusive window on a row's at timestamp. A nil bound is
// unbounded on that side, matching v1 range_window().
type Range struct {
	Since *time.Time
	Until *time.Time
}

// ParseRange resolves the dashboard's selector. today/week/month are local
// calendar windows anchored to local midnight (week starts Monday); custom
// uses the caller's bounds, swaps reversed bounds, and drops unparseable or
// negative values; everything else is unbounded.
func ParseRange(raw string, since, until *time.Time) Range {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "today", "day", "1d":
		start := localMidnight(time.Now())
		return Range{Since: &start}
	case "week", "w":
		now := time.Now()
		offset := (int(now.Weekday()) + 6) % 7
		start := localMidnight(now).AddDate(0, 0, -offset)
		return Range{Since: &start}
	case "month", "m":
		now := time.Now()
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
		return Range{Since: &start}
	case "custom":
		lo := validEpochBound(since)
		hi := validEpochBound(until)
		if lo != nil && hi != nil && hi.Before(*lo) {
			lo, hi = hi, lo
		}
		return Range{Since: lo, Until: hi}
	default:
		return Range{}
	}
}

// ParseRangeQuery parses the panel's raw epoch strings and resolves the
// selector in one call. Invalid or negative values become unbounded.
func ParseRangeQuery(raw, since, until string) Range {
	return ParseRange(raw, parseEpoch(since), parseEpoch(until))
}

func parseEpoch(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return nil
	}
	when := time.Unix(int64(value), 0)
	return &when
}

func validEpochBound(value *time.Time) *time.Time {
	if value == nil || value.IsZero() || value.Unix() < 0 {
		return nil
	}
	copy := *value
	return &copy
}

func localMidnight(when time.Time) time.Time {
	return time.Date(when.Year(), when.Month(), when.Day(), 0, 0, 0, 0, time.Local)
}

func localHourStart(when time.Time) time.Time {
	return time.Date(when.Year(), when.Month(), when.Day(), when.Hour(), 0, 0, 0, time.Local)
}

func inRange(at float64, window Range) bool {
	if window.Since != nil && at < float64(window.Since.Unix()) {
		return false
	}
	if window.Until != nil && at > float64(window.Until.Unix()) {
		return false
	}
	return true
}

func epochValue(value any) float64 {
	number, _ := numericValue(value)
	return number
}
