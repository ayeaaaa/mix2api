package usage

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
)

// RecentOptions selects one page of rows from the tail of the ledger.
type RecentOptions struct {
	Limit int
	Page  int
	Realm string
}

// RecentPage is the v1 /usage/recent payload.
type RecentPage struct {
	Total      int              `json:"total"`
	Page       int              `json:"page"`
	Limit      int              `json:"limit"`
	TotalPages int              `json:"total_pages"`
	USDCNY     float64          `json:"usd_cny"`
	Rows       []map[string]any `json:"rows"`
}

const (
	recentChunkSize       = 256 * 1024
	recentIndexSchema     = 1
	recentIndexSegment    = 65536
	recentIndexRealmCap   = 6
	recentIndexRealmMax   = 128
	recentIndexTailLength = 64
)

// Recent returns the requested page, newest first. It uses the v1 row-offset
// index when a structurally valid, log-matching index is present and falls
// back to the byte-for-byte v1 backward scan otherwise. Malformed and blank
// lines never fail the read.
func (s *Store) Recent(opts RecentOptions) (RecentPage, error) {
	if s == nil {
		return RecentPage{}, errors.New("usage: nil store")
	}
	limit := opts.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	page := opts.Page
	if page < 1 {
		page = 1
	}
	realm := opts.Realm
	if realm == "all" {
		realm = ""
	}

	var (
		total      int
		rows       []map[string]any
		pageOutput = page
	)
	indexed := false
	if index, ok := s.loadRecentIndex(); ok && (realm == "" || !index.legacy) {
		total = int(index.totalFor(realm))
		pages := totalPages(total, limit)
		pageOutput = page
		if pageOutput > pages {
			pageOutput = pages
		}
		pageRows, pageOK := s.recentIndexPage(index, realm, pageOutput, limit)
		if pageOK {
			rows = pageRows
			indexed = true
		}
	}

	if !indexed {
		var err error
		total, err = s.countUsageRows(realm)
		if err != nil {
			return RecentPage{}, err
		}
		pages := totalPages(total, limit)
		pageOutput = page
		if pageOutput > pages {
			pageOutput = pages
		}
		rows, err = s.recentBackwardPage(realm, pageOutput, limit)
		if err != nil {
			return RecentPage{}, err
		}
	}

	if rows == nil {
		rows = []map[string]any{}
	}
	return RecentPage{
		Total:      total,
		Page:       pageOutput,
		Limit:      limit,
		TotalPages: totalPages(total, limit),
		USDCNY:     0,
		Rows:       rows,
	}, nil
}

func totalPages(total, limit int) int {
	if total <= 0 {
		return 1
	}
	return (total + limit - 1) / limit
}

// countUsageRows mirrors v1 count_usage_rows: every non-empty newline-
// terminated line counts toward the total, including malformed JSON, while a
// trailing fragment is left for the next append. Realm counts use the same
// textual realm field fast path as v1 and fall back to row_realm only for
// lines that have no realm field at all.
func (s *Store) countUsageRows(realm string) (int, error) {
	file, err := os.Open(s.Paths.Log)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("usage: open ledger for counting: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	total := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				if realm == "" {
					total++
				} else if s.lineMatchesRealm(trimmed, realm) {
					total++
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return total, fmt.Errorf("usage: read ledger for counting: %w", readErr)
		}
	}
	return total, nil
}

func (s *Store) lineMatchesRealm(line []byte, realm string) bool {
	values, seen := realmValuesFromLine(line)
	if seen {
		for _, value := range values {
			if value == realm {
				return true
			}
		}
		return false
	}
	row, ok := decodeRow(line)
	if !ok {
		return false
	}
	return s.rowRealm(row) == realm
}

// realmValuesFromLine reproduces the two field spellings v1's count uses
// (": \"value\"" and ":\"value\""). The second return value distinguishes a
// malformed realm field from a row that has no realm field, so the latter can
// use the parsed row_realm fallback.
func realmValuesFromLine(line []byte) ([]string, bool) {
	const key = `"realm"`
	var values []string
	seen := false
	for offset := 0; offset < len(line); {
		index := bytes.Index(line[offset:], []byte(key))
		if index < 0 {
			break
		}
		seen = true
		position := offset + index
		end := position + len(key)
		var start int
		switch {
		case bytes.HasPrefix(line[end:], []byte(`: "`)):
			start = end + 3
		case bytes.HasPrefix(line[end:], []byte(`:"`)):
			start = end + 2
		default:
			offset = end
			continue
		}
		quote := bytes.IndexByte(line[start:], '"')
		if quote >= 0 {
			values = append(values, string(line[start:start+quote]))
		}
		offset = end
	}
	return values, seen
}

// recentBackwardPage is the v1 fallback: read 256 KiB chunks from the tail,
// append just enough newest rows, then slice out the requested page.
func (s *Store) recentBackwardPage(realm string, page, limit int) ([]map[string]any, error) {
	file, err := os.Open(s.Paths.Log)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("usage: open ledger for recent: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("usage: stat ledger for recent: %w", err)
	}

	targetCount := page * limit
	matching := make([]map[string]any, 0, targetCount)
	position := info.Size()
	var buffer []byte
	for position > 0 && len(matching) < targetCount {
		step := int64(recentChunkSize)
		if position < step {
			step = position
		}
		position -= step

		chunk := make([]byte, int(step))
		if _, err := file.ReadAt(chunk, position); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("usage: read ledger for recent: %w", err)
		}
		buffer = append(chunk, buffer...)
		parts := bytes.Split(buffer, []byte("\n"))
		buffer = append([]byte(nil), parts[0]...)
		for index := len(parts) - 1; index >= 1; index-- {
			line := bytes.TrimSpace(parts[index])
			if len(line) == 0 {
				continue
			}
			row, ok := decodeRow(line)
			if !ok {
				continue
			}
			if realm != "" && s.rowRealm(row) != realm {
				continue
			}
			matching = append(matching, row)
			if len(matching) >= targetCount {
				break
			}
		}
	}

	// The first line of the file has no preceding newline in any chunk, so it
	// is parsed separately. A complete trailing JSON object without a final
	// newline was already handled by the loop, matching v1.
	if len(matching) < targetCount {
		line := bytes.TrimSpace(buffer)
		if len(line) > 0 {
			if row, ok := decodeRow(line); ok &&
				(realm == "" || s.rowRealm(row) == realm) {
				matching = append(matching, row)
			}
		}
	}

	start := (page - 1) * limit
	if start >= len(matching) {
		return nil, nil
	}
	end := start + limit
	if end > len(matching) {
		end = len(matching)
	}
	return matching[start:end], nil
}

type recentIndex struct {
	size      int64
	tail      []byte
	legacy    bool
	realms    []string
	lines     int64
	rows      int64
	counts    map[string]int64
	rowCounts map[string]int64
	segs      [][]int64 // cumulative: offset, all rows, then rows per realm
}

func (index *recentIndex) totalFor(realm string) int64 {
	if realm == "" {
		return index.lines
	}
	return index.counts[realm]
}

type recentIndexDocument struct {
	Schema   *int               `json:"schema"`
	Segment  *int               `json:"seg"`
	Key      *[]int64           `json:"key"`
	Tail     *string            `json:"tail"`
	RealmMap json.RawMessage    `json:"realm_map"`
	Legacy   *bool              `json:"legacy"`
	Realms   *[]string          `json:"realms"`
	Totals   *recentIndexTotals `json:"totals"`
	Segs     *[][]int64         `json:"segs"`
}

type recentIndexTotals struct {
	Lines     *int64            `json:"lines"`
	Rows      *int64            `json:"rows"`
	Counts    *map[string]int64 `json:"counts"`
	RowCounts *map[string]int64 `json:"row_counts"`
}

// loadRecentIndex reads and validates the v1 row-offset index. The index is
// only an accelerator: every malformed field, wrong schema/segment width,
// changed log identity, or incorrect tail signature makes the caller rescan.
func (s *Store) loadRecentIndex() (*recentIndex, bool) {
	if s == nil || s.Paths.Log == "" || s.Paths.RecentIndex == "" {
		return nil, false
	}
	data, err := os.ReadFile(s.Paths.RecentIndex)
	if err != nil {
		return nil, false
	}
	var document recentIndexDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, false
	}
	if document.Schema == nil || *document.Schema != recentIndexSchema ||
		document.Segment == nil || *document.Segment != recentIndexSegment ||
		document.Legacy == nil || document.Realms == nil ||
		document.Key == nil || document.Tail == nil ||
		document.Totals == nil || document.Segs == nil {
		return nil, false
	}
	if len(*document.Key) != 3 {
		return nil, false
	}
	for _, value := range *document.Key {
		if value < 0 {
			return nil, false
		}
	}
	size := (*document.Key)[0]

	realms := *document.Realms
	if len(realms) > recentIndexRealmCap {
		return nil, false
	}
	seenRealms := make(map[string]struct{}, len(realms))
	for _, realm := range realms {
		if len(realm) > recentIndexRealmMax {
			return nil, false
		}
		if _, exists := seenRealms[realm]; exists {
			return nil, false
		}
		seenRealms[realm] = struct{}{}
	}

	if document.RealmMap != nil && !bytes.Equal(bytes.TrimSpace(document.RealmMap), []byte("null")) {
		var realmMap [][]string
		if err := json.Unmarshal(document.RealmMap, &realmMap); err != nil {
			return nil, false
		}
		for _, pair := range realmMap {
			if len(pair) != 2 {
				return nil, false
			}
		}
	}

	if document.Totals.Lines == nil || document.Totals.Rows == nil ||
		*document.Totals.Lines < 0 || *document.Totals.Rows < 0 ||
		document.Totals.Counts == nil || document.Totals.RowCounts == nil {
		return nil, false
	}
	for _, counts := range []map[string]int64{*document.Totals.Counts, *document.Totals.RowCounts} {
		for _, count := range counts {
			if count < 0 {
				return nil, false
			}
		}
	}

	tail, err := hex.DecodeString(*document.Tail)
	if err != nil {
		return nil, false
	}

	info, err := os.Stat(s.Paths.Log)
	if err != nil || info.Size() != size {
		return nil, false
	}
	if dev, inode, ok := fileIdentity(info); ok {
		if uint64((*document.Key)[1]) != dev || uint64((*document.Key)[2]) != inode {
			return nil, false
		}
	}
	signature, err := ledgerTail(s.Paths.Log, size)
	if err != nil || !bytes.Equal(signature, tail) {
		return nil, false
	}

	width := 2 + len(realms)
	segs := make([][]int64, 0, len(*document.Segs))
	previous := make([]int64, width)
	offset := int64(0)
	for _, raw := range *document.Segs {
		if len(raw) != width {
			return nil, false
		}
		for _, value := range raw {
			if value < 0 {
				return nil, false
			}
		}
		offset += raw[0]
		if offset > size {
			return nil, false
		}
		current := make([]int64, width)
		current[0] = offset
		for index := 1; index < width; index++ {
			current[index] = previous[index] + raw[index]
		}
		segs = append(segs, current)
		previous = current
	}

	return &recentIndex{
		size:      size,
		tail:      tail,
		legacy:    *document.Legacy,
		realms:    append([]string(nil), realms...),
		lines:     *document.Totals.Lines,
		rows:      *document.Totals.Rows,
		counts:    cloneCounts(*document.Totals.Counts),
		rowCounts: cloneCounts(*document.Totals.RowCounts),
		segs:      segs,
	}, true
}

func cloneCounts(source map[string]int64) map[string]int64 {
	cloned := make(map[string]int64, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func ledgerTail(path string, size int64) ([]byte, error) {
	if size <= 0 {
		return []byte{}, nil
	}
	start := size - recentIndexTailLength
	if start < 0 {
		start = 0
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	tail := make([]byte, size-start)
	if _, err := file.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return tail, nil
}

// fileIdentity extracts the device/inode fields v1 stores in its index key.
// The field names differ between platforms (and some platforms have no such
// fields), so a missing identity simply skips the inode half of the check;
// the size and tail signature still validate the file.
func fileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	if info == nil || info.Sys() == nil {
		return 0, 0, false
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, 0, false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, 0, false
	}
	device, okDevice := structUint(value, "Dev")
	inode, okInode := structUint(value, "Ino")
	if !okDevice || !okInode {
		return 0, 0, false
	}
	return device, inode, true
}

func structUint(value reflect.Value, name string) (uint64, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number := field.Int()
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	default:
		return 0, false
	}
}

func (s *Store) recentIndexPage(index *recentIndex, realm string, page, limit int) ([]map[string]any, bool) {
	column := 1
	totalRows := index.rows
	if realm != "" {
		position := -1
		for index, name := range index.realms {
			if name == realm {
				position = index
				break
			}
		}
		if position < 0 {
			return []map[string]any{}, true
		}
		column = 2 + position
		totalRows = index.rowCounts[realm]
	}
	if totalRows <= 0 {
		return []map[string]any{}, true
	}

	target := int64(page) * int64(limit)
	high := totalRows - int64(page-1)*int64(limit)
	if high < 1 {
		return []map[string]any{}, true
	}
	low := totalRows - target + 1
	if low < 1 {
		low = 1
	}

	left, right := 0, len(index.segs)
	for left < right {
		middle := (left + right) / 2
		if index.segs[middle][column] < low {
			left = middle + 1
		} else {
			right = middle
		}
	}
	start := int64(0)
	have := int64(0)
	if left > 0 {
		start = index.segs[left-1][0]
		have = index.segs[left-1][column]
	}
	skip := low - have - 1
	if skip < 0 {
		return nil, false
	}
	take := high - low + 1
	rows, ok := s.readIndexRows(start, realm, int(skip), int(take))
	if !ok {
		return nil, false
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows, true
}

// readIndexRows reads forward from a segment boundary and returns the
// requested slice in file order. Any disagreement with the index makes the
// caller fall back to the authoritative backward scan.
func (s *Store) readIndexRows(start int64, realm string, skip, take int) ([]map[string]any, bool) {
	file, err := os.Open(s.Paths.Log)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, false
	}

	reader := bufio.NewReaderSize(file, 64*1024)
	rows := make([]map[string]any, 0, take)
	seen := 0
	for len(rows) < take {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				row, ok := decodeRow(trimmed)
				if !ok {
					// Malformed lines are counted but not returned, exactly
					// like the backward scan.
				} else if realm == "" || s.rowRealm(row) == realm {
					if seen < skip {
						seen++
					} else {
						rows = append(rows, row)
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, false
		}
	}
	if len(rows) < take {
		return nil, false
	}
	return rows, true
}

func decodeRow(line []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var row map[string]any
	if err := decoder.Decode(&row); err != nil || row == nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return row, true
}

func (s *Store) rowRealm(row Row) string {
	return rowRealm(row, s.AccountRealm)
}
