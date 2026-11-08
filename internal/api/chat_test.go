package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	srv := NewServer(ServerOptions{Version: "2.0.0-test", Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=raccoon",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	if got, want := rec.Body.String(), strings.Join(frames, ""); got != want {
		t.Fatalf("stream body = %q, want %q", got, want)
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
	ts := httptest.NewServer(NewServer(ServerOptions{Providers: []providers.Provider{provider}}).Handler)
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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

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
	if !strings.Contains(body, "upstream read failed") {
		t.Fatalf("missing error message: %q", body)
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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)

	requireErrorEnvelope(t, rec, http.StatusBadGateway, "invalid_request_error")
}

func TestChatErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		errType string
	}{
		{"bad request", &mappedTestError{status: http.StatusBadRequest, errType: "invalid_request_error", message: "bad"}, http.StatusBadRequest, "invalid_request_error"},
		{"auth", &mappedTestError{status: http.StatusBadGateway, errType: "invalid_request_error", message: "auth"}, http.StatusBadGateway, "invalid_request_error"},
		{"rate limit", &mappedTestError{status: http.StatusTooManyRequests, errType: "rate_limit_error", message: "slow"}, http.StatusTooManyRequests, "rate_limit_error"},
		{"unavailable", &mappedTestError{status: http.StatusServiceUnavailable, errType: "invalid_request_error", message: "down"}, http.StatusServiceUnavailable, "invalid_request_error"},
		{"upstream", &mappedTestError{status: http.StatusBadGateway, errType: "invalid_request_error", message: "upstream"}, http.StatusBadGateway, "invalid_request_error"},
		{"unknown", errors.New("plain failure"), http.StatusBadGateway, "invalid_request_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubProvider{
				id:      "raccoon",
				realm:   "raccoon",
				models:  []providers.Model{{"id": "m1"}},
				openErr: tc.err,
			}
			srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})
			rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
				`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
			requireErrorEnvelope(t, rec, tc.status, tc.errType)
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
			srv := NewServer(ServerOptions{Providers: []providers.Provider{alpha, beta}})
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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})
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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=missing",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
	requireErrorEnvelope(t, rec, http.StatusNotFound, "invalid_request_error")

	rec = serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"missing-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	requireErrorEnvelope(t, rec, http.StatusNotFound, "invalid_request_error")
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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{intl, cn}})
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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=all",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
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
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

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
