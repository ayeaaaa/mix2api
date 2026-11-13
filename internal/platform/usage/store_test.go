package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	paths, err := ResolvePaths("", filepath.Join(t.TempDir(), "usage"))
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	return New(paths)
}

func mustAppend(t *testing.T, store *Store, row map[string]any) {
	t.Helper()
	if err := store.Append(row); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func decodeLine(t *testing.T, line []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var row map[string]any
	if err := dec.Decode(&row); err != nil {
		t.Fatalf("decode JSON line: %v (line=%q)", err, line)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		t.Fatalf("decode JSON line: trailing data (err=%v, line=%q)", err, line)
	}
	return row
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

func TestAppendWritesExactlyOneJSONLine(t *testing.T) {
	store := newTestStore(t)
	row := map[string]any{
		"at":      1700000000.25,
		"iso":     "2026-11-08T01:02:03",
		"model":   "model<&>",
		"stream":  false,
		"outcome": "completed",
		"kind":    "chat",
		"future_field": map[string]any{
			"tag": "<b>a & b</b>",
			"n":   json.Number("42"),
		},
	}
	mustAppend(t, store, row)

	data := readFile(t, store.Paths.Log)
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("log does not end with newline: %q", data)
	}
	if got := bytes.Count(data, []byte("\n")); got != 1 {
		t.Fatalf("newline count = %d, want 1: %q", got, data)
	}
	if bytes.Contains(data, []byte(`\u003c`)) ||
		bytes.Contains(data, []byte(`\u003e`)) ||
		bytes.Contains(data, []byte(`\u0026`)) {
		t.Fatalf("HTML characters were escaped: %q", data)
	}
	if !bytes.Contains(data, []byte("<b>a & b</b>")) {
		t.Fatalf("literal HTML payload missing: %q", data)
	}

	got := decodeLine(t, bytes.TrimSpace(data))
	if got["model"] != "model<&>" {
		t.Fatalf("model = %#v", got["model"])
	}
	future, ok := got["future_field"].(map[string]any)
	if !ok {
		t.Fatalf("future_field = %#v", got["future_field"])
	}
	if future["tag"] != "<b>a & b</b>" {
		t.Fatalf("future_field.tag = %#v", future["tag"])
	}
	if number, ok := future["n"].(json.Number); !ok || number.String() != "42" {
		t.Fatalf("future_field.n = %#v, want json.Number(42)", future["n"])
	}
	if got["at"].(json.Number).String() != "1700000000.25" {
		t.Fatalf("at = %#v", got["at"])
	}
}

func TestAppendRawWritesOneLine(t *testing.T) {
	store := newTestStore(t)
	if err := store.AppendRaw([]byte(`{"id":"raw-1"}` + "\n")); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	if err := store.AppendRaw([]byte(`{"id":"raw-2"}`)); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	data := readFile(t, store.Paths.Log)
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2: %q", len(lines), data)
	}
	if got := decodeLine(t, lines[0])["id"]; got != "raw-1" {
		t.Fatalf("first id = %#v", got)
	}
	if got := decodeLine(t, lines[1])["id"]; got != "raw-2" {
		t.Fatalf("second id = %#v", got)
	}
}

func TestAppendConcurrentWritersDoNotLoseRows(t *testing.T) {
	store := newTestStore(t)
	const workers, perWorker = 8, 300

	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for index := 0; index < perWorker; index++ {
				row := map[string]any{
					"worker":  worker,
					"index":   index,
					"at":      1700000000 + index,
					"padding": strings.Repeat("x", 200),
				}
				if err := store.Append(row); err != nil {
					errs <- err
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}

	data := readFile(t, store.Paths.Log)
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("log does not end with newline")
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(lines) != workers*perWorker {
		t.Fatalf("line count = %d, want %d", len(lines), workers*perWorker)
	}
	seen := make(map[string]bool, workers*perWorker)
	for _, line := range lines {
		row := decodeLine(t, line)
		key := fmt.Sprintf("%v/%v", row["worker"], row["index"])
		if seen[key] {
			t.Fatalf("duplicate row %s", key)
		}
		seen[key] = true
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("unique rows = %d, want %d", len(seen), workers*perWorker)
	}
}

func TestAppendToleratesMissingParentDirectory(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths("", filepath.Join(root, "deep", "nested", "usage"))
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	store := New(paths)
	mustAppend(t, store, map[string]any{"at": 1, "model": "m"})

	info, err := os.Stat(paths.Dir)
	if err != nil {
		t.Fatalf("Stat usage dir: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Fatalf("usage dir mode = %04o, want %04o", got, want)
	}
	fileInfo, err := os.Stat(paths.Log)
	if err != nil {
		t.Fatalf("Stat usage log: %v", err)
	}
	if got, want := fileInfo.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("usage log mode = %04o, want %04o", got, want)
	}
}

func TestLegacyUsageSummaryIsNeverWritten(t *testing.T) {
	store := newTestStore(t)
	if err := store.Paths.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	summary := filepath.Join(store.Paths.Dir, "usage-summary.json")
	original := []byte("{\"requests\": 123, \"unknown\": true}\n")
	if err := os.WriteFile(summary, original, 0o644); err != nil {
		t.Fatalf("write legacy summary: %v", err)
	}

	mustAppend(t, store, map[string]any{"at": 1, "model": "m", "realm": "intl"})
	if _, err := store.Recent(RecentOptions{Limit: 10}); err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if _, err := store.Export(io.Discard, ExportOptions{}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := readFile(t, summary); !bytes.Equal(got, original) {
		t.Fatalf("legacy usage-summary.json changed: %q", got)
	}
}
