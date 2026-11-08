package accountstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
)

// knownStateJSONKeys are fields modeled by State. They are never copied into
// Extra and cannot be overridden by raw JSON.
var knownStateJSONKeys = map[string]struct{}{
	"uid":               {},
	"disabled":          {},
	"manual_disabled":   {},
	"disabled_by":       {},
	"reason":            {},
	"err_count":         {},
	"last_error":        {},
	"cool_until":        {},
	"last_used":         {},
	"note":              {},
	"tasks_enabled":     {},
	"active_concurrent": {},
	"max_concurrent":    {},
}

// LoadStore reads a state file keyed by account UID. A missing, unreadable, or
// malformed file yields an empty map and no error, matching the tolerant v1
// behavior. Entries that are not JSON objects are skipped individually.
// Legacy numeric Unix-second timestamps are accepted alongside RFC3339, and
// unknown per-account fields are retained in State.Extra.
func LoadStore(path string) (map[string]State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]State{}, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return map[string]State{}, nil
	}

	states := make(map[string]State, len(raw))
	for uid, entry := range raw {
		if uid == "" {
			continue
		}
		state, ok := decodeState(entry)
		if !ok {
			continue
		}
		state.UID = uid
		states[uid] = state
	}
	return states, nil
}

func decodeState(raw json.RawMessage) (State, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return State{}, false
	}
	var state State
	if err := json.Unmarshal(trimmed, &state); err != nil {
		return State{}, false
	}
	return state, true
}

// MarshalJSON writes known fields in a fixed order followed by unknown fields
// sorted by key. SaveStore writes the surrounding UID map in sorted order.
func (s State) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	fields := []struct {
		name  string
		value any
	}{
		{"disabled", s.Disabled},
		{"manual_disabled", s.ManualDisabled},
		{"disabled_by", s.DisabledBy},
		{"reason", s.Reason},
		{"err_count", s.ErrCount},
		{"last_error", s.LastError},
		{"cool_until", s.CoolUntil},
		{"last_used", s.LastUsed},
		{"note", s.Note},
		{"tasks_enabled", s.TasksEnabled},
		{"active_concurrent", s.ActiveConcurrent},
		{"max_concurrent", s.MaxConcurrent},
	}
	for _, field := range fields {
		if err := appendJSONField(&buf, &first, field.name, field.value); err != nil {
			return nil, err
		}
	}

	extraKeys := make([]string, 0, len(s.Extra))
	for key := range s.Extra {
		if _, known := knownStateJSONKeys[key]; !known {
			extraKeys = append(extraKeys, key)
		}
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		if err := appendJSONField(&buf, &first, key, s.Extra[key]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func appendJSONField(buf *bytes.Buffer, first *bool, name string, value any) error {
	key, err := json.Marshal(name)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if !*first {
		buf.WriteByte(',')
	}
	*first = false
	buf.Write(key)
	buf.WriteByte(':')
	buf.Write(encoded)
	return nil
}

// UnmarshalJSON tolerates malformed known fields, legacy numeric timestamps,
// and unknown keys. ActiveConcurrent is runtime-only and always starts at zero
// after a restart so an account cannot be left permanently busy.
func (s *State) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var state State
	decodeStateField(raw, "disabled", &state.Disabled)
	decodeStateField(raw, "manual_disabled", &state.ManualDisabled)
	decodeStateField(raw, "disabled_by", &state.DisabledBy)
	decodeStateField(raw, "reason", &state.Reason)
	decodeStateField(raw, "err_count", &state.ErrCount)
	decodeStateField(raw, "last_error", &state.LastError)
	decodeTimestampField(raw, "cool_until", &state.CoolUntil)
	decodeTimestampField(raw, "last_used", &state.LastUsed)
	decodeStateField(raw, "note", &state.Note)
	decodeStateField(raw, "tasks_enabled", &state.TasksEnabled)
	decodeStateField(raw, "max_concurrent", &state.MaxConcurrent)
	// active_concurrent is intentionally not restored.

	for key, value := range raw {
		if _, known := knownStateJSONKeys[key]; known {
			continue
		}
		if state.Extra == nil {
			state.Extra = map[string]json.RawMessage{}
		}
		state.Extra[key] = value
	}

	*s = state
	return nil
}

func decodeStateField(raw map[string]json.RawMessage, key string, dst any) {
	value, ok := raw[key]
	if !ok {
		return
	}
	_ = json.Unmarshal(value, dst)
}

func decodeTimestampField(raw map[string]json.RawMessage, key string, dst *time.Time) {
	value, ok := raw[key]
	if !ok {
		return
	}
	if timestamp, err := parseTimestamp(value); err == nil {
		*dst = timestamp
	}
}

// parseTimestamp accepts RFC3339 strings and legacy v1 numeric Unix seconds
// (integer or fractional). Zero is v1's "never" sentinel and becomes the zero
// time.
func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return time.Time{}, err
		}
		if text == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339, text)
	}

	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return time.Time{}, err
	}
	seconds, err := number.Float64()
	if err != nil {
		return time.Time{}, err
	}
	if seconds == 0 {
		return time.Time{}, nil
	}
	whole, fraction := math.Modf(seconds)
	return time.Unix(int64(whole), int64(math.Round(fraction*1e9))).UTC(), nil
}

// SaveStore writes states as a JSON object keyed by UID. Empty UIDs are
// skipped because LoadStore ignores them. Keys are sorted and fields use a
// fixed order so repeated saves are byte-identical. The file is written
// atomically with private permissions.
func SaveStore(path string, states map[string]State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("accountstate: create parent directory: %w", err)
	}

	data, err := marshalStore(states)
	if err != nil {
		return fmt.Errorf("accountstate: encode states: %w", err)
	}
	if err := secrets.WritePrivate(path, data); err != nil {
		return fmt.Errorf("accountstate: save store: %w", err)
	}
	return nil
}

func marshalStore(states map[string]State) ([]byte, error) {
	uids := make([]string, 0, len(states))
	for uid := range states {
		if uid == "" {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, uid := range uids {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(uid)
		if err != nil {
			return nil, err
		}
		entry, err := json.MarshalIndent(states[uid], "  ", "  ")
		if err != nil {
			return nil, err
		}
		buf.WriteString("\n  ")
		buf.Write(key)
		buf.WriteString(": ")
		buf.Write(entry)
	}
	if len(uids) > 0 {
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}
