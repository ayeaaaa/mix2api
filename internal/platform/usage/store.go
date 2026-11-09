package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store is the usage JSONL ledger. Append is safe for concurrent use; readers
// either see a complete line or no line at all.
type Store struct {
	Paths Paths

	// AccountRealm optionally resolves a legacy row's account uid to its realm
	// before falling back to the model's home realm. It mirrors v1 row_realm,
	// which consulted the live account pool. Keep it immutable once the store
	// is in use.
	AccountRealm func(account string) string
}

// New returns a Store for paths. No files are created until the first append.
func New(paths Paths) *Store { return &Store{Paths: paths} }

// appendMu is process-wide rather than per-Store: several Store values may
// point at the same ledger, and v1's append path was serialized process-wide
// too. One encoded []byte is handed to a single O_APPEND write per row.
var appendMu sync.Mutex

// Append serializes one row as a compact JSON line and appends it with a
// single write. HTML escaping is disabled so the ledger matches v1's
// json.dumps(ensure_ascii=False) output. Unknown fields are passed through
// untouched.
func (s *Store) Append(row map[string]any) error {
	if s == nil {
		return errors.New("usage: nil store")
	}
	if row == nil {
		return errors.New("usage: nil usage row")
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(row); err != nil {
		return fmt.Errorf("usage: encode row: %w", err)
	}
	return s.appendLine(buf.Bytes())
}

// AppendRaw appends one already-encoded JSON line. A single trailing newline
// is added, and any CR/LF characters already at the end are removed first.
func (s *Store) AppendRaw(line []byte) error {
	if s == nil {
		return errors.New("usage: nil store")
	}
	trimmed := bytes.TrimRight(line, "\r\n")
	payload := make([]byte, len(trimmed)+1)
	copy(payload, trimmed)
	payload[len(payload)-1] = '\n'
	return s.appendLine(payload)
}

func (s *Store) appendLine(payload []byte) error {
	if s.Paths.Log == "" {
		return errors.New("usage: empty usage log path")
	}

	appendMu.Lock()
	defer appendMu.Unlock()

	// v1 creates the usage directory on demand (os.makedirs exist_ok=True).
	if err := os.MkdirAll(filepath.Dir(s.Paths.Log), 0o755); err != nil {
		return fmt.Errorf("usage: create ledger directory: %w", err)
	}
	file, err := os.OpenFile(s.Paths.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("usage: open ledger: %w", err)
	}
	written, writeErr := file.Write(payload)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("usage: append ledger: %w", writeErr)
	}
	if written != len(payload) {
		return fmt.Errorf("usage: append ledger: short write (%d of %d bytes)", written, len(payload))
	}
	if closeErr != nil {
		return fmt.Errorf("usage: close ledger: %w", closeErr)
	}
	return nil
}
