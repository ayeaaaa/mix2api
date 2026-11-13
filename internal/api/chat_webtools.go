package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// runChatWebRounds executes local web tools for non-stream and stream chat.
func (s *Server) runChatWebRounds(
	ctx context.Context,
	provider providers.Provider,
	base providers.ChatRequest,
	request *chatHTTPRequest,
) (map[string]any, bool, error) {
	tools := decodeToolsRaw(request.payload["tools"])
	wants := webtools.WantsFromTools(tools)
	if !wants.Search && !wants.Fetch {
		return nil, false, nil
	}
	chat := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(mustMarshalRawMap(request.payload)))
	decoder.UseNumber()
	if decoder.Decode(&chat) != nil {
		return nil, false, localFailure(http.StatusBadRequest, "invalid request")
	}
	chat["tools"] = webtools.InstallToolDefs(tools, wants)
	messages, _ := chat["messages"].([]any)
	maxRounds := webtools.MaxRounds()
	var final map[string]any
	for round := 0; round <= maxRounds; round++ {
		dropTools := round == maxRounds
		if dropTools {
			dropInternalWebTools(chat)
			messages = append(messages, map[string]any{
				"role": "system",
				"content": "The web tools are no longer available. Answer the user now with " +
					"what you already have. Do not say that you are searching again.",
			})
			chat["messages"] = messages
		}
		raw, err := chatMapToRaw(chat)
		if err != nil {
			return nil, true, localFailure(http.StatusBadRequest, "invalid request")
		}
		roundRequest := base
		roundRequest.Raw = raw
		roundRequest.Messages = chatMessagesRaw(chat)
		stream, openErr := provider.OpenChat(ctx, roundRequest)
		if openErr != nil {
			if stream != nil {
				_ = stream.Close()
			}
			return nil, true, openErr
		}
		completion, aggregateErr := aggregateChatStream(stream, request.model)
		_ = stream.Close()
		if aggregateErr != nil {
			return nil, true, aggregateErr
		}
		if dropTools {
			stripInternalToolCalls(completion)
			final = completion
			break
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
			_, _, result := s.executeWebCall(ctx, call)
			callID := stringValue(call["id"])
			if callID == "" {
				callID = randomChatID("call_")
			}
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": callID,
				"name": stringValue(callFunction(call)["name"]), "content": result,
			})
		}
		chat["messages"] = messages
		if len(clientToolCalls(completion)) > 0 {
			stripInternalToolCalls(completion)
			final = completion
			break
		}
	}
	if final == nil {
		final = map[string]any{"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "Web search round limit reached."},
			"finish_reason": "stop",
		}}}
	}
	// Provider rounds speak the upstream ID; only a qualified request forces the
	// canonical ID into the client-visible completion. Bare requests keep the
	// upstream echo.
	if request.exit != "" && request.requestedModel != "" {
		final["model"] = request.requestedModel
	}
	return final, true, nil
}

func (s *Server) handleChatWithWebTools(
	w http.ResponseWriter,
	r *http.Request,
	provider providers.Provider,
	base providers.ChatRequest,
	request *chatHTTPRequest,
	started time.Time,
) bool {
	final, handled, err := s.runChatWebRounds(r.Context(), provider, base, request)
	if !handled {
		return false
	}
	if err != nil {
		failure := failureFromError(err)
		s.observeProtocol(r, ProtocolObservation{
			Protocol: "chat", Model: request.model, RequestedModel: request.requestedModel,
			Realm: provider.Realm(), Kind: "chat", Effort: chatReasoningEffort(request.payload),
			Stream: false, Outcome: "failed",
			Status: failure.Status, Error: true, Message: failure.Message,
		})
		writeFailure(w, err)
		return true
	}
	usage, _ := final["usage"].(map[string]any)
	s.observeProtocol(r, ProtocolObservation{
		Protocol: "chat", Model: request.model, RequestedModel: request.requestedModel,
		Realm: provider.Realm(), Kind: "chat", Effort: chatReasoningEffort(request.payload),
		Stream: false, Outcome: "completed", Status: http.StatusOK,
		Usage: usage, ElapsedMS: time.Since(started).Milliseconds(),
	})
	writeJSONResponse(w, http.StatusOK, final)
	return true
}

func (s *Server) handleChatWithWebToolsStream(
	w http.ResponseWriter,
	r *http.Request,
	provider providers.Provider,
	base providers.ChatRequest,
	request *chatHTTPRequest,
	started time.Time,
) bool {
	final, handled, err := s.runChatWebRounds(r.Context(), provider, base, request)
	if !handled {
		return false
	}
	if err != nil {
		failure := failureFromError(err)
		s.observeProtocol(r, ProtocolObservation{
			Protocol: "chat", Model: request.model, RequestedModel: request.requestedModel,
			Realm: provider.Realm(), Kind: "chat", Effort: chatReasoningEffort(request.payload),
			Stream: true, Outcome: "failed",
			Status: failure.Status, Error: true, Message: failure.Message,
		})
		writeFailure(w, err)
		return true
	}
	stream := newUsageTracker(chatCompletionStream(final), started)
	defer func() { _ = stream.Close() }()
	result := writeChatStreamFor(w, r, stream, request.requestedModel, request.exit != "")
	usage, _ := final["usage"].(map[string]any)
	observation := ProtocolObservation{
		Protocol: "chat", Model: request.model, RequestedModel: request.requestedModel,
		Realm: provider.Realm(), Kind: "chat", Effort: chatReasoningEffort(request.payload),
		Stream: true, Outcome: result.Outcome, Status: result.Status,
		Usage: usage, Error: result.Error, Message: result.Message,
	}
	applyTracker(&observation, stream)
	s.observeProtocol(r, observation)
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
