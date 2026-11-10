package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func webToolCallStream() *stubStream {
	return newStubStream(chatSSE(
		map[string]any{"id": "c1", "model": "m1", "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "web_search", "arguments": `{"query":"golang"}`},
			}}},
		}}},
		map[string]any{"id": "c1", "model": "m1", "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls",
		}}},
	)...)
}

func TestWebRoundsInResponses(t *testing.T) {
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<li class="b_algo"><h2><a href="https://go.dev/doc">Go docs</a></h2><p>Official docs</p></li>`)
	}))
	defer searchServer.Close()
	web := webtools.New()
	web.AllowPrivate = true
	web.MinInterval = 0
	web.SearchBudget = 5 * time.Second
	web.Engines = []string{"bing"}
	web.EngineURLs = map[string]string{"bing": searchServer.URL + "?q=%s"}

	first := webToolCallStream()
	second := newStubStream(chatSSE(textChunks("c2", "final answer")...)...)
	server, provider := responseTestServer(t, nil, ServerOptions{WebTools: web})
	provider.mu.Lock()
	provider.openStreams = []providers.ChatStream{first, second}
	provider.openStream = nil
	provider.mu.Unlock()

	rec := doRequest(server, http.MethodPost, "/v1/responses", `{
		"model":"m1","stream":false,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"search go"}]}],
		"tools":[{"type":"web_search"}]
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	output, _ := body["output"].([]any)
	hasSearchCall := false
	hasText := false
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] == "web_search_call" {
			hasSearchCall = true
		}
		if item["type"] == "message" && strings.Contains(rec.Body.String(), "final answer") {
			hasText = true
		}
	}
	if !hasSearchCall || !hasText {
		t.Fatalf("output = %#v", output)
	}
	if sources, _ := body["sources"].([]any); len(sources) == 0 {
		t.Fatalf("sources = %#v", body["sources"])
	}
	opened := provider.opened()
	if len(opened) != 2 {
		t.Fatalf("rounds = %d", len(opened))
	}
	secondMessages := opened[1].Messages
	foundToolResult := false
	for _, raw := range secondMessages {
		var message map[string]any
		if err := jsonUnmarshal(raw, &message); err == nil && message["role"] == "tool" {
			if strings.Contains(stringOf(message["content"]), "Go docs") {
				foundToolResult = true
			}
		}
	}
	if !foundToolResult {
		t.Fatalf("second round did not receive tool result: %#v", secondMessages)
	}
	// The upstream declaration is replaced by one local function definition.
	if len(opened[0].Raw) > 0 {
		var raw map[string]any
		_ = jsonUnmarshal(opened[0].Raw["tools"], &raw)
	}
}

func TestWebRoundsCap(t *testing.T) {
	web := webtools.New()
	web.AllowPrivate = true
	web.Engines = []string{"bing"}
	web.EngineURLs = map[string]string{"bing": "http://127.0.0.1:1/?q=%s"}
	web.SearchBudget = 50 * time.Millisecond
	streams := []providers.ChatStream{}
	for i := 0; i < 6; i++ {
		streams = append(streams, webToolCallStream())
	}
	server, provider := responseTestServer(t, nil, ServerOptions{WebTools: web})
	provider.mu.Lock()
	provider.openStreams = streams
	provider.mu.Unlock()
	rec := doRequest(server, http.MethodPost, "/v1/responses", `{
		"model":"m1","stream":false,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"loop"}]}],
		"tools":[{"type":"web_search"}]
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if opened := provider.opened(); len(opened) != webtools.MaxRounds() {
		t.Fatalf("round cap = %d, want %d", len(opened), webtools.MaxRounds())
	}
}

func jsonUnmarshal(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}

func stringOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
