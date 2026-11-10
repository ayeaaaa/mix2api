package probes

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowProbeState(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000, 0)
	state := NewWindowState(dir)
	state.Now = func() time.Time { return now }
	if !state.Schedule("u1", "m1", now.Add(60*time.Second), false, now, now) {
		t.Fatal("schedule failed")
	}
	if state.Schedule("u1", "m1", now.Add(60*time.Second), false, now, now) {
		t.Fatal("duplicate window was rewritten")
	}
	if !state.Schedule("u1", "m1", now.Add(60*time.Second), true, now, now) {
		t.Fatal("cap marker upgrade failed")
	}
	if !state.Schedule("u2", "m2", now.Add(30*time.Second), false, now, now) {
		t.Fatal("second schedule failed")
	}
	snapshot := state.Snapshot(now)
	if len(snapshot) != 2 || snapshot[0]["uid8"] != "u2" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if window, ok := state.Get("u1", "m1"); !ok || !window.Cap {
		t.Fatalf("window = %#v ok=%v", window, ok)
	}
	if due := state.Due(now, 8); len(due) != 0 {
		t.Fatalf("future windows due = %#v", due)
	}
	if due := state.Due(now.Add(61*time.Second), 8); len(due) != 2 {
		t.Fatalf("due windows = %#v", due)
	}
	if !state.Drop("u1", "m1") || state.HasWindow("u1", "m1") {
		t.Fatal("drop failed")
	}
	state.Update(Window{UID: "u2", Model: "m2"}, now.Add(120*time.Second), 2, "retry", now)
	if window, _ := state.Get("u2", "m2"); window.Attempts != 2 || window.LastResult != "retry" {
		t.Fatalf("updated window = %#v", window)
	}
	raw, err := os.ReadFile(filepath.Join(dir, WindowStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("state file empty")
	}

	// Max-age pruning ignores stale persisted records but keeps recent ones.
	stale := now.Add(-WindowMaxAge - time.Hour).Unix()
	if err := os.WriteFile(filepath.Join(dir, WindowStateFile), []byte(`{"version":1,"windows":[{"uid":"old","model":"m","reset_at":`+itoa64(stale)+`}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := NewWindowState(dir)
	reloaded.Now = func() time.Time { return now }
	if _, ok := reloaded.Get("old", "m"); ok {
		t.Fatal("stale window survived prune")
	}
}

func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := ""
	for value > 0 {
		digits = string(byte('0'+value%10)) + digits
		value /= 10
	}
	if negative {
		return "-" + digits
	}
	return digits
}
