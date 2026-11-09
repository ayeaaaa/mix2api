package usage

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Row is one usage ledger record. It stays a map so unknown fields survive
// read/modify/write paths; decoders use json.Number for numeric fidelity.
type Row map[string]any

// RowAt returns the row's epoch-seconds timestamp. The boolean is false when
// the field is missing or cannot be read as a number.
func RowAt(row Row) (float64, bool) {
	value, ok := row["at"]
	if !ok {
		return 0, false
	}
	return numericValue(value)
}

// RowISO returns the v1 display timestamp (YYYY-MM-DDTHH:MM:SS).
func RowISO(row Row) string { return stringValue(row, "iso") }

// RowModel returns the billed model id.
func RowModel(row Row) string { return stringValue(row, "model") }

// RowStream reports whether the request used streaming.
func RowStream(row Row) bool { return truthy(row["stream"]) }

// RowAccount returns the account uid a row is attributed to.
func RowAccount(row Row) string { return stringValue(row, "account") }

// RowKey returns the settings id of the client API key that paid for a row.
func RowKey(row Row) string { return stringValue(row, "key") }

// RowOutcome mirrors v1 row_outcome: explicit outcomes win; rows written
// before the field existed fall back to failed when an error is present and
// completed otherwise.
func RowOutcome(row Row) string {
	if value, ok := row["outcome"]; ok && truthy(value) {
		if text, ok := value.(string); ok {
			return text
		}
		return fmt.Sprint(value)
	}
	if truthy(row["error"]) {
		return "failed"
	}
	return "completed"
}

// RowKind returns the request kind; rows written before the field existed are
// chat rows.
func RowKind(row Row) string {
	value, ok := row["kind"]
	if !ok {
		return "chat"
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "chat"
	}
	return text
}

// RowRealm mirrors v1 row_realm when no live account pool is available.
// Explicit rows carry the field; older rows fall back to the model's home
// realm, then to intl.
func RowRealm(row Row) string { return rowRealm(row, nil) }

func rowRealm(row Row, accountRealm func(string) string) string {
	if value, ok := row["realm"]; ok && truthy(value) {
		if text, ok := value.(string); ok {
			return text
		}
		// v1 returns a hand-edited non-string realm unchanged; stringifying it
		// keeps it from accidentally matching a normal realm filter.
		return fmt.Sprint(value)
	}
	if accountRealm != nil {
		if account := RowAccount(row); account != "" {
			if realm := accountRealm(account); realm != "" {
				return realm
			}
		}
	}
	if model := RowModel(row); model != "" {
		return DetectModelRealm(model)
	}
	return "intl"
}

var intlOnlyModels = map[string]struct{}{
	"gpt-6-astra":      {},
	"gpt-6.1-sol":      {},
	"gpt-6-sol":        {},
	"gpt-6-luna":       {},
	"gpt-5.6-sol":      {},
	"gpt-5.6-terra":    {},
	"gpt-5.6-luna":     {},
	"gemini-3.8-flash": {},
	"gemini-3.5-flash": {},
	"grok-4.7":         {},
}

var cnOnlyModels = map[string]struct{}{
	"deepseek-v4-pro":  {},
	"minimax-m3":       {},
	"minimax-m2.7":     {},
	"minimax-m2.5":     {},
	"glm-5.1":          {},
	"glm-5.0-turbo":    {},
	"glm-4.6v":         {},
	"kimi-k3-1":        {},
	"kimi-k2.7":        {},
	"kimi-k2-thinking": {},
	"hy3-x":            {},
	"hy4-preview-dev":  {},
	"hy4-preview-x":    {},
}

// DetectModelRealm returns the v1 home-realm guess for a model that has an
// exclusive exit. Models served by both exits fall back to intl, which is the
// v1 default realm.
func DetectModelRealm(model string) string {
	m := strings.ToLower(model)
	if m == "" {
		return "intl"
	}

	if _, ok := intlOnlyModels[m]; ok ||
		strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "gemini-") {
		return "intl"
	}

	if _, ok := cnOnlyModels[m]; ok ||
		strings.HasPrefix(m, "minimax-") || strings.HasPrefix(m, "deepseek-v4-pro") {
		return "cn"
	}
	return "intl"
}

func stringValue(row Row, key string) string {
	value, ok := row[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number != 0
	case float64:
		return typed != 0
	case float32:
		return typed != 0
	case int:
		return typed != 0
	case int8:
		return typed != 0
	case int16:
		return typed != 0
	case int32:
		return typed != 0
	case int64:
		return typed != 0
	case uint:
		return typed != 0
	case uint8:
		return typed != 0
	case uint16:
		return typed != 0
	case uint32:
		return typed != 0
	case uint64:
		return typed != 0
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}
