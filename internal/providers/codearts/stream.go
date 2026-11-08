package codearts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	chatStreamID           = "chatcmpl-codearts"
	chatStreamErrorMessage = "codearts: stream ended before the [DONE] event"
)

// CodeArtsStream normalizes Huawei CodeArts SSE events into OpenAI-style
// complete SSE frames. It owns one acquired account slot and releases it
// exactly once, either on Close, on upstream [DONE], or on EOF.
type CodeArtsStream struct {
	parent context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	reader *bufio.Reader

	pool    *Pool
	account *Account
	model   string
	auth    Auth
	secrets []string

	created int64

	pending    []byte
	hasPending bool
	eof        bool
	terminal   bool
	closed     bool
	finishSent bool
	lastText   string
	usage      map[string]any
	queue      [][]byte

	closeOnce sync.Once
}

func newCodeArtsStream(
	parent context.Context,
	cancel context.CancelFunc,
	body io.ReadCloser,
	pool *Pool,
	account *Account,
	model string,
	auth Auth,
) *CodeArtsStream {
	return &CodeArtsStream{
		parent:  parent,
		cancel:  cancel,
		body:    body,
		reader:  bufio.NewReader(body),
		pool:    pool,
		account: account,
		model:   model,
		auth:    auth,
		secrets: codeartsSecretValues(auth),
		created: time.Now().Unix(),
	}
}

// preflight reads the first data event so a 200 response carrying an embedded
// business error fails before the API layer writes headers. A completely empty
// event stream is an upstream failure.
func (s *CodeArtsStream) preflight() error {
	for {
		data, err := s.readData()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return &APIError{Kind: KindUpstream, Status: http.StatusBadGateway,
					Message: chatStreamErrorMessage}
			}
			return s.mapReadError(err)
		}
		if isChatDoneData(data) {
			s.pending = data
			s.hasPending = true
			return nil
		}
		if embeddedErr := embeddedChatError(data, s.auth); embeddedErr != nil {
			return embeddedErr
		}
		s.pending = data
		s.hasPending = true
		return nil
	}
}

// Next returns one complete SSE frame. Upstream events are normalized into
// standard chunks with one synthetic finish, an optional usage frame, and a
// single terminal [DONE].
func (s *CodeArtsStream) Next() ([]byte, error) {
	if s == nil {
		return nil, io.EOF
	}
	for {
		if len(s.queue) > 0 {
			frame := s.queue[0]
			s.queue = s.queue[1:]
			return frame, nil
		}
		if s.closed || s.terminal {
			return nil, io.EOF
		}
		if s.parent != nil {
			if ctxErr := s.parent.Err(); ctxErr != nil {
				_ = s.Close()
				return nil, ctxErr
			}
		}

		var data []byte
		if s.hasPending {
			data = s.pending
			s.pending = nil
			s.hasPending = false
		} else {
			next, err := s.readData()
			if err != nil {
				if errors.Is(err, io.EOF) {
					_ = s.Close()
					return nil, &APIError{Kind: KindUpstream, Status: http.StatusBadGateway,
						Message: chatStreamErrorMessage}
				}
				_ = s.Close()
				return nil, s.mapReadError(err)
			}
			data = next
		}

		if isChatDoneData(data) {
			s.queueTerminal()
			_ = s.Close()
			continue
		}
		if embeddedErr := embeddedChatError(data, s.auth); embeddedErr != nil {
			_ = s.Close()
			return nil, embeddedErr
		}
		s.handleEvent(data)
		if s.terminal {
			// Text-snapshot "[DONE]" and embedded terminal events close the
			// account slot immediately, but queued frames are still served.
			_ = s.Close()
		}
	}
}

// handleEvent queues zero or more normalized frames for one upstream data
// payload: standard choices/delta, top-level delta, text snapshots, output
// snapshots, and usage-only chunks.
func (s *CodeArtsStream) handleEvent(data []byte) {
	if len(bytes.TrimSpace(data)) == 0 {
		return
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return
	}

	if usage, ok := payload["usage"].(map[string]any); ok && len(usage) > 0 {
		s.usage = usage
	}

	if choices := chatList(payload["choices"]); len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			choice = map[string]any{}
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			if message, ok := choice["message"].(map[string]any); ok && message != nil {
				delta = map[string]any{"role": firstChatText(message["role"], "assistant")}
				if content, exists := message["content"]; exists {
					delta["content"] = content
				}
				if calls := chatList(message["tool_calls"]); len(calls) > 0 {
					delta["tool_calls"] = calls
				}
			} else {
				delta = map[string]any{}
			}
		}
		finish := choice["finish_reason"]
		if finish != nil && !chatZeroValue(finish) {
			if s.finishSent {
				finish = nil
			} else {
				s.finishSent = true
			}
		}
		index := chatEmbeddedInt(choice["index"])
		if index < 0 {
			index = 0
		}
		s.queue = append(s.queue, s.chunkAt(index, delta, finish))
		return
	}

	if delta, ok := payload["delta"].(map[string]any); ok && delta != nil {
		s.queue = append(s.queue, s.chunk(delta, nil))
		return
	}

	if textValue, exists := payload["text"]; exists {
		text := modelText(textValue)
		trimmed := strings.TrimSpace(text)
		if trimmed == "[DONE]" || strings.EqualFold(trimmed, "done") {
			s.queueTerminal()
			return
		}
		if delta := s.textDelta(text); delta != "" {
			s.queue = append(s.queue, s.chunk(map[string]any{"content": delta}, nil))
		}
		return
	}

	if output := chatList(payload["output"]); len(output) > 0 {
		var text strings.Builder
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item == nil || modelText(item["type"]) != "output_text" {
				continue
			}
			text.WriteString(modelText(item["text"]))
		}
		if delta := s.textDelta(text.String()); delta != "" {
			s.queue = append(s.queue, s.chunk(map[string]any{"content": delta}, nil))
		}
	}
}

func (s *CodeArtsStream) textDelta(full string) string {
	if full == s.lastText {
		return ""
	}
	delta := full
	if strings.HasPrefix(full, s.lastText) {
		delta = full[len(s.lastText):]
	}
	s.lastText = full
	return delta
}

func (s *CodeArtsStream) chunk(delta map[string]any, finish any) []byte {
	return s.chunkAt(0, delta, finish)
}

func (s *CodeArtsStream) chunkAt(index int, delta map[string]any, finish any) []byte {
	if delta == nil {
		delta = map[string]any{}
	}
	payload := map[string]any{
		"id":      chatStreamID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         index,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	return chatSSEFrame(payload)
}

func (s *CodeArtsStream) usageFrame() []byte {
	return chatSSEFrame(map[string]any{
		"id":      chatStreamID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage":   s.usage,
	})
}

func chatSSEFrame(payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	frame := make([]byte, 0, len(encoded)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, encoded...)
	frame = append(frame, "\n\n"...)
	return frame
}

// queueTerminal adds exactly one finish chunk when the upstream did not send
// one, the optional usage frame, and one [DONE].
func (s *CodeArtsStream) queueTerminal() {
	if s.terminal {
		return
	}
	s.terminal = true
	if !s.finishSent {
		s.queue = append(s.queue, s.chunk(map[string]any{}, "stop"))
		s.finishSent = true
	}
	if len(s.usage) > 0 {
		s.queue = append(s.queue, s.usageFrame())
	}
	s.queue = append(s.queue, []byte("data: [DONE]\n\n"))
}

// readData reads the next non-empty SSE data payload. Other fields and
// comments are skipped, matching v1's data-line handling.
func (s *CodeArtsStream) readData() ([]byte, error) {
	for {
		if s.eof {
			return nil, io.EOF
		}
		line, err := s.reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				data := strings.TrimSpace(trimmed[len("data:"):])
				if data != "" {
					if err != nil {
						s.eof = true
					}
					return []byte(data), nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.eof = true
				return nil, io.EOF
			}
			return nil, err
		}
	}
}

func (s *CodeArtsStream) mapReadError(err error) error {
	if s.parent != nil {
		if ctxErr := s.parent.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if s.closed {
		return io.EOF
	}
	return newChatError(KindUpstream, http.StatusBadGateway,
		"codearts: read chat stream failed: "+err.Error(), s.auth)
}

// abort closes the transport without releasing the account slot. It is used
// before a stream is returned; the caller still owns the slot.
func (s *CodeArtsStream) abort() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.body != nil {
		_ = s.body.Close()
	}
}

// Close closes the upstream body, cancels the request, and releases the
// account exactly once. It is safe after io.EOF and idempotent.
func (s *CodeArtsStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closed = true
		s.abort()
		if s.pool != nil {
			s.pool.Release(s.account)
		}
	})
	return nil
}

func isChatDoneData(data []byte) bool {
	return string(bytes.TrimSpace(data)) == "[DONE]"
}

func firstChatText(value any, fallback string) string {
	if text := strings.TrimSpace(modelText(value)); text != "" {
		return text
	}
	return fallback
}

// embeddedChatError maps an in-band SSE error object to the chat error kinds.
// Normal chunks with a zero code are ignored.
func embeddedChatError(data []byte, auth Auth) *APIError {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "[DONE]" {
		return nil
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil
	}

	errorValue := payload["error"]
	errorMap, _ := errorValue.(map[string]any)
	code := payload["error_code"]
	if chatZeroCode(code) {
		code = payload["code"]
	}
	if chatZeroCode(code) && errorMap != nil {
		if candidate := errorMap["code"]; chatStreamTruthy(candidate) {
			code = candidate
		} else {
			code = errorMap["type"]
		}
	}

	message := ""
	if errorMap != nil {
		for _, key := range []string{"message", "error_msg", "detail"} {
			if value := strings.TrimSpace(modelText(errorMap[key])); value != "" {
				message = value
				break
			}
		}
	} else if value, ok := errorValue.(string); ok {
		message = strings.TrimSpace(value)
	}
	if message == "" {
		for _, key := range []string{"error_msg", "message", "detail", "error_description"} {
			if value := strings.TrimSpace(modelText(payload[key])); value != "" {
				message = value
				break
			}
		}
	}

	hasNormal := chatStreamTruthy(payload["choices"]) ||
		payload["usage"] != nil || payload["delta"] != nil ||
		payload["text"] != nil || payload["output"] != nil
	errorish := chatStreamTruthy(errorValue) || !chatZeroCode(code) ||
		(message != "" && !hasNormal)
	if !errorish {
		return nil
	}
	if message == "" {
		if chatZeroCode(code) {
			return nil
		}
		message = "error_code=" + modelText(code)
	}

	status := embeddedChatStatus(payload)
	normalizedCode := strings.ToLower(strings.TrimSpace(modelText(code)))
	switch {
	case isConcurrencyMessage(message):
		return newChatError(KindConcurrency, http.StatusBadRequest, message, auth)
	case isCreditExhaustedMessage(message):
		return newChatError(KindCreditExhausted, http.StatusBadRequest, message, auth)
	case isEmbeddedBadRequest(message, normalizedCode):
		return newChatError(KindBadRequest, http.StatusBadRequest, message, auth)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return newChatError(KindAuth, status, message, auth)
	case status == http.StatusTooManyRequests || normalizedCode == "429":
		return newChatError(KindRateLimited, http.StatusTooManyRequests, message, auth)
	case status == http.StatusBadRequest:
		return newChatError(KindBadRequest, http.StatusBadRequest, message, auth)
	default:
		return newChatError(KindUpstream, status, message, auth)
	}
}

func isEmbeddedBadRequest(message, code string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"request param is invalid",
		"request model is not multimodal",
		"tool function cannot empty",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	switch code {
	case "400", "bad_request", "invalid_request", "invalid_request_error",
		"invalid_param", "invalid_parameter":
		return true
	default:
		return false
	}
}

func embeddedChatStatus(payload map[string]any) int {
	for _, key := range []string{"status", "status_code", "http_status"} {
		if value := chatEmbeddedInt(payload[key]); value >= 400 && value <= 599 {
			return value
		}
	}
	code := payload["error_code"]
	if chatZeroCode(code) {
		code = payload["code"]
	}
	if value := chatEmbeddedInt(code); value >= 400 && value <= 599 {
		return value
	}
	return 0
}

func chatEmbeddedInt(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case bool:
		if typed {
			return 1
		}
		return 0
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return int(number)
		}
		if number, err := typed.Float64(); err == nil {
			return int(number)
		}
	case string:
		if number, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return number
		}
	}
	return 0
}

func chatZeroCode(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		text := strings.TrimSpace(typed)
		return text == "" || text == "0"
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number == 0
	case float64:
		return typed == 0
	case float32:
		return typed == 0
	case int:
		return typed == 0
	case int64:
		return typed == 0
	default:
		return false
	}
}

func chatZeroValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		return strings.TrimSpace(typed) == "" || strings.TrimSpace(typed) == "0"
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number == 0
	case float64:
		return typed == 0
	default:
		return false
	}
}

func chatStreamTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number != 0
	case float64:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}
