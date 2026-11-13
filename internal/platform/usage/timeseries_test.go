package usage

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestTimeseriesBuckets(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	since := time.Date(2026, 1, 5, 10, 0, 0, 0, time.Local)
	until := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	rows := []map[string]any{
		row(float64(since.Unix()), "m", "intl", "a", 10, 1, map[string]any{"credit": 1.0}),
		row(float64(since.Add(30*time.Minute).Unix()), "m", "intl", "a", 20, 2, map[string]any{"credit": 2.0}),
		row(float64(since.Add(60*time.Minute).Unix()), "m", "intl", "a", 0, 0, map[string]any{
			"error": true, "outcome": "failed", "credit": 3.0,
		}),
		row(float64(since.Add(70*time.Minute).Unix()), "m", "intl", "a", 0, 0, map[string]any{
			"outcome": "client_aborted", "credit": 9.0,
		}),
	}
	env := newTestEnv(t, rows)
	service := env.service()
	service.Now = func() time.Time { return now }
	payload, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until, BucketSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	series := payload["series"].([]SeriesBucket)
	if len(series) != 2 {
		t.Fatalf("series = %+v", series)
	}
	if series[0].At != float64(since.Unix()) || series[0].Requests != 2 || series[0].Credit != 3 {
		t.Fatalf("first bucket = %+v", series[0])
	}
	if series[1].Errors != 2 || series[1].Credit != 3 {
		t.Fatalf("second bucket = %+v", series[1])
	}
	credits := payload["credits"].([]SeriesCredit)
	if len(credits) != 4 {
		t.Fatalf("credit history = %+v", credits)
	}
	if credits[0].Credit != 9 || credits[1].Credit != 3 || credits[2].Credit != 2 || credits[3].Credit != 1 {
		t.Fatalf("credit order = %+v", credits)
	}

	customBucket, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until, BucketSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	customSeries := customBucket["series"].([]SeriesBucket)
	if len(customSeries) == 0 || customSeries[0].At != float64(since.Unix()) {
		t.Fatalf("custom bucket should anchor at window start: %+v", customSeries)
	}
}

func TestSeriesAdoptsGlobalV1Cache(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	rows := []map[string]any{
		row(float64(now.Add(-time.Hour).Unix()), "m", "intl", "a", 100, 10, nil),
	}
	env := newTestEnv(t, rows)
	env.Now = now
	t.Setenv("WB_USAGE_CACHE", "1")
	info, err := os.Stat(env.Paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	position, ok := CachePosition(env.Paths.Log, info.Size())
	if !ok {
		t.Fatal("position")
	}
	minute := float64(now.Add(-time.Hour).Unix() / 60)
	entry := map[string]any{
		"realm":        "",
		"minutes":      []any{[]any{minute, 9, 0, 900, 90, 0, 0, 990, 4.5}},
		"minutes_from": float64(now.Add(-2*time.Hour).Unix() / 60),
		"days":         map[string]any{},
		"days_floor":   now.Format("2006-01-02"),
		"max_at":       float64(now.Unix()),
		"hits":         []any{},
		"log":          "/old/migrated/usage.jsonl",
	}
	for key, value := range position {
		entry[key] = value
	}
	doc, err := json.Marshal(map[string]any{
		"schema": cacheSchema, "kind": "series", "entries": []any{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Cache.Path("series"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	service := env.service()
	state, ok := service.adoptSeries(context.Background(), "")
	if !ok {
		t.Fatal("global v1 series cache was not adopted")
	}
	minuteKey := int64(minute) * 60
	bucket := state.Minutes[minuteKey]
	if bucket == nil || bucket.Requests != 9 || bucket.Credit != 4.5 {
		t.Fatalf("minutes = %+v, want the v1 minute cache", bucket)
	}
}

func TestSeriesRejectsGlobalV1CacheForScopedRealm(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Add(-time.Hour).Unix()), "m", "intl", "a", 100, 10, nil),
	})
	env.Now = now
	t.Setenv("WB_USAGE_CACHE", "1")
	info, err := os.Stat(env.Paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	position, ok := CachePosition(env.Paths.Log, info.Size())
	if !ok {
		t.Fatal("position")
	}
	minute := float64(now.Add(-time.Hour).Unix() / 60)
	entry := map[string]any{
		"realm":        "",
		"minutes":      []any{[]any{minute, 9, 0, 900, 90, 0, 0, 990, 4.5}},
		"minutes_from": float64(now.Add(-2*time.Hour).Unix() / 60),
		"days":         map[string]any{},
		"days_floor":   now.Format("2006-01-02"),
		"max_at":       float64(now.Unix()),
		"hits":         []any{},
	}
	for key, value := range position {
		entry[key] = value
	}
	doc, err := json.Marshal(map[string]any{"schema": cacheSchema, "kind": "series", "entries": []any{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Cache.Path("series"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.service().adoptSeries(context.Background(), "intl"); ok {
		t.Fatal("scoped realm adopted global v1 series cache")
	}
}

func TestSeriesWindowAllowsEmptyCurrentPeriod(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 1, 30, 0, time.Local)
	lo := localMidnight(now)
	yesterday := now.AddDate(0, 0, -1)
	state := seriesState{
		Minutes: map[int64]*SeriesBucket{
			int64(yesterday.Add(-time.Hour).Unix()): {At: float64(yesterday.Add(-time.Hour).Unix()), Requests: 1},
		},
		MinutesFrom:     float64(yesterday.Add(-6 * time.Hour).Unix()),
		MaxAt:           float64(yesterday.Add(-time.Minute).Unix()),
		MinuteComplete:  true,
		MinutesCoverAll: false,
		Hours: map[int64]*SeriesBucket{
			int64(yesterday.Add(-time.Hour).Unix()): {At: float64(yesterday.Add(-time.Hour).Unix()), Requests: 1},
		},
		HoursFloor:    yesterday.Format("2006-01-02"),
		HoursMaxAt:    float64(yesterday.Add(-time.Minute).Unix()),
		HoursComplete: true,
	}
	for _, step := range []int{60, 3600, 86400} {
		buckets, _, ok := seriesSlice(state, lo, now, step)
		if !ok {
			t.Fatalf("step %d: empty current period should be servable from cache", step)
		}
		if len(buckets) != 0 {
			t.Fatalf("step %d: buckets = %+v, want empty", step, buckets)
		}
	}
}
