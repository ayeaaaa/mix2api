package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupsPruneAndStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := backupFile(dir, "codex", path, []byte("original"), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal(err)
	}
	// Newest 10 per basename survive.
	for i := 0; i < 12; i++ {
		if _, err := backupFile(dir, "codex", path, []byte("old"), now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(backup))
	if len(entries) != BackupKeep {
		t.Fatalf("backup count = %d, want %d", len(entries), BackupKeep)
	}

	state := loadState(dir)
	state.Clients["codex"] = Record{AppliedAt: "2026-01-02 03:04:05", Files: []FileRecord{{Path: path, Existed: true, Backup: backup, SHA256: hashBytes([]byte("new"))}}}
	if err := saveState(dir, state); err != nil {
		t.Fatal(err)
	}
	loaded := loadState(dir)
	if loaded.Clients["codex"].Files[0].Backup != backup {
		t.Fatalf("state round-trip = %#v", loaded)
	}
}

func TestStatePreservesUnknownKeysAndV1Shape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFile)
	if err := os.WriteFile(path, []byte(`{
  "claude-code": {"applied_at":"2026-01-01 00:00:00","base_url":"http://x","files":[]},
  "future_metadata": {"keep": true},
  "version": 7
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := loadState(dir)
	state.Clients["codex"] = Record{AppliedAt: "2026-01-02 00:00:00", BaseURL: "http://x"}
	if err := saveState(dir, state); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{`"future_metadata"`, `"keep": true`, `"version": 7`, `"claude-code"`, `"codex"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("state missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"clients"`) {
		t.Fatalf("state used non-v1 wrapper:\n%s", raw)
	}
}
