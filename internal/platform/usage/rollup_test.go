package usage

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestRollupAnalyticsMatchesColdScan(t *testing.T) {
	now := time.Now()
	rows := []map[string]any{
		row(float64(now.Add(-2*time.Hour).Unix()), "m", "intl", "a", 100, 10, map[string]any{"key": "k1", "credit": 1.5, "ttft_ms": 100}),
		row(float64(now.Add(-time.Hour).Unix()), "m", "cn", "b", 200, 20, map[string]any{"key": "k2", "error": true, "status": 429}),
		row(float64(now.Add(-30*time.Minute).Unix()), "m", "intl", "a", 50, 5, map[string]any{"key": ""}),
	}
	env := newTestEnv(t, rows)
	env.Now = now
	t.Setenv("WB_USAGE_CACHE", "0")

	cold := env.service()
	cold.Cache = nil
	coldPayload, err := cold.Analytics(context.Background(), AnalyticsOptions{Range: "today", Realm: "all"})
	if err != nil {
		t.Fatal(err)
	}

	warm := env.service()
	warm.Cache = nil
	warm.Rollup = NewRollup(env.Usage, env.Pricing)
	t.Cleanup(func() { _ = warm.Rollup.Close() })
	warmPayload, err := warm.Analytics(context.Background(), AnalyticsOptions{Range: "today", Realm: "all"})
	if err != nil {
		t.Fatal(err)
	}

	left, _ := json.Marshal(coldPayload)
	right, _ := json.Marshal(warmPayload)
	var leftValue, rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leftValue, rightValue) {
		t.Fatalf("rollup payload differs from cold scan\ncold=%s\nwarm=%s", left, right)
	}

	// A second service reading the same database must not need another log
	// fold; the table is already at the current offset.
	again := env.service()
	again.Cache = nil
	again.Rollup = NewRollup(env.Usage, env.Pricing)
	t.Cleanup(func() { _ = again.Rollup.Close() })
	againPayload, err := again.Analytics(context.Background(), AnalyticsOptions{Range: "today", Realm: "all"})
	if err != nil {
		t.Fatal(err)
	}
	againJSON, _ := json.Marshal(againPayload)
	var againValue any
	if err := json.Unmarshal(againJSON, &againValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rightValue, againValue) {
		t.Fatalf("second rollup read differs\nfirst=%s\nagain=%s", right, againJSON)
	}
}

func TestRollupAnalyticsScopedAndIncremental(t *testing.T) {
	now := time.Now()
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Add(-2*time.Hour).Unix()), "m", "intl", "a", 100, 10, map[string]any{"key": "k1"}),
		row(float64(now.Add(-time.Hour).Unix()), "m", "cn", "b", 200, 20, map[string]any{"key": "k2"}),
	})
	env.Now = now
	t.Setenv("WB_USAGE_CACHE", "0")
	rollup := NewRollup(env.Usage, env.Pricing)
	t.Cleanup(func() { _ = rollup.Close() })
	warm := env.service()
	warm.Cache = nil
	warm.Rollup = rollup
	payload, err := warm.Analytics(context.Background(), AnalyticsOptions{Range: "today", Realm: "intl"})
	if err != nil {
		t.Fatal(err)
	}
	summary := mustMap(t, mustMap(t, payload["summary"])["window"])
	if mustNumber(t, summary["requests"]) != 1 || mustNumber(t, summary["total_tokens"]) != 110 {
		t.Fatalf("scoped rollup summary = %+v", summary)
	}

	// Append after the checkpoint; the next read must fold only the new row.
	if err := env.Store.Append(row(float64(now.Add(-time.Minute).Unix()), "m", "intl", "a", 30, 3, map[string]any{"key": "k1"})); err != nil {
		t.Fatal(err)
	}
	warm.resultCache = map[string]cachedResult{}
	payload, err = warm.Analytics(context.Background(), AnalyticsOptions{Range: "today", Realm: "intl"})
	if err != nil {
		t.Fatal(err)
	}
	summary = mustMap(t, mustMap(t, payload["summary"])["window"])
	if mustNumber(t, summary["requests"]) != 2 || mustNumber(t, summary["total_tokens"]) != 143 {
		t.Fatalf("incremental rollup summary = %+v", summary)
	}
}
