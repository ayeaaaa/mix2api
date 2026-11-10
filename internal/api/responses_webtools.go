package api

import (
	"net/http"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// serveResponsesWithWebTools executes local web tool calls and feeds the
// results back into additional chat rounds. It returns false when the client
// did not declare either local tool.
func (s *Server) serveResponsesWithWebTools(
	w http.ResponseWriter,
	r *http.Request,
	provider providers.Provider,
	chatRequest providers.ChatRequest,
	translated *responsesTranslation,
	model string,
) bool {
	wants := webtools.WantsFromTools(metaTools(translated.Meta.Tools))
	if !wants.Search && !wants.Fetch {
		return false
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
	rounds := webtools.MaxRounds()
	for round := 0; round < rounds; round++ {
		raw, err := chatMapToRaw(translated.Chat)
		if err != nil {
			writeFailure(w, localFailure(http.StatusBadRequest, "invalid request"))
			return true
		}
		request := chatRequest
		request.Raw = raw
		request.Messages = chatMessagesRaw(translated.Chat)
		upstream, openErr := provider.OpenChat(r.Context(), request)
		if openErr != nil {
			if upstream != nil {
				_ = upstream.Close()
			}
			s.observeResponseFailure(model, false, providerFailure(openErr).Status)
			writeFailure(w, openErr)
			return true
		}
		chat, aggregateErr := aggregateChatStream(upstream, model)
		_ = upstream.Close()
		if aggregateErr != nil {
			s.observeResponseFailure(model, false, providerFailure(aggregateErr).Status)
			writeFailure(w, aggregateErr)
			return true
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
			function, _ := call["function"].(map[string]any)
			name := stringValue(function["name"])
			arguments := function["arguments"]
			result := s.opts.WebTools.Execute(r.Context(), name, arguments)
			callID := stringValue(call["id"])
			if callID == "" {
				callID = randomChatID("call_")
			}
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": callID, "name": name, "content": result,
			})
			args := webtools.ArgsFromAny(arguments)
			item := map[string]any{
				"id": randomChatID("ws_"), "type": "web_search_call", "status": "completed",
			}
			if name == webtools.WebFetchName {
				item["action"] = map[string]any{"type": "open_url", "url": webtools.URLArg(args)}
			} else {
				item["action"] = map[string]any{"type": "search", "query": webtools.QueryArgs(args)}
			}
			for _, source := range webtools.Sources(result) {
				sources = append(sources, map[string]any{"title": source.Title, "url": source.URL})
			}
			webItems = append(webItems, item)
		}
		translated.Chat["messages"] = messages
	}
	if final == nil {
		// Round cap reached while the model still asked for tools: return the
		// last assistant text rather than leaking another internal call.
		final = map[string]any{"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "Web search round limit reached."},
			"finish_reason": "stop",
		}}}
	}
	translated.Meta.WebItems = webItems
	translated.Meta.Sources = sources
	response := chatToResponse(final, model, translated.Meta)
	attachWebItems(response, webItems, sources)
	s.observeProtocol(ProtocolObservation{
		Protocol: "responses", Model: model, Stream: false, Outcome: "completed", Status: http.StatusOK,
		Usage: usageMap(final["usage"]),
	})
	writeJSONResponse(w, http.StatusOK, response)
	return true
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
		function, _ := call["function"].(map[string]any)
		if webtools.IsInternalTool(stringValue(function["name"])) {
			out = append(out, call)
		}
	}
	return out
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
