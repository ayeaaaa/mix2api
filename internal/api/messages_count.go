package api

import (
	"net/http"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api/protocol"
)

// estimateMessagesInputTokens mirrors v1 _anthropic_estimate_chat_tokens. It
// uses the same CJK-aware estimator as gateway accounting over serialized
// message content, tool calls, and tool definitions. It is intentionally a
// local estimate, not an official model tokenizer.
func estimateMessagesInputTokens(chat map[string]any) int {
	total := 0
	if chat == nil {
		return total
	}
	messages, _ := chat["messages"].([]any)
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if value := protocol.JSONText(message["content"]); value != "" {
			total += estimateTokens(value)
		}
		toolCalls, _ := message["tool_calls"].([]any)
		for _, rawCall := range toolCalls {
			if call, ok := rawCall.(map[string]any); ok {
				total += estimateTokens(protocol.JSONText(call))
			}
		}
	}
	tools, _ := chat["tools"].([]any)
	for _, rawTool := range tools {
		total += estimateTokens(protocol.JSONText(rawTool))
	}
	return total
}

func (s *Server) handleMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", time.Time{})
		return
	}
	if !s.anthropicMessagesEnabled() {
		writeAnthropicError(w, http.StatusBadRequest, anthropicDisabledMessage, "invalid_request_error", time.Time{})
		return
	}
	payload, failure := s.parseMessagesPayload(w, r)
	if failure != nil {
		writeAnthropicError(w, failure.Status, failure.Message, protocol.AnthropicErrorType(failure.Status), time.Time{})
		return
	}
	chat, err := messagesToChatWithDefault(payload, s.opts.DefaultMaxOutputTokens)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", time.Time{})
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"input_tokens": estimateMessagesInputTokens(chat)})
}
