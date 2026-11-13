package usage

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func decodeNDJSON(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	if len(data) == 0 {
		return nil
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("export does not end with newline: %q", data)
	}
	parts := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	rows := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		rows = append(rows, decodeLine(t, part))
	}
	return rows
}

func ndjsonIDs(rows []map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestExportFiltersAndPreservesOrder(t *testing.T) {
	store := newTestStore(t)
	writeUsageRows(t, store.Paths.Log, []map[string]any{
		{"id": "e1", "at": 100, "model": "m1", "realm": "intl", "kind": "chat"},
		{"id": "e2", "at": 200, "model": "m2", "realm": "cn", "kind": "image"},
		{"id": "e3", "at": 300, "model": "m3", "realm": "intl", "kind": "video"},
		{"id": "e4", "at": 400, "model": "m4", "realm": "cn"},
		{"id": "e5", "at": 500, "model": "<b>&</b>", "realm": "intl", "kind": "chat", "future_field": 7},
	})
	before := readFile(t, store.Paths.Log)

	since := time.Unix(200, 0)
	until := time.Unix(300, 0)
	cases := []struct {
		name string
		opts ExportOptions
		want []string
	}{
		{name: "all rows in file order", opts: ExportOptions{}, want: []string{"e1", "e2", "e3", "e4", "e5"}},
		{name: "realm all is unfiltered", opts: ExportOptions{Realm: "all"}, want: []string{"e1", "e2", "e3", "e4", "e5"}},
		{name: "realm cn", opts: ExportOptions{Realm: "cn"}, want: []string{"e2", "e4"}},
		{name: "kind image", opts: ExportOptions{Kind: "image"}, want: []string{"e2"}},
		{name: "legacy rows count as chat", opts: ExportOptions{Kind: "chat"}, want: []string{"e1", "e4", "e5"}},
		{name: "since is inclusive", opts: ExportOptions{Since: &since}, want: []string{"e2", "e3", "e4", "e5"}},
		{name: "until is inclusive", opts: ExportOptions{Until: &until}, want: []string{"e1", "e2", "e3"}},
		{name: "window and realm", opts: ExportOptions{Realm: "intl", Since: &since}, want: []string{"e3", "e5"}},
		{name: "realm kind and window", opts: ExportOptions{Realm: "intl", Kind: "chat", Since: &until}, want: []string{"e5"}},
		{name: "max rows caps after filtering", opts: ExportOptions{Realm: "cn", MaxRows: 1}, want: []string{"e2"}},
		{name: "max rows caps in file order", opts: ExportOptions{MaxRows: 2}, want: []string{"e1", "e2"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			count, err := store.Export(&buf, tc.opts)
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			rows := decodeNDJSON(t, buf.Bytes())
			if got := ndjsonIDs(rows); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
			if count != int64(len(tc.want)) {
				t.Fatalf("count = %d, want %d", count, len(tc.want))
			}
			if tc.name == "all rows in file order" {
				if !bytes.Contains(buf.Bytes(), []byte("<b>&</b>")) {
					t.Fatalf("export HTML-escaped the row: %q", buf.Bytes())
				}
				if future, ok := rows[4]["future_field"]; !ok || future == nil {
					t.Fatalf("unknown field was dropped: %#v", rows[4])
				}
			}
		})
	}

	if after := readFile(t, store.Paths.Log); !bytes.Equal(before, after) {
		t.Fatal("Export mutated usage.jsonl")
	}
}

func TestExportMissingLogIsEmpty(t *testing.T) {
	store := newTestStore(t)
	var buf bytes.Buffer
	count, err := store.Export(&buf, ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if count != 0 || buf.Len() != 0 {
		t.Fatalf("count=%d output=%q, want empty", count, buf.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestExportPropagatesWriterErrors(t *testing.T) {
	store := newTestStore(t)
	writeUsageRows(t, store.Paths.Log, []map[string]any{
		{"id": "e1", "at": 1, "model": "m1", "realm": "intl"},
	})
	if _, err := store.Export(failingWriter{}, ExportOptions{}); err == nil {
		t.Fatal("Export returned nil when the writer failed")
	}
	if _, err := store.Export(io.Discard, ExportOptions{}); err != nil {
		t.Fatalf("Export to Discard: %v", err)
	}
}
