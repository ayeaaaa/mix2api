// Package webtools implements the gateway-local web_search and web_fetch tools.
package webtools

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

const (
	WebSearchName = "web_search"
	WebFetchName  = "web_fetch"

	defaultMaxWebRounds = 3
	maxWebRounds        = 8
)

// Wants reports which local tools the client declared.
type Wants struct {
	Search bool
	Fetch  bool
}

var searchDeclTypes = map[string]bool{
	"web_search": true, "web_search_preview": true, "web_search_preview_2025_03_11": true,
}

var fetchDeclTypes = map[string]bool{"web_fetch": true}

// WantsFromTools recognizes server-declaration and function-shaped tools.
func WantsFromTools(tools []map[string]any) Wants {
	wants := Wants{}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.ToLower(strings.TrimSpace(stringValue(tool["name"])))
		if function, ok := tool["function"].(map[string]any); ok {
			if name == "" {
				name = strings.ToLower(strings.TrimSpace(stringValue(function["name"])))
			}
		}
		if searchDeclTypes[kind] || searchDeclTypes[name] || name == WebSearchName {
			wants.Search = true
		}
		if fetchDeclTypes[kind] || fetchDeclTypes[name] || name == WebFetchName {
			wants.Fetch = true
		}
	}
	return wants
}

// ToolDefs returns the function declarations for the requested tools.
func ToolDefs(wants Wants) []map[string]any {
	out := []map[string]any{}
	if wants.Search {
		out = append(out, webSearchToolDef())
	}
	if wants.Fetch {
		out = append(out, webFetchToolDef())
	}
	return out
}

func webSearchToolDef() map[string]any {
	return map[string]any{
		"type": "function", "name": WebSearchName,
		"description": "Searches the web for real-time information and returns ranked results with titles, URLs and snippets. Use it for current events, documentation lookup, or anything beyond your knowledge cutoff. To read a page in full, call web_fetch on its URL afterwards.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":      map[string]any{"type": "string", "description": "The search query (at least 2 characters)."},
				"numResults": map[string]any{"type": "number", "description": "How many results to return (1-10, default 5)."},
			},
			"required": []string{"query"},
		},
	}
}

func webFetchToolDef() map[string]any {
	return map[string]any{
		"type": "function", "name": WebFetchName,
		"description": "Fetches a URL and returns its readable text. Use startIndex to page through a long page.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":        map[string]any{"type": "string", "description": "Absolute http:// or https:// URL to fetch."},
				"startIndex": map[string]any{"type": "number", "description": "Character offset to continue reading a long page."},
			},
			"required": []string{"url"},
		},
	}
}

// InstallToolDefs removes same-name declarations and appends one canonical
// function per requested tool.
func InstallToolDefs(tools []map[string]any, wants Wants) []map[string]any {
	names := map[string]bool{}
	if wants.Search {
		names[WebSearchName] = true
	}
	if wants.Fetch {
		names[WebFetchName] = true
	}
	kept := make([]map[string]any, 0, len(tools)+2)
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.ToLower(strings.TrimSpace(stringValue(tool["name"])))
		if function, ok := tool["function"].(map[string]any); ok && name == "" {
			name = strings.ToLower(strings.TrimSpace(stringValue(function["name"])))
		}
		if names[name] || names[kind] {
			continue
		}
		kept = append(kept, tool)
	}
	kept = append(kept, ToolDefs(wants)...)
	return kept
}

// IsInternalTool reports whether name is executed by this gateway.
func IsInternalTool(name string) bool {
	name = strings.TrimSpace(name)
	return name == WebSearchName || name == WebFetchName
}

// MaxRounds returns the v1 web-round cap (default 3, hard cap 8).
func MaxRounds() int {
	raw := strings.TrimSpace(os.Getenv("WB_MAX_WEB_ROUNDS"))
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultMaxWebRounds
	}
	if value > maxWebRounds {
		return maxWebRounds
	}
	return value
}

// MaxWebRounds is the default for callers that do not read the environment.
const MaxWebRounds = defaultMaxWebRounds

// QueryArgs accepts query/queries/q and joins arrays with " or ".
func QueryArgs(args map[string]any) string {
	if args == nil {
		return ""
	}
	raw, ok := args["query"]
	if !ok {
		raw, ok = args["queries"]
	}
	if !ok {
		raw, ok = args["q"]
	}
	if !ok {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := []string{}
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, " or ")
	case []string:
		return strings.Join(typed, " or ")
	default:
		return ""
	}
}

// URLArg accepts url/urls and uses the first array element.
func URLArg(args map[string]any) string {
	if args == nil {
		return ""
	}
	raw, ok := args["url"]
	if !ok {
		raw, ok = args["urls"]
	}
	if !ok {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	case []string:
		if len(typed) > 0 {
			return strings.TrimSpace(typed[0])
		}
	}
	return ""
}

// Source is one parsed citation.
type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Sources parses titles and URLs out of our formatted search result.
func Sources(result string) []Source {
	out := []Source{}
	lines := strings.Split(result, "\n")
	for index := 0; index+1 < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if len(line) < 3 || line[0] < '0' || line[0] > '9' {
			continue
		}
		dot := strings.Index(line, ". ")
		if dot < 0 {
			continue
		}
		title := strings.TrimSpace(line[dot+2:])
		urlLine := strings.TrimSpace(lines[index+1])
		if !strings.HasPrefix(urlLine, "http://") && !strings.HasPrefix(urlLine, "https://") {
			continue
		}
		url := strings.TrimRight(urlLine, ".,;:!?")
		duplicate := false
		for _, source := range out {
			if source.URL == url {
				duplicate = true
				break
			}
		}
		if !duplicate && url != "" {
			if title == "" {
				title = url
			}
			out = append(out, Source{Title: title, URL: url})
		}
	}
	return out
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// ArgsFromAny decodes an arguments string or map.
func ArgsFromAny(raw any) map[string]any {
	switch typed := raw.(type) {
	case map[string]any:
		return typed
	case string:
		var out map[string]any
		if json.Unmarshal([]byte(typed), &out) == nil {
			return out
		}
	case json.RawMessage:
		var out map[string]any
		if json.Unmarshal(typed, &out) == nil {
			return out
		}
	case []byte:
		var out map[string]any
		if json.Unmarshal(typed, &out) == nil {
			return out
		}
	}
	return map[string]any{}
}
