package codearts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

var _ providers.HTTPError = (*APIError)(nil)

type t5Capture struct {
	Path   string
	Host   string
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

type t5Fake struct {
	mu       sync.Mutex
	calls    int
	captures []t5Capture
	respond  func(w http.ResponseWriter, r *http.Request, call int)
}

func (f *t5Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&body)

	f.mu.Lock()
	f.calls++
	call := f.calls
	f.captures = append(f.captures, t5Capture{
		Path:   r.URL.Path,
		Host:   r.Host,
		Header: r.Header.Clone(),
		Body:   body,
		Raw:    append([]byte(nil), raw...),
	})
	respond := f.respond
	f.mu.Unlock()

	if respond != nil {
		respond(w, r, call)
		return
	}
	t5WriteSuccess(w)
}

func (f *t5Fake) snapshot() []t5Capture {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]t5Capture, len(f.captures))
	copy(out, f.captures)
	return out
}

func (f *t5Fake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newT5Fake(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, call int)) (*t5Fake, *httptest.Server) {
	t.Helper()
	fake := &t5Fake{respond: respond}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("WB_CODEARTS_SNAP_HOST", server.URL)
	t.Setenv("WB_CODEARTS_BENEFIT_HOST", server.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", server.URL)
	return fake, server
}

func t5WriteSSEFrames(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		_, _ = io.WriteString(w, frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func t5DataFrame(payload string) string {
	return "data: " + payload + "\n\n"
}

func t5WriteSuccess(w http.ResponseWriter) {
	t5WriteSSEFrames(w,
		t5DataFrame(`{"id":"upstream","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
		t5DataFrame(`{"id":"upstream","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`),
		t5DataFrame(`{"id":"upstream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		t5DataFrame(`{"id":"upstream","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
		"data: [DONE]\n\n",
	)
}

func newT5Provider(t *testing.T, uids ...string) (*Provider, map[string]*Account) {
	t.Helper()
	dir := t.TempDir()
	provider, err := New(config.New(filepath.Join(dir, "settings.json")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := LoadConfig(provider.store)
	cfg.Enabled = true
	cfg.StreamTimeoutSeconds = 600
	cfg.ModelsTTLSeconds = 3600
	cfg.MaxConcurrent = 1
	provider.pool.SetConfig(cfg)
	provider.chatRetryDelay = time.Millisecond

	accounts := make(map[string]*Account, len(uids))
	for _, uid := range uids {
		auth := testAuth(uid)
		auth.Expiration = time.Now().Add(4 * time.Hour)
		account, err := provider.Pool().Add(auth)
		if err != nil {
			t.Fatalf("Pool.Add(%s): %v", uid, err)
		}
		setAccountCatalog(t, account, []Model{
			{"id": "GLM-5.2", "max_tokens": int64(8192)},
			{"id": "GLM-5.3-Flash", "max_tokens": int64(32000)},
			{"id": "Qwen3-VL-235B", "max_tokens": int64(8192)},
		}, time.Now())
		accounts[uid] = account
	}
	return provider, accounts
}

func t5Request(t *testing.T, values map[string]any, sessionKey string) providers.ChatRequest {
	t.Helper()
	raw := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}
		raw[key] = encoded
	}
	return providers.ChatRequest{Raw: raw, SessionKey: sessionKey}
}

func t5Messages(values ...any) []any {
	return values
}

func t5UserMessage(text string) map[string]any {
	return map[string]any{"role": "user", "content": text}
}

func t5DecodeFrame(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	text := string(frame)
	if !strings.HasPrefix(text, "data: ") || !strings.HasSuffix(text, "\n\n") {
		t.Fatalf("not a complete SSE data frame: %q", text)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(text, "data: "), "\n\n")
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode frame %q: %v", text, err)
	}
	return decoded
}

func t5CollectFrames(t *testing.T, stream providers.ChatStream) [][]byte {
	t.Helper()
	frames := make([][]byte, 0, 8)
	for index := 0; index < 200; index++ {
		frame, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !bytes.HasPrefix(frame, []byte("data: ")) || !bytes.HasSuffix(frame, []byte("\n\n")) {
			t.Fatalf("Next returned an incomplete frame: %q", frame)
		}
		frames = append(frames, frame)
	}
	t.Fatal("stream did not terminate")
	return nil
}

func t5Delta(frame []byte) map[string]any {
	var decoded map[string]any
	text := strings.TrimSuffix(strings.TrimPrefix(string(frame), "data: "), "\n\n")
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil
	}
	choices, _ := decoded["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	return delta
}

func t5Finish(frame []byte) any {
	var decoded map[string]any
	text := strings.TrimSuffix(strings.TrimPrefix(string(frame), "data: "), "\n\n")
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil
	}
	choices, _ := decoded["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	return choice["finish_reason"]
}

func t5Text(value any) string {
	return modelText(value)
}

func t5FrameStrings(frames [][]byte) string {
	var builder strings.Builder
	for _, frame := range frames {
		builder.Write(frame)
	}
	return builder.String()
}

func t5HeaderMap(header http.Header) map[string][]string {
	return map[string][]string(header)
}

func TestChatDisabledProviderReturnsV1BadRequest(t *testing.T) {
	dir := t.TempDir()
	provider, err := New(config.New(filepath.Join(dir, "settings.json")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("OpenChat error = %T %v, want *APIError", err, err)
	}
	if apiErr.HTTPStatus() != http.StatusBadRequest || apiErr.ErrorType() != "invalid_request_error" {
		t.Fatalf("disabled provider mapping = %d/%s", apiErr.HTTPStatus(), apiErr.ErrorType())
	}
	const want = "CodeArts 出口未启用：请到设置页打开「启用 CodeArts 出口」。"
	if apiErr.SafeMessage() != want {
		t.Fatalf("disabled message = %q, want %q", apiErr.SafeMessage(), want)
	}
}

func TestChatBodyAllowlistCanonicalAndClamp(t *testing.T) {
	fake, _ := newT5Fake(t, nil)
	provider, _ := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":                 "glm-5.2",
		"messages":              t5Messages(t5UserMessage("hi")),
		"temperature":           0.2,
		"top_p":                 0.9,
		"top_k":                 5,
		"seed":                  7,
		"frequency_penalty":     0.1,
		"presence_penalty":      0.2,
		"stop":                  "END",
		"response_format":       map[string]any{"type": "json_object"},
		"parallel_tool_calls":   false,
		"max_completion_tokens": 999999,
		"n":                     2,
		"stream_options":        map[string]any{"include_usage": true},
		"user":                  "cli",
		"conversation_id":       "not-a-chat-id",
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = stream.Close()

	captures := fake.snapshot()
	if len(captures) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(captures))
	}
	body := captures[0].Body
	if got := t5Text(body["model"]); got != "GLM-5.2" {
		t.Fatalf("model = %q, want GLM-5.2", got)
	}
	if got := t5Text(body["max_tokens"]); got != "8192" {
		t.Fatalf("max_tokens = %q, want clamped 8192", got)
	}
	if _, exists := body["max_completion_tokens"]; exists {
		t.Fatalf("max_completion_tokens was not folded into max_tokens: %#v", body)
	}
	for _, key := range []string{"n", "stream_options", "user", "conversation_id", "reasoningEffort", "thinking", "enable_thinking"} {
		if _, exists := body[key]; exists {
			t.Fatalf("disallowed key %q reached upstream: %#v", key, body)
		}
	}
	if body["stream"] != true || body["tool_stream"] != true {
		t.Fatalf("stream flags = %#v/%#v", body["stream"], body["tool_stream"])
	}
	stop, ok := body["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop = %#v, want [END]", body["stop"])
	}
	for _, key := range []string{"temperature", "top_p", "top_k", "seed", "frequency_penalty", "presence_penalty", "response_format", "parallel_tool_calls"} {
		if _, exists := body[key]; !exists {
			t.Fatalf("allowed key %q was dropped: %#v", key, body)
		}
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", body["parallel_tool_calls"])
	}
}

func TestChatClampsBothTokenSpellings(t *testing.T) {
	fake, _ := newT5Fake(t, nil)
	provider, _ := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":                 "GLM-5.2",
		"messages":              t5Messages(t5UserMessage("hi")),
		"max_tokens":            200000,
		"max_completion_tokens": 190000,
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = stream.Close()
	body := fake.snapshot()[0].Body
	if t5Text(body["max_tokens"]) != "8192" || t5Text(body["max_completion_tokens"]) != "8192" {
		t.Fatalf("both token spellings not clamped: %#v", body)
	}
}

func TestChatInvalidOutputTokensRejectedWithoutFailover(t *testing.T) {
	fake, _ := newT5Fake(t, nil)
	provider, accounts := newT5Provider(t, "u1")
	_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":      "GLM-5.2",
		"messages":   t5Messages(t5UserMessage("hi")),
		"max_tokens": "abc",
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400", err)
	}
	if !strings.Contains(apiErr.SafeMessage(), "max_tokens must be a positive integer") {
		t.Fatalf("message = %q", apiErr.SafeMessage())
	}
	if fake.callCount() != 0 {
		t.Fatalf("upstream calls = %d, want 0", fake.callCount())
	}
	account := accounts["u1"]
	if account.State.CoolUntil != 0 {
		t.Fatalf("validation error cooled the account: %+v", account.State)
	}
	if account.activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d, want 0", account.activeConcurrent)
	}
}

func t5OpenBody(t *testing.T, values map[string]any) (*t5Fake, map[string]any, error) {
	t.Helper()
	fake, _ := newT5Fake(t, nil)
	provider, _ := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, values, ""))
	if err == nil {
		_ = stream.Close()
	}
	captures := fake.snapshot()
	if len(captures) == 0 {
		return fake, nil, err
	}
	return fake, captures[len(captures)-1].Body, err
}

func TestChatReasoningEffortMapping(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		values := map[string]any{
			"model":    "GLM-5.2",
			"messages": t5Messages(t5UserMessage("hi")),
		}
		for key, value := range extra {
			values[key] = value
		}
		return values
	}

	t.Run("direct", func(t *testing.T) {
		_, body, err := t5OpenBody(t, base(map[string]any{"reasoning_effort": "high"}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if body["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
		}
	})

	t.Run("camel", func(t *testing.T) {
		_, body, err := t5OpenBody(t, base(map[string]any{"reasoningEffort": "medium"}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if body["reasoning_effort"] != "medium" {
			t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
		}
	})

	t.Run("thinking enabled", func(t *testing.T) {
		_, body, err := t5OpenBody(t, base(map[string]any{"thinking": map[string]any{"type": "enabled"}}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if body["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
		}
	})

	t.Run("thinking disabled", func(t *testing.T) {
		_, body, err := t5OpenBody(t, base(map[string]any{"thinking": map[string]any{"type": "disabled"}}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if body["reasoning_effort"] != "none" {
			t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
		}
	})

	t.Run("enable thinking false", func(t *testing.T) {
		_, body, err := t5OpenBody(t, base(map[string]any{"enable_thinking": false}))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if body["reasoning_effort"] != "none" {
			t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
		}
	})

	t.Run("invalid", func(t *testing.T) {
		fake, body, err := t5OpenBody(t, base(map[string]any{"reasoning_effort": "auto"}))
		if err == nil {
			t.Fatalf("OpenChat succeeded with invalid effort: %#v", body)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadRequest {
			t.Fatalf("error = %v, want 400", err)
		}
		if !strings.Contains(apiErr.SafeMessage(), "reasoning_effort must be one of") {
			t.Fatalf("message = %q", apiErr.SafeMessage())
		}
		if fake.callCount() != 0 {
			t.Fatalf("invalid effort reached upstream %d times", fake.callCount())
		}
	})

	t.Run("always thinking none becomes low", func(t *testing.T) {
		_, body, err := t5OpenBody(t, map[string]any{
			"model":            "glm-5.3-flash",
			"messages":         t5Messages(t5UserMessage("hi")),
			"reasoning_effort": "none",
		})
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if body["reasoning_effort"] != "low" {
			t.Fatalf("reasoning_effort = %#v, want low", body["reasoning_effort"])
		}
	})

	t.Run("always thinking rejects medium", func(t *testing.T) {
		fake, _, err := t5OpenBody(t, map[string]any{
			"model":            "glm-5.3-flash",
			"messages":         t5Messages(t5UserMessage("hi")),
			"reasoning_effort": "medium",
		})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadRequest {
			t.Fatalf("error = %v, want 400", err)
		}
		if fake.callCount() != 0 {
			t.Fatalf("invalid effort reached upstream %d times", fake.callCount())
		}
	})
}

func TestChatContentImagesAndTools(t *testing.T) {
	t.Run("content parts", func(t *testing.T) {
		_, body, err := t5OpenBody(t, map[string]any{
			"model": "GLM-5.2",
			"messages": t5Messages(
				map[string]any{"role": "developer", "content": "rules"},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "hello "},
					map[string]any{"type": "text", "text": "world"},
				}},
			),
		})
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 {
			t.Fatalf("messages = %#v", messages)
		}
		first, _ := messages[0].(map[string]any)
		if first["role"] != "system" {
			t.Fatalf("developer role = %#v, want system", first["role"])
		}
		second, _ := messages[1].(map[string]any)
		if second["content"] != "hello world" {
			t.Fatalf("flattened content = %#v, want hello world", second["content"])
		}
	})

	t.Run("images", func(t *testing.T) {
		_, body, err := t5OpenBody(t, map[string]any{
			"model": "Qwen3-VL-235B",
			"messages": t5Messages(map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "look"},
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64,AAA", "detail": "high",
					}},
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/jpeg", "data": "BBB",
					}},
					map[string]any{"type": "image", "source": map[string]any{
						"type": "url", "url": "https://example.test/img.png",
					}},
					map[string]any{"type": "input_image", "image_url": "https://example.test/resp.png"},
				},
			}),
		})
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		messages, _ := body["messages"].([]any)
		message, _ := messages[0].(map[string]any)
		parts, ok := message["content"].([]any)
		if !ok || len(parts) != 5 {
			t.Fatalf("image content = %#v", message["content"])
		}
		want := []map[string]any{
			{"type": "text", "text": "look"},
			{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA", "detail": "high"}},
			{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,BBB"}},
			{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/img.png"}},
			{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/resp.png"}},
		}
		for index, expected := range want {
			part, _ := parts[index].(map[string]any)
			left, _ := json.Marshal(part)
			right, _ := json.Marshal(expected)
			if string(left) != string(right) {
				t.Fatalf("part %d = %s, want %s", index, left, right)
			}
		}
	})

	t.Run("tools and tool choice", func(t *testing.T) {
		_, body, err := t5OpenBody(t, map[string]any{
			"model":    "GLM-5.2",
			"messages": t5Messages(t5UserMessage("hi")),
			"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "ok", "parameters": map[string]any{}}},
				map[string]any{"type": "function"},
				map[string]any{"type": "web_search"},
				map[string]any{"type": "function", "function": map[string]any{"name": "no_params"}},
				map[string]any{"type": "function", "function": map[string]any{"name": "ok"}},
				map[string]any{"name": "flat", "description": "d"},
			},
			"tool_choice": map[string]any{
				"type": "function", "function": map[string]any{"name": "flat"},
			},
		})
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools = %#v, want only flat", tools)
		}
		tool, _ := tools[0].(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if fn["name"] != "flat" {
			t.Fatalf("function = %#v", fn)
		}
		params, _ := fn["parameters"].(map[string]any)
		if params["type"] != "object" || params["properties"] == nil {
			t.Fatalf("flat tool parameters not normalized: %#v", params)
		}
		if body["tool_choice"] != "required" {
			t.Fatalf("tool_choice = %#v, want required", body["tool_choice"])
		}
	})

	t.Run("tool pairing repair", func(t *testing.T) {
		_, body, err := t5OpenBody(t, map[string]any{
			"model": "GLM-5.2",
			"messages": t5Messages(
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "a", "arguments": "{}"}},
					map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "b", "arguments": "{}"}},
				}},
				map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
					map[string]any{"id": "call_3", "type": "function", "function": map[string]any{"name": "c", "arguments": "{}"}},
				}, "reasoning_content": "more"},
				map[string]any{"role": "user", "content": "wait"},
				map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "one"},
				map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "two"},
				map[string]any{"role": "tool", "tool_call_id": "orphan", "content": "drop"},
			),
		})
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 4 {
			t.Fatalf("repaired messages = %#v", messages)
		}
		assistant, _ := messages[0].(map[string]any)
		calls, _ := assistant["tool_calls"].([]any)
		if len(calls) != 2 {
			t.Fatalf("merged calls = %#v, want call_1/call_2 only", calls)
		}
		if assistant["reasoning_content"] != "more" {
			t.Fatalf("reasoning_content = %#v", assistant["reasoning_content"])
		}
		result1, _ := messages[1].(map[string]any)
		result2, _ := messages[2].(map[string]any)
		intruder, _ := messages[3].(map[string]any)
		if result1["tool_call_id"] != "call_1" || result2["tool_call_id"] != "call_2" || intruder["content"] != "wait" {
			t.Fatalf("tool result repack order = %#v", messages)
		}
	})
}

func TestChatIdentityHeadersChatIDAndPromptCache(t *testing.T) {
	fake, _ := newT5Fake(t, nil)
	provider, accounts := newT5Provider(t, "u1")
	accounts["u1"].SetModelBenefit(map[string]bool{"glm-5.2": true})

	conversationID := "0123456789abcdef0123456789abcdef"
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":           "GLM-5.2",
		"messages":        t5Messages(t5UserMessage("hi")),
		"conversation_id": conversationID,
	}, "ignored-session"))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = stream.Close()

	captures := fake.snapshot()
	if len(captures) != 1 {
		t.Fatalf("calls = %d", len(captures))
	}
	capture := captures[0]
	body := capture.Body
	if body["chat_id"] != conversationID || body["prompt_cache_key"] != conversationID {
		t.Fatalf("chat_id/prompt_cache_key = %#v/%#v", body["chat_id"], body["prompt_cache_key"])
	}
	wantSession := t5SHA("codearts-session:" + conversationID)
	headers := capture.Header
	if got := headers.Get("Chat-Id"); got != conversationID {
		t.Fatalf("Chat-Id = %q, want %q", got, conversationID)
	}
	if got := headers.Get("Session-Id"); got != wantSession {
		t.Fatalf("Session-Id = %q, want %q", got, wantSession)
	}
	if got := headers.Get("x-auth-token"); got != "cloud-token-u1" {
		t.Fatalf("x-auth-token = %q", got)
	}
	if headers.Get("X-Language") != "zh-cn" || headers.Get("app-id") != "CodeAgent3.0" || headers.Get("is_confidential") != "false" {
		t.Fatalf("identity headers = %#v", t5HeaderMap(headers))
	}
	if headers.Get("maas_type") != "benefit" {
		t.Fatalf("maas_type = %q, want benefit", headers.Get("maas_type"))
	}
	if len(headers.Get("x-snap-traceid")) != 32 || len(headers.Get("x-ot-trace-id")) != 32 {
		t.Fatalf("trace headers = %q/%q", headers.Get("x-snap-traceid"), headers.Get("x-ot-trace-id"))
	}
	wantOT := t5SHA("codearts-ot-session:u1")
	if headers.Get("x-ot-session-id") != wantOT || headers.Get("user-session-id") != wantOT {
		t.Fatalf("ot session headers = %q/%q, want %q", headers.Get("x-ot-session-id"), headers.Get("user-session-id"), wantOT)
	}
	sum := sha256.Sum256(capture.Raw)
	if got := headers.Get("X-Sdk-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("X-Sdk-Content-Sha256 = %q", got)
	}
	authorization := headers.Get("Authorization")
	lowerAuth := strings.ToLower(authorization)
	if !strings.HasPrefix(authorization, "SDK-HMAC-SHA256 ") || !strings.Contains(lowerAuth, "signedheaders=") {
		t.Fatalf("Authorization is not a Huawei signature: %q", authorization)
	}
	if !strings.Contains(lowerAuth, "x-auth-token") {
		t.Fatalf("identity headers were not signed: %q", authorization)
	}
	if strings.Contains(lowerAuth, "chat-id") || strings.Contains(lowerAuth, "session-id") {
		t.Fatalf("post-sign identity headers leaked into SignedHeaders: %q", authorization)
	}

	sessionKey := "sess-abc"
	stream, err = provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, sessionKey))
	if err != nil {
		t.Fatalf("OpenChat session: %v", err)
	}
	_ = stream.Close()
	captures = fake.snapshot()
	second := captures[len(captures)-1]
	wantChatID := t5SHA(sessionKey)
	if second.Body["chat_id"] != wantChatID || second.Body["prompt_cache_key"] != wantChatID {
		t.Fatalf("session chat id = %#v, want %q", second.Body["chat_id"], wantChatID)
	}
	if second.Header.Get("x-ot-session-id") != wantOT {
		t.Fatalf("ot session is not stable per account: %q vs %q", second.Header.Get("x-ot-session-id"), wantOT)
	}
	if second.Header.Get("x-ot-trace-id") == capture.Header.Get("x-ot-trace-id") {
		t.Fatal("x-ot-trace-id did not change between requests")
	}
	if second.Header.Get("x-snap-traceid") == capture.Header.Get("x-snap-traceid") {
		t.Fatal("x-snap-traceid did not change between requests")
	}
}

func TestChatDerivesConversationIDs(t *testing.T) {
	fake, _ := newT5Fake(t, nil)
	provider, _ := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model": "GLM-5.2",
		"messages": t5Messages(
			map[string]any{"role": "system", "content": "rules"},
			t5UserMessage("hello"),
		),
		"conversation_id": "not-valid",
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = stream.Close()
	body := fake.snapshot()[0].Body
	want := t5SHA("codearts:hello")
	if body["chat_id"] != want {
		t.Fatalf("first-user chat id = %#v, want %q", body["chat_id"], want)
	}

	stream, err = provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(map[string]any{"role": "system", "content": "rules"}),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat random: %v", err)
	}
	_ = stream.Close()
	randomID := fake.snapshot()[1].Body["chat_id"]
	text, _ := randomID.(string)
	if !validChatID(text) {
		t.Fatalf("random chat id = %#v, want 32 hex", randomID)
	}
}

func TestChatUnknownModelUsesDefaultOutputClamp(t *testing.T) {
	fake, _ := newT5Fake(t, nil)
	provider, _ := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":      "unknown-model",
		"messages":   t5Messages(t5UserMessage("hi")),
		"max_tokens": 999999,
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = stream.Close()
	if got := t5Text(fake.snapshot()[0].Body["max_tokens"]); got != "65536" {
		t.Fatalf("default clamp = %q, want 65536", got)
	}
}

func TestChatEmptyEventStreamIsUpstreamError(t *testing.T) {
	newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	provider, accounts := newT5Provider(t, "u1")
	_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindUpstream || apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("empty stream error = %v, want upstream 502", err)
	}
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d, want 0", accounts["u1"].activeConcurrent)
	}
}

func t5SHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:32]
}

func TestChatRequestFromModelAndMessagesFallback(t *testing.T) {
	fake, _ := newT5Fake(t, nil)
	provider, _ := newT5Provider(t, "u1")
	message, _ := json.Marshal(t5UserMessage("hi"))
	stream, err := provider.OpenChat(context.Background(), providers.ChatRequest{
		Model:    "glm-5.2",
		Messages: []json.RawMessage{message},
	})
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = stream.Close()
	body := fake.snapshot()[0].Body
	if body["model"] != "GLM-5.2" {
		t.Fatalf("model = %#v", body["model"])
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestChatStreamStandardEvents(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		t5WriteSSEFrames(w,
			t5DataFrame(`{"id":"other","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
			t5DataFrame(`{"id":"other","choices":[{"index":0,"delta":{"content":"hi","reasoning_content":"think"},"finish_reason":null}]}`),
			t5DataFrame(`{"id":"other","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`),
			t5DataFrame(`{"id":"other","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
			t5DataFrame(`{"id":"other","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`),
			"data: [DONE]\n\n",
		)
	})
	provider, accounts := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := t5CollectFrames(t, stream)
	if len(frames) < 5 {
		t.Fatalf("frames = %d, want at least 5", len(frames))
	}

	finishes := 0
	done := 0
	usageIndex := -1
	doneIndex := -1
	for index, frame := range frames {
		if strings.Contains(string(frame), "[DONE]") {
			done++
			doneIndex = index
			continue
		}
		decoded := t5DecodeFrame(t, frame)
		if decoded["id"] != "chatcmpl-codearts" {
			t.Fatalf("frame %d id = %#v", index, decoded["id"])
		}
		if finish := t5Finish(frame); finish != nil {
			if finish != "tool_calls" {
				t.Fatalf("finish = %#v, want tool_calls", finish)
			}
			finishes++
		}
		choices, _ := decoded["choices"].([]any)
		if len(choices) == 0 {
			if decoded["usage"] != nil && usageIndex < 0 {
				usageIndex = index
			}
		}
	}
	if finishes != 1 {
		t.Fatalf("finish chunks = %d, want exactly 1", finishes)
	}
	if done != 1 || doneIndex != len(frames)-1 {
		t.Fatalf("DONE frames = %d at %d, last index %d", done, doneIndex, len(frames)-1)
	}
	if usageIndex < 0 || usageIndex > doneIndex {
		t.Fatalf("usage frame index = %d, done = %d", usageIndex, doneIndex)
	}

	firstDelta := t5Delta(frames[0])
	if firstDelta["role"] != "assistant" {
		t.Fatalf("first delta = %#v", firstDelta)
	}
	secondDelta := t5Delta(frames[1])
	if secondDelta["content"] != "hi" || secondDelta["reasoning_content"] != "think" {
		t.Fatalf("second delta = %#v", secondDelta)
	}
	thirdDelta := t5Delta(frames[2])
	calls, _ := thirdDelta["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls delta = %#v", thirdDelta["tool_calls"])
	}
	account := accounts["u1"]
	if account.activeConcurrent != 0 {
		t.Fatalf("activeConcurrent after DONE = %d, want 0", account.activeConcurrent)
	}
	if fake.callCount() != 1 {
		t.Fatalf("calls = %d", fake.callCount())
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after DONE = %v, want io.EOF", err)
	}
}

func TestChatStreamSnapshotShapes(t *testing.T) {
	t.Run("text snapshots", func(t *testing.T) {
		newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
			t5WriteSSEFrames(w,
				t5DataFrame(`{"text":"你","prompt_tokens":3,"completion_tokens":1}`),
				t5DataFrame(`{"text":"你好","prompt_tokens":3,"completion_tokens":2}`),
				t5DataFrame(`{"text":"[DONE]","error_code":"0"}`),
			)
		})
		provider, accounts := newT5Provider(t, "u1")
		stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
			"model":    "GLM-5.2",
			"messages": t5Messages(t5UserMessage("hi")),
		}, ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		frames := t5CollectFrames(t, stream)
		if accounts["u1"].activeConcurrent != 0 {
			t.Fatalf("text-snapshot DONE did not release the slot: %d", accounts["u1"].activeConcurrent)
		}
		var content strings.Builder
		finishes := 0
		for _, frame := range frames {
			if delta := t5Delta(frame); delta != nil {
				if text, ok := delta["content"].(string); ok {
					content.WriteString(text)
					if strings.Contains(text, "[DONE]") {
						t.Fatalf("DONE leaked as content: %q", text)
					}
				}
			}
			if t5Finish(frame) != nil {
				finishes++
			}
		}
		if content.String() != "你好" {
			t.Fatalf("content = %q, want 你好", content.String())
		}
		if finishes != 1 {
			t.Fatalf("finish chunks = %d, want 1", finishes)
		}
		if strings.Count(t5FrameStrings(frames), "[DONE]") != 1 {
			t.Fatalf("DONE count mismatch: %q", frames)
		}
	})

	t.Run("output snapshots", func(t *testing.T) {
		newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
			t5WriteSSEFrames(w,
				t5DataFrame(`{"output":[{"type":"output_text","text":"he"}]}`),
				t5DataFrame(`{"output":[{"type":"output_text","text":"hello"}]}`),
				t5DataFrame(`{"output":[{"type":"output_text","text":"hello"}]}`),
				"data: [DONE]\n\n",
			)
		})
		provider, _ := newT5Provider(t, "u1")
		stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
			"model":    "GLM-5.2",
			"messages": t5Messages(t5UserMessage("hi")),
		}, ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		frames := t5CollectFrames(t, stream)
		var content strings.Builder
		for _, frame := range frames {
			if delta := t5Delta(frame); delta != nil {
				if text, ok := delta["content"].(string); ok {
					content.WriteString(text)
				}
			}
		}
		if content.String() != "hello" {
			t.Fatalf("content = %q, want hello", content.String())
		}
	})

	t.Run("top level delta", func(t *testing.T) {
		newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
			t5WriteSSEFrames(w,
				t5DataFrame(`{"delta":{"reasoning_content":"r"}}`),
				t5DataFrame(`{"delta":{"content":"ok"}}`),
				"data: [DONE]\n\n",
			)
		})
		provider, _ := newT5Provider(t, "u1")
		stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
			"model":    "GLM-5.2",
			"messages": t5Messages(t5UserMessage("hi")),
		}, ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		frames := t5CollectFrames(t, stream)
		if len(frames) < 3 {
			t.Fatalf("frames = %d", len(frames))
		}
		if t5Delta(frames[0])["reasoning_content"] != "r" {
			t.Fatalf("reasoning delta = %#v", t5Delta(frames[0]))
		}
		if t5Delta(frames[1])["content"] != "ok" {
			t.Fatalf("content delta = %#v", t5Delta(frames[1]))
		}
		if t5Finish(frames[2]) != "stop" {
			t.Fatalf("finish = %#v, want stop", t5Finish(frames[2]))
		}
	})
}

func TestChatStreamEOFBeforeDoneIsUpstreamError(t *testing.T) {
	newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		t5WriteSSEFrames(w, t5DataFrame(`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`))
	})
	provider, accounts := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	first, err := stream.Next()
	if err != nil || t5Delta(first)["content"] != "partial" {
		t.Fatalf("first frame = %q, err %v", first, err)
	}
	_, err = stream.Next()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindUpstream || apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("EOF error = %v, want upstream 502", err)
	}
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d, want 0", accounts["u1"].activeConcurrent)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after error = %v", err)
	}
}

func TestChatStreamEmbeddedParameterErrorIsRedactedBadRequest(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		t5WriteSSEFrames(w, t5DataFrame(`{"error_code":"invalid_param","error_msg":"The request param is invalid token cloud-token-u1 refresh-token-u1"}`))
	})
	provider, accounts := newT5Provider(t, "u1")
	_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindBadRequest || apiErr.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("error = %v, want bad request", err)
	}
	if strings.Contains(apiErr.SafeMessage(), "cloud-token-u1") || strings.Contains(apiErr.SafeMessage(), "refresh-token-u1") {
		t.Fatalf("secret leaked in safe message: %q", apiErr.SafeMessage())
	}
	if fake.callCount() != 1 {
		t.Fatalf("calls = %d, want no failover", fake.callCount())
	}
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d, want 0", accounts["u1"].activeConcurrent)
	}
}

func TestChatStreamContextCancelClosesAndReleases(t *testing.T) {
	newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		t5WriteSSEFrames(w, t5DataFrame(`{"choices":[{"index":0,"delta":{"content":"first"},"finish_reason":null}]}`))
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	provider, accounts := newT5Provider(t, "u1")
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := provider.OpenChat(ctx, t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	if _, err := stream.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	done := make(chan error, 1)
	go func() {
		_, nextErr := stream.Next()
		done <- nextErr
	}()
	select {
	case nextErr := <-done:
		if !errors.Is(nextErr, context.Canceled) {
			t.Fatalf("Next after cancel = %v, want context.Canceled", nextErr)
		}
	case <-deadline:
		t.Fatal("Next did not observe context cancellation")
	}
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d, want 0", accounts["u1"].activeConcurrent)
	}
	_ = stream.Close()
	_ = stream.Close()
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("activeConcurrent after repeated Close = %d", accounts["u1"].activeConcurrent)
	}
}

func TestChatStreamCloseIsIdempotentAndReleasesOnce(t *testing.T) {
	newT5Fake(t, nil)
	provider, accounts := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	account := accounts["u1"]
	if account.activeConcurrent != 1 {
		t.Fatalf("activeConcurrent = %d, want 1", account.activeConcurrent)
	}
	for i := 0; i < 3; i++ {
		if err := stream.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	if account.activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d, want 0", account.activeConcurrent)
	}

	second, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("second OpenChat: %v", err)
	}
	if account.activeConcurrent != 1 {
		t.Fatalf("slot was not released for the second request: %d", account.activeConcurrent)
	}
	_ = second.Close()
	if account.activeConcurrent != 0 {
		t.Fatalf("second Close left activeConcurrent = %d", account.activeConcurrent)
	}
}

func TestChatConcurrencyRetryThenSuccess(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_msg":"并发会话数已达上限(3个)，请关闭部分会话后重试。"}`)
			return
		}
		t5WriteSuccess(w)
	})
	provider, accounts := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, "retry"))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := t5CollectFrames(t, stream)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	if fake.callCount() != 2 {
		t.Fatalf("calls = %d, want retry once", fake.callCount())
	}
	captures := fake.snapshot()
	if captures[0].Body["chat_id"] != captures[1].Body["chat_id"] {
		t.Fatalf("retry used a different chat_id: %#v/%#v", captures[0].Body["chat_id"], captures[1].Body["chat_id"])
	}
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d", accounts["u1"].activeConcurrent)
	}
}

func TestChatConcurrencyRetryExhaustsThenFailsOver(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.Header.Get("x-auth-token") == "cloud-token-u1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_msg":"concurrent session limit reached"}`)
			return
		}
		t5WriteSuccess(w)
	})
	provider, accounts := newT5Provider(t, "u1", "u2")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = t5CollectFrames(t, stream)
	if fake.callCount() != 3 {
		t.Fatalf("calls = %d, want 2 on u1 + 1 on u2", fake.callCount())
	}
	if accounts["u1"].State.CoolUntil == 0 {
		t.Fatal("u1 was not cooled after concurrency exhaustion")
	}
	if accounts["u2"].State.CoolUntil != 0 {
		t.Fatalf("u2 cooled unexpectedly: %+v", accounts["u2"].State)
	}
}

func TestChatConcurrencyAllAccountsFail(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error_msg":"并发会话数已达上限(3个)"}`)
	})
	provider, accounts := newT5Provider(t, "u1")
	_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusTooManyRequests || apiErr.ErrorType() != "rate_limit_error" {
		t.Fatalf("error = %v, want 429 rate_limit_error", err)
	}
	if fake.callCount() != 2 {
		t.Fatalf("calls = %d, want initial + one retry", fake.callCount())
	}
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("activeConcurrent = %d", accounts["u1"].activeConcurrent)
	}
}

func TestChatCreditFailoverMarksAccountAndSucceeds(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.Header.Get("x-auth-token") == "cloud-token-u1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"您的积分已耗尽，请关注活动积分领取活动或者升级套餐。","type":"invalid_request_error","code":400}`)
			return
		}
		t5WriteSuccess(w)
	})
	provider, accounts := newT5Provider(t, "u1", "u2")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = t5CollectFrames(t, stream)
	if fake.callCount() != 2 {
		t.Fatalf("calls = %d, want failover", fake.callCount())
	}
	if !accounts["u1"].CreditExhausted() {
		t.Fatal("credit-exhausted account was not parked")
	}
	if accounts["u2"].CreditExhausted() {
		t.Fatal("healthy account was parked")
	}
	if accounts["u2"].activeConcurrent != 0 || accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("slot leak after credit failover: u1=%d u2=%d", accounts["u1"].activeConcurrent, accounts["u2"].activeConcurrent)
	}
}

func TestChatAllCreditsExhaustedReturnsReadable400(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"insufficient credit","type":"invalid_request_error","code":400}`)
	})
	provider, accounts := newT5Provider(t, "u1", "u2")
	_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400", err)
	}
	const want = "所有可用的 CodeArts 账号积分已耗尽，请领取积分或升级套餐。"
	if apiErr.SafeMessage() != want {
		t.Fatalf("message = %q, want %q", apiErr.SafeMessage(), want)
	}
	if fake.callCount() != 2 {
		t.Fatalf("calls = %d", fake.callCount())
	}
	for uid, account := range accounts {
		if !account.CreditExhausted() {
			t.Fatalf("%s was not marked credit exhausted", uid)
		}
		if account.activeConcurrent != 0 {
			t.Fatalf("%s slot leak: %d", uid, account.activeConcurrent)
		}
	}
}

func TestChatAuthTerminalDisablesAndFailsOver(t *testing.T) {
	t.Run("failover to healthy account", func(t *testing.T) {
		fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
			if r.Header.Get("x-auth-token") == "cloud-token-u1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error_msg":"bad token cloud-token-u1 refresh-token-u1"}`)
				return
			}
			t5WriteSuccess(w)
		})
		provider, accounts := newT5Provider(t, "u1", "u2")
		stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
			"model":    "GLM-5.2",
			"messages": t5Messages(t5UserMessage("hi")),
		}, ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		_ = t5CollectFrames(t, stream)
		if fake.callCount() != 2 {
			t.Fatalf("calls = %d", fake.callCount())
		}
		if !accounts["u1"].State.Disabled {
			t.Fatal("401 account was not disabled")
		}
		if strings.Contains(accounts["u1"].State.LastError, "cloud-token-u1") || strings.Contains(accounts["u1"].State.LastError, "refresh-token-u1") {
			t.Fatalf("secret leaked into state: %q", accounts["u1"].State.LastError)
		}
		if accounts["u1"].activeConcurrent != 0 {
			t.Fatalf("u1 slot leak: %d", accounts["u1"].activeConcurrent)
		}
	})

	t.Run("only account returns v1 auth status", func(t *testing.T) {
		fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error_msg":"forbidden"}`)
		})
		provider, accounts := newT5Provider(t, "u1")
		_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
			"model":    "GLM-5.2",
			"messages": t5Messages(t5UserMessage("hi")),
		}, ""))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth {
			t.Fatalf("error = %v, want auth", err)
		}
		if apiErr.HTTPStatus() != http.StatusBadGateway {
			t.Fatalf("client status = %d, want v1 502", apiErr.HTTPStatus())
		}
		if fake.callCount() != 1 {
			t.Fatalf("calls = %d", fake.callCount())
		}
		if !accounts["u1"].State.Disabled {
			t.Fatal("403 account was not disabled")
		}
		if accounts["u1"].activeConcurrent != 0 {
			t.Fatalf("slot leak: %d", accounts["u1"].activeConcurrent)
		}
	})
}

func TestChatSoftFailuresFailOver(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCalls  int
		wantCooled bool
	}{
		{name: "429", status: http.StatusTooManyRequests, body: `{"error_msg":"slow down"}`, wantCalls: 2, wantCooled: true},
		{name: "500", status: http.StatusInternalServerError, body: `{"error_msg":"boom"}`, wantCalls: 2, wantCooled: true},
		{name: "503", status: http.StatusServiceUnavailable, body: `{"error_msg":"unavailable"}`, wantCalls: 2, wantCooled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
				if r.Header.Get("x-auth-token") == "cloud-token-u1" {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				t5WriteSuccess(w)
			})
			provider, accounts := newT5Provider(t, "u1", "u2")
			stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
				"model":    "GLM-5.2",
				"messages": t5Messages(t5UserMessage("hi")),
			}, ""))
			if err != nil {
				t.Fatalf("OpenChat: %v", err)
			}
			_ = t5CollectFrames(t, stream)
			if fake.callCount() != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", fake.callCount(), tc.wantCalls)
			}
			if tc.wantCooled && accounts["u1"].State.CoolUntil == 0 {
				t.Fatalf("u1 was not cooled: %+v", accounts["u1"].State)
			}
			if accounts["u1"].activeConcurrent != 0 || accounts["u2"].activeConcurrent != 0 {
				t.Fatalf("slot leak: u1=%d u2=%d", accounts["u1"].activeConcurrent, accounts["u2"].activeConcurrent)
			}
		})
	}
}

func TestChatNetworkFailureFailsOver(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		t5WriteSuccess(w)
	})
	deadListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadURL := "http://" + deadListener.Addr().String()
	_ = deadListener.Close()

	provider, accounts := newT5Provider(t, "u1", "u2")
	provider.Pool().ApplyProxySlots(transport.Slots{{ID: "dead", URL: deadURL, Enabled: true}})
	if err := provider.Pool().SetProxySlot("u1", "dead"); err != nil {
		t.Fatalf("SetProxySlot: %v", err)
	}
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = t5CollectFrames(t, stream)
	if fake.callCount() != 1 {
		t.Fatalf("healthy account call count = %d, want 1", fake.callCount())
	}
	if accounts["u1"].State.CoolUntil == 0 {
		t.Fatalf("network failure did not cool u1: %+v", accounts["u1"].State)
	}
	if accounts["u2"].activeConcurrent != 0 {
		t.Fatalf("u2 slot leak: %d", accounts["u2"].activeConcurrent)
	}
}

func TestChatValidation400DoesNotFailover(t *testing.T) {
	fake, _ := newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error_msg":"The request param is invalid, Please check it"}`)
	})
	provider, accounts := newT5Provider(t, "u1", "u2")
	_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400", err)
	}
	if fake.callCount() != 1 {
		t.Fatalf("calls = %d, want no failover", fake.callCount())
	}
	if accounts["u1"].State.CoolUntil != 0 {
		t.Fatalf("validation error cooled u1: %+v", accounts["u1"].State)
	}
	if accounts["u1"].activeConcurrent != 0 {
		t.Fatalf("slot leak: %d", accounts["u1"].activeConcurrent)
	}
}

func TestChatUsesAccountProxy(t *testing.T) {
	t.Setenv("WB_CODEARTS_SNAP_HOST", "http://snap.unit.test")
	var mu sync.Mutex
	paths := make([]string, 0, 1)
	hosts := make([]string, 0, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		hosts = append(hosts, r.Host)
		mu.Unlock()
		t5WriteSuccess(w)
	}))
	t.Cleanup(proxy.Close)

	provider, _ := newT5Provider(t, "u1")
	provider.Pool().ApplyProxySlots(transport.Slots{{ID: "proxy-1", URL: proxy.URL, Enabled: true}})
	if err := provider.Pool().SetProxySlot("u1", "proxy-1"); err != nil {
		t.Fatalf("SetProxySlot: %v", err)
	}
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	_ = t5CollectFrames(t, stream)
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != chatCompletionsPath {
		t.Fatalf("proxy paths = %#v", paths)
	}
	if len(hosts) != 1 || hosts[0] != "snap.unit.test" {
		t.Fatalf("proxy hosts = %#v", hosts)
	}
}

func TestChatNoAccountUnavailable(t *testing.T) {
	newT5Fake(t, nil)
	provider, _ := newT5Provider(t)
	_, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want 503", err)
	}
	if !strings.Contains(apiErr.SafeMessage(), "没有可用的 CodeArts 账号") {
		t.Fatalf("message = %q", apiErr.SafeMessage())
	}
}
