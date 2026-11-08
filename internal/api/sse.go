package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// sseKeepaliveInterval is a variable so tests can shorten it. Production keeps
// v1's 15-second heartbeat cadence.
var sseKeepaliveInterval = 15 * time.Second

const postDoneReadIdleTimeout = 45 * time.Second

// sseWriter serializes SSE frame writes and heartbeat comments. A dedicated
// lock is required because the heartbeat goroutine writes while the request
// goroutine blocks in ChatStream.Next; without it a comment can split a frame.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher

	mu      sync.Mutex
	started bool
	closed  bool
	lastAt  time.Time
	stop    chan struct{}
	done    chan struct{}
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	return &sseWriter{
		w:       w,
		flusher: flusher,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (s *sseWriter) WroteHeaders() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *sseWriter) Write(frame []byte) error {
	if s == nil || len(frame) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
		s.lastAt = time.Now()
		go s.heartbeat()
	}
	if _, err := s.w.Write(frame); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	s.lastAt = time.Now()
	return nil
}

func (s *sseWriter) heartbeat() {
	interval := sseKeepaliveInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	tick := interval / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			if time.Since(s.lastAt) < interval {
				s.mu.Unlock()
				continue
			}
			_, err := s.w.Write([]byte(": keepalive\n\n"))
			if err == nil && s.flusher != nil {
				s.flusher.Flush()
			}
			s.lastAt = time.Now()
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (s *sseWriter) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	wasStarted := s.started
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stop)
	s.mu.Unlock()
	if wasStarted {
		<-s.done
	}
}

// streamNormalizer turns arbitrary provider chat chunks into one OpenAI-style
// stream: a single gateway id, deferred complete tool calls, one synthetic
// finish when the upstream omitted it, and usage hoisted out of choice-bearing
// chunks into a trailing choices-less frame before [DONE]. Post-DONE frames are
// forwarded by the caller so Raccoon's credit accounting frame is preserved.
type streamNormalizer struct {
	id      string
	model   string
	created int64

	usage     map[string]any
	usageSeen bool
	finish    string
	finishSet bool
	sawChoice bool
	sawAny    bool

	tools   map[int]*chatToolCall
	content strings.Builder
	reason  strings.Builder
}

func newStreamNormalizer(model string) *streamNormalizer {
	return &streamNormalizer{
		id:    randomChatID("chatcmpl-wb-"),
		model: strings.TrimSpace(model),
		tools: make(map[int]*chatToolCall),
	}
}

func (n *streamNormalizer) absorb(frame []byte) [][]byte {
	if n == nil {
		return nil
	}
	data := ssePayload(frame)
	if strings.TrimSpace(data) == "" {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(data)))
	decoder.UseNumber()
	var chunk map[string]any
	if err := decoder.Decode(&chunk); err != nil || chunk == nil {
		n.sawAny = true
		return [][]byte{frame}
	}
	n.sawAny = true

	if model, ok := chunk["model"].(string); ok && strings.TrimSpace(model) != "" {
		n.model = strings.TrimSpace(model)
	}
	if created, ok := int64Value(chunk["created"]); ok && created > 0 && n.created == 0 {
		n.created = created
	}

	usageSeenHere := false
	if usage, ok := chunk["usage"].(map[string]any); ok && len(usage) > 0 {
		usageSeenHere = true
		n.usageSeen = true
		n.usage = betterUsage(n.usage, usage)
		delete(chunk, "usage")
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) > 0 {
		n.sawChoice = true
	}
	meaningful := false
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			meaningful = true
			continue
		}
		deltaMeaningful := false
		finishPresent := false
		delta, _ := choice["delta"].(map[string]any)
		if delta != nil {
			if piece, ok := delta["content"].(string); ok && piece != "" {
				n.content.WriteString(piece)
			}
			if piece, ok := delta["reasoning_content"].(string); ok && piece != "" {
				n.reason.WriteString(piece)
			}
			n.absorbToolCalls(delta["tool_calls"])
			delete(delta, "tool_calls")
			if functionCall, ok := delta["function_call"].(map[string]any); ok {
				n.absorbLegacyFunctionCall(functionCall)
				delete(delta, "function_call")
			}
			deltaMeaningful = len(delta) > 0
		}
		if finish, ok := choice["finish_reason"].(string); ok && strings.TrimSpace(finish) != "" {
			n.finish = finish
			n.finishSet = true
			finishPresent = true
		}
		choice["finish_reason"] = nil
		if deltaMeaningful || finishPresent {
			meaningful = true
		}
	}
	if len(choices) == 0 && usageSeenHere {
		return nil
	}
	if len(choices) > 0 && !meaningful {
		return nil
	}

	chunk["id"] = n.id
	if _, ok := chunk["object"]; !ok && len(choices) > 0 {
		chunk["object"] = "chat.completion.chunk"
	}
	if _, ok := chunk["model"]; !ok && n.model != "" {
		chunk["model"] = n.model
	}
	encoded, err := marshalJSON(chunk)
	if err != nil {
		return [][]byte{frame}
	}
	return [][]byte{[]byte("data: " + string(encoded) + "\n\n")}
}

func betterUsage(current, candidate map[string]any) map[string]any {
	if current == nil {
		return cloneUsage(candidate)
	}
	currentTotal, _ := jsonNumberValue(current["total_tokens"])
	candidateTotal, _ := jsonNumberValue(candidate["total_tokens"])
	if candidateTotal >= currentTotal {
		return cloneUsage(candidate)
	}
	return current
}

func jsonNumberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

func cloneUsage(usage map[string]any) map[string]any {
	copied := make(map[string]any, len(usage))
	for key, value := range usage {
		copied[key] = value
	}
	return copied
}

func (n *streamNormalizer) absorbToolCalls(value any) {
	calls, ok := value.([]any)
	if !ok {
		return
	}
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		index, ok := intValue(call["index"])
		if !ok {
			index = len(n.tools)
		}
		entry := n.tools[index]
		if entry == nil {
			entry = &chatToolCall{}
			n.tools[index] = entry
		}
		if id, ok := call["id"].(string); ok && strings.TrimSpace(id) != "" {
			entry.id = strings.TrimSpace(id)
		}
		if kind, ok := call["type"].(string); ok && strings.TrimSpace(kind) != "" {
			entry.kind = strings.TrimSpace(kind)
		}
		if function, ok := call["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				entry.name += name
			}
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				entry.arguments += arguments
			}
		}
	}
}

func (n *streamNormalizer) absorbLegacyFunctionCall(function map[string]any) {
	if n == nil || function == nil {
		return
	}
	name, _ := function["name"].(string)
	arguments, _ := function["arguments"].(string)
	if strings.TrimSpace(name) == "" && strings.TrimSpace(arguments) == "" {
		return
	}
	entry := n.tools[0]
	if entry == nil {
		entry = &chatToolCall{}
		n.tools[0] = entry
	}
	if strings.TrimSpace(name) != "" && entry.name == "" {
		entry.name = strings.TrimSpace(name)
	}
	entry.arguments += arguments
}

// finalize returns the deferred tool-call frames, a synthesized/held finish
// frame, and the trailing usage frame. The caller appends [DONE] afterwards.
func (n *streamNormalizer) finalize() [][]byte {
	if n == nil {
		return nil
	}
	if !n.sawAny && len(n.tools) == 0 && !n.usageSeen && !n.finishSet {
		return nil
	}

	out := make([][]byte, 0, len(n.tools)+2)
	indexes := make([]int, 0, len(n.tools))
	for index := range n.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	kept := 0
	for _, index := range indexes {
		call := n.tools[index]
		if call == nil || strings.TrimSpace(call.name) == "" {
			continue
		}
		if !validToolArguments(call.arguments) {
			continue
		}
		id := call.id
		if id == "" {
			id = randomChatID("call_")
		}
		kind := call.kind
		if kind == "" {
			kind = "function"
		}
		frame := n.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index,
			"id":    id,
			"type":  kind,
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.arguments,
			},
		}}}, nil)
		out = append(out, frame)
		kept++
	}

	finish := n.finish
	if kept > 0 {
		if finish == "" || finish == "stop" {
			finish = "tool_calls"
		}
	} else if finish == "tool_calls" {
		finish = "stop"
	}
	if finish == "" {
		finish = "stop"
	}
	if n.sawChoice || n.finishSet || len(n.tools) > 0 {
		out = append(out, n.chunk(map[string]any{}, finish))
	}

	usage := n.usage
	if usage == nil || usageTotal(usage) == 0 {
		if estimated := estimateChatUsage(n.content.String(), n.reason.String()); estimated != nil {
			usage = estimated
		}
	}
	if usage != nil {
		out = append(out, n.usageFrame(usage))
	}
	return out
}

func validToolArguments(arguments string) bool {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return true
	}
	return json.Valid([]byte(trimmed))
}

func usageTotal(usage map[string]any) int64 {
	if usage == nil {
		return 0
	}
	value, ok := jsonNumberValue(usage["total_tokens"])
	if !ok {
		return 0
	}
	return int64(value)
}

func (n *streamNormalizer) createdValue() int64 {
	if n.created > 0 {
		return n.created
	}
	return time.Now().Unix()
}

func (n *streamNormalizer) chunk(delta map[string]any, finish any) []byte {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finish != nil {
		choice["finish_reason"] = finish
	}
	payload := map[string]any{
		"id":      n.id,
		"object":  "chat.completion.chunk",
		"created": n.createdValue(),
		"model":   n.model,
		"choices": []any{choice},
	}
	encoded, err := marshalJSON(payload)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(encoded) + "\n\n")
}

func (n *streamNormalizer) usageFrame(usage map[string]any) []byte {
	payload := map[string]any{
		"id":      n.id,
		"object":  "chat.completion.chunk",
		"created": n.createdValue(),
		"model":   n.model,
		"choices": []any{},
		"usage":   cloneUsage(usage),
	}
	encoded, err := marshalJSON(payload)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(encoded) + "\n\n")
}

// rewritePostDoneFrame rewrites the gateway stream id on post-[DONE] provider
// frames. Choice-less usage/credit frames keep every other field verbatim.
func (n *streamNormalizer) rewritePostDoneFrame(frame []byte) []byte {
	if n == nil || len(frame) == 0 {
		return frame
	}
	data := ssePayload(frame)
	if strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	var chunk map[string]any
	if err := decoder.Decode(&chunk); err != nil || chunk == nil {
		return frame
	}
	if _, ok := chunk["id"]; ok {
		chunk["id"] = n.id
	}
	encoded, err := marshalJSON(chunk)
	if err != nil {
		return frame
	}
	return []byte("data: " + string(encoded) + "\n\n")
}

// estimateTokens mirrors v1's prompt/response estimator: CJK counts one token
// each and other text is divided by 3.6, with a one-token floor for non-empty
// ASCII text.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk := 0
	for _, r := range text {
		if (r >= 0x4e00 && r <= 0x9fff) || (r >= 0x3400 && r <= 0x4dbf) {
			cjk++
		}
	}
	other := utf8.RuneCountInString(text) - cjk
	tokens := cjk
	if other > 0 {
		tokens += maxInt(1, int(float64(other)/3.6))
	}
	return tokens
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func estimateChatUsage(content, reasoning string) map[string]any {
	completion := estimateTokens(content) + estimateTokens(reasoning)
	if completion <= 0 {
		return nil
	}
	prompt := completion / 2
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
