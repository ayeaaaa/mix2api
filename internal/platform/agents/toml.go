package agents

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	tomlTablePattern = regexp.MustCompile(`^\s*\[([A-Za-z0-9_.-]+)\]\s*(?:#.*)?$`)
	tomlKeyPattern   = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=`)
)

func tomlValue(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		raw, _ := json.Marshal(typed)
		return string(raw), nil
	case bool:
		return fmt.Sprintf("%t", typed), nil
	case nil:
		return "", fmt.Errorf("TOML has no null value")
	default:
		raw, _ := json.Marshal(typed)
		return string(raw), nil
	}
}

func tomlUpsertTopKey(text, key string, value any) (string, error) {
	rendered, err := tomlValue(value)
	if err != nil {
		return "", err
	}
	line := key + " = " + rendered
	lines := strings.Split(text, "\n")
	lastTop := -1
	for index, current := range lines {
		if tomlTablePattern.MatchString(current) {
			break
		}
		match := tomlKeyPattern.FindStringSubmatch(current)
		if match == nil {
			continue
		}
		lastTop = index
		if match[1] == key {
			lines[index] = line
			return strings.Join(lines, "\n"), nil
		}
	}
	insert := lastTop + 1
	lines = append(lines, "")
	copy(lines[insert+1:], lines[insert:])
	lines[insert] = line
	return strings.Join(lines, "\n"), nil
}

func tomlUpsertTable(text, table string, values map[string]any) (string, error) {
	lines := strings.Split(text, "\n")
	for index, current := range lines {
		match := tomlTablePattern.FindStringSubmatch(current)
		if match == nil || match[1] != table {
			continue
		}
		end := index + 1
		for end < len(lines) && !tomlTablePattern.MatchString(lines[end]) {
			end++
		}
		return strings.Join(tomlUpsertRange(lines, index+1, end, values), "\n"), nil
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, "["+table+"]")
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// Keep deterministic order for stable diffs.
	sortStrings(keys)
	for _, key := range keys {
		rendered, err := tomlValue(values[key])
		if err != nil {
			return "", err
		}
		lines = append(lines, key+" = "+rendered)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func tomlUpsertRange(lines []string, start, end int, values map[string]any) []string {
	remaining := map[string]any{}
	for key, value := range values {
		remaining[key] = value
	}
	last := start
	for index := start; index < end; index++ {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		last = index + 1
		match := tomlKeyPattern.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		if value, ok := remaining[match[1]]; ok {
			rendered, err := tomlValue(value)
			if err == nil {
				lines[index] = match[1] + " = " + rendered
			}
			delete(remaining, match[1])
		}
	}
	keys := make([]string, 0, len(remaining))
	for key := range remaining {
		keys = append(keys, key)
	}
	sortStrings(keys)
	for _, key := range keys {
		rendered, err := tomlValue(remaining[key])
		if err != nil {
			continue
		}
		lines = append(lines, "")
		copy(lines[last+1:], lines[last:])
		lines[last] = key + " = " + rendered
		last++
	}
	return lines
}

func tomlDropTableKeys(text, table string, drop []string) string {
	wanted := map[string]bool{}
	for _, key := range drop {
		wanted[key] = true
	}
	lines := strings.Split(text, "\n")
	for index, current := range lines {
		match := tomlTablePattern.FindStringSubmatch(current)
		if match == nil || match[1] != table {
			continue
		}
		end := index + 1
		for end < len(lines) && !tomlTablePattern.MatchString(lines[end]) {
			end++
		}
		kept := []string{lines[index]}
		for _, line := range lines[index+1 : end] {
			keyMatch := tomlKeyPattern.FindStringSubmatch(line)
			if keyMatch != nil && wanted[keyMatch[1]] {
				continue
			}
			kept = append(kept, line)
		}
		result := append([]string(nil), lines[:index]...)
		result = append(result, kept...)
		result = append(result, lines[end:]...)
		return strings.Join(result, "\n")
	}
	return text
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
