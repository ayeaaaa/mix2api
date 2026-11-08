package codearts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
)

// maxStateNoteRunes mirrors v1's note[:200] truncation, which counted
// characters rather than bytes.
const maxStateNoteRunes = 200

// AccountState is one entry in the v1 CodeArts state.json list. Name is the
// account UID and is also the key returned by LoadState. CoolUntil remains
// numeric Unix seconds because that is the shape v1 reads with
// float(entry.get("cool_until") or 0).
type AccountState struct {
	Name           string
	ErrCount       int
	CoolUntil      float64
	Disabled       bool
	ManualDisabled bool
	Reason         string
	LastError      string
	Checkin        map[string]any
	Note           string
	TasksEnabled   bool

	// Extra holds unknown per-account JSON fields so a later save preserves
	// settings owned by newer v1/v2 code.
	Extra map[string]json.RawMessage
}

var knownStateJSONKeys = map[string]struct{}{
	"name":            {},
	"err_count":       {},
	"cool_until":      {},
	"disabled":        {},
	"manual_disabled": {},
	"reason":          {},
	"last_error":      {},
	"checkin":         {},
	"note":            {},
	"tasks_enabled":   {},
}

// MarshalJSON writes the v1 field order followed by unknown fields sorted by
// key. The note is capped here as well as on load so a caller cannot persist a
// longer value by constructing AccountState directly.
func (s AccountState) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	fields := []struct {
		name  string
		value any
	}{
		{"name", s.Name},
		{"err_count", s.ErrCount},
		{"cool_until", s.CoolUntil},
		{"disabled", s.Disabled},
		{"manual_disabled", s.ManualDisabled},
		{"reason", s.Reason},
		{"last_error", s.LastError},
		{"checkin", s.Checkin},
		{"note", truncateStateNote(s.Note)},
		{"tasks_enabled", s.TasksEnabled},
	}
	for _, field := range fields {
		if err := appendStateField(&buf, &first, field.name, field.value); err != nil {
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
		encoded := json.RawMessage(bytes.TrimSpace(s.Extra[key]))
		if len(encoded) == 0 {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(encoded)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func appendStateField(buf *bytes.Buffer, first *bool, name string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	key, err := json.Marshal(name)
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

// UnmarshalJSON tolerates malformed non-safety fields, numeric Unix or
// RFC3339 cool_until values, and missing tasks_enabled (default true).
// Safety fields fail closed: a present but non-boolean disabled or
// manual_disabled value marks the account disabled instead of silently
// enabling it. This covers v1's Python truthiness edge where the JSON string
// "false" is truthy.
func (s *AccountState) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("codearts: state entry is not a JSON object")
	}

	var state AccountState
	state.Name = decodeStringValue(raw["name"])
	if value, present, valid := decodeStateSafetyBool(raw, "disabled"); present {
		if valid {
			state.Disabled = value
		} else {
			state.Disabled = true
		}
	}
	if value, present, valid := decodeStateSafetyBool(raw, "manual_disabled"); present {
		if valid {
			state.ManualDisabled = value
		} else {
			state.ManualDisabled = true
			state.Disabled = true
		}
	}
	state.Reason = decodeStringValue(raw["reason"])
	state.ErrCount = decodeStateInt(raw["err_count"])
	state.LastError = decodeStringValue(raw["last_error"])
	state.CoolUntil = decodeCoolUntil(raw["cool_until"])
	state.Note = truncateStateNote(decodeStringValue(raw["note"]))
	state.Checkin = decodeCheckin(raw["checkin"])
	state.TasksEnabled = decodeTasksEnabled(raw)

	// v1 inferred manual disables before the flag existed through this exact
	// reason string.
	if state.Disabled && !state.ManualDisabled && state.Reason == "manual disable" {
		state.ManualDisabled = true
	}

	for key, value := range raw {
		if _, known := knownStateJSONKeys[key]; known {
			continue
		}
		if state.Extra == nil {
			state.Extra = make(map[string]json.RawMessage)
		}
		state.Extra[key] = append(json.RawMessage(nil), value...)
	}

	*s = state
	return nil
}

// decodeStateSafetyBool accepts only literal JSON booleans. Anything else is
// reported as present-but-invalid so the caller can fail closed.
func decodeStateSafetyBool(raw map[string]json.RawMessage, key string) (value, present, valid bool) {
	encoded, ok := raw[key]
	if !ok {
		return false, false, true
	}
	switch string(bytes.TrimSpace(encoded)) {
	case "true":
		return true, true, true
	case "false":
		return false, true, true
	default:
		return false, true, false
	}
}

func decodeTasksEnabled(raw map[string]json.RawMessage) bool {
	encoded, ok := raw["tasks_enabled"]
	if !ok {
		return true
	}
	switch string(bytes.TrimSpace(encoded)) {
	case "true":
		return true
	case "false":
		return false
	default:
		// Automation opt-out fails closed for any malformed value.
		return false
	}
}

func decodeCheckin(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var checkin map[string]any
	if err := json.Unmarshal(trimmed, &checkin); err != nil {
		return nil
	}
	return checkin
}

func decodeStateInt(raw json.RawMessage) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err == nil {
		if value, err := number.Int64(); err == nil {
			return int(value)
		}
		if value, err := number.Float64(); err == nil {
			return int(value)
		}
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err == nil {
			if value, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
				return value
			}
			if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
				return int(value)
			}
		}
		return 0
	}
	switch string(trimmed) {
	case "true":
		return 1
	case "false":
		return 0
	}
	return 0
}

// decodeCoolUntil accepts numeric Unix seconds, numeric strings, and RFC3339
// strings. Malformed values become zero, matching v1's "never" sentinel.
func decodeCoolUntil(raw json.RawMessage) float64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err == nil {
		if value, err := number.Float64(); err == nil {
			return value
		}
		return 0
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return 0
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return 0
		}
		if value, err := strconv.ParseFloat(text, 64); err == nil {
			return value
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return float64(parsed.Unix()) + float64(parsed.Nanosecond())/float64(time.Second)
		}
	}
	return 0
}

func truncateStateNote(note string) string {
	runes := []rune(note)
	if len(runes) <= maxStateNoteRunes {
		return note
	}
	return string(runes[:maxStateNoteRunes])
}

// LoadState reads the v1 CodeArts state document. A missing file yields an
// empty map. Both v1's bare JSON list and the accepted {"accounts":[...]}
// wrapper are read; other top-level shapes are rejected so a later save cannot
// silently destroy data the pool did not understand. Entries that are not JSON
// objects or have no usable name are skipped individually.
func LoadState(path string) (map[string]AccountState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]AccountState{}, nil
		}
		return nil, fmt.Errorf("codearts: read state file %s: %w", path, err)
	}

	entries, err := decodeStateEntries(data)
	if err != nil {
		return nil, fmt.Errorf("codearts: state file %s: %w", path, err)
	}
	states := make(map[string]AccountState, len(entries))
	for _, entry := range entries {
		state, ok := decodeStateEntry(entry)
		if !ok {
			continue
		}
		states[state.Name] = state
	}
	return states, nil
}

func decodeStateEntries(data []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("document is empty")
	}

	switch trimmed[0] {
	case '[':
		var entries []json.RawMessage
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("decode list: %w", err)
		}
		return entries, nil
	case '{':
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &wrapper); err != nil {
			return nil, fmt.Errorf("decode object: %w", err)
		}
		accounts, ok := wrapper["accounts"]
		if !ok {
			return nil, errors.New("unsupported object shape: missing accounts list")
		}
		accounts = bytes.TrimSpace(accounts)
		if len(accounts) == 0 || accounts[0] != '[' {
			return nil, errors.New("unsupported object shape: accounts is not a list")
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(accounts, &entries); err != nil {
			return nil, fmt.Errorf("decode accounts list: %w", err)
		}
		return entries, nil
	default:
		return nil, errors.New("unsupported top-level shape: want a JSON list or an object with an accounts list")
	}
}

func decodeStateEntry(raw json.RawMessage) (AccountState, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return AccountState{}, false
	}
	var state AccountState
	if err := json.Unmarshal(trimmed, &state); err != nil {
		return AccountState{}, false
	}
	if state.Name == "" {
		return AccountState{}, false
	}
	return state, true
}

// SaveState writes states as a v1-shaped JSON list with entries sorted by UID
// and fields in v1 order. The map key is authoritative for each entry's name.
// An existing state document is validated first; an unsupported or malformed
// shape is never overwritten. The file is written atomically with mode 0600.
func SaveState(path string, states map[string]AccountState) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("codearts: state path is empty")
	}

	existing, err := os.ReadFile(path)
	if err == nil {
		if _, err := decodeStateEntries(existing); err != nil {
			return fmt.Errorf("codearts: refusing to overwrite state file %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codearts: read state file %s: %w", path, err)
	}

	data, err := marshalStateList(states)
	if err != nil {
		return fmt.Errorf("codearts: encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codearts: create state directory: %w", err)
	}
	if err := secrets.WritePrivate(path, data); err != nil {
		return fmt.Errorf("codearts: save state: %w", err)
	}
	return nil
}

func marshalStateList(states map[string]AccountState) ([]byte, error) {
	names := make([]string, 0, len(states))
	for name := range states {
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	list := make([]AccountState, 0, len(names))
	for _, name := range names {
		state := states[name]
		state.Name = name
		list = append(list, state)
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
