package accountstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	if !b.TasksEnabled || b.MaxConcurrent != 2 {
		t.Fatalf("other state fields changed: %+v", b)
	}
	if b.ActiveConcurrent != 0 {
		t.Fatalf("ActiveConcurrent = %d, want reset to 0 on load", b.ActiveConcurrent)
	}
	if got["uid-a"].UID != "uid-a" {
		t.Fatalf("UID for uid-a = %q", got["uid-a"].UID)
	}
}

func TestSaveStoreDeterministicSortedByUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	states := map[string]State{
		"zeta": {UID: "zeta", Note: "z", Extra: map[string]json.RawMessage{
			"zzz": json.RawMessage(`{"n":1}`),
			"aaa": json.RawMessage(`[1,2]`),
		}},
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
	aaa := strings.Index(raw, `"aaa"`)
	zzz := strings.Index(raw, `"zzz"`)
	if aaa < 0 || zzz < 0 || aaa > zzz {
		t.Fatalf("extra keys are not sorted: %s", raw)
	}
}

func TestStoreTimestampsUseUnixSeconds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ts := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)
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
	if got := raw["uid"]["cool_until"]; got != float64(ts.Unix()) {
		t.Fatalf("cool_until = %#v, want Unix seconds %d", got, ts.Unix())
	}
	if got := raw["uid"]["last_used"]; got != float64(ts.Add(time.Hour).Unix()) {
		t.Fatalf("last_used = %#v, want Unix seconds %d", got, ts.Add(time.Hour).Unix())
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

func TestLoadStoreToleratesInvalidEntriesAndNonObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"good": {"manual_disabled": true, "note": "ok", "cool_until": "2026-11-08T01:02:03Z"},
		"bad-type": {"active_concurrent": "many", "manual_disabled": true, "note": "survive"},
		"bad-time": {"cool_until": "not-a-time", "manual_disabled": true},
		"legacy": {"cool_until": 12345, "manual_disabled": true},
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
	if len(got) != 4 {
		t.Fatalf("LoadStore = %#v, want good/bad-type/bad-time/legacy", got)
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
	badType := got["bad-type"]
	if !badType.ManualDisabled || badType.Note != "survive" || badType.ActiveConcurrent != 0 {
		t.Fatalf("entry with a bad known field lost good data: %+v", badType)
	}
	badTime := got["bad-time"]
	if !badTime.ManualDisabled || !badTime.CoolUntil.IsZero() {
		t.Fatalf("entry with a bad timestamp = %+v", badTime)
	}
	legacy := got["legacy"]
	if !legacy.ManualDisabled || !legacy.CoolUntil.Equal(time.Unix(12345, 0)) {
		t.Fatalf("numeric timestamp entry = %+v", legacy)
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

func TestLegacyV1FixtureRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"pu-v1": {
			"disabled": true,
			"manual_disabled": true,
			"disabled_by": "manual",
			"reason": "manual disable",
			"cool_until": 1800000000.75,
			"err_count": 4,
			"last_error": "upstream 500",
			"note": "operator note",
			"tasks_enabled": true,
			"last_used": 1760000000,
			"max_concurrent": 3,
			"active_concurrent": 2,
			"future_key": {"nested": [1, 2, 3]},
			"future_string": "keep me"
		},
		"pu-plain": {"note": "second account"}
	}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	wantCool := time.Unix(1800000000, 750000000).UTC()
	wantLastUsed := time.Unix(1760000000, 0).UTC()

	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadStore returned %d entries, want 2: %#v", len(got), got)
	}
	state := got["pu-v1"]
	if state.UID != "pu-v1" {
		t.Fatalf("UID = %q, want pu-v1", state.UID)
	}
	if !state.Disabled || !state.ManualDisabled || state.DisabledBy != "manual" {
		t.Fatalf("disable fields = %+v", state)
	}
	if state.Reason != "manual disable" || state.ErrCount != 4 || state.LastError != "upstream 500" {
		t.Fatalf("error fields = %+v", state)
	}
	if state.Note != "operator note" || !state.TasksEnabled || state.MaxConcurrent != 3 {
		t.Fatalf("account fields = %+v", state)
	}
	if state.ActiveConcurrent != 0 {
		t.Fatalf("ActiveConcurrent = %d, want reset to 0", state.ActiveConcurrent)
	}
	if !state.CoolUntil.Equal(wantCool) {
		t.Fatalf("CoolUntil = %v, want %v", state.CoolUntil, wantCool)
	}
	if !state.LastUsed.Equal(wantLastUsed) {
		t.Fatalf("LastUsed = %v, want %v", state.LastUsed, wantLastUsed)
	}
	if len(state.Extra) != 2 {
		t.Fatalf("Extra = %#v, want two unknown keys", state.Extra)
	}
	assertJSONValue(t, state.Extra["future_key"], `{"nested":[1,2,3]}`)
	assertJSONValue(t, state.Extra["future_string"], `"keep me"`)

	if err := SaveStore(path, got); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var saved map[string]map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("saved state is not JSON: %v\n%s", err, data)
	}
	if got := saved["pu-v1"]["cool_until"]; got != unixSecondsValue(wantCool) {
		t.Fatalf("saved cool_until = %#v, want Unix seconds %v", got, unixSecondsValue(wantCool))
	}
	if got := saved["pu-v1"]["last_used"]; got != unixSecondsValue(wantLastUsed) {
		t.Fatalf("saved last_used = %#v, want Unix seconds %v", got, unixSecondsValue(wantLastUsed))
	}
	if got := saved["pu-v1"]["active_concurrent"]; got != float64(0) {
		t.Fatalf("saved active_concurrent = %#v, want 0", got)
	}
	if _, ok := saved["pu-v1"]["future_key"]; !ok {
		t.Fatalf("future_key was dropped on save: %s", data)
	}
	if _, ok := saved["pu-v1"]["future_string"]; !ok {
		t.Fatalf("future_string was dropped on save: %s", data)
	}

	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore after save: %v", err)
	}
	again := reloaded["pu-v1"]
	if !again.ManualDisabled || again.DisabledBy != "manual" || again.ErrCount != 4 ||
		again.LastError != "upstream 500" || again.Note != "operator note" || !again.TasksEnabled {
		t.Fatalf("state changed across save/load: %+v", again)
	}
	if !again.CoolUntil.Equal(wantCool) || !again.LastUsed.Equal(wantLastUsed) {
		t.Fatalf("timestamps changed across save/load: %+v", again)
	}
	assertJSONValue(t, again.Extra["future_key"], `{"nested":[1,2,3]}`)
	assertJSONValue(t, again.Extra["future_string"], `"keep me"`)
}

func TestStateUIDKeyIsAuthoritative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"by-key": {"uid": "stale", "note": "keep"}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if got["by-key"].UID != "by-key" {
		t.Fatalf("UID = %q, want by-key", got["by-key"].UID)
	}
	if _, ok := got["by-key"].Extra["uid"]; ok {
		t.Fatalf("entry uid leaked into Extra: %#v", got["by-key"].Extra)
	}

	if err := SaveStore(path, map[string]State{"by-key": {UID: "other", Note: "keep"}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "other") || strings.Contains(string(data), `"uid"`) {
		t.Fatalf("save used State.UID instead of the map key: %s", data)
	}
}

func TestLoadStoreLegacyNumericTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"pu-legacy": {
			"disabled": true,
			"manual_disabled": true,
			"reason": "manual disable",
			"cool_until": 1800000000,
			"last_used": 1760000000.25,
			"note": "keep",
			"tasks_enabled": true
		},
		"pu-zero": {"cool_until": 0, "last_used": 0.0, "note": "never used"}
	}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore error = %v, want nil", err)
	}
	state, ok := got["pu-legacy"]
	if !ok {
		t.Fatalf("legacy entry was dropped: %#v", got)
	}
	if !state.Disabled || !state.ManualDisabled || state.Reason != "manual disable" {
		t.Fatalf("legacy flags were not preserved: %+v", state)
	}
	if state.Note != "keep" || !state.TasksEnabled {
		t.Fatalf("legacy fields were not preserved: %+v", state)
	}
	if !state.CoolUntil.Equal(time.Unix(1800000000, 0)) {
		t.Fatalf("CoolUntil = %v, want Unix 1800000000", state.CoolUntil)
	}
	if !state.LastUsed.Equal(time.Unix(1760000000, 250000000)) {
		t.Fatalf("LastUsed = %v, want Unix 1760000000.25", state.LastUsed)
	}
	zero := got["pu-zero"]
	if !zero.CoolUntil.IsZero() || !zero.LastUsed.IsZero() {
		t.Fatalf("legacy zero timestamps did not become zero time: %+v", zero)
	}
}

func TestLoadStoreResetsActiveConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"uid": {"active_concurrent": 7, "max_concurrent": 3, "last_used": "2026-11-08T01:02:03Z"}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore error = %v, want nil", err)
	}
	state, ok := got["uid"]
	if !ok {
		t.Fatalf("entry was dropped: %#v", got)
	}
	if state.ActiveConcurrent != 0 {
		t.Fatalf("ActiveConcurrent = %d, want 0 after load", state.ActiveConcurrent)
	}
	if state.MaxConcurrent != 3 {
		t.Fatalf("MaxConcurrent = %d, want 3", state.MaxConcurrent)
	}
}

func TestSaveStoreSkipsEmptyUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	states := map[string]State{
		"":   {UID: "", Note: "drop me"},
		"ok": {UID: "ok", Note: "keep me"},
	}
	if err := SaveStore(path, states); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "drop me") {
		t.Fatalf("empty UID entry was written: %s", data)
	}
	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if len(got) != 1 || got["ok"].Note != "keep me" {
		t.Fatalf("LoadStore = %#v, want only the ok entry", got)
	}
}

func TestMalformedSafetyFieldsFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"string-disabled": {"disabled": "false", "note": "truthy in v1"},
		"number-disabled": {"disabled": 0},
		"string-manual": {"manual_disabled": "false"},
		"null-disabled": {"disabled": null}
	}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	now := time.Now()
	for uid, state := range got {
		if state.Healthy(now) {
			t.Fatalf("%s with malformed safety field loaded healthy: %+v", uid, state)
		}
		if !state.Disabled {
			t.Fatalf("%s malformed safety field did not set Disabled: %+v", uid, state)
		}
	}
	if !got["string-manual"].ManualDisabled {
		t.Fatalf("malformed manual_disabled did not fail closed: %+v", got["string-manual"])
	}
}

func TestOutOfRangeTimestampsClampAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"future": {"cool_until": 1e300, "last_used": 1e300},
		"past": {"cool_until": -1e300, "last_used": -1e300},
		"rfc-future": {"cool_until": "9999-12-31T23:59:59Z", "last_used": "0000-01-01T00:00:00Z"}
	}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if !got["future"].CoolUntil.Equal(maxStateTime()) {
		t.Fatalf("future CoolUntil = %v, want clamp %v", got["future"].CoolUntil, maxStateTime())
	}
	if !got["past"].CoolUntil.Equal(minStateTime()) {
		t.Fatalf("past CoolUntil = %v, want clamp %v", got["past"].CoolUntil, minStateTime())
	}
	if got["future"].Healthy(time.Now()) {
		t.Fatal("far-future cooldown was treated as healthy")
	}
	if err := SaveStore(path, got); err != nil {
		t.Fatalf("SaveStore after clamped load: %v", err)
	}

	// A directly constructed out-of-range time must not wedge SaveStore either.
	if err := SaveStore(path, map[string]State{
		"manual-future": {UID: "manual-future", CoolUntil: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		"manual-past":   {UID: "manual-past", CoolUntil: time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)},
	}); err != nil {
		t.Fatalf("SaveStore with out-of-range times: %v", err)
	}
}

// TestSaveStoreIsReadableByV1NumericParser mirrors the exact v1.6.5 load
// expression: float(entry.get("cool_until") or 0) and float(...last_used...).
func TestSaveStoreIsReadableByV1NumericParser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cool := time.Unix(1800000000, 750000000).UTC()
	lastUsed := time.Unix(1760000000, 0).UTC()
	if err := SaveStore(path, map[string]State{
		"pu-v1-reader": {UID: "pu-v1-reader", CoolUntil: cool, LastUsed: lastUsed},
		"pu-zero":      {UID: "pu-zero"},
	}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("v1 JSON load failed: %v", err)
	}
	loadFloat := func(uid, key string) float64 {
		t.Helper()
		var number float64
		if err := json.Unmarshal(raw[uid][key], &number); err != nil {
			t.Fatalf("v1 float(%q) failed for %s: %v", key, uid, err)
		}
		return number
	}
	if got := loadFloat("pu-v1-reader", "cool_until"); got != 1800000000.75 {
		t.Fatalf("v1 cool_until = %v, want 1800000000.75", got)
	}
	if got := loadFloat("pu-v1-reader", "last_used"); got != 1760000000 {
		t.Fatalf("v1 last_used = %v, want 1760000000", got)
	}
	if got := loadFloat("pu-zero", "cool_until"); got != 0 {
		t.Fatalf("v1 zero cool_until = %v, want 0", got)
	}
	if got := loadFloat("pu-zero", "last_used"); got != 0 {
		t.Fatalf("v1 zero last_used = %v, want 0", got)
	}
}

func assertJSONValue(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(raw, &gotValue); err != nil {
		t.Fatalf("unmarshal got JSON %q: %v", raw, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("unmarshal want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %#v, want %#v", gotValue, wantValue)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
