package agents

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var yamlKeyPattern = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]*)\s*:(?:\s|$)`)

func yamlIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func yamlKey(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
		return ""
	}
	match := yamlKeyPattern.FindStringSubmatch(strings.TrimLeft(line, " "))
	if match == nil {
		return ""
	}
	return match[1]
}

func yamlBlockEnd(lines []string, start, indent int) int {
	index := start + 1
	for index < len(lines) {
		line := lines[index]
		if strings.TrimSpace(line) != "" && yamlIndent(line) <= indent {
			break
		}
		index++
	}
	return index
}

func yamlInsertPoint(lines []string, start, end int) int {
	index := end
	for index > start && strings.TrimSpace(lines[index-1]) == "" {
		index--
	}
	if index > start {
		return index
	}
	return end
}

func yamlScalar(value any) string {
	switch typed := value.(type) {
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case int, int64, float64:
		return fmt.Sprint(typed)
	case nil:
		return "null"
	default:
		text := fmt.Sprint(value)
		if regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_ ./:@+()\[\]{}<>~-]*$`).MatchString(text) &&
			!strings.HasSuffix(text, " ") && !strings.Contains(text, ": ") && !strings.Contains(text, " #") {
			return text
		}
		raw, _ := json.Marshal(text)
		return string(raw)
	}
}

func yamlEmit(value any, indent int) []string {
	pad := strings.Repeat(" ", indent)
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sortStrings(keys)
		out := []string{}
		for _, key := range keys {
			item := typed[key]
			switch nested := item.(type) {
			case map[string]any:
				out = append(out, pad+yamlScalar(key)+":")
				out = append(out, yamlEmit(nested, indent+2)...)
			case []map[string]any, []any, []string:
				out = append(out, pad+yamlScalar(key)+":")
				out = append(out, yamlEmitList(nested, indent+2)...)
			default:
				out = append(out, pad+yamlScalar(key)+": "+yamlScalar(item))
			}
		}
		return out
	case []map[string]any:
		return yamlEmitList(typed, indent)
	case []any:
		return yamlEmitList(typed, indent)
	default:
		return []string{pad + yamlScalar(value)}
	}
}

func yamlEmitList(value any, indent int) []string {
	pad := strings.Repeat(" ", indent)
	out := []string{}
	var items []any
	switch typed := value.(type) {
	case []map[string]any:
		for _, item := range typed {
			items = append(items, item)
		}
	case []any:
		items = typed
	case []string:
		for _, item := range typed {
			items = append(items, item)
		}
	default:
		return []string{pad + yamlScalar(value)}
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			out = append(out, pad+"- "+yamlScalar(raw))
			continue
		}
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sortStrings(keys)
		first := true
		for _, key := range keys {
			prefix := pad + "  "
			if first {
				prefix = pad + "- "
				first = false
			}
			nested := item[key]
			switch value := nested.(type) {
			case map[string]any:
				out = append(out, prefix+yamlScalar(key)+":")
				out = append(out, yamlEmit(value, indent+4)...)
			case []map[string]any, []any, []string:
				out = append(out, prefix+yamlScalar(key)+":")
				out = append(out, yamlEmitList(value, indent+4)...)
			default:
				out = append(out, prefix+yamlScalar(key)+": "+yamlScalar(nested))
			}
		}
	}
	return out
}

func yamlInline(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// yamlUpsertInline writes a nested key as one inline JSON value.
func yamlUpsertInline(text, keyPath string, value any) (string, error) {
	keys := splitPath(keyPath)
	if len(keys) == 0 {
		return "", fmt.Errorf("yaml upsert: empty key path")
	}
	lines := strings.Split(text, "\n")
	lines = yamlUpsertAt(lines, 0, len(lines), 0, keys, yamlInline(value), nil)
	return strings.Join(lines, "\n"), nil
}

// yamlUpsertBlock writes a nested key as a block subtree.
func yamlUpsertBlock(text, keyPath string, value any) (string, error) {
	keys := splitPath(keyPath)
	if len(keys) == 0 {
		return "", fmt.Errorf("yaml upsert: empty key path")
	}
	lines := strings.Split(text, "\n")
	lines = yamlUpsertAt(lines, 0, len(lines), 0, keys, "", func(indent int) []string { return yamlEmit(value, indent) })
	return strings.Join(lines, "\n"), nil
}

func yamlUpsertAt(lines []string, start, end, indent int, keys []string, inline string, block func(int) []string) []string {
	key := keys[0]
	for index := start; index < end; index++ {
		line := lines[index]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") ||
			yamlIndent(line) != indent || yamlKey(line) != key {
			continue
		}
		after := ""
		if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
			after = strings.TrimSpace(parts[1])
		}
		childEnd := yamlBlockEnd(lines, index, indent)
		if len(keys) == 1 {
			if block != nil {
				replacement := []string{strings.Repeat(" ", indent) + key + ":"}
				replacement = append(replacement, block(indent+2)...)
				return replaceRange(lines, index, childEnd, replacement)
			}
			lines[index] = strings.Repeat(" ", indent) + key + ": " + inline
			return lines
		}
		if after != "" {
			expanded, err := yamlExpandInline(lines, index, key, after)
			if err != nil {
				return lines
			}
			lines = expanded
			childEnd = yamlBlockEnd(lines, index, indent)
		}
		return yamlUpsertAt(lines, index+1, childEnd, indent+2, keys[1:], inline, block)
	}
	insert := yamlInsertPoint(lines, start, end)
	var replacement []string
	if len(keys) == 1 {
		if block != nil {
			replacement = []string{strings.Repeat(" ", indent) + key + ":"}
			replacement = append(replacement, block(indent+2)...)
		} else {
			replacement = []string{strings.Repeat(" ", indent) + key + ": " + inline}
		}
	} else {
		replacement = []string{strings.Repeat(" ", indent) + key + ":"}
		replacement = append(replacement, yamlUpsertAt(nil, 0, 0, indent+2, keys[1:], inline, block)...)
	}
	return replaceRange(lines, insert, insert, replacement)
}

func yamlExpandInline(lines []string, index int, key, inline string) ([]string, error) {
	indent := yamlIndent(lines[index])
	value := strings.TrimSpace(inline)
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return lines, fmt.Errorf("cannot nest keys under %q", key)
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	replacement := []string{strings.Repeat(" ", indent) + key + ":"}
	if inner != "" {
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				replacement = append(replacement, strings.Repeat(" ", indent+2)+part)
			}
		}
	}
	return replaceRange(lines, index, index+1, replacement), nil
}

func replaceRange(lines []string, start, end int, replacement []string) []string {
	result := make([]string, 0, len(lines)-(end-start)+len(replacement))
	result = append(result, lines[:start]...)
	result = append(result, replacement...)
	result = append(result, lines[end:]...)
	return result
}

func splitPath(path string) []string {
	raw := strings.Split(path, ".")
	out := make([]string, 0, len(raw))
	for _, part := range raw {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// yamlRefsUpsert writes key: value inside a top-level refs block.
func yamlRefsUpsert(text, key, value string) (string, error) {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") ||
			yamlIndent(line) != 0 || yamlKey(line) != "refs" {
			continue
		}
		after := strings.TrimSpace(strings.TrimPrefix(line, "refs:"))
		if after != "" {
			return "", fmt.Errorf("cannot upsert into refs: %s", after)
		}
		end := yamlBlockEnd(lines, index, 0)
		body := append([]string(nil), lines[index+1:end]...)
		if updated, changed := yamlFlowMapLines(body, key, value); changed {
			lines = replaceRange(lines, index+1, end, updated)
			return strings.Join(lines, "\n"), nil
		}
		if len(body) > 0 && strings.HasPrefix(strings.TrimSpace(body[0]), "{") {
			close := -1
			for cursor, candidate := range body {
				trimmed := strings.TrimSpace(candidate)
				if trimmed == "}" || strings.HasSuffix(trimmed, "}") {
					close = cursor
					break
				}
			}
			if close >= 0 {
				indent := -1
				for cursor := close - 1; cursor >= 0; cursor-- {
					trimmed := strings.TrimSpace(body[cursor])
					if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "{") {
						indent = yamlIndent(body[cursor])
						break
					}
				}
				if indent < 0 {
					indent = yamlIndent(body[close]) + 2
				}
				for cursor := close - 1; cursor >= 0; cursor-- {
					trimmed := strings.TrimSpace(body[cursor])
					if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "{") {
						if !strings.HasSuffix(trimmed, ",") {
							body[cursor] = strings.TrimRight(body[cursor], " \t") + ","
						}
						break
					}
				}
				insert := []string{strings.Repeat(" ", indent) + key + ": " + value}
				body = replaceRange(body, close, close, insert)
				lines = replaceRange(lines, index+1, end, body)
				return strings.Join(lines, "\n"), nil
			}
		}
		for cursor := index + 1; cursor < end; cursor++ {
			if strings.TrimSpace(lines[cursor]) != "" && yamlIndent(lines[cursor]) == 2 && yamlKey(lines[cursor]) == key {
				lines[cursor] = "  " + key + ": " + value
				return strings.Join(lines, "\n"), nil
			}
		}
		insert := yamlInsertPoint(lines, index+1, end)
		return strings.Join(replaceRange(lines, insert, insert, []string{"  " + key + ": " + value}), "\n"), nil
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	lines = append(lines, "refs:", "  "+key+": "+value)
	return strings.Join(lines, "\n") + "\n", nil
}

// yamlFlowMapLines replaces key inside a multi-line flow mapping (DSH writes
// refs as "{ ... }"). It returns changed=true when the key was replaced; an
// absent key is inserted by the caller before the closing brace.
func yamlFlowMapLines(block []string, key, value string) ([]string, bool) {
	open, close := -1, -1
	for index, line := range block {
		trimmed := strings.TrimSpace(line)
		if open < 0 && strings.HasPrefix(trimmed, "{") {
			open = index
		}
		if trimmed == "}" || strings.HasSuffix(trimmed, "}") {
			close = index
			break
		}
	}
	if open < 0 || close < 0 {
		return block, false
	}
	for index := open + 1; index < close; index++ {
		trimmed := strings.TrimSpace(block[index])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || yamlKey(trimmed) != key {
			continue
		}
		entryEnd := index + 1
		for cursor := index + 1; cursor < close; cursor++ {
			next := strings.TrimSpace(block[cursor])
			if next != "" && !strings.HasPrefix(next, "#") && yamlKey(next) != "" {
				break
			}
			entryEnd = cursor + 1
		}
		last := strings.TrimSpace(block[entryEnd-1])
		hadComma := strings.HasSuffix(last, ",")
		suffix := ""
		if hadComma || entryEnd < close {
			suffix = ","
		}
		indent := yamlIndent(block[index])
		replacement := []string{strings.Repeat(" ", indent) + key + ": " + value + suffix}
		return replaceRange(block, index, entryEnd, replacement), true
	}
	return block, false
}

// dshPatchUpsert updates only the two entries this integration owns, using
// nested upserts so unrelated keys inside those entries survive.
func dshPatchUpsert(text string, provider map[string]any, model string) string {
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "[]" {
			continue
		}
		filtered = append(filtered, line)
	}
	lines = filtered
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	if start, end, ok := dshEntryRange(lines, "llm-pi-ai"); ok {
		lines = yamlUpsertAt(lines, start+1, end, 2, []string{"config", "providers", ProviderID}, "",
			func(indent int) []string { return yamlEmit(provider, indent) })
	} else {
		entry := []string{"- id: llm-pi-ai", "  name: @deepseek-ai/dsh-llm-pi-ai", "  config:", "    providers:"}
		entry = append(entry, yamlEmit(map[string]any{ProviderID: provider}, 6)...)
		lines = append(lines, entry...)
	}
	if model != "" {
		if start, end, ok := dshEntryRange(lines, "agent-default-model"); ok {
			lines = yamlUpsertAt(lines, start+1, end, 2, []string{"config", "provider"}, yamlInline(ProviderID), nil)
			if start, end, ok = dshEntryRange(lines, "agent-default-model"); ok {
				lines = yamlUpsertAt(lines, start+1, end, 2, []string{"config", "model"}, yamlInline(model), nil)
			}
		} else {
			lines = append(lines,
				"- id: agent-default-model", "  config:",
				"    provider: "+yamlScalar(ProviderID),
				"    model: "+yamlScalar(model),
			)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func dshEntryRange(lines []string, entryID string) (int, int, bool) {
	starts := []int{}
	for index, line := range lines {
		if strings.HasPrefix(line, "- ") || strings.TrimSpace(line) == "-" {
			starts = append(starts, index)
		}
	}
	for position, start := range starts {
		end := len(lines)
		if position+1 < len(starts) {
			end = starts[position+1]
		}
		id := ""
		if strings.HasPrefix(strings.TrimSpace(lines[start]), "- id:") {
			id = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[start]), "- id:"))
		} else {
			for cursor := start + 1; cursor < end; cursor++ {
				if yamlIndent(lines[cursor]) == 2 && yamlKey(lines[cursor]) == "id" {
					id = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[cursor]), "id:"))
					break
				}
			}
		}
		if strings.Trim(id, `"'`) == entryID {
			return start, end, true
		}
	}
	return 0, 0, false
}

func dshDefaultModelMerge(text, provider, model string) map[string]any {
	out := map[string]any{"provider": provider, "model": model}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "agent-default-model:") {
			continue
		}
		raw := strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		if strings.HasPrefix(raw, "{") {
			var current map[string]any
			if json.Unmarshal([]byte(raw), &current) == nil && current != nil {
				out = current
			}
		}
		break
	}
	out["provider"] = provider
	out["model"] = model
	return out
}
