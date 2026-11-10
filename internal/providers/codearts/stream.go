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
	"sync/atomic"
	"time"
)

const chatStreamID = "chatcmpl-codearts"

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

	created int64

	pending    []byte
	hasPending bool
	eof        bool
	terminal   bool
	finishSent bool
	lastText   string
	usage      map[string]any
	usageMu    sync.Mutex
	queue      [][]byte

	// closed is written by Close and read by Next; Close may race any caller
	// after [DONE]. idleTimedOut records the per-read SSE timeout so a
	// canceled body read maps to a classified upstream timeout rather than a
	// generic context error.
	closed       atomic.Bool
	idleTimedOut atomic.Bool

	// idleTimeout is v1's stream_timeout_seconds applied to each blocking
	// upstream line read. The timer is armed only while a read is blocked and
	// stopped as soon as the read returns.
	idleTimeout time.Duration
	idleMu      sync.Mutex
	idleTimer   *time.Timer

	closeOnce sync.Once
}

func newCodeArtsStream(
	parent context.Context,
	cancel context.CancelFunc,
	body io.ReadCloser,
	pool *Pool,
	account *Account,
	model string,
	idleTimeout time.Duration,
) *CodeArtsStream {
	return &CodeArtsStream{
		parent:      parent,
		cancel:      cancel,
		body:        body,
		reader:      bufio.NewReader(body),
		pool:        pool,
		account:     account,
		model:       model,
		created:     time.Now().Unix(),
		idleTimeout: idleTimeout,
	}
}

// currentAuth snapshots the live credential document. Token refresh may rotate
// the access token while a stream is open, so error redaction always reads the
// current value instead of a stale constructor snapshot.
func (s *CodeArtsStream) currentAuth() Auth {
	return currentAccountAuth(s.account)
}

// preflight reads the first data event so a 200 response carrying an embedded
// business error fails before the API layer writes headers. A clean EOF before
// any event is not an error: v1's iterator synthesizes the terminal finish,
// usage, and [DONE] frames exactly as it does for a mid-stream EOF.
func (s *CodeArtsStream) preflight() error {
	for {
		data, err := s.readData()
		if err != nil {
			if errors.Is(err, io.EOF) && !s.idleTimedOut.Load() {
				return nil
			}
			return s.mapReadError(err)
		}
		if isChatDoneData(data) {
			s.pending = data
			s.hasPending = true
			return nil
		}
		if embeddedErr := embeddedChatError(data, s.currentAuth()); embeddedErr != nil {
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
		if s.closed.Load() || s.terminal {
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
				if errors.Is(err, io.EOF) && !s.idleTimedOut.Load() {
					// v1 ends a clean EOF as a successful stream. Synthesize
					// the missing finish, optional usage, and [DONE].
					s.queueTerminal()
					_ = s.Close()
					continue
				}
				mapped := s.mapReadError(err)
				_ = s.Close()
				return nil, mapped
			}
			data = next
		}

		if isChatDoneData(data) {
			s.queueTerminal()
			_ = s.Close()
			continue
		}
		if embeddedErr := embeddedChatError(data, s.currentAuth()); embeddedErr != nil {
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
		s.usageMu.Lock()
		s.usage = usage
		s.usageMu.Unlock()
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
	s.usageMu.Lock()
	usage := s.usage
	s.usageMu.Unlock()
	return chatSSEFrame(map[string]any{
		"id":      chatStreamID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage":   usage,
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
	s.usageMu.Lock()
	hasUsage := len(s.usage) > 0
	s.usageMu.Unlock()
	if hasUsage {
		s.queue = append(s.queue, s.usageFrame())
	}
	s.queue = append(s.queue, []byte("data: [DONE]\n\n"))
}

// startIdle arms the per-read idle timer. Each new line read starts a fresh
// window, matching v1's socket timeout reset on every read.
func (s *CodeArtsStream) startIdle() {
	if s == nil || s.idleTimeout <= 0 {
		return
	}
	s.idleMu.Lock()
	s.stopIdleLocked()
	s.idleTimer = time.AfterFunc(s.idleTimeout, s.idleExpired)
	s.idleMu.Unlock()
}

func (s *CodeArtsStream) stopIdle() {
	if s == nil {
		return
	}
	s.idleMu.Lock()
	s.stopIdleLocked()
	s.idleMu.Unlock()
}

func (s *CodeArtsStream) stopIdleLocked() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// idleExpired records the timeout and cancels the upstream request context.
// The body read then returns; mapReadError turns that condition into a
// classified 502 timeout instead of a generic cancellation.
func (s *CodeArtsStream) idleExpired() {
	if s == nil {
		return
	}
	s.idleMu.Lock()
	if s.idleTimer == nil {
		// The read completed and stopped this window first.
		s.idleMu.Unlock()
		return
	}
	s.idleTimer = nil
	s.idleTimedOut.Store(true)
	s.idleMu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *CodeArtsStream) readLine() (string, error) {
	s.startIdle()
	line, err := s.reader.ReadString('\n')
	s.stopIdle()
	return line, err
}

// readData reads the next non-empty SSE data payload. Other fields and
// comments are skipped, matching v1's data-line handling.
func (s *CodeArtsStream) readData() ([]byte, error) {
	for {
		if s.eof {
			return nil, io.EOF
		}
		line, err := s.readLine()
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
		// Caller cancellation keeps its own classification even when an idle
		// timer or a concurrent Close raced it.
		if ctxErr := s.parent.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if s.closed.Load() {
		// An explicit/concurrent Close ends the caller-visible stream. Internal
		// close for a read error happens only after this classification.
		return io.EOF
	}
	if s.idleTimedOut.Load() {
		return newChatError(KindUpstream, http.StatusBadGateway,
			"codearts: stream idle timeout before the next SSE frame", s.currentAuth())
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return newChatError(KindUpstream, http.StatusBadGateway,
		"codearts: read chat stream failed: "+err.Error(), s.currentAuth())
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
		// Publish the closed state before stopping the timer: a timer that
		// fires in this window may cancel the request, but mapReadError sees
		// the explicit close and returns io.EOF.
		s.closed.Store(true)
		s.stopIdle()
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
// Normal chunks with a zero code are ignored. A top-level message without an
// error object or code is treated as benign: v1's _raise_embedded only reacts
// to error_code/code, and some gateways attach informational messages to
// otherwise normal chunks.
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
		} else if candidate := errorMap["type"]; chatStreamTruthy(candidate) {
			code = candidate
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

	errorish := chatStreamTruthy(errorValue) || !chatZeroCode(code)
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
	case isConcurrencyMessage(message) || isConcurrencyEmbeddedCode(normalizedCode):
		return newChatError(KindConcurrency, http.StatusBadRequest, message, auth)
	case isCreditExhaustedMessage(message) || isCreditEmbeddedCode(normalizedCode):
		return newChatError(KindCreditExhausted, http.StatusBadRequest, message, auth)
	case isEmbeddedBadRequest(message, normalizedCode):
		return newChatError(KindBadRequest, http.StatusBadRequest, message, auth)
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		isAuthEmbeddedCode(normalizedCode):
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			status = http.StatusUnauthorized
		}
		return newChatError(KindAuth, status, message, auth)
	case status == http.StatusTooManyRequests || isRateLimitedEmbeddedCode(normalizedCode):
		return newChatError(KindRateLimited, http.StatusTooManyRequests, message, auth)
	case status == http.StatusBadRequest:
		return newChatError(KindBadRequest, http.StatusBadRequest, message, auth)
	default:
		return newChatError(KindUpstream, status, message, auth)
	}
}

func isConcurrencyEmbeddedCode(code string) bool {
	if code == "" {
		return false
	}
	if code == "tm.00001041" || strings.Contains(code, "concurren") ||
		strings.Contains(code, "session_limit") {
		return true
	}
	return false
}

func isCreditEmbeddedCode(code string) bool {
	if code == "" || !strings.Contains(code, "credit") {
		return false
	}
	for _, marker := range []string{
		"exhaust", "insufficient", "empty", "no_credit", "zero", "not_enough",
	} {
		if strings.Contains(code, marker) {
			return true
		}
	}
	return false
}

func isAuthEmbeddedCode(code string) bool {
	if code == "" {
		return false
	}
	if code == "401" || code == "403" {
		return true
	}
	for _, marker := range []string{
		"auth", "unauthorized", "forbidden", "invalid_token", "token_expired",
	} {
		if strings.Contains(code, marker) {
			return true
		}
	}
	return false
}

func isRateLimitedEmbeddedCode(code string) bool {
	if code == "" {
		return false
	}
	if code == "429" {
		return true
	}
	for _, marker := range []string{
		"rate_limit", "rate limit", "ratelimit", "too_many_requests", "too many requests",
	} {
		if strings.Contains(code, marker) {
			return true
		}
	}
	return false
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
	if errorMap, ok := payload["error"].(map[string]any); ok && errorMap != nil {
		for _, key := range []string{"status", "status_code", "http_status"} {
			if value := chatEmbeddedInt(errorMap[key]); value >= 400 && value <= 599 {
				return value
			}
		}
		if value := chatEmbeddedInt(errorMap["code"]); value >= 400 && value <= 599 {
			return value
		}
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
