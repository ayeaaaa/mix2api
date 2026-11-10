package usage

import (
	"testing"
	"time"
)

func TestParseRangeCalendarWindows(t *testing.T) {
	now := time.Now()
	today := localMidnight(now)
	window := ParseRange("today", nil, nil)
	if window.Since == nil || window.Since.Unix() != today.Unix() || window.Until != nil {
		t.Fatalf("today = %+v", window)
	}
	week := ParseRange("week", nil, nil)
	offset := (int(now.Weekday()) + 6) % 7
	wantWeek := today.AddDate(0, 0, -offset)
	if week.Since == nil || week.Since.Unix() != wantWeek.Unix() {
		t.Fatalf("week start = %v, want %v", week.Since, wantWeek)
	}
	month := ParseRange("month", nil, nil)
	wantMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	if month.Since == nil || month.Since.Unix() != wantMonth.Unix() {
		t.Fatalf("month start = %v, want %v", month.Since, wantMonth)
	}
	for _, raw := range []string{"all", "", "unknown", "7d"} {
		if got := ParseRange(raw, nil, nil); got.Since != nil || got.Until != nil {
			t.Fatalf("%q should be unbounded: %+v", raw, got)
		}
	}
	// Custom one-sided, reversed, invalid and negative bounds.
	lo := time.Unix(1000, 0)
	hi := time.Unix(2000, 0)
	custom := ParseRange("custom", &lo, nil)
	if custom.Since == nil || custom.Since.Unix() != 1000 || custom.Until != nil {
		t.Fatalf("one-sided custom = %+v", custom)
	}
	reversed := ParseRange("custom", &hi, &lo)
	if reversed.Since.Unix() != 1000 || reversed.Until.Unix() != 2000 {
		t.Fatalf("reversed custom = %+v", reversed)
	}
	negative := time.Unix(-5, 0)
	invalid := ParseRange("custom", &negative, nil)
	if invalid.Since != nil {
		t.Fatalf("negative bound should be dropped: %+v", invalid)
	}
	query := ParseRangeQuery("custom", "not-a-number", "-10")
	if query.Since != nil || query.Until != nil {
		t.Fatalf("invalid query bounds should be dropped: %+v", query)
	}
	query = ParseRangeQuery("custom", "2000", "1000")
	if query.Since.Unix() != 1000 || query.Until.Unix() != 2000 {
		t.Fatalf("query custom swap = %+v", query)
	}
}
