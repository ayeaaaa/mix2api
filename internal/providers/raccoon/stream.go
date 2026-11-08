package raccoon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// chatStream passes Raccoon SSE payloads through as complete frames. It caches
// the first data event during preflight so an HTTP 200 carrying an embedded
// error fails before the API layer writes headers, and it releases the account
// slot exactly once.
type chatStream struct {
	parent context.Context
	ctx    context.Context
	cancel context.CancelFunc

	body   io.ReadCloser
	reader *bufio.Reader

	pool    *Pool
	account *Account
	secrets []string

	pending     []byte
	pendingData []byte
	preflighted bool
	sawDone     bool

	closeOnce sync.Once
	closeErr  error
}

func newChatStream(
	parent context.Context,
	ctx context.Context,
	cancel context.CancelFunc,
	body io.ReadCloser,
	pool *Pool,
	account *Account,
	secrets []string,
) *chatStream {
	return &chatStream{
		parent:  parent,
		ctx:     ctx,
		cancel:  cancel,
		body:    body,
		reader:  bufio.NewReaderSize(body, 64*1024),
		pool:    pool,
		account: account,
		secrets: append([]string(nil), secrets...),
	}
}

// preflight reads the first data event. A normal chat/usage chunk is cached
// for Next; an embedded error object is returned before any headers reach the
// client. EOF before any event is deferred to Next, matching v1.
func (s *chatStream) preflight() error {
	if s == nil || s.preflighted {
		return nil
	}
	s.preflighted = true
	for {
		frame, data, err := s.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return s.mapReadError(err)
		}
		if err := embeddedStreamError(data, s.secrets); err != nil {
			return err
		}
		s.pending = frame
		s.pendingData = data
		return nil
	}
}

// Next returns one complete SSE frame, including its trailing blank line.
// [DONE] is yielded once; the following call returns io.EOF and releases the
// account. A stream that ends without [DONE] reports an upstream error.
func (s *chatStream) Next() ([]byte, error) {
	if s == nil {
		return nil, io.EOF
	}
	if s.sawDone {
		_ = s.Close()
		return nil, io.EOF
	}
	if s.pending != nil {
		frame := s.pending
		data := s.pendingData
		s.pending = nil
		s.pendingData = nil
		if isDoneEvent(data) {
			s.sawDone = true
		}
		return frame, nil
	}

	frame, data, err := s.readFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			_ = s.Close()
			if !s.sawDone {
				return nil, &APIError{
					Kind:    KindUpstream,
					Message: "raccoon: stream ended before the [DONE] event",
				}
			}
			return nil, io.EOF
		}
		_ = s.Close()
		return nil, s.mapReadError(err)
	}

	if isDoneEvent(data) {
		s.sawDone = true
		return frame, nil
	}
	if err := embeddedStreamError(data, s.secrets); err != nil {
		_ = s.Close()
		return nil, err
	}
	return frame, nil
}

// Close closes the upstream body, cancels the stream timeout, and releases the
// account exactly once. It is safe to call after io.EOF and concurrently.
func (s *chatStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.body != nil {
			s.closeErr = s.body.Close()
		}
		if s.pool != nil {
			s.pool.Release(s.account)
		}
	})
	return s.closeErr
}

// abort closes the transport without releasing the slot. The caller is still
// the slot owner (used when preflight fails before a stream is returned).
func (s *chatStream) abort() {
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

type sseLine struct {
	raw     []byte
	payload []byte
}

// readFrame reads one SSE event and returns its complete frame plus the joined
// data payload used for error detection. Comments and other non-data fields
// are skipped, matching v1's data-line pass-through.
func (s *chatStream) readFrame() ([]byte, []byte, error) {
	lines := make([]sseLine, 0, 1)
	for {
		line, err := s.reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				if len(lines) > 0 {
					frame, data := buildSSEFrame(lines)
					return frame, data, nil
				}
			case bytes.HasPrefix(trimmed, []byte("data:")):
				payload := trimmed[len("data:"):]
				if len(payload) > 0 && payload[0] == ' ' {
					payload = payload[1:]
				}
				if len(bytes.TrimSpace(payload)) == 0 {
					continue
				}
				lines = append(lines, sseLine{
					raw:     append([]byte(nil), trimmed...),
					payload: append([]byte(nil), payload...),
				})
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(lines) > 0 {
				frame, data := buildSSEFrame(lines)
				return frame, data, nil
			}
			return nil, nil, err
		}
	}
}

func buildSSEFrame(lines []sseLine) ([]byte, []byte) {
	var frame bytes.Buffer
	var data bytes.Buffer
	for index, line := range lines {
		if index > 0 {
			frame.WriteByte('\n')
			data.WriteByte('\n')
		}
		frame.Write(line.raw)
		data.Write(line.payload)
	}
	frame.WriteString("\n\n")
	return frame.Bytes(), data.Bytes()
}

func isDoneEvent(data []byte) bool {
	return string(bytes.TrimSpace(data)) == "[DONE]"
}

// mapReadError preserves caller cancellation and reports a stream-scoped
// timeout separately from a broken upstream. All messages are redacted.
func (s *chatStream) mapReadError(err error) error {
	if s.parent != nil {
		if ctxErr := s.parent.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if s.ctx != nil {
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return &APIError{
					Kind:    KindTransient,
					Message: "raccoon: chat stream timed out",
				}
			}
			return ctxErr
		}
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return &APIError{
		Kind:    KindTransient,
		Message: redactText("raccoon: read chat stream failed: "+err.Error(), s.secrets...),
	}
}

// embeddedStreamError maps v1's `_raise_embedded` logic. A frame is only an
// error when it carries an `error` value/message or a non-zero
// `error_code`/`code`; normal choices/usage chunks are ignored.
func embeddedStreamError(data []byte, secrets []string) error {
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
	rawCode := payload["error_code"]
	if embeddedZeroCode(rawCode) {
		rawCode = payload["code"]
	}
	hasNormalChunk := streamTruthy(payload["choices"]) || payload["usage"] != nil
	if hasNormalChunk && !streamTruthy(errorValue) && embeddedZeroCode(rawCode) {
		return nil
	}

	code := payload["error_code"]
	if embeddedZeroCode(code) {
		code = payload["code"]
	}
	message := ""
	switch typed := errorValue.(type) {
	case map[string]any:
		for _, key := range []string{"message", "error_msg", "detail"} {
			if value, ok := typed[key].(string); ok && strings.TrimSpace(value) != "" {
				message = strings.TrimSpace(value)
				break
			}
		}
		if embeddedZeroCode(code) {
			// Python uses `error.get("code") or error.get("type")`, where a
			// non-empty "0" string is truthy even though it equals zero.
			if candidate := typed["code"]; streamTruthy(candidate) {
				code = candidate
			} else {
				code = typed["type"]
			}
		}
	case string:
		message = strings.TrimSpace(typed)
	}
	if message == "" {
		for _, key := range []string{"error_msg", "message", "detail", "error_description"} {
			if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
				message = strings.TrimSpace(value)
				break
			}
		}
	}
	if message == "" {
		if embeddedZeroCode(code) {
			return nil
		}
		message = fmt.Sprintf("error_code=%s", catalogText(code))
	}
	message = truncateUTF8(redactText(message, secrets...), maxErrorMessageBytes)

	status := 0
	for _, key := range []string{"status", "status_code", "http_status"} {
		if value := embeddedInt(payload[key]); value > 0 {
			status = value
			break
		}
	}
	if status == 0 {
		switch numeric := embeddedInt(code); {
		case numeric == http.StatusBadRequest, numeric == http.StatusUnauthorized,
			numeric == http.StatusForbidden, numeric == http.StatusTooManyRequests,
			numeric >= 500 && numeric < 600:
			status = numeric
		}
	}

	normalized := strings.ToLower(strings.TrimSpace(catalogText(code)))
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &APIError{Kind: KindAuth, Status: status, Message: message}
	case status == http.StatusBadRequest || embeddedBadRequestCode(normalized):
		return &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest, Message: message}
	case status == http.StatusTooManyRequests || normalized == "429":
		return &APIError{Kind: KindRateLimited, Status: http.StatusTooManyRequests, Message: message}
	default:
		return &APIError{Kind: KindUpstream, Status: status, Message: message}
	}
}

func embeddedBadRequestCode(code string) bool {
	switch code {
	case "400", "bad_request", "invalid_request", "invalid_request_error",
		"invalid_param", "invalid_parameter":
		return true
	default:
		return false
	}
}

func embeddedZeroCode(value any) bool {
	switch typed := value.(type) {
	case bool:
		return !typed
	case json.Number:
		// JSON maps 0 and 0.0 to different Python types but both are falsy;
		// zeroErrorCode only recognizes the integer spelling.
		if typed.String() == "0" {
			return true
		}
		if number, err := typed.Float64(); err == nil && number == 0 {
			return true
		}
		return false
	default:
		return zeroErrorCode(value)
	}
}

func streamTruthy(value any) bool {
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
	case float32:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// embeddedInt mirrors Python int() for status/code values.
func embeddedInt(value any) int {
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
		if number, err := typed.Float64(); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
			return int(number)
		}
	case float64:
		if !math.IsNaN(typed) && !math.IsInf(typed, 0) {
			return int(typed)
		}
	case float32:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case string:
		// Python int("401.5") raises, so a fractional string is not a status
		// or code; numeric JSON floats still truncate above.
		text := strings.TrimSpace(typed)
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			return int(number)
		}
	}
	return 0
}
