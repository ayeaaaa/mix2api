package webtools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const unavailablePrefix = "Error: web search is temporarily unavailable"

var cjkPattern = regexp.MustCompile(`[\p{Han}\p{Hiragana}\p{Katakana}\p{Hangul}]`)
var blockedMarkers = []string{
	"captcha", "unusual traffic", "are you a robot", "access denied",
	"request blocked", "too many requests", "bot detection",
	"enable javascript and cookies to continue",
}

type engineResult struct {
	Title   string
	URL     string
	Snippet string
}

func hasCJK(text string) bool { return cjkPattern.MatchString(text) }

func (s *Service) engineNames() []string {
	if s != nil && len(s.Engines) > 0 {
		return append([]string(nil), s.Engines...)
	}
	return []string{"duckduckgo", "bing", "wikipedia", "stackoverflow", "news"}
}

func (s *Service) engineURL(name, query string, n int) string {
	if s != nil && s.EngineURLs != nil {
		if override := strings.TrimSpace(s.EngineURLs[name]); override != "" {
			if strings.Contains(override, "%s") {
				return strings.ReplaceAll(override, "%s", url.QueryEscape(query))
			}
			return override
		}
	}
	switch name {
	case "duckduckgo":
		return "https://html.duckduckgo.com/html/?" + url.Values{"q": {query}}.Encode()
	case "bing":
		return "https://www.bing.com/search?" + url.Values{"q": {query}, "count": {itoa(n)}}.Encode()
	case "stackoverflow":
		return "https://api.stackexchange.com/2.3/search/advanced?" + url.Values{
			"order": {"desc"}, "sort": {"relevance"}, "q": {query}, "site": {"stackoverflow"},
			"pagesize": {itoa(n)}, "filter": {"default"},
		}.Encode()
	case "wikipedia":
		lang := "en"
		if hasCJK(query) {
			lang = "zh"
		}
		return "https://" + lang + ".wikipedia.org/w/api.php?" + url.Values{
			"action": {"query"}, "list": {"search"}, "srsearch": {query},
			"format": {"json"}, "utf8": {"1"}, "srlimit": {itoa(n)},
		}.Encode()
	case "news":
		params := url.Values{"q": {query}}
		if hasCJK(query) {
			params.Set("hl", "zh-TW")
			params.Set("gl", "TW")
			params.Set("ceid", "TW:zh")
		} else {
			params.Set("hl", "en-US")
			params.Set("gl", "US")
			params.Set("ceid", "US:en")
		}
		return "https://news.google.com/rss/search?" + params.Encode()
	default:
		return ""
	}
}

func (s *Service) sourceNote(name string) string {
	switch name {
	case "stackoverflow":
		return "Source: Stack Overflow (Q&A site, not a general web search engine) - tell the user these results came from Stack Overflow."
	case "wikipedia":
		return "Source: Wikipedia (encyclopedia entries, not a general web search engine) - tell the user these results came from Wikipedia."
	case "news":
		return "Source: Google News RSS (news feed, not a general web search engine) - tell the user these results came from Google News."
	default:
		return ""
	}
}

func (s *Service) searchFanout(ctx context.Context, query string, n int) string {
	names := s.engineNames()
	if len(names) == 0 {
		return "Error: no search engines are configured."
	}
	started := s.now()
	deadline := started.Add(s.SearchBudget)
	attempts := 0
	failures := []string{}
	skipped := []string{}
	cleanEmpty := false

	finish := func(name string, results []engineResult) string {
		s.log("webtools: search served by " + name)
		return formatResults(query, results, s.sourceNote(name))
	}
	try := func(name string) ([]engineResult, bool) {
		attempts++
		remaining := s.SearchBudget
		if s.SearchBudget > 0 {
			remaining = deadline.Sub(s.now())
			if remaining <= 0 {
				return nil, false
			}
		}
		timeout := HTTPTimeout * time.Second
		if remaining < timeout && remaining > 3*time.Second {
			timeout = remaining
		}
		state, results, detail := s.runEngine(ctx, name, query, n, timeout)
		switch state {
		case "ok":
			s.clearCooling(name)
			return results, true
		case "blocked", "error":
			duration := s.CooldownError
			if state == "blocked" {
				duration = s.CooldownBlocked
			}
			s.markCooling(name, duration, state+": "+detail)
		default:
			cleanEmpty = true
		}
		failures = append(failures, name+": "+state+detailSuffix(detail))
		return nil, false
	}
	for _, name := range names {
		if s.cooling(name) {
			skipped = append(skipped, name)
			continue
		}
		if s.SearchBudget > 0 && deadline.Sub(s.now()) <= time.Second {
			failures = append(failures, name+": budget exhausted")
			break
		}
		if results, ok := try(name); ok {
			return finish(name, results)
		}
	}
	for _, name := range skipped {
		if s.SearchBudget > 0 && deadline.Sub(s.now()) <= time.Second {
			break
		}
		if results, ok := try(name); ok {
			return finish(name, results)
		}
	}
	if cleanEmpty {
		return "No results found for: " + query + "\n\nTry a broader or differently worded query."
	}
	detail := strings.Join(failures, "; ")
	if detail == "" {
		detail = "no engine available"
	}
	s.log("webtools: search failed on every backend (" + detail + ")")
	return unavailablePrefix + ". Every backend was tried automatically and all failed (" + detail + "). Do not retry the same query right away; tell the user that web search is temporarily unavailable and continue with what you already know."
}

func detailSuffix(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return " (" + detail + ")"
}

func (s *Service) runEngine(ctx context.Context, name, query string, n int, timeout time.Duration) (string, []engineResult, string) {
	target := s.engineURL(name, query, n)
	if target == "" {
		return "error", nil, "unknown engine"
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, status, _, charset, err := s.openURL(runCtx, target, SearchBodyBytes, "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if err != nil {
		return "error", nil, errType(err)
	}
	page := decodeText(raw, charset)
	results := parseEngine(name, page, n, query)
	if len(results) > 0 {
		return "ok", results, ""
	}
	if looksBlocked(page) {
		return "blocked", nil, "anti-bot page"
	}
	if status == 202 {
		return "blocked", nil, "HTTP 202"
	}
	if status >= 400 {
		return "error", nil, "HTTP " + itoa(status)
	}
	return "empty", nil, "no results"
}

func looksBlocked(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range blockedMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func formatResults(query string, results []engineResult, note string) string {
	header := "Search results for: " + query
	if note != "" {
		header += "\n[" + note + "]"
	}
	lines := []string{}
	for index, result := range results {
		lines = append(lines, itoa(index+1)+". "+result.Title+"\n   "+result.URL+"\n   "+result.Snippet)
	}
	return header + "\n\n" + strings.Join(lines, "\n\n") + "\n\nCite the sources you used at the end of your answer."
}

func parseEngine(name, page string, n int, query string) []engineResult {
	switch name {
	case "duckduckgo":
		return parseDuckDuckGo(page, n)
	case "bing":
		return parseBing(page, n)
	case "stackoverflow":
		return parseStackOverflow(page, n)
	case "wikipedia":
		return parseWikipedia(page, n, query)
	case "news":
		return parseNews(page, n)
	default:
		return nil
	}
}

var ddgBlockPattern = regexp.MustCompile(`(?is)<div[^>]+class="[^"]*result__body[^"]*"`)
var ddgLinkPattern = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
var ddgSnippetPattern = regexp.MustCompile(`(?is)class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)

func parseDuckDuckGo(page string, n int) []engineResult {
	blocks := ddgBlockPattern.Split(page, -1)
	out := []engineResult{}
	seen := map[string]bool{}
	for _, block := range blocks[1:] {
		match := ddgLinkPattern.FindStringSubmatch(block)
		if match == nil {
			continue
		}
		href := ddgTarget(match[1])
		title := stripTags(match[2])
		if !strings.HasPrefix(href, "http") || title == "" || seen[href] {
			continue
		}
		seen[href] = true
		snippet := ""
		if snippetMatch := ddgSnippetPattern.FindStringSubmatch(block); snippetMatch != nil {
			snippet = stripTags(snippetMatch[1])
		}
		out = append(out, engineResult{Title: title, URL: href, Snippet: snippet})
		if len(out) >= n {
			break
		}
	}
	return out
}

func ddgTarget(href string) string {
	href = html.UnescapeString(strings.TrimSpace(href))
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	parsed, err := url.Parse(href)
	if err == nil && strings.Contains(parsed.Hostname(), "duckduckgo.com") && strings.HasPrefix(parsed.Path, "/l/") {
		if target := parsed.Query().Get("uddg"); target != "" {
			if decoded, err := url.QueryUnescape(target); err == nil {
				return decoded
			}
			return target
		}
	}
	return href
}

var bingBlockPattern = regexp.MustCompile(`(?is)<li class="b_algo".*?</li>`)
var bingLinkPattern = regexp.MustCompile(`(?is)<h2[^>]*>\s*<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
var bingSnippetPattern = regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)

func parseBing(page string, n int) []engineResult {
	out := []engineResult{}
	seen := map[string]bool{}
	for _, block := range bingBlockPattern.FindAllString(page, -1) {
		match := bingLinkPattern.FindStringSubmatch(block)
		if match == nil {
			continue
		}
		href := bingTarget(match[1])
		title := stripTags(match[2])
		if !strings.HasPrefix(href, "http") || title == "" || seen[href] || strings.Contains(href, "bing.com/search") {
			continue
		}
		seen[href] = true
		snippet := ""
		if snippetMatch := bingSnippetPattern.FindStringSubmatch(block); snippetMatch != nil {
			snippet = stripTags(snippetMatch[1])
		}
		out = append(out, engineResult{Title: title, URL: href, Snippet: snippet})
		if len(out) >= n {
			break
		}
	}
	return out
}

func bingTarget(href string) string {
	href = html.UnescapeString(strings.TrimSpace(href))
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	parsed, err := url.Parse(href)
	if err == nil {
		raw := parsed.Query().Get("u")
		if strings.HasPrefix(raw, "a1") {
			raw = raw[2:]
			for len(raw)%4 != 0 {
				raw += "="
			}
			if decoded, err := base64.URLEncoding.DecodeString(raw); err == nil && strings.HasPrefix(string(decoded), "http") {
				return string(decoded)
			}
		}
	}
	return href
}

func parseStackOverflow(page string, n int) []engineResult {
	var data map[string]any
	if json.Unmarshal([]byte(page), &data) != nil {
		return nil
	}
	items, _ := data["items"].([]any)
	out := []engineResult{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		title := stripTags(stringValue(item["title"]))
		link := strings.TrimSpace(stringValue(item["link"]))
		if title == "" || !strings.HasPrefix(link, "http") {
			continue
		}
		snippet := "Stack Overflow"
		if score, ok := item["score"].(float64); ok {
			snippet += " - score " + itoa(int(score))
		}
		out = append(out, engineResult{Title: title, URL: link, Snippet: snippet})
		if len(out) >= n {
			break
		}
	}
	return out
}

func parseWikipedia(page string, n int, query string) []engineResult {
	var data map[string]any
	if json.Unmarshal([]byte(page), &data) != nil {
		return nil
	}
	queryBlock, _ := data["query"].(map[string]any)
	hits, _ := queryBlock["search"].([]any)
	lang := "en"
	if hasCJK(query) {
		lang = "zh"
	}
	out := []engineResult{}
	for _, raw := range hits {
		hit, _ := raw.(map[string]any)
		if hit == nil {
			continue
		}
		title := strings.TrimSpace(stringValue(hit["title"]))
		if title == "" {
			continue
		}
		out = append(out, engineResult{
			Title: title, URL: "https://" + lang + ".wikipedia.org/wiki/" + url.PathEscape(strings.ReplaceAll(title, " ", "_")),
			Snippet: stripTags(stringValue(hit["snippet"])),
		})
		if len(out) >= n {
			break
		}
	}
	return out
}

var newsItemPattern = regexp.MustCompile(`(?is)<item>(.*?)</item>`)
var newsTitlePattern = regexp.MustCompile(`(?is)<title>(.*?)</title>`)
var newsLinkPattern = regexp.MustCompile(`(?is)<link>(.*?)</link>`)
var newsDatePattern = regexp.MustCompile(`(?is)<pubDate>(.*?)</pubDate>`)

func parseNews(page string, n int) []engineResult {
	out := []engineResult{}
	for _, item := range newsItemPattern.FindAllStringSubmatch(page, -1) {
		body := item[1]
		title := ""
		link := ""
		date := ""
		if match := newsTitlePattern.FindStringSubmatch(body); match != nil {
			title = stripTags(match[1])
		}
		if match := newsLinkPattern.FindStringSubmatch(body); match != nil {
			link = html.UnescapeString(strings.TrimSpace(match[1]))
		}
		if match := newsDatePattern.FindStringSubmatch(body); match != nil {
			date = stripTags(match[1])
		}
		if title == "" || !strings.HasPrefix(link, "http") {
			continue
		}
		snippet := "News item"
		if date != "" {
			snippet += " - " + date
		}
		out = append(out, engineResult{Title: title, URL: link, Snippet: snippet})
		if len(out) >= n {
			break
		}
	}
	return out
}
