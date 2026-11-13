package logs

import (
	"regexp"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
)

// The phone rules deliberately mirror v1.6.5:
//   - a value is masked only when the whole value is an 11-digit mobile number;
//   - text-level matches tolerate the common 3-4-4 separators and require
//     ASCII identifier boundaries so long order IDs, timestamps, tokens, and
//     account UIDs are not carved up.
var (
	phoneExact  = regexp.MustCompile(`^1[3-9]\d{9}$`)
	phoneInText = regexp.MustCompile(
		`1[3-9]\d[ \t\-.·]?(\d{4})[ \t\-.·]?(\d{4})`,
	)
	callbackLogSecret = regexp.MustCompile(
		`(?i)([?&](?:authorization_code|code|state|secret|ticket_id)=)[^&\s"']*`,
	)
)

// identityKeys carry identifiers, not display names. Rewriting one of these
// would make the panel send an ID the account pool cannot resolve.
var identityKeys = map[string]struct{}{
	"uid": {}, "account": {}, "accountId": {}, "id": {}, "file": {},
	"path": {}, "machineId": {}, "sessionId": {}, "key": {}, "token": {},
	"accessToken": {}, "refreshToken": {},
}

// RedactText masks phone numbers embedded in text and applies the shared
// core/secrets URL-userinfo rule. It is idempotent because neither mask matches
// its own output.
func RedactText(text string) string {
	return secrets.Redact(redactPhones(text))
}

func redactPhones(text string) string {
	matches := phoneInText.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text))
	last := 0
	changed := false
	for _, match := range matches {
		start, end := match[0], match[1]
		if !phoneBoundary(text, start, end) {
			continue
		}
		changed = true
		builder.WriteString(text[last:start])
		builder.WriteString(text[start : start+3])
		builder.WriteString("****")
		// Group 2 is the final four digits; the first four are group 1.
		builder.WriteString(text[match[4]:match[5]])
		last = end
	}
	if !changed {
		return text
	}
	builder.WriteString(text[last:])
	return builder.String()
}

func phoneBoundary(text string, start, end int) bool {
	if start > 0 && isIdentifierByte(text[start-1]) {
		return false
	}
	if end < len(text) && isIdentifierByte(text[end]) {
		return false
	}
	return true
}

func isIdentifierByte(value byte) bool {
	return value == '_' ||
		(value >= '0' && value <= '9') ||
		(value >= 'a' && value <= 'z') ||
		(value >= 'A' && value <= 'Z')
}

// RedactQuery masks OAuth callback credentials in a request log line.
func RedactQuery(text string) string {
	return callbackLogSecret.ReplaceAllString(text, "${1}<redacted>")
}

// RedactPanelJSON recursively redacts account display names with
// copy-on-write semantics. Phone-free branches keep their original identity so
// callers can safely pass shared TTL-cache objects.
func RedactPanelJSON(value any) any {
	out, _ := redactPanelValue(value)
	return out
}

func redactPanelValue(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		redacted := RedactText(typed)
		return redacted, redacted != typed
	case map[string]any:
		var out map[string]any
		for key, child := range typed {
			var replacement any
			var changed bool
			if key == "nickname" {
				replacement, changed = redactNickname(child)
			} else if _, identity := identityKeys[key]; identity {
				if _, isString := child.(string); isString {
					replacement, changed = child, false
				} else {
					replacement, changed = redactPanelValue(child)
				}
			} else {
				replacement, changed = redactPanelValue(child)
			}
			if !changed {
				continue
			}
			if out == nil {
				out = make(map[string]any, len(typed))
				for copyKey, copyValue := range typed {
					out[copyKey] = copyValue
				}
			}
			out[key] = replacement
		}
		if out == nil {
			return typed, false
		}
		return out, true
	case map[string]string:
		var out map[string]string
		for key, child := range typed {
			replacement, changed := child, false
			if key == "nickname" {
				if redacted, ok := redactNickname(child); ok {
					if text, isString := redacted.(string); isString {
						replacement = text
						changed = replacement != child
					}
				}
			} else if _, identity := identityKeys[key]; !identity {
				replacement = RedactText(child)
				changed = replacement != child
			}
			if !changed {
				continue
			}
			if out == nil {
				out = make(map[string]string, len(typed))
				for copyKey, copyValue := range typed {
					out[copyKey] = copyValue
				}
			}
			out[key] = replacement
		}
		if out == nil {
			return typed, false
		}
		return out, true
	case []any:
		var out []any
		for index, child := range typed {
			replacement, changed := redactPanelValue(child)
			if !changed {
				continue
			}
			if out == nil {
				out = append([]any(nil), typed...)
			}
			out[index] = replacement
		}
		if out == nil {
			return typed, false
		}
		return out, true
	case []string:
		var out []string
		for index, child := range typed {
			replacement := RedactText(child)
			if replacement == child {
				continue
			}
			if out == nil {
				out = append([]string(nil), typed...)
			}
			out[index] = replacement
		}
		if out == nil {
			return typed, false
		}
		return out, true
	case []map[string]any:
		var out []map[string]any
		for index, child := range typed {
			replacement, changed := redactPanelValue(child)
			if !changed {
				continue
			}
			if out == nil {
				out = append([]map[string]any(nil), typed...)
			}
			out[index] = replacement.(map[string]any)
		}
		if out == nil {
			return typed, false
		}
		return out, true
	default:
		return value, false
	}
}

func redactNickname(value any) (any, bool) {
	text, ok := value.(string)
	if !ok {
		return value, false
	}
	trimmed := strings.TrimSpace(text)
	if phoneExact.MatchString(trimmed) {
		return trimmed[:3] + "****" + trimmed[len(trimmed)-4:], true
	}
	redacted := RedactText(text)
	return redacted, redacted != text
}
