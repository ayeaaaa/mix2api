package logs

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS logs (
    id    INTEGER PRIMARY KEY,
    ts    TEXT NOT NULL,
    time  TEXT NOT NULL,
    level TEXT NOT NULL,
    tag   TEXT NOT NULL,
    msg   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_level ON logs(level);
CREATE INDEX IF NOT EXISTS idx_logs_tag   ON logs(tag);
`

var sqliteColumns = []string{"id", "ts", "time", "level", "tag", "msg"}

// SQLiteStore is the durable v1-compatible logs.sqlite3 store. All access goes
// through one database/sql connection, matching v1's single worker connection.
type SQLiteStore struct {
	db   *sql.DB
	path string

	// Now is injectable for retention tests. It defaults to time.Now.
	Now func() time.Time
}

// NewSQLite opens (creating when necessary) a v1-compatible logs.sqlite3.
func NewSQLite(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("logs: empty sqlite path")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if directory := filepath.Dir(path); directory != "" && directory != "." {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return nil, fmt.Errorf("logs: create database directory: %w", err)
			}
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("logs: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &SQLiteStore{db: db, path: path}
	closeOnError := func(openErr error) (*SQLiteStore, error) {
		_ = db.Close()
		return nil, openErr
	}

	// auto_vacuum must be set before the first table exists. On an existing
	// v1 database it is a no-op and the byte cap falls back to VACUUM.
	if _, err := db.Exec("PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return closeOnError(fmt.Errorf("logs: set auto_vacuum: %w", err))
	}
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journalMode); err != nil {
		return closeOnError(fmt.Errorf("logs: set journal_mode: %w", err))
	}
	if !strings.EqualFold(journalMode, "delete") {
		return closeOnError(fmt.Errorf("logs: journal_mode is %q, want delete", journalMode))
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		return closeOnError(fmt.Errorf("logs: set synchronous: %w", err))
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return closeOnError(fmt.Errorf("logs: set busy_timeout: %w", err))
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		return closeOnError(fmt.Errorf("logs: create schema: %w", err))
	}
	if err := db.Ping(); err != nil {
		return closeOnError(fmt.Errorf("logs: ping sqlite: %w", err))
	}
	return store, nil
}

func (s *SQLiteStore) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Path reports the database file path.
func (s *SQLiteStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// AppendBatch writes up to the caller's batch size in one transaction.
func (s *SQLiteStore) AppendBatch(entries []Entry) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("logs: sqlite store is closed")
	}
	if len(entries) == 0 {
		return nil
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("logs: begin append batch: %w", err)
	}
	statement, err := transaction.Prepare(
		"INSERT OR REPLACE INTO logs (id, ts, time, level, tag, msg) VALUES (?, ?, ?, ?, ?, ?)",
	)
	if err != nil {
		_ = transaction.Rollback()
		return fmt.Errorf("logs: prepare append: %w", err)
	}
	for _, entry := range entries {
		if _, err := statement.Exec(
			entry.ID, entry.TS, entry.Time, entry.Level, entry.Tag, truncateMessage(entry.Msg),
		); err != nil {
			_ = statement.Close()
			_ = transaction.Rollback()
			return fmt.Errorf("logs: append row %d: %w", entry.ID, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = transaction.Rollback()
		return fmt.Errorf("logs: close append statement: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("logs: commit append batch: %w", err)
	}
	return nil
}

// Query returns v1-shaped rows. SinceID takes precedence over Limit.
func (s *SQLiteStore) Query(query Query) (Result, error) {
	result := Result{Logs: make([]Entry, 0)}
	if s == nil || s.db == nil {
		return result, fmt.Errorf("logs: sqlite store is closed")
	}

	where := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if query.SinceID > 0 {
		where = append(where, "id > ?")
		args = append(args, query.SinceID)
	}
	if level := strings.ToUpper(strings.TrimSpace(query.Level)); level != "" {
		where = append(where, "level = ?")
		args = append(args, level)
	}
	if tag := strings.ToLower(strings.TrimSpace(query.Tag)); tag != "" {
		where = append(where, "LOWER(tag) = ?")
		args = append(args, tag)
	}
	if search := strings.ToLower(strings.TrimSpace(query.Search)); search != "" {
		where = append(where, "(instr(LOWER(msg), ?) > 0 OR instr(LOWER(tag), ?) > 0)")
		args = append(args, search, search)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM logs"+clause, args...).Scan(&total); err != nil {
		return result, fmt.Errorf("logs: count rows: %w", err)
	}
	result.Total = int(total)

	columns := strings.Join(sqliteColumns, ", ")
	selectSQL := "SELECT " + columns + " FROM logs" + clause + " ORDER BY id"
	selectArgs := append([]any(nil), args...)
	if query.Limit > 0 && query.SinceID <= 0 {
		selectSQL += " DESC LIMIT ?"
		selectArgs = append(selectArgs, query.Limit)
	}
	rows, err := s.db.Query(selectSQL, selectArgs...)
	if err != nil {
		return result, fmt.Errorf("logs: query rows: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entry Entry
		if err := rows.Scan(&entry.ID, &entry.TS, &entry.Time, &entry.Level, &entry.Tag, &entry.Msg); err != nil {
			return result, fmt.Errorf("logs: scan row: %w", err)
		}
		result.Logs = append(result.Logs, entry)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("logs: read rows: %w", err)
	}
	if query.Limit > 0 && query.SinceID <= 0 {
		for left, right := 0, len(result.Logs)-1; left < right; left, right = left+1, right-1 {
			result.Logs[left], result.Logs[right] = result.Logs[right], result.Logs[left]
		}
	}
	if len(result.Logs) > 0 {
		result.MaxID = result.Logs[len(result.Logs)-1].ID
	} else if query.SinceID > 0 {
		result.MaxID = query.SinceID
	}
	return result, nil
}

// Clear deletes every row.
func (s *SQLiteStore) Clear() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("logs: sqlite store is closed")
	}
	if _, err := s.db.Exec("DELETE FROM logs"); err != nil {
		return fmt.Errorf("logs: clear rows: %w", err)
	}
	return nil
}

// MaxID is the highest stored ID, or zero when empty.
func (s *SQLiteStore) MaxID() (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("logs: sqlite store is closed")
	}
	var id int64
	if err := s.db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM logs").Scan(&id); err != nil {
		return 0, fmt.Errorf("logs: read max id: %w", err)
	}
	return id, nil
}

// Stats reports row count, time span, and file size.
func (s *SQLiteStore) Stats() (Stats, error) {
	stats := Stats{}
	if s == nil || s.db == nil {
		return stats, fmt.Errorf("logs: sqlite store is closed")
	}
	var oldest, newest sql.NullString
	if err := s.db.QueryRow("SELECT COUNT(*), MIN(ts), MAX(ts) FROM logs").Scan(
		&stats.Rows, &oldest, &newest,
	); err != nil {
		return stats, fmt.Errorf("logs: read stats: %w", err)
	}
	if oldest.Valid {
		stats.Oldest = oldest.String
	}
	if newest.Valid {
		stats.Newest = newest.String
	}
	stats.Bytes = s.fileSize()
	stats.Path = s.path
	return stats, nil
}

// Trim applies the row and age caps. Both are cheap DELETEs.
func (s *SQLiteStore) Trim(caps Caps) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("logs: sqlite store is closed")
	}
	caps = caps.normalized()
	if caps.MaxRows == 0 && caps.MaxDays == 0 {
		return nil
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("logs: begin trim: %w", err)
	}
	if caps.MaxDays > 0 {
		cutoff := time.Unix(s.now().Unix()-caps.MaxDays*86400, 0).Format("2006-01-02 15:04:05")
		if _, err := transaction.Exec("DELETE FROM logs WHERE ts < ?", cutoff); err != nil {
			_ = transaction.Rollback()
			return fmt.Errorf("logs: trim days: %w", err)
		}
	}
	if caps.MaxRows > 0 {
		var total int64
		if err := transaction.QueryRow("SELECT COUNT(*) FROM logs").Scan(&total); err != nil {
			_ = transaction.Rollback()
			return fmt.Errorf("logs: count for row trim: %w", err)
		}
		if total > caps.MaxRows {
			if _, err := transaction.Exec(
				"DELETE FROM logs WHERE id NOT IN (SELECT id FROM logs ORDER BY id DESC LIMIT ?)",
				caps.MaxRows,
			); err != nil {
				_ = transaction.Rollback()
				return fmt.Errorf("logs: trim rows: %w", err)
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("logs: commit trim: %w", err)
	}
	return nil
}

// TrimBytes deletes oldest rows and VACUUMs until the database file is under
// maxBytes. It aims below the cap so the next writes do not trip it at once.
func (s *SQLiteStore) TrimBytes(maxBytes int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("logs: sqlite store is closed")
	}
	if maxBytes <= 0 {
		return nil
	}
	size := s.fileSize()
	if size <= maxBytes {
		return nil
	}
	target := maxBytes * 85 / 100
	if target < 1 {
		target = 1
	}
	for attempt := 0; attempt < 4; attempt++ {
		var total int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&total); err != nil {
			return fmt.Errorf("logs: count for byte trim: %w", err)
		}
		if total <= 0 {
			break
		}
		average := float64(size) / float64(total)
		if average < 1 {
			average = 1
		}
		keep := int64(float64(target) / average)
		if keep < 1 {
			keep = 1
		}
		drop := total - keep
		if drop <= 0 {
			break
		}
		if _, err := s.db.Exec(
			"DELETE FROM logs WHERE id IN (SELECT id FROM logs ORDER BY id ASC LIMIT ?)",
			drop,
		); err != nil {
			return fmt.Errorf("logs: delete oldest rows: %w", err)
		}
		if _, err := s.db.Exec("VACUUM"); err != nil {
			return fmt.Errorf("logs: vacuum: %w", err)
		}
		newSize := s.fileSize()
		if newSize >= size {
			break
		}
		size = newSize
		if size <= maxBytes {
			break
		}
	}
	return nil
}

// Close closes the database. It is safe to call more than once.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	if err != nil {
		return fmt.Errorf("logs: close sqlite: %w", err)
	}
	return nil
}

func (s *SQLiteStore) fileSize() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(s.path + suffix)
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}
