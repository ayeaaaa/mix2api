package workbuddy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type chatTestCall struct {
	Method      string
	Path        string
	Header      http.Header
	RawBody     []byte
	Body        []byte
	ProxyTarget string
}

type chatTestFake struct {
	mu      sync.Mutex
	calls   []chatTestCall
	respond func(*http.Request, []byte) (int, http.Header, []byte)
	status  int
	header  http.Header
	body    string
}

func (f *chatTestFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := raw
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			body, _ = io.ReadAll(reader)
			_ = reader.Close()
		}
	}
	call := chatTestCall{
		Method:  r.Method,
		Path:    r.URL.Path,
		Header:  r.Header.Clone(),
		RawBody: append([]byte(nil), raw...),
		Body:    append([]byte(nil), body...),
	}
	if r.URL.IsAbs() {
		call.ProxyTarget = r.URL.String()
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	respond := f.respond
	status := f.status
	header := cloneTestHeader(f.header)
	responseBody := f.body
	f.mu.Unlock()

	if respond != nil {
		respondStatus, respondHeader, respondBody := respond(r, body)
		status, header, responseBody = respondStatus, respondHeader, string(respondBody)
	}
	if status == 0 {
		status = http.StatusOK
	}
	header = cloneTestHeader(header)
	if header == nil {
		header = make(http.Header)
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "text/event-stream")
	}
	for name, values := range header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, responseBody)
}

func cloneTestHeader(source http.Header) http.Header {
	if source == nil {
		return nil
	}
	return source.Clone()
}

func (f *chatTestFake) snapshot() []chatTestCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]chatTestCall, len(f.calls))
	for i, call := range f.calls {
		call.Header = call.Header.Clone()
		call.RawBody = append([]byte(nil), call.RawBody...)
		call.Body = append([]byte(nil), call.Body...)
		out[i] = call
	}
	return out
}

func newChatTestServer(t *testing.T, fake *chatTestFake) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return server
}

func chatRaw(t *testing.T, payload map[string]any) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal chat payload: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal chat payload: %v", err)
	}
	return raw
}

func chatRequest(t *testing.T, model string, payload map[string]any) providers.ChatRequest {
	t.Helper()
	raw := chatRaw(t, payload)
	messages := []json.RawMessage(nil)
	if value, ok := raw["messages"]; ok {
		_ = json.Unmarshal(value, &messages)
	}
	return providers.ChatRequest{
		Raw:      raw,
		Model:    model,
		Messages: messages,
	}
}

func ssePayload(events ...string) string {
	var builder strings.Builder
	for _, event := range events {
		builder.WriteString("data: ")
		builder.WriteString(event)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

func chatChunk(content, finish string) string {
	choice := map[string]any{"index": 0, "delta": map[string]any{"content": content}}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	payload := map[string]any{
		"id":      "up-1",
		"object":  "chat.completion.chunk",
		"model":   "deepseek-v4.1-flash",
		"choices": []any{choice},
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func drainChatFrames(t *testing.T, stream providers.ChatStream) []string {
	t.Helper()
	var frames []string
	for {
		frame, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream Next: %v", err)
		}
		frames = append(frames, string(frame))
	}
	return frames
}

func decodeChatBody(t *testing.T, call chatTestCall) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(call.Body))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode chat body (%s): %v", call.Body, err)
	}
	return body
}

func setupChatProvider(t *testing.T, realm, dir string, settings map[string]any) *Provider {
	t.Helper()
	writePoolSettings(t, dir, settings)
	provider, _ := newPoolProvider(t, realm, dir)
	return provider
}

func writeChatAccount(t *testing.T, dir, uid, realm string, fields map[string]any) {
	t.Helper()
	writePoolAccount(t, dir, uid, realm, fields)
}

func accountByUID(t *testing.T, pool *Pool, uid string) *Account {
	t.Helper()
	account := pool.Get(uid)
	if account == nil {
		t.Fatalf("account %q not found", uid)
	}
	return account
}

func chatStreamUsage(t *testing.T, stream providers.ChatStream) StreamUsage {
	t.Helper()
	reporter, ok := stream.(StreamUsageReporter)
	if !ok {
		t.Fatalf("stream %T does not report usage", stream)
	}
	return reporter.StreamUsage()
}

func boolPointer(value bool) *bool { return &value }

// ---------------------------------------------------------------------------
// Headers, identity, body
// ---------------------------------------------------------------------------

func TestChatIntlDesktopHeadersAndBody(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-1", realmIntl, map[string]any{
		"product":  productDesktop,
		"platform": "CLI",
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{body: ssePayload(chatChunk("hello", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	raw := map[string]any{
		"model":                 "deepseek-v4.1-flash",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":                true,
		"max_completion_tokens": 64,
		"stop":                  "END",
		"metadata":              map[string]any{"trace": "keep"},
	}
	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", raw))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("chat calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.Method != http.MethodPost || call.Path != "/v2/chat/completions" {
		t.Fatalf("request = %s %s, want POST /v2/chat/completions", call.Method, call.Path)
	}
	headers := call.Header
	checks := map[string]string{
		"Authorization":       "Bearer access-intl-1",
		"X-Domain":            "www.workbuddy.ai",
		"User-Agent":          "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0",
		"Origin":              "https://www.workbuddy.ai",
		"Referer":             "https://www.workbuddy.ai/",
		"X-CodeBuddy-Request": "1",
		"Accept-Language":     "en-US",
		"Content-Type":        "application/json",
	}
	for name, want := range checks {
		if got := headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	for _, forbidden := range []string{"X-Device-Token", "X-API-Key", "Content-Encoding", "X-Model-ID"} {
		if got := headers.Get(forbidden); got != "" {
			t.Fatalf("unexpected %s = %q", forbidden, got)
		}
	}
	if headers.Get("X-Request-ID") != headers.Get("X-Conversation-Message-ID") {
		t.Fatalf("desktop message/request ids differ: %q vs %q",
			headers.Get("X-Request-ID"), headers.Get("X-Conversation-Message-ID"))
	}
	if len(headers.Get("X-Conversation-ID")) != 36 {
		t.Fatalf("conversation id = %q, want dashed UUID", headers.Get("X-Conversation-ID"))
	}

	body := decodeChatBody(t, call)
	if body["stream"] != true {
		t.Fatalf("stream = %#v, want true", body["stream"])
	}
	options, _ := body["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("stream_options = %#v", body["stream_options"])
	}
	if got, _ := body["max_tokens"].(json.Number).Int64(); got != 64 {
		t.Fatalf("max_tokens = %#v, want 64", body["max_tokens"])
	}
	if _, exists := body["max_completion_tokens"]; exists {
		t.Fatalf("max_completion_tokens was not translated: %#v", body)
	}
	stop, _ := body["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop = %#v, want [END]", body["stop"])
	}
	if metadata, _ := body["metadata"].(map[string]any); metadata["trace"] != "keep" {
		t.Fatalf("desktop metadata was not preserved: %#v", body["metadata"])
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want injected system + user", messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != defaultChatSystemPrompt {
		t.Fatalf("first message = %#v", first)
	}
}

func TestChatCNHeadersDeviceTokenAPIKeyAndEnterprise(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token": "cn-device-secret"},
	})
	writeChatAccount(t, dir, "cn-api", realmCN, map[string]any{
		"accessToken":  "ck_public.secret-value",
		"refreshToken": "",
		"product":      productDesktop,
		"apiKey":       true,
		"enterpriseId": "ent-1",
	})
	provider, _ := newPoolProvider(t, realmCN, dir)
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "hy3", map[string]any{
		"model":    "hy3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("chat calls = %d, want 1", len(calls))
	}
	headers := calls[0].Header
	if got := headers.Get("Origin"); got != "https://www.codebuddy.cn" {
		t.Fatalf("Origin = %q", got)
	}
	if got := headers.Get("X-Domain"); got != "copilot.tencent.com" {
		t.Fatalf("X-Domain = %q", got)
	}
	if got := headers.Get("X-Product"); got != "SaaS" {
		t.Fatalf("X-Product = %q", got)
	}
	if got := headers.Get("X-Device-Token"); got != "cn-device-secret" {
		t.Fatalf("X-Device-Token = %q", got)
	}
	if got := headers.Get("X-API-Key"); got != "ck_public.secret-value" {
		t.Fatalf("X-API-Key = %q", got)
	}
	if got := headers.Get("Authorization"); got != "Bearer ck_public.secret-value" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("X-Enterprise-Id"); got != "ent-1" {
		t.Fatalf("X-Enterprise-Id = %q", got)
	}
	if got := headers.Get("X-Tenant-Id"); got != "ent-1" {
		t.Fatalf("X-Tenant-Id = %q", got)
	}
	if got := headers.Get("Accept-Language"); got != "zh-CN" {
		t.Fatalf("Accept-Language = %q", got)
	}
}

func TestChatCLIGzipBodyDropsAndCacheRetention(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-cli", realmIntl, map[string]any{
		"product":  productCLI,
		"platform": "CLI",
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":                "deepseek-v4.1-flash",
		"messages":             []any{map[string]any{"role": "user", "content": "hi"}},
		"metadata":             map[string]any{"trace": "drop"},
		"conversation_id":      "drop-conv",
		"previous_response_id": "drop-prev",
		"extra_field":          "keep",
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("chat calls = %d, want 1", len(calls))
	}
	headers := calls[0].Header
	if got := headers.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	for _, forbidden := range []string{"Origin", "Referer", "X-CodeBuddy-Request", "Accept-Language", "X-Requested-With"} {
		if got := headers.Get(forbidden); got != "" {
			t.Fatalf("CLI request sent %s = %q", forbidden, got)
		}
	}
	if got := headers.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if !bytes.HasPrefix(calls[0].RawBody, []byte{0x1f, 0x8b}) {
		t.Fatalf("request body is not gzip: %x", calls[0].RawBody[:minInt(len(calls[0].RawBody), 4)])
	}
	if bytes.Contains(calls[0].Body, []byte(`"stream": true`)) {
		t.Fatalf("body is not compact JSON: %s", calls[0].Body)
	}
	body := decodeChatBody(t, calls[0])
	for _, key := range []string{"metadata", "conversation_id", "previous_response_id"} {
		if _, exists := body[key]; exists {
			t.Fatalf("CLI body kept %s: %#v", key, body)
		}
	}
	if body["cache_retention"] != "long" {
		t.Fatalf("cache_retention = %#v", body["cache_retention"])
	}
	if body["extra_field"] != "keep" {
		t.Fatalf("unknown field was dropped: %#v", body)
	}
	if body["stream"] != true {
		t.Fatalf("stream = %#v", body["stream"])
	}
	if got, _ := body["max_tokens"].(json.Number).Int64(); got != 128000 {
		t.Fatalf("default max_tokens = %#v", body["max_tokens"])
	}
}

func TestChatVSCodeHeadersGzipDeviceToken(t *testing.T) {
	t.Run("intl non-enterprise placeholder", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "intl-vsc", realmIntl, map[string]any{
			"product":  productVSCode,
			"platform": "VSCode",
		})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		defer stream.Close()

		calls := fake.snapshot()
		if len(calls) != 1 {
			t.Fatalf("chat calls = %d", len(calls))
		}
		headers := calls[0].Header
		if got := headers.Get("X-Device-Token-Error"); got != "sdk_not_initialized" {
			t.Fatalf("X-Device-Token-Error = %q", got)
		}
		if got := headers.Get("X-Device-Token"); got != "" {
			t.Fatalf("unexpected X-Device-Token = %q", got)
		}
		if got := headers.Get("X-Model-ID"); got != "deepseek-v4.1-flash" {
			t.Fatalf("X-Model-ID = %q", got)
		}
		if got := headers.Get("X-Product-Version"); got != "4.12.39997873" {
			t.Fatalf("X-Product-Version = %q", got)
		}
		if got := headers.Get("X-CodeBuddy-Request"); got != "1" {
			t.Fatalf("X-CodeBuddy-Request = %q", got)
		}
		if got := headers.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q", got)
		}
		_ = decodeChatBody(t, calls[0])
	})

	t.Run("cn enterprise device token", func(t *testing.T) {
		dir := t.TempDir()
		writePoolSettings(t, dir, map[string]any{
			"upstream": map[string]any{"device_token": "cn-device-vsc"},
		})
		writeChatAccount(t, dir, "cn-vsc", realmCN, map[string]any{
			"product":      productVSCode,
			"platform":     "VSCode",
			"enterpriseId": "ent-vsc",
		})
		provider, _ := newPoolProvider(t, realmCN, dir)
		fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "hy3", map[string]any{
			"model":    "hy3",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		defer stream.Close()

		headers := fake.snapshot()[0].Header
		if got := headers.Get("X-Device-Token"); got != "cn-device-vsc" {
			t.Fatalf("X-Device-Token = %q", got)
		}
		if got := headers.Get("X-Device-Token-Error"); got != "" {
			t.Fatalf("unexpected X-Device-Token-Error = %q", got)
		}
	})
}

func TestChatPromptCacheKeyOptIn(t *testing.T) {
	t.Setenv("WB_PROMPT_CACHE_KEY", "1")
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-cache", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":           "deepseek-v4.1-flash",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"conversation_id": "conv-cache",
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()
	body := decodeChatBody(t, fake.snapshot()[0])
	key, _ := body["prompt_cache_key"].(string)
	if !strings.HasPrefix(key, "wb2a-intl-cac-") || len(key) != len("wb2a-intl-cac-")+32 {
		t.Fatalf("prompt_cache_key = %q", key)
	}

	// An explicit client key wins.
	stream.Close()
	fake2 := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	provider.chatBaseURL = newChatTestServer(t, fake2).URL
	stream, err = provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":            "deepseek-v4.1-flash",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"prompt_cache_key": "client-key",
	}))
	if err != nil {
		t.Fatalf("OpenChat explicit key: %v", err)
	}
	defer stream.Close()
	if got := decodeChatBody(t, fake2.snapshot()[0])["prompt_cache_key"]; got != "client-key" {
		t.Fatalf("explicit prompt_cache_key = %#v", got)
	}
}

func TestChatToolsImagesAndReasoning(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-tools", realmIntl, map[string]any{
		"product": productDesktop,
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	payload := map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "developer", "content": "be helpful"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			}},
		},
		"functions": []any{map[string]any{
			"name":        "lookup",
			"description": "look things up",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "str"},
					"limit": map[string]any{"type": "int"},
				},
				"required": []any{"query", "", nil},
			},
		}},
		"tool_choice": map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "lookup"},
		},
		"max_completion_tokens": 32,
		"stop":                  "",
		"reasoning_effort":      "extreme",
	}
	// This payload exercises the legacy root-level functions alias, which the
	// normalization must wrap into the modern tools shape.
	raw := chatRaw(t, payload)
	stream, err := provider.OpenChat(context.Background(), providers.ChatRequest{
		Raw:   raw,
		Model: "deepseek-v4.1-flash",
	})
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	body := decodeChatBody(t, fake.snapshot()[0])
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool type = %#v", tool["type"])
	}
	function, _ := tool["function"].(map[string]any)
	if function["name"] != "lookup" {
		t.Fatalf("tool function = %#v", function)
	}
	parameters, _ := function["parameters"].(map[string]any)
	if parameters["type"] != "object" {
		t.Fatalf("parameters type = %#v", parameters["type"])
	}
	properties, _ := parameters["properties"].(map[string]any)
	query, _ := properties["query"].(map[string]any)
	limit, _ := properties["limit"].(map[string]any)
	if query["type"] != "string" || limit["type"] != "integer" {
		t.Fatalf("schema types were not normalized: %#v", properties)
	}
	required, _ := parameters["required"].([]any)
	if len(required) != 1 || required[0] != "query" {
		t.Fatalf("required = %#v", parameters["required"])
	}
	if body["tool_choice"] != "lookup" {
		t.Fatalf("tool_choice = %#v", body["tool_choice"])
	}
	if _, exists := body["functions"]; exists {
		t.Fatalf("legacy functions key survived: %#v", body)
	}
	if got, _ := body["max_tokens"].(json.Number).Int64(); got != 32 {
		t.Fatalf("max_tokens = %#v", body["max_tokens"])
	}
	if _, exists := body["stop"]; exists {
		t.Fatalf("empty stop was not removed: %#v", body["stop"])
	}
	messages, _ := body["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be helpful" {
		t.Fatalf("developer role was not mapped: %#v", first)
	}
	user, _ := messages[1].(map[string]any)
	content, _ := user["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("image content was lost: %#v", user["content"])
	}
	if image, _ := content[1].(map[string]any); image["type"] != "image_url" {
		t.Fatalf("image block = %#v", content[1])
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", body["thinking"])
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want mapped high", body["reasoning_effort"])
	}
}

// ---------------------------------------------------------------------------
// Retry, refresh, and failover
// ---------------------------------------------------------------------------

func TestChat401ForceRefreshRetriesSameAccount(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-401", realmIntl, map[string]any{
		"product":      productDesktop,
		"refreshToken": "refresh-old",
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})

	tokenFake := &tokenTestFake{body: `{"data":{"accessToken":"access-new","refreshToken":"refresh-new","expiresAt":9999999999}}`}
	tokenServer := httptest.NewServer(tokenFake)
	defer tokenServer.Close()
	provider.tokenBaseURL = tokenServer.URL

	chatFake := &chatTestFake{respond: func(r *http.Request, _ []byte) (int, http.Header, []byte) {
		if r.Header.Get("Authorization") == "Bearer access-new" {
			return 200, http.Header{"Content-Type": []string{"text/event-stream"}},
				[]byte(ssePayload(chatChunk("ok", "stop"), "[DONE]"))
		}
		return 401, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":{"message":"expired"}}`)
	}}
	chatServer := newChatTestServer(t, chatFake)
	provider.chatBaseURL = chatServer.URL

	request := chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	request.SessionKey = "session-1"
	stream, err := provider.OpenChat(context.Background(), request)
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	chatCalls := chatFake.snapshot()
	if len(chatCalls) != 2 {
		t.Fatalf("chat calls = %d, want 2", len(chatCalls))
	}
	if got := chatCalls[1].Header.Get("Authorization"); got != "Bearer access-new" {
		t.Fatalf("retry Authorization = %q", got)
	}
	if first, second := chatCalls[0].Header.Get("X-Conversation-ID"), chatCalls[1].Header.Get("X-Conversation-ID"); first == "" || first != second {
		t.Fatalf("conversation id changed across retry: %q vs %q", first, second)
	}
	if chatCalls[0].Header.Get("X-Request-ID") == chatCalls[1].Header.Get("X-Request-ID") {
		t.Fatal("per-request message id was reused across retry")
	}
	tokenCalls := tokenFake.snapshot()
	if len(tokenCalls) != 1 {
		t.Fatalf("token refresh calls = %d, want 1", len(tokenCalls))
	}
	if got := tokenCalls[0].Header.Get("X-Refresh-Token"); got != "refresh-old" {
		t.Fatalf("refresh header = %q", got)
	}
	account := accountByUID(t, provider.Pool(), "intl-401")
	account.mu.RLock()
	access := account.Auth.AccessToken
	account.mu.RUnlock()
	if access != "access-new" {
		t.Fatalf("account access token = %q", access)
	}
}

func TestChat401SecondTerminalNoLoop(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-401b", realmIntl, map[string]any{
		"product":      productDesktop,
		"refreshToken": "refresh-old",
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})

	tokenFake := &tokenTestFake{body: `{"data":{"accessToken":"access-new","refreshToken":"refresh-new","expiresAt":9999999999}}`}
	tokenServer := httptest.NewServer(tokenFake)
	defer tokenServer.Close()
	provider.tokenBaseURL = tokenServer.URL

	chatFake := &chatTestFake{status: 401, body: `{"error":{"message":"expired"}}`}
	chatServer := newChatTestServer(t, chatFake)
	provider.chatBaseURL = chatServer.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if stream != nil {
		stream.Close()
	}
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want APIError", err, err)
	}
	if got := apiErr.HTTPStatus(); got != http.StatusBadGateway {
		t.Fatalf("HTTPStatus = %d, want 502", got)
	}
	if calls := chatFake.snapshot(); len(calls) != 2 {
		t.Fatalf("chat calls = %d, want exactly 2", len(calls))
	}
	account := accountByUID(t, provider.Pool(), "intl-401b")
	account.mu.RLock()
	disabled, manual, tokenExpired := account.State.Disabled, account.State.ManualDisabled, account.tokenExpired
	account.mu.RUnlock()
	if manual || !disabled || !tokenExpired {
		t.Fatalf("second 401 state disabled=%v manual=%v tokenExpired=%v", disabled, manual, tokenExpired)
	}
}

func TestChat429FailoverAndCooldowns(t *testing.T) {
	t.Run("model scoped", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "a-429", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "b-429", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{respond: func(r *http.Request, _ []byte) (int, http.Header, []byte) {
			if strings.Contains(r.Header.Get("Authorization"), "a-429") {
				return 429, http.Header{"Content-Type": []string{"application/json"}},
					[]byte(`{"code":6004,"msg":"usage exceeds frequency limit; reset at 2099-01-01 00:00:00 UTC+8"}`)
			}
			return 200, http.Header{"Content-Type": []string{"text/event-stream"}},
				[]byte(ssePayload(chatChunk("ok", "stop"), "[DONE]"))
		}}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		defer stream.Close()

		if calls := fake.snapshot(); len(calls) != 2 {
			t.Fatalf("chat calls = %d, want 2", len(calls))
		}
		account := accountByUID(t, provider.Pool(), "a-429")
		now := time.Now()
		if !account.modelCooling("deepseek-v4.1-flash", now) {
			t.Fatal("model cooldown was not recorded")
		}
		if account.State.CoolUntil.After(now) {
			t.Fatalf("model-scoped 429 parked the whole account until %v", account.State.CoolUntil)
		}
		if got := chatStreamUsage(t, stream).Account; got != "b-429" {
			t.Fatalf("serving account = %q", got)
		}
	})

	t.Run("shortest cooldown wins", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "a-429", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "b-429", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{respond: func(r *http.Request, _ []byte) (int, http.Header, []byte) {
			if strings.Contains(r.Header.Get("Authorization"), "a-429") {
				return 429, http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"30"}},
					[]byte(`{"error":{"message":"this account rate limit exceeded"}}`)
			}
			return 429, http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"90"}},
				[]byte(`{"error":{"message":"this account rate limit exceeded"}}`)
		}}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusTooManyRequests {
			t.Fatalf("error = %v, want 429", err)
		}
		if apiErr.RetryAfter != 30*time.Second {
			t.Fatalf("final RetryAfter = %v, want shortest 30s", apiErr.RetryAfter)
		}
		if apiErr.ResetAt.IsZero() || time.Until(apiErr.ResetAt) > 31*time.Second {
			t.Fatalf("final ResetAt = %v", apiErr.ResetAt)
		}
	})

	t.Run("shortest existing pool window wins", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "a-429", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "b-429", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		accountA := accountByUID(t, provider.Pool(), "a-429")
		if err := provider.Pool().NoteModelError(accountA, "deepseek-v4.1-flash", "existing window", 10*time.Second); err != nil {
			t.Fatalf("NoteModelError: %v", err)
		}
		fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
			return 429, http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"60"}},
				[]byte(`{"error":{"message":"this account rate limit exceeded"}}`)
		}}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusTooManyRequests {
			t.Fatalf("error = %v, want 429", err)
		}
		if apiErr.RetryAfter > 10*time.Second {
			t.Fatalf("existing pool window was not preferred: RetryAfter=%v", apiErr.RetryAfter)
		}
	})

	t.Run("account scoped", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "a-429", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "b-429", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{respond: func(r *http.Request, _ []byte) (int, http.Header, []byte) {
			if strings.Contains(r.Header.Get("Authorization"), "a-429") {
				return 429, http.Header{"Content-Type": []string{"application/json"}},
					[]byte(`{"error":{"message":"this account rate limit exceeded"}}`)
			}
			return 200, http.Header{"Content-Type": []string{"text/event-stream"}},
				[]byte(ssePayload(chatChunk("ok", "stop"), "[DONE]"))
		}}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		defer stream.Close()

		account := accountByUID(t, provider.Pool(), "a-429")
		if !account.State.CoolUntil.After(time.Now()) {
			t.Fatalf("account cooldown = %v, want future", account.State.CoolUntil)
		}
	})
}

func TestChat400NoFailoverAndNoSecretLeak(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "a-400", realmIntl, map[string]any{"product": productDesktop})
	writeChatAccount(t, dir, "b-400", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
		return 400, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":{"message":"bad request echoed access-a-400 and Bearer access-a-400"}}`)
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if stream != nil {
		stream.Close()
	}
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if apiErr.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("HTTPStatus = %d, want 400", apiErr.HTTPStatus())
	}
	if calls := fake.snapshot(); len(calls) != 1 {
		t.Fatalf("400 failover happened: %d calls", len(calls))
	}
	if strings.Contains(apiErr.SafeMessage(), "access-a-400") {
		t.Fatalf("safe message leaks token: %q", apiErr.SafeMessage())
	}
	if strings.Contains(err.Error(), "access-a-400") {
		t.Fatalf("error leaks token: %v", err)
	}
}

func TestChat500RedactsAccountLastError(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "a-leak", realmIntl, map[string]any{
		"product": productDesktop,
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
		return 500, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":{"message":"upstream received access-a-leak"}}`)
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "access-a-leak") {
		t.Fatalf("error leaks token: %v", err)
	}
	account := accountByUID(t, provider.Pool(), "a-leak")
	account.mu.RLock()
	lastError := account.State.LastError
	account.mu.RUnlock()
	if strings.Contains(lastError, "access-a-leak") {
		t.Fatalf("account LastError leaks token: %q", lastError)
	}
}

func TestChat5xxRetriesSameAccountOnce(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "a-500", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	var mu sync.Mutex
	calls := 0
	fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			return 500, http.Header{"Content-Type": []string{"application/json"}},
				[]byte(`{"error":{"message":"upstream exploded"}}`)
		}
		return 200, http.Header{"Content-Type": []string{"text/event-stream"}},
			[]byte(ssePayload(chatChunk("ok", "stop"), "[DONE]"))
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("single-account 5xx attempts = %d, want 2", got)
	}
}

func TestChat5xxFailoverAndErrorStatus(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "a-500", realmIntl, map[string]any{"product": productDesktop})
	writeChatAccount(t, dir, "b-500", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{respond: func(r *http.Request, _ []byte) (int, http.Header, []byte) {
		if strings.Contains(r.Header.Get("Authorization"), "a-500") {
			return 500, http.Header{"Content-Type": []string{"application/json"}},
				[]byte(`{"error":{"message":"upstream exploded"}}`)
		}
		return 200, http.Header{"Content-Type": []string{"text/event-stream"}},
			[]byte(ssePayload(chatChunk("ok", "stop"), "[DONE]"))
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	if calls := fake.snapshot(); len(calls) != 2 {
		t.Fatalf("5xx failover calls = %d, want 2", len(calls))
	}
}

func TestChatNetworkClassificationAndFailover(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "a-net", realmIntl, map[string]any{"product": productDesktop})
	writeChatAccount(t, dir, "b-net", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	server := newChatTestServer(t, &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")})
	provider.chatBaseURL = server.URL

	var mu sync.Mutex
	failures := 0
	provider.chatDoer = chatDoerFunc(func(ctx context.Context, req *http.Request, proxy string) (*http.Response, error) {
		mu.Lock()
		first := failures == 0
		failures++
		mu.Unlock()
		if first {
			return nil, errors.New("connection reset by peer")
		}
		return http.DefaultClient.Do(req)
	})

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	// A single-account pool with a non-local transport failure maps to 502 and
	// charges the account once.
	dir2 := t.TempDir()
	writeChatAccount(t, dir2, "only-net", realmIntl, map[string]any{"product": productDesktop})
	provider2 := setupChatProvider(t, realmIntl, dir2, map[string]any{})
	provider2.chatBaseURL = server.URL
	provider2.chatDoer = chatDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})
	_, err = provider2.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("HTTPStatus = %d, want 502", apiErr.HTTPStatus())
	}
	account2 := accountByUID(t, provider2.Pool(), "only-net")
	account2.mu.RLock()
	errCount2 := account2.State.ErrCount
	account2.mu.RUnlock()
	if errCount2 == 0 {
		t.Fatal("non-local transport failure did not charge the account")
	}

	// Local DNS/route failures are 503 and must not charge an account.
	dir3 := t.TempDir()
	writeChatAccount(t, dir3, "only-local", realmIntl, map[string]any{"product": productDesktop})
	provider3 := setupChatProvider(t, realmIntl, dir3, map[string]any{})
	provider3.chatBaseURL = server.URL
	provider3.chatDoer = chatDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "upstream.invalid", IsNotFound: true}
	})
	_, err = provider3.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("local network OpenChat unexpectedly succeeded")
	}
	if !errors.As(err, &apiErr) {
		t.Fatalf("local error = %T %v", err, err)
	}
	if apiErr.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("local HTTPStatus = %d, want 503", apiErr.HTTPStatus())
	}
	account3 := accountByUID(t, provider3.Pool(), "only-local")
	account3.mu.RLock()
	errCount3 := account3.State.ErrCount
	account3.mu.RUnlock()
	if errCount3 != 0 {
		t.Fatalf("local network failure charged the account: ErrCount=%d", errCount3)
	}
}

func TestChatGuardAndBusyAdmission429(t *testing.T) {
	t.Run("busy account cap", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "busy-1", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{
			"workbuddy": map[string]any{"intl": map[string]any{"account_max_concurrent": 1}},
		})
		fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL
		account := accountByUID(t, provider.Pool(), "busy-1")
		if !provider.Pool().Acquire(account) {
			t.Fatal("manual Acquire failed")
		}
		defer provider.Pool().Release(account)

		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("busy pool unexpectedly succeeded")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusTooManyRequests {
			t.Fatalf("busy error = %v, want 429", err)
		}
		if apiErr.RetryAfter <= 0 || apiErr.RetryAfter > time.Minute {
			t.Fatalf("busy RetryAfter = %v", apiErr.RetryAfter)
		}
		if !strings.Contains(strings.ToLower(apiErr.SafeMessage()), "concurrency") {
			t.Fatalf("busy message = %q", apiErr.SafeMessage())
		}
		if calls := fake.snapshot(); len(calls) != 0 {
			t.Fatalf("busy pool made %d upstream calls", len(calls))
		}
	})

	t.Run("daily guard", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "guard-1", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL
		account := accountByUID(t, provider.Pool(), "guard-1")
		one := int64(1)
		account.mu.Lock()
		account.DailyTokenLimit = 1
		account.DailyTokensToday = &one
		account.mu.Unlock()

		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("guarded pool unexpectedly succeeded")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusTooManyRequests {
			t.Fatalf("guard error = %v, want 429", err)
		}
		if !strings.Contains(strings.ToLower(apiErr.SafeMessage()), "token limit") {
			t.Fatalf("guard message = %q", apiErr.SafeMessage())
		}
		if apiErr.ResetAt.IsZero() || !apiErr.ResetAt.After(time.Now()) {
			t.Fatalf("guard ResetAt = %v", apiErr.ResetAt)
		}
		if calls := fake.snapshot(); len(calls) != 0 {
			t.Fatalf("guarded pool made %d upstream calls", len(calls))
		}
	})
}

func TestChatMaxConcurrentAdmission(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-admission", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{
		"workbuddy": map[string]any{"intl": map[string]any{"max_concurrent": 1}},
	})
	shared := NewAdmissionWithWait(1, 80*time.Millisecond)
	provider.SetAdmission(shared)
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	request := chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	first, err := provider.OpenChat(context.Background(), request)
	if err != nil {
		t.Fatalf("first OpenChat: %v", err)
	}
	if got := shared.InFlight(); got != 1 {
		t.Fatalf("in-flight permits = %d, want 1", got)
	}

	_, err = provider.OpenChat(context.Background(), request)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("second OpenChat err = %v, want 503 busy", err)
	}
	if !strings.Contains(strings.ToLower(apiErr.SafeMessage()), "concurrent") {
		t.Fatalf("busy message = %q", apiErr.SafeMessage())
	}
	if calls := fake.snapshot(); len(calls) != 1 {
		t.Fatalf("blocked request reached upstream %d times, want 1", len(calls))
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}
	if got := shared.InFlight(); got != 0 {
		t.Fatalf("in-flight permits after Close = %d, want 0", got)
	}
	second, err := provider.OpenChat(context.Background(), request)
	if err != nil {
		t.Fatalf("OpenChat after Close: %v", err)
	}
	_ = drainChatFrames(t, second)
	// Reading EOF releases the permit even when the caller forgets Close.
	if got := shared.InFlight(); got != 0 {
		t.Fatalf("in-flight permits after EOF = %d, want 0", got)
	}
	third, err := provider.OpenChat(context.Background(), request)
	if err != nil {
		t.Fatalf("OpenChat after EOF release: %v", err)
	}
	_ = third.Close()
}

func TestChatSharedAdmissionAcrossRealmsAndFallback(t *testing.T) {
	newSharedProviders := func(t *testing.T, fallback bool) (*Provider, *Provider, *Admission) {
		t.Helper()
		dir := t.TempDir()
		writeChatAccount(t, dir, "intl-shared", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "cn-shared", realmCN, map[string]any{"product": productDesktop})
		writePoolSettings(t, dir, map[string]any{
			"workbuddy": map[string]any{
				"intl": map[string]any{
					"max_concurrent":         1,
					"realm_fallback_enabled": fallback,
				},
				"cn": map[string]any{"max_concurrent": 1},
			},
		})
		intl, _ := newPoolProvider(t, realmIntl, dir)
		cn, _ := newPoolProvider(t, realmCN, dir)
		intl.SetPeer(cn)
		cn.SetPeer(intl)
		shared := NewAdmissionWithWait(1, 80*time.Millisecond)
		intl.SetAdmission(shared)
		cn.SetAdmission(shared)
		return intl, cn, shared
	}

	t.Run("one shared cap across realms", func(t *testing.T) {
		intl, cn, shared := newSharedProviders(t, false)
		intlFake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		cnFake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		intl.chatBaseURL = newChatTestServer(t, intlFake).URL
		cn.chatBaseURL = newChatTestServer(t, cnFake).URL
		request := chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})

		intlStream, err := intl.OpenChat(context.Background(), request)
		if err != nil {
			t.Fatalf("intl OpenChat: %v", err)
		}
		_, err = cn.OpenChat(context.Background(), request)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusServiceUnavailable {
			t.Fatalf("cn OpenChat while intl holds the shared permit = %v, want 503 busy", err)
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("saturated cn request reached upstream %d times", len(calls))
		}
		_ = intlStream.Close()
		if got := shared.InFlight(); got != 0 {
			t.Fatalf("shared in-flight after intl close = %d", got)
		}
		cnStream, err := cn.OpenChat(context.Background(), request)
		if err != nil {
			t.Fatalf("cn OpenChat after release: %v", err)
		}
		_ = cnStream.Close()
	})

	t.Run("fallback reuses the outer permit", func(t *testing.T) {
		intl, cn, shared := newSharedProviders(t, true)
		intlFake := &chatTestFake{status: http.StatusTooManyRequests, body: `{"error":{"message":"throttled"}}`}
		cnFake := &chatTestFake{body: ssePayload(chatChunk("cn", "stop"), "[DONE]")}
		intl.chatBaseURL = newChatTestServer(t, intlFake).URL
		cn.chatBaseURL = newChatTestServer(t, cnFake).URL
		request := chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		stream, err := intl.OpenChat(context.Background(), request)
		if err != nil {
			t.Fatalf("fallback OpenChat: %v", err)
		}
		if got := shared.InFlight(); got != 1 {
			t.Fatalf("fallback in-flight permits = %d, want exactly 1", got)
		}
		if calls := cnFake.snapshot(); len(calls) != 1 {
			t.Fatalf("cn fallback calls = %d, want 1", len(calls))
		}
		_ = stream.Close()
		if got := shared.InFlight(); got != 0 {
			t.Fatalf("fallback in-flight after Close = %d, want 0", got)
		}
	})
}

func TestChatAdmissionLimitIncreaseWakesWaiter(t *testing.T) {
	admission := NewAdmissionWithWait(1, time.Second)
	if err := admission.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- admission.Acquire(context.Background())
	}()
	time.Sleep(20 * time.Millisecond)
	admission.SetLimit(2)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter after limit increase: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not woken by limit increase")
	}
	admission.Release()
	admission.Release()
}

func TestChatAdmissionReleasedOnOpenError(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-admission-error", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{
		"workbuddy": map[string]any{"intl": map[string]any{"max_concurrent": 1}},
	})
	provider.SetAdmission(NewAdmissionWithWait(1, 100*time.Millisecond))
	fake := &chatTestFake{
		status: http.StatusBadRequest,
		body:   `{"error":{"message":"bad request"}}`,
	}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	request := chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, err := provider.OpenChat(context.Background(), request); err == nil {
		t.Fatal("invalid upstream request unexpectedly succeeded")
	}

	fake.mu.Lock()
	fake.status = http.StatusOK
	fake.body = ssePayload(chatChunk("ok", "stop"), "[DONE]")
	fake.mu.Unlock()
	stream, err := provider.OpenChat(context.Background(), request)
	if err != nil {
		t.Fatalf("OpenChat after upstream error: %v", err)
	}
	_ = stream.Close()
}

func TestChat403RestrictedRotatesButContentDoesNot(t *testing.T) {
	t.Run("restricted rotates", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "a-403", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "b-403", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{
			"workbuddy": map[string]any{"intl": map[string]any{"restricted_403_rotate_max": 1}},
		})
		fake := &chatTestFake{respond: func(r *http.Request, _ []byte) (int, http.Header, []byte) {
			if strings.Contains(r.Header.Get("Authorization"), "a-403") {
				return 403, http.Header{"Content-Type": []string{"application/json"}},
					[]byte(`{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`)
			}
			return 200, http.Header{"Content-Type": []string{"text/event-stream"}},
				[]byte(ssePayload(chatChunk("ok", "stop"), "[DONE]"))
		}}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		defer stream.Close()

		if calls := fake.snapshot(); len(calls) != 2 {
			t.Fatalf("restricted rotation calls = %d, want 2", len(calls))
		}
		account := accountByUID(t, provider.Pool(), "a-403")
		account.mu.RLock()
		banned := taskBanned(account.Auth)
		enabled := account.Auth.Enabled
		account.mu.RUnlock()
		if !banned || enabled {
			t.Fatalf("restricted account not banned: banned=%v enabled=%v", banned, enabled)
		}
	})

	t.Run("default stops rotation", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "a-403", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "b-403", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
			return 403, http.Header{"Content-Type": []string{"application/json"}},
				[]byte(`{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`)
		}}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusForbidden {
			t.Fatalf("error = %v, want 403", err)
		}
		if calls := fake.snapshot(); len(calls) != 1 {
			t.Fatalf("default restricted rotation made %d calls, want 1", len(calls))
		}
	})

	t.Run("content rejected passes through", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "a-403", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "b-403", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
			return 403, http.Header{"Content-Type": []string{"application/json"}},
				[]byte(`{"error":{"message":"content policy rejected"}}`)
		}}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL

		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusForbidden {
			t.Fatalf("error = %v, want 403", err)
		}
		if calls := fake.snapshot(); len(calls) != 1 {
			t.Fatalf("content 403 rotated: %d calls", len(calls))
		}
	})
}

// ---------------------------------------------------------------------------
// Cross-exit fallback
// ---------------------------------------------------------------------------

func TestChatCrossExitFallback(t *testing.T) {
	setup := func(t *testing.T, enabled bool) (*Provider, *chatTestFake, *chatTestFake) {
		dir := t.TempDir()
		settings := map[string]any{
			"workbuddy": map[string]any{
				"intl": map[string]any{"realm_fallback_enabled": enabled},
				"cn":   map[string]any{"enabled": true},
			},
		}
		writeChatAccount(t, dir, "intl-a", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "cn-a", realmCN, map[string]any{"product": productDesktop})
		writePoolSettings(t, dir, settings)
		intl, _ := newPoolProvider(t, realmIntl, dir)
		cn, _ := newPoolProvider(t, realmCN, dir)
		intl.SetPeer(cn)

		intlFake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
			return 429, http.Header{"Content-Type": []string{"application/json"}},
				[]byte(`{"error":{"message":"usage exceeds frequency limit"}}`)
		}}
		cnFake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		intlServer := newChatTestServer(t, intlFake)
		cnServer := newChatTestServer(t, cnFake)
		intl.chatBaseURL = intlServer.URL
		cn.chatBaseURL = cnServer.URL
		return intl, intlFake, cnFake
	}

	t.Run("enabled", func(t *testing.T) {
		provider, intlFake, cnFake := setup(t, true)
		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		defer stream.Close()
		if calls := intlFake.snapshot(); len(calls) != 1 {
			t.Fatalf("intl calls = %d", len(calls))
		}
		if calls := cnFake.snapshot(); len(calls) != 1 {
			t.Fatalf("cn fallback calls = %d", len(calls))
		}
		usage := chatStreamUsage(t, stream)
		if usage.Realm != realmCN || usage.Account != "cn-a" {
			t.Fatalf("usage realm/account = %q/%q", usage.Realm, usage.Account)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		provider, _, cnFake := setup(t, false)
		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusTooManyRequests {
			t.Fatalf("error = %v, want 429", err)
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("disabled fallback called cn %d times", len(calls))
		}
	})

	t.Run("auto switch product alone does not enable fallback", func(t *testing.T) {
		provider, _, cnFake := setup(t, false)
		if _, err := SaveConfig(provider.store, realmIntl, map[string]any{"auto_switch_product": true}); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("auto_switch_product alone fell back %d times", len(calls))
		}
	})

	t.Run("explicit request suppresses", func(t *testing.T) {
		provider, _, cnFake := setup(t, true)
		req := chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		req.AllowRealmFallback = boolPointer(false)
		_, err := provider.OpenChat(context.Background(), req)
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("explicit request fell back %d times", len(calls))
		}
	})

	t.Run("X-Realm header suppresses", func(t *testing.T) {
		provider, _, cnFake := setup(t, true)
		req := chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		req.Headers = http.Header{"X-Realm": []string{"intl"}}
		_, err := provider.OpenChat(context.Background(), req)
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("X-Realm request fell back %d times", len(calls))
		}
	})

	t.Run("exclusive model does not fall back", func(t *testing.T) {
		provider, _, cnFake := setup(t, true)
		_, err := provider.OpenChat(context.Background(), chatRequest(t, "gpt-6-astra", map[string]any{
			"model":    "gpt-6-astra",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("exclusive model fell back %d times", len(calls))
		}
	})

	t.Run("peer disabled does not fall back", func(t *testing.T) {
		dir := t.TempDir()
		writePoolSettings(t, dir, map[string]any{"workbuddy": map[string]any{
			"intl": map[string]any{"realm_fallback_enabled": true},
			"cn":   map[string]any{"enabled": false},
		}})
		writeChatAccount(t, dir, "intl-a", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "cn-a", realmCN, map[string]any{"product": productDesktop})
		intl, _ := newPoolProvider(t, realmIntl, dir)
		cn, _ := newPoolProvider(t, realmCN, dir)
		intl.SetPeer(cn)
		intlFake := &chatTestFake{status: 429, body: `{"error":{"message":"too many requests"}}`}
		cnFake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		intl.chatBaseURL = newChatTestServer(t, intlFake).URL
		cn.chatBaseURL = newChatTestServer(t, cnFake).URL

		_, err := intl.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("disabled peer was called %d times", len(calls))
		}
	})

	t.Run("peer without ready account does not fall back", func(t *testing.T) {
		dir := t.TempDir()
		writePoolSettings(t, dir, map[string]any{"workbuddy": map[string]any{
			"intl": map[string]any{"realm_fallback_enabled": true},
		}})
		writeChatAccount(t, dir, "intl-a", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "cn-a", realmCN, map[string]any{"product": productDesktop, "enabled": false})
		intl, _ := newPoolProvider(t, realmIntl, dir)
		cn, _ := newPoolProvider(t, realmCN, dir)
		intl.SetPeer(cn)
		intlFake := &chatTestFake{status: 429, body: `{"error":{"message":"too many requests"}}`}
		cnFake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		intl.chatBaseURL = newChatTestServer(t, intlFake).URL
		cn.chatBaseURL = newChatTestServer(t, cnFake).URL

		_, err := intl.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if calls := cnFake.snapshot(); len(calls) != 0 {
			t.Fatalf("unready peer was called %d times", len(calls))
		}
	})

	t.Run("failed fallback does not hop again", func(t *testing.T) {
		provider, intlFake, cnFake := setup(t, true)
		cnFake.respond = func(*http.Request, []byte) (int, http.Header, []byte) {
			return 429, http.Header{"Content-Type": []string{"application/json"}},
				[]byte(`{"error":{"message":"cn throttled"}}`)
		}
		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if calls := intlFake.snapshot(); len(calls) != 1 {
			t.Fatalf("intl calls = %d, want 1", len(calls))
		}
		if calls := cnFake.snapshot(); len(calls) != 1 {
			t.Fatalf("cn calls = %d, want 1 (no second hop)", len(calls))
		}
	})

	t.Run("live config enables fallback", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "intl-a", realmIntl, map[string]any{"product": productDesktop})
		writeChatAccount(t, dir, "cn-a", realmCN, map[string]any{"product": productDesktop})
		intl, _ := newPoolProvider(t, realmIntl, dir)
		cn, _ := newPoolProvider(t, realmCN, dir)
		intl.SetPeer(cn)
		intlFake := &chatTestFake{status: 429, body: `{"error":{"message":"too many requests"}}`}
		cnFake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		intl.chatBaseURL = newChatTestServer(t, intlFake).URL
		cn.chatBaseURL = newChatTestServer(t, cnFake).URL

		if _, err := SaveConfig(intl.store, realmIntl, map[string]any{"realm_fallback_enabled": true}); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
		stream, err := intl.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat after live config: %v", err)
		}
		defer stream.Close()
		if calls := cnFake.snapshot(); len(calls) != 1 {
			t.Fatalf("cn calls = %d, want 1", len(calls))
		}
	})
}

// ---------------------------------------------------------------------------
// SSE, usage, close, cancel, timeouts, proxy
// ---------------------------------------------------------------------------

func TestChatSSEFramesGzipUsageAndCloseOnce(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-sse", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})

	usageFrame := `{"id":"up-2","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"credit":2,"completion_tokens_details":{"reasoning_tokens":3},"prompt_tokens_details":{"cached_tokens":4}}}`
	payload := ssePayload(
		chatChunk("hel", ""),
		chatChunk("lo", ""),
		`{"id":"up-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		usageFrame,
		"[DONE]",
	)
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	gzipWriter.Header.ModTime = time.Time{}
	_, _ = gzipWriter.Write([]byte(payload))
	_ = gzipWriter.Close()

	fake := &chatTestFake{body: compressed.String(), header: http.Header{
		"Content-Type":     []string{"text/event-stream"},
		"Content-Encoding": []string{"gzip"},
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	account := accountByUID(t, provider.Pool(), "intl-sse")

	frame1, err := stream.Next()
	if err != nil {
		t.Fatalf("Next 1: %v", err)
	}
	if !strings.Contains(string(frame1), `"hel"`) || !strings.HasSuffix(string(frame1), "\n\n") {
		t.Fatalf("frame 1 = %q", frame1)
	}
	frames := drainChatFrames(t, stream)
	if len(frames) < 3 {
		t.Fatalf("frames after first = %#v", frames)
	}
	if !strings.Contains(frames[len(frames)-2], "usage") {
		t.Fatalf("usage frame not preserved: %#v", frames)
	}
	if strings.TrimSpace(frames[len(frames)-1]) != "data: [DONE]" {
		t.Fatalf("last frame = %q", frames[len(frames)-1])
	}
	usage := chatStreamUsage(t, stream)
	if usage.Realm != realmIntl || usage.Account != "intl-sse" || usage.Model != "deepseek-v4.1-flash" {
		t.Fatalf("usage identity = %+v", usage)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 ||
		usage.CachedTokens != 4 || usage.ReasoningTokens != 3 || usage.Credit != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.CostPolicy != "credits" {
		t.Fatalf("cost policy = %q", usage.CostPolicy)
	}

	// Prove Close releases exactly once even with a second acquired slot.
	if !provider.Pool().Acquire(account) {
		t.Fatal("second Acquire failed")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	account.mu.RLock()
	active := account.State.ActiveConcurrent
	account.mu.RUnlock()
	if active != 1 {
		t.Fatalf("active after double close = %d, want 1", active)
	}
	provider.Pool().Release(account)
}

func TestChatClientCancelReleasesOnce(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-cancel", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ssePayload(chatChunk("ok", "")))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	provider.chatBaseURL = server.URL

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := provider.OpenChat(ctx, chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	<-started
	if _, err := stream.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	cancel()
	if _, err := stream.Next(); err == nil {
		t.Fatal("Next after cancel succeeded")
	}
	_ = stream.Close()
	_ = stream.Close()
	account := accountByUID(t, provider.Pool(), "intl-cancel")
	account.mu.RLock()
	active := account.State.ActiveConcurrent
	account.mu.RUnlock()
	if active != 0 {
		t.Fatalf("active after cancel = %d", active)
	}
}

func TestChatProxyAndTimeouts(t *testing.T) {
	t.Run("account proxy is passed through", func(t *testing.T) {
		dir := t.TempDir()
		writePoolSettings(t, dir, map[string]any{
			"proxy_slots": []any{map[string]any{
				"id": "slot-1", "name": "p", "url": "http://proxy.example:8080", "enabled": true,
			}},
		})
		writeChatAccount(t, dir, "intl-proxy", realmIntl, map[string]any{
			"product":   productDesktop,
			"proxySlot": "slot-1",
		})
		provider, _ := newPoolProvider(t, realmIntl, dir)
		server := newChatTestServer(t, &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")})
		provider.chatBaseURL = server.URL
		var gotProxy string
		provider.chatDoer = chatDoerFunc(func(ctx context.Context, req *http.Request, proxy string) (*http.Response, error) {
			gotProxy = proxy
			return http.DefaultClient.Do(req)
		})
		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		stream.Close()
		if gotProxy != "http://proxy.example:8080" {
			t.Fatalf("proxy = %q", gotProxy)
		}
	})

	t.Run("header timeout", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "intl-timeout", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{
			"workbuddy": map[string]any{"intl": map[string]any{"timeout_seconds": 0.05}},
		})
		provider.chatDoer = chatDoerFunc(func(ctx context.Context, _ *http.Request, _ string) (*http.Response, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		start := time.Now()
		_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err == nil {
			t.Fatal("OpenChat unexpectedly succeeded")
		}
		if elapsed := time.Since(start); elapsed > 4*time.Second {
			t.Fatalf("header timeout took %v", elapsed)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadGateway {
			t.Fatalf("error = %v, want 502", err)
		}
	})

	t.Run("stream idle timeout", func(t *testing.T) {
		dir := t.TempDir()
		writeChatAccount(t, dir, "intl-idle", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{
			"workbuddy": map[string]any{"intl": map[string]any{"stream_timeout_seconds": 0.05}},
		})
		release := make(chan struct{})
		// Use a handler that holds the connection open after the first frame;
		// the response body is written directly.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ssePayload(chatChunk("ok", "")))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer server.Close()
		defer close(release)
		provider.chatBaseURL = server.URL

		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model":    "deepseek-v4.1-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		defer stream.Close()
		if _, err := stream.Next(); err != nil {
			t.Fatalf("first Next: %v", err)
		}
		if _, err := stream.Next(); err == nil {
			t.Fatal("idle timeout did not fire")
		}
	})
}

// ---------------------------------------------------------------------------
// Pool note helpers
// ---------------------------------------------------------------------------

func TestChatGPTMaxTokensClamp(t *testing.T) {
	for _, value := range []int{0, -5} {
		t.Run(fmt.Sprintf("%d", value), func(t *testing.T) {
			dir := t.TempDir()
			writeChatAccount(t, dir, "gpt-clamp", realmIntl, map[string]any{"product": productDesktop})
			provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
			fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
			server := newChatTestServer(t, fake)
			provider.chatBaseURL = server.URL
			stream, err := provider.OpenChat(context.Background(), chatRequest(t, "gpt-6-astra", map[string]any{
				"model":      "gpt-6-astra",
				"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
				"max_tokens": value,
			}))
			if err != nil {
				t.Fatalf("OpenChat: %v", err)
			}
			defer stream.Close()
			body := decodeChatBody(t, fake.snapshot()[0])
			if got, _ := body["max_tokens"].(json.Number).Int64(); got != 16 {
				t.Fatalf("max_tokens = %#v, want 16", body["max_tokens"])
			}
		})
	}
}

func TestChatToolChoiceUnknownAndTupleItems(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "tool-low", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "lookup",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"one": map[string]any{"type": "object", "items": []any{
							map[string]any{"type": "str"},
							map[string]any{"type": "int"},
						}},
					},
				},
			},
		}},
		"tool_choice": "future-mode",
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()
	body := decodeChatBody(t, fake.snapshot()[0])
	if body["tool_choice"] != "future-mode" {
		t.Fatalf("unknown tool_choice = %#v", body["tool_choice"])
	}
	tools, _ := body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	function, _ := tool["function"].(map[string]any)
	parameters, _ := function["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	one, _ := properties["one"].(map[string]any)
	items, _ := one["items"].(map[string]any)
	if items["type"] != "string" {
		t.Fatalf("tuple items normalized to %#v", one["items"])
	}
}

// Pin upstream PR #57 at the actual outbound request boundary: disabling
// tool selection must not remove the schemas the agent supplied.
func TestChatToolChoiceNoneKeepsDeclarations(t *testing.T) {
	for _, choice := range []any{"none", map[string]any{"type": "none"}} {
		dir := t.TempDir()
		writeChatAccount(t, dir, "none-contract", realmIntl, map[string]any{"product": productDesktop})
		provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
		fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
		server := newChatTestServer(t, fake)
		provider.chatBaseURL = server.URL
		stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
			"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "user", "content": "do the thing"}},
			"tool_choice": choice, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "run_shell", "parameters": map[string]any{"type": "object"}}}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
		body := decodeChatBody(t, fake.snapshot()[0])
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 || body["tool_choice"] != "none" {
			t.Fatalf("choice=%v outbound tools/choice lost: %v", choice, body)
		}
	}
}

func TestChatResetClockUnescapeAndNestedRestriction(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := `{"msg":"\u5c06\u5728 2099-01-01 08:00:00 UTC+8 \u91cd\u7f6e"}`
	reset := parseChatResetAt(body, now)
	if reset.IsZero() {
		t.Fatalf("escaped CN reset clock was not parsed: %q", body)
	}
	if got := reset.UTC().Format("2006-01-02 15:04:05"); got != "2099-01-01 00:00:00" {
		t.Fatalf("escaped reset = %q", got)
	}
	if !chatRestrictionIsAccountLevel(`{"data":{"code":11140}}`) {
		t.Fatal("nested data restriction code was not detected")
	}
	if !chatRestrictionIsAccountLevel(`{"data":{"error":{"code":11140}}}`) {
		t.Fatal("nested data.error restriction code was not detected")
	}
}

func TestChatUsagePreservesAbsentFields(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "usage-merge", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	frames := []string{
		`{"id":"u1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}}`,
		`{"id":"u1","choices":[],"usage":{"total_tokens":15,"credit":3}}`,
		"[DONE]",
	}
	fake := &chatTestFake{body: ssePayload(frames...)}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()
	_ = drainChatFrames(t, stream)
	usage := chatStreamUsage(t, stream)
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 ||
		usage.CachedTokens != 4 || usage.ReasoningTokens != 2 || usage.Credit != 3 {
		t.Fatalf("usage merge = %+v", usage)
	}
}

func TestChatFinal5xxStatus(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "all-500", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
		return 500, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":{"message":"upstream down"}}`)
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("error = %v, want 502", err)
	}
	if calls := fake.snapshot(); len(calls) != 2 {
		t.Fatalf("single-account 5xx calls = %d, want retry then final (2)", len(calls))
	}
}

func TestChatStaticKey401NoRefresh(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "static-401", realmCN, map[string]any{
		"accessToken":  "ck_static.secret",
		"refreshToken": "",
		"product":      productDesktop,
	})
	provider := setupChatProvider(t, realmCN, dir, map[string]any{})
	fake := &chatTestFake{status: 401, body: `{"error":{"message":"bad key"}}`}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	_, err := provider.OpenChat(context.Background(), chatRequest(t, "hy3", map[string]any{
		"model":    "hy3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("error = %v, want 502", err)
	}
	if calls := fake.snapshot(); len(calls) != 1 {
		t.Fatalf("static key retried: %d calls", len(calls))
	}
	account := accountByUID(t, provider.Pool(), "static-401")
	account.mu.RLock()
	disabled := account.State.Disabled
	account.mu.RUnlock()
	if !disabled {
		t.Fatal("static key 401 did not terminal-disable the account")
	}
}

func TestChatRefreshFailure(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "refresh-fail", realmIntl, map[string]any{
		"product":      productDesktop,
		"refreshToken": "refresh-fail",
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	tokenFake := &tokenTestFake{status: 500, body: `{"error":{"message":"refresh down"}}`}
	tokenServer := httptest.NewServer(tokenFake)
	defer tokenServer.Close()
	provider.tokenBaseURL = tokenServer.URL
	chatFake := &chatTestFake{status: 401, body: `{"error":{"message":"expired"}}`}
	chatServer := newChatTestServer(t, chatFake)
	provider.chatBaseURL = chatServer.URL

	_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("error = %v, want 502", err)
	}
	if calls := chatFake.snapshot(); len(calls) != 1 {
		t.Fatalf("chat calls = %d, want 1", len(calls))
	}
	if calls := tokenFake.snapshot(); len(calls) != 1 {
		t.Fatalf("token calls = %d, want 1", len(calls))
	}
	account := accountByUID(t, provider.Pool(), "refresh-fail")
	account.mu.RLock()
	disabled := account.State.Disabled
	account.mu.RUnlock()
	if disabled {
		t.Fatal("transient refresh failure terminal-disabled the account")
	}
}

func TestChatSecretsRedacted(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token": "device-secret"},
		"proxy_slots": []any{map[string]any{
			"id": "slot-secret", "name": "p", "url": "http://user:proxy-password@proxy.example:8080", "enabled": true,
		}},
	})
	writeChatAccount(t, dir, "secret-1", realmIntl, map[string]any{
		"product":      productDesktop,
		"accessToken":  "access-secret",
		"refreshToken": "refresh-secret",
		"proxySlot":    "slot-secret",
	})
	provider, _ := newPoolProvider(t, realmIntl, dir)
	fake := &chatTestFake{respond: func(*http.Request, []byte) (int, http.Header, []byte) {
		return 500, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":{"message":"access-secret refresh-secret proxy-password device-secret"}}`)
	}}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	account := accountByUID(t, provider.Pool(), "secret-1")
	account.mu.RLock()
	lastError := account.State.LastError
	account.mu.RUnlock()
	for _, secret := range []string{"access-secret", "refresh-secret", "proxy-password", "device-secret"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(apiErr.SafeMessage(), secret) || strings.Contains(lastError, secret) {
			t.Fatalf("secret %q leaked: err=%v message=%q last=%q", secret, err, apiErr.SafeMessage(), lastError)
		}
	}
}

func TestChatSSEToolFinishPostDoneUsage(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "sse-tool", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	frames := []string{
		`{"id":"tool-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"id":"tool-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]",
		`{"id":"tool-1","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"credit":1}}`,
	}
	fake := &chatTestFake{body: ssePayload(frames...)}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL
	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()
	got := drainChatFrames(t, stream)
	if len(got) != 4 {
		t.Fatalf("frames = %d, want tool/finish/DONE/post-DONE: %#v", len(got), got)
	}
	if !strings.Contains(got[0], `"tool_calls"`) || !strings.Contains(got[1], `"tool_calls"`) {
		t.Fatalf("tool frames were not preserved: %#v", got)
	}
	if strings.TrimSpace(got[2]) != "data: [DONE]" {
		t.Fatalf("DONE frame = %q", got[2])
	}
	if !strings.Contains(got[3], `"usage"`) {
		t.Fatalf("post-DONE usage frame = %q", got[3])
	}
	usage := chatStreamUsage(t, stream)
	if usage.FinishReason != "tool_calls" || usage.TotalTokens != 10 || usage.Credit != 1 {
		t.Fatalf("usage/finish = %+v", usage)
	}
}

func TestChatStalledErrorBodyTimesOut(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-stall", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{
		"workbuddy": map[string]any{"intl": map[string]any{
			"timeout_seconds":        1,
			"stream_timeout_seconds": 1,
		}},
	})
	body := &stalledErrorBody{closed: make(chan struct{})}
	provider.chatDoer = chatDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}, nil
	})

	start := time.Now()
	_, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err == nil {
		t.Fatal("OpenChat unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("stalled error body took %v", elapsed)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("error = %v, want 502", err)
	}
	body.mu.Lock()
	closed := body.wasClosed
	body.mu.Unlock()
	if !closed {
		t.Fatal("stalled error body was not closed on timeout")
	}
	account := accountByUID(t, provider.Pool(), "intl-stall")
	account.mu.RLock()
	active := account.State.ActiveConcurrent
	account.mu.RUnlock()
	if active != 0 {
		t.Fatalf("active after stalled error = %d", active)
	}
}

type stalledErrorBody struct {
	closed    chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	wasClosed bool
}

func (b *stalledErrorBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *stalledErrorBody) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.wasClosed = true
		b.mu.Unlock()
		close(b.closed)
	})
	return nil
}

func TestPoolNoteErrorAndSuccess(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "note-1", realmIntl, map[string]any{"product": productDesktop})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	pool := provider.Pool()
	account := accountByUID(t, pool, "note-1")

	if err := pool.NoteError(account, "boom", false, false); err != nil {
		t.Fatalf("NoteError: %v", err)
	}
	account.mu.RLock()
	if account.State.ErrCount != 1 || account.State.LastError != "boom" || !account.State.CoolUntil.IsZero() {
		t.Fatalf("ordinary error state = %+v", account.State)
	}
	account.mu.RUnlock()

	if err := pool.NoteModelError(account, "deepseek-v4.1-flash", "throttled", 30*time.Second); err != nil {
		t.Fatalf("NoteModelError: %v", err)
	}
	if !account.modelCooling("deepseek-v4.1-flash", time.Now()) {
		t.Fatal("model cooldown not recorded")
	}
	if view := account.View(); len(view.ModelCooldowns) != 1 ||
		view.ModelCooldowns[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("model cooldown view = %#v", view.ModelCooldowns)
	}
	firstStarted := account.View().ModelCooldowns[0].StartedAt
	later := time.Now().Add(5 * time.Second)
	pool.now = func() time.Time { return later }
	if err := pool.NoteModelError(account, "deepseek-v4.1-flash", "throttled again", 45*time.Second); err != nil {
		t.Fatalf("second NoteModelError: %v", err)
	}
	if refreshed := account.View().ModelCooldowns[0].StartedAt; refreshed != later.Unix() || refreshed <= firstStarted {
		t.Fatalf("model cooldown StartedAt did not refresh: first=%d second=%d", firstStarted, refreshed)
	}
	pool.now = time.Now
	if got := pool.CountReady("deepseek-v4.1-flash"); got != 0 {
		t.Fatalf("CountReady with model cooldown = %d", got)
	}
	if got := pool.CountReady("hy3"); got != 1 {
		t.Fatalf("CountReady for sibling model = %d", got)
	}

	if err := pool.NoteError(account, "soft", true, false); err != nil {
		t.Fatalf("NoteError soft: %v", err)
	}
	account.mu.RLock()
	if !account.State.CoolUntil.After(time.Now()) {
		t.Fatalf("soft cooldown = %v", account.State.CoolUntil)
	}
	account.mu.RUnlock()

	if err := pool.NoteSuccess(account); err != nil {
		t.Fatalf("NoteSuccess: %v", err)
	}
	account.mu.RLock()
	cooling := account.State.CoolUntil
	lastError := account.State.LastError
	errCount := account.State.ErrCount
	account.mu.RUnlock()
	if !cooling.IsZero() || lastError != "" || errCount != 0 {
		t.Fatalf("NoteSuccess did not clear state: cool=%v err=%q count=%d", cooling, lastError, errCount)
	}
	if account.modelCooling("deepseek-v4.1-flash", time.Now()) {
		t.Fatal("NoteSuccess did not clear model cooldowns")
	}
}

// ---------------------------------------------------------------------------
// Fakes / utility
// ---------------------------------------------------------------------------

type chatDoerFunc func(ctx context.Context, req *http.Request, proxy string) (*http.Response, error)

func (f chatDoerFunc) Do(ctx context.Context, req *http.Request, proxy string) (*http.Response, error) {
	return f(ctx, req, proxy)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func TestFillEmptyChatTextParts(t *testing.T) {
	messages := fillEmptyChatTextParts([]any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": ""},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:x"}},
			map[string]any{"type": "text", "text": "hi"},
		}},
		map[string]any{"role": "user", "content": "plain"},
	})
	parts := messages[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != " " {
		t.Fatalf("empty text part = %#v", parts[0])
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image part changed: %#v", parts[1])
	}
	if parts[2].(map[string]any)["text"] != "hi" {
		t.Fatalf("non-empty text changed: %#v", parts[2])
	}
	if messages[1].(map[string]any)["content"] != "plain" {
		t.Fatalf("string content changed: %#v", messages[1])
	}
}

func TestFillEmptyChatTextPartsSniffsStringArrays(t *testing.T) {
	messages := fillEmptyChatTextParts([]any{
		map[string]any{"role": "user", "content": `[{"type":"text","text":null},{"type":"text"},{"type":"text","text":"hi"}]`},
	})
	var parts []any
	if err := json.Unmarshal([]byte(messages[0].(map[string]any)["content"].(string)), &parts); err != nil {
		t.Fatalf("sniffed content is not JSON: %v", err)
	}
	if parts[0].(map[string]any)["text"] != " " || parts[1].(map[string]any)["text"] != " " {
		t.Fatalf("empty/null parts = %#v", parts)
	}
	if parts[2].(map[string]any)["text"] != "hi" {
		t.Fatalf("non-empty part = %#v", parts[2])
	}
	again := fillEmptyChatTextParts(messages)
	if again[0].(map[string]any)["content"] != messages[0].(map[string]any)["content"] {
		t.Fatal("fill is not idempotent")
	}
}

func TestMirrorUpstreamReasoning(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": "a", "reasoning_content": "think"},
		map[string]any{"role": "assistant", "content": "b", "reasoning_content": "new", "reasoning": "keep"},
		map[string]any{"role": "user", "content": "hi"},
	}
	out := mirrorUpstreamReasoning(messages, "kimi-k3")
	if out[0].(map[string]any)["reasoning"] != "think" {
		t.Fatalf("mirrored reasoning = %#v", out[0])
	}
	if out[1].(map[string]any)["reasoning"] != "keep" {
		t.Fatalf("existing reasoning overwritten: %#v", out[1])
	}
	if _, exists := out[2].(map[string]any)["reasoning"]; exists {
		t.Fatalf("user message gained reasoning: %#v", out[2])
	}
	if got := mirrorUpstreamReasoning(messages, "deepseek-v4.1-flash")[0].(map[string]any)["reasoning"]; got != nil {
		t.Fatalf("deepseek message was mirrored: %#v", got)
	}
}

func TestWorkBuddyToolSchemaHardensKeywordTypes(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"n": map[string]any{"type": "integer", "maximum": "10"},
		},
	}
	normalizeChatSchema(schema)
	props := schema["properties"].(map[string]any)
	if props["n"].(map[string]any)["maximum"] != int64(10) {
		t.Fatalf("maximum = %#v", props["n"])
	}
}
