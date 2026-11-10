package keys

import (
	"strings"
)

// NormalizeModelPatterns exposes v1's model allow-list normalization. The
// result is lower-cased, comma/semicolon/newline split, de-duplicated, and
// keeps the original order.
func NormalizeModelPatterns(value any) []string {
	return normalizeModelPatterns(value)
}

func normalizeModelPatterns(value any) []string {
	var raw []string
	switch typed := value.(type) {
	case string:
		raw = splitPatterns(typed)
	case []string:
		raw = typed
	case []any:
		raw = make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				raw = append(raw, text)
			}
		}
	default:
		return []string{}
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		pattern := strings.ToLower(strings.TrimSpace(item))
		if pattern == "" || seen[pattern] {
			continue
		}
		seen[pattern] = true
		out = append(out, pattern)
	}
	return out
}

func splitPatterns(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
}

// NormalizeAccountRefs exposes v1's account allow-list normalization: exact
// ids, de-duplicated, at most 200 refs of at most 128 bytes each.
func NormalizeAccountRefs(value any) []string {
	return normalizeAccountRefs(value)
}

func normalizeAccountRefs(value any) []string {
	const (
		maxAccounts = 200
		maxLength   = 128
	)
	var raw []string
	switch typed := value.(type) {
	case string:
		raw = splitPatterns(typed)
	case []string:
		raw = typed
	case []any:
		raw = make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				raw = append(raw, text)
			}
		}
	default:
		return []string{}
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		ref := strings.TrimSpace(item)
		if ref == "" || len([]byte(ref)) > maxLength || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
		if len(out) >= maxAccounts {
			break
		}
	}
	return out
}

// AllowsModel reports whether a key may use model. An empty list is
// unrestricted; a restricted key with an empty model is denied.
func AllowsModel(entry Entry, model string) bool {
	patterns := normalizeModelPatterns(entry.Models)
	if len(patterns) == 0 {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	for _, pattern := range patterns {
		if fnmatch(pattern, name) {
			return true
		}
	}
	return false
}

// AllowsAccount reports whether a key may use account uid. Matching is exact;
// an empty list is unrestricted.
func AllowsAccount(entry Entry, uid string) bool {
	refs := normalizeAccountRefs(entry.Accounts)
	if len(refs) == 0 {
		return true
	}
	uid = strings.TrimSpace(uid)
	for _, ref := range refs {
		if ref == uid {
			return true
		}
	}
	return false
}

// BoundRealm returns the entry's normalized realm binding, or "" when the key
// follows the panel-wide exit.
func BoundRealm(entry *Entry) string {
	if entry == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(entry.Realm))
}

// patternToken is one fnmatch element. A class token consumes exactly one
// character, which lets the DP treat it like a literal or '?'.
type patternToken struct {
	kind  rune // 'l' literal, '?' any, '*' star, 'c' class
	value rune
	class []rune
}

// fnmatch implements shell-style matching where '*' spans path separators too,
// matching Python's fnmatchcase rather than path.Match. Both inputs are already
// lower-cased by the callers.
func fnmatch(pattern, name string) bool {
	tokens := tokenizePattern([]rune(pattern))
	runes := []rune(name)
	rows := len(runes) + 1
	cols := len(tokens) + 1
	table := make([]bool, rows*cols)
	table[0] = true
	for col := 1; col < cols; col++ {
		if tokens[col-1].kind == '*' {
			table[col] = table[col-1]
		}
	}
	for row := 1; row < rows; row++ {
		for col := 1; col < cols; col++ {
			token := tokens[col-1]
			index := row*cols + col
			switch token.kind {
			case '*':
				table[index] = table[(row-1)*cols+col] || table[row*cols+col-1]
			case '?':
				table[index] = table[(row-1)*cols+col-1]
			case 'c':
				if matchClass(token.class, runes[row-1]) {
					table[index] = table[(row-1)*cols+col-1]
				}
			default:
				table[index] = token.value == runes[row-1] && table[(row-1)*cols+col-1]
			}
		}
	}
	return table[len(table)-1]
}

func tokenizePattern(pattern []rune) []patternToken {
	tokens := make([]patternToken, 0, len(pattern))
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			tokens = append(tokens, patternToken{kind: '*'})
		case '?':
			tokens = append(tokens, patternToken{kind: '?'})
		case '[':
			if end, ok := bracketEnd(pattern, index); ok {
				tokens = append(tokens, patternToken{kind: 'c', class: append([]rune(nil), pattern[index+1:end]...)})
				index = end
				continue
			}
			tokens = append(tokens, patternToken{kind: 'l', value: '['})
		default:
			tokens = append(tokens, patternToken{kind: 'l', value: pattern[index]})
		}
	}
	return tokens
}

// bracketEnd returns the index of the closing ']' for a class starting at
// start, or false when the bracket is literal (unterminated).
func bracketEnd(pattern []rune, start int) (int, bool) {
	if start >= len(pattern) || pattern[start] != '[' {
		return 0, false
	}
	index := start + 1
	if index < len(pattern) && (pattern[index] == '!' || pattern[index] == '^') {
		index++
	}
	if index < len(pattern) && pattern[index] == ']' {
		index++
	}
	for ; index < len(pattern); index++ {
		if pattern[index] == ']' {
			return index, true
		}
	}
	return 0, false
}

func matchClass(class []rune, value rune) bool {
	negate := false
	if len(class) > 0 && (class[0] == '!' || class[0] == '^') {
		negate = true
		class = class[1:]
	}
	matched := false
	for index := 0; index < len(class); index++ {
		if index+2 < len(class) && class[index+1] == '-' {
			if value >= class[index] && value <= class[index+2] {
				matched = true
			}
			index += 2
			continue
		}
		if class[index] == value {
			matched = true
		}
	}
	if negate {
		return !matched
	}
	return matched
}
