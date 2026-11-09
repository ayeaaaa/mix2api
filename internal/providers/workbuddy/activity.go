package workbuddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	activityHistoryFile  = "activity_history.jsonl"
	activityDefaultLimit = 100
	activityMaxLimit     = 1000
	activityMaxRecords   = 5000
	activityMaxAgeDays   = 90
	activityCompactBytes = 2_000_000
)

// ActivityHistoryFile is the shared WorkBuddy activity-log filename. App
// composition points both realm providers at one file under the accounts
// directory so the intl and cn scheduler histories stay unified.
const ActivityHistoryFile = activityHistoryFile

var (
	activityLogMu   sync.Mutex
	errActivityRead = errors.New("activity history is unreadable")
)

var (
	activityBearerPattern   = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	activityKeyValuePattern = regexp.MustCompile(`(?i)\b(?:access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|authorization|cookie|set-cookie|password|secret|token)\b\s*[:=]\s*\S+`)
	activityJWTPattern      = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\b`)
	activitySKPattern       = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`)
	activityHexPattern      = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)
	activityBase64Pattern   = regexp.MustCompile(`\b[A-Za-z0-9+/]{40,}={0,2}\b`)
)

// safeActivityMessage applies the read-side projection's defensive redaction.
// Our writes already scrub account-specific values; this also protects rows
// inserted by an older build, a manual edit, or another writer.
func safeActivityMessage(text string) string {
	line := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	for _, pattern := range []*regexp.Regexp{
		activityBearerPattern, activityKeyValuePattern, activityJWTPattern,
		activitySKPattern, activityHexPattern, activityBase64Pattern,
	} {
		line = pattern.ReplaceAllString(line, "[redacted]")
	}
	return truncateText(line, 200)
}

func projectActivityEvent(row ActivityEvent) ActivityEvent {
	row.TS = truncateText(strings.TrimSpace(row.TS), 40)
	row.UID = truncateText(strings.TrimSpace(row.UID), 80)
	row.Nickname = truncateText(strings.TrimSpace(row.Nickname), 120)
	if row.Realm != realmIntl && row.Realm != realmCN && row.Realm != "raccoon" {
		row.Realm = ""
	}
	if row.Task != taskCheckinTask && row.Task != taskDailyChatTask {
		row.Task = ""
	}
	switch row.Trigger {
	case taskTriggerManual, taskTriggerScheduler, "account_add", "account_import":
	default:
		row.Trigger = "unknown"
	}
	row.Message = safeActivityMessage(row.Message)
	return row
}

// SetActivityPath overrides the activity_history.jsonl path (by default it is
// next to the account files). App composition may point both realm providers at
// the shared usage directory.
func (p *Provider) SetActivityPath(path string) {
	if p == nil {
		return
	}
	p.activityPath = strings.TrimSpace(path)
}

func (p *Provider) activityHistoryPath() string {
	if p == nil {
		return ""
	}
	if path := strings.TrimSpace(p.activityPath); path != "" {
		return path
	}
	dir := strings.TrimSpace(p.AccountsDir())
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, activityHistoryFile)
}

// recordActivity appends one already-redacted task event. History is an
// operational record, never a credential channel: ActivityEvent only contains
// timestamp/uid/nickname/realm/task/trigger/result/message. Write errors are
// deliberately ignored so a full disk cannot turn a successful check-in into
// a failed one.
func (p *Provider) recordActivity(event *ActivityEvent) {
	p.recordActivityEvent(event, false)
}

// recordDailyActivity keeps one daily-chat row per account/day: a later success
// replaces the same day's earlier failure row instead of leaving both the
// failed and successful attempt in the panel history.
func (p *Provider) recordDailyActivity(event *ActivityEvent) {
	p.recordActivityEvent(event, true)
}

func (p *Provider) recordActivityEvent(event *ActivityEvent, replaceFailure bool) {
	if p == nil || event == nil {
		return
	}
	path := p.activityHistoryPath()
	if path == "" {
		return
	}
	projected := projectActivityEvent(*event)
	data, err := json.Marshal(projected)
	if err != nil {
		return
	}
	activityLogMu.Lock()
	defer activityLogMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if replaceFailure && projected.Task == taskDailyChatTask {
		rows, readErr := readActivityRowsLocked(path)
		if readErr != nil {
			return
		}
		for index := len(rows) - 1; index >= 0; index-- {
			row := rows[index]
			if row.UID != projected.UID || row.Task != projected.Task ||
				row.Realm != projected.Realm || activityDay(row.TS) != activityDay(projected.TS) {
				continue
			}
			if row.OK && !projected.OK {
				// The day already has its preferred successful outcome; a later
				// failure must not add a second row or replace it.
				return
			}
			rows[index] = projected
			writeActivityRowsLocked(path, rows)
			return
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
	if info, err := os.Stat(path); err == nil && info.Size() > activityCompactBytes {
		if rows, readErr := readActivityRowsLocked(path); readErr == nil {
			rows, changed := retainedActivityRows(rows, time.Now())
			if changed {
				writeActivityRowsLocked(path, rows)
			}
		}
	}
}

func activityDay(stamp string) string {
	stamp = strings.TrimSpace(stamp)
	if index := strings.IndexByte(stamp, 'T'); index > 0 {
		return stamp[:index]
	}
	if len(stamp) >= 10 {
		return stamp[:10]
	}
	return stamp
}

func readActivityRowsLocked(path string) ([]ActivityEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []ActivityEvent{}, nil
		}
		return nil, errActivityRead
	}
	rows := make([]ActivityEvent, 0)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row ActivityEvent
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		rows = append(rows, projectActivityEvent(row))
	}
	return rows, nil
}

func retainedActivityRows(rows []ActivityEvent, now time.Time) ([]ActivityEvent, bool) {
	cutoff := now.Add(-activityMaxAgeDays * 24 * time.Hour)
	kept := make([]ActivityEvent, 0, len(rows))
	changed := false
	for _, row := range rows {
		stamp, err := time.Parse(time.RFC3339, row.TS)
		if err == nil && stamp.Before(cutoff) {
			changed = true
			continue
		}
		kept = append(kept, row)
	}
	if len(kept) > activityMaxRecords {
		kept = kept[len(kept)-activityMaxRecords:]
		changed = true
	}
	return kept, changed
}

func writeActivityRowsLocked(path string, rows []ActivityEvent) {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil {
			continue
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return
		}
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, path)
}

func (p *Provider) handleActivityHistory(w http.ResponseWriter, r *http.Request) {
	result, err := p.queryActivity(r.URL.Query())
	if err != nil {
		if errors.Is(err, errActivityRead) {
			writeWBAdminError(w, wbServerError("activity history is unreadable"))
			return
		}
		writeWBAdminError(w, wbBadRequest(err.Error()))
		return
	}
	writeWBAdminJSON(w, http.StatusOK, result)
}

func (p *Provider) queryActivity(query map[string][]string) (map[string]any, error) {
	first := func(name string) string {
		values := query[name]
		if len(values) == 0 {
			return ""
		}
		return strings.TrimSpace(values[0])
	}
	rangeKey := strings.ToLower(first("range"))
	if rangeKey == "" {
		rangeKey = "7d"
	}
	cutoff, err := activityRangeCutoff(rangeKey, time.Now())
	if err != nil {
		return nil, err
	}
	task := strings.ToLower(first("task"))
	if task != "" && task != taskCheckinTask && task != taskDailyChatTask {
		return nil, errors.New("task must be one of: checkin, daily_chat")
	}
	resultFilter := strings.ToLower(first("result"))
	if resultFilter != "" {
		switch resultFilter {
		case "ok", "success", "succeeded":
			resultFilter = "ok"
		case "failed", "fail", "failure":
			resultFilter = "failed"
		default:
			return nil, errors.New("result must be one of: ok, failed")
		}
	}
	uid := first("uid")
	limit := activityDefaultLimit
	if raw := first("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > activityMaxLimit {
		limit = activityMaxLimit
	}

	rows := []ActivityEvent{}
	path := p.activityHistoryPath()
	if path != "" {
		activityLogMu.Lock()
		allRows, readErr := readActivityRowsLocked(path)
		if readErr != nil {
			activityLogMu.Unlock()
			return nil, fmt.Errorf("%w: %v", errActivityRead, readErr)
		}
		retained, changed := retainedActivityRows(allRows, time.Now())
		if changed {
			writeActivityRowsLocked(path, retained)
		}
		activityLogMu.Unlock()
		for _, row := range retained {
			if uid != "" && row.UID != uid {
				continue
			}
			if task != "" && row.Task != task {
				continue
			}
			if resultFilter == "ok" && !row.OK {
				continue
			}
			if resultFilter == "failed" && row.OK {
				continue
			}
			if cutoff != nil {
				stamp, err := time.Parse(time.RFC3339, row.TS)
				if err != nil || stamp.Before(*cutoff) {
					continue
				}
			}
			rows = append(rows, row)
		}
	}
	total := len(rows)
	// The file is append-ordered, so latest first is the reverse of read order.
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return map[string]any{
		"rows": rows, "total": total, "limit": limit, "range": rangeKey,
		"filters": map[string]any{"uid": uid, "task": task, "result": resultFilter},
	}, nil
}

func activityRangeCutoff(rangeKey string, now time.Time) (*time.Time, error) {
	switch rangeKey {
	case "today":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		return &start, nil
	case "1d":
		value := now.Add(-24 * time.Hour)
		return &value, nil
	case "7d":
		value := now.Add(-7 * 24 * time.Hour)
		return &value, nil
	case "30d":
		value := now.Add(-30 * 24 * time.Hour)
		return &value, nil
	case "90d":
		value := now.Add(-90 * 24 * time.Hour)
		return &value, nil
	case "all":
		return nil, nil
	default:
		return nil, errors.New("range must be one of: today, 1d, 7d, 30d, 90d, all")
	}
}
