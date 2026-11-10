package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestMessagesCountTokens(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	var observed []ProtocolObservation
	server, provider := messagesTestServer(t, newStubStream(), ServerOptions{
		ProtocolObserver: func(observation ProtocolObservation) { observed = append(observed, observation) },
	})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages/count_tokens", `{
		"model":"m1",
		"messages":[{"role":"user","content":"你好 hello"}],
		"tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}]
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	count, ok := body["input_tokens"].(float64)
	if !ok || count <= 0 {
		t.Fatalf("count = %#v", body)
	}
	if len(provider.opened()) != 0 {
		t.Fatalf("count_tokens called the upstream")
	}
	if len(observed) != 0 {
		t.Fatalf("count_tokens recorded usage: %#v", observed)
	}

	cjk, err := messagesToChat(map[string]any{"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "你好"}}})
	if err != nil {
		t.Fatalf("cjk translation: %v", err)
	}
	ascii, err := messagesToChat(map[string]any{"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if err != nil {
		t.Fatalf("ascii translation: %v", err)
	}
	if estimateMessagesInputTokens(cjk) <= estimateMessagesInputTokens(ascii) {
		t.Fatalf("CJK estimate = %d, ASCII = %d", estimateMessagesInputTokens(cjk), estimateMessagesInputTokens(ascii))
	}
	withTools, err := messagesToChat(map[string]any{
		"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{"name": "Bash", "input_schema": map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("tools translation: %v", err)
	}
	if estimateMessagesInputTokens(withTools) <= estimateMessagesInputTokens(ascii) {
		t.Fatalf("tools did not contribute: %d vs %d", estimateMessagesInputTokens(withTools), estimateMessagesInputTokens(ascii))
	}
}

func TestMessagesDefaultMaxTokensFromCatalog(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
	provider := &stubProvider{id: "workbuddy", realm: "intl", openStream: stream,
		models: []providers.Model{{"id": "m1", "max_output_tokens": json.Number("128000")}}}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	requests := provider.opened()
	if len(requests) != 1 {
		t.Fatalf("open requests = %d", len(requests))
	}
	if got := string(requests[0].Raw["max_tokens"]); got != "128000" {
		t.Fatalf("catalog default max_tokens = %s", got)
	}
}

func TestMessagesCountTokensAliases(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, path := range []string{"/messages/count_tokens", "/v1/messages/count_tokens"} {
		server, provider := messagesTestServer(t, newStubStream(), ServerOptions{})
		rec := serveRequest(server.Handler(), http.MethodPost, path,
			`{"model":"m1","messages":[{"role":"user","content":"hello world"}]}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["input_tokens"] == nil {
			t.Fatalf("%s body = %s", path, rec.Body)
		}
		if len(provider.opened()) != 0 {
			t.Fatalf("%s reached upstream", path)
		}
	}
}

func TestMessagesDisabledByDefault(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "")
	server, provider := messagesTestServer(t, newStubStream(), ServerOptions{})
	for _, path := range []string{"/messages", "/v1/messages", "/messages/count_tokens", "/v1/messages/count_tokens"} {
		rec := serveRequest(server.Handler(), http.MethodPost, path,
			`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
		failure := requireAnthropicError(t, rec, http.StatusBadRequest, "invalid_request_error")
		message := stringValue(failure["message"])
		if !strings.Contains(message, "WB_ENABLE_ANTHROPIC_MESSAGES") || !strings.Contains(message, "issue #367") {
			t.Fatalf("%s message = %q", path, message)
		}
	}
	if len(provider.opened()) != 0 {
		t.Fatalf("disabled requests reached upstream")
	}

	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
	server, _ = messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("env-enabled status = %d body=%s", rec.Code, rec.Body)
	}

	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "")
	stream = newStubStream(chatSSE(textChunks("u", "hi")...)...)
	server, _ = messagesTestServer(t, stream, ServerOptions{AnthropicMessagesEnabled: func() bool { return true }})
	rec = serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings-enabled status = %d body=%s", rec.Code, rec.Body)
	}
}

func TestMessagesSettingsPrecedence(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "0")
	cfg := false
	server, _ := messagesTestServer(t, newStubStream(), ServerOptions{
		AnthropicMessagesEnabled: func() bool { return cfg },
	})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
	requireAnthropicError(t, rec, http.StatusBadRequest, "invalid_request_error")
	cfg = true
	stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
	server, _ = messagesTestServer(t, stream, ServerOptions{
		AnthropicMessagesEnabled: func() bool { return cfg },
	})
	rec = serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled setting status = %d body=%s", rec.Code, rec.Body)
	}
}

func TestAnthropicErrors(t *testing.T) {
	cases := []struct {
		status  int
		errType string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusServiceUnavailable, "overloaded_error"},
		{http.StatusBadGateway, "api_error"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeAnthropicError(rec, tc.status, "failure", "", time.Time{})
		if rec.Code != tc.status {
			t.Fatalf("status %d -> %d", tc.status, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("status %d body: %v", tc.status, err)
		}
		if body["type"] != "error" || body["error"].(map[string]any)["type"] != tc.errType {
			t.Fatalf("status %d = %#v", tc.status, body)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("status %d content type = %q", tc.status, rec.Header().Get("Content-Type"))
		}
	}

	reset := time.Now().Add(30 * time.Second).Truncate(time.Second)
	rec := httptest.NewRecorder()
	writeAnthropicError(rec, http.StatusTooManyRequests, "slow down", "rate_limit_error", reset)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body: %v", err)
	}
	message := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "(resets at ") {
		t.Fatalf("429 message = %q", message)
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" || rec.Header().Get("X-RateLimit-Reset-At") == "" ||
		rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 headers = %#v", rec.Header())
	}
}

func TestMessagesAuthUsesAnthropicEnvelope(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: newStubStream()}
	server := New(ServerOptions{
		Providers:  []providers.Provider{provider},
		APIKeyAuth: RequireAPIKeyAuth("secret"),
		PanelAuth:  passThroughAuth,
	})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
	requireAnthropicError(t, rec, http.StatusUnauthorized, "authentication_error")
	if len(provider.opened()) != 0 {
		t.Fatalf("unauthenticated request reached provider")
	}
}

func TestMessagesRejectsInvalidJSONWithAnthropicEnvelope(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	server, _ := messagesTestServer(t, newStubStream(), ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", "{not json", nil)
	requireAnthropicError(t, rec, http.StatusBadRequest, "invalid_request_error")
}
