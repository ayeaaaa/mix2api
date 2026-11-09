package workbuddy

import (
	"encoding/json"
	"errors"
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
)

var activityLogMu sync.Mutex

var (
	activityBearerPattern   = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	activityKeyValuePattern = regexp.MustCompile(`(?i)\b(?:access[_-]?token|refresh[_-]?token|api[_-]?key|authorization|cookie|password|secret|token)\b\s*[:=]\s*\S+`)
	activityJWTPattern      = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\b`)
)

// safeActivityMessage applies the read-side projection's defensive redaction.
// Our writes already scrub account-specific values; this also protects rows
// inserted by an older build, a manual edit, or another writer.
func safeActivityMessage(text string) string {
	line := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	line = activityBearerPattern.ReplaceAllString(line, "[redacted]")
	line = activityKeyValuePattern.ReplaceAllString(line, "[redacted]")
	line = activityJWTPattern.ReplaceAllString(line, "[redacted]")
	return truncateText(line, 200)
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
	if p == nil || event == nil {
		return
	}
	path := p.activityHistoryPath()
	if path == "" {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	activityLogMu.Lock()
	defer activityLogMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
}

func (p *Provider) handleActivityHistory(w http.ResponseWriter, r *http.Request) {
	result, err := p.queryActivity(r.URL.Query())
	if err != nil {
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
		data, readErr := os.ReadFile(path)
		activityLogMu.Unlock()
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return nil, errors.New("activity history is unreadable")
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var row ActivityEvent
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				continue
			}
			row.Message = safeActivityMessage(row.Message)
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
