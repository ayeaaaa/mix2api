package usage

import (
	"context"
	"testing"
	"time"
)

func TestKeyTokensTodayCountsTodayByKey(t *testing.T) {
	now := time.Now()
	rows := []map[string]any{
		row(float64(now.Unix()), "m", "intl", "acct-a", 100, 10, map[string]any{"key": "k1"}),
		row(float64(now.Unix()), "m", "intl", "", 10, 0, map[string]any{"key": "k1", "error": true, "outcome": "failed"}),
		row(float64(now.AddDate(0, 0, -1).Unix()), "m", "intl", "acct-a", 200, 20, map[string]any{"key": "k1"}),
		row(float64(now.Unix()), "m", "intl", "acct-b", 50, 5, map[string]any{"key": "k2"}),
	}
	env := newTestEnv(t, rows)
	env.Now = now
	service := env.service()
	got, err := service.KeyTokensToday(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if got != 120 {
		t.Fatalf("k1 tokens = %d, want 120 (today only, including failed rows)", got)
	}
	got, err = service.KeyTokensToday(context.Background(), "k2")
	if err != nil {
		t.Fatal(err)
	}
	if got != 55 {
		t.Fatalf("k2 tokens = %d, want 55", got)
	}
	if got, err = service.KeyTokensToday(context.Background(), "missing"); err != nil || got != 0 {
		t.Fatalf("missing key = %d, %v", got, err)
	}
}
