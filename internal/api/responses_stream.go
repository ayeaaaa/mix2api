package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api/protocol"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type responsesEmitter struct {
	writer *sseWriter
	seq    int
}

func newResponsesEmitter(writer *sseWriter) *responsesEmitter {
	return &responsesEmitter{writer: writer}
}

func (e *responsesEmitter) emit(eventType string, payload map[string]any) {
	e.seq++
	body := map[string]any{"type": eventType, "sequence_number": e.seq}
	for key, value := range payload {
		body[key] = value
	}
	encoded, err := marshalJSON(body)
	if err != nil {
		return
	}
	_ = e.writer.Write([]byte("event: " + eventType + "\ndata: " + string(encoded) + "\n\n"))
}

type responsesToolCall struct {
	index       int
	outputIndex int
	itemID      string
	callID      string
	name        string
	arguments   string
	custom      bool
}

type responsesStreamState struct {
	responseID       string
	msgID            string
	reasonID         string
	model            string
	meta             responseMeta
	created          int64
	finish           string
	usage            map[string]any
	sawDone          bool
	droppedTruncated bool

	outputs     []any
	reasonIndex int
	msgIndex    int
	reason      strings.Builder
	text        strings.Builder
	finalText   string
	filter      responsesTextFilter
	tools       map[int]*responsesToolCall
	toolOrder   []int
	dsmlCalls   []dsmlCall
}

type responsesStreamResult struct {
	usage   map[string]any
	outcome string
	status  int
}

// writeResponsesStream emits the Responses SSE form of a provider chat stream.
// The handler uses serveResponsesStream so it can observe the terminal outcome;
// this function keeps the public translator seam from the task contract.
func writeResponsesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string, meta responseMeta) {
	_ = runResponsesStream(w, r, stream, model, meta)
}

// serveResponsesStream emits a provider chat stream as Responses SSE events.
// model is the client-visible requested ID (canonical for qualified requests);
// upstreamModel is what the provider actually served and what observations
// record for pricing.
func (s *Server) serveResponsesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model, upstreamModel string, meta responseMeta) {
	result := runResponsesStream(w, r, stream, model, meta)
	observation := ProtocolObservation{
		Protocol:       "responses",
		Model:          upstreamModel,
		RequestedModel: model,
		Effort:         meta.Effort,
		Stream:         true,
		Outcome:        result.outcome,
		Status:         result.status,
		Usage:          result.usage,
		Error:          result.outcome == "failed" || result.outcome == "upstream_aborted",
		Message:        "upstream stream error",
	}
	applyTracker(&observation, stream)
	s.observeProtocol(r, observation)
}

func runResponsesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string, meta responseMeta) responsesStreamResult {
	writer := newSSEWriter(w)
	defer writer.Close()
	w.Header().Set("Connection", "close")
	writer.Start()
	emitter := newResponsesEmitter(writer)
	state := &responsesStreamState{
		responseID:  randomChatID("resp_"),
		msgID:       randomChatID("msg_"),
		reasonID:    randomChatID("rs_"),
		model:       model,
		meta:        meta,
		created:     time.Now().Unix(),
		reasonIndex: -1,
		msgIndex:    -1,
		tools:       map[int]*responsesToolCall{},
	}

	emitter.emit("response.created", map[string]any{"response": state.responseObject("in_progress")})
	emitter.emit("response.in_progress", map[string]any{"response": state.responseObject("in_progress")})
	for _, raw := range meta.WebItems {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := len(state.outputs)
		state.outputs = append(state.outputs, item)
		added := cloneStringMap(item)
		added["status"] = "in_progress"
		emitter.emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		emitter.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}

	reader := protocol.NewReader(stream)
	for {
		if clientCanceled(r) {
			return responsesStreamResult{usage: state.usage, outcome: "client_aborted", status: http.StatusOK}
		}
		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if r != nil && r.Context().Err() != nil {
				return responsesStreamResult{usage: state.usage, outcome: "client_aborted", status: http.StatusOK}
			}
			if errors.Is(err, context.Canceled) {
				return responsesStreamResult{usage: state.usage, outcome: "client_aborted", status: http.StatusOK}
			}
			state.emitFailed(emitter, err)
			return responsesStreamResult{usage: state.usage, outcome: "upstream_aborted", status: providerFailure(err).Status}
		}
		if chunk.Done {
			state.sawDone = true
			continue
		}
		if rawError, ok := chunk.Fields["error"].(map[string]any); ok {
			message := stringValue(rawError["message"])
			if message == "" {
				message = "upstream stream error"
			}
			state.emitFailedMessage(emitter, message)
			return responsesStreamResult{usage: state.usage, outcome: "upstream_aborted", status: http.StatusBadGateway}
		}
		if len(chunk.Usage) > 0 {
			state.captureUsage(chunk.Usage)
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if reasoning := delta.ReasoningContent; reasoning != "" {
				state.appendReasoning(emitter, reasoning)
			}
			for _, call := range delta.ToolCalls {
				state.appendTool(emitter, call)
			}
			if piece := delta.Content; piece != "" {
				state.appendText(emitter, piece)
			}
			if choice.FinishReason != "" {
				state.finish = choice.FinishReason
			}
		}
	}

	state.dropTruncatedTools()
	outcome, status := state.finalize(emitter)
	return responsesStreamResult{usage: state.usage, outcome: outcome, status: status}
}

func (s *responsesStreamState) outputItems() []any {
	items := make([]any, 0, len(s.outputs))
	for _, item := range s.outputs {
		if item != nil {
			items = append(items, item)
		}
	}
	return items
}

func (s *responsesStreamState) responseObject(status string) map[string]any {
	text := s.currentText()
	output := s.outputItems()
	output = applyNamespaceToCalls(output, s.meta.NamespaceMap)
	response := map[string]any{
		"id":                  s.responseID,
		"object":              "response",
		"created_at":          s.created,
		"status":              status,
		"model":               s.model,
		"output":              output,
		"output_text":         text,
		"metadata":            map[string]any{},
		"parallel_tool_calls": s.meta.ParallelToolCalls,
		"tool_choice":         s.meta.ToolChoice,
		"tools":               nonNilSlice(s.meta.Tools),
	}
	if usage := responsesUsage(s.usage); usage != nil {
		response["usage"] = usage
	}
	if len(s.meta.Sources) > 0 {
		response["sources"] = s.meta.Sources
	}
	return response
}

func cloneStringMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func (s *responsesStreamState) currentText() string {
	if s.finalText != "" {
		return s.finalText
	}
	return s.text.String()
}

func (s *responsesStreamState) captureUsage(usage map[string]any) {
	if len(usage) == 0 {
		return
	}
	currentTotal := int64(0)
	if s.usage != nil {
		currentTotal, _ = protocol.Int64(s.usage["total_tokens"])
	}
	candidateTotal, _ := protocol.Int64(usage["total_tokens"])
	if s.usage == nil || candidateTotal >= currentTotal {
		copied := make(map[string]any, len(usage))
		for key, value := range usage {
			copied[key] = value
		}
		s.usage = copied
	}
}

func (s *responsesStreamState) reasoningItem(status string) map[string]any {
	return map[string]any{
		"id":     s.reasonID,
		"type":   "reasoning",
		"status": status,
		"summary": []any{map[string]any{
			"type": "summary_text",
			"text": s.reason.String(),
		}},
	}
}

func (s *responsesStreamState) appendReasoning(emitter *responsesEmitter, piece string) {
	if s.reasonIndex < 0 {
		s.reasonIndex = len(s.outputs)
		s.outputs = append(s.outputs, nil)
		emitter.emit("response.output_item.added", map[string]any{
			"output_index": s.reasonIndex,
			"item":         s.reasoningItem("in_progress"),
		})
		emitter.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	s.reason.WriteString(piece)
	emitter.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex,
		"summary_index": 0, "delta": piece,
	})
}

func (s *responsesStreamState) closeReasoning(emitter *responsesEmitter) {
	if s.reasonIndex < 0 || s.outputs[s.reasonIndex] != nil {
		return
	}
	full := s.reason.String()
	emitter.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0, "text": full,
	})
	emitter.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": full},
	})
	s.outputs[s.reasonIndex] = s.reasoningItem("completed")
	emitter.emit("response.output_item.done", map[string]any{
		"output_index": s.reasonIndex,
		"item":         s.outputs[s.reasonIndex],
	})
}

func (s *responsesStreamState) appendTool(emitter *responsesEmitter, call protocol.ToolCall) {
	index := call.Index
	if index < 0 {
		index = len(s.tools)
	}
	entry := s.tools[index]
	if entry == nil {
		callID := strings.TrimSpace(call.ID)
		if callID == "" {
			callID = randomChatID("call_")
		}
		custom := s.meta.CustomToolNames[call.Name]
		prefix := "fc_"
		if custom {
			prefix = "ctc_"
		}
		entry = &responsesToolCall{
			index:       index,
			outputIndex: len(s.outputs),
			itemID:      randomChatID(prefix),
			callID:      callID,
			name:        call.Name,
			arguments:   call.Arguments,
			custom:      custom,
		}
		s.tools[index] = entry
		s.toolOrder = append(s.toolOrder, index)
		s.outputs = append(s.outputs, nil)
		item := map[string]any{
			"id":      entry.itemID,
			"status":  "in_progress",
			"call_id": entry.callID,
			"name":    entry.name,
		}
		if entry.custom {
			item["type"] = "custom_tool_call"
			item["input"] = ""
		} else {
			item["type"] = "function_call"
			item["arguments"] = ""
		}
		stampNamespace(item, s.meta.NamespaceMap)
		emitter.emit("response.output_item.added", map[string]any{
			"output_index": entry.outputIndex,
			"item":         item,
		})
	} else {
		if call.Name != "" && entry.name == "" {
			entry.name = call.Name
			if s.meta.CustomToolNames[call.Name] {
				entry.custom = true
			}
		}
		if call.Arguments != "" {
			entry.arguments += call.Arguments
		}
	}
	if call.Arguments == "" {
		return
	}
	if entry.custom {
		emitter.emit("response.custom_tool_call_input.delta", map[string]any{
			"output_index": entry.outputIndex, "item_id": entry.itemID,
			"call_id": entry.callID, "delta": call.Arguments,
		})
		return
	}
	emitter.emit("response.function_call_arguments.delta", map[string]any{
		"output_index": entry.outputIndex, "item_id": entry.itemID,
		"call_id": entry.callID, "delta": call.Arguments,
	})
}

func (s *responsesStreamState) appendText(emitter *responsesEmitter, piece string) {
	if s.msgIndex < 0 {
		s.closeReasoning(emitter)
		s.msgIndex = len(s.outputs)
		s.outputs = append(s.outputs, nil)
		emitter.emit("response.output_item.added", map[string]any{
			"output_index": s.msgIndex,
			"item": map[string]any{
				"id": s.msgID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		emitter.emit("response.content_part.added", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	clean, calls := s.filter.Feed(piece)
	if len(calls) > 0 {
		s.dsmlCalls = append(s.dsmlCalls, calls...)
	}
	if clean == "" {
		return
	}
	s.text.WriteString(clean)
	emitter.emit("response.output_text.delta", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex,
		"content_index": 0, "delta": clean,
	})
}

func (s *responsesStreamState) dropTruncatedTools() {
	if len(s.tools) == 0 {
		return
	}
	if s.finish != "length" && s.sawDone {
		return
	}
	for index, entry := range s.tools {
		if entry == nil {
			continue
		}
		if !validToolArguments(entry.arguments) {
			s.droppedTruncated = true
			delete(s.tools, index)
			if entry.outputIndex >= 0 && entry.outputIndex < len(s.outputs) {
				s.outputs[entry.outputIndex] = nil
			}
		}
	}
}

func (s *responsesStreamState) finalize(emitter *responsesEmitter) (string, int) {
	clean, calls := s.filter.Flush()
	if len(calls) > 0 {
		s.dsmlCalls = append(s.dsmlCalls, calls...)
	}
	if clean != "" {
		s.text.WriteString(clean)
		if s.msgIndex >= 0 {
			emitter.emit("response.output_text.delta", map[string]any{
				"item_id": s.msgID, "output_index": s.msgIndex,
				"content_index": 0, "delta": clean,
			})
		}
	}
	s.closeReasoning(emitter)

	ordered := append([]int(nil), s.toolOrder...)
	sort.Ints(ordered)
	hasStructuredCalls := false
	for _, index := range ordered {
		entry := s.tools[index]
		if entry == nil {
			continue
		}
		hasStructuredCalls = true
		var item map[string]any
		if entry.custom {
			input := unwrapCustomInput(entry.arguments)
			emitter.emit("response.custom_tool_call_input.done", map[string]any{
				"output_index": entry.outputIndex, "item_id": entry.itemID,
				"call_id": entry.callID, "input": input,
			})
			item = map[string]any{
				"id": entry.itemID, "type": "custom_tool_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "input": input,
			}
		} else {
			emitter.emit("response.function_call_arguments.done", map[string]any{
				"output_index": entry.outputIndex, "item_id": entry.itemID,
				"call_id": entry.callID, "arguments": entry.arguments,
			})
			item = map[string]any{
				"id": entry.itemID, "type": "function_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "arguments": entry.arguments,
			}
		}
		stampNamespace(item, s.meta.NamespaceMap)
		s.outputs[entry.outputIndex] = item
		emitter.emit("response.output_item.done", map[string]any{
			"output_index": entry.outputIndex,
			"item":         item,
		})
	}

	fullText := s.text.String()
	if !hasStructuredCalls && len(s.dsmlCalls) == 0 {
		if parsed, cleaned := parseDSMLToolCalls(fullText); len(parsed) > 0 {
			s.dsmlCalls = parsed
			s.finalText = cleaned
			fullText = cleaned
		}
	}
	if !hasStructuredCalls && len(s.dsmlCalls) > 0 {
		for _, call := range s.dsmlCalls {
			outputIndex := len(s.outputs)
			itemID := randomChatID("fc_")
			item := map[string]any{
				"id": itemID, "type": "function_call", "status": "completed",
				"call_id": call.id, "name": call.name, "arguments": call.arguments,
			}
			stampNamespace(item, s.meta.NamespaceMap)
			s.outputs = append(s.outputs, item)
			emitter.emit("response.output_item.added", map[string]any{
				"output_index": outputIndex,
				"item": map[string]any{
					"id": itemID, "type": "function_call", "status": "in_progress",
					"call_id": call.id, "name": call.name, "arguments": "",
				},
			})
			emitter.emit("response.function_call_arguments.delta", map[string]any{
				"output_index": outputIndex, "item_id": itemID,
				"call_id": call.id, "delta": call.arguments,
			})
			emitter.emit("response.function_call_arguments.done", map[string]any{
				"output_index": outputIndex, "item_id": itemID,
				"call_id": call.id, "arguments": call.arguments,
			})
			emitter.emit("response.output_item.done", map[string]any{
				"output_index": outputIndex,
				"item":         item,
			})
		}
	}

	hasOther := false
	for _, item := range s.outputs {
		if item != nil {
			hasOther = true
			break
		}
	}
	if s.msgIndex >= 0 || fullText != "" || !hasOther {
		if s.msgIndex < 0 {
			s.msgIndex = len(s.outputs)
			s.outputs = append(s.outputs, nil)
			emitter.emit("response.output_item.added", map[string]any{
				"output_index": s.msgIndex,
				"item": map[string]any{
					"id": s.msgID, "type": "message", "status": "in_progress",
					"role": "assistant", "content": []any{},
				},
			})
			emitter.emit("response.content_part.added", map[string]any{
				"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
		}
		if s.finalText == "" {
			s.finalText = fullText
		}
		annotations := buildCitations(fullText, s.meta.Sources)
		part := map[string]any{"type": "output_text", "text": fullText, "annotations": annotations}
		emitter.emit("response.output_text.done", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0, "text": fullText,
		})
		emitter.emit("response.content_part.done", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0, "part": part,
		})
		content := []any{}
		if fullText != "" {
			content = append(content, part)
		}
		s.outputs[s.msgIndex] = map[string]any{
			"id": s.msgID, "type": "message", "status": "completed",
			"role": "assistant", "content": content,
		}
		emitter.emit("response.output_item.done", map[string]any{
			"output_index": s.msgIndex,
			"item":         s.outputs[s.msgIndex],
		})
	}

	if s.usage == nil || usageTotal(s.usage) == 0 {
		if estimated := estimateResponsesUsage(fullText, s.reason.String(), s.meta.InputText); estimated != nil {
			s.usage = estimated
		}
	}
	emptyReasoning := strings.TrimSpace(fullText) == "" && strings.TrimSpace(s.reason.String()) != "" &&
		!hasStructuredCalls && len(s.dsmlCalls) == 0 && s.finish != "length" && !s.droppedTruncated
	status := "completed"
	if s.finish == "length" || s.droppedTruncated {
		status = "incomplete"
	}
	response := s.responseObject(status)
	if s.finish == "length" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if emptyReasoning {
		response = s.responseObject("failed")
		response["error"] = map[string]any{
			"code":    "empty_reasoning_only",
			"message": "upstream returned reasoning without output text; retry",
		}
		emitter.emit("response.failed", map[string]any{"response": response})
		return "failed", http.StatusBadGateway
	}
	emitter.emit("response.completed", map[string]any{"response": response})
	return "completed", http.StatusOK
}

func (s *responsesStreamState) emitFailed(emitter *responsesEmitter, err error) {
	message := providerFailure(err).Message
	if message == "" {
		message = "upstream stream error"
	}
	s.emitFailedMessage(emitter, message)
}

func (s *responsesStreamState) emitFailedMessage(emitter *responsesEmitter, message string) {
	response := s.responseObject("failed")
	response["error"] = map[string]any{"code": "upstream_stream_error", "message": message}
	emitter.emit("response.failed", map[string]any{"response": response})
}

func estimateResponsesUsage(text, reasoning, input string) map[string]any {
	completion := estimateTokens(text) + estimateTokens(reasoning)
	if completion <= 0 {
		return nil
	}
	prompt := estimateTokens(input)
	if prompt < 1 {
		prompt = 1
	}
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": estimateTokens(reasoning),
		},
		"prompt_tokens_details": map[string]any{"cached_tokens": 0},
	}
}

// responsesTextFilter holds back text that might be a printed DSML tool call.
type responsesTextFilter struct {
	buffer string
}

func (f *responsesTextFilter) Feed(piece string) (string, []dsmlCall) {
	if piece == "" {
		return "", nil
	}
	f.buffer += piece
	var emitted strings.Builder
	var calls []dsmlCall
	for f.buffer != "" {
		index := strings.Index(f.buffer, "<")
		if index < 0 {
			emitted.WriteString(f.buffer)
			f.buffer = ""
			break
		}
		if match := dsmlCallsRE.FindStringIndex(f.buffer); match != nil && match[0] == index {
			emitted.WriteString(f.buffer[:index])
			parsed, _ := parseDSMLToolCalls(f.buffer[index:match[1]])
			calls = append(calls, parsed...)
			f.buffer = f.buffer[match[1]:]
			continue
		}
		candidate := f.buffer[index:]
		if len(candidate) > 30 {
			candidate = candidate[:30]
		}
		isCandidate := strings.Contains(candidate, "DSML") ||
			(len(candidate) < 10 && !strings.ContainsAny(candidate, " \t\n>"))
		if isCandidate {
			emitted.WriteString(f.buffer[:index])
			f.buffer = f.buffer[index:]
			break
		}
		next := strings.Index(f.buffer[index+1:], "<")
		if next >= 0 {
			flush := index + 1 + next
			emitted.WriteString(f.buffer[:flush])
			f.buffer = f.buffer[flush:]
			continue
		}
		emitted.WriteString(f.buffer)
		f.buffer = ""
	}
	return emitted.String(), calls
}

func clientCanceled(r *http.Request) bool {
	if r == nil {
		return false
	}
	select {
	case <-r.Context().Done():
		return true
	default:
		return false
	}
}

func (f *responsesTextFilter) Flush() (string, []dsmlCall) {
	if f.buffer == "" {
		return "", nil
	}
	if calls, clean := parseDSMLToolCalls(f.buffer); len(calls) > 0 {
		f.buffer = ""
		return clean, calls
	}
	buffer := f.buffer
	f.buffer = ""
	return buffer, nil
}
