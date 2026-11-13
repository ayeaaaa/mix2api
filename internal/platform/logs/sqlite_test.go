package logs

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func newTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLite(filepath.Join(t.TempDir(), "logs.sqlite3"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func testEntry(id int64, level, tag, msg, ts string) Entry {
	return Entry{ID: id, TS: ts, Time: "00:00:00", Level: level, Tag: tag, Msg: msg}
}

func TestSQLiteSchemaMatchesV1(t *testing.T) {
	store := newTestSQLite(t)

	rows, err := store.db.Query("PRAGMA table_info(logs)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns = append(columns, name)
		if name == "id" {
			if !strings.EqualFold(columnType, "INTEGER") || primaryKey != 1 {
				t.Fatalf("id column = type %q pk %d", columnType, primaryKey)
			}
			continue
		}
		if !strings.EqualFold(columnType, "TEXT") || notNull != 1 {
			t.Fatalf("%s column = type %q notnull %d", name, columnType, notNull)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	want := []string{"id", "ts", "time", "level", "tag", "msg"}
	if strings.Join(columns, ",") != strings.Join(want, ",") {
		t.Fatalf("columns = %v, want %v", columns, want)
	}

	indexRows, err := store.db.Query("PRAGMA index_list(logs)")
	if err != nil {
		t.Fatalf("index_list: %v", err)
	}
	defer indexRows.Close()
	indexes := map[string]bool{}
	for indexRows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := indexRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		indexes[name] = true
	}
	if err := indexRows.Err(); err != nil {
		t.Fatalf("index_list rows: %v", err)
	}
	for _, name := range []string{"idx_logs_level", "idx_logs_tag"} {
		if !indexes[name] {
			t.Fatalf("missing index %s in %v", name, indexes)
		}
	}
	for indexName, columnName := range map[string]string{
		"idx_logs_level": "level",
		"idx_logs_tag":   "tag",
	} {
		infoRows, err := store.db.Query("PRAGMA index_info(" + indexName + ")")
		if err != nil {
			t.Fatalf("index_info(%s): %v", indexName, err)
		}
		var found bool
		for infoRows.Next() {
			var seqno, cid int
			var name string
			if err := infoRows.Scan(&seqno, &cid, &name); err != nil {
				infoRows.Close()
				t.Fatalf("scan index_info(%s): %v", indexName, err)
			}
			if name == columnName {
				found = true
			}
		}
		if err := infoRows.Err(); err != nil {
			infoRows.Close()
			t.Fatalf("index_info(%s) rows: %v", indexName, err)
		}
		if err := infoRows.Close(); err != nil {
			t.Fatalf("close index_info(%s): %v", indexName, err)
		}
		if !found {
			t.Fatalf("index %s does not cover %s", indexName, columnName)
		}
	}

	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "delete") {
		t.Fatalf("journal_mode = %q, want delete", journalMode)
	}

	var autoVacuum int
	if err := store.db.QueryRow("PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	if autoVacuum != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental)", autoVacuum)
	}
}

func TestSQLiteV1FixtureCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage", "logs.sqlite3")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE logs (
			id    INTEGER PRIMARY KEY,
			ts    TEXT NOT NULL,
			time  TEXT NOT NULL,
			level TEXT NOT NULL,
			tag   TEXT NOT NULL,
			msg   TEXT NOT NULL
		);
		CREATE INDEX idx_logs_level ON logs(level);
		CREATE INDEX idx_logs_tag   ON logs(tag);
		INSERT INTO logs(id, ts, time, level, tag, msg) VALUES
			(7, '2026-10-01 00:00:00', '00:00:00', 'INFO', 'tasks', 'legacy 13812345678'),
			(9, '2026-10-01 00:00:01', '00:00:01', 'ERROR', 'chat', 'legacy failure');
	`); err != nil {
		raw.Close()
		t.Fatalf("seed fixture: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	store, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite(fixture): %v", err)
	}
	defer store.Close()

	result, err := store.Query(Query{})
	if err != nil {
		t.Fatalf("Query(fixture): %v", err)
	}
	if result.Total != 2 || result.Logs[0].ID != 7 || result.Logs[1].ID != 9 {
		t.Fatalf("fixture rows = %#v", result)
	}
	if result.Logs[0].Msg != "legacy 138****5678" {
		t.Fatalf("fixture message = %q", result.Logs[0].Msg)
	}
	maxID, err := store.MaxID()
	if err != nil || maxID != 9 {
		t.Fatalf("MaxID = %d, %v; want 9", maxID, err)
	}
	if err := store.AppendBatch([]Entry{testEntry(10, "INFO", "system", "after fixture", "2026-10-01 00:00:02")}); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if maxID, err = store.MaxID(); err != nil || maxID != 10 {
		t.Fatalf("MaxID after append = %d, %v; want 10", maxID, err)
	}
}

func TestSQLiteDisabledCreatesNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage", "logs.sqlite3")
	store := Open(path, false, Caps{}, nil)
	defer store.Close()

	if store.Enabled() {
		t.Fatal("store unexpectedly enabled")
	}
	if store.Mode() != "memory" {
		t.Fatalf("mode = %q, want memory", store.Mode())
	}
	if err := store.Append(Entry{Level: "INFO", Tag: "system", Msg: "one"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	result, err := store.Query(Query{})
	if err != nil || result.Total != 1 {
		t.Fatalf("memory query = %#v, %v", result, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled store created %s (err %v)", path, err)
	}

	if err := store.Configure(true, 1000, 7, 0); err != nil {
		t.Fatalf("Configure(true): %v", err)
	}
	if !store.Enabled() || store.Mode() != "db" {
		t.Fatalf("store not enabled after Configure: enabled=%v mode=%s", store.Enabled(), store.Mode())
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("enabled store did not create %s: %v", path, err)
	}
	if err := store.Append(Entry{Level: "INFO", Tag: "system", Msg: "two"}); err != nil {
		t.Fatalf("Append(db): %v", err)
	}
	result, err = store.Query(Query{})
	if err != nil {
		t.Fatalf("Query(db): %v", err)
	}
	if result.Total != 1 || result.Logs[0].Msg != "two" {
		t.Fatalf("db query = %#v, want only the enabled-time row", result)
	}

	if err := store.Configure(false, 0, 0, 0); err != nil {
		t.Fatalf("Configure(false): %v", err)
	}
	if store.Enabled() || store.Mode() != "memory" {
		t.Fatalf("store still enabled after disable: enabled=%v mode=%s", store.Enabled(), store.Mode())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("disabling removed the database file: %v", err)
	}
	if err := store.Append(Entry{Level: "INFO", Tag: "system", Msg: "three"}); err != nil {
		t.Fatalf("Append(memory): %v", err)
	}
	result, err = store.Query(Query{})
	if err != nil || result.Total != 3 {
		t.Fatalf("memory query after disable = %#v, %v", result, err)
	}
}

func TestSQLiteRetentionRowsDaysBytes(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)

	t.Run("rows", func(t *testing.T) {
		store := newTestSQLite(t)
		store.Now = func() time.Time { return now }
		entries := make([]Entry, 0, 10)
		for id := int64(1); id <= 10; id++ {
			entries = append(entries, testEntry(id, "INFO", "system", fmt.Sprintf("row %d", id), "2026-11-10 12:00:00"))
		}
		if err := store.AppendBatch(entries); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		if err := store.Trim(Caps{MaxRows: 5}); err != nil {
			t.Fatalf("Trim rows: %v", err)
		}
		result, err := store.Query(Query{})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if result.Total != 5 || result.Logs[0].ID != 6 || result.Logs[4].ID != 10 {
			t.Fatalf("row trim result = %#v", result)
		}
		if err := store.Trim(Caps{}); err != nil {
			t.Fatalf("Trim unlimited: %v", err)
		}
		if result, _ := store.Query(Query{}); result.Total != 5 {
			t.Fatalf("blank caps removed rows: total %d", result.Total)
		}
	})

	t.Run("days", func(t *testing.T) {
		store := newTestSQLite(t)
		store.Now = func() time.Time { return now }
		old := now.AddDate(0, 0, -8).Format("2006-01-02 15:04:05")
		recent := now.AddDate(0, 0, -6).Format("2006-01-02 15:04:05")
		today := now.Format("2006-01-02 15:04:05")
		entries := []Entry{
			testEntry(1, "INFO", "system", "old", old),
			testEntry(2, "INFO", "system", "recent", recent),
			testEntry(3, "INFO", "system", "today", today),
		}
		if err := store.AppendBatch(entries); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		if err := store.Trim(Caps{MaxDays: 7}); err != nil {
			t.Fatalf("Trim days: %v", err)
		}
		result, err := store.Query(Query{})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if result.Total != 2 || result.Logs[0].ID != 2 || result.Logs[1].ID != 3 {
			t.Fatalf("day trim result = %#v", result)
		}
		if DefaultCaps().MaxDays != 7 {
			t.Fatalf("DefaultCaps().MaxDays = %d, want 7", DefaultCaps().MaxDays)
		}
		if err := store.AppendBatch([]Entry{testEntry(4, "INFO", "system", "old again", old)}); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		if err := store.Trim(DefaultCaps()); err != nil {
			t.Fatalf("Trim default: %v", err)
		}
		if result, _ := store.Query(Query{}); result.Total != 2 {
			t.Fatalf("default days cap result total = %d, want 2", result.Total)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		store := newTestSQLite(t)
		msg := strings.Repeat("x", 4096)
		entries := make([]Entry, 0, 300)
		for id := int64(1); id <= 300; id++ {
			entries = append(entries, testEntry(id, "INFO", "system", msg, "2026-11-10 12:00:00"))
		}
		if err := store.AppendBatch(entries); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		before, err := os.Stat(store.Path())
		if err != nil {
			t.Fatalf("Stat before: %v", err)
		}
		const capBytes = int64(64 * 1024)
		if before.Size() <= capBytes {
			t.Fatalf("test database unexpectedly small: %d", before.Size())
		}
		if err := store.TrimBytes(capBytes); err != nil {
			t.Fatalf("TrimBytes: %v", err)
		}
		after, err := os.Stat(store.Path())
		if err != nil {
			t.Fatalf("Stat after: %v", err)
		}
		if after.Size() > capBytes {
			t.Fatalf("size after byte trim = %d, want <= %d", after.Size(), capBytes)
		}
		result, err := store.Query(Query{})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if result.Total <= 0 || result.Total >= 300 {
			t.Fatalf("byte trim kept %d rows, want some but not all", result.Total)
		}
	})

	t.Run("message cap", func(t *testing.T) {
		store := newTestSQLite(t)
		long := strings.Repeat("a", MaxMessageBytes+1000)
		unicode := strings.Repeat("好", MaxMessageBytes/3+10)
		if err := store.AppendBatch([]Entry{
			testEntry(1, "INFO", "system", long, "2026-11-10 12:00:00"),
			testEntry(2, "INFO", "system", unicode, "2026-11-10 12:00:01"),
		}); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		result, err := store.Query(Query{})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		for _, entry := range result.Logs {
			if len(entry.Msg) > MaxMessageBytes {
				t.Fatalf("message length = %d, want <= %d", len(entry.Msg), MaxMessageBytes)
			}
			if !utf8.ValidString(entry.Msg) {
				t.Fatalf("message was cut inside a rune")
			}
		}
	})
}

func TestStoreFallsBackToRingOnDatabaseError(t *testing.T) {
	store := Open(filepath.Join(t.TempDir(), "logs.sqlite3"), true, Caps{}, nil)
	defer store.Close()

	if err := store.Append(Entry{Level: "INFO", Tag: "system", Msg: "persisted"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if result, err := store.Query(Query{}); err != nil || result.Total != 1 {
		t.Fatalf("first query = %#v, %v", result, err)
	}

	// Break the durable connection after the first query has flushed the
	// append. The next query must transparently use the memory ring.
	if err := store.db.Close(); err != nil {
		t.Fatalf("close underlying database: %v", err)
	}
	if err := store.Append(Entry{Level: "WARN", Tag: "system", Msg: "memory"}); err != nil {
		t.Fatalf("Append after break: %v", err)
	}
	result, err := store.Query(Query{})
	if err != nil {
		t.Fatalf("fallback query returned error: %v", err)
	}
	if result.Total != 2 {
		t.Fatalf("fallback total = %d, want 2", result.Total)
	}
}

func TestSQLiteReopenResumesIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.sqlite3")
	first := Open(path, true, Caps{}, nil)
	for index := 1; index <= 3; index++ {
		if err := first.Append(Entry{Level: "INFO", Tag: "system", Msg: fmt.Sprintf("row %d", index)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	second := Open(path, true, Caps{}, nil)
	defer second.Close()
	if !second.Enabled() {
		t.Fatal("reopened store not enabled")
	}
	if err := second.Append(Entry{Level: "INFO", Tag: "system", Msg: "row 4"}); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	result, err := second.Query(Query{SinceID: 3})
	if err != nil {
		t.Fatalf("Query since 3: %v", err)
	}
	if result.Total != 1 || len(result.Logs) != 1 || result.Logs[0].ID != 4 || result.MaxID != 4 {
		t.Fatalf("resumed ids = %#v", result)
	}
	all, err := second.Query(Query{})
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if all.Total != 4 {
		t.Fatalf("reopened total = %d, want 4", all.Total)
	}
}

func TestConcurrentAppendAndQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.sqlite3")
	store := Open(path, true, Caps{}, nil)
	defer store.Close()

	const writers = 4
	const perWriter = 100
	var writeGroup sync.WaitGroup
	writeGroup.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func(writer int) {
			defer writeGroup.Done()
			for index := 0; index < perWriter; index++ {
				entry := Entry{
					Level: "INFO",
					Tag:   "concurrent",
					Msg:   fmt.Sprintf("writer %d entry %d", writer, index),
				}
				if err := store.Append(entry); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(writer)
	}

	var readGroup sync.WaitGroup
	for reader := 0; reader < 3; reader++ {
		readGroup.Add(1)
		go func() {
			defer readGroup.Done()
			for index := 0; index < 50; index++ {
				result, err := store.Query(Query{Limit: 25})
				if err != nil {
					t.Errorf("Query: %v", err)
					return
				}
				if len(result.Logs) > 25 {
					t.Errorf("Query returned %d rows", len(result.Logs))
					return
				}
				for position := 1; position < len(result.Logs); position++ {
					if result.Logs[position-1].ID >= result.Logs[position].ID {
						t.Errorf("ids not ascending: %d then %d", result.Logs[position-1].ID, result.Logs[position].ID)
						return
					}
				}
			}
		}()
	}

	writeGroup.Wait()
	readGroup.Wait()

	result, err := store.Query(Query{})
	if err != nil {
		t.Fatalf("final Query: %v", err)
	}
	if result.Total != writers*perWriter {
		t.Fatalf("final total = %d, want %d", result.Total, writers*perWriter)
	}
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	result, err = store.Query(Query{})
	if err != nil {
		t.Fatalf("Query after clear: %v", err)
	}
	if result.Total != 0 || len(result.Logs) != 0 {
		t.Fatalf("rows after clear = %#v", result)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats after clear: %v", err)
	}
	if stats.Rows != 0 {
		t.Fatalf("stats rows after clear = %d", stats.Rows)
	}
}

func TestOpenDefaultsToSevenDayRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.sqlite3")
	seed, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	now := time.Now()
	oldTS := now.Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	newTS := now.Format("2006-01-02 15:04:05")
	if err := seed.AppendBatch([]Entry{
		{ID: 1, TS: oldTS, Time: "00:00:00", Level: "INFO", Tag: "system", Msg: "old"},
		{ID: 2, TS: newTS, Time: "00:00:01", Level: "INFO", Tag: "system", Msg: "new"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	store := Open(path, true, Caps{}, nil)
	defer store.Close()
	result, err := store.Query(Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Total != 1 || len(result.Logs) != 1 || result.Logs[0].ID != 2 {
		t.Fatalf("default retention did not drop the old row: %#v", result)
	}

	unlimitedPath := filepath.Join(t.TempDir(), "logs-unlimited.sqlite3")
	unlimitedSeed, err := NewSQLite(unlimitedPath)
	if err != nil {
		t.Fatalf("NewSQLite unlimited: %v", err)
	}
	if err := unlimitedSeed.AppendBatch([]Entry{{
		ID: 1, TS: oldTS, Time: "00:00:00", Level: "INFO", Tag: "system", Msg: "old",
	}}); err != nil {
		t.Fatalf("seed unlimited: %v", err)
	}
	if err := unlimitedSeed.Close(); err != nil {
		t.Fatalf("close unlimited seed: %v", err)
	}
	unlimited := OpenUnlimited(unlimitedPath, true, nil)
	defer unlimited.Close()
	unlimitedResult, err := unlimited.Query(Query{})
	if err != nil {
		t.Fatalf("unlimited Query: %v", err)
	}
	if unlimitedResult.Total != 1 {
		t.Fatalf("OpenUnlimited dropped caps: %#v", unlimitedResult)
	}
}

func TestConfigureMaxIDFailureFailsClosed(t *testing.T) {
	original := openSQLiteStore
	t.Cleanup(func() { openSQLiteStore = original })
	openSQLiteStore = func(path string) (*SQLiteStore, error) {
		store, err := NewSQLite(path)
		if err != nil {
			return nil, err
		}
		if _, err := store.db.Exec("DROP TABLE logs"); err != nil {
			_ = store.Close()
			return nil, err
		}
		return store, nil
	}

	store := Open(filepath.Join(t.TempDir(), "logs.sqlite3"), false, Caps{}, nil)
	defer store.Close()
	if err := store.Configure(true, 0, 0, 0); err == nil {
		t.Fatal("Configure succeeded after MaxID failure")
	}
	if store.Enabled() {
		t.Fatal("store enabled after MaxID failure")
	}
}

func TestWorkerRecoversFromPanic(t *testing.T) {
	original := openSQLiteStore
	t.Cleanup(func() { openSQLiteStore = original })
	panicOnce := true
	now := time.Now
	openSQLiteStore = func(path string) (*SQLiteStore, error) {
		store, err := NewSQLite(path)
		if err != nil {
			return nil, err
		}
		store.Now = func() time.Time {
			if panicOnce {
				panicOnce = false
				panic("trim panic")
			}
			return now()
		}
		return store, nil
	}

	store := Open(filepath.Join(t.TempDir(), "logs.sqlite3"), false, Caps{}, nil)
	defer store.Close()
	if err := store.Configure(true, 0, 7, 0); err == nil {
		t.Fatal("panicking configure unexpectedly succeeded")
	}
	if err := store.Configure(true, 0, 7, 0); err != nil {
		t.Fatalf("worker did not recover after panic: %v", err)
	}
	if !store.Enabled() {
		t.Fatal("store not enabled after recovered configure")
	}
	if _, err := store.Query(Query{}); err != nil {
		t.Fatalf("query after recovered panic: %v", err)
	}
}

func TestConcurrentAppendAndClear(t *testing.T) {
	store := Open(filepath.Join(t.TempDir(), "logs.sqlite3"), true, Caps{}, nil)
	defer store.Close()

	const writers = 4
	const perWriter = 75
	var group sync.WaitGroup
	group.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func(writer int) {
			defer group.Done()
			for index := 0; index < perWriter; index++ {
				if err := store.Append(Entry{Level: "INFO", Tag: "concurrent", Msg: fmt.Sprintf("w%d-%d", writer, index)}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(writer)
	}
	for clear := 0; clear < 100; clear++ {
		if err := store.Clear(); err != nil {
			t.Errorf("Clear: %v", err)
			break
		}
	}
	group.Wait()

	result, err := store.Query(Query{})
	if err != nil {
		t.Fatalf("Query after concurrent clear: %v", err)
	}
	if result.Total < 0 || result.Total > writers*perWriter {
		t.Fatalf("total = %d", result.Total)
	}
	for index := 1; index < len(result.Logs); index++ {
		if result.Logs[index-1].ID >= result.Logs[index].ID {
			t.Fatalf("ids not ascending: %d then %d", result.Logs[index-1].ID, result.Logs[index].ID)
		}
	}
}
