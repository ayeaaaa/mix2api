// Package logs implements the runtime log ring, optional SQLite persistence,
// redaction helpers, and text export used by the gateway panel.
package logs

import (
	"errors"
	"strings"
	"time"
	"unicode"
)

const (
	// DefaultCapacity is the number of recent entries kept in memory. It
	// matches v1.6.5's LOG_BUFFER deque.
	DefaultCapacity = 2000

	// MaxMessageBytes bounds one stored message. It is a guard against a
	// pathological payload taking the database to its size cap in one row,
	// not a normal line-length limit.
	MaxMessageBytes = 64 * 1024

	// DefaultMaxDays is the panel's default age cap for logs.sqlite3.
	DefaultMaxDays = 7
)

// ErrClosed is returned when a closed store is used.
var ErrClosed = errors.New("logs: store closed")

// Entry is one runtime log line. The JSON names are the v1 panel contract.
type Entry struct {
	ID    int64  `json:"id"`
	TS    string `json:"ts"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Tag   string `json:"tag"`
	Msg   string `json:"msg"`
}

// Query is a panel log query. SinceID is the incremental-fetch cursor.
type Query struct {
	Limit   int
	Level   string
	Tag     string
	Search  string
	SinceID int64
}

// Result is the v1-compatible query result. Logs are ordered by ascending ID.
type Result struct {
	Total int     `json:"total"`
	Logs  []Entry `json:"logs"`
	MaxID int64   `json:"max_id"`
}

// Stats describes the durable store.
type Stats struct {
	Rows   int64  `json:"rows"`
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
	Bytes  int64  `json:"bytes"`
	Path   string `json:"path,omitempty"`
}

// Store is the runtime log sink.
type Store interface {
	Append(Entry) error
	Query(Query) (Result, error)
	Clear() error
	Stats() (Stats, error)
	Close() error
}

// Caps bounds the durable log database. A zero field means that axis is
// unlimited. Callers that want the v1 default use DefaultCaps.
type Caps struct {
	MaxRows  int64
	MaxDays  int64
	MaxBytes int64
}

// DefaultCaps returns the v1 defaults: a 7-day age cap and no row/byte cap.
func DefaultCaps() Caps {
	return Caps{MaxDays: DefaultMaxDays}
}

func (c Caps) normalized() Caps {
	if c.MaxRows < 0 {
		c.MaxRows = 0
	}
	if c.MaxDays < 0 {
		c.MaxDays = 0
	}
	if c.MaxBytes < 0 {
		c.MaxBytes = 0
	}
	return c
}

// logErrorHints and logTagHints mirror v1.6.5's add_log_entry inference. They
// are applied to the original text before redaction.
var logErrorHints = []string{
	"error", "exception", "failed", "fail:", "reject", "traceback", "errno", "fatal",
	"11-128", "11101", "11140",
	"失败", "异常", "错误", "拒绝", "不可用", "无效",
}

var logWarnHints = []string{
	"warn", "warning", "retry", "timeout", "throttl",
	"重试", "超时", "告警", "警告", "降级", "限流",
}

var logTagHints = []struct {
	tag   string
	hints []string
}{
	{"chat", []string{"chat:", "chat done", "request error", "/v1/chat", "/chat/completions", "responses"}},
	{"scheduler", []string{"scheduler", "调度器"}},
	{"tasks", []string{"task", "任务", "打卡", "签到", "猫猫", "travel", "growth"}},
	{"accounts", []string{"account", "账号", "pool", "imported"}},
	{"pricing", []string{"pricing", "定价", "取价", "价格", "price", "抓取"}},
	{"egress", []string{"egress", "出口", "槽位", "proxy slot", "mihomo"}},
	{"upstream", []string{"upstream", "上游"}},
	{"webtools", []string{"webtool", "web tool", "本地网络工具"}},
	{"catalog", []string{"model", "catalog", "模型"}},
	{"auth", []string{"auth", "token", "oauth"}},
	{"settings", []string{"settings", "设置"}},
	{"update", []string{"更新", "release"}},
	{"usage", []string{"usage", "用量"}},
	{"agents", []string{"agents", "智能体"}},
}

// InferLevel returns the v1 log level for a message when the caller did not
// provide one.
func InferLevel(message string) string {
	lower := strings.ToLower(message)
	for _, hint := range logErrorHints {
		if strings.Contains(lower, hint) {
			return "ERROR"
		}
	}
	for _, hint := range logWarnHints {
		if strings.Contains(lower, hint) {
			return "WARN"
		}
	}
	return "INFO"
}

// InferTag returns the v1 module tag for a message when the caller did not
// provide one.
func InferTag(message string) string {
	lower := strings.ToLower(message)
	for _, candidate := range logTagHints {
		for _, hint := range candidate.hints {
			if strings.Contains(lower, hint) {
				return candidate.tag
			}
		}
	}
	return "system"
}

func normalizeEntry(entry Entry, now time.Time) Entry {
	message := strings.TrimRightFunc(entry.Msg, unicode.IsSpace)
	level := strings.ToUpper(strings.TrimSpace(entry.Level))
	if level == "" {
		level = InferLevel(message)
	}
	tag := strings.ToLower(strings.TrimSpace(entry.Tag))
	if tag == "" {
		tag = InferTag(message)
	}
	if entry.TS == "" {
		entry.TS = now.Format("2006-01-02 15:04:05")
	}
	if entry.Time == "" {
		if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", entry.TS, now.Location()); err == nil {
			entry.Time = parsed.Format("15:04:05")
		} else {
			entry.Time = now.Format("15:04:05")
		}
	}
	entry.Level = level
	entry.Tag = tag
	entry.Msg = truncateMessage(RedactText(RedactQuery(message)))
	return entry
}

func truncateMessage(message string) string {
	if len(message) <= MaxMessageBytes {
		return message
	}
	cut := MaxMessageBytes
	for cut > 0 && (message[cut]&0xc0) == 0x80 {
		cut--
	}
	return message[:cut]
}
