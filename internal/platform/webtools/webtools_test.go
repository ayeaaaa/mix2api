package webtools

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebToolDefsAndDeclarations(t *testing.T) {
	tools := []map[string]any{
		{"type": "web_search"},
		{"type": "function", "function": map[string]any{"name": "web_fetch"}},
	}
	wants := WantsFromTools(tools)
	if !wants.Search || !wants.Fetch {
		t.Fatalf("wants = %#v", wants)
	}
	installed := InstallToolDefs([]map[string]any{
		{"type": "web_search"},
		{"type": "function", "function": map[string]any{"name": "web_search"}},
		{"type": "function", "name": "keep", "parameters": map[string]any{}},
	}, wants)
	if len(installed) != 3 {
		t.Fatalf("installed = %#v", installed)
	}
	searchCount := 0
	for _, tool := range installed {
		if tool["name"] == WebSearchName {
			searchCount++
		}
	}
	if searchCount != 1 {
		t.Fatalf("search duplicates = %d", searchCount)
	}
	if len(ToolDefs(Wants{})) != 0 {
		t.Fatal("empty wants produced tools")
	}
	if MaxRounds() < 1 || MaxRounds() > 8 {
		t.Fatalf("MaxRounds = %d", MaxRounds())
	}
	t.Setenv("WB_MAX_WEB_ROUNDS", "99")
	if MaxRounds() != 8 {
		t.Fatalf("cap = %d", MaxRounds())
	}
}

func TestWebSearchFallbackAndCache(t *testing.T) {
	var ddgCalls, bingCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			atomic.AddInt32(&ddgCalls, 1)
			io.WriteString(w, `<html><body>unusual traffic from your network</body></html>`)
		case "/bing":
			atomic.AddInt32(&bingCalls, 1)
			io.WriteString(w, `<html><body>
<li class="b_algo"><h2><a href="https://example.com/a">Example A</a></h2><p>Snippet A</p></li>
<li class="b_algo"><h2><a href="https://example.com/b">Example B</a></h2><p>Snippet B</p></li>
</body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	service := New()
	service.AllowPrivate = true
	service.MinInterval = 0
	service.SearchBudget = 5 * time.Second
	service.EngineURLs = map[string]string{
		"duckduckgo": server.URL + "/ddg?q=%s",
		"bing":       server.URL + "/bing?q=%s",
	}
	text := service.Search(context.Background(), "golang testing", 5)
	if !strings.Contains(text, "Search results for: golang testing") || !strings.Contains(text, "Example A") {
		t.Fatalf("search = %s", text)
	}
	if atomic.LoadInt32(&ddgCalls) != 1 || atomic.LoadInt32(&bingCalls) != 1 {
		t.Fatalf("calls ddg=%d bing=%d", ddgCalls, bingCalls)
	}
	// Cache hit: no extra backend calls.
	if cached := service.Search(context.Background(), "golang testing", 5); cached != text {
		t.Fatalf("cache miss: %s", cached)
	}
	if atomic.LoadInt32(&bingCalls) != 1 {
		t.Fatal("cache hit still called bing")
	}
	// Cooling skips the blocked engine on a different query.
	if got := service.Search(context.Background(), "different query", 5); !strings.Contains(got, "Example A") {
		t.Fatalf("cooling fallback = %s", got)
	}
	if atomic.LoadInt32(&ddgCalls) != 1 {
		t.Fatalf("cooling did not skip ddg: %d", ddgCalls)
	}
	sources := Sources(text)
	if len(sources) != 2 || sources[0].URL != "https://example.com/a" || sources[0].Title != "Example A" {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestWebSearchUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer server.Close()
	service := New()
	service.AllowPrivate = true
	service.MinInterval = 0
	service.SearchBudget = 2 * time.Second
	service.Engines = []string{"duckduckgo", "bing"}
	service.EngineURLs = map[string]string{"duckduckgo": server.URL, "bing": server.URL}
	text := service.Search(context.Background(), "no results anywhere", 5)
	if !strings.HasPrefix(text, unavailablePrefix) {
		t.Fatalf("unavailable = %s", text)
	}
}

func TestWebFetchGuard(t *testing.T) {
	service := New()
	for _, target := range []string{
		"file:///etc/passwd", "http://localhost/x", "http://127.0.0.1/x",
		"http://10.0.0.1/x", "http://192.168.1.1/x", "http://169.254.1.1/x",
		"http://[::1]/x", "http://user:pass@example.com/x",
	} {
		if got := service.Fetch(context.Background(), target, 0); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("guard accepted %s: %s", target, got)
		}
	}
	// A host resolving to a private address is rejected even when the literal
	// is not private.
	service.LookupIP = func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.1.2.3")}, nil }
	if got := service.Fetch(context.Background(), "http://public.example/x", 0); !strings.HasPrefix(got, "Error:") {
		t.Fatalf("private resolution accepted: %s", got)
	}
}

func TestWebFetchAndTruncation(t *testing.T) {
	body := "<html><head><title>x</title><style>bad{}</style></head><body><p>Hello&nbsp;world</p><script>evil()</script><p>" + strings.Repeat("A", MaxFetchChars+5) + "</p></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/binary" {
			w.Header().Set("Content-Type", "application/pdf")
			io.WriteString(w, "pdf")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	}))
	defer server.Close()
	service := New()
	service.AllowPrivate = true
	text := service.Fetch(context.Background(), server.URL+"/page?q=1", 0)
	if !strings.Contains(text, "Hello world") || strings.Contains(text, "evil()") || strings.Contains(text, "bad{}") {
		t.Fatalf("fetch text = %s", text[:minIntWeb(200, len(text))])
	}
	if !strings.Contains(text, "URL: "+server.URL+"/page?q=1") {
		t.Fatalf("query URL lost: %s", text[:200])
	}
	if !strings.Contains(text, "Truncated. Call web_fetch again with startIndex=") {
		t.Fatalf("truncation missing: %s", text[len(text)-120:])
	}
	binary := service.Fetch(context.Background(), server.URL+"/binary", 0)
	if !strings.Contains(binary, "not a text page") {
		t.Fatalf("binary fetch = %s", binary)
	}
}

func TestWebToolExecution(t *testing.T) {
	service := New()
	service.AllowPrivate = true
	service.MinInterval = 0
	service.EngineURLs = map[string]string{"duckduckgo": "http://127.0.0.1:1/?q=%s"}
	service.Engines = []string{"duckduckgo"}
	service.SearchBudget = 100 * time.Millisecond
	if got := service.Execute(context.Background(), WebSearchName, `{"queries":["one","two"],"numResults":99}`); strings.Contains(got, "at least 2") {
		t.Fatalf("query args not accepted: %s", got)
	}
	if got := service.Execute(context.Background(), "nope", "{}"); !strings.Contains(got, "not a tool") {
		t.Fatalf("unknown tool = %s", got)
	}
	if got := service.Execute(context.Background(), WebSearchName, `{"query":""}`); !strings.Contains(got, "at least 2") {
		t.Fatalf("short query = %s", got)
	}
	if got := service.Execute(context.Background(), WebFetchName, `{"urls":["file:///etc/passwd"]}`); !strings.Contains(got, "Only http") && !strings.Contains(got, "Error") {
		t.Fatalf("url arg = %s", got)
	}
}

func minIntWeb(a, b int) int {
	if a < b {
		return a
	}
	return b
}
