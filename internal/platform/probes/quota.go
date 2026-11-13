package probes

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// QuotaRow is one watched account/model line.
type QuotaRow struct {
	UID        string  `json:"uid"`
	Model      string  `json:"model"`
	Realm      string  `json:"realm,omitempty"`
	LastScan   float64 `json:"last_scan"`
	NextAt     float64 `json:"next_at"`
	SeenAt     float64 `json:"seen_at"`
	Attempts   int     `json:"attempts"`
	LastTry    float64 `json:"last_try"`
	LastResult string  `json:"last_result"`
	LastDetail string  `json:"last_detail"`
}

// QuotaState persists accounts/quota_scan.json.
type QuotaState struct {
	Dir string
	Now func() time.Time

	mu    sync.Mutex
	scans map[string]QuotaRow
}

// NewQuotaState loads the state file, ignoring corruption.
func NewQuotaState(dir string) *QuotaState {
	state := &QuotaState{Dir: dir, Now: time.Now, scans: map[string]QuotaRow{}}
	state.load()
	return state
}

func (s *QuotaState) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *QuotaState) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.Dir, QuotaStateFile))
	if err != nil {
		return
	}
	now := resetTime(s.now())
	for _, row := range decodeQuotaRows(raw) {
		if row.UID == "" || row.Model == "" {
			continue
		}
		stamp := row.LastTry
		if stamp == 0 {
			stamp = row.SeenAt
		}
		if stamp > 0 && stamp < now-float64(QuotaMaxAge/time.Second) {
			continue
		}
		s.scans[realmWindowKey(row.Realm, row.UID, row.Model)] = row
	}
}

func decodeQuotaRows(raw []byte) []QuotaRow {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil
	}
	return decodeQuotaValue(value)
}

func decodeQuotaValue(value any) []QuotaRow {
	switch typed := value.(type) {
	case []any:
		out := []QuotaRow{}
		for _, item := range typed {
			if row, ok := decodeQuotaMap(item); ok {
				out = append(out, row)
			}
		}
		return out
	case map[string]any:
		if nested, ok := typed["scans"]; ok {
			return decodeQuotaValue(nested)
		}
		out := []QuotaRow{}
		for _, item := range typed {
			if row, ok := decodeQuotaMap(item); ok {
				out = append(out, row)
			}
		}
		return out
	default:
		return nil
	}
}

func decodeQuotaMap(value any) (QuotaRow, bool) {
	item, ok := value.(map[string]any)
	if !ok {
		return QuotaRow{}, false
	}
	row := QuotaRow{
		UID: probeString(item["uid"]), Model: probeString(item["model"]),
		LastScan: probeFloat(item["last_scan"]), NextAt: probeFloat(item["next_at"]),
		SeenAt: probeFloat(item["seen_at"]), Attempts: probeInt(item["attempts"]),
		LastTry: probeFloat(item["last_try"]), LastResult: probeString(item["last_result"]),
		LastDetail: probeString(item["last_detail"]),
		Realm:      probeString(item["realm"]),
	}
	return row, row.UID != "" && row.Model != ""
}

func (s *QuotaState) Ensure(uid, model string, now time.Time, delay time.Duration) bool {
	return s.EnsureRealm(uid, model, "", now, delay)
}

// EnsureRealm seeds one quota line with its provider realm.
func (s *QuotaState) EnsureRealm(uid, model, realm string, now time.Time, delay time.Duration) bool {
	if s == nil || uid == "" || model == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := realmWindowKey(realm, uid, model)
	if _, ok := s.scans[key]; ok {
		return false
	}
	s.scans[key] = QuotaRow{UID: uid, Model: model, Realm: realm, SeenAt: resetTime(now), NextAt: resetTime(now.Add(delay))}
	s.saveLocked()
	return true
}

func (s *QuotaState) Get(uid, model string) (QuotaRow, bool) {
	return s.GetRealm("", uid, model)
}

// GetRealm returns one realm-owned quota line.
func (s *QuotaState) GetRealm(realm, uid, model string) (QuotaRow, bool) {
	if s == nil {
		return QuotaRow{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.scans[realmWindowKey(realm, uid, model)]; ok {
		return row, true
	}
	if strings.TrimSpace(realm) != "" {
		return QuotaRow{}, false
	}
	for _, row := range s.scans {
		if row.UID == uid && row.Model == model {
			return row, true
		}
	}
	return QuotaRow{}, false
}

// DueAt is max(last_scan+interval, next_at).
func (s *QuotaState) DueAt(row QuotaRow) time.Time {
	if row.LastScan > 0 {
		return time.Unix(int64(row.LastScan+float64(QuotaInterval/time.Second)), 0)
	}
	return time.Unix(int64(row.NextAt), 0)
}

// Due returns due rows filtered by the watched set.
func (s *QuotaState) Due(watched map[string]bool, now time.Time, limit int, force bool) []QuotaRow {
	if s == nil {
		return nil
	}
	if limit <= 0 {
		limit = QuotaBatchLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []QuotaRow{}
	for _, row := range s.scans {
		if len(watched) > 0 && !watched[row.Model] {
			continue
		}
		if !force && now.Before(s.DueAt(row)) {
			continue
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := s.DueAt(out[i]), s.DueAt(out[j])
		if left.Equal(right) {
			if out[i].Model != out[j].Model {
				return out[i].Model < out[j].Model
			}
			return out[i].UID < out[j].UID
		}
		return left.Before(right)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Record writes one scan outcome for a realm-less line. lastScan zero means
// "not really scanned".
func (s *QuotaState) Record(uid, model string, now time.Time, lastScan bool, nextAt time.Time, attempts int, result, detail string) QuotaRow {
	return s.RecordRealm("", uid, model, now, lastScan, nextAt, attempts, result, detail)
}

// RecordRealm writes one provider-realm scan outcome.
func (s *QuotaState) RecordRealm(realm, uid, model string, now time.Time, lastScan bool, nextAt time.Time, attempts int, result, detail string) QuotaRow {
	if s == nil {
		return QuotaRow{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := realmWindowKey(realm, uid, model)
	row, ok := s.scans[key]
	if !ok {
		row = QuotaRow{UID: uid, Model: model, Realm: realm, SeenAt: resetTime(now)}
	}
	if lastScan {
		row.LastScan = resetTime(now)
	}
	row.NextAt = resetTime(nextAt)
	row.Attempts = attempts
	row.LastTry = resetTime(now)
	row.LastResult = truncate(result, 40)
	row.LastDetail = truncate(detail, 200)
	s.scans[key] = row
	s.saveLocked()
	return row
}

// Drop removes one realm-less row.
func (s *QuotaState) Drop(uid, model string) bool { return s.DropRealm("", uid, model) }

// DropRealm removes one realm-owned row.
func (s *QuotaState) DropRealm(realm, uid, model string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := realmWindowKey(realm, uid, model)
	if _, ok := s.scans[key]; !ok {
		return false
	}
	delete(s.scans, key)
	s.saveLocked()
	return true
}

// Snapshot returns panel rows sorted by due time.
func (s *QuotaState) Snapshot(watched map[string]bool, now time.Time, limit int) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	if limit <= 0 {
		limit = 20
	}
	s.mu.Lock()
	rows := make([]QuotaRow, 0, len(s.scans))
	for _, row := range s.scans {
		rows = append(rows, row)
	}
	s.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return s.DueAt(rows[i]).Before(s.DueAt(rows[j])) })
	out := []map[string]any{}
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		due := s.DueAt(row)
		dueIn := int(due.Sub(now).Seconds())
		if dueIn < 0 {
			dueIn = 0
		}
		out = append(out, map[string]any{
			"uid": row.UID, "uid8": first8(row.UID), "model": row.Model, "realm": row.Realm,
			"watched": watched[row.Model], "last_scan": row.LastScan,
			"last_result": row.LastResult, "last_detail": row.LastDetail,
			"due_in": dueIn,
		})
	}
	return out
}

func (s *QuotaState) saveLocked() {
	rows := make([]QuotaRow, 0, len(s.scans))
	for _, row := range s.scans {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].UID < rows[j].UID
	})
	payload := struct {
		Version      int        `json:"version"`
		SavedAt      string     `json:"saved_at"`
		IntervalHour float64    `json:"interval_hours"`
		Scans        []QuotaRow `json:"scans"`
	}{Version: StateVersion, SavedAt: s.now().Format("2006-01-02 15:04:05"), IntervalHour: 6, Scans: rows}
	_ = atomicWriteJSON(filepath.Join(s.Dir, QuotaStateFile), payload)
}
