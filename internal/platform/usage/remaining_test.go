package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestRemainingUsage(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	nowEpoch := float64(now.Unix())
	rows := []map[string]any{
		row(nowEpoch-3*3600, "m", "intl", "a", 60, 40, nil),
		row(nowEpoch-2*3600, "m", "intl", "a", 60, 40, nil),
		row(nowEpoch-3600, "m", "intl", "a", 60, 40, nil),
		row(nowEpoch-3*3600, "m", "intl", "b", 30, 20, nil),
		row(nowEpoch-3600, "m", "intl", "b", 30, 20, nil),
		row(nowEpoch-3600, "x", "intl", "c", 6, 4, nil),
	}
	env := newTestEnv(t, rows)
	zone := time.FixedZone("UTC+8", 8*3600)
	resetText := time.Unix(int64(nowEpoch+3600), 0).In(zone).Format("2006-01-02 15:04:05")
	recorder := &Recorder{Store: env.Store, Now: func() time.Time {
		return time.Unix(int64(nowEpoch-4*3600), 0)
	}}
	if err := recorder.Record(context.Background(), RecordInput{
		Model: "m", Realm: "intl", Account: "b", KeyID: "k",
		Outcome: "failed", Error: true, Status: 429,
		Message: fmt.Sprintf("usage will reset at %s UTC+8", resetText),
	}); err != nil {
		t.Fatal(err)
	}
	events := []map[string]any{
		{"at": nowEpoch - 4*3600, "reset": nowEpoch - 2*3600, "uid": "a", "model": "m", "realm": "intl", "sample": 1000},
		{"at": nowEpoch - 4*3600, "reset": nowEpoch + 3600, "uid": "b", "model": "m", "realm": "intl", "sample": 500},
	}
	for _, event := range events {
		data, _ := json.Marshal(event)
		file, err := os.OpenFile(env.Paths.LimitEvents, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write(append(data, '\n'))
		_ = file.Close()
	}
	yesterday := localDayKey(nowEpoch - 86400)
	daily := map[string]any{
		"schema": 1,
		"days": map[string]any{
			yesterday: map[string]any{
				"budgets": map[string]any{
					"intl\x1fm": map[string]any{"avg": 600, "min": 600, "max": 600, "n": 1},
				},
			},
		},
	}
	data, _ := json.Marshal(daily)
	if err := os.WriteFile(env.Paths.EstimateDaily, data, 0o644); err != nil {
		t.Fatal(err)
	}

	service := env.service()
	service.Now = func() time.Time { return now }
	service.RemainingAccounts = []RemainingAccount{
		{UID: "a", Nickname: "A", Realm: "intl", Enabled: true},
		{UID: "b", Nickname: "B", Realm: "intl", Enabled: true},
		{UID: "c", Nickname: "C", Realm: "intl", Enabled: true},
	}
	payload, err := service.Remaining(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, payload["window_seconds"]) != 86400 {
		t.Fatalf("window = %+v", payload["window_seconds"])
	}
	budgets := payload["budgets"].([]map[string]any)
	if len(budgets) != 1 {
		t.Fatalf("events not deduped: %+v", budgets)
	}
	budget := budgets[0]
	if mustNumber(t, budget["avg"]) != 700 || mustNumber(t, budget["min"]) != 500 ||
		mustNumber(t, budget["max"]) != 1000 || mustNumber(t, budget["n"]) != 3 {
		t.Fatalf("weighted budget = %+v", budget)
	}
	rowsOut := payload["rows"].([]map[string]any)
	byPair := map[string]map[string]any{}
	for _, entry := range rowsOut {
		byPair[entry["uid"].(string)+"/"+entry["model"].(string)] = entry
	}
	a := byPair["a/m"]
	if a == nil {
		t.Fatalf("a/m missing: %+v", rowsOut)
	}
	if mustNumber(t, a["used"]) != 200 {
		t.Fatalf("a used = %+v", a)
	}
	if mustNumber(t, a["remaining"]) != 500 {
		t.Fatalf("a remaining = %+v", a)
	}
	segments := a["segments"].([]map[string]any)
	if len(segments) != 2 || segments[0]["capped"] != true || segments[1]["current"] != true {
		t.Fatalf("a segments = %+v", segments)
	}
	b := byPair["b/m"]
	if b == nil || b["cooling"] != true || b["remaining"] != int64(0) {
		t.Fatalf("cooling pair should have remaining 0: %+v", b)
	}
	c := byPair["c/x"]
	if c == nil || c["remaining"] != nil || c["budget"] != nil || c["estimated"] != true {
		t.Fatalf("unknown budget should stay nil: %+v", c)
	}
	models := payload["models"].([]map[string]any)
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	var modelM, modelX map[string]any
	for _, entry := range models {
		if entry["model"] == "m" {
			modelM = entry
		}
		if entry["model"] == "x" {
			modelX = entry
		}
	}
	if modelM == nil || mustNumber(t, modelM["total_remaining"]) != 1200 || mustNumber(t, modelM["unknown"]) != 0 {
		t.Fatalf("model m totals = %+v", modelM)
	}
	if modelX == nil || modelX["total_remaining"] != nil || mustNumber(t, modelX["unknown"]) != 1 {
		t.Fatalf("model x totals = %+v", modelX)
	}
	if rowsOut[0]["remaining"] != int64(0) {
		t.Fatalf("rows should sort zero-remaining first: %+v", rowsOut)
	}
}

func TestRemainingCapOnlyPairAndArchiveRealm(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	env := newTestEnv(t, nil)
	service := env.service()
	service.Now = func() time.Time { return now }
	service.RemainingAccounts = []RemainingAccount{{
		UID: "a", Nickname: "A", Realm: "intl", Enabled: true,
		ModelCaps: map[string]CapAnchor{
			"m": {At: float64(now.Unix()) - 3600, Reset: float64(now.Unix()) + 3600},
		},
	}}
	payload, err := service.Remaining(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows := payload["rows"].([]map[string]any)
	if len(rows) != 1 || rows[0]["model"] != "m" || rows[0]["cooling"] != true {
		t.Fatalf("cap-only pair missing: %+v", rows)
	}
	if rows[0]["remaining"] != int64(0) {
		t.Fatalf("cap-only pair remaining = %v", rows[0]["remaining"])
	}

	// An archived event without a realm field inherits the account's realm.
	env2 := newTestEnv(t, nil)
	yesterday := now.Add(-24 * time.Hour)
	raw := map[string]any{
		"at": float64(yesterday.Unix()), "reset": float64(yesterday.Unix() + 3600),
		"uid": "a", "model": "m", "sample": 1234,
	}
	data, _ := json.Marshal(raw)
	if err := os.WriteFile(env2.Paths.LimitEvents, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	service2 := env2.service()
	service2.Now = func() time.Time { return now }
	service2.RemainingAccounts = []RemainingAccount{{UID: "a", Nickname: "A", Realm: "intl", Enabled: true}}
	if _, err := service2.Remaining(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(env2.Paths.EstimateDaily)
	if err != nil {
		t.Fatalf("estimate-daily not written: %v", err)
	}
	var document struct {
		Days map[string]map[string]any `json:"days"`
	}
	if err := json.Unmarshal(stored, &document); err != nil {
		t.Fatal(err)
	}
	day := localDayKey(float64(yesterday.Unix()))
	if document.Days[day] == nil {
		t.Fatalf("yesterday not archived: %s", stored)
	}
	budgets, _ := document.Days[day]["budgets"].(map[string]any)
	if budgets["intl\x1fm"] == nil {
		t.Fatalf("archive did not apply account realm: %+v", budgets)
	}
}
