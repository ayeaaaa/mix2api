package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// handleChatWithWebTools is the v1 non-stream chat path: declared local web
// tools are executed by the gateway and their results fed into another round.
func (s *Server) handleChatWithWebTools(
	w http.ResponseWriter,
	r *http.Request,
	provider providers.Provider,
	base providers.ChatRequest,
	request *chatHTTPRequest,
) bool {
	tools := decodeToolsRaw(request.payload["tools"])
	wants := webtools.WantsFromTools(tools)
	if !wants.Search && !wants.Fetch {
		return false
	}
	chat := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(mustMarshalRawMap(request.payload)))
	decoder.UseNumber()
	if decoder.Decode(&chat) != nil {
		return false
	}
	chat["tools"] = webtools.InstallToolDefs(tools, wants)
	messages, _ := chat["messages"].([]any)
	var final map[string]any
	for round := 0; round < webtools.MaxRounds(); round++ {
		raw, err := chatMapToRaw(chat)
		if err != nil {
			writeFailure(w, localFailure(http.StatusBadRequest, "invalid request"))
			return true
		}
		roundRequest := base
		roundRequest.Raw = raw
		roundRequest.Messages = chatMessagesRaw(chat)
		stream, openErr := provider.OpenChat(r.Context(), roundRequest)
		if openErr != nil {
			if stream != nil {
				_ = stream.Close()
			}
			writeFailure(w, openErr)
			return true
		}
		completion, aggregateErr := aggregateChatStream(stream, request.model)
		_ = stream.Close()
		if aggregateErr != nil {
			writeFailure(w, aggregateErr)
			return true
		}
		calls := internalWebCalls(completion)
		if len(calls) == 0 {
			final = completion
			break
		}
		message := assistantMessage(completion)
		if message == nil {
			final = completion
			break
		}
		messages = append(messages, message)
		for _, call := range calls {
			function, _ := call["function"].(map[string]any)
			name := stringValue(function["name"])
			result := s.opts.WebTools.Execute(r.Context(), name, function["arguments"])
			callID := stringValue(call["id"])
			if callID == "" {
				callID = randomChatID("call_")
			}
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": callID, "name": name, "content": result,
			})
		}
		chat["messages"] = messages
	}
	if final == nil {
		final = map[string]any{"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "Web search round limit reached."},
			"finish_reason": "stop",
		}}}
	}
	writeJSONResponse(w, http.StatusOK, final)
	return true
}

func decodeToolsRaw(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var values []any
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return metaTools(values)
}

func mustMarshalRawMap(payload map[string]json.RawMessage) []byte {
	raw, _ := json.Marshal(payload)
	return raw
}
