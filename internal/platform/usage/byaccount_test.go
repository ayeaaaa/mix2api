package usage

import (
	"context"
	"testing"
	"time"
)

func TestByAccountTotals(t *testing.T) {
	now := float64(time.Now().Unix())
	rows := make([]map[string]any, 0)
	for index := 0; index < 7; index++ {
		model := "m" + string(rune('a'+index))
		rows = append(rows, row(now+float64(index), model, "intl", "a", 10, 1, nil))
	}
	rows = append(rows, row(now+10, "ma", "intl", "a", 10, 1, nil))
	rows = append(rows, row(now+11, "m", "intl", "b", 100, 10, nil))
	rows = append(rows, row(now+12, "m", "intl", "a", 0, 0, map[string]any{"error": true, "outcome": "failed"}))
	env := newTestEnv(t, rows)
	payload, err := env.service().ByAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accounts := payload["accounts"].([]map[string]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0]["account"] != "b" {
		t.Fatalf("accounts should sort by total tokens: %+v", accounts)
	}
	var accountA map[string]any
	for _, entry := range accounts {
		if entry["account"] == "a" {
			accountA = entry
		}
	}
	if accountA == nil {
		t.Fatal("account a missing")
	}
	if mustNumber(t, accountA["requests"]) != 8 {
		t.Fatalf("error row should be skipped: %+v", accountA)
	}
	models := accountA["models"].([][]any)
	if len(models) != 5 {
		t.Fatalf("models should be top 5: %+v", models)
	}
	if models[0][0] != "ma" || mustNumber(t, models[0][1]) != 2 {
		t.Fatalf("model sort = %+v", models)
	}
}
