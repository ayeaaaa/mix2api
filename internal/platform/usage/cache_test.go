package usage

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
)

func cacheEntriesFor(t *testing.T, cache *Cache, kind string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(cache.Path(kind))
	if err != nil {
		t.Fatalf("%s cache not written: %v", kind, err)
	}
	var envelope struct {
		Schema  int              `json:"schema"`
		Kind    string           `json:"kind"`
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("%s cache is not JSON: %v", kind, err)
	}
	if envelope.Schema != cacheSchema || envelope.Kind != kind || len(envelope.Entries) == 0 {
		t.Fatalf("%s envelope = %+v", kind, envelope)
	}
	return envelope.Entries
}

func TestCacheCheckpointRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-3600, "m", "intl", "a", 100, 10, map[string]any{"credit": 1.0}),
		row(float64(now.Unix())-1800, "m", "cn", "b", 200, 20, map[string]any{"credit": 2.0}),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	service := env.service()
	service.Now = func() time.Time { return now }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Analytics(context.Background(), AnalyticsOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ByAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: ptrTime(now.Add(-2 * time.Hour)), Until: ptrTime(now),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Remaining(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"snapshot", "analytics", "by_account", "series", "remaining"} {
		cacheEntriesFor(t, env.Cache, kind)
	}
	if err := env.Store.Append(row(float64(now.Unix())-60, "m", "intl", "a", 300, 30, nil)); err != nil {
		t.Fatal(err)
	}
	fresh := env.service()
	fresh.Now = func() time.Time { return now }
	snapshot, err := fresh.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, snapshot["requests"]) != 3 || mustNumber(t, snapshot["total_tokens"]) != 660 {
		t.Fatalf("cache catch-up failed: %+v", snapshot)
	}
}

func TestCacheAdoptionAllKinds(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-3600, "m", "intl", "a", 100, 10, map[string]any{"credit": 1.0}),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	service := env.service()
	service.Now = func() time.Time { return now }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Analytics(context.Background(), AnalyticsOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ByAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-2 * time.Hour)
	until := now
	if _, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Remaining(context.Background()); err != nil {
		t.Fatal(err)
	}

	rescans := 0
	fresh := env.service()
	fresh.Now = func() time.Time { return now }
	fresh.AfterScan = func() { rescans++ }
	if _, err := fresh.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Analytics(context.Background(), AnalyticsOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.ByAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Remaining(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rescans != 0 {
		t.Fatalf("expected every kind to adopt its checkpoint, got %d full rescans", rescans)
	}
}

func TestCacheInvalidatesOnTruncationAndRewrite(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	first := row(float64(now.Unix())-100, "m", "intl", "a", 10, 1, nil)
	second := row(float64(now.Unix())-50, "m", "intl", "a", 20, 2, nil)
	env := newTestEnv(t, []map[string]any{first, second})
	t.Setenv("WB_USAGE_CACHE", "1")
	service := env.service()
	service.Now = func() time.Time { return now }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}

	firstLine, _ := json.Marshal(first)
	if err := os.WriteFile(env.Paths.Log, append(firstLine, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := env.service()
	fresh.Now = func() time.Time { return now }
	snapshot, err := fresh.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, snapshot["requests"]) != 1 {
		t.Fatalf("truncated log served stale cache: %+v", snapshot)
	}

	replacement := row(float64(now.Unix())-100, "m", "intl", "a", 99, 9, nil)
	replacementData, _ := json.Marshal(replacement)
	original, _ := os.ReadFile(env.Paths.Log)
	for len(replacementData)+1 < len(original) {
		replacementData = append(replacementData, ' ')
	}
	if err := os.WriteFile(env.Paths.Log, append(replacementData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh = env.service()
	fresh.Now = func() time.Time { return now }
	snapshot, err = fresh.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, snapshot["total_tokens"]) != 108 {
		t.Fatalf("same-size rewrite served stale cache: %+v", snapshot)
	}
}

func TestCacheCorruptSchemaAndKindFallback(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-100, "m", "intl", "a", 10, 1, nil),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	service := env.service()
	service.Now = func() time.Time { return now }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	path := env.Cache.Path("snapshot")
	for _, payload := range []string{
		"{broken",
		`{"schema":99,"kind":"snapshot","entries":[]}`,
		`{"schema":1,"kind":"by_account","entries":[]}`,
	} {
		if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh := env.service()
		fresh.Now = func() time.Time { return now }
		snapshot, err := fresh.Snapshot(context.Background(), SnapshotOptions{})
		if err != nil {
			t.Fatalf("corrupt cache was fatal: %v", err)
		}
		if mustNumber(t, snapshot["requests"]) != 1 {
			t.Fatalf("corrupt cache changed output: %+v", snapshot)
		}
	}
}

func TestCacheFingerprintInvalidation(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-100, "m", "intl", "a", 100, 10, nil),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	service := env.service()
	service.Now = func() time.Time { return now }
	first, err := service.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := mustNumber(t, first["cost_cny"])
	path := os.Getenv("WB_PRICING_FILE")
	doc := map[string]any{
		"meta": map[string]any{"usd_cny": 7.0},
		"models": map[string]any{
			"m": map[string]any{
				"display": "m", "source": "openrouter", "currency": "USD", "unit": 1000000,
				"or_id": "vendor/m",
				"flat":  map[string]any{"input_cache_hit": 1.0, "input_cache_miss": 4.0, "output": 3.0},
			},
		},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := pricing.Load(env.Accounts, env.Usage)
	if err != nil {
		t.Fatal(err)
	}
	fresh := env.service()
	fresh.Pricing = reloaded
	fresh.Now = func() time.Time { return now }
	second, err := fresh.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, second["cost_cny"]) == before {
		t.Fatalf("pricing change did not invalidate/repricing: %v", before)
	}
}

func TestETagPairing(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-100, "m", "intl", "a", 10, 1, nil),
	})
	clock := now
	service := env.service()
	service.Now = func() time.Time { return clock }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	first := service.QueryETag("snapshot", SnapshotETagKey("", Range{}))
	if first == "" {
		t.Fatal("no ETag recorded")
	}
	clock = now.Add(5 * time.Second)
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if second := service.QueryETag("snapshot", SnapshotETagKey("", Range{})); second != first {
		t.Fatalf("ETag should be stable within TTL: %q vs %q", first, second)
	}
	clock = now.Add(30 * time.Second)
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if third := service.QueryETag("snapshot", SnapshotETagKey("", Range{})); third == first {
		t.Fatalf("ETag should change on rebuild: %q", third)
	}
}

func TestCheckpointUsesScannedPrefixNotLaterSize(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		run  func(*Service) error
		load func(*Service) (float64, error)
	}{
		{
			name: "snapshot",
			run: func(service *Service) error {
				_, err := service.Snapshot(context.Background(), SnapshotOptions{})
				return err
			},
			load: func(service *Service) (float64, error) {
				value, err := service.Snapshot(context.Background(), SnapshotOptions{})
				if err != nil {
					return 0, err
				}
				number, _ := numericValue(value["requests"])
				return number, nil
			},
		},
		{
			name: "analytics",
			run: func(service *Service) error {
				_, err := service.Analytics(context.Background(), AnalyticsOptions{})
				return err
			},
			load: func(service *Service) (float64, error) {
				value, err := service.Analytics(context.Background(), AnalyticsOptions{})
				if err != nil {
					return 0, err
				}
				summary := value["summary"].(map[string]any)
				all := summary["all_time"].(AnalyticsStat)
				return float64(all.Requests), nil
			},
		},
		{
			name: "by_account",
			run: func(service *Service) error {
				_, err := service.ByAccount(context.Background())
				return err
			},
			load: func(service *Service) (float64, error) {
				value, err := service.ByAccount(context.Background())
				if err != nil {
					return 0, err
				}
				accounts := value["accounts"].([]map[string]any)
				number, _ := numericValue(accounts[0]["requests"])
				return number, nil
			},
		},
		{
			name: "series",
			run: func(service *Service) error {
				_, err := service.Timeseries(context.Background(), SeriesOptions{})
				return err
			},
			load: func(service *Service) (float64, error) {
				value, err := service.Timeseries(context.Background(), SeriesOptions{})
				if err != nil {
					return 0, err
				}
				var total float64
				for _, bucket := range value["series"].([]SeriesBucket) {
					total += float64(bucket.Requests)
				}
				return total, nil
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t, []map[string]any{
				row(float64(now.Unix())-3600, "m", "intl", "a", 10, 1, nil),
			})
			t.Setenv("WB_USAGE_CACHE", "1")
			service := env.service()
			service.Now = func() time.Time { return now }
			appended := false
			service.AfterScan = func() {
				if appended {
					return
				}
				appended = true
				_ = env.Store.Append(row(float64(now.Unix())-60, "m", "intl", "a", 20, 2, nil))
			}
			if err := testCase.run(service); err != nil {
				t.Fatal(err)
			}
			fresh := env.service()
			fresh.Now = func() time.Time { return now }
			count, err := testCase.load(fresh)
			if err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("checkpoint lost appended row: %v rows", count)
			}
		})
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func TestLegacyV3CacheMTimeOptional(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, []map[string]any{
		row(float64(now.Unix())-100, "m", "intl", "a", 10, 1, nil),
	})
	t.Setenv("WB_USAGE_CACHE", "1")
	service := env.service()
	service.Now = func() time.Time { return now }
	if _, err := service.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	entries := cacheEntriesFor(t, env.Cache, "snapshot")
	entry := entries[0]
	delete(entry, "mtime")
	legacy := map[string]any{"schema": 3, "snapshot": []any{entry}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(env.Cache.Dir+"/usage-aggregate-cache.json", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.Cache.Path("snapshot")); err != nil {
		t.Fatal(err)
	}
	rescans := 0
	fresh := env.service()
	fresh.Now = func() time.Time { return now }
	fresh.AfterScan = func() { rescans++ }
	if _, err := fresh.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if rescans != 0 {
		t.Fatalf("legacy v3 checkpoint with no mtime was not adopted")
	}
}
