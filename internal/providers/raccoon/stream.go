package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errStreamIdleTimeout = errors.New("raccoon: chat stream idle timeout")

// chatStream passes Raccoon SSE payloads through as complete frames. It caches
// the first data event during preflight so an HTTP 200 carrying an embedded
// error fails before the API layer writes headers, and it releases the account
// slot exactly once. After [DONE] it stays open long enough to emit the v1
// credit usage frame, which the API forwards after the terminal marker.
type chatStream struct {
	parent context.Context
	ctx    context.Context
	cancel context.CancelFunc

	pump   *streamPump
	buffer []byte
	idle   time.Duration

	pool      *Pool
	account   *Account
	model     string
	secrets   []string
	startedAt time.Time

	pending     []byte
	pendingData []byte
	preflighted bool
	sawDone     bool

	lastUsage     map[string]any
	lastCredit    float64
	creditKnown   bool
	creditChecked bool
	usageMu       sync.Mutex

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
	model string,
	secrets []string,
	idle time.Duration,
	startedAt time.Time,
) *chatStream {
	pump := newStreamPump(body)
	return &chatStream{
		parent:    parent,
		ctx:       ctx,
		cancel:    cancel,
		pump:      pump,
		idle:      idle,
		pool:      pool,
		account:   account,
		model:     model,
		secrets:   append([]string(nil), secrets...),
		startedAt: startedAt,
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
// [DONE] is yielded once; the following calls first emit the synthetic credit
// usage frame and then drain any post-DONE upstream frames before io.EOF.
func (s *chatStream) Next() ([]byte, error) {
	if s == nil {
		return nil, io.EOF
	}
	if s.sawDone {
		return s.nextAfterDone()
	}
	if s.pending != nil {
		frame := s.pending
		data := s.pendingData
		s.pending = nil
		s.pendingData = nil
		if isDoneEvent(data) {
			s.sawDone = true
			return frame, nil
		}
		s.trackUsage(data)
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
	s.trackUsage(data)
	return frame, nil
}

// nextAfterDone emits the v1 per-request credit frame once, then forwards any
// upstream bytes that follow [DONE]. A missing/multiple bill match produces no
// frame and no guess.
func (s *chatStream) nextAfterDone() ([]byte, error) {
	if !s.creditChecked {
		s.creditChecked = true
		if frame := s.creditFrame(); frame != nil {
			return frame, nil
		}
	}

	frame, data, err := s.readFrame()
	if err != nil {
		_ = s.Close()
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, s.mapReadError(err)
	}
	if isDoneEvent(data) {
		return s.nextAfterDone()
	}
	if err := embeddedStreamError(data, s.secrets); err != nil {
		_ = s.Close()
		return nil, err
	}
	s.trackUsage(data)
	return frame, nil
}

func (s *chatStream) trackUsage(data []byte) {
	if s == nil || len(data) == 0 {
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return
	}
	usage, ok := payload["usage"].(map[string]any)
	if !ok || len(usage) == 0 {
		return
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.lastUsage == nil {
		s.lastUsage = cloneUsage(usage)
		return
	}
	if usageTotal(usage) >= usageTotal(s.lastUsage) {
		s.lastUsage = cloneUsage(usage)
	}
}

func (s *chatStream) creditFrame() []byte {
	if s == nil || s.pool == nil || s.account == nil {
		return nil
	}
	if !s.pool.creditLookbackEnabled() {
		return nil
	}
	credit, ok := s.pool.requestCredit(s.ctx, s.account, s.startedAt)
	if !ok {
		return nil
	}
	s.usageMu.Lock()
	s.lastCredit = float64(credit)
	s.creditKnown = true
	usage := map[string]any{}
	if s.lastUsage != nil {
		usage = cloneUsage(s.lastUsage)
	}
	s.usageMu.Unlock()
	usage["credit"] = credit
	payload := map[string]any{
		"id":      "chatcmpl-raccoon-usage",
		"object":  "chat.completion.chunk",
		"created": s.startedAt.Unix(),
		"model":   s.model,
		"choices": []any{},
		"usage":   usage,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(encoded) + "\n\n")
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
		if s.pump != nil {
			s.pump.close()
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
	if s.pump != nil {
		s.pump.close()
	}
}

type sseLine struct {
	raw     []byte
	payload []byte
}

// streamPump continuously reads the body into an unbuffered channel. The
// consumer applies an idle timeout per chunk, so a long active stream is not
// cut by an absolute whole-stream deadline while a silent upstream still times
// out after the configured interval.
type streamPump struct {
	body io.ReadCloser
	data chan streamChunk
	stop chan struct{}
	done chan struct{}

	stopOnce sync.Once
}

type streamChunk struct {
	data []byte
	err  error
}

func newStreamPump(body io.ReadCloser) *streamPump {
	pump := &streamPump{
		body: body,
		data: make(chan streamChunk),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go pump.run()
	return pump
}

func (p *streamPump) run() {
	defer close(p.done)
	defer close(p.data)
	buffer := make([]byte, 64*1024)
	for {
		n, err := p.body.Read(buffer)
		if n > 0 {
			chunk := streamChunk{data: append([]byte(nil), buffer[:n]...)}
			select {
			case p.data <- chunk:
			case <-p.stop:
				return
			}
		}
		if err != nil {
			select {
			case p.data <- streamChunk{err: err}:
			case <-p.stop:
			}
			return
		}
	}
}

func (p *streamPump) next(parent context.Context, idle time.Duration) ([]byte, error) {
	if p == nil {
		return nil, io.EOF
	}
	if idle <= 0 {
		idle = 15 * time.Second
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	select {
	case chunk, ok := <-p.data:
		if !ok {
			return nil, io.EOF
		}
		if chunk.err != nil {
			return nil, chunk.err
		}
		return chunk.data, nil
	case <-parent.Done():
		p.close()
		return nil, parent.Err()
	case <-timer.C:
		p.close()
		return nil, errStreamIdleTimeout
	}
}

func (p *streamPump) close() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stop)
		if p.body != nil {
			_ = p.body.Close()
		}
	})
}

func (s *chatStream) readLine() ([]byte, error) {
	for {
		if index := bytes.IndexByte(s.buffer, '\n'); index >= 0 {
			line := append([]byte(nil), s.buffer[:index+1]...)
			s.buffer = s.buffer[index+1:]
			return line, nil
		}
		chunk, err := s.pump.next(s.ctx, s.idle)
		if err != nil {
			if errors.Is(err, io.EOF) && len(s.buffer) > 0 {
				line := s.buffer
				s.buffer = nil
				return line, nil
			}
			return nil, err
		}
		s.buffer = append(s.buffer, chunk...)
	}
}

// readFrame reads one SSE event and returns its complete frame plus the joined
// data payload used for error detection. Comments and other non-data fields
// are skipped, matching v1's data-line pass-through.
func (s *chatStream) readFrame() ([]byte, []byte, error) {
	lines := make([]sseLine, 0, 1)
	for {
		line, err := s.readLine()
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
	if errors.Is(err, errStreamIdleTimeout) {
		return &APIError{
			Kind:    KindTransient,
			Message: "raccoon: chat stream timed out",
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
		// Redact only the code value, then build the synthetic label. Passing
		// the whole label through the redactor would match the `error_code=`
		// named-secret pattern and turn a useful numeric code into
		// "error_code=<redacted>".
		message = "error_code=" + redactText(catalogText(code), secrets...)
	} else {
		message = redactText(message, secrets...)
	}
	message = truncateUTF8(message, maxErrorMessageBytes)

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
