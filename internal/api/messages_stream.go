package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api/protocol"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type messagesToolState struct {
	index         int
	blockIndex    int
	id            string
	name          string
	opened        bool
	arguments     strings.Builder
	argumentsSeen bool
}

type messagesStreamState struct {
	id           string
	model        string
	started      bool
	stopReason   string
	thinkingOpen bool
	thinkingIdx  int
	textOpen     bool
	textIdx      int
	nextIndex    int
	tools        map[int]*messagesToolState
	toolOrder    []int
	usage        map[string]any
}

type messagesStreamResult struct {
	usage   map[string]any
	outcome string
	status  int
}

// writeMessagesStream emits the Anthropic Messages SSE sequence from a chat
// stream. The handler calls serveMessagesStream to keep the terminal outcome
// observable by the usage recorder seam.
func writeMessagesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string) {
	_ = runMessagesStream(w, r, stream, model, false)
}

// serveMessagesStream writes the Messages SSE form and records its terminal
// outcome. requestedModel is the client-visible ID; upstreamModel is the bare ID
// sent to the provider. forceModel pins the client-visible model to
// requestedModel so an upstream alias cannot leak into message_start.
func (s *Server) serveMessagesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, requestedModel, upstreamModel string, forceModel bool, effort string) {
	result := runMessagesStream(w, r, stream, requestedModel, forceModel)
	observation := ProtocolObservation{
		Protocol: "messages", Model: upstreamModel, RequestedModel: requestedModel, Effort: effort,
		Stream: true, Outcome: result.outcome, Status: result.status, Usage: result.usage,
		Error:   result.outcome == "failed" || result.outcome == "upstream_aborted",
		Message: "upstream stream error",
	}
	applyTracker(&observation, stream)
	s.observeProtocol(r, observation)
}

const messagesPostDoneTimeout = 5 * time.Second

var errMessagesPostDoneTimeout = errors.New("post-DONE drain timeout")

func runMessagesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string, forceModel bool) messagesStreamResult {
	writer := newSSEWriter(w)
	defer writer.Close()
	w.Header().Set("Connection", "close")
	// v1 commits 200 text/event-stream before reading the upstream. Keeping
	// that timing means an upstream failure after this point is an SSE error
	// event, never a second HTTP status line.
	writer.Start()

	state := &messagesStreamState{
		model:       model,
		stopReason:  "end_turn",
		thinkingIdx: -1,
		textIdx:     -1,
		tools:       map[int]*messagesToolState{},
	}
	reader := protocol.NewReader(stream)
	postDone := false
	var postDoneDeadline time.Time
	for {
		if clientCanceled(r) {
			return messagesStreamResult{usage: state.usage, outcome: "client_aborted", status: http.StatusOK}
		}
		var chunk protocol.Chunk
		var err error
		if postDone {
			chunk, err = readPostDoneChunk(reader, stream, postDoneDeadline)
		} else {
			chunk, err = reader.Next()
		}
		if err != nil {
			if errors.Is(err, errMessagesPostDoneTimeout) {
				break
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if r != nil && r.Context().Err() != nil || errors.Is(err, context.Canceled) {
				return messagesStreamResult{usage: state.usage, outcome: "client_aborted", status: http.StatusOK}
			}
			failure := providerFailure(err)
			message := failure.Message
			if message == "" {
				message = "upstream stream error"
			}
			state.emit(writer, "error", map[string]any{
				"error": map[string]any{"type": protocol.AnthropicErrorType(http.StatusBadGateway), "message": message},
			})
			return messagesStreamResult{usage: state.usage, outcome: "upstream_aborted", status: failure.Status}
		}
		if chunk.Done {
			postDone = true
			postDoneDeadline = time.Now().Add(messagesPostDoneTimeout)
			continue
		}
		if postDone {
			// Providers may emit a credit/usage accounting frame after
			// [DONE]. Keep reading until EOF, but never turn that frame into
			// client-visible content.
			if len(chunk.Usage) > 0 {
				state.usage = chunk.Usage
			}
			continue
		}
		if chunk.ID != "" {
			state.id = chunk.ID
		}
		if chunk.Model != "" && !forceModel {
			state.model = chunk.Model
		}
		if rawError, ok := chunk.Fields["error"].(map[string]any); ok {
			message := anthropicText(rawError["message"])
			if message == "" {
				message = "upstream error"
			}
			state.ensureStart(writer)
			state.emit(writer, "error", map[string]any{
				"error": map[string]any{"type": protocol.AnthropicErrorType(http.StatusBadGateway), "message": message},
			})
			return messagesStreamResult{usage: state.usage, outcome: "upstream_aborted", status: http.StatusBadGateway}
		}
		if len(chunk.Usage) > 0 {
			state.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if reasoning := delta.ReasoningContent; reasoning != "" {
				state.ensureStart(writer)
				state.ensureThinking(writer)
				state.emit(writer, "content_block_delta", map[string]any{
					"index": state.thinkingIdx,
					"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
				})
			}
			if text := delta.Content; text != "" {
				state.ensureStart(writer)
				state.ensureText(writer)
				state.emit(writer, "content_block_delta", map[string]any{
					"index": state.textIdx,
					"delta": map[string]any{"type": "text_delta", "text": text},
				})
			}
			for _, call := range delta.ToolCalls {
				state.ensureStart(writer)
				state.appendTool(writer, call)
			}
			if choice.FinishReason != "" {
				state.stopReason = protocol.AnthropicStopReason(choice.FinishReason)
			}
		}
	}

	state.ensureStart(writer)
	if state.thinkingOpen {
		state.thinkingOpen = false
		state.emit(writer, "content_block_stop", map[string]any{"index": state.thinkingIdx})
	}
	if state.textOpen {
		state.textOpen = false
		state.emit(writer, "content_block_stop", map[string]any{"index": state.textIdx})
	}
	order := append([]int(nil), state.toolOrder...)
	sort.Ints(order)
	for _, index := range order {
		entry := state.tools[index]
		if entry == nil || !entry.opened {
			continue
		}
		state.emit(writer, "content_block_stop", map[string]any{"index": entry.blockIndex})
	}
	state.emit(writer, "message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": state.stopReason, "stop_sequence": nil},
		"usage": protocol.AnthropicUsage(state.usage),
	})
	state.emit(writer, "message_stop", map[string]any{})
	return messagesStreamResult{usage: state.usage, outcome: "completed", status: http.StatusOK}
}

func readPostDoneChunk(reader *protocol.Reader, stream providers.ChatStream, deadline time.Time) (protocol.Chunk, error) {
	type result struct {
		chunk protocol.Chunk
		err   error
	}
	done := make(chan result, 1)
	go func() {
		chunk, err := reader.Next()
		done <- result{chunk: chunk, err: err}
	}()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		_ = stream.Close()
		return protocol.Chunk{}, errMessagesPostDoneTimeout
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case outcome := <-done:
		return outcome.chunk, outcome.err
	case <-timer.C:
		// Providers may never close after [DONE]. Close the stream so the
		// reader goroutine can finish, then finalize the client response with
		// the usage already observed.
		_ = stream.Close()
		return protocol.Chunk{}, errMessagesPostDoneTimeout
	}
}

func (s *messagesStreamState) emit(writer *sseWriter, eventType string, payload map[string]any) {
	body := map[string]any{"type": eventType}
	for key, value := range payload {
		body[key] = value
	}
	encoded, err := marshalJSON(body)
	if err != nil {
		return
	}
	_ = writer.Write([]byte("event: " + eventType + "\ndata: " + string(encoded) + "\n\n"))
}

func (s *messagesStreamState) ensureStart(writer *sseWriter) {
	if s.started {
		return
	}
	s.started = true
	s.emit(writer, "message_start", map[string]any{
		"message": map[string]any{
			"id":            anthropicMessageID(s.id),
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         s.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)},
		},
	})
}

func (s *messagesStreamState) ensureThinking(writer *sseWriter) {
	if s.thinkingOpen {
		return
	}
	s.thinkingIdx = s.nextIndex
	s.nextIndex++
	s.thinkingOpen = true
	s.emit(writer, "content_block_start", map[string]any{
		"index":         s.thinkingIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

func (s *messagesStreamState) ensureText(writer *sseWriter) {
	if s.textOpen {
		return
	}
	if s.thinkingOpen {
		s.thinkingOpen = false
		s.emit(writer, "content_block_stop", map[string]any{"index": s.thinkingIdx})
	}
	s.textIdx = s.nextIndex
	s.nextIndex++
	s.textOpen = true
	s.emit(writer, "content_block_start", map[string]any{
		"index":         s.textIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *messagesStreamState) appendTool(writer *sseWriter, call protocol.ToolCall) {
	index := call.Index
	if index < 0 {
		index = len(s.tools)
	}
	entry := s.tools[index]
	if entry == nil {
		entry = &messagesToolState{index: index}
		s.tools[index] = entry
		s.toolOrder = append(s.toolOrder, index)
	}
	if call.ID != "" {
		entry.id = call.ID
	}
	if call.Name != "" {
		entry.name = call.Name
	}
	if call.Arguments != "" {
		entry.arguments.WriteString(call.Arguments)
		entry.argumentsSeen = true
	}
	if !entry.opened && (entry.id != "" || entry.name != "" || entry.argumentsSeen) {
		if s.thinkingOpen {
			s.thinkingOpen = false
			s.emit(writer, "content_block_stop", map[string]any{"index": s.thinkingIdx})
		}
		if s.textOpen {
			s.textOpen = false
			s.emit(writer, "content_block_stop", map[string]any{"index": s.textIdx})
		}
		entry.opened = true
		entry.blockIndex = s.nextIndex
		s.nextIndex++
		if entry.id == "" {
			entry.id = "toolu_" + strconv.Itoa(index)
		}
		s.emit(writer, "content_block_start", map[string]any{
			"index": entry.blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    entry.id,
				"name":  entry.name,
				"input": map[string]any{},
			},
		})
	}
	if entry.opened && call.Arguments != "" {
		s.emit(writer, "content_block_delta", map[string]any{
			"index": entry.blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
		})
	}
}
