package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// runResponsesWebRounds executes local web tools and returns the final chat
// completion plus the web items/sources the client should see.
func (s *Server) runResponsesWebRounds(
	ctx context.Context,
	provider providers.Provider,
	chatRequest providers.ChatRequest,
	translated *responsesTranslation,
	model string,
) (map[string]any, responseMeta, error) {
	wants := webtools.WantsFromTools(metaTools(translated.Meta.Tools))
	if !wants.Search && !wants.Fetch {
		return nil, translated.Meta, nil
	}
	if tools, ok := translated.Chat["tools"].([]map[string]any); ok {
		translated.Chat["tools"] = webtools.InstallToolDefs(tools, wants)
	} else if tools, ok := translated.Chat["tools"].([]any); ok {
		flat := make([]map[string]any, 0, len(tools))
		for _, item := range tools {
			if typed, ok := item.(map[string]any); ok {
				flat = append(flat, typed)
			}
		}
		translated.Chat["tools"] = webtools.InstallToolDefs(flat, wants)
	} else {
		translated.Chat["tools"] = webtools.ToolDefs(wants)
	}

	messages := responseMessages(translated.Chat)
	webItems := []any{}
	sources := []map[string]any{}
	var final map[string]any
	maxRounds := webtools.MaxRounds()
	for round := 0; round <= maxRounds; round++ {
		dropTools := round == maxRounds
		if dropTools {
			dropInternalWebTools(translated.Chat)
			messages = append(messages, map[string]any{
				"role": "system",
				"content": "The web tools are no longer available. Answer the user now with " +
					"what you already have. Do not say that you are searching again.",
			})
			translated.Chat["messages"] = messages
		}
		raw, err := chatMapToRaw(translated.Chat)
		if err != nil {
			return nil, translated.Meta, err
		}
		request := chatRequest
		request.Raw = raw
		request.Messages = chatMessagesRaw(translated.Chat)
		upstream, openErr := provider.OpenChat(ctx, request)
		if openErr != nil {
			if upstream != nil {
				_ = upstream.Close()
			}
			return nil, translated.Meta, openErr
		}
		chat, aggregateErr := aggregateChatStream(upstream, model)
		_ = upstream.Close()
		if aggregateErr != nil {
			return nil, translated.Meta, aggregateErr
		}
		if dropTools {
			stripInternalToolCalls(chat)
			final = chat
			break
		}
		calls := internalWebCalls(chat)
		if len(calls) == 0 {
			final = chat
			break
		}
		message := assistantMessage(chat)
		if message == nil {
			final = chat
			break
		}
		messages = append(messages, message)
		for _, call := range calls {
			if !webtools.IsInternalTool(stringValue(callFunction(call)["name"])) {
				continue
			}
			item, sourceList, result := s.executeWebCall(ctx, call)
			webItems = append(webItems, item)
			sources = append(sources, sourceList...)
			callID := stringValue(call["id"])
			if callID == "" {
				callID = randomChatID("call_")
			}
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": callID,
				"name": stringValue(callFunction(call)["name"]), "content": result,
			})
		}
		translated.Chat["messages"] = messages
		clientCalls := clientToolCalls(chat)
		if len(clientCalls) > 0 {
			// Mixed client/internal calls: internal calls are executed, but the
			// client's own tool calls are returned instead of being dropped.
			stripInternalToolCalls(chat)
			final = chat
			break
		}
	}
	if final == nil {
		final = map[string]any{"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "Web search round limit reached."},
			"finish_reason": "stop",
		}}}
	}
	meta := translated.Meta
	meta.WebItems = webItems
	meta.Sources = sources
	return final, meta, nil
}

func (s *Server) serveResponsesWithWebTools(
	w http.ResponseWriter,
	r *http.Request,
	provider providers.Provider,
	chatRequest providers.ChatRequest,
	translated *responsesTranslation,
	model string,
	started time.Time,
) bool {
	wants := webtools.WantsFromTools(metaTools(translated.Meta.Tools))
	if !wants.Search && !wants.Fetch {
		return false
	}
	final, meta, err := s.runResponsesWebRounds(r.Context(), provider, chatRequest, translated, model)
	if err != nil {
		s.observeResponseFailureDetail(r, model, false, failureFromError(err), provider.Realm())
		writeFailure(w, err)
		return true
	}
	response := chatToResponse(final, model, meta)
	attachWebItems(response, meta.WebItems, meta.Sources)
	s.observeProtocol(r, ProtocolObservation{
		Protocol: "responses", Model: model, RequestedModel: model,
		Realm: provider.Realm(), Effort: translated.Meta.Effort,
		Stream: false, Outcome: "completed", Status: http.StatusOK,
		Usage: usageMap(final["usage"]), ElapsedMS: time.Since(started).Milliseconds(),
	})
	writeJSONResponse(w, http.StatusOK, response)
	return true
}

func (s *Server) serveResponsesWithWebToolsStream(
	w http.ResponseWriter,
	r *http.Request,
	provider providers.Provider,
	chatRequest providers.ChatRequest,
	translated *responsesTranslation,
	model string,
	started time.Time,
) bool {
	wants := webtools.WantsFromTools(metaTools(translated.Meta.Tools))
	if !wants.Search && !wants.Fetch {
		return false
	}
	final, meta, err := s.runResponsesWebRounds(r.Context(), provider, chatRequest, translated, model)
	if err != nil {
		s.observeResponseFailureDetail(r, model, true, failureFromError(err), provider.Realm())
		writeFailure(w, err)
		return true
	}
	stream := newUsageTracker(newSnapshotStream(chatCompletionStream(final), snapshotFromChat(final, provider.Realm(), model)), started)
	s.serveResponsesStream(w, r, stream, model, meta)
	return true
}

// dropInternalWebTools removes only the gateway-owned function declarations on
// the final round. Client tools stay available, and a tool_choice naming a
// now-removed web tool is normalized away instead of poisoning the request.
func dropInternalWebTools(chat map[string]any) {
	if chat == nil {
		return
	}
	var kept any
	switch tools := chat["tools"].(type) {
	case []map[string]any:
		filtered := make([]map[string]any, 0, len(tools))
		for _, tool := range tools {
			if !isInternalWebTool(tool) {
				filtered = append(filtered, tool)
			}
		}
		if len(filtered) == 0 {
			delete(chat, "tools")
		} else {
			kept = filtered
			chat["tools"] = kept
		}
	case []any:
		filtered := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			if tool == nil || !isInternalWebTool(tool) {
				filtered = append(filtered, raw)
			}
		}
		if len(filtered) == 0 {
			delete(chat, "tools")
		} else {
			chat["tools"] = filtered
		}
	}
	if choice, ok := chat["tool_choice"]; ok {
		if toolChoiceIsInternal(choice) {
			delete(chat, "tool_choice")
		}
	}
}

func isInternalWebTool(tool map[string]any) bool {
	kind := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
	name := strings.ToLower(strings.TrimSpace(stringValue(tool["name"])))
	if function, ok := tool["function"].(map[string]any); ok && name == "" {
		name = strings.ToLower(strings.TrimSpace(stringValue(function["name"])))
	}
	switch kind {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_fetch":
		return true
	}
	return webtools.IsInternalTool(name)
}

func toolChoiceIsInternal(choice any) bool {
	switch typed := choice.(type) {
	case string:
		return webtools.IsInternalTool(strings.TrimSpace(typed))
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(stringValue(typed["type"])))
		if kind == "web_search" || kind == "web_fetch" || strings.Contains(kind, "web_search") {
			return true
		}
		if function, ok := typed["function"].(map[string]any); ok {
			return webtools.IsInternalTool(stringValue(function["name"]))
		}
		if name := stringValue(typed["name"]); name != "" {
			return webtools.IsInternalTool(name)
		}
	}
	return false
}

func (s *Server) executeWebCall(ctx context.Context, call map[string]any) (map[string]any, []map[string]any, string) {
	function := callFunction(call)
	name := stringValue(function["name"])
	arguments := function["arguments"]
	result := s.opts.WebTools.Execute(ctx, name, arguments)
	args := webtools.ArgsFromAny(arguments)
	item := map[string]any{
		"id": randomChatID("ws_"), "type": "web_search_call", "status": "completed",
	}
	if name == webtools.WebFetchName {
		item["action"] = map[string]any{"type": "open_url", "url": webtools.URLArg(args)}
	} else {
		item["action"] = map[string]any{"type": "search", "query": webtools.QueryArgs(args)}
	}
	sources := []map[string]any{}
	for _, source := range webtools.Sources(result) {
		sources = append(sources, map[string]any{"title": source.Title, "url": source.URL})
	}
	return item, sources, result
}

func callFunction(call map[string]any) map[string]any {
	function, _ := call["function"].(map[string]any)
	if function == nil {
		return map[string]any{}
	}
	return function
}

func metaTools(values []any) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if typed, ok := value.(map[string]any); ok {
			out = append(out, typed)
		}
	}
	return out
}

func responseMessages(chat map[string]any) []any {
	if messages, ok := chat["messages"].([]any); ok {
		return append([]any(nil), messages...)
	}
	return []any{}
}

func internalWebCalls(chat map[string]any) []map[string]any {
	message := assistantMessage(chat)
	if message == nil {
		return nil
	}
	toolCalls, _ := message["tool_calls"].([]any)
	out := []map[string]any{}
	for _, raw := range toolCalls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function := callFunction(call)
		if webtools.IsInternalTool(stringValue(function["name"])) {
			out = append(out, call)
		}
	}
	return out
}

func clientToolCalls(chat map[string]any) []map[string]any {
	message := assistantMessage(chat)
	if message == nil {
		return nil
	}
	toolCalls, _ := message["tool_calls"].([]any)
	out := []map[string]any{}
	for _, raw := range toolCalls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !webtools.IsInternalTool(stringValue(callFunction(call)["name"])) {
			out = append(out, call)
		}
	}
	return out
}

func stripInternalToolCalls(chat map[string]any) {
	message := assistantMessage(chat)
	if message == nil {
		return
	}
	toolCalls, _ := message["tool_calls"].([]any)
	kept := []any{}
	for _, raw := range toolCalls {
		call, ok := raw.(map[string]any)
		if !ok || !webtools.IsInternalTool(stringValue(callFunction(call)["name"])) {
			kept = append(kept, raw)
		}
	}
	if len(kept) == 0 {
		delete(message, "tool_calls")
		return
	}
	message["tool_calls"] = kept
}

func assistantMessage(chat map[string]any) map[string]any {
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return message
}

func attachWebItems(response map[string]any, items []any, sources []map[string]any) {
	if len(items) == 0 {
		return
	}
	output, _ := response["output"].([]any)
	output = append(output, items...)
	response["output"] = output
	if len(sources) > 0 {
		response["sources"] = sources
	}
}

// chatCompletionStream turns an aggregated chat completion into the minimal
// ChatStream shape so the existing streaming writers can emit one continuous
// response after local web rounds.
func chatCompletionStream(chat map[string]any) providers.ChatStream {
	frames := [][]byte{}
	message := assistantMessage(chat)
	finish := "stop"
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if value, ok := choice["finish_reason"].(string); ok && value != "" {
				finish = value
			}
		}
	}
	delta := map[string]any{}
	role := "assistant"
	delta["role"] = role
	if message != nil {
		if text, ok := message["content"].(string); ok && text != "" {
			delta["content"] = text
		}
		if toolCalls, ok := message["tool_calls"].([]any); ok && len(toolCalls) > 0 {
			delta["tool_calls"] = toolCalls
			if finish == "stop" {
				finish = "tool_calls"
			}
		}
	}
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}}
	if usage := chat["usage"]; usage != nil {
		chunk["usage"] = usage
	}
	raw, _ := json.Marshal(chunk)
	frames = append(frames, []byte("data: "+string(raw)+"\n\n"))
	finalChunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}}
	if usage := chat["usage"]; usage != nil {
		finalChunk["usage"] = usage
	}
	raw, _ = json.Marshal(finalChunk)
	frames = append(frames, []byte("data: "+string(raw)+"\n\n"))
	frames = append(frames, []byte("data: [DONE]\n\n"))
	return &memoryChatStream{frames: frames}
}

type memoryChatStream struct {
	frames [][]byte
	index  int
}

func (m *memoryChatStream) Next() ([]byte, error) {
	if m.index >= len(m.frames) {
		return nil, io.EOF
	}
	frame := m.frames[m.index]
	m.index++
	return frame, nil
}

func (m *memoryChatStream) Close() error { return nil }

var _ = bytes.NewReader
var _ = errors.Is
