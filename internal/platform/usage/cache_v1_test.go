package usage

import (
	"bytes"
	"context"
	"encoding/json"
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
