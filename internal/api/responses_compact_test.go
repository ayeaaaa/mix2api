package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func compactionItemFromBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	output, ok := body["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("compaction output = %#v", body["output"])
	}
	item, ok := output[0].(map[string]any)
	if !ok || item["type"] != "compaction" {
		t.Fatalf("compaction item = %#v", output[0])
	}
	return item
}

func TestResponsesCompactionLegacyAndTrigger(t *testing.T) {
	for _, path := range []string{"/responses/compact", "/v1/responses/compact"} {
		stream := newStubStream(chatSSE(textChunks("u", "legacy summary")...)...)
		server, _ := responseTestServer(t, stream, ServerOptions{})
		rec := serveRequest(server.Handler(), http.MethodPost, path,
			`{"model":"m1","input":[{"type":"message","role":"user","content":"do work"}]}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s body: %v", path, err)
		}
		item := compactionItemFromBody(t, body)
		if summary := decodeCompactionSummary(item["encrypted_content"]); summary != "legacy summary" {
			t.Fatalf("%s summary = %q", path, summary)
		}
		if body["output_text"] != "" {
			t.Fatalf("%s output_text = %#v", path, body["output_text"])
		}
		if _, ok := body["usage"].(map[string]any); !ok {
			t.Fatalf("%s usage = %#v", path, body["usage"])
		}
	}

	stream := newStubStream(chatSSE(textChunks("u", "trigger summary")...)...)
	server, provider := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{
		"model":"m1",
		"input":[
			{"type":"message","role":"user","content":"do work"},
			{"type":"compaction_trigger"}
		],
		"tools":[{"type":"function","name":"run","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger status = %d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("trigger body: %v", err)
	}
	item := compactionItemFromBody(t, body)
	if summary := decodeCompactionSummary(item["encrypted_content"]); summary != "trigger summary" {
		t.Fatalf("trigger summary = %q", summary)
	}
	requests := provider.opened()
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %d", len(requests))
	}
	raw := requests[0].Raw
	if _, exists := raw["tools"]; exists {
		t.Fatalf("compaction forwarded tools: %#v", raw["tools"])
	}
	if _, exists := raw["tool_choice"]; exists {
		t.Fatalf("compaction forwarded tool_choice")
	}
	if _, exists := raw["parallel_tool_calls"]; exists {
		t.Fatalf("compaction forwarded parallel_tool_calls")
	}
	var messages []map[string]any
	if err := json.Unmarshal(raw["messages"], &messages); err != nil {
		t.Fatalf("messages: %v", err)
	}
	last := messages[len(messages)-1]
	if last["role"] != "user" || !strings.Contains(stringValue(last["content"]), "CONTEXT CHECKPOINT COMPACTION") {
		t.Fatalf("last message = %#v", last)
	}
}

func TestResponsesOrdinaryTurnIsNotCompaction(t *testing.T) {
	stream := newStubStream(chatSSE(textChunks("u", "ordinary answer")...)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi"}`, nil)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	output := body["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("ordinary output = %#v", body["output"])
	}
}

func TestResponsesCompactionStream(t *testing.T) {
	for _, path := range []string{"/responses/compact", "/v1/responses/compact"} {
		stream := newStubStream(chatSSE(textChunks("u", "stream summary")...)...)
		server, _ := responseTestServer(t, stream, ServerOptions{})
		rec := serveRequest(server.Handler(), http.MethodPost, path, `{"model":"m1","input":"do work","stream":true}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body)
		}
		events := parseSSEBody(t, rec.Body.String())
		counts := map[string]int{}
		var compactionItems []map[string]any
		for _, event := range events {
			counts[event.Event]++
			if event.Event == "response.output_item.added" {
				t.Fatalf("%s emitted output_item.added for compaction", path)
			}
			if event.Event == "response.output_item.done" {
				if item, ok := event.Data["item"].(map[string]any); ok && item["type"] == "compaction" {
					compactionItems = append(compactionItems, item)
				}
			}
		}
		if len(compactionItems) != 1 {
			t.Fatalf("%s compaction items = %#v", path, compactionItems)
		}
		if summary := decodeCompactionSummary(compactionItems[0]["encrypted_content"]); summary != "stream summary" {
			t.Fatalf("%s summary = %q", path, summary)
		}
		if counts["response.created"] != 1 || counts["response.in_progress"] != 1 ||
			counts["response.completed"] != 1 || counts["response.failed"] != 0 {
			t.Fatalf("%s lifecycle = %#v", path, counts)
		}
		if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), `"output_text":"stream summary"`) {
			t.Fatalf("%s emitted ordinary text", path)
		}
		if last := events[len(events)-1]; last.Event != "response.completed" {
			t.Fatalf("%s last event = %s", path, last.Event)
		}
	}
}

func TestResponsesCompactionEmptySummary(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "   "}, "finish_reason": "stop"}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses/compact", `{"model":"m1","input":"do work"}`, nil)
	failure := requireErrorEnvelope(t, rec, http.StatusBadGateway, "server_error")
	if !strings.Contains(stringValue(failure["message"]), "compaction produced no summary") {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestCompactionReplayBecomesContext(t *testing.T) {
	blob := encodeCompactionSummary("earlier work summary")
	chat, err := responsesToChat(rawPayload(t, map[string]any{
		"model": "m1",
		"input": []any{
			map[string]any{"type": "compaction", "encrypted_content": blob},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	}))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || !strings.Contains(stringValue(system["content"]), "earlier work summary") {
		t.Fatalf("replayed summary = %#v", system)
	}
	if messages[1].(map[string]any)["content"] != "continue" {
		t.Fatalf("replayed user = %#v", messages[1])
	}
}

func TestCompactionSummaryCodec(t *testing.T) {
	blob := encodeCompactionSummary("the summary")
	if got := decodeCompactionSummary(blob); got != "the summary" {
		t.Fatalf("round trip = %q", got)
	}
	if got := decodeCompactionSummary("plain text"); got != "plain text" {
		t.Fatalf("plain text = %q", got)
	}
	if got := decodeCompactionSummary(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	if !hasCompactionTrigger(map[string]any{"input": "compaction_trigger"}) ||
		!hasCompactionTrigger(map[string]any{"input": []any{map[string]any{"type": "compaction_trigger"}}}) {
		t.Fatalf("trigger detection failed")
	}
	if hasCompactionTrigger(map[string]any{"input": "hello"}) ||
		hasCompactionTrigger(map[string]any{"input": []any{map[string]any{"type": "message"}}}) {
		t.Fatalf("false positive trigger")
	}
	if !looksLikeCompactionPrompt([]any{map[string]any{"role": "user", "content": compactionPrompt}}) {
		t.Fatalf("prompt detection failed")
	}
}
