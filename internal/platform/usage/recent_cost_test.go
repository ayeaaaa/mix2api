package usage

import (
	"context"
	"testing"
	"time"
)

func TestRecentCostDecoration(t *testing.T) {
	now := float64(time.Now().Unix())
	rows := []map[string]any{
		row(now-1, "missing", "intl", "a", 10, 1, map[string]any{"key": "k1"}),
		row(now, "m", "intl", "a", 1000, 100, map[string]any{"cached_tokens": 400, "key": "k1"}),
	}
	env := newTestEnv(t, rows)
	service := env.service()
	service.KeyName = func(id string) string {
		if id == "k1" {
			return "Key One"
		}
		return id
	}
	page, err := service.Recent(context.Background(), RecentOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.USDCNY != 7 {
		t.Fatalf("usd_cny = %v", page.USDCNY)
	}
	known := page.Rows[0]
	if known["key_name"] != "Key One" {
		t.Fatalf("key_name = %v", known["key_name"])
	}
	if known["cost_cny"] == nil || mustNumber(t, known["cost_cny"]) <= 0 {
		t.Fatalf("known cost missing: %+v", known)
	}
	rates := mustMap(t, known["cost_rates"])
	if mustNumber(t, rates["output"]) != 3 {
		t.Fatalf("cost rates = %+v", rates)
	}
	if known["cost_source"] != "builtin" {
		t.Fatalf("cost source = %v", known["cost_source"])
	}
	unknown := page.Rows[1]
	for _, field := range []string{"cost_cny", "cost_band", "cost_source", "cost_source_at", "cost_rates", "cost_unit", "cost_currency", "cost_usd_cny", "cost_or_id", "cost_via", "cost_inherited_from", "cost_override_from", "cost_band_note", "cost_via_derived"} {
		if unknown[field] != nil {
			t.Fatalf("unknown model field %s = %v", field, unknown[field])
		}
	}
	if unknown["cost_backfilled"] != false {
		t.Fatalf("unknown backfilled = %v", unknown["cost_backfilled"])
	}
}
