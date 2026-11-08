package accountstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHealthy(t *testing.T) {
	now := time.Unix(1000, 0)
	if !(State{}).Healthy(now) {
		t.Fatal("default should be healthy")
	}
	if (State{Disabled: true}).Healthy(now) {
		t.Fatal("disabled")
	}
	if (State{CoolUntil: now.Add(time.Second)}).Healthy(now) {
		t.Fatal("cooling")
	}
}

func TestHealthyAtCooldownBoundary(t *testing.T) {
	now := time.Unix(1000, 0)
	if !(State{CoolUntil: now}).Healthy(now) {
		t.Fatal("cooldown ending exactly now should be healthy")
	}
	if !(State{CoolUntil: now.Add(-time.Second)}).Healthy(now) {
		t.Fatal("expired cooldown should be healthy")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)
	states := map[string]State{
		"uid-b": {
			UID:              "uid-b",
			Disabled:         true,
			ManualDisabled:   true,
			Reason:           "manual disable",
			CoolUntil:        now.Add(time.Hour),
			LastUsed:         now.Add(-time.Minute),
			Note:             "keep me",
			TasksEnabled:     true,
			ActiveConcurrent: 1,
			MaxConcurrent:    2,
		},
		"uid-a": {UID: "uid-a"},
	}

	if err := SaveStore(path, states); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if len(got) != len(states) {
		t.Fatalf("LoadStore returned %d states, want %d", len(got), len(states))
	}

	b := got["uid-b"]
	if b.UID != "uid-b" {
		t.Fatalf("UID = %q, want %q", b.UID, "uid-b")
	}
	if !b.ManualDisabled {
		t.Fatal("ManualDisabled was not persisted")
	}
	if b.Note != "keep me" {
		t.Fatalf("Note = %q, want %q", b.Note, "keep me")
	}
	if !b.Disabled || b.Reason != "manual disable" {
		t.Fatalf("disabled state = %+v", b)
	}
	if !b.CoolUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("CoolUntil = %v, want %v", b.CoolUntil, now.Add(time.Hour))
	}
	if !b.LastUsed.Equal(now.Add(-time.Minute)) {
		t.Fatalf("LastUsed = %v, want %v", b.LastUsed, now.Add(-time.Minute))
	}
	if !b.TasksEnabled || b.ActiveConcurrent != 1 || b.MaxConcurrent != 2 {
		t.Fatalf("other state fields changed: %+v", b)
	}
	if got["uid-a"].UID != "uid-a" {
		t.Fatalf("UID for uid-a = %q", got["uid-a"].UID)
	}
}

func TestSaveStoreDeterministicSortedByUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	states := map[string]State{
		"zeta":  {UID: "zeta", Note: "z"},
		"alpha": {UID: "alpha", Note: "a"},
		"mike":  {UID: "mike", Note: "m"},
	}

	var first []byte
	for i := 0; i < 10; i++ {
		if err := SaveStore(path, states); err != nil {
			t.Fatalf("SaveStore: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if i == 0 {
			first = data
			continue
		}
		if !bytes.Equal(data, first) {
			t.Fatalf("save %d differs:\n%s\nwant:\n%s", i, data, first)
		}
	}

	raw := string(first)
	alpha := strings.Index(raw, `"alpha"`)
	mike := strings.Index(raw, `"mike"`)
	zeta := strings.Index(raw, `"zeta"`)
	if alpha < 0 || mike < 0 || zeta < 0 || !(alpha < mike && mike < zeta) {
		t.Fatalf("UID keys are not sorted: %s", raw)
	}
}

func TestStoreTimestampsUseRFC3339(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ts := time.Date(2026, 11, 8, 1, 2, 3, 0, time.FixedZone("CST", 8*60*60))
	states := map[string]State{"uid": {UID: "uid", CoolUntil: ts, LastUsed: ts.Add(time.Hour)}}
	if err := SaveStore(path, states); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("state file is not JSON: %v", err)
	}
	if got := raw["uid"]["cool_until"]; got != "2026-11-08T01:02:03+08:00" {
		t.Fatalf("cool_until = %#v, want RFC3339 timestamp", got)
	}
	if got := raw["uid"]["last_used"]; got != "2026-11-08T02:02:03+08:00" {
		t.Fatalf("last_used = %#v, want RFC3339 timestamp", got)
	}

	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if !got["uid"].CoolUntil.Equal(ts) {
		t.Fatalf("CoolUntil = %v, want %v", got["uid"].CoolUntil, ts)
	}
}

func TestLoadStoreMissingFileReturnsEmptyMap(t *testing.T) {
	got, err := LoadStore(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadStore error = %v, want nil", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("LoadStore = %#v, want empty map", got)
	}
}

func TestLoadStoreInvalidFileReturnsEmptyMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, body := range []string{
		`{`,
		`[]`,
		`null`,
		`"text"`,
		`42`,
		`{"uid":`,
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		got, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore(%q) error = %v, want nil", body, err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("LoadStore(%q) = %#v, want empty map", body, got)
		}
	}
}

func TestLoadStoreSkipsInvalidEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"good": {"manual_disabled": true, "note": "ok", "cool_until": "2026-11-08T01:02:03Z"},
		"bad-type": {"active_concurrent": "many"},
		"bad-time": {"cool_until": 12345},
		"scalar": 5,
		"null": null,
		"": {"note": "empty uid"}
	}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadStore = %#v, want only the good entry", got)
	}
	good, ok := got["good"]
	if !ok {
		t.Fatalf("good entry missing: %#v", got)
	}
	if !good.ManualDisabled || good.Note != "ok" || good.UID != "good" {
		t.Fatalf("good entry = %+v", good)
	}
	if !good.CoolUntil.Equal(time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("good CoolUntil = %v", good.CoolUntil)
	}
}

func TestSaveStorePrivateFileAndCleanDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := SaveStore(path, map[string]State{"uid": {UID: "uid"}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("mode = %o, want 0600", perm)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("leftover files: %v", entryNames(entries))
	}
}

func TestSaveStoreCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts", "raccoon", "state.json")
	if err := SaveStore(path, map[string]State{"uid": {UID: "uid"}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if len(got) != 1 || got["uid"].UID != "uid" {
		t.Fatalf("LoadStore = %#v", got)
	}
}

func TestSaveStoreEmptyMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveStore(path, nil); err != nil {
		t.Fatalf("SaveStore(nil): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]State
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("state file is not a JSON object: %v (%s)", err, data)
	}
	if raw == nil || len(raw) != 0 {
		t.Fatalf("state file = %#v, want empty object", raw)
	}
	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LoadStore = %#v, want empty map", got)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
