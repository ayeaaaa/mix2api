package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestMessagesStreamEvents(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(
		map[string]any{"id": "chatcmpl_2", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hel"}}}},
		map[string]any{"id": "chatcmpl_2", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "lo"}}}},
		map[string]any{"id": "chatcmpl_2", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 2, "total_tokens": 13,
				"prompt_cache_hit_tokens": 3, "prompt_cache_write_tokens": 1}},
	)...)
	server, _ := messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("Messages stream must not emit [DONE]")
	}
	if strings.Contains(rec.Body.String(), "event: ping") || strings.Contains(rec.Body.String(), ": ping") {
		t.Fatalf("Messages stream emitted a ping: %s", rec.Body)
	}
	events := parseSSEBody(t, rec.Body.String())
	if len(events) < 6 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Event != "message_start" || events[len(events)-1].Event != "message_stop" {
		t.Fatalf("event order = %#v", events)
	}
	counts := map[string]int{}
	for index, event := range events {
		counts[event.Event]++
		if event.Data["type"] != event.Event {
			t.Fatalf("event %d data type = %#v, event = %q", index, event.Data["type"], event.Event)
		}
	}
	if counts["content_block_start"] != 1 || counts["content_block_stop"] != 1 || counts["content_block_delta"] != 2 {
		t.Fatalf("content lifecycle = %#v", counts)
	}
	start := events[0].Data["message"].(map[string]any)
	if start["model"] != "m1" || start["role"] != "assistant" {
		t.Fatalf("message_start = %#v", start)
	}
	text := ""
	for _, event := range events {
		if event.Event == "content_block_delta" && event.Data["delta"].(map[string]any)["type"] == "text_delta" {
			text += stringValue(event.Data["delta"].(map[string]any)["text"])
		}
	}
	if text != "hello" {
		t.Fatalf("text deltas = %q", text)
	}
	delta := events[len(events)-2]
	if delta.Event != "message_delta" {
		t.Fatalf("penultimate event = %s", delta.Event)
	}
	if delta.Data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("stop reason = %#v", delta.Data)
	}
	usage := delta.Data["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestMessagesStreamThinkingAndToolEvents(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "hmm"}}}},
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": " ok"}}}},
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "42"}}}},
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "Bash", "arguments": `{"cmd":`}},
		}}}}},
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "function": map[string]any{"arguments": `"ls"}`}},
		}}, "finish_reason": "tool_calls"}}},
	)...)
	server, _ := messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	starts := []map[string]any{}
	stopIndices := []float64{}
	thinking := ""
	text := ""
	partial := ""
	for _, event := range events {
		switch event.Event {
		case "content_block_start":
			block := event.Data["content_block"].(map[string]any)
			starts = append(starts, block)
		case "content_block_stop":
			stopIndices = append(stopIndices, event.Data["index"].(float64))
		case "content_block_delta":
			delta := event.Data["delta"].(map[string]any)
			switch delta["type"] {
			case "thinking_delta":
				thinking += stringValue(delta["thinking"])
			case "text_delta":
				text += stringValue(delta["text"])
			case "input_json_delta":
				partial += stringValue(delta["partial_json"])
			}
		}
	}
	if len(starts) != 3 || starts[0]["type"] != "thinking" || starts[1]["type"] != "text" || starts[2]["type"] != "tool_use" {
		t.Fatalf("starts = %#v", starts)
	}
	if thinking != "hmm ok" || text != "42" {
		t.Fatalf("thinking=%q text=%q", thinking, text)
	}
	if partial != `{"cmd":"ls"}` {
		t.Fatalf("partial = %q", partial)
	}
	if len(stopIndices) != 3 || stopIndices[0] != 0 || stopIndices[1] != 1 || stopIndices[2] != 2 {
		t.Fatalf("stop indices = %#v", stopIndices)
	}
	delta := events[len(events)-2]
	if delta.Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop reason = %#v", delta.Data)
	}
}

func TestMessagesStreamAbort(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
	var observed []ProtocolObservation
	server, _ := messagesTestServer(t, stream, ServerOptions{
		ProtocolObserver: func(observation ProtocolObservation) { observed = append(observed, observation) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m1","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "message_stop") || strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("abort emitted a terminal event: %s", rec.Body)
	}
	if stream.closes() != 1 {
		t.Fatalf("stream closes = %d", stream.closes())
	}
	if len(observed) != 1 || observed[0].Outcome != "client_aborted" || observed[0].Protocol != "messages" {
		t.Fatalf("observations = %#v", observed)
	}
}

func TestMessagesStreamPostDoneUsage(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	frames := chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}, "finish_reason": "stop"}}},
	)
	frames = append(frames,
		"data: [DONE]\n\n",
		"data: {\"id\":\"u\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":2,\"total_tokens\":13,\"prompt_cache_hit_tokens\":3,\"prompt_cache_write_tokens\":1}}\n\n",
	)
	stream := newStubStream(frames...)
	server, _ := messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	delta := events[len(events)-2]
	if delta.Event != "message_delta" {
		t.Fatalf("last delta = %s", delta.Event)
	}
	usage := delta.Data["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(2) ||
		usage["cache_read_input_tokens"] != float64(3) || usage["cache_creation_input_tokens"] != float64(1) {
		t.Fatalf("post-DONE usage = %#v", usage)
	}
}

func TestMessagesStreamMidstreamCancel(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}},
	)...)
	stream.terminal = context.Canceled
	var observed []ProtocolObservation
	server, _ := messagesTestServer(t, stream, ServerOptions{
		ProtocolObserver: func(observation ProtocolObservation) { observed = append(observed, observation) },
	})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	body := rec.Body.String()
	if strings.Contains(body, "message_stop") || strings.Contains(body, "event: error") {
		t.Fatalf("cancellation emitted a terminal event: %s", body)
	}
	if stream.closes() != 1 {
		t.Fatalf("stream closes = %d", stream.closes())
	}
	if len(observed) != 1 || observed[0].Outcome != "client_aborted" {
		t.Fatalf("observations = %#v", observed)
	}
}

func TestMessagesStreamUpstreamError(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}},
	)...)
	stream.terminal = errors.New("upstream died")
	server, _ := messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	if len(events) == 0 || events[len(events)-1].Event != "error" {
		t.Fatalf("events = %#v", events)
	}
	errorBody := events[len(events)-1].Data["error"].(map[string]any)
	if errorBody["type"] != "api_error" {
		t.Fatalf("error = %#v", errorBody)
	}
	for _, event := range events {
		if event.Event == "message_stop" {
			t.Fatalf("message_stop after upstream error")
		}
	}
}

func TestMessagesOpenErrorUsesAnthropicEnvelope(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}},
		openErr: &mappedTestError{status: http.StatusBadRequest, errType: "invalid_request_error", message: "bad request"}}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	requireAnthropicError(t, rec, http.StatusBadRequest, "invalid_request_error")
	if strings.Contains(rec.Body.String(), "text/event-stream") {
		t.Fatalf("error response used SSE")
	}
}

func TestMessagesStreamJSONDataMatchesEvent(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	stream := newStubStream(chatSSE(textChunks("u", "ok")...)...)
	server, _ := messagesTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	for _, event := range parseSSEBody(t, rec.Body.String()) {
		encoded, err := json.Marshal(event.Data)
		if err != nil || len(encoded) == 0 {
			t.Fatalf("event %s: %v", event.Event, err)
		}
	}
}
