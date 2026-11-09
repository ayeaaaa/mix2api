package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// stubProvider is a generic (non-Raccoon) provider used to prove the API layer
// only depends on the providers.Provider contract.
type stubProvider struct {
	id    string
	realm string

	models    []providers.Model
	modelsErr error

	health    providers.Health
	healthErr error

	openStream providers.ChatStream
	openErr    error

	routes []providers.Route

	mu           sync.Mutex
	openRequests []providers.ChatRequest
	modelCalls   int
}

func (p *stubProvider) ID() string    { return p.id }
func (p *stubProvider) Realm() string { return p.realm }

func (p *stubProvider) Overview(context.Context, bool) (any, error) { return nil, nil }

func (p *stubProvider) Health(context.Context, bool) (providers.Health, error) {
	if p.healthErr != nil {
		return nil, p.healthErr
	}
	if p.health == nil {
		return providers.Health{"ok": true}, nil
	}
	return p.health, nil
}

func (p *stubProvider) Models(context.Context, bool) ([]providers.Model, error) {
	p.mu.Lock()
	p.modelCalls++
	p.mu.Unlock()
	if p.modelsErr != nil {
		return nil, p.modelsErr
	}
	out := make([]providers.Model, len(p.models))
	copy(out, p.models)
	return out, nil
}

func (p *stubProvider) OpenChat(_ context.Context, req providers.ChatRequest) (providers.ChatStream, error) {
	p.mu.Lock()
	p.openRequests = append(p.openRequests, req)
	p.mu.Unlock()
	if p.openErr != nil {
		return nil, p.openErr
	}
	if p.openStream == nil {
		return nil, errors.New("stub provider has no stream")
	}
	return p.openStream, nil
}

func (p *stubProvider) Routes() []providers.Route { return p.routes }
func (p *stubProvider) Jobs() []scheduler.Job     { return nil }

func (p *stubProvider) opened() []providers.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]providers.ChatRequest, len(p.openRequests))
	copy(out, p.openRequests)
	return out
}

// stubStream serves frames and then an optional terminal error.
type stubStream struct {
	mu          sync.Mutex
	frames      [][]byte
	terminal    error
	index       int
	errReturned bool
	closed      bool
}

func newStubStream(frames ...string) *stubStream {
	stream := &stubStream{frames: make([][]byte, len(frames))}
	for i, frame := range frames {
		stream.frames[i] = []byte(frame)
	}
	return stream
}

func (s *stubStream) Next() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index < len(s.frames) {
		frame := s.frames[s.index]
		s.index++
		return append([]byte(nil), frame...), nil
	}
	if s.terminal != nil && !s.errReturned {
		s.errReturned = true
		return nil, s.terminal
	}
	return nil, io.EOF
}

func (s *stubStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *stubStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// mappedTestError exercises the provider-neutral HTTP error contract.
type mappedTestError struct {
	status  int
	errType string
	message string
}

func (e *mappedTestError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *mappedTestError) HTTPStatus() int { return e.status }
func (e *mappedTestError) ErrorType() string {
	return e.errType
}
func (e *mappedTestError) SafeMessage() string { return e.message }

func serveRequest(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	handler.ServeHTTP(rec, req)
	return rec
}

func requireErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, status int, errType string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, status, rec.Body)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %s: %v", rec.Body, err)
	}
	failure, ok := body["error"]
	if !ok {
		t.Fatalf("missing error envelope: %#v", body)
	}
	if failure["type"] != errType {
		t.Fatalf("error type = %#v, want %q (body=%s)", failure["type"], errType, rec.Body)
	}
	if failure["code"] != float64(status) {
		t.Fatalf("error code = %#v, want %d (body=%s)", failure["code"], status, rec.Body)
	}
	return failure
}

func TestChatStreamingRaccoon(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":100,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":100,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n",
		"data: [DONE]\n\n",
	}
	stream := newStubStream(frames...)
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Version: "2.0.0-test", Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hello"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream body = %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("synthesized finish frame missing: %q", body)
	}
	if !stream.isClosed() {
		t.Fatal("stream was not closed")
	}
	requests := provider.opened()
	if len(requests) != 1 {
		t.Fatalf("OpenChat calls = %d, want 1", len(requests))
	}
	if requests[0].Model != "m1" {
		t.Fatalf("request model = %q, want m1", requests[0].Model)
	}
	if len(requests[0].Messages) != 1 {
		t.Fatalf("request messages = %d, want 1", len(requests[0].Messages))
	}
}

func TestChatStreamsHeadersOnlyAfterFirstFrame(t *testing.T) {
	gate := make(chan struct{})
	stream := &gateStream{
		gate:  gate,
		frame: []byte("data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"),
	}
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	ts := httptest.NewServer(newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}}).Handler)
	defer ts.Close()

	type result struct {
		resp *http.Response
		err  error
	}
	results := make(chan result, 1)
	go func() {
		resp, err := http.Post(
			ts.URL+"/v1/chat/completions?realm=raccoon",
			"application/json",
			strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		)
		results <- result{resp: resp, err: err}
	}()

	select {
	case got := <-results:
		if got.resp != nil {
			_ = got.resp.Body.Close()
		}
		t.Fatalf("response headers arrived before the first stream frame: %v", got.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(gate)

	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("request failed: %v", got.err)
		}
		defer got.resp.Body.Close()
		body, err := io.ReadAll(got.resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), `"content":"ok"`) {
			t.Fatalf("body = %q", body)
		}
		if !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
			t.Fatalf("stream did not terminate with [DONE]: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not complete after the first frame")
	}
}

type gateStream struct {
	gate  <-chan struct{}
	frame []byte
	step  int
}

func (s *gateStream) Next() ([]byte, error) {
	if s.step == 0 {
		<-s.gate
		s.step = 1
		return s.frame, nil
	}
	return nil, io.EOF
}

func (s *gateStream) Close() error { return nil }

// silentStream emits one frame then blocks in Next until released or closed,
// which is exactly the window an SSE heartbeat must cover.
type silentStream struct {
	first   []byte
	release chan struct{}
	once    sync.Once
	closed  chan struct{}
	step    int
}

func newSilentStream(first string) *silentStream {
	return &silentStream{
		first:   []byte(first),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (s *silentStream) Next() ([]byte, error) {
	if s.step == 0 {
		s.step = 1
		return s.first, nil
	}
	select {
	case <-s.release:
		return nil, io.EOF
	case <-s.closed:
		return nil, io.EOF
	}
}

func (s *silentStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

type delayedCreditStream struct {
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
	step    int
}

func newDelayedCreditStream() *delayedCreditStream {
	return &delayedCreditStream{release: make(chan struct{}), closed: make(chan struct{})}
}

func (s *delayedCreditStream) Next() ([]byte, error) {
	switch s.step {
	case 0:
		s.step++
		return []byte("data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"), nil
	case 1:
		s.step++
		return []byte("data: [DONE]\n\n"), nil
	case 2:
		select {
		case <-s.release:
			s.step++
			return []byte("data: {\"id\":\"chatcmpl-raccoon-usage\",\"choices\":[],\"usage\":{\"total_tokens\":2,\"credit\":2}}\n\n"), nil
		case <-s.closed:
			return nil, io.EOF
		}
	default:
		return nil, io.EOF
	}
}

func (s *delayedCreditStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestChatForwardsDoneBeforePostDoneCreditLookup(t *testing.T) {
	stream := newDelayedCreditStream()
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	live := httptest.NewServer(newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}}).Handler)
	defer live.Close()

	response, err := http.Post(live.URL+"/v1/chat/completions?realm=raccoon", "application/json",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	doneSeen := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		var body strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			body.WriteString(line)
			if strings.Contains(body.String(), "data: [DONE]") {
				close(doneSeen)
				return
			}
		}
	}()
	select {
	case <-doneSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("[DONE] was delayed by the post-DONE credit lookup")
	}
	<-readerDone

	close(stream.release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read post-DONE frames: %v", err)
	}
	if !strings.Contains(string(rest), `"credit":2`) {
		t.Fatalf("post-DONE credit frame missing: %q", rest)
	}
}

func TestChatStreamSendsKeepaliveWhileBlocked(t *testing.T) {
	oldInterval := sseKeepaliveInterval
	sseKeepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { sseKeepaliveInterval = oldInterval })

	stream := newSilentStream("data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	live := httptest.NewServer(newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}}).Handler)
	defer live.Close()

	response, err := http.Post(live.URL+"/v1/chat/completions?realm=raccoon", "application/json",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	found := make(chan struct{})
	go func() {
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			if strings.Contains(line, "keepalive") {
				close(found)
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(2 * time.Second):
		t.Fatal("silent upstream did not produce an SSE keepalive")
	}
	close(stream.release)
	_, _ = io.Copy(io.Discard, reader)
}

func TestChatStreamingMidstreamErrorEmitsSSEError(t *testing.T) {
	stream := newStubStream(
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
	)
	stream.terminal = errors.New("upstream read failed")
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"partial"`) {
		t.Fatalf("missing partial chunk: %q", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("missing SSE error frame: %q", body)
	}
	if !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("missing generic error message: %q", body)
	}
	if strings.Contains(body, "upstream read failed") {
		t.Fatalf("midstream error leaked raw provider text: %q", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("midstream error must not emit [DONE]: %q", body)
	}
	if !stream.isClosed() {
		t.Fatal("stream was not closed")
	}
}

func TestChatFirstFrameErrorUsesJSONEnvelope(t *testing.T) {
	stream := &stubStream{terminal: &mappedTestError{
		status: http.StatusServiceUnavailable, errType: "invalid_request_error", message: "no account available",
	}}
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	failure := requireErrorEnvelope(t, rec, http.StatusServiceUnavailable, "invalid_request_error")
	if failure["message"] != "no account available" {
		t.Fatalf("message = %#v", failure["message"])
	}
	if got := rec.Header().Get("Content-Type"); strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("first-frame failure wrote SSE headers: %q", got)
	}
}

func TestChatEmptyStreamIsBadGateway(t *testing.T) {
	stream := newStubStream()
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	requireErrorEnvelope(t, rec, http.StatusBadGateway, "invalid_request_error")
}

func TestChatNonStreamingRaccoon(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":100,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n",
		"data: [DONE]\n\n",
	}
	stream := newStubStream(frames...)
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %#v", out["object"])
	}
	if out["id"] != "chatcmpl-1" || out["model"] != "m1" || out["created"] != float64(100) {
		t.Fatalf("completion identity = %#v", out)
	}
	choices, ok := out["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %#v", out["choices"])
	}
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] != "Hello" {
		t.Fatalf("message = %#v", message)
	}
	if message["reasoning_content"] != "think" {
		t.Fatalf("reasoning_content = %#v", message["reasoning_content"])
	}
	toolCalls, ok := message["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %#v", message["tool_calls"])
	}
	call := toolCalls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || fn["name"] != "lookup" {
		t.Fatalf("tool call = %#v", call)
	}
	if fn["arguments"] != `{"a":1}` {
		t.Fatalf("arguments = %#v", fn["arguments"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["total_tokens"] != float64(7) {
		t.Fatalf("usage = %#v", usage)
	}
	if !stream.isClosed() {
		t.Fatal("stream was not closed")
	}
}

func TestChatNonStreamingUsageOnlyIsBadGateway(t *testing.T) {
	stream := newStubStream(
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[],\"usage\":{\"total_tokens\":7}}\n\n",
		"data: [DONE]\n\n",
	)
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)

	requireErrorEnvelope(t, rec, http.StatusBadGateway, "invalid_request_error")
}

func TestChatNonStreamingReadErrorIsBadGateway(t *testing.T) {
	stream := newStubStream(
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
	)
	stream.terminal = errors.New("upstream read failed")
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)

	requireErrorEnvelope(t, rec, http.StatusBadGateway, "invalid_request_error")
}

func TestChatErrorMapping(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		status      int
		errType     string
		wantMessage string
	}{
		{"bad request", &mappedTestError{status: http.StatusBadRequest, errType: "invalid_request_error", message: "bad"}, http.StatusBadRequest, "invalid_request_error", "bad"},
		{"auth", &mappedTestError{status: http.StatusBadGateway, errType: "invalid_request_error", message: "auth"}, http.StatusBadGateway, "invalid_request_error", "auth"},
		{"rate limit", &mappedTestError{status: http.StatusTooManyRequests, errType: "rate_limit_error", message: "slow"}, http.StatusTooManyRequests, "rate_limit_error", "slow"},
		{"unavailable", &mappedTestError{status: http.StatusServiceUnavailable, errType: "invalid_request_error", message: "down"}, http.StatusServiceUnavailable, "invalid_request_error", "down"},
		{"upstream", &mappedTestError{status: http.StatusBadGateway, errType: "invalid_request_error", message: "upstream"}, http.StatusBadGateway, "invalid_request_error", "upstream"},
		{"unknown", errors.New("plain failure"), http.StatusBadGateway, "invalid_request_error", "upstream request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubProvider{
				id:      "raccoon",
				realm:   "raccoon",
				models:  []providers.Model{{"id": "m1"}},
				openErr: tc.err,
			}
			srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
			rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
				`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
			failure := requireErrorEnvelope(t, rec, tc.status, tc.errType)
			if failure["message"] != tc.wantMessage {
				t.Fatalf("message = %#v, want %q", failure["message"], tc.wantMessage)
			}
		})
	}
}

func TestChatResolvesExplicitRealmAndHeaderAndBody(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		body    string
	}{
		{
			name: "query realm",
			path: "/v1/chat/completions?realm=beta",
			body: `{"model":"beta-model","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
		{
			name:    "header realm",
			path:    "/v1/chat/completions",
			headers: map[string]string{"X-Realm": "beta"},
			body:    `{"model":"beta-model","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
		{
			name:    "header wins over query",
			path:    "/v1/chat/completions?realm=alpha",
			headers: map[string]string{"X-Realm": "beta"},
			body:    `{"model":"beta-model","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
		{
			name: "body realm",
			path: "/v1/chat/completions",
			body: `{"model":"beta-model","realm":"beta","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
		{
			name: "model match",
			path: "/v1/chat/completions",
			body: `{"model":"beta-model","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alpha := &stubProvider{
				id: "alpha", realm: "alpha",
				models: []providers.Model{{"id": "alpha-model"}}, openStream: newStubStream("data: [DONE]\n\n"),
			}
			beta := &stubProvider{
				id: "beta", realm: "beta",
				models: []providers.Model{{"id": "beta-model"}}, openStream: newStubStream("data: [DONE]\n\n"),
			}
			srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{alpha, beta}})
			rec := serveRequest(srv.Handler, http.MethodPost, tc.path, tc.body, tc.headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if len(alpha.opened()) != 0 {
				t.Fatalf("alpha provider was opened: %#v", alpha.opened())
			}
			requests := beta.opened()
			if len(requests) != 1 {
				t.Fatalf("beta OpenChat calls = %d, want 1", len(requests))
			}
			if _, leaked := requests[0].Raw["realm"]; leaked {
				t.Fatalf("gateway-only realm field leaked to provider: %#v", requests[0].Raw)
			}
		})
	}
}

func TestChatSingleProviderFallback(t *testing.T) {
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(provider.opened()) != 1 {
		t.Fatalf("OpenChat calls = %d, want 1", len(provider.opened()))
	}
}

func TestChatUnknownRealmAndModel(t *testing.T) {
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=missing",
		`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	requireErrorEnvelope(t, rec, http.StatusNotFound, "invalid_request_error")

	// A model absent from a stale/empty catalog must reach the provider so the
	// upstream can classify it instead of being rejected by discovery cache.
	rec = serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"missing-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("catalog pre-validation rejected an upstream-supported model: %s", rec.Body)
	}
	requests := provider.opened()
	if len(requests) != 1 || requests[0].Model != "missing-model" {
		t.Fatalf("provider requests = %#v, want missing-model", requests)
	}
}

func TestChatRealmAllWorkBuddy(t *testing.T) {
	intl := &stubProvider{
		id: "workbuddy", realm: "intl",
		models: []providers.Model{{"id": "intl-model"}}, openStream: newStubStream("data: [DONE]\n\n"),
	}
	cn := &stubProvider{
		id: "workbuddy", realm: "cn",
		models: []providers.Model{{"id": "cn-model"}}, openStream: newStubStream("data: [DONE]\n\n"),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=all",
		`{"model":"cn-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(cn.opened()) != 1 || len(intl.opened()) != 0 {
		t.Fatalf("intl=%d cn=%d, want cn only", len(intl.opened()), len(cn.opened()))
	}
}

func TestChatRealmAllLockedWithoutWorkBuddy(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models: []providers.Model{{"id": "m1"}}, openStream: newStubStream("data: [DONE]\n\n"),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=all",
		`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	failure := requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
	if !strings.Contains(failure["message"].(string), "WorkBuddy") {
		t.Fatalf("message = %#v", failure["message"])
	}
}

func TestChatMethodAndBodyValidation(t *testing.T) {
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/chat/completions", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", rec.Code)
	}
	requireErrorEnvelope(t, rec, http.StatusMethodNotAllowed, "invalid_request_error")

	for _, body := range []string{
		``,
		`not-json`,
		`[]`,
		`{"model":"m1"}`,
		`{"messages":[{"role":"user","content":"hi"}]}`,
	} {
		rec = serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", body, nil)
		requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
	}
}

func assertNoSensitiveText(t *testing.T, body string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if strings.Contains(body, needle) {
			t.Fatalf("response leaked %q: %s", needle, body)
		}
	}
}

func TestChatUnknownProviderErrorDoesNotLeakLocalDetail(t *testing.T) {
	localErr := errors.New("open /root/private/raccoon/u-abc.json: uid u-abc")
	provider := &stubProvider{
		id:      "raccoon",
		realm:   "raccoon",
		models:  []providers.Model{{"id": "m1"}},
		openErr: localErr,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)

	failure := requireErrorEnvelope(t, rec, http.StatusBadGateway, "invalid_request_error")
	if failure["message"] != "upstream request failed" {
		t.Fatalf("message = %#v", failure["message"])
	}
	assertNoSensitiveText(t, rec.Body.String(), "/root/private", "u-abc", ".json")
}

func TestChatMidstreamUnknownErrorDoesNotLeakLocalDetail(t *testing.T) {
	stream := newStubStream(
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
	)
	stream.terminal = errors.New("open /root/private/raccoon/u-abc.json: uid u-abc")
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("missing generic SSE error: %q", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("midstream error emitted [DONE]: %q", body)
	}
	assertNoSensitiveText(t, body, "/root/private", "u-abc", ".json")
}

func TestChatNonStreamCleanEOFWithoutDone(t *testing.T) {
	stream := newStubStream(
		"data: {\"id\":\"chatcmpl-1\",\"created\":100,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n",
	)
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: stream,
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	choices := completion["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "ok" || choice["finish_reason"] != "stop" {
		t.Fatalf("completion = %#v", completion)
	}
	if !stream.isClosed() {
		t.Fatal("stream was not closed")
	}
}

func TestChatSessionKeyExtraction(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		body    string
		want    string
	}{
		{
			name:    "header",
			headers: map[string]string{"X-Conversation-Id": "conv-header"},
			body:    `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			want:    "conv-header",
		},
		{
			name: "body conversation_id",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"conversation_id":"conv-body","stream":true}`,
			want: "conv-body",
		},
		{
			name: "body session_id",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"session_id":"sess-body","stream":true}`,
			want: "sess-body",
		},
		{
			name: "metadata conversation_id",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-meta"},"stream":true}`,
			want: "conv-meta",
		},
		{
			name:    "header wins over body",
			headers: map[string]string{"Session-Id": "sess-header"},
			body:    `{"model":"m1","messages":[{"role":"user","content":"hi"}],"session_id":"sess-body","stream":true}`,
			want:    "sess-header",
		},
		{
			name: "none",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubProvider{
				id:         "raccoon",
				realm:      "raccoon",
				models:     []providers.Model{{"id": "m1"}},
				openStream: newStubStream("data: [DONE]\n\n"),
			}
			srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
			rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", tc.body, tc.headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			requests := provider.opened()
			if len(requests) != 1 {
				t.Fatalf("OpenChat calls = %d, want 1", len(requests))
			}
			if requests[0].SessionKey != tc.want {
				t.Fatalf("SessionKey = %q, want %q", requests[0].SessionKey, tc.want)
			}
		})
	}
}

func TestChatBodyLimitReturns413(t *testing.T) {
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"pad":"` +
		strings.Repeat("x", maxChatBodyBytes) + `"}`

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", body, nil)
	requireErrorEnvelope(t, rec, http.StatusRequestEntityTooLarge, "invalid_request_error")
	if len(provider.opened()) != 0 {
		t.Fatalf("oversized request reached provider: %#v", provider.opened())
	}
}

func TestChatDoesNotBlockOnCatalogDiscovery(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		modelsErr:  errors.New("catalog unavailable"),
		openStream: newStubStream("data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n", "data: [DONE]\n\n"),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	for _, path := range []string{"/v1/chat/completions?realm=raccoon", "/v1/chat/completions"} {
		provider.openStream = newStubStream("data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n", "data: [DONE]\n\n")
		rec := serveRequest(srv.Handler, http.MethodPost, path,
			`{"model":"not-in-catalog","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want provider classification not catalog rejection: %s", path, rec.Code, rec.Body)
		}
	}
	provider.mu.Lock()
	modelCalls := provider.modelCalls
	provider.mu.Unlock()
	if modelCalls != 0 {
		t.Fatalf("chat performed %d catalog discovery calls", modelCalls)
	}
	if len(provider.opened()) != 2 {
		t.Fatalf("provider OpenChat calls = %d, want 2", len(provider.opened()))
	}
}

func TestHandleChatModelParameterSuppliesMissingBodyModel(t *testing.T) {
	provider := &stubProvider{
		id:         "raccoon",
		realm:      "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	server := newTestServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	server.HandleChat(rec, req, provider, "m1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	requests := provider.opened()
	if len(requests) != 1 || requests[0].Model != "m1" {
		t.Fatalf("requests = %#v", requests)
	}
	var rawModel string
	if err := json.Unmarshal(requests[0].Raw["model"], &rawModel); err != nil || rawModel != "m1" {
		t.Fatalf("forwarded model = %q (err=%v), want m1", rawModel, err)
	}
}

func TestChatStreamNormalizationSingleIDAndTrailingUsage(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"upstream-one\",\"object\":\"chat.completion.chunk\",\"created\":100,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n",
		"data: {\"id\":\"upstream-two\",\"object\":\"chat.completion.chunk\",\"created\":100,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n",
		"data: [DONE]\n\n",
	}
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream(frames...),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var ids []string
	var finishSeen, usageSeen bool
	for _, frame := range splitTestFrames(rec.Body.String()) {
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(frame), "data:"))
		if data == "[DONE]" || data == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		id, _ := payload["id"].(string)
		if id == "" {
			t.Fatalf("frame missing gateway id: %s", data)
		}
		ids = append(ids, id)
		choices, _ := payload["choices"].([]any)
		if len(choices) > 0 {
			if _, hasUsage := payload["usage"]; hasUsage {
				t.Fatalf("choice-bearing frame kept usage: %s", data)
			}
			choice := choices[0].(map[string]any)
			if reason, _ := choice["finish_reason"].(string); reason != "" {
				finishSeen = true
			}
		} else if usage, ok := payload["usage"].(map[string]any); ok && usage["total_tokens"] != nil {
			usageSeen = true
			if usage["total_tokens"] != float64(13) {
				t.Fatalf("trailing usage = %#v, want upstream 13", usage)
			}
			if !finishSeen {
				t.Fatalf("usage frame appeared before synthesized finish: %s", rec.Body)
			}
		}
	}
	if len(ids) < 4 {
		t.Fatalf("normalized frames = %d, want role/content/finish/usage: %s", len(ids), rec.Body)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("stream ids differ: %#v", ids)
		}
	}
	if !strings.HasPrefix(ids[0], "chatcmpl-wb-") {
		t.Fatalf("gateway id = %q", ids[0])
	}
	if !finishSeen || !usageSeen {
		t.Fatalf("finish=%v usage=%v (%s)", finishSeen, usageSeen, rec.Body)
	}
}

func TestChatPostDoneUsageFrameForwardedAndAggregated(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
		"data: {\"id\":\"chatcmpl-raccoon-usage\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"credit\":2}}\n\n",
	}
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream(frames...),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	doneIndex := strings.Index(body, "data: [DONE]\n\n")
	creditIndex := strings.Index(body, `"credit":2`)
	if doneIndex < 0 || creditIndex < 0 || creditIndex < doneIndex {
		t.Fatalf("post-DONE credit frame not forwarded after DONE: %q", body)
	}

	// Non-stream aggregation must consume the same post-DONE frame.
	provider.openStream = newStubStream(frames...)
	rec = serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nonstream status = %d, body = %s", rec.Code, rec.Body)
	}
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	usage := completion["usage"].(map[string]any)
	if usage["credit"] != float64(2) {
		t.Fatalf("nonstream usage = %#v, want credit 2", usage)
	}
}

func TestChatStreamDropsTruncatedToolArguments(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"a\\\":\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream(frames...),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "tool_calls") || strings.Contains(body, `"arguments"`) {
		t.Fatalf("truncated tool call was forwarded: %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("tool_calls finish was not downgraded: %q", body)
	}
}

func TestChatStreamKeepsCompleteToolCall(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n",
		"data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream(frames...),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("complete tool call did not set finish_reason: %q", body)
	}
	foundArguments := ""
	for _, frame := range splitTestFrames(body) {
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(frame), "data:"))
		if data == "[DONE]" || data == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		if len(calls) == 0 {
			continue
		}
		function, _ := calls[0].(map[string]any)["function"].(map[string]any)
		foundArguments, _ = function["arguments"].(string)
	}
	if foundArguments != `{"a":1}` {
		t.Fatalf("arguments = %q, want complete JSON", foundArguments)
	}
}

func TestChatNonStreamUsageEstimateAndTruncatedToolDrop(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models: []providers.Model{{"id": "m1"}},
		openStream: newStubStream(
			"data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"length\"}]}\n\n",
			"data: {\"id\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n",
		),
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := message["tool_calls"]; ok {
		t.Fatalf("truncated tool call reached nonstream message: %#v", message)
	}
	usage, ok := completion["usage"].(map[string]any)
	if !ok || usage["total_tokens"] == float64(0) {
		t.Fatalf("missing usage estimate: %#v", completion)
	}
}

func splitTestFrames(body string) []string {
	frames := make([]string, 0)
	current := strings.Builder{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			if current.Len() > 0 {
				frames = append(frames, current.String()+"\n\n")
				current.Reset()
			}
			continue
		}
		current.WriteString(line)
		current.WriteByte('\n')
	}
	return frames
}

func TestRealmAllRequiresWorkBuddyIdentity(t *testing.T) {
	makeProvider := func(id, realm, model string) *stubProvider {
		return &stubProvider{
			id: id, realm: realm,
			models:     []providers.Model{{"id": model}},
			openStream: newStubStream("data: [DONE]\n\n"),
		}
	}
	intl := makeProvider("raccoon", "intl", "intl-model")
	cn := makeProvider("raccoon", "cn", "cn-model")
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=all", "", nil)
	requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")

	rec = serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=all",
		`{"model":"intl-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")

	if len(intl.opened()) != 0 || len(cn.opened()) != 0 {
		t.Fatalf("realm=all opened a non-WorkBuddy provider")
	}
}

func TestChatRealmFallbackSuppression(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		headers      map[string]string
		body         string
		keyRealm     string
		wantExplicit bool
	}{
		{
			name:         "query realm",
			path:         "/v1/chat/completions?realm=raccoon",
			body:         `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantExplicit: true,
		},
		{
			name:         "header realm",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"X-Realm": "raccoon"},
			body:         `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantExplicit: true,
		},
		{
			name:         "body realm",
			path:         "/v1/chat/completions",
			body:         `{"model":"m1","realm":"raccoon","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantExplicit: true,
		},
		{
			name:         "api key realm binding",
			path:         "/v1/chat/completions",
			body:         `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			keyRealm:     "raccoon",
			wantExplicit: true,
		},
		{
			name:         "no explicit intent",
			path:         "/v1/chat/completions",
			body:         `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantExplicit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubProvider{
				id: "raccoon", realm: "raccoon",
				models:     []providers.Model{{"id": "m1"}},
				openStream: newStubStream("data: [DONE]\n\n"),
			}
			opts := ServerOptions{Providers: []providers.Provider{provider}}
			if tc.keyRealm != "" {
				opts.APIKeyRealm = func(*http.Request) string { return tc.keyRealm }
			}
			srv := newTestHTTPServer(opts)
			rec := serveRequest(srv.Handler, http.MethodPost, tc.path, tc.body, tc.headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			requests := provider.opened()
			if len(requests) != 1 {
				t.Fatalf("OpenChat calls = %d", len(requests))
			}
			flag := requests[0].AllowRealmFallback
			if tc.wantExplicit {
				if flag == nil || *flag {
					t.Fatalf("AllowRealmFallback = %v, want explicit false", flag)
				}
			} else if flag != nil {
				t.Fatalf("AllowRealmFallback = %v, want nil for unconstrained request", *flag)
			}
		})
	}
}

type retryTestError struct {
	status int
	reset  time.Time
}

func (e *retryTestError) Error() string             { return "rate limited" }
func (e *retryTestError) HTTPStatus() int           { return e.status }
func (e *retryTestError) ErrorType() string         { return "rate_limit_error" }
func (e *retryTestError) SafeMessage() string       { return "slow down" }
func (e *retryTestError) RetryDelay() time.Duration { return 90 * time.Second }
func (e *retryTestError) RetryReset() time.Time     { return e.reset }

func TestChatRateLimitRetryMetadataHeaders(t *testing.T) {
	reset := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:  []providers.Model{{"id": "m1"}},
		openErr: &retryTestError{status: http.StatusTooManyRequests, reset: reset},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	failure := requireErrorEnvelope(t, rec, http.StatusTooManyRequests, "rate_limit_error")
	if got := rec.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want 90", got)
	}
	if got := rec.Header().Get("X-RateLimit-Reset"); got != strconv.FormatInt(reset.Unix(), 10) {
		t.Fatalf("X-RateLimit-Reset = %q", got)
	}
	if got := rec.Header().Get("X-RateLimit-Reset-At"); got != reset.Format(time.RFC3339) {
		t.Fatalf("X-RateLimit-Reset-At = %q", got)
	}
	if got, _ := failure["retry_after"].(float64); got != 90 {
		t.Fatalf("body retry_after = %#v", failure["retry_after"])
	}
	if got, _ := failure["reset_at"].(float64); int64(got) != reset.Unix() {
		t.Fatalf("body reset_at = %#v", failure["reset_at"])
	}
}
