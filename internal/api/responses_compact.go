package api

import (
	"net/http"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const compactionPrompt = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff " +
	"summary for another LLM that will resume the task.\n\n" +
	"Include:\n" +
	"- Current progress and key decisions made\n" +
	"- Important context, constraints, or user preferences\n" +
	"- What remains to be done (clear next steps)\n" +
	"- Any critical data, examples, or references needed to continue\n\n" +
	"Be concise, structured, and focused on helping the next LLM seamlessly " +
	"continue the work. Return only the summary."

// writeResponsesCompaction answers both the legacy /responses/compact path and
// Codex's compaction_trigger turn with exactly one compaction output item. The
// upstream is a Chat Completions API, so the gateway asks for the handoff
// summary itself and stores it in the item's opaque encrypted_content field.
func (s *Server) writeResponsesCompaction(w http.ResponseWriter, upstream providers.ChatStream, model string, meta responseMeta) {
	chat, err := aggregateChatStream(upstream, model)
	if err != nil {
		s.observeProtocol(ProtocolObservation{
			Protocol: "responses", Model: model, Stream: meta.Streaming, Outcome: "failed",
			Status: providerFailure(err).Status,
		})
		writeFailure(w, err)
		return
	}
	summary := chatMessageText(chat)
	if summary == "" {
		s.observeProtocol(ProtocolObservation{
			Protocol: "responses", Model: model, Stream: meta.Compaction, Outcome: "failed",
			Status: http.StatusBadGateway, Usage: usageMap(chat["usage"]),
		})
		writeFailure(w, &apiFailure{
			Status:  http.StatusBadGateway,
			Type:    "server_error",
			Message: "compaction produced no summary (the model returned no text); retry",
		})
		return
	}
	response := responsesCompactionResponse(chat, model)
	if meta.Streaming {
		s.writeCompactionStream(w, response)
	} else {
		writeJSONResponse(w, http.StatusOK, response)
	}
	s.observeProtocol(ProtocolObservation{
		Protocol: "responses", Model: model, Stream: meta.Streaming,
		Outcome: "completed", Status: http.StatusOK, Usage: usageMap(chat["usage"]),
	})
}

func (s *Server) writeCompactionStream(w http.ResponseWriter, response map[string]any) {
	item, _ := response["output"].([]any)
	var compactionItem any
	if len(item) > 0 {
		compactionItem = item[0]
	}
	base := map[string]any{
		"id":          response["id"],
		"object":      "response",
		"created_at":  response["created_at"],
		"model":       response["model"],
		"output":      []any{compactionItem},
		"output_text": "",
		"metadata":    map[string]any{},
	}
	writer := newSSEWriter(w)
	defer writer.Close()
	w.Header().Set("Connection", "close")
	writer.Start()
	emitter := newResponsesEmitter(writer)
	emitter.emit("response.created", map[string]any{
		"response": withResponseStatus(base, "in_progress", []any{}),
	})
	emitter.emit("response.in_progress", map[string]any{
		"response": withResponseStatus(base, "in_progress", []any{}),
	})
	// Codex's remote-compaction collector only reads output_item.done; an
	// `added` frame for this item would hand it an empty summary.
	emitter.emit("response.output_item.done", map[string]any{
		"output_index": 0,
		"item":         compactionItem,
	})
	completed := withResponseStatus(base, "completed", []any{compactionItem})
	if usage, ok := response["usage"].(map[string]any); ok && len(usage) > 0 {
		completed["usage"] = usage
	} else {
		completed["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	emitter.emit("response.completed", map[string]any{"response": completed})
}

func withResponseStatus(base map[string]any, status string, output []any) map[string]any {
	response := make(map[string]any, len(base)+1)
	for key, value := range base {
		response[key] = value
	}
	response["status"] = status
	if output == nil {
		output = []any{}
	}
	response["output"] = output
	return response
}
