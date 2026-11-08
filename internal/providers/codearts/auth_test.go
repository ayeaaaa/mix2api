package codearts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAuthRoundTripWireFormatAndPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path, err := AuthPath(dir, "u/1")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, "codearts", "codearts-u_1.json")
	if path != wantPath {
		t.Fatalf("AuthPath() = %q, want %q", path, wantPath)
	}

	expiration := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	dpop := json.RawMessage(`{"kty":"EC","crv":"P-256","x":"abc","y":"def","d":"secret"}`)
	auth := Auth{
		UserID:           "u/1",
		UserName:         "账号一",
		DomainID:         "d1",
		CloudDragonToken: "cloud-token",
		AccessKeyID:      "AK",
		SecretAccessKey:  "SK",
		RefreshToken:     "refresh-1",
		CodeVerifier:     "verifier",
		ClientID:         "codearts-agent",
		Expiration:       expiration,
		DPopPrivateKey:   dpop,
		ProxySlot:        "slot-1",
		UpdatedAt:        1760000000,
	}
	if err := SaveAuth(path, auth); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}

	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0600 {
			t.Fatalf("auth mode = %v, want 0600", st.Mode().Perm())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("auth file is not a JSON object: %v", err)
	}
	for _, key := range []string{
		"user_id", "user_name", "domain_id", "cloud_dragon_token",
		"access_key_id", "secret_access_key", "refresh_token", "code_verifier",
		"client_id", "expiration", "dpop_private_key", "proxySlot", "updated_at",
	} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("auth file missing required key %q: %s", key, data)
		}
	}
	var gotExpiration string
	if err := json.Unmarshal(raw["expiration"], &gotExpiration); err != nil {
		t.Fatalf("expiration is not a string: %v", err)
	}
	if gotExpiration != "2030-01-02T03:04:05Z" {
		t.Fatalf("expiration = %q, want ISO-8601 2030-01-02T03:04:05Z", gotExpiration)
	}
	assertJSONValue(t, raw["dpop_private_key"], string(dpop))

	loaded, err := LoadAuth(path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if loaded.UserID != auth.UserID || loaded.UserName != auth.UserName ||
		loaded.DomainID != auth.DomainID || loaded.CloudDragonToken != auth.CloudDragonToken ||
		loaded.AccessKeyID != auth.AccessKeyID || loaded.SecretAccessKey != auth.SecretAccessKey ||
		loaded.RefreshToken != auth.RefreshToken || loaded.CodeVerifier != auth.CodeVerifier ||
		loaded.ClientID != auth.ClientID || loaded.ProxySlot != auth.ProxySlot ||
		loaded.UpdatedAt != auth.UpdatedAt {
		t.Fatalf("LoadAuth() = %#v, want %#v", loaded, auth)
	}
	if !loaded.Expiration.Equal(expiration) {
		t.Fatalf("Expiration = %v, want %v", loaded.Expiration, expiration)
	}
	assertJSONValueBytes(t, loaded.DPopPrivateKey, dpop)

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("leftover files: %v", entryNames(entries))
	}
}

func TestAuthPreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codearts", "codearts-u1.json")
	fixture := `{
		"user_id":"u1",
		"user_name":"name",
		"domain_id":"d1",
		"cloud_dragon_token":"token",
		"access_key_id":"AK",
		"secret_access_key":"SK",
		"refresh_token":"refresh",
		"code_verifier":"verifier",
		"client_id":"codearts-agent",
		"expiration":"2030-01-02T03:04:05Z",
		"dpop_private_key":{"kty":"EC","d":"secret"},
		"proxySlot":"slot-1",
		"updated_at":1760000000,
		"future_field":{"nested":[1,2]},
		"future_scalar":"keep me",
		"legacy_extra":42
	}`
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadAuth(path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if err := SaveAuth(path, loaded); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	assertJSONValue(t, raw["future_field"], `{"nested":[1,2]}`)
	assertJSONValue(t, raw["future_scalar"], `"keep me"`)
	assertJSONValue(t, raw["legacy_extra"], `42`)
	if raw["proxySlot"] == nil {
		t.Fatal("proxySlot was dropped")
	}
}

func TestLoadAuthToleratesEmptyAndMalformedExpiration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codearts-u1.json")
	for _, expiration := range []any{"", "not-a-date", nil, 12345} {
		body, err := json.Marshal(map[string]any{
			"user_id":            "u1",
			"cloud_dragon_token": "token",
			"expiration":         expiration,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadAuth(path)
		if err != nil {
			t.Fatalf("LoadAuth(expiration=%#v) error = %v", expiration, err)
		}
		if !loaded.Expiration.IsZero() {
			t.Fatalf("Expiration(expiration=%#v) = %v, want zero", expiration, loaded.Expiration)
		}
	}
}

func TestLoadAuthParsesISO8601Expiration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codearts-u1.json")
	body := `{"user_id":"u1","cloud_dragon_token":"token","expiration":"2026-11-08T01:02:03.5+08:00"}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAuth(path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	want := time.Date(2026, 11, 7, 17, 2, 3, 500000000, time.UTC)
	if !loaded.Expiration.Equal(want) {
		t.Fatalf("Expiration = %v, want %v", loaded.Expiration, want)
	}
}

func TestLoadAuthRejectsMissingRequiredFields(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
	}{
		{"missing token", `{"user_id":"u1"}`},
		{"empty token", `{"user_id":"u1","cloud_dragon_token":""}`},
		{"missing uid", `{"cloud_dragon_token":"token"}`},
		{"empty uid", `{"user_id":"","cloud_dragon_token":"token"}`},
		{"malformed json", `{`},
		{"non-object", `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "codearts-u1.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAuth(path); err == nil {
				t.Fatalf("LoadAuth(%s) error = nil, want error", tc.name)
			}
		})
	}

	if _, err := LoadAuth(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("LoadAuth(missing) error = nil, want error")
	}
}

func TestSaveAuthRejectsIncompleteInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codearts-u1.json")
	if err := SaveAuth("", Auth{UserID: "u1", CloudDragonToken: "token"}); err == nil {
		t.Fatal("SaveAuth(empty path) error = nil, want error")
	}
	if err := SaveAuth(path, Auth{CloudDragonToken: "token"}); err == nil {
		t.Fatal("SaveAuth(missing user_id) error = nil, want error")
	}
	if err := SaveAuth(path, Auth{UserID: "u1"}); err == nil {
		t.Fatal("SaveAuth(missing cloud_dragon_token) error = nil, want error")
	}
}

func TestAuthPathSanitizesUID(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"u1":         "codearts-u1.json",
		"a/b\\c d:e": "codearts-a_b_c_d_e.json",
		"..":         "codearts-...json",
	}
	for uid, wantName := range cases {
		got, err := AuthPath(dir, uid)
		if err != nil {
			t.Fatalf("AuthPath(%q): %v", uid, err)
		}
		if filepath.Dir(got) != filepath.Join(dir, "codearts") {
			t.Fatalf("AuthPath(%q) escaped folder: %q", uid, got)
		}
		if filepath.Base(got) != wantName {
			t.Fatalf("AuthPath(%q) = %q, want base %q", uid, got, wantName)
		}
	}

	got, err := AuthPath(dir, "")
	if err != nil {
		t.Fatalf("AuthPath(empty): %v", err)
	}
	if filepath.Base(got) != "codearts-unknown.json" {
		t.Fatalf("AuthPath(empty) = %q, want codearts-unknown.json", got)
	}
}

func TestStateListRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codearts", "state.json")
	states := map[string]AccountState{
		"u2": {
			Name:           "u2",
			ErrCount:       4,
			CoolUntil:      1800000000.75,
			Disabled:       true,
			ManualDisabled: true,
			Reason:         "manual disable",
			LastError:      "upstream 500",
			Checkin: map[string]any{
				"ok":         true,
				"checked_at": "2026-11-08T01:02:03",
				"credits":    float64(1000),
				"details":    map[string]any{"campaign": "daily"},
			},
			Note:         "operator note",
			TasksEnabled: true,
			Extra: map[string]json.RawMessage{
				"future_key":    json.RawMessage(`{"nested":[1,2,3]}`),
				"future_string": json.RawMessage(`"keep me"`),
			},
		},
		"u1": {Name: "u1", TasksEnabled: true},
	}
	if err := SaveState(path, states); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0600 {
			t.Fatalf("state mode = %v, want 0600", st.Mode().Perm())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("state file is not a JSON list: %v\n%s", err, data)
	}
	if len(raw) != 2 {
		t.Fatalf("state file has %d entries, want 2: %s", len(raw), data)
	}
	if string(raw[0]["name"]) != `"u1"` || string(raw[1]["name"]) != `"u2"` {
		t.Fatalf("state entries are not sorted by uid: %s", data)
	}
	var coolUntil float64
	if err := json.Unmarshal(raw[1]["cool_until"], &coolUntil); err != nil {
		t.Fatalf("cool_until is not numeric: %v (%s)", err, raw[1]["cool_until"])
	}
	if coolUntil != 1800000000.75 {
		t.Fatalf("cool_until = %v, want 1800000000.75", coolUntil)
	}
	assertJSONValue(t, raw[1]["future_key"], `{"nested":[1,2,3]}`)
	assertJSONValue(t, raw[1]["future_string"], `"keep me"`)

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadState returned %d entries, want 2: %#v", len(got), got)
	}
	u2 := got["u2"]
	if u2.Name != "u2" || u2.ErrCount != 4 || u2.CoolUntil != 1800000000.75 ||
		!u2.Disabled || !u2.ManualDisabled || u2.Reason != "manual disable" ||
		u2.LastError != "upstream 500" || u2.Note != "operator note" ||
		!u2.TasksEnabled {
		t.Fatalf("u2 state = %+v", u2)
	}
	if !reflect.DeepEqual(u2.Checkin, states["u2"].Checkin) {
		t.Fatalf("Checkin = %#v, want %#v", u2.Checkin, states["u2"].Checkin)
	}
	assertJSONValue(t, u2.Extra["future_key"], `{"nested":[1,2,3]}`)
	assertJSONValue(t, u2.Extra["future_string"], `"keep me"`)
}

func TestStateAcceptAccountsWrapperAndLegacyInference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"accounts":[
		{"name":"old-manual","disabled":true,"reason":"manual disable","cool_until":1234,"err_count":2,"last_error":"boom","checkin":{"ok":true},"note":"n"},
		{"name":"old-auto","disabled":true,"reason":"auth failed"},
		{"name":"manual-only","disabled":false,"reason":"manual disable"},
		{"name":"explicit","disabled":false,"manual_disabled":false,"tasks_enabled":false}
	]}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("LoadState returned %d entries, want 4: %#v", len(got), got)
	}
	oldManual := got["old-manual"]
	if !oldManual.Disabled || !oldManual.ManualDisabled ||
		oldManual.Reason != "manual disable" || oldManual.CoolUntil != 1234 ||
		oldManual.ErrCount != 2 || oldManual.LastError != "boom" ||
		!oldManual.TasksEnabled {
		t.Fatalf("old-manual state = %+v", oldManual)
	}
	if !got["old-auto"].Disabled || got["old-auto"].ManualDisabled {
		t.Fatalf("old-auto state = %+v", got["old-auto"])
	}
	if got["manual-only"].ManualDisabled {
		t.Fatalf("manual-only inferred a manual disable without disabled=true: %+v", got["manual-only"])
	}
	if got["explicit"].TasksEnabled {
		t.Fatalf("explicit tasks_enabled=false was not honored: %+v", got["explicit"])
	}

	// Saving v2 state keeps the v1 bare-list output shape.
	if err := SaveState(path, got); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("saved state is not a JSON list: %v\n%s", err, data)
	}
	if len(list) != 4 {
		t.Fatalf("saved state has %d entries, want 4: %s", len(list), data)
	}
}

func TestStateUnknownFieldsSurviveLoadAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `[{"name":"u1","note":"keep","future_key":{"nested":[1,2]},"future_scalar":"keep me"}]`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if err := SaveState(path, got); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("state = %s, want one entry", data)
	}
	assertJSONValue(t, raw[0]["future_key"], `{"nested":[1,2]}`)
	assertJSONValue(t, raw[0]["future_scalar"], `"keep me"`)
}

func TestStateCoolUntilAcceptsNumericAndRFC3339(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `[
		{"name":"numeric","cool_until":1800000000.5},
		{"name":"rfc","cool_until":"2026-11-08T01:02:03Z"},
		{"name":"zero","cool_until":0}
	]`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got["numeric"].CoolUntil != 1800000000.5 {
		t.Fatalf("numeric cool_until = %v, want 1800000000.5", got["numeric"].CoolUntil)
	}
	wantRFC := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)
	if got["rfc"].CoolUntil != float64(wantRFC.Unix()) {
		t.Fatalf("rfc cool_until = %v, want %v", got["rfc"].CoolUntil, wantRFC.Unix())
	}
	if got["zero"].CoolUntil != 0 {
		t.Fatalf("zero cool_until = %v, want 0", got["zero"].CoolUntil)
	}
}

func TestStateMalformedSafetyFieldsFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `[
		{"name":"string-disabled","disabled":"false","note":"truthy in v1"},
		{"name":"number-disabled","disabled":0},
		{"name":"string-manual","manual_disabled":"false"},
		{"name":"null-disabled","disabled":null},
		{"name":"null-tasks","tasks_enabled":null}
	]`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("LoadState returned %d entries, want 5: %#v", len(got), got)
	}
	for uid, state := range got {
		if uid == "null-tasks" {
			continue
		}
		if !state.Disabled {
			t.Fatalf("%s with malformed safety field loaded enabled: %+v", uid, state)
		}
	}
	if !got["string-manual"].ManualDisabled {
		t.Fatalf("malformed manual_disabled did not fail closed: %+v", got["string-manual"])
	}
	if got["null-tasks"].TasksEnabled {
		t.Fatalf("malformed tasks_enabled did not fail closed: %+v", got["null-tasks"])
	}
}

func TestStateNoteCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	longNote := strings.Repeat("字", 205)
	body, err := json.Marshal([]map[string]any{{
		"name": "u1",
		"note": longNote,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if n := utf8.RuneCountInString(got["u1"].Note); n != 200 {
		t.Fatalf("loaded note length = %d runes, want 200", n)
	}

	state := got["u1"]
	state.Note = strings.Repeat("字", 250)
	if err := SaveState(path, map[string]AccountState{"u1": state}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var savedNote string
	if err := json.Unmarshal(raw[0]["note"], &savedNote); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(savedNote); n != 200 {
		t.Fatalf("saved note length = %d runes, want 200", n)
	}
}

func TestStateSkipsMalformedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `[{"name":"good","note":"ok"}, 5, null, {"note":"missing name"}, {"name":""}, {"name":123,"note":"numeric"}]`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadState returned %d entries, want good and numeric: %#v", len(got), got)
	}
	if got["good"].Note != "ok" {
		t.Fatalf("good entry = %+v", got["good"])
	}
	if got["123"].Note != "numeric" {
		t.Fatalf("numeric name entry = %#v", got)
	}
}

func TestStateMissingFileIsEmpty(t *testing.T) {
	got, err := LoadState(filepath.Join(t.TempDir(), "missing", "state.json"))
	if err != nil {
		t.Fatalf("LoadState error = %v, want nil", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("LoadState = %#v, want empty map", got)
	}
}

func TestStateRejectsUnsupportedTopLevelShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"empty document", ``, true},
		{"truncated", `{`, true},
		{"null", `null`, true},
		{"string", `"text"`, true},
		{"number", `42`, true},
		{"empty object", `{}`, true},
		{"accounts object", `{"accounts":{}}`, true},
		{"accounts null", `{"accounts":null}`, true},
		{"accounts string", `{"accounts":"no"}`, true},
		{"bare list", `[{"name":"u1"}]`, false},
		{"empty list", `[]`, false},
		{"accounts list", `{"accounts":[{"name":"u1"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadState(path)
			if tc.wantErr && err == nil {
				t.Fatalf("LoadState(%q) error = nil, want error", tc.body)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("LoadState(%q) error = %v, want nil", tc.body, err)
			}
		})
	}
}

func TestSaveStateRefusesToOverwriteUnsupportedShapes(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{`{"future":{}}`, `{`, `null`, `{"accounts":{}}`} {
		path := filepath.Join(dir, "state.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(path, map[string]AccountState{"u1": {Name: "u1"}}); err == nil {
			t.Fatalf("SaveState over %q error = nil, want error", body)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != body {
			t.Fatalf("SaveState overwrote %q with %q", body, data)
		}
	}
}

func TestSaveStateEmptyWritesEmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, nil); err != nil {
		t.Fatalf("SaveState(nil): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var list []any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("state file is not a JSON list: %v (%s)", err, data)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("state file = %#v, want empty list", list)
	}
}

func assertJSONValue(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	assertJSONValueBytes(t, raw, json.RawMessage(want))
}

func assertJSONValueBytes(t *testing.T, raw, want json.RawMessage) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(raw, &gotValue); err != nil {
		t.Fatalf("unmarshal got JSON %q: %v", raw, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("unmarshal want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %q, want %q", raw, want)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
