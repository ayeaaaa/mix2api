package workbuddy

import "strings"

// kimiSchemaContainsRef reports whether a schema contains a local $ref. Literal
// data under default/examples/example/enum/const is not a schema and must not
// trigger expansion.
func kimiSchemaContainsRef(node any) bool {
	switch typed := node.(type) {
	case map[string]any:
		if _, ok := typed["$ref"]; ok {
			return true
		}
		for key, value := range typed {
			switch key {
			case "default", "examples", "example", "enum", "const":
				continue
			}
			if kimiSchemaContainsRef(value) {
				return true
			}
		}
	case []any:
		for _, value := range typed {
			if kimiSchemaContainsRef(value) {
				return true
			}
		}
	}
	return false
}

func inlineKimiSchemaNode(node any, defs map[string]any, seen map[string]bool, depth int, budget *int) any {
	if depth > 64 || *budget <= 0 {
		return map[string]any{}
	}
	*budget--
	if list, ok := node.([]any); ok {
		out := make([]any, 0, len(list))
		for _, value := range list {
			expanded := inlineKimiSchemaNode(value, defs, seen, depth+1, budget)
			if *budget < 0 {
				return map[string]any{}
			}
			out = append(out, expanded)
		}
		return out
	}
	object, ok := node.(map[string]any)
	if !ok {
		return node
	}
	if rawRef, ok := object["$ref"].(string); ok {
		name := ""
		for _, prefix := range []string{"#/$defs/", "#/definitions/"} {
			if strings.HasPrefix(rawRef, prefix) {
				name = strings.TrimPrefix(rawRef, prefix)
				name = strings.ReplaceAll(name, "~1", "/")
				name = strings.ReplaceAll(name, "~0", "~")
				break
			}
		}
		if name == "" {
			return cloneMap(object)
		}
		if seen == nil {
			seen = map[string]bool{}
		}
		if _, exists := defs[name]; !exists {
			return map[string]any{"type": "object"}
		}
		if seen[name] {
			return map[string]any{}
		}
		active := make(map[string]bool, len(seen)+1)
		for key, value := range seen {
			active[key] = value
		}
		active[name] = true
		expanded := inlineKimiSchemaNode(defs[name], defs, active, depth+1, budget)
		if *budget < 0 {
			return map[string]any{}
		}
		siblings := map[string]any{}
		root := depth == 0
		for key, value := range object {
			if key == "$ref" || (root && (key == "$defs" || key == "definitions" || key == "$schema")) {
				continue
			}
			child := inlineKimiSchemaNode(value, defs, active, depth+1, budget)
			if *budget < 0 {
				return map[string]any{}
			}
			siblings[key] = child
		}
		if expandedMap, ok := expanded.(map[string]any); ok && len(siblings) > 0 {
			merged := cloneMap(expandedMap)
			for key, value := range siblings {
				merged[key] = value
			}
			return merged
		}
		return expanded
	}
	out := map[string]any{}
	root := depth == 0
	for key, value := range object {
		if root && (key == "$defs" || key == "definitions" || key == "$schema") {
			continue
		}
		if key == "default" || key == "examples" || key == "example" || key == "enum" || key == "const" {
			out[key] = value
			continue
		}
		child := inlineKimiSchemaNode(value, defs, seen, depth+1, budget)
		if *budget < 0 {
			return map[string]any{}
		}
		out[key] = child
	}
	return out
}

// inlineKimiToolSchemas inlines local $ref/$defs for CN Kimi tool declarations.
// It runs after normalizeChatTools and mutates a private request body.
func inlineKimiToolSchemas(body map[string]any, model string) {
	if body == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(model))
	name = strings.TrimPrefix(name, "cn:")
	if !strings.HasPrefix(name, "kimi") {
		return
	}
	tools, ok := body["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		parameters, ok := function["parameters"].(map[string]any)
		if !ok {
			continue
		}
		if _, hasDefs := parameters["$defs"]; !hasDefs {
			if _, hasDefinitions := parameters["definitions"]; !hasDefinitions &&
				!kimiSchemaContainsRef(parameters) {
				continue
			}
		}
		defs := map[string]any{}
		for _, key := range []string{"definitions", "$defs"} {
			if values, ok := parameters[key].(map[string]any); ok {
				for name, value := range values {
					defs[name] = value
				}
			}
		}
		budget := 10000
		expanded := inlineKimiSchemaNode(parameters, defs, map[string]bool{}, 0, &budget)
		if budget < 0 {
			continue
		}
		expandedMap, ok := expanded.(map[string]any)
		if !ok {
			expandedMap = map[string]any{"type": "object"}
		}
		copiedFunction := cloneMap(function)
		copiedFunction["parameters"] = expandedMap
		normalizeChatToolParameters(copiedFunction)
		tool["function"] = copiedFunction
	}
}
