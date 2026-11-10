package contract

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/app"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
)

func copyPlatformFixture(t *testing.T, name, dest string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "platform", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
}

func TestGateDDataRoundTrip(t *testing.T) {
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	usageDir := filepath.Join(root, "usage")
	mediaDir := filepath.Join(root, "media")
	updateDir := filepath.Join(root, "updates")
	for _, dir := range []string{accounts, usageDir, mediaDir, updateDir, filepath.Join(accounts, "agent-backups", "claude-code")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyPlatformFixture(t, "settings-v1.json", filepath.Join(accounts, "settings.json"))
	copyPlatformFixture(t, "usage-v1.jsonl", filepath.Join(usageDir, "usage.jsonl"))
	copyPlatformFixture(t, "integration-state-v1.json", filepath.Join(accounts, "integration-state.json"))
	copyPlatformFixture(t, "tasks-v1.json", filepath.Join(mediaDir, "tasks.json"))
	copyPlatformFixture(t, "status-v1.json", filepath.Join(updateDir, "status.json"))
	copyPlatformFixture(t, "backups-v1.json", filepath.Join(updateDir, "backups.json"))
	stateFixture := filepath.Join(accounts, "integration-state.json")
	stateBefore, err := os.ReadFile(stateFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "video-prices.json"), []byte(`{"video-model|720p":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "image-prices.json"), []byte(`{"image-model":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	summary := []byte(`{"legacy":"untouched"}`)
	if err := os.WriteFile(filepath.Join(usageDir, "usage-summary.json"), summary, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB_UPDATE_INSTALL_ENABLED", "1")
	t.Setenv("WB_UPDATE_EXECUTOR", "external")

	setup, err := app.Build(app.Paths{AccountsDir: accounts, UsageDir: usageDir})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	platform, err := app.BuildPlatform(setup, app.PlatformOptions{
		Paths:         setup.Paths,
		Version:       "2.0.0-test",
		PanelPassword: "admin",
		MediaDir:      mediaDir,
		UpdateDir:     updateDir,
		Home:          root,
	})
	if err != nil {
		t.Fatalf("BuildPlatform: %v", err)
	}
	defer platform.Close()

	// Settings: unknown keys survive both an unrelated and a limit save.
	view, err := platform.Settings.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rawBefore, err := platform.Store.Load()
	if err != nil || rawBefore["unknown_v1_key"] == nil {
		t.Fatalf("settings load lost unknown key: %v err=%v", rawBefore, err)
	}
	if view["limits"] == nil {
		t.Fatalf("settings view lost the normalized limits: %v", view)
	}
	if _, err := platform.Settings.Save(context.Background(), map[string]any{"ui_language": "en"}); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Settings.Save(context.Background(), map[string]any{"daily_token_limit": 55}); err != nil {
		t.Fatal(err)
	}
	data, err := platform.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if data["unknown_v1_key"] == nil {
		t.Fatal("limit save dropped an unknown settings key")
	}
	if _, ok := data["limits"].(map[string]any); !ok {
		t.Fatalf("legacy limits were not grouped: %v", data["limits"])
	}
	info, err := os.Stat(filepath.Join(accounts, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %o, want 600", info.Mode().Perm())
	}

	// Usage: legacy rows keep unknown keys and the recorder appends v2 rows.
	rows, err := platform.UsageStore.ReadRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0]["unknown_v1_row_key"] == nil {
		t.Fatalf("legacy usage rows = %d %+v", len(rows), rows)
	}
	if err := platform.RecordUsage(context.Background(), recordInputForFixture()); err != nil {
		t.Fatal(err)
	}
	rows, err = platform.UsageStore.ReadRows()
	if err != nil || len(rows) != 4 {
		t.Fatalf("usage append = %d rows, err=%v", len(rows), err)
	}
	summaryAfter, err := os.ReadFile(filepath.Join(usageDir, "usage-summary.json"))
	if err != nil || !bytes.Equal(summary, summaryAfter) {
		t.Fatalf("usage-summary.json changed: %q err=%v", summaryAfter, err)
	}
	usageInfo, err := os.Stat(filepath.Join(usageDir, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if usageInfo.Mode().Perm()&0o777 != 0o644 && usageInfo.Mode().Perm()&0o777 != 0o600 {
		t.Fatalf("usage mode = %o", usageInfo.Mode().Perm())
	}

	// Logs are memory-only until the panel enables the database; then the v1
	// file name appears under the usage directory.
	if _, err := os.Stat(filepath.Join(usageDir, "logs.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("disabled log mode created a file: %v", err)
	}
	if err := platform.LogStore.Configure(true, 0, 7, 0); err != nil {
		t.Fatal(err)
	}
	if err := platform.LogStore.Append(contractLogEntry()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(usageDir, "logs.sqlite3")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("logs.sqlite3 was not created")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Media, agents, and update state read the v1-shaped synthetic files.
	tasks, _ := platform.Media.TasksList(10)
	if len(tasks) != 1 || tasks[0].ID != "task-1" {
		t.Fatalf("media tasks = %+v", tasks)
	}
	overview := platform.Agents.Overview(context.Background())
	if entry, _ := overview["claude-code"].(map[string]any); entry == nil || entry["configured"] != true {
		t.Fatalf("agent state not read: %+v", overview)
	}
	stateAfter, err := os.ReadFile(stateFixture)
	if err != nil || !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("agent state changed by a read path: err=%v", err)
	}
	if err := platform.Agents.SetSelection([]string{"hy4-preview"}); err != nil {
		t.Fatal(err)
	}
	if selection, configured := platform.Agents.Selection(); !configured || len(selection) != 1 || selection[0] != "hy4-preview" {
		t.Fatalf("agent selection roundtrip = %v configured=%t", selection, configured)
	}
	settingsAfterAgent, err := platform.Store.Load()
	if err != nil || settingsAfterAgent["unknown_v1_key"] == nil {
		t.Fatalf("agent selection dropped unknown settings: %+v err=%v", settingsAfterAgent, err)
	}

	if platform.Pricing == nil {
		t.Fatal("pricing snapshot missing")
	}
	cost := platform.Pricing.CostForRow(map[string]any{
		"model": "deepseek-v4.1-flash-sg", "prompt_tokens": 1000, "completion_tokens": 100,
	})
	if !cost.Known || cost.CNY <= 0 {
		t.Fatalf("embedded pricing roundtrip = %+v", cost)
	}
	if price := platform.Media.Pricing.ImagePrice("image-model"); price == nil || *price != 2 {
		t.Fatalf("media image price = %v", price)
	}
	if price := platform.Media.Pricing.PricePerSecond("video-model", "720p", true, false); price == nil || *price != 3 {
		t.Fatalf("media video price = %v", price)
	}
	executor := update.NewExecutor(updateDir)
	status, ok := executor.ReadStatus()
	if !ok || status.State != "idle" {
		t.Fatalf("update status = %+v ok=%t", status, ok)
	}
	backups, ok := executor.ReadBackups()
	if !ok || len(backups) != 1 || backups[0].Version != "1.6.4" {
		t.Fatalf("update backups = %+v ok=%t", backups, ok)
	}
}

func recordInputForFixture() usage.RecordInput {
	return usage.RecordInput{
		Model: "hy4-preview", Realm: "intl", Account: "uid-a", KeyID: "k1",
		Outcome: "completed", Stream: false,
		Usage: map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	}
}

func contractLogEntry() logs.Entry {
	return logs.Entry{Level: "info", Tag: "contract", Msg: "v1 log fixture"}
}

func TestUsageRecordsCrossProvider(t *testing.T) {
	dir := t.TempDir()
	paths, err := usage.ResolvePaths("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	store := usage.New(paths)
	recorder := &usage.Recorder{Store: store}
	cases := []usage.RecordInput{
		{Model: "same-model", Realm: "codearts", Account: "u1", Kind: "chat", Usage: map[string]any{"total_tokens": 3, "credit": 99}},
		{Model: "same-model", Realm: "raccoon", Account: "u2", Kind: "chat", Usage: map[string]any{"total_tokens": 4, "credit": 99}},
		{Model: "image-model", Realm: "intl", Account: "u3", Kind: "image", Usage: map[string]any{"total_tokens": 5, "credit": 1}},
	}
	for _, input := range cases {
		if err := recorder.Record(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.ReadRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0]["cost_policy"] != "codearts:unpriced" {
		t.Fatalf("codearts policy = %v", rows[0]["cost_policy"])
	}
	if rows[1]["cost_policy"] != "raccoon:unpriced" {
		t.Fatalf("raccoon policy = %v", rows[1]["cost_policy"])
	}
	if rows[2]["cost_policy"] != "media" || rows[2]["kind"] != "image" {
		t.Fatalf("media row = %v", rows[2])
	}
	if fmt.Sprint(rows[0]["credit"]) != "99" || rows[0]["realm"] != "codearts" {
		t.Fatalf("provider credit/realm was rewritten: %v", rows[0])
	}
}

func TestGateDFixtureFilesAreSynthetic(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "platform", "usage-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "access_token") || strings.Contains(string(raw), "refresh_token") {
		t.Fatal("fixture contains credential material")
	}
}
