package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadV1Fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v1", "usage-aggregate-cache-"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func patchV1Entry(t *testing.T, env *testEnv, entry map[string]any) map[string]any {
	t.Helper()
	info, err := os.Stat(env.Paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	position, ok := CachePosition(env.Paths.Log, info.Size())
	if !ok {
		t.Fatal("cannot build position")
	}
	for key, value := range position {
		if key == "mtime" {
			continue // v1 entries predate this optional field
		}
		entry[key] = value
	}
	delete(entry, "mtime")
	return entry
}

func writeV1Kind(t *testing.T, env *testEnv, kind string, entry map[string]any) {
	t.Helper()
	envelope := map[string]any{"schema": 5, "kind": kind, "entries": []any{entry}}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Cache.Path(kind), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestV1CacheFixturesAdopt(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-3600, "m", "intl", "a", 10, 1, nil),
	})
	t.Setenv("WB_USAGE_CACHE", "1")

	// Snapshot: pick the real v1 intl section.
	snapshotEnvelope := loadV1Fixture(t, "snapshot")
	var snapshotEntry map[string]any
	for _, raw := range snapshotEnvelope["entries"].([]any) {
		entry, ok := raw.(map[string]any)
		if ok && entry["realm"] == "intl" {
			snapshotEntry = entry
			break
		}
	}
	if snapshotEntry == nil {
		t.Fatal("no intl v1 snapshot entry")
	}
	writeV1Kind(t, env, "snapshot", patchV1Entry(t, env, snapshotEntry))

	analyticsEntry, _ := loadV1Fixture(t, "analytics")["entries"].([]any)[0].(map[string]any)
	if analyticsEntry == nil {
		t.Fatal("no v1 analytics entry")
	}
	writeV1Kind(t, env, "analytics", patchV1Entry(t, env, analyticsEntry))

	byAccountEntry, _ := loadV1Fixture(t, "by_account")["entries"].([]any)[0].(map[string]any)
	if byAccountEntry == nil {
		t.Fatal("no v1 by_account entry")
	}
	writeV1Kind(t, env, "by_account", patchV1Entry(t, env, byAccountEntry))

	seriesEntry, _ := loadV1Fixture(t, "series")["entries"].([]any)[0].(map[string]any)
	if seriesEntry == nil {
		t.Fatal("no v1 series entry")
	}
	writeV1Kind(t, env, "series", patchV1Entry(t, env, seriesEntry))

	remainingEntry, _ := loadV1Fixture(t, "remaining")["entries"].([]any)[0].(map[string]any)
	if remainingEntry == nil {
		t.Fatal("no v1 remaining entry")
	}
	delete(remainingEntry, "file")
	writeV1Kind(t, env, "remaining", patchV1Entry(t, env, remainingEntry))

	rescans := 0
	service := env.service()
	service.Now = func() time.Time { return now }
	service.AfterScan = func() { rescans++ }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{Realm: "intl"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Analytics(context.Background(), AnalyticsOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ByAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	minutes := seriesEntry["minutes"].([]any)
	firstMinute, _ := numericValue(minutes[0].([]any)[0])
	lastMinute, _ := numericValue(minutes[len(minutes)-1].([]any)[0])
	since := time.Unix(int64(firstMinute)*60, 0)
	until := time.Unix(int64(lastMinute)*60, 0)
	if _, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until, BucketSeconds: 60,
	}); err != nil {
		t.Fatal(err)
	}
	if rescans != 0 {
		t.Fatalf("v1 fixtures caused %d cold rescans", rescans)
	}
	state, _, _, ok := service.adoptRemaining(context.Background())
	if !ok || len(state.Usage) == 0 {
		t.Fatalf("v1 remaining entry not adopted: ok=%v usage=%d", ok, len(state.Usage))
	}
}

func TestV1SeriesCoverageFallsBackToColdScan(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-3600, "m", "intl", "a", 10, 1, nil),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	seriesEnvelope := loadV1Fixture(t, "series")
	seriesEntry := seriesEnvelope["entries"].([]any)[0].(map[string]any)
	writeV1Kind(t, env, "series", patchV1Entry(t, env, seriesEntry))
	minutes := seriesEntry["minutes"].([]any)
	firstMinute, _ := numericValue(minutes[0].([]any)[0])
	lastMinute, _ := numericValue(minutes[len(minutes)-1].([]any)[0])
	recentSince := time.Unix(int64(firstMinute)*60, 0)
	recentUntil := time.Unix(int64(lastMinute)*60, 0)

	rescans := 0
	service := env.service()
	service.Now = func() time.Time { return now }
	service.AfterScan = func() { rescans++ }
	if _, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &recentSince, Until: &recentUntil, BucketSeconds: 60,
	}); err != nil {
		t.Fatal(err)
	}
	if rescans != 0 {
		t.Fatalf("recent minute window cold-scanned: %d", rescans)
	}

	// Older than the six-hour minute ring: must cold-scan.
	olderSince := recentSince.Add(-24 * time.Hour)
	olderUntil := recentSince.Add(-23 * time.Hour)
	cold, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &olderSince, Until: &olderUntil, BucketSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rescans == 0 {
		t.Fatal("older-than-ring window was served by the v1 minute ring")
	}
	t.Setenv("WB_USAGE_CACHE", "0")
	coldService := env.service()
	coldService.Now = func() time.Time { return now }
	coldCold, err := coldService.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &olderSince, Until: &olderUntil, BucketSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(cold["series"])
	right, _ := json.Marshal(coldCold["series"])
	if string(left) != string(right) {
		t.Fatalf("adopted fallback differs from cold scan: %s vs %s", left, right)
	}
}

func TestV1SeriesNullDaysOrHoursColdScans(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(entry map[string]any) { entry["days_floor"] = nil },
		func(entry map[string]any) { delete(entry, "max_at") },
		func(entry map[string]any) {
			for _, raw := range entry["days"].(map[string]any) {
				if day, ok := raw.(map[string]any); ok {
					day["hours"] = nil
					return
				}
			}
		},
	} {
		now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
		env := newTestEnv(t, []map[string]any{
			row(float64(now.Unix())-3600, "m", "intl", "a", 10, 1, nil),
		})
		t.Setenv("WB_USAGE_CACHE", "1")
		entry := loadV1Fixture(t, "series")["entries"].([]any)[0].(map[string]any)
		mutate(entry)
		writeV1Kind(t, env, "series", patchV1Entry(t, env, entry))
		// Pick an hour window inside the fixture day range.
		since := time.Unix(int64(1793430000), 0)
		until := since.Add(time.Hour)
		rescans := 0
		service := env.service()
		service.Now = func() time.Time { return now }
		service.AfterScan = func() { rescans++ }
		if _, err := service.Timeseries(context.Background(), SeriesOptions{
			Range: "custom", Since: &since, Until: &until, BucketSeconds: 3600,
		}); err != nil {
			t.Fatal(err)
		}
		if rescans == 0 {
			t.Fatalf("null days_floor/hours did not cold-scan")
		}
	}
}

func TestV1CacheNullPricingColdScans(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-100, "m", "intl", "a", 10, 1, nil),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	envelope := loadV1Fixture(t, "snapshot")
	var entry map[string]any
	for _, raw := range envelope["entries"].([]any) {
		candidate, ok := raw.(map[string]any)
		if ok && candidate["realm"] == "intl" {
			entry = candidate
			break
		}
	}
	if entry == nil {
		t.Fatal("no intl snapshot fixture")
	}
	entry["pricing"] = nil
	writeV1Kind(t, env, "snapshot", patchV1Entry(t, env, entry))
	rescans := 0
	service := env.service()
	service.Now = func() time.Time { return now }
	service.AfterScan = func() { rescans++ }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{Realm: "intl"}); err != nil {
		t.Fatal(err)
	}
	if rescans == 0 {
		t.Fatal("pricing:null v1 checkpoint was adopted")
	}
}

func TestV1RemainingBuffersRepairAndRequireSections(t *testing.T) {
	entry := map[string]any{
		"events": []any{},
		"usage": map[string]any{
			"u\x1fm": map[string]any{
				"at":   []any{300, 100, 200},
				"cum":  []any{10, 20, 30},
				"head": 0,
				"base": 0,
			},
		},
	}
	state, ok := remainingStateFromV1(entry)
	if !ok {
		t.Fatal("out-of-order v1 remaining buffers rejected")
	}
	buffer := state.Usage["u\x1fm"]
	if buffer == nil || len(buffer.At) != 3 {
		t.Fatalf("buffer = %+v", buffer)
	}
	if buffer.At[0] != 100 || buffer.At[1] != 200 || buffer.At[2] != 300 {
		t.Fatalf("buffer not sorted: %+v", buffer.At)
	}
	if buffer.Tokens[0] != 10 || buffer.Tokens[1] != 10 || buffer.Tokens[2] != 10 {
		t.Fatalf("buffer token repair = %+v", buffer.Tokens)
	}
	for _, missing := range []map[string]any{
		{"usage": map[string]any{}},
		{"events": []any{}},
		{"events": []any{}, "usage": map[string]any{
			"u\x1fm": map[string]any{"at": []any{1}, "cum": []any{1}, "head": 0.5, "base": 0},
		}},
		{"events": []any{}, "usage": map[string]any{
			"u\x1fm": map[string]any{"at": []any{1}, "cum": []any{1}, "head": 0, "base": -1},
		}},
	} {
		if _, ok := remainingStateFromV1(missing); ok {
			t.Fatalf("invalid remaining entry was adopted: %+v", missing)
		}
	}
}

func TestV1SeriesHourWindowUsesDaysHours(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-3600, "m", "intl", "a", 10, 1, nil),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	entry := loadV1Fixture(t, "series")["entries"].([]any)[0].(map[string]any)
	writeV1Kind(t, env, "series", patchV1Entry(t, env, entry))
	since := time.Unix(1793430000, 0)
	until := since.Add(time.Hour)
	rescans := 0
	service := env.service()
	service.Now = func() time.Time { return now }
	service.AfterScan = func() { rescans++ }
	if _, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until, BucketSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	if rescans != 0 {
		t.Fatalf("hour window inside days[].hours cold-scanned")
	}
}

func buildClosedWindowV1Series(t *testing.T, rows []map[string]any) map[string]any {
	t.Helper()
	minutes := map[int64]*SeriesBucket{}
	hours := map[int64]*SeriesBucket{}
	days := map[string]map[string]any{}
	hits := make([]any, 0, len(rows))
	credits := make([]any, 0)
	maxAt := 0.0
	add := func(bucket *SeriesBucket, row map[string]any) {
		outcome := RowOutcome(Row(row))
		if outcome == "completed" {
			bucket.Requests++
			bucket.PromptTokens += intNumber(Row(row), "prompt_tokens")
			bucket.CompletionTokens += intNumber(Row(row), "completion_tokens")
			bucket.ReasoningTokens += intNumber(Row(row), "reasoning_tokens")
			bucket.CachedTokens += intNumber(Row(row), "cached_tokens")
			bucket.TotalTokens += intNumber(Row(row), "total_tokens")
		} else {
			bucket.Errors++
		}
		credit := number(Row(row), "credit")
		if outcome != "client_aborted" {
			bucket.Credit += credit
		}
	}
	for _, row := range rows {
		at := number(Row(row), "at")
		if at > maxAt {
			maxAt = at
		}
		hits = append(hits, int64(at))
		minuteKey := int64(at/60) * 60
		minute := minutes[minuteKey]
		if minute == nil {
			minute = &SeriesBucket{At: float64(minuteKey)}
			minutes[minuteKey] = minute
		}
		add(minute, row)
		credit := number(Row(row), "credit")
		if credit > 0 && RowOutcome(Row(row)) != "client_aborted" {
			credits = append(credits, map[string]any{
				"at": at, "iso": row["iso"], "model": row["model"], "account": row["account"],
				"credit": credit, "total_tokens": intNumber(Row(row), "total_tokens"),
			})
		}
		hourKey := localHourStart(time.Unix(int64(at), 0)).Unix()
		hour := hours[hourKey]
		if hour == nil {
			hour = &SeriesBucket{At: float64(hourKey)}
			hours[hourKey] = hour
		}
		add(hour, row)
		dayKey := localDayKey(at)
		day := days[dayKey]
		if day == nil {
			start, _ := dayKeyStart(dayKey)
			day = map[string]any{"at": start, "hours": map[string]any{}}
			days[dayKey] = day
		}
		hourRelative := fmt.Sprint(int64((at - number(Row(day), "at")) / 3600))
		hourRow := day["hours"].(map[string]any)[hourRelative]
		var hourMap map[string]any
		if hourRow == nil {
			hourMap = map[string]any{"at": float64(hourKey)}
			day["hours"].(map[string]any)[hourRelative] = hourMap
		} else {
			hourMap = hourRow.(map[string]any)
		}
		hourBucketFields(hourMap, hour)
		// day aggregate fields are derived from the hour bucket sums.
	}
	minutesRows := make([]any, 0, len(minutes))
	for key, bucket := range minutes {
		minutesRows = append(minutesRows, []any{
			key / 60, bucket.Requests, bucket.Errors, bucket.PromptTokens,
			bucket.CompletionTokens, bucket.ReasoningTokens, bucket.CachedTokens,
			bucket.TotalTokens, bucket.Credit,
		})
	}
	dayMap := map[string]any{}
	for key, day := range days {
		hoursMap := day["hours"].(map[string]any)
		var requests, errors, prompt, completion, reasoning, cached, total int64
		var credit float64
		for _, raw := range hoursMap {
			hour := raw.(map[string]any)
			requests += int64(numberValueAny(hour["requests"]))
			errors += int64(numberValueAny(hour["errors"]))
			prompt += int64(numberValueAny(hour["prompt_tokens"]))
			completion += int64(numberValueAny(hour["completion_tokens"]))
			reasoning += int64(numberValueAny(hour["reasoning_tokens"]))
			cached += int64(numberValueAny(hour["cached_tokens"]))
			total += int64(numberValueAny(hour["total_tokens"]))
			credit += numberValueAny(hour["credit"])
		}
		day["requests"] = requests
		day["errors"] = errors
		day["prompt_tokens"] = prompt
		day["completion_tokens"] = completion
		day["reasoning_tokens"] = reasoning
		day["cached_tokens"] = cached
		day["total_tokens"] = total
		day["credit"] = credit
		dayMap[key] = day
	}
	floor := ""
	for key := range days {
		if floor == "" || key < floor {
			floor = key
		}
	}
	return map[string]any{
		"minutes":       minutesRows,
		"days":          dayMap,
		"days_floor":    floor,
		"hits":          hits,
		"hits_overflow": false,
		"max_at":        maxAt,
		"credits":       credits,
		"realm":         nil,
	}
}

func hourBucketFields(target map[string]any, bucket *SeriesBucket) {
	target["requests"] = bucket.Requests
	target["errors"] = bucket.Errors
	target["prompt_tokens"] = bucket.PromptTokens
	target["completion_tokens"] = bucket.CompletionTokens
	target["reasoning_tokens"] = bucket.ReasoningTokens
	target["cached_tokens"] = bucket.CachedTokens
	target["total_tokens"] = bucket.TotalTokens
	target["credit"] = bucket.Credit
}

func TestV1SeriesClosedWindowMatchesColdScan(t *testing.T) {
	since60 := time.Date(2026, 1, 5, 9, 0, 0, 0, time.Local)
	hi60 := time.Date(2026, 1, 5, 10, 0, 0, 0, time.Local)
	since3600 := time.Date(2026, 1, 5, 8, 0, 0, 0, time.Local)
	sinceDay := time.Date(2026, 1, 4, 0, 0, 0, 0, time.Local)
	rows := []map[string]any{
		row(float64(time.Date(2026, 1, 4, 10, 5, 0, 0, time.Local).Unix()), "m", "intl", "a", 10, 1, map[string]any{"credit": 1.0}),
		row(float64(time.Date(2026, 1, 4, 10, 25, 0, 0, time.Local).Unix()), "m", "intl", "a", 20, 2, nil),
		row(float64(time.Date(2026, 1, 4, 11, 5, 0, 0, time.Local).Unix()), "m", "intl", "a", 30, 3, nil),
		row(float64(time.Date(2026, 1, 4, 11, 45, 0, 0, time.Local).Unix()), "m", "intl", "a", 40, 4, nil),
		row(float64(time.Date(2026, 1, 5, 9, 5, 0, 0, time.Local).Unix()), "m", "intl", "a", 50, 5, nil),
		row(float64(time.Date(2026, 1, 5, 9, 35, 0, 0, time.Local).Unix()), "m", "intl", "a", 60, 6, nil),
		row(float64(time.Date(2026, 1, 5, 10, 5, 0, 0, time.Local).Unix()), "m", "intl", "a", 70, 7, nil),
		row(float64(time.Date(2026, 1, 5, 10, 35, 0, 0, time.Local).Unix()), "m", "intl", "a", 80, 8, nil),
	}
	env := newTestEnv(t, rows)
	t.Setenv("WB_USAGE_CACHE", "1")
	entry := buildClosedWindowV1Series(t, rows)
	writeV1Kind(t, env, "series", patchV1Entry(t, env, entry))

	check := func(step int, since, until time.Time) {
		t.Helper()
		service := env.service()
		service.Now = func() time.Time { return time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local) }
		rescans := 0
		service.AfterScan = func() { rescans++ }
		adopted, err := service.Timeseries(context.Background(), SeriesOptions{
			Range: "custom", Since: &since, Until: &until, BucketSeconds: step,
		})
		if err != nil {
			t.Fatal(err)
		}
		if rescans != 0 {
			t.Fatalf("step %d closed window cold-scanned instead of using adopted buckets", step)
		}
		coldService := env.service()
		coldService.Cache = nil
		coldService.Now = service.Now
		cold, err := coldService.Timeseries(context.Background(), SeriesOptions{
			Range: "custom", Since: &since, Until: &until, BucketSeconds: step,
		})
		if err != nil {
			t.Fatal(err)
		}
		left, _ := json.Marshal(adopted["series"])
		right, _ := json.Marshal(cold["series"])
		if string(left) != string(right) {
			t.Fatalf("step %d closed series differs: adopted=%s cold=%s", step, left, right)
		}
		leftCredits, _ := json.Marshal(adopted["credits"])
		rightCredits, _ := json.Marshal(cold["credits"])
		if string(leftCredits) != string(rightCredits) {
			t.Fatalf("step %d closed credits differ: adopted=%s cold=%s", step, leftCredits, rightCredits)
		}
	}
	check(60, since60, hi60)
	check(3600, since3600, hi60)
	check(86400, sinceDay, localMidnight(hi60))
}

func TestV1CacheFingerprintRequiresFullShape(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"list pricing", func(entry map[string]any) { entry["pricing"] = []any{"on"} }},
		{"missing realm inputs", func(entry map[string]any) { delete(entry, "realm_inputs") }},
	}
	for _, testCase := range cases {
		now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
		env := newTestEnv(t, []map[string]any{
			row(float64(now.Unix())-100, "m", "intl", "a", 10, 1, nil),
		})
		t.Setenv("WB_USAGE_CACHE", "1")
		entry := loadV1Fixture(t, "snapshot")["entries"].([]any)[3].(map[string]any) // intl
		testCase.mutate(entry)
		writeV1Kind(t, env, "snapshot", patchV1Entry(t, env, entry))
		rescans := 0
		service := env.service()
		service.Now = func() time.Time { return now }
		service.AfterScan = func() { rescans++ }
		if _, err := service.Snapshot(context.Background(), SnapshotOptions{Realm: "intl"}); err != nil {
			t.Fatal(err)
		}
		if rescans == 0 {
			t.Fatalf("%s was adopted", testCase.name)
		}
	}
}
