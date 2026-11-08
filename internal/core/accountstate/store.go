package accountstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
)

// LoadStore reads a state file keyed by account UID. A missing, unreadable, or
// malformed file yields an empty map and no error, matching the tolerant v1
// behavior. Entries that are not JSON objects are skipped individually.
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

// SaveStore writes states as a JSON object keyed by UID. Keys are sorted and
// fields use a fixed order so repeated saves are byte-identical. The file is
// written atomically with private permissions.
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
