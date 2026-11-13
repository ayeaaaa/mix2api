package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCompleteBackupsAcceptsPrereleaseAndDropsUnknown(t *testing.T) {
	items := []Backup{
		{Version: "2.0.0-rc.1", Complete: true, CreatedAt: "2026-11-09 00:00:00"},
		{Version: "0.0.0", Complete: true, CreatedAt: "2026-11-10 00:00:00"},
		{Version: "2.0.0-rc.2", Complete: false},
		{Version: "bad", Complete: true},
	}
	got := CompleteBackups(items)
	if len(got) != 1 || got[0].Version != "2.0.0-rc.1" {
		t.Fatalf("CompleteBackups = %#v", got)
	}
}

func TestReadBackupsFiltersUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	payload := map[string]any{"backups": []any{
		map[string]any{"version": "2.0.0-rc.1", "complete": true, "files": 2, "size_mb": 10},
		map[string]any{"version": "0.0.0", "complete": true, "files": 1, "size_mb": 1},
	}}
	raw, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(dir, BackupsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	items, ok := (&Executor{Dir: dir, AllowOwner: func(uint32) bool { return true }}).ReadBackups()
	if !ok || len(items) != 1 || items[0].Version != "2.0.0-rc.1" {
		t.Fatalf("ReadBackups = %#v ok=%v", items, ok)
	}
}
