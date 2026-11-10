package agents

import (
	"os"
	"path/filepath"
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
