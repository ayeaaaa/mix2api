package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesStreamEventSequence(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "think"}}}},
		map[string]any{"id": "u1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}},
		map[string]any{"id": "u1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "run", "arguments": `{"x":`}},
			map[string]any{"index": 1, "id": "call_2", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": `{"input":"patch"}`}},
		}}}}},
		map[string]any{"id": "u1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}},
		}}, "finish_reason": "tool_calls"}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}},
	)...)
	server, provider := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses?realm=intl",
		`{"model":"m1","input":"hi","stream":true,"tools":[{"type":"function","name":"run","parameters":{"type":"object"}},{"type":"custom","name":"apply_patch"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), "event: ping") {
		t.Fatalf("Responses stream must not emit [DONE]: %s", rec.Body)
	}
	events := parseSSEBody(t, rec.Body.String())
	names := make([]string, 0, len(events))
	counts := map[string]int{}
	for index, event := range events {
		names = append(names, event.Event)
		counts[event.Event]++
		if value, ok := event.Data["type"]; !ok || value != event.Event {
			t.Fatalf("event %d data type = %#v, event = %q", index, value, event.Event)
		}
		if sequence, ok := event.Data["sequence_number"].(float64); !ok || int(sequence) != index+1 {
			t.Fatalf("event %d sequence = %#v", index, event.Data["sequence_number"])
		}
	}
	if len(events) < 10 || names[0] != "response.created" || names[1] != "response.in_progress" ||
		names[len(names)-1] != "response.completed" {
		t.Fatalf("event order = %v", names)
	}
	if counts["response.created"] != 1 || counts["response.completed"] != 1 {
		t.Fatalf("lifecycle counts = %#v", counts)
	}
	for _, required := range []string{
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",
	} {
		if counts[required] == 0 {
			t.Fatalf("missing event %s in %v", required, names)
		}
	}
	completed := events[len(events)-1].Data["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Fatalf("completed status = %#v", completed["status"])
	}
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(3) {
		t.Fatalf("completed usage = %#v", usage)
	}
	outputTypes := []string{}
	for _, rawItem := range completed["output"].([]any) {
		outputTypes = append(outputTypes, stringValue(rawItem.(map[string]any)["type"]))
	}
	if strings.Join(outputTypes, ",") != "reasoning,message,function_call,custom_tool_call" {
		t.Fatalf("completed output = %v", outputTypes)
	}
	var customAdded map[string]any
	for _, event := range events {
		if event.Event != "response.output_item.added" {
			continue
		}
		item := event.Data["item"].(map[string]any)
		if item["type"] == "custom_tool_call" {
			customAdded = item
		}
	}
	if customAdded == nil || customAdded["name"] != "apply_patch" {
		t.Fatalf("custom added item = %#v", customAdded)
	}
	raw := provider.opened()
	if len(raw) != 1 || raw[0].Model != "m1" {
		t.Fatalf("upstream requests = %#v", raw)
	}
	if _, leaked := raw[0].Raw["_namespace_map"]; leaked {
		t.Fatalf("gateway-only namespace map forwarded")
	}
}

func TestResponsesStreamPostDoneUsage(t *testing.T) {
	frames := chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}, "finish_reason": "stop"}}},
	)
	frames = append(frames,
		"data: [DONE]\n\n",
		"data: {\"id\":\"u\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":2,\"total_tokens\":13}}\n\n",
	)
	stream := newStubStream(frames...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	completed := events[len(events)-1].Data["response"].(map[string]any)
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(2) {
		t.Fatalf("post-DONE usage = %#v", usage)
	}
}

func TestResponsesStreamTruncatedToolDropped(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "run", "arguments": `{"path":`}},
		}}, "finish_reason": "length"}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true,"tools":[{"type":"function","name":"run"}]}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	for _, event := range events {
		switch event.Event {
		case "response.function_call_arguments.done", "response.output_item.done":
			if item, ok := event.Data["item"].(map[string]any); ok && item["type"] == "function_call" {
				t.Fatalf("truncated call closed out: %#v", event)
			}
			if event.Event == "response.function_call_arguments.done" {
				t.Fatalf("truncated call arguments.done emitted: %#v", event)
			}
		}
	}
	last := events[len(events)-1]
	if last.Event != "response.completed" {
		t.Fatalf("last event = %#v", last)
	}
	response := last.Data["response"].(map[string]any)
	if response["status"] != "incomplete" {
		t.Fatalf("status = %#v", response["status"])
	}
}

func TestResponsesStreamEmptyReasoningWithTruncatedCall(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "only thinking"}}}},
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "run", "arguments": `{"path":`}},
		}}}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true,"tools":[{"type":"function","name":"run"}]}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	if events[len(events)-1].Event != "response.completed" {
		t.Fatalf("reasoning + dropped call was classified as a failed response: %#v", events[len(events)-1])
	}
	response := events[len(events)-1].Data["response"].(map[string]any)
	if response["status"] != "incomplete" {
		t.Fatalf("status = %#v", response["status"])
	}
}

func TestResponsesStreamMidstreamCancel(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}},
	)...)
	stream.terminal = context.Canceled
	var observed []ProtocolObservation
	server, _ := responseTestServer(t, stream, ServerOptions{
		ProtocolObserver: func(observation ProtocolObservation) { observed = append(observed, observation) },
	})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true}`, nil)
	body := rec.Body.String()
	if strings.Contains(body, "response.completed") || strings.Contains(body, "response.failed") {
		t.Fatalf("cancellation emitted a terminal event: %s", body)
	}
	if stream.closes() != 1 {
		t.Fatalf("stream closes = %d", stream.closes())
	}
	if len(observed) != 1 || observed[0].Outcome != "client_aborted" {
		t.Fatalf("observations = %#v", observed)
	}
}

func TestResponsesStreamToolArgumentsNotDuplicated(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "run", "arguments": `{"city":"`}},
		}}}}},
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "function": map[string]any{"arguments": `Beijing"}`}},
		}}, "finish_reason": "tool_calls"}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true,"tools":[{"type":"function","name":"run"}]}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	arguments := ""
	done := ""
	for _, event := range events {
		if event.Event == "response.function_call_arguments.delta" {
			arguments += stringValue(event.Data["delta"])
		}
		if event.Event == "response.function_call_arguments.done" {
			done = stringValue(event.Data["arguments"])
		}
	}
	if arguments != `{"city":"Beijing"}` || done != arguments {
		t.Fatalf("arguments = %q done = %q", arguments, done)
	}
}

func TestResponsesStreamUsageEstimateAndReasoningFailure(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}, "finish_reason": "stop"}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hello","stream":true}`, nil)
	events := parseSSEBody(t, rec.Body.String())
	completed := events[len(events)-1].Data["response"].(map[string]any)
	usage, ok := completed["usage"].(map[string]any)
	if !ok {
		t.Fatalf("estimated usage missing; events=%#v", events)
	}
	if value, ok := usage["output_tokens"].(float64); !ok || value <= 0 {
		t.Fatalf("estimated usage = %#v", usage)
	}

	reasoning := newStubStream(chatSSE(
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "only"}, "finish_reason": "stop"}}},
	)...)
	server, _ = responseTestServer(t, reasoning, ServerOptions{})
	rec = serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true}`, nil)
	events = parseSSEBody(t, rec.Body.String())
	if events[len(events)-1].Event != "response.failed" {
		t.Fatalf("reasoning-only events = %#v", events)
	}
	response := events[len(events)-1].Data["response"].(map[string]any)
	if response["error"].(map[string]any)["code"] != "empty_reasoning_only" {
		t.Fatalf("failed response = %#v", response)
	}
}

func TestResponsesStreamJSONShape(t *testing.T) {
	stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/responses", `{"model":"m1","input":"hi","stream":true}`, nil)
	for _, event := range parseSSEBody(t, rec.Body.String()) {
		encoded, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatalf("event %s: %v", event.Event, err)
		}
		if len(encoded) == 0 {
			t.Fatalf("empty event %s", event.Event)
		}
	}
}
