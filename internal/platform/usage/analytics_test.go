package usage

import (
	"context"
	"testing"
	"time"
)

func TestAnalyticsPerKeyAndAccount(t *testing.T) {
	now := float64(time.Now().Unix())
	noKeyField := row(now-40, "m", "intl", "acct-a", 40, 4, nil)
	delete(noKeyField, "key")
	errorRow := row(now-50, "m", "intl", "acct-a", 0, 0, map[string]any{"error": true, "outcome": "failed", "key": "k1", "credit": 2.0})
	rows := []map[string]any{
		row(now, "m", "intl", "acct-a", 100, 10, map[string]any{"key": "k1"}),
		row(now-10, "m", "cn", "acct-b", 200, 20, map[string]any{"key": "k1"}),
		row(now-20, "m", "intl", "", 30, 3, map[string]any{"key": ""}),
		row(now-30, "m", "intl", "acct-a", 50, 5, map[string]any{"key": "k2"}),
		noKeyField,
		errorRow,
	}
	env := newTestEnv(t, rows)
	service := env.service()
	service.Keys = []KeyInfo{{ID: "k1", Name: "One", Realm: "", Enabled: true}}
	payload, err := service.Analytics(context.Background(), AnalyticsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	summary := mustMap(t, payload["summary"])
	all := mustMap(t, summary["all_time"])
	if mustNumber(t, all["requests"]) != 5 || mustNumber(t, all["errors"]) != 1 {
		t.Fatalf("all_time summary = %+v", all)
	}
	if mustNumber(t, all["total_tokens"]) != 462 {
		t.Fatalf("token totals = %+v", all)
	}
	keys := payload["keys"].([]map[string]any)
	byKey := map[string]map[string]any{}
	for _, entry := range keys {
		byKey[entry["key"].(string)] = entry
	}
	k1 := byKey["k1"]
	if k1 == nil {
		t.Fatalf("k1 row missing: %+v", keys)
	}
	realms := k1["realms"].(map[string]int64)
	if realms["intl"] == 0 || realms["cn"] == 0 {
		t.Fatalf("k1 realms = %+v", realms)
	}
	if mustNumber(t, k1["last_at"]) != now {
		t.Fatalf("k1 last_at = %v", k1["last_at"])
	}
	models := k1["models"].([]map[string]any)
	if len(models) == 0 || models[0]["model"] != "m" {
		t.Fatalf("k1 top models = %+v", models)
	}
	for _, bucket := range []string{KeyBucketAnon, KeyBucketBefore, KeyBucketUnknown} {
		if byKey[bucket] == nil {
			t.Fatalf("bucket %s missing: %+v", bucket, byKey)
		}
	}
	if mustNumber(t, byKey[KeyBucketUnknown]["all_time"].(AnalyticsStat).Requests) != 1 {
		t.Fatalf("unknown bucket did not absorb k2")
	}

	// Window half: only rows at/after local midnight.
	today := float64(localMidnight(time.Now()).Unix())
	window, err := service.Analytics(context.Background(), AnalyticsOptions{Range: "today"})
	if err != nil {
		t.Fatal(err)
	}
	windowSummary := mustMap(t, mustMap(t, window["summary"])["window"])
	if mustNumber(t, windowSummary["requests"]) != 5 || mustNumber(t, windowSummary["errors"]) != 1 {
		t.Fatalf("window summary = %+v (today=%v)", windowSummary, today)
	}
	accounts := payload["accounts"].([]map[string]any)
	if len(accounts) < 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
}
