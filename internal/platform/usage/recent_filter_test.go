package usage

import (
	"context"
	"testing"
	"time"
)

func TestRecentFiltersByAccountModelKeyAndOutcome(t *testing.T) {
	now := time.Now()
	rows := []map[string]any{
		row(float64(now.Unix()), "m1", "intl", "acct-a", 10, 1, map[string]any{"key": "k1", "outcome": "completed"}),
		row(float64(now.Add(-time.Second).Unix()), "m1", "intl", "acct-a", 20, 2, map[string]any{"key": "k1", "outcome": "failed", "error": "boom"}),
		row(float64(now.Add(-2*time.Second).Unix()), "m2", "intl", "acct-b", 30, 3, map[string]any{"key": "k2", "outcome": "completed"}),
		row(float64(now.Add(-3*time.Second).Unix()), "m2", "intl", "acct-b", 40, 4, map[string]any{"key": "k2", "outcome": "upstream_aborted"}),
	}
	env := newTestEnv(t, rows)
	page, err := env.Store.Recent(RecentOptions{Account: "acct-a", Outcome: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0]["key"] != "k1" || !page.TotalExact {
		t.Fatalf("account/outcome page = %+v", page)
	}
	page, err = env.Store.Recent(RecentOptions{Model: "m2", Key: "k2", Outcome: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Rows) != 1 || intNumber(Row(page.Rows[0]), "total_tokens") != 44 {
		t.Fatalf("model/key/outcome page = %+v", page)
	}
	page, err = env.Store.Recent(RecentOptions{Outcome: "completed", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("completed page = %+v", page)
	}
	if _, err := env.Store.Recent(RecentOptions{Account: "missing", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	_ = context.Background()
}
