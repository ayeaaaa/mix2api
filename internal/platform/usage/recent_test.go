package usage

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeUsage(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func encodeUsageRows(t *testing.T, rows []map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("Marshal row: %v", err)
		}
		buf.Write(encoded)
		buf.WriteByte('\n')
	}
	return buf.String()
}

func writeUsageRows(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	writeUsage(t, path, encodeUsageRows(t, rows))
}

func pageIDs(page RecentPage) []string {
	ids := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestRecentPaginationAndRealmFilter(t *testing.T) {
	store := newTestStore(t)
	writeUsageRows(t, store.Paths.Log, []map[string]any{
		{"id": "r1", "at": 1, "model": "deepseek-v4.1-flash", "realm": "intl", "kind": "chat"},
		{"id": "r2", "at": 2, "model": "glm-5.3", "realm": "cn", "kind": "chat"},
		{"id": "r3", "at": 3, "model": "gpt-5.6", "realm": "intl", "kind": "image"},
		{"id": "r4", "at": 4, "model": "deepseek-v4.1-flash", "realm": "cn", "kind": "chat"},
		{"id": "r5", "at": 5, "model": "deepseek-v4-pro"},
	})

	page, err := store.Recent(RecentOptions{Limit: 2, Page: 1})
	if err != nil {
		t.Fatalf("Recent page 1: %v", err)
	}
	if page.Total != 5 || page.TotalPages != 3 || page.Page != 1 || page.Limit != 2 {
		t.Fatalf("page metadata = %+v", page)
	}
	if page.USDCNY != 0 {
		t.Fatalf("usd_cny = %v, want 0 until pricing is wired", page.USDCNY)
	}
	if page.Rows == nil {
		t.Fatal("Rows is nil; an empty page must still encode as []")
	}
	// r5 has no realm field and falls back to the cn-only model.
	assertIDs(t, pageIDs(page), "r5", "r4")

	page, err = store.Recent(RecentOptions{Limit: 2, Page: 2})
	if err != nil {
		t.Fatalf("Recent page 2: %v", err)
	}
	assertIDs(t, pageIDs(page), "r3", "r2")

	page, err = store.Recent(RecentOptions{Limit: 2, Page: 99})
	if err != nil {
		t.Fatalf("Recent clamped page: %v", err)
	}
	if page.Page != 3 {
		t.Fatalf("Page = %d, want clamped 3", page.Page)
	}
	assertIDs(t, pageIDs(page), "r1")

	page, err = store.Recent(RecentOptions{Limit: 2, Page: 1, Realm: "cn"})
	if err != nil {
		t.Fatalf("Recent cn: %v", err)
	}
	if page.Total != 3 || page.TotalPages != 2 {
		t.Fatalf("cn metadata = %+v", page)
	}
	assertIDs(t, pageIDs(page), "r5", "r4")

	page, err = store.Recent(RecentOptions{Limit: 2, Page: 2, Realm: "cn"})
	if err != nil {
		t.Fatalf("Recent cn page 2: %v", err)
	}
	assertIDs(t, pageIDs(page), "r2")

	page, err = store.Recent(RecentOptions{Limit: 10, Realm: "intl"})
	if err != nil {
		t.Fatalf("Recent intl: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("intl total = %d, want 2", page.Total)
	}
	assertIDs(t, pageIDs(page), "r3", "r1")

	for _, realm := range []string{"", "all"} {
		page, err = store.Recent(RecentOptions{Limit: 10, Realm: realm})
		if err != nil {
			t.Fatalf("Recent realm %q: %v", realm, err)
		}
		if page.Total != 5 {
			t.Fatalf("realm %q total = %d, want 5", realm, page.Total)
		}
		assertIDs(t, pageIDs(page), "r5", "r4", "r3", "r2", "r1")
	}

	// v1's HTTP reader clamps limit to 1..1000 and page to >=1. The store
	// applies the same bounds so tooling cannot accidentally scan everything.
	for _, tc := range []struct {
		name      string
		limit     int
		page      int
		wantLimit int
		wantPage  int
	}{
		{name: "zero limit", limit: 0, page: 1, wantLimit: 1, wantPage: 1},
		{name: "negative limit", limit: -3, page: 1, wantLimit: 1, wantPage: 1},
		{name: "limit capped at 1000", limit: 5000, page: 1, wantLimit: 1000, wantPage: 1},
		{name: "zero page", limit: 2, page: 0, wantLimit: 2, wantPage: 1},
		{name: "negative page", limit: 2, page: -9, wantLimit: 2, wantPage: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := store.Recent(RecentOptions{Limit: tc.limit, Page: tc.page})
			if err != nil {
				t.Fatalf("Recent: %v", err)
			}
			if page.Limit != tc.wantLimit || page.Page != tc.wantPage {
				t.Fatalf("metadata = %+v, want limit=%d page=%d", page, tc.wantLimit, tc.wantPage)
			}
		})
	}
}

func TestRecentSkipsBlankAndMalformedLines(t *testing.T) {
	t.Run("malformed middle line and empty lines", func(t *testing.T) {
		store := newTestStore(t)
		raw := "{\"id\":\"r1\",\"at\":1,\"model\":\"m1\",\"realm\":\"intl\"}\n\n" +
			"not json {oops\n" +
			"{\"id\":\"r2\",\"at\":2,\"model\":\"m2\",\"realm\":\"intl\"}\n" +
			"{\"id\":\"r3\",\"at\":3,\"model\":\"m3\",\"realm\":\"intl\"}"
		writeUsage(t, store.Paths.Log, raw)

		page, err := store.Recent(RecentOptions{Limit: 10})
		if err != nil {
			t.Fatalf("Recent: %v", err)
		}
		// The v1 count includes every non-empty newline-terminated line,
		// including malformed JSON, but not a trailing fragment with no newline.
		if page.Total != 3 {
			t.Fatalf("total = %d, want v1 count 3", page.Total)
		}
		if page.TotalPages != 1 {
			t.Fatalf("total_pages = %d, want 1", page.TotalPages)
		}
		// The trailing complete JSON row is a real row for the backward scan.
		assertIDs(t, pageIDs(page), "r3", "r2", "r1")
	})

	t.Run("trailing half line is ignored", func(t *testing.T) {
		store := newTestStore(t)
		raw := "{\"id\":\"r1\",\"at\":1,\"model\":\"m1\"}\n" +
			"{\"id\":\"r2\",\"at\":2,\"model\":\"m2\"}\n" +
			"{\"id\":\"r3\",\"at\":3,"
		writeUsage(t, store.Paths.Log, raw)

		page, err := store.Recent(RecentOptions{Limit: 10})
		if err != nil {
			t.Fatalf("Recent: %v", err)
		}
		if page.Total != 2 {
			t.Fatalf("total = %d, want 2", page.Total)
		}
		assertIDs(t, pageIDs(page), "r2", "r1")
	})

	t.Run("missing log is an empty page", func(t *testing.T) {
		store := newTestStore(t)
		page, err := store.Recent(RecentOptions{Limit: 10})
		if err != nil {
			t.Fatalf("Recent: %v", err)
		}
		if page.Total != 0 || page.TotalPages != 1 || page.Page != 1 || page.Limit != 10 {
			t.Fatalf("empty page = %+v", page)
		}
		if page.Rows == nil || len(page.Rows) != 0 {
			t.Fatalf("Rows = %#v, want non-nil empty slice", page.Rows)
		}
	})
}

func TestRecentLegacyRowsWithoutRealm(t *testing.T) {
	store := newTestStore(t)
	store.AccountRealm = func(account string) string {
		if account == "acct-cn" {
			return "cn"
		}
		return ""
	}
	writeUsageRows(t, store.Paths.Log, []map[string]any{
		{"id": "account-cn", "at": 1, "model": "gpt-5.6", "account": "acct-cn"},
		{"id": "model-cn", "at": 2, "model": "deepseek-v4-pro", "account": "acct-unknown"},
		{"id": "model-intl", "at": 3, "model": "gpt-6-astra"},
		{"id": "unknown-default", "at": 4, "model": "deepseek-v4.1-flash"},
	})

	if got := RowRealm(Row{"model": "deepseek-v4-pro", "account": "acct-unknown"}); got != "cn" {
		t.Fatalf("RowRealm model fallback = %q, want cn", got)
	}
	if got := RowRealm(Row{"model": "gpt-6-astra"}); got != "intl" {
		t.Fatalf("RowRealm intl fallback = %q, want intl", got)
	}
	if got := RowRealm(Row{}); got != "intl" {
		t.Fatalf("RowRealm default = %q, want intl", got)
	}
	if got := RowRealm(Row{"realm": 5, "model": "deepseek-v4-pro"}); got != "5" {
		t.Fatalf("RowRealm explicit non-string = %q, want 5", got)
	}

	// v1 checks the live account pool before the model's home realm.
	page, err := store.Recent(RecentOptions{Limit: 10, Realm: "cn"})
	if err != nil {
		t.Fatalf("Recent cn: %v", err)
	}
	assertIDs(t, pageIDs(page), "model-cn", "account-cn")
	if page.Total != 2 {
		t.Fatalf("cn total = %d, want 2", page.Total)
	}

	page, err = store.Recent(RecentOptions{Limit: 10, Realm: "intl"})
	if err != nil {
		t.Fatalf("Recent intl: %v", err)
	}
	assertIDs(t, pageIDs(page), "unknown-default", "model-intl")
}

func TestRowOutcomeAndKind(t *testing.T) {
	cases := []struct {
		name string
		row  Row
		want string
	}{
		{name: "explicit outcome", row: Row{"outcome": "client_aborted"}, want: "client_aborted"},
		{name: "legacy error", row: Row{"error": true}, want: "failed"},
		{name: "legacy success", row: Row{}, want: "completed"},
		{name: "empty error is not an error", row: Row{"error": false}, want: "completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowOutcome(tc.row); got != tc.want {
				t.Fatalf("RowOutcome = %q, want %q", got, tc.want)
			}
		})
	}
	if got := RowKind(Row{}); got != "chat" {
		t.Fatalf("missing kind = %q, want chat", got)
	}
	if got := RowKind(Row{"kind": "video"}); got != "video" {
		t.Fatalf("kind = %q, want video", got)
	}
}

func writeRecentIndex(t *testing.T, store *Store, mutate func(map[string]any)) {
	t.Helper()
	data := readFile(t, store.Paths.Log)
	info, err := os.Stat(store.Paths.Log)
	if err != nil {
		t.Fatalf("Stat log: %v", err)
	}
	dev, ino, _ := fileIdentity(info)
	start := len(data) - 64
	if start < 0 {
		start = 0
	}
	counts := map[string]int{"intl": 3, "cn": 2}
	rowCounts := map[string]int{"intl": 3, "cn": 2}
	doc := map[string]any{
		"schema":    1,
		"seg":       65536,
		"key":       []any{len(data), dev, ino},
		"tail":      hex.EncodeToString(data[start:]),
		"legacy":    false,
		"realms":    []string{"intl", "cn"},
		"totals":    map[string]any{"lines": 5, "rows": 5, "counts": counts, "row_counts": rowCounts},
		"segs":      []any{},
		"realm_map": []any{},
	}
	if mutate != nil {
		mutate(doc)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(store.Paths.RecentIndex, encoded, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

func TestRecentIndexReaderAndFallback(t *testing.T) {
	store := newTestStore(t)
	writeUsageRows(t, store.Paths.Log, []map[string]any{
		{"id": "r1", "at": 1, "model": "m1", "realm": "intl"},
		{"id": "r2", "at": 2, "model": "m2", "realm": "cn"},
		{"id": "r3", "at": 3, "model": "m3", "realm": "intl"},
		{"id": "r4", "at": 4, "model": "m4", "realm": "cn"},
		{"id": "r5", "at": 5, "model": "m5", "realm": "intl"},
	})
	writeRecentIndex(t, store, nil)

	index, ok := store.loadRecentIndex()
	if !ok {
		t.Fatal("valid v1-shaped recent index was not accepted")
	}
	if index.lines != 5 || index.rows != 5 {
		t.Fatalf("index totals = lines %d rows %d", index.lines, index.rows)
	}
	page, err := store.Recent(RecentOptions{Limit: 2, Page: 1, Realm: "cn"})
	if err != nil {
		t.Fatalf("Recent via index: %v", err)
	}
	if page.Total != 2 || page.TotalPages != 1 {
		t.Fatalf("index page metadata = %+v", page)
	}
	assertIDs(t, pageIDs(page), "r4", "r2")

	// A segment at the boundary after row 3 starts page 2 directly at row 4.
	// The page then reads one row and reverses it, matching the backward scan.
	raw := readFile(t, store.Paths.Log)
	lines := bytes.SplitAfter(raw, []byte("\n"))
	offset := len(lines[0]) + len(lines[1]) + len(lines[2])
	writeRecentIndex(t, store, func(doc map[string]any) {
		doc["segs"] = []any{[]any{offset, 3, 2, 1}}
	})
	page, err = store.Recent(RecentOptions{Limit: 1, Page: 2, Realm: "cn"})
	if err != nil {
		t.Fatalf("Recent with segment index: %v", err)
	}
	if page.Total != 2 || page.TotalPages != 2 || page.Page != 2 {
		t.Fatalf("segment page metadata = %+v", page)
	}
	assertIDs(t, pageIDs(page), "r2")

	// A structurally valid index is trusted like v1: if it says 999 lines the
	// accelerator reports 999 rather than silently rescanning.
	writeRecentIndex(t, store, func(doc map[string]any) {
		doc["totals"].(map[string]any)["lines"] = 999
	})
	page, err = store.Recent(RecentOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Recent with accepted index: %v", err)
	}
	if page.Total != 999 {
		t.Fatalf("total = %d, want 999 from accepted index", page.Total)
	}

	// Malformed, mismatched, or stale indexes must fall back to the log.
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong schema", mutate: func(doc map[string]any) { doc["schema"] = 2 }},
		{name: "wrong segment", mutate: func(doc map[string]any) { doc["seg"] = 4096 }},
		{name: "bad tail", mutate: func(doc map[string]any) { doc["tail"] = "zz" }},
		{name: "null totals", mutate: func(doc map[string]any) { doc["totals"] = nil }},
		{name: "null lines", mutate: func(doc map[string]any) {
			doc["totals"].(map[string]any)["lines"] = nil
		}},
		{name: "null counts", mutate: func(doc map[string]any) {
			doc["totals"].(map[string]any)["counts"] = nil
		}},
		{name: "null key", mutate: func(doc map[string]any) { doc["key"] = nil }},
		{name: "null segs", mutate: func(doc map[string]any) { doc["segs"] = nil }},
		{name: "wrong size", mutate: func(doc map[string]any) { doc["key"].([]any)[0] = len(readFile(t, store.Paths.Log)) - 1 }},
		{name: "wrong inode", mutate: func(doc map[string]any) { doc["key"].([]any)[2] = 987654321 }},
		{name: "wrong realm width", mutate: func(doc map[string]any) { doc["segs"] = []any{[]any{1, 2}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeRecentIndex(t, store, func(doc map[string]any) {
				// Poison the totals first so a bad index that is accidentally
				// accepted makes the assertion fail loudly.
				doc["totals"].(map[string]any)["lines"] = 999
				tc.mutate(doc)
			})
			page, err := store.Recent(RecentOptions{Limit: 10})
			if err != nil {
				t.Fatalf("Recent: %v", err)
			}
			if page.Total != 5 {
				t.Fatalf("total = %d, want fallback scan count 5", page.Total)
			}
			assertIDs(t, pageIDs(page), "r5", "r4", "r3", "r2", "r1")
		})
	}

	// An index that folded rows without a realm field depends on the live
	// account pool. This package cannot reproduce that attribution, so a realm
	// query must use the authoritative scan instead of the index.
	writeRecentIndex(t, store, func(doc map[string]any) {
		doc["legacy"] = true
		doc["totals"].(map[string]any)["lines"] = 999
		doc["totals"].(map[string]any)["counts"].(map[string]int)["cn"] = 999
	})
	page, err = store.Recent(RecentOptions{Limit: 10, Realm: "cn"})
	if err != nil {
		t.Fatalf("Recent legacy index: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("legacy index realm total = %d, want fallback 2", page.Total)
	}
	assertIDs(t, pageIDs(page), "r4", "r2")

	// Appending changes the log identity, so a stale index is not reused.
	writeRecentIndex(t, store, nil)
	mustAppend(t, store, map[string]any{"id": "r6", "at": 6, "model": "m6", "realm": "intl"})
	page, err = store.Recent(RecentOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Recent after append: %v", err)
	}
	if page.Total != 6 {
		t.Fatalf("total after append = %d, want 6", page.Total)
	}
	assertIDs(t, pageIDs(page), "r6", "r5", "r4", "r3", "r2", "r1")
}
