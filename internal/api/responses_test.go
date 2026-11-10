package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func rawPayload(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

func chatSSE(chunks ...map[string]any) []string {
	frames := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		encoded, _ := json.Marshal(chunk)
		frames = append(frames, "data: "+string(encoded)+"\n\n")
	}
	return frames
}

func textChunks(id, text string) []map[string]any {
	return []map[string]any{
		{"id": id, "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}},
		{"id": id, "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}},
	}
}

type parseSSEEvent struct {
	Event string
	Data  map[string]any
}

func parseSSEBody(t *testing.T, body string) []parseSSEEvent {
	t.Helper()
	events := []parseSSEEvent{}
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		eventName := ""
		dataLines := []string{}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				eventName = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			}
		}
		if eventName == "" || len(dataLines) == 0 {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &data); err != nil {
			t.Fatalf("event %s data %q: %v", eventName, strings.Join(dataLines, "\n"), err)
		}
		events = append(events, parseSSEEvent{Event: eventName, Data: data})
	}
	return events
}

func responseTestServer(t *testing.T, stream *stubStream, options ServerOptions) (*Server, *stubProvider) {
	t.Helper()
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: stream}
	options.Version = "2.0.0-test"
	options.Providers = []providers.Provider{provider}
	return newTestServer(options), provider
}

func TestResponsesRequestMapsToChat(t *testing.T) {
	payload := rawPayload(t, map[string]any{
		"model":        "m1",
		"instructions": "be brief",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "look"},
				map[string]any{"type": "input_image", "data": "aaa", "mimeType": "image/png"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "run", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "answer"},
			}},
		},
		"previous_response_id": "resp_old",
	})
	chat, err := responsesToChat(payload)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	messages, _ := chat["messages"].([]any)
	if len(messages) != 5 {
		t.Fatalf("messages = %#v", messages)
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be brief" {
		t.Fatalf("system = %#v", system)
	}
	user := messages[1].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("multimodal user = %#v", user["content"])
	}
	image := parts[1].(map[string]any)["image_url"].(map[string]any)
	if image["url"] != "data:image/png;base64,aaa" {
		t.Fatalf("image URL = %#v", image["url"])
	}
	toolCall := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if toolCall["id"] != "call_1" || toolCall["function"].(map[string]any)["name"] != "run" {
		t.Fatalf("tool call = %#v", toolCall)
	}
	toolMessage := messages[3].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_1" || toolMessage["content"] != "done" {
		t.Fatalf("tool message = %#v", toolMessage)
	}
	if messages[4].(map[string]any)["content"] != "answer" {
		t.Fatalf("assistant text = %#v", messages[4])
	}
	if _, present := chat["previous_response_id"]; present {
		t.Fatalf("previous_response_id forwarded: %#v", chat)
	}

	t.Setenv("WB_PRESERVE_REQUEST_CONTRACT", "1")
	formatted := rawPayload(t, map[string]any{
		"model": "m1", "input": "hi",
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "fixture", "schema": map[string]any{"type": "object"}, "strict": true,
		}},
	})
	formattedChat, err := responsesToChat(formatted)
	if err != nil {
		t.Fatalf("responsesToChat formatted: %v", err)
	}
	format, ok := formattedChat["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("response_format = %#v", formattedChat["response_format"])
	}
	if _, ok := format["json_schema"].(map[string]any)["strict"]; !ok {
		t.Fatalf("strict missing: %#v", format)
	}
	t.Setenv("WB_PRESERVE_REQUEST_CONTRACT", "0")
	unformatted, _ := responsesToChat(formatted)
	if _, present := unformatted["response_format"]; present {
		t.Fatalf("format mapped with preserve off: %#v", unformatted)
	}

	stringInput, err := responsesToChat(rawPayload(t, map[string]any{"model": "m1", "input": "hello"}))
	if err != nil {
		t.Fatalf("string input: %v", err)
	}
	if messages := stringInput["messages"].([]any); len(messages) != 1 || messages[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("string input messages = %#v", messages)
	}
}

func TestResponsesOrphanToolOutputs(t *testing.T) {
	imageOutput, err := responsesToChat(rawPayload(t, map[string]any{
		"model": "m1", "input": []any{
			map[string]any{"type": "function_call_output", "output": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aaa"},
			}},
		},
	}))
	if err != nil {
		t.Fatalf("image orphan: %v", err)
	}
	message := imageOutput["messages"].([]any)[0].(map[string]any)
	parts := message["content"].([]any)
	if message["role"] != "user" || !strings.HasPrefix(stringValue(parts[0].(map[string]any)["text"]), "[Message from another task") {
		t.Fatalf("image orphan message = %#v", message)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image orphan parts = %#v", parts)
	}
	dictOutput, err := responsesToChat(rawPayload(t, map[string]any{
		"model": "m1", "input": []any{
			map[string]any{"type": "function_call_output", "output": map[string]any{"type": "output_text", "text": "done"}},
		},
	}))
	if err != nil {
		t.Fatalf("dict orphan: %v", err)
	}
	textMessage := dictOutput["messages"].([]any)[0].(map[string]any)
	if textMessage["role"] != "user" || !strings.Contains(stringValue(textMessage["content"]), "done") {
		t.Fatalf("dict orphan message = %#v", textMessage)
	}
}

func TestResponsesImageOnlyInput(t *testing.T) {
	chat, err := responsesToChat(rawPayload(t, map[string]any{
		"model": "m1",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aaa"},
		}}},
	}))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("image-only input was dropped: %#v", messages)
	}
	message := messages[0].(map[string]any)
	parts, ok := message["content"].([]any)
	if !ok || len(parts) != 1 || parts[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image-only content = %#v", message["content"])
	}
}

func TestResponsesToolsCustomAndNamespace(t *testing.T) {
	payload := rawPayload(t, map[string]any{
		"model": "m1", "input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "function", "name": "extra", "parameters": map[string]any{"type": "object"}},
			}},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "weather", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "custom", "name": "apply_patch", "description": "edit",
				"format": map[string]any{"definition": "start: /.*/"}},
			map[string]any{"type": "namespace", "name": "codex_apps", "tools": []any{
				map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
			}},
			map[string]any{"type": "web_search"},
		},
		"additional_tools":    []any{map[string]any{"type": "function", "name": "extra", "parameters": map[string]any{"type": "object"}}},
		"tool_choice":         map[string]any{"type": "function", "function": map[string]any{"name": "codex_apps__lookup"}},
		"parallel_tool_calls": false,
	})
	chat, err := responsesToChat(payload)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if _, present := chat["_namespace_map"]; present {
		t.Fatalf("_namespace_map leaked upstream: %#v", chat)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools = %#v", tools)
	}
	byName := map[string]map[string]any{}
	for _, rawTool := range tools {
		tool := rawTool.(map[string]any)
		byName[stringValue(tool["name"])] = tool
	}
	custom := byName["apply_patch"]
	if custom == nil || custom["type"] != "function" {
		t.Fatalf("custom tool = %#v", custom)
	}
	properties := custom["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, ok := properties["input"]; !ok {
		t.Fatalf("custom tool input property missing: %#v", properties)
	}
	if !strings.Contains(stringValue(custom["description"]), "freeform tool") {
		t.Fatalf("custom tool hint missing: %#v", custom["description"])
	}
	if byName["lookup"] == nil || byName["extra"] == nil || byName["weather"] == nil {
		t.Fatalf("flat tools = %#v", byName)
	}
	if byName["web_search"] != nil {
		t.Fatalf("unknown server tool forwarded: %#v", byName["web_search"])
	}
	choice := chat["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("tool_choice = %#v", choice)
	}
	if chat["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", chat["parallel_tool_calls"])
	}
}

func TestResponsesNamespaceReturnTrip(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "js", "arguments": `{"code":"1"}`}},
		}}, "finish_reason": "tool_calls"}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{
		"model":"m1","input":"hi",
		"tools":[{"type":"namespace","name":"node_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object"}}]}]
	}`, nil)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	output := body["output"].([]any)
	var call map[string]any
	for _, rawItem := range output {
		item := rawItem.(map[string]any)
		if item["type"] == "function_call" {
			call = item
		}
	}
	if call == nil || call["name"] != "js" || call["namespace"] != "node_repl" {
		t.Fatalf("namespaced call = %#v", call)
	}
}

func TestResponsesDSMLFallback(t *testing.T) {
	dsml := `<|DSML| calls><|DSML| invoke name="run"><|DSML| parameter name="cmd">ls</|DSML| parameter></|DSML| invoke></|DSML| calls>`
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "before " + dsml}, "finish_reason": "tool_calls"}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi"}`, nil)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if strings.Contains(body["output_text"].(string), "DSML") {
		t.Fatalf("DSML leaked into output_text: %q", body["output_text"])
	}
	found := false
	for _, rawItem := range body["output"].([]any) {
		item := rawItem.(map[string]any)
		if item["type"] == "function_call" && item["name"] == "run" {
			found = true
			if !strings.Contains(stringValue(item["arguments"]), "ls") {
				t.Fatalf("DSML arguments = %#v", item["arguments"])
			}
		}
	}
	if !found {
		t.Fatalf("DSML function call missing: %#v", body["output"])
	}
}

func TestResponsesReasoningAndMaxTokens(t *testing.T) {
	chat, err := responsesToChat(rawPayload(t, map[string]any{
		"model": "m1", "input": "hi", "reasoning": map[string]any{"effort": "high"},
		"max_output_tokens": 4096,
	}))
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if chat["reasoning_effort"] != "high" {
		t.Fatalf("effort = %#v", chat["reasoning_effort"])
	}
	if number, ok := chat["max_tokens"].(json.Number); !ok || number.String() != "4096" {
		if number, ok := chat["max_tokens"].(float64); !ok || number != 4096 {
			t.Fatalf("max_tokens = %#v", chat["max_tokens"])
		}
	}
	chat, err = responsesToChat(rawPayload(t, map[string]any{
		"model": "m1", "input": "hi", "reasoning_effort": "low",
	}))
	if err != nil {
		t.Fatalf("reasoning_effort: %v", err)
	}
	if chat["reasoning_effort"] != "low" {
		t.Fatalf("effort = %#v", chat["reasoning_effort"])
	}
	translated, err := translateResponsesRequest(rawPayload(t, map[string]any{"model": "m1", "input": "hi"}), responseTranslateOptions{
		DefaultMaxOutputTokens: func(model string) int {
			if model == "m1" {
				return 128000
			}
			return 0
		},
	})
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if number, ok := translated.Chat["max_tokens"].(int); !ok || number != 128000 {
		t.Fatalf("default max_tokens = %#v", translated.Chat["max_tokens"])
	}

	stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
	provider := &stubProvider{id: "workbuddy", realm: "intl", openStream: stream,
		models: []providers.Model{{"id": "m1"}}}
	server := newTestServer(ServerOptions{
		Providers: []providers.Provider{provider},
		DefaultMaxOutputTokens: func(model string) int {
			if model == "m1" {
				return 128000
			}
			return 0
		},
	})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler default status = %d body=%s", rec.Code, rec.Body)
	}
	requests := provider.opened()
	if len(requests) != 1 {
		t.Fatalf("open requests = %d", len(requests))
	}
	if got := string(requests[0].Raw["max_tokens"]); got != "128000" {
		t.Fatalf("configured default max_tokens = %s", got)
	}
}

func TestResponsesNonStreamShape(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "up1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "think"}}}},
		map[string]any{"id": "up1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}},
		map[string]any{"id": "up1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "run", "arguments": `{"x":1}`}},
		}}}}},
		map[string]any{"id": "up1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
				"prompt_cache_hit_tokens": 3, "completion_tokens_details": map[string]any{"reasoning_tokens": 2}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses",
		`{"model":"m1","input":"hi","tool_choice":"auto","parallel_tool_calls":false,"tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["object"] != "response" || body["status"] != "completed" || body["model"] != "m1" {
		t.Fatalf("response = %#v", body)
	}
	if body["output_text"] != "hello" {
		t.Fatalf("output_text = %#v", body["output_text"])
	}
	output := body["output"].([]any)
	types := []string{}
	for _, rawItem := range output {
		types = append(types, stringValue(rawItem.(map[string]any)["type"]))
	}
	if strings.Join(types, ",") != "reasoning,function_call,message" {
		t.Fatalf("output types = %v", types)
	}
	if body["parallel_tool_calls"] != false || body["tool_choice"] != "auto" {
		t.Fatalf("echo = %#v / %#v", body["parallel_tool_calls"], body["tool_choice"])
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) ||
		usage["total_tokens"] != float64(15) {
		t.Fatalf("usage = %#v", usage)
	}
	if usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(3) {
		t.Fatalf("cached = %#v", usage["input_tokens_details"])
	}
	if usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("reasoning = %#v", usage["output_tokens_details"])
	}
}

func TestResponsesNonStreamIncompleteAndReasoningOnly(t *testing.T) {
	lengthStream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}, "finish_reason": "length"}}},
	)...)
	server, _ := responseTestServer(t, lengthStream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi"}`, nil)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["status"] != "incomplete" || body["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("length response = %#v", body)
	}

	reasoningStream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "only thinking"}, "finish_reason": "stop"}}},
	)...)
	server, _ = responseTestServer(t, reasoningStream, ServerOptions{})
	rec = serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi"}`, nil)
	requireErrorEnvelope(t, rec, http.StatusBadGateway, "server_error")
	if !strings.Contains(rec.Body.String(), "reasoning without output text") {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestResponsesPreviousResponseIDRejected(t *testing.T) {
	stream := newStubStream()
	server, provider := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses",
		`{"model":"m1","input":"hi","previous_response_id":"resp_1"}`, nil)
	requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
	if len(provider.opened()) != 0 {
		t.Fatalf("provider called for rejected request")
	}
}

func TestResponsesReasoningWithCustomToolIsNotReasoningOnly(t *testing.T) {
	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "think"}}}},
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": `{"input":"patch"}`}},
		}}, "finish_reason": "tool_calls"}}},
	)...)
	server, _ := responseTestServer(t, stream, ServerOptions{})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{
		"model":"m1","input":"hi",
		"tools":[{"type":"custom","name":"apply_patch"}]
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	found := false
	for _, rawItem := range body["output"].([]any) {
		item := rawItem.(map[string]any)
		if item["type"] == "custom_tool_call" && item["name"] == "apply_patch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("custom call missing from = %#v", body["output"])
	}
}

func TestResponsesCitationTitleUsesMarkdownLabel(t *testing.T) {
	annotations := buildCitations("[fixture label](https://example.test/a)",
		[]map[string]any{{"url": "https://example.test/a"}})
	if len(annotations) != 1 {
		t.Fatalf("annotations = %#v", annotations)
	}
	if title := annotations[0].(map[string]any)["title"]; title != "fixture label" {
		t.Fatalf("title = %#v", title)
	}
}

func TestResponsesSSEError(t *testing.T) {
	openProvider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}},
		openErr: &mappedTestError{status: 400, errType: "invalid_request_error", message: "bad model"}}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{openProvider}})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true}`, nil)
	requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")

	stream := newStubStream(chatSSE(
		map[string]any{"id": "u", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}},
	)...)
	stream.terminal = errors.New("upstream died")
	server, _ = responseTestServer(t, stream, ServerOptions{})
	rec = serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi","stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	events := parseSSEBody(t, rec.Body.String())
	if len(events) == 0 || events[len(events)-1].Event != "response.failed" {
		t.Fatalf("events = %#v", events)
	}
	for _, event := range events {
		if event.Event == "response.completed" {
			t.Fatalf("terminal success emitted after error")
		}
	}
}

func TestResponsesAliases(t *testing.T) {
	for _, path := range []string{"/responses", "/v1/responses"} {
		stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
		server, _ := responseTestServer(t, stream, ServerOptions{})
		rec := serveRequest(server.Handler(), http.MethodPost, path, `{"model":"m1","input":"hi"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["object"] != "response" {
			t.Fatalf("%s body = %s", path, rec.Body)
		}
	}
	for _, path := range []string{"/responses", "/v1/responses", "/responses/compact", "/v1/responses/compact"} {
		stream := newStubStream(chatSSE(textChunks("u", "summary")...)...)
		server, _ := responseTestServer(t, stream, ServerOptions{})
		rec := serveRequest(server.Handler(), http.MethodGet, path, "", nil)
		requireErrorEnvelope(t, rec, http.StatusMethodNotAllowed, "invalid_request_error")
		if rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s Allow = %q", path, rec.Header().Get("Allow"))
		}
	}
}

func TestResponsesClientAbort(t *testing.T) {
	stream := newStubStream(chatSSE(textChunks("u", "hi")...)...)
	var observed []ProtocolObservation
	server, _ := responseTestServer(t, stream, ServerOptions{
		ProtocolObserver: func(observation ProtocolObservation) { observed = append(observed, observation) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1","input":"hi","stream":true}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "response.completed") || strings.Contains(body, "response.failed") {
		t.Fatalf("terminal event after abort: %s", body)
	}
	if stream.closes() != 1 {
		t.Fatalf("stream closes = %d", stream.closes())
	}
	if len(observed) != 1 || observed[0].Outcome != "client_aborted" || observed[0].Protocol != "responses" {
		t.Fatalf("observations = %#v", observed)
	}
}

func TestResponsesRateLimitRetryMetadata(t *testing.T) {
	reset := time.Now().Add(30 * time.Second)
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}},
		openErr: &retryTestError{status: http.StatusTooManyRequests, reset: reset}}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(server.Handler(), http.MethodPost, "/v1/responses", `{"model":"m1","input":"hi"}`, nil)
	failure := requireErrorEnvelope(t, rec, http.StatusTooManyRequests, "rate_limit_error")
	if failure["reset_at"] == nil || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit metadata = body %#v headers %#v", failure, rec.Header())
	}
}
