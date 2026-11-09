package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ExportOptions selects rows for Export. An empty Realm or Kind means no
// filter; Realm "all" also means every realm. Since and Until are inclusive.
// MaxRows <= 0 means no limit.
type ExportOptions struct {
	Realm   string
	Kind    string
	Since   *time.Time
	Until   *time.Time
	MaxRows int
}

// Export streams matching rows as NDJSON in file order. It is a read-only
// internal/tooling helper: it never rewrites, rotates, or truncates the
// ledger, and it does not add fields to the source rows.
func (s *Store) Export(w io.Writer, opts ExportOptions) (int64, error) {
	if s == nil {
		return 0, errors.New("usage: nil store")
	}
	if w == nil {
		return 0, errors.New("usage: nil export writer")
	}
	if s.Paths.Log == "" {
		return 0, errors.New("usage: empty usage log path")
	}

	file, err := os.Open(s.Paths.Log)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("usage: open ledger for export: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	var count int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				if row, ok := decodeRow(trimmed); ok && s.exportMatches(row, opts) {
					var buf bytes.Buffer
					encoder := json.NewEncoder(&buf)
					encoder.SetEscapeHTML(false)
					if err := encoder.Encode(row); err != nil {
						return count, fmt.Errorf("usage: encode export row: %w", err)
					}
					written, err := w.Write(buf.Bytes())
					if err != nil {
						return count, fmt.Errorf("usage: write export row: %w", err)
					}
					if written != buf.Len() {
						return count, fmt.Errorf("usage: write export row: short write (%d of %d bytes)", written, buf.Len())
					}
					count++
					if opts.MaxRows > 0 && count >= int64(opts.MaxRows) {
						return count, nil
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return count, fmt.Errorf("usage: read ledger for export: %w", readErr)
		}
	}
	return count, nil
}

func (s *Store) exportMatches(row Row, opts ExportOptions) bool {
	if realm := opts.Realm; realm != "" && realm != "all" {
		if s.rowRealm(row) != realm {
			return false
		}
	}
	if kind := opts.Kind; kind != "" && kind != "all" {
		if RowKind(row) != kind {
			return false
		}
	}
	at, ok := RowAt(row)
	if !ok {
		at = 0
	}
	if opts.Since != nil && at < timeSeconds(*opts.Since) {
		return false
	}
	if opts.Until != nil && at > timeSeconds(*opts.Until) {
		return false
	}
	return true
}

func timeSeconds(value time.Time) float64 {
	return float64(value.Unix()) + float64(value.Nanosecond())/1e9
}
