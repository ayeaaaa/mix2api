package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func messagesTestServer(t *testing.T, stream *stubStream, options ServerOptions) (*Server, *stubProvider) {
	t.Helper()
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: stream}
	options.Version = "2.0.0-test"
	options.Providers = []providers.Provider{provider}
	return newTestServer(options), provider
}

// messagesChatStream returns a Messages-compatible chat stream whose chunks
// carry the given model, so tests can distinguish the provider's model field
// from the client-visible requested ID.
func messagesChatStream(model string) providers.ChatStream {
	return newStubStream(chatSSE(
		map[string]any{"id": "upstream-one", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}}},
		map[string]any{"id": "upstream-one", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}},
	)...)
}

func requireAnthropicError(t *testing.T, rec *httptest.ResponseRecorder, status int, errType string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, status, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["type"] != "error" {
		t.Fatalf("envelope = %#v", body)
	}
	failure, _ := body["error"].(map[string]any)
	if failure["type"] != errType {
		t.Fatalf("error type = %#v, want %q (body=%s)", failure["type"], errType, rec.Body)
	}
	return failure
}

func TestMessagesRequestMapsToChat(t *testing.T) {
	request := map[string]any{
		"model": "m1", "max_tokens": 128, "stream": true,
		"system": []any{
			map[string]any{"type": "text", "text": "sys-a"},
			map[string]any{"type": "text", "text": "sys-b"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aaa"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "secret", "signature": "sig"},
				map[string]any{"type": "text", "text": "ok"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"cmd": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
				map[string]any{"type": "text", "text": "next"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"type": "web_search_20250305", "name": "web_search"},
		},
		"tool_choice":        map[string]any{"type": "tool", "name": "Bash", "disable_parallel_tool_use": true},
		"stop_sequences":     []any{"END"},
		"metadata":           map[string]any{"user_id": "u"},
		"thinking":           map[string]any{"type": "adaptive", "display": "summarized"},
		"output_config":      map[string]any{"effort": "high", "format": map[string]any{"type": "json_schema"}},
		"top_k":              5,
		"context_management": map[string]any{"edits": []any{}},
	}
	chat, err := messagesToChat(request)
	if err != nil {
		t.Fatalf("messagesToChat: %v", err)
	}
	if chat["model"] != "m1" || chat["stream"] != true || chat["max_tokens"] != 128 {
		t.Fatalf("basic fields = %#v", chat)
	}
	if chat["stop"].([]any)[0] != "END" || chat["user"] != "u" {
		t.Fatalf("stop/user = %#v", chat)
	}
	if chat["parallel_tool_calls"] != false || chat["reasoning_effort"] != "high" {
		t.Fatalf("parallel/effort = %#v", chat)
	}
	for _, absent := range []string{"top_k", "thinking", "output_config", "context_management"} {
		if _, ok := chat[absent]; ok {
			t.Fatalf("%s forwarded: %#v", absent, chat)
		}
	}
	messages := chat["messages"].([]any)
	system := messages[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(system, "sys-a\nsys-b") || !strings.Contains(system, "web_search") {
		t.Fatalf("system = %q", system)
	}
	if len(chat["tools"].([]any)) != 1 {
		t.Fatalf("tools = %#v", chat["tools"])
	}
	user := messages[1].(map[string]any)
	parts := user["content"].([]any)
	if parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,aaa" {
		t.Fatalf("image = %#v", parts[1])
	}
	assistant := messages[2].(map[string]any)
	if strings.Contains(compactJSON(assistant), "secret") {
		t.Fatalf("thinking forwarded: %#v", assistant)
	}
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "toolu_1" || call["function"].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("tool call = %#v", call)
	}
	toolMessage := messages[3].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "toolu_1" || toolMessage["content"] != "a.txt" {
		t.Fatalf("tool message = %#v", toolMessage)
	}
	if messages[4].(map[string]any)["content"] != "next" {
		t.Fatalf("user follow-up = %#v", messages[4])
	}
	if chat["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "Bash" {
		t.Fatalf("tool_choice = %#v", chat["tool_choice"])
	}

	mid, err := messagesToChat(map[string]any{
		"model": "m1", "system": "top",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "hook context"}}},
			map[string]any{"role": "assistant", "content": "ok"},
		},
	})
	if err != nil {
		t.Fatalf("mid system: %v", err)
	}
	roles := []string{}
	for _, raw := range mid["messages"].([]any) {
		roles = append(roles, stringValue(raw.(map[string]any)["role"]))
	}
	if strings.Join(roles, ",") != "system,user,system,assistant" {
		t.Fatalf("mid-system roles = %v", roles)
	}
	if _, err := messagesToChat(map[string]any{"model": "m1", "messages": []any{map[string]any{"role": "tool", "content": "x"}}}); err == nil || !strings.Contains(err.Error(), "tool") {
		t.Fatalf("unsupported role err = %v", err)
	}
	for _, invalid := range []map[string]any{
		{"messages": []any{map[string]any{"role": "user", "content": "x"}}},
		{"model": "m1", "messages": []any{}},
		{"model": "m1", "messages": "x"},
	} {
		if _, err := messagesToChat(invalid); err == nil {
			t.Fatalf("invalid payload accepted: %#v", invalid)
		}
	}
}

func TestMessagesToolResultImageAndEffort(t *testing.T) {
	image := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "bbb"}}
	mixed := anthropicToolResultContent([]any{
		map[string]any{"type": "text", "text": "screenshot read"},
		image,
		map[string]any{"type": "document", "source": map[string]any{"type": "text", "data": "doc"}},
	}, nil)
	parts, ok := mixed.([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("mixed tool result = %#v", mixed)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image part = %#v", parts[1])
	}
	if got := anthropicToolResultContent("a.txt", nil); got != "a.txt" {
		t.Fatalf("text result = %#v", got)
	}
	if got := anthropicToolResultContent("boom", true); got != "error: boom" {
		t.Fatalf("error result = %#v", got)
	}
	errParts := anthropicToolResultContent([]any{image}, true).([]any)
	if errParts[0].(map[string]any)["text"] != "error:" {
		t.Fatalf("error image parts = %#v", errParts)
	}
	for budget, want := range map[string]string{
		"disabled": "none", "enabled:1000": "low", "enabled:9000": "medium",
		"enabled:17000": "high", "enabled:40000": "xhigh",
	} {
		payload := map[string]any{"thinking": map[string]any{"type": "disabled"}}
		if strings.HasPrefix(budget, "enabled:") {
			payload = map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": json.Number(strings.TrimPrefix(budget, "enabled:"))}}
		}
		if got := anthropicEffortFromMessages(payload); got != want {
			t.Fatalf("effort %s = %q, want %q", budget, got, want)
		}
	}
}

func TestMessagesNonStreamShape(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "chatcmpl_1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Hello"}}}},
		map[string]any{"id": "chatcmpl_1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 2, "total_tokens": 13,
				"prompt_cache_hit_tokens": 3, "prompt_cache_write_tokens": 1}},
	)...)
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	server, _ := messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["type"] != "message" || body["role"] != "assistant" || body["stop_reason"] != "end_turn" || body["stop_sequence"] != nil {
		t.Fatalf("message = %#v", body)
	}
	content := body["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "Hello" {
		t.Fatalf("content = %#v", content)
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(2) ||
		usage["cache_read_input_tokens"] != float64(3) || usage["cache_creation_input_tokens"] != float64(1) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestMessagesNonStreamReasoningAndToolUse(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "chatcmpl_2", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "secret"}}}},
		map[string]any{"id": "chatcmpl_2", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}},
		map[string]any{"id": "chatcmpl_2", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "Bash", "arguments": `{"cmd":"ls"}`}},
		}}, "finish_reason": "tool_calls"}}},
	)...)
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	server, _ := messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	content := body["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %#v", content)
	}
	if content[0].(map[string]any)["type"] != "thinking" || content[1].(map[string]any)["type"] != "text" ||
		content[2].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("blocks = %#v", content)
	}
	if body["stop_reason"] != "tool_use" {
		t.Fatalf("stop reason = %#v", body["stop_reason"])
	}
	tool := content[2].(map[string]any)
	if tool["id"] != "call_1" || tool["name"] != "Bash" || tool["input"].(map[string]any)["cmd"] != "ls" {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestMessagesAliases(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, path := range []string{"/messages", "/v1/messages"} {
		stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
		server, _ := messagesTestServer(t, stream, ServerOptions{})
		rec := serveRequest(server.Handler(), http.MethodPost, path,
			`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["type"] != "message" {
			t.Fatalf("%s body = %s", path, rec.Body)
		}
	}
	for _, path := range []string{"/messages", "/v1/messages", "/messages/count_tokens", "/v1/messages/count_tokens"} {
		server, _ := messagesTestServer(t, newStubStream(), ServerOptions{})
		rec := serveRequest(server.Handler(), http.MethodGet, path, "", nil)
		requireAnthropicError(t, rec, http.StatusMethodNotAllowed, "invalid_request_error")
		if rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s Allow = %q", path, rec.Header().Get("Allow"))
		}
	}
}

func TestMessagesQualifiedModelRoutesExactExit(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	intl := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{intl, cn, codearts}})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"codearts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if got := len(intl.opened()); got != 0 {
		t.Fatalf("intl OpenChat calls = %d, want 0", got)
	}
	if got := len(cn.opened()); got != 0 {
		t.Fatalf("cn OpenChat calls = %d, want 0", got)
	}
	requests := codearts.opened()
	if len(requests) != 1 {
		t.Fatalf("codearts OpenChat calls = %d, want 1", len(requests))
	}
	if requests[0].Model != "shared" {
		t.Fatalf("ChatRequest.Model = %q, want shared", requests[0].Model)
	}
	if got, ok := decodeJSONString(requests[0].Raw["model"]); !ok || got != "shared" {
		t.Fatalf("Raw[model] = %q ok=%v, want shared", got, ok)
	}
	if flag := requests[0].AllowRealmFallback; flag == nil || *flag {
		t.Fatalf("AllowRealmFallback = %v, want explicit false", flag)
	}
	if body := decodeBody(t, rec); body["model"] != "codearts/shared" {
		t.Fatalf("response model = %#v, want codearts/shared", body["model"])
	}
}

func TestMessagesQualifiedNonStreamEchoesCanonicalModel(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}},
		openStream: messagesChatStream("upstream-alias")}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{codearts}})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"codearts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "upstream-alias") {
		t.Fatalf("qualified response leaked the upstream model: %s", rec.Body)
	}
	if body := decodeBody(t, rec); body["model"] != "codearts/shared" {
		t.Fatalf("response model = %#v, want codearts/shared", body["model"])
	}
}

func TestMessagesQualifiedModelRealmConflict(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{cn, codearts}})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages?realm=codearts",
		`{"model":"cn/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	failure := requireAnthropicError(t, rec, http.StatusBadRequest, "invalid_request_error")
	if message, _ := failure["message"].(string); !strings.Contains(message, "realm and model exit conflict") {
		t.Fatalf("failure = %#v", failure)
	}
	if len(cn.opened()) != 0 || len(codearts.opened()) != 0 {
		t.Fatalf("conflict reached providers cn=%d codearts=%d", len(cn.opened()), len(codearts.opened()))
	}
}

func TestMessagesQualifiedModelKeyRealmConflict(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	server := newTestServer(ServerOptions{
		Providers:   []providers.Provider{cn, codearts},
		APIKeyRealm: func(*http.Request) string { return "cn" },
	})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"codearts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	failure := requireAnthropicError(t, rec, http.StatusForbidden, "permission_error")
	if message, _ := failure["message"].(string); !strings.Contains(message, "api key realm does not allow exit codearts") {
		t.Fatalf("failure = %#v", failure)
	}
	if len(cn.opened()) != 0 || len(codearts.opened()) != 0 {
		t.Fatalf("bound-key conflict reached providers cn=%d codearts=%d", len(cn.opened()), len(codearts.opened()))
	}
}

func TestMessagesQualifiedModelKeyAllowList(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	_, keyStore, _, _, _ := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{
		{ID: "upstream", Name: "upstream", Key: "key-upstream", Models: []string{"shared"}},
		{ID: "denied", Name: "denied", Key: "key-denied", Models: []string{"cn/*"}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}},
		openStream: messagesChatStream("shared")}
	// Use New, not newTestServer: the panel gate must not pass through, or the
	// API-key row never reaches request context.
	server := New(ServerOptions{Providers: []providers.Provider{codearts}, KeyStore: keyStore})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"codearts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "key-upstream"})
	if rec.Code != http.StatusOK {
		t.Fatalf("upstream allow-list status = %d body=%s", rec.Code, rec.Body)
	}
	if got := len(codearts.opened()); got != 1 {
		t.Fatalf("codearts OpenChat calls = %d, want 1", got)
	}

	rec = serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"CodeArts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "key-denied"})
	failure := requireAnthropicError(t, rec, http.StatusBadRequest, "invalid_request_error")
	if message, _ := failure["message"].(string); !strings.Contains(message, "CodeArts/shared") {
		t.Fatalf("failure lost the client spelling: %#v", failure)
	}
	if message, _ := failure["message"].(string); strings.Contains(message, "codearts/shared") {
		t.Fatalf("failure used the canonical spelling only: %#v", failure)
	}
	if got := len(codearts.opened()); got != 1 {
		t.Fatalf("denied request reached the provider: OpenChat calls = %d, want 1", got)
	}
}

func TestMessagesQualifiedModelObservation(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	var observed []ProtocolObservation
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}},
		openStream: messagesChatStream("upstream-alias")}
	server := newTestServer(ServerOptions{
		Providers:        []providers.Provider{codearts},
		ProtocolObserver: func(observation ProtocolObservation) { observed = append(observed, observation) },
	})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"codearts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if len(observed) != 1 {
		t.Fatalf("observations = %d, want 1: %#v", len(observed), observed)
	}
	observation := observed[0]
	if observation.Model != "shared" {
		t.Fatalf("observation Model = %q, want shared", observation.Model)
	}
	if observation.RequestedModel != "codearts/shared" {
		t.Fatalf("observation RequestedModel = %q, want codearts/shared", observation.RequestedModel)
	}
	if observation.Realm != "codearts" {
		t.Fatalf("observation Realm = %q, want codearts", observation.Realm)
	}
}

func TestMessagesQualifiedModelDefaultMaxOutput(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}},
		openStream: messagesChatStream("shared")}
	server := newTestServer(ServerOptions{
		Providers: []providers.Provider{codearts},
		DefaultMaxOutputTokens: func(model string) int {
			if model == "shared" {
				return 128
			}
			return 0
		},
	})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"codearts/shared","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	requests := codearts.opened()
	if len(requests) != 1 {
		t.Fatalf("OpenChat calls = %d, want 1", len(requests))
	}
	if got := string(requests[0].Raw["max_tokens"]); got != "128" {
		t.Fatalf("defaulted max_tokens = %q, want 128", got)
	}
}

func TestMessagesBareModelRealmUnchanged(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	intl := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{intl, cn, codearts}})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages?realm=cn",
		`{"model":"shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	requests := cn.opened()
	if len(requests) != 1 {
		t.Fatalf("cn OpenChat calls = %d, want 1", len(requests))
	}
	if requests[0].Model != "shared" {
		t.Fatalf("ChatRequest.Model = %q, want shared", requests[0].Model)
	}
	if len(intl.opened()) != 0 || len(codearts.opened()) != 0 {
		t.Fatalf("bare request leaked intl=%d codearts=%d", len(intl.opened()), len(codearts.opened()))
	}
	if body := decodeBody(t, rec); body["model"] != "shared" {
		t.Fatalf("response model = %#v, want shared", body["model"])
	}
}

func TestMessagesGatewayKeyQualifiedExit(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}}, openStream: messagesChatStream("shared")}
	server := newTestServer(ServerOptions{
		Providers:   []providers.Provider{codearts},
		APIKeyRealm: func(*http.Request) string { return "gateway" },
	})

	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages",
		`{"model":"codearts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if len(codearts.opened()) != 1 {
		t.Fatalf("codearts OpenChat calls = %d, want 1", len(codearts.opened()))
	}
	if body := decodeBody(t, rec); body["model"] != "codearts/shared" {
		t.Fatalf("response model = %#v, want codearts/shared", body["model"])
	}
}

func TestMsgstatAffinityKey(t *testing.T) {
	t.Setenv("WB_MSGSTAT_SHARDS", "3")
	t.Setenv("WB_MSGSTAT_DAILY_SALT", "0")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(withKeyEntry(req.Context(), &keys.Entry{ID: "k1"}))
	chat := map[string]any{
		"model": "m1",
		"messages": []any{
			map[string]any{"role": "system", "content": "stable system"},
			map[string]any{"role": "user", "content": "one"},
		},
		"tools": []any{map[string]any{"function": map[string]any{"name": "b"}},
			map[string]any{"function": map[string]any{"name": "a"}}},
	}
	first := msgstatAffinityKey(req, chat, "m1")
	second := msgstatAffinityKey(req, chat, "m1")
	if first == "" || first != second || !strings.HasPrefix(first, "msgstat-") {
		t.Fatalf("affinity key = %q / %q", first, second)
	}
	streaming := map[string]any{"stream": true, "messages": chat["messages"]}
	if got := msgstatAffinityKey(req, streaming, "m1"); got != "" {
		t.Fatalf("streaming key = %q, want empty", got)
	}
	t.Setenv("WB_MSGSTAT_SHARDS", "0")
	if got := msgstatAffinityKey(req, chat, "m1"); got != "" {
		t.Fatalf("disabled key = %q, want empty", got)
	}
}
