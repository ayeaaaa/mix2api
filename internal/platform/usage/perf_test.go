package usage

import (
	"context"
	"testing"
	"time"
)

func TestPerfPercentiles(t *testing.T) {
	now := float64(time.Now().Unix())
	rows := make([]map[string]any, 0)
	for index := 1; index <= 10; index++ {
		rows = append(rows, row(now+float64(index), "m", "intl", "a", 10, int64(index), map[string]any{
			"ttft_ms": float64(index * 10), "gen_ms": 100.0, "elapsed_ms": 200.0,
			"tokens_per_sec": float64(index), "cache_hit_pct": float64(index),
		}))
	}
	rows = append(rows, row(now+20, "m", "intl", "a", 0, 0, map[string]any{
		"error": true, "outcome": "failed", "elapsed_ms": 300.0,
	}))
	rows = append(rows, row(now+21, "m", "intl", "a", 0, 0, map[string]any{
		"outcome": "client_aborted", "elapsed_ms": 400.0,
	}))
	env := newTestEnv(t, rows)
	service := env.service()
	perf, err := service.Perf(context.Background(), PerfOptions{Sample: 100})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, perf["sampled"]) != 12 || mustNumber(t, perf["success"]) != 10 {
		t.Fatalf("counts = %+v", perf)
	}
	if mustNumber(t, perf["errors"]) != 1 || mustNumber(t, perf["client_aborted"]) != 1 {
		t.Fatalf("outcomes = %+v", perf)
	}
	if got := mustNumber(t, perf["success_rate_pct"]); got < 90.8 || got > 91.0 {
		t.Fatalf("success rate = %v", got)
	}
	ttft := mustMap(t, perf["ttft_ms"])
	if mustNumber(t, ttft["p50"]) != 50 || mustNumber(t, ttft["p90"]) != 90 || mustNumber(t, ttft["p99"]) != 100 {
		t.Fatalf("ttft percentiles = %+v", ttft)
	}
	if mustNumber(t, ttft["samples"]) != 10 {
		t.Fatalf("ttft samples = %+v", ttft)
	}
	byModel := mustMap(t, perf["by_model"])
	model := mustMap(t, byModel["m"])
	if mustNumber(t, model["requests"]) != 12 || mustNumber(t, model["errors"]) != 1 || mustNumber(t, model["client_aborted"]) != 1 {
		t.Fatalf("by_model = %+v", model)
	}
	if _, ok := mustMap(t, perf["by_model_realm"])["m"]; !ok {
		t.Fatalf("by_model_realm missing")
	}
	if _, ok := mustMap(t, perf["by_model_acct"])["m"]; !ok {
		t.Fatalf("by_model_acct missing")
	}

	capped, err := service.Perf(context.Background(), PerfOptions{Sample: 10})
	if err != nil {
		t.Fatal(err)
	}
	if capped["sample_capped"] != true {
		t.Fatalf("sample_capped = %+v", capped["sample_capped"])
	}
}
