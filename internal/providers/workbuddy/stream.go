package workbuddy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var errChatStreamIdleTimeout = errors.New("workbuddy: chat stream idle timeout")

// StreamUsage is the per-stream usage snapshot exposed by chatStream. It is an
// optional provider extension: the generic API does not require it, but a
// WorkBuddy-specific usage writer can persist realm/account/model attribution,
// token counts, the optional credit charge, and the cost policy without
// re-parsing the stream.
type StreamUsage struct {
	Realm            string  `json:"realm"`
	Account          string  `json:"account"`
	Model            string  `json:"model"`
	Tokens           int64   `json:"tokens"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CachedTokens     int64   `json:"cached_tokens,omitempty"`
	ReasoningTokens  int64   `json:"reasoning_tokens,omitempty"`
	Credit           float64 `json:"credit,omitempty"`
	CreditKnown      bool    `json:"-"`
	Free             bool    `json:"free"`
	CostPolicy       string  `json:"cost_policy"`
	FinishReason     string  `json:"finish_reason,omitempty"`
}

// StreamUsageReporter is the optional provider extension used by a
// WorkBuddy-specific usage writer. The generic API does not require it.
type StreamUsageReporter interface {
	StreamUsage() StreamUsage
}

var _ StreamUsageReporter = (*chatStream)(nil)

// chatStream passes complete upstream SSE frames through. It owns one acquired
// account slot, releases it exactly once, and stops reading on parent context
// cancellation or an idle timeout. Complete frames (data/event/id/comment
// lines plus the trailing blank line) are preserved; generic API normalization
// handles ids, finish, and usage hoisting.
type chatStream struct {
	parent context.Context
	cancel context.CancelFunc
	pump   *sseLinePump
	idle   time.Duration

	pool    *Pool
	account *Account

	usageMu sync.Mutex
	usage   StreamUsage

	pendingErr error

	closeOnce sync.Once
	closeErr  error
}

func newChatStream(
	parent context.Context,
	cancel context.CancelFunc,
	body io.ReadCloser,
	contentEncoding string,
	pool *Pool,
	account *Account,
	realm, model string,
	idle time.Duration,
) (*chatStream, error) {
	if parent == nil {
		parent = context.Background()
	}
	if body == nil {
		return nil, newAPIError(KindUpstream, http.StatusBadGateway, "upstream returned no response body")
	}
	if idle <= 0 {
		idle = 5 * time.Minute
	}

	reader := io.ReadCloser(body)
	if strings.Contains(strings.ToLower(strings.TrimSpace(contentEncoding)), "gzip") {
		gzipReader, err := gzip.NewReader(body)
		if err != nil {
			_ = body.Close()
			return nil, newAPIError(KindUpstream, http.StatusBadGateway, "upstream returned invalid gzip data")
		}
		reader = &gzipResponseBody{Reader: gzipReader, raw: body}
	}

	stream := &chatStream{
		parent:  parent,
		cancel:  cancel,
		pump:    newSSELinePump(reader),
		idle:    idle,
		pool:    pool,
		account: account,
		usage: StreamUsage{
			Realm:      realm,
			Model:      strings.TrimSpace(model),
			Free:       false,
			CostPolicy: "credits",
		},
	}
	if account != nil {
		stream.usage.Account = strings.TrimSpace(account.UID())
	}
	return stream, nil
}

type gzipResponseBody struct {
	Reader *gzip.Reader
	raw    io.ReadCloser
}

func (b *gzipResponseBody) Read(p []byte) (int, error) {
	if b == nil || b.Reader == nil {
		return 0, io.EOF
	}
	return b.Reader.Read(p)
}

func (b *gzipResponseBody) Close() error {
	if b == nil {
		return nil
	}
	var err error
	if b.Reader != nil {
		err = b.Reader.Close()
	}
	if b.raw != nil {
		if rawErr := b.raw.Close(); err == nil {
			err = rawErr
		}
	}
	return err
}

// Next returns one complete SSE frame, including its trailing blank line.
// [DONE] is passed through once and any post-DONE upstream frames are still
// returned, matching v1's trailing usage/credit behavior. io.EOF means the
// upstream body closed.
func (s *chatStream) Next() ([]byte, error) {
	if s == nil {
		return nil, io.EOF
	}
	if s.pendingErr != nil {
		err := s.pendingErr
		s.pendingErr = nil
		_ = s.Close()
		return nil, err
	}

	for {
		frame, err := s.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = s.Close()
				return nil, io.EOF
			}
			_ = s.Close()
			if s.parent != nil && s.parent.Err() != nil {
				return nil, s.parent.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if errors.Is(err, errChatStreamIdleTimeout) {
				return nil, newAPIError(KindUpstream, http.StatusBadGateway, "upstream chat stream idle timeout")
			}
			return nil, newAPIError(KindUpstream, http.StatusBadGateway, "upstream chat stream aborted")
		}
		if len(frame) == 0 {
			continue
		}
		s.trackFrame(frame)
		return frame, nil
	}
}

func (s *chatStream) readFrame() ([]byte, error) {
	var frame []byte
	for {
		line, err := s.nextLine()
		if err != nil {
			if len(frame) > 0 {
				if !bytes.HasSuffix(frame, []byte("\n\n")) {
					frame = append(frame, '\n', '\n')
				}
				s.pendingErr = err
				return frame, nil
			}
			return nil, err
		}
		frame = append(frame, line...)
		if sseBlankLine(line) {
			return frame, nil
		}
	}
}

func (s *chatStream) nextLine() ([]byte, error) {
	if s.pump == nil {
		return nil, io.EOF
	}
	timer := time.NewTimer(s.idle)
	defer timer.Stop()
	select {
	case item, ok := <-s.pump.lines:
		if !ok {
			return nil, io.EOF
		}
		if item.err != nil {
			return nil, item.err
		}
		return item.line, nil
	case <-s.parent.Done():
		return nil, s.parent.Err()
	case <-timer.C:
		return nil, errChatStreamIdleTimeout
	}
}

func sseBlankLine(line []byte) bool {
	trimmed := strings.TrimRight(string(line), "\n")
	trimmed = strings.TrimRight(trimmed, "\r")
	return trimmed == ""
}

// streamSSEData joins the data lines of a frame. It mirrors the generic API's
// parser closely enough for usage/finish tracking without altering the frame.
func streamSSEData(frame []byte) string {
	var lines []string
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		if strings.HasPrefix(data, " ") {
			data = data[1:]
		}
		lines = append(lines, data)
	}
	return strings.Join(lines, "\n")
}

func (s *chatStream) trackFrame(frame []byte) {
	data := strings.TrimSpace(streamSSEData(frame))
	if data == "" || data == "[DONE]" {
		return
	}
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	var chunk map[string]any
	if err := decoder.Decode(&chunk); err != nil || chunk == nil {
		return
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if model, ok := chunk["model"].(string); ok && strings.TrimSpace(model) != "" {
		s.usage.Model = strings.TrimSpace(model)
	}
	if choices, ok := chunk["choices"].([]any); ok {
		for _, rawChoice := range choices {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			if finish, ok := choice["finish_reason"].(string); ok && strings.TrimSpace(finish) != "" {
				s.usage.FinishReason = strings.TrimSpace(finish)
			}
		}
	}
	usage, ok := chunk["usage"].(map[string]any)
	if !ok || len(usage) == 0 {
		return
	}
	prompt, _ := usageInt64(usage["prompt_tokens"])
	completion, _ := usageInt64(usage["completion_tokens"])
	total, _ := usageInt64(usage["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	if total < s.usage.TotalTokens {
		return
	}
	s.usage.PromptTokens = prompt
	s.usage.CompletionTokens = completion
	s.usage.TotalTokens = total
	s.usage.Tokens = total
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		s.usage.ReasoningTokens, _ = usageInt64(details["reasoning_tokens"])
	}
	s.usage.CachedTokens = bestChatCachedTokens(usage)
	if credit, ok := usageFloat64(usage["credit"]); ok {
		s.usage.Credit = credit
		s.usage.CreditKnown = true
	}
}

func usageInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		return number, err == nil
	case float64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	default:
		return 0, false
	}
}

func bestChatCachedTokens(usage map[string]any) int64 {
	best := int64(0)
	candidates := []any{usage["cached_tokens"], usage["cache_read_input_tokens"], usage["prompt_cache_hit_tokens"]}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		candidates = append(candidates, details["cached_tokens"])
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		candidates = append(candidates, details["cached_tokens"])
	}
	for _, candidate := range candidates {
		if value, ok := usageInt64(candidate); ok && value > best {
			best = value
		}
	}
	return best
}

func usageFloat64(value any) (float64, bool) {
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

// StreamUsage returns a lock-safe snapshot of the stream's usage/finish state.
func (s *chatStream) StreamUsage() StreamUsage {
	if s == nil {
		return StreamUsage{}
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.usage
}

// Close closes the upstream body and releases the account slot exactly once.
func (s *chatStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.pump != nil {
			s.pump.close()
		}
		if s.cancel != nil {
			s.cancel()
		}
		if s.pool != nil {
			s.pool.Release(s.account)
		}
	})
	return s.closeErr
}

// sseLinePump reads lines from a response body in one goroutine. The consumer
// applies the idle timeout per line, so an active long stream is not cut by an
// absolute whole-stream deadline.
type sseLinePump struct {
	reader *bufio.Reader
	closer io.Closer
	lines  chan sseLine
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

type sseLine struct {
	line []byte
	err  error
}

func newSSELinePump(reader io.ReadCloser) *sseLinePump {
	pump := &sseLinePump{
		reader: bufio.NewReaderSize(reader, 64*1024),
		closer: reader,
		lines:  make(chan sseLine),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go pump.run()
	return pump
}

func (p *sseLinePump) run() {
	defer close(p.done)
	defer close(p.lines)
	for {
		line, err := p.reader.ReadBytes('\n')
		if len(line) > 0 {
			select {
			case p.lines <- sseLine{line: line}:
			case <-p.stop:
				return
			}
		}
		if err != nil {
			select {
			case p.lines <- sseLine{err: err}:
			case <-p.stop:
			}
			return
		}
	}
}

func (p *sseLinePump) close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		close(p.stop)
	})
	// The reader is owned by the pump. Closing it unblocks a pending Read.
	if p.closer != nil {
		_ = p.closer.Close()
	}
}
