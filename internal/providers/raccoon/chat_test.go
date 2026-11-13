package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// chatFake is the deterministic Go translation of v1's _fake_raccoon.py chat
// and refresh subset. No test reaches a real upstream.
type chatFake struct {
	mu sync.Mutex

	chatCalls    int
	refreshCalls int
	bodies       []map[string]any
	headers      []http.Header
	auths        []string

	// statuses is consumed per chat call; an exhausted queue uses
	// statusByToken and then 200.
	statuses []int
	// statusByToken overrides the HTTP status per bearer token.
	statusByToken map[string]int
	errorMessage  string

	// embedded is emitted as the only SSE frame when non-empty.
	embedded string
	// embeddedAfter emits this many normal frames before embedded.
	embeddedAfter int
	// embeddedByToken overrides embedded for a bearer token.
	embeddedByToken map[string]string

	// sseBody replaces the default role/content/finish/usage stream.
	sseBody string
	// truncateAfter emits this many default frames and closes the body
	// without [DONE].
	truncateAfter int

	refreshToken    string
	refreshedToken  string
	refreshedRT     string
	expectedRefresh string
	refreshStatus   int

	// chatStarted is closed before the first chat response is written.
	chatStarted chan struct{}
	// blockAfterHeaders keeps the chat handler alive after writing its SSE
	// body until the request context is canceled (or the test releases it).
	blockAfterHeaders bool
	// hang blocks after sending response headers but before any SSE frame.
	hang bool

	// balance and billItems serve the points endpoints for credit prewarm and
	// the post-DONE per-request credit look-back.
	balance   map[string]any
	billItems []map[string]any
	billCalls int
}

func (f *chatFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == ChatPath:
		f.serveChat(w, r)
	case r.Method == http.MethodPost && r.URL.Path == RefreshPath:
		f.serveRefresh(w, r)
	case r.Method == http.MethodGet && r.URL.Path == PointsBalancePath:
		f.serveBalance(w)
	case r.Method == http.MethodGet && r.URL.Path == PointsBillsPath:
		f.serveBills(w, r)
	default:
		writeCatalogJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
	}
}

func (f *chatFake) serveBalance(w http.ResponseWriter) {
	f.mu.Lock()
	balance := f.balance
	f.mu.Unlock()
	if balance == nil {
		writeCatalogJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
		return
	}
	writeCatalogJSON(w, http.StatusOK, map[string]any{"code": 0, "data": balance})
}

func (f *chatFake) serveBills(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.billCalls++
	items := append([]map[string]any(nil), f.billItems...)
	f.mu.Unlock()
	_ = r
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"data": map[string]any{"items": items, "has_more": false},
	})
}

func (f *chatFake) setBillItems(items []map[string]any) {
	f.mu.Lock()
	f.billItems = append([]map[string]any(nil), items...)
	f.mu.Unlock()
}

func (f *chatFake) serveChat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if raw, err := io.ReadAll(r.Body); err == nil {
		_ = json.Unmarshal(raw, &body)
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))

	f.mu.Lock()
	f.chatCalls++
	call := f.chatCalls
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	f.auths = append(f.auths, token)
	status := 0
	if len(f.statuses) >= call {
		status = f.statuses[call-1]
	}
	if perToken, ok := f.statusByToken[token]; ok {
		status = perToken
	}
	embedded := f.embedded
	if perToken, ok := f.embeddedByToken[token]; ok {
		embedded = perToken
	}
	embeddedAfter := f.embeddedAfter
	sseBody := f.sseBody
	truncateAfter := f.truncateAfter
	errorMessage := f.errorMessage
	started := f.chatStarted
	block := f.blockAfterHeaders
	hang := f.hang
	f.mu.Unlock()

	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if status != 0 && status != http.StatusOK {
		if errorMessage == "" {
			errorMessage = fmt.Sprintf("chat failed with HTTP %d", status)
		}
		writeCatalogJSON(w, status, map[string]any{"message": errorMessage})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if hang {
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
		return
	}

	model, _ := body["model"].(string)
	frames := chatFakeFrames(model)
	if sseBody != "" {
		frames = [][]byte{[]byte(sseBody)}
	}
	if embedded != "" {
		payload := []byte("data: " + embedded + "\n\n")
		if embeddedAfter <= 0 {
			_, _ = w.Write(payload)
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		for index, frame := range frames {
			if index >= embeddedAfter {
				break
			}
			_, _ = w.Write(frame)
		}
		_, _ = w.Write(payload)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if truncateAfter > 0 {
		for index, frame := range frames {
			if index >= truncateAfter {
				break
			}
			_, _ = w.Write(frame)
		}
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	for _, frame := range frames {
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	if block {
		<-r.Context().Done()
	}
}

func (f *chatFake) serveRefresh(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.refreshCalls++
	status := f.refreshStatus
	expected := f.expectedRefresh
	token := f.refreshedToken
	refreshToken := f.refreshedRT
	f.mu.Unlock()

	if expected != "" {
		if supplied, _ := body["refresh_token"].(string); supplied != expected {
			writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid refresh token"})
			return
		}
	}
	if status != 0 && status != http.StatusOK {
		writeCatalogJSON(w, status, map[string]any{"message": "refresh rejected"})
		return
	}
	if token == "" {
		writeCatalogJSON(w, http.StatusInternalServerError, map[string]any{"message": "test refresh token missing"})
		return
	}
	if refreshToken == "" {
		refreshToken = "refresh-rotated"
	}
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"data": map[string]any{
			"access_token":    token,
			"refresh_token":   refreshToken,
			"token_type":      "Bearer",
			"expires_at":      time.Now().Add(2 * time.Hour).Unix(),
			"office_identity": "personal",
		},
	})
}

func (f *chatFake) snapshot() (chatCalls, refreshCalls int, bodies []map[string]any, headers []http.Header, auths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatCalls, f.refreshCalls,
		append([]map[string]any(nil), f.bodies...),
		append([]http.Header(nil), f.headers...),
		append([]string(nil), f.auths...)
}

func chatFakeFrames(model string) [][]byte {
	chunks := []map[string]any{
		{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "你"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "好"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
			}},
		},
		{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1,
			"model":   model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5,
			},
		},
	}
	frames := make([][]byte, 0, len(chunks)+1)
	for _, chunk := range chunks {
		encoded, _ := json.Marshal(chunk)
		frames = append(frames, []byte("data: "+string(encoded)+"\n\n"))
	}
	frames = append(frames, []byte("data: [DONE]\n\n"))
	return frames
}

func newChatPool(t *testing.T, cfg Config, uids ...string) (*Pool, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for _, uid := range uids {
		writePoolAuth(t, dir, uid, "", time.Now().Add(time.Hour), nil)
	}
	p := mustPool(t, dir, cfg)
	p.SetCreditLookbackEnabled(false)
	tokens := make(map[string]string, len(uids))
	for _, uid := range uids {
		tokens[uid] = chatAccountToken(t, p, uid)
	}
	return p, tokens
}

func chatAccountToken(t *testing.T, p *Pool, uid string) string {
	t.Helper()
	account := p.Get(uid)
	if account == nil {
		t.Fatalf("account %q not found", uid)
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.Auth.AccessToken
}

func configureChatPool(t *testing.T, p *Pool, baseURL string) {
	t.Helper()
	client := transport.NewClient(transport.ClientOptions{})
	p.SetHTTPClient(NewHTTPClient(client, HTTPClientOptions{BaseURL: baseURL}))
	p.SetTokenManager(p.NewTokenManager(client, TokenManagerOptions{
		BaseURL: baseURL,
		Timeout: 2 * time.Second,
	}))
}

func activeConcurrent(account *Account) int {
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.activeConcurrent
}

func accountDisabled(account *Account) bool {
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.State.Disabled || account.State.ManualDisabled
}

func rawMessage(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw message: %v", err)
	}
	return encoded
}

func chatRequest(raw map[string]any, session string) providers.ChatRequest {
	values := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		encoded := json.RawMessage(value.(json.RawMessage))
		values[key] = encoded
	}
	request := providers.ChatRequest{Raw: values, SessionKey: session}
	if model, ok := raw["model"].(json.RawMessage); ok {
		var text string
		if json.Unmarshal(model, &text) == nil {
			request.Model = text
		}
	}
	return request
}

func chatRaw(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw := make(map[string]any, len(value))
	for key, item := range value {
		raw[key] = rawMessage(t, item)
	}
	return raw
}

func drainChat(t *testing.T, stream providers.ChatStream) [][]byte {
	t.Helper()
	var frames [][]byte
	for {
		frame, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("stream Next: %v", err)
		}
		frames = append(frames, frame)
	}
}

func TestChatForcesStreamAndAllowlist(t *testing.T) {
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-chat")
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	image := "data:image/png;base64,iVBORw0KGgo="
	messages := []map[string]any{
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "what is this?"},
			{"type": "image_url", "image_url": map[string]any{"url": image}},
		}},
	}
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "lookup", "parameters": map[string]any{"type": "object"},
		},
	}}
	payload := chatRaw(t, map[string]any{
		"model":                 "  raccoon-090dd3  ",
		"messages":              messages,
		"tools":                 tools,
		"tool_choice":           "auto",
		"parallel_tool_calls":   true,
		"temperature":           0.2,
		"top_p":                 0.9,
		"n":                     1,
		"stream":                false,
		"stop":                  []string{"END"},
		"max_tokens":            999999,
		"max_completion_tokens": 888888,
		"presence_penalty":      0.1,
		"frequency_penalty":     0.2,
		"logit_bias":            map[string]int{"1": 1},
		"user":                  "user-1",
		"seed":                  7,
		"response_format":       map[string]any{"type": "json_object"},
		"reasoning_effort":      "high",
		"thinking":              map[string]any{"type": "enabled"},
		"enable_thinking":       true,
		"api_key":               "must-not-leak",
		"metadata":              map[string]any{"must": "not-leak"},
		"stream_options":        map[string]any{"include_usage": true},
		"functions":             []map[string]any{{"name": "legacy"}},
	})
	request := chatRequest(payload, "session-1")

	stream, err := p.OpenChat(context.Background(), request)
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := drainChat(t, stream)
	if len(frames) == 0 {
		t.Fatal("stream produced no frames")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, _, bodies, headers, auths := fake.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("chat calls = %d, want 1", len(bodies))
	}
	body := bodies[0]
	if body["stream"] != true {
		t.Fatalf("upstream stream = %v, want true", body["stream"])
	}
	if body["model"] != "raccoon-090dd3" {
		t.Fatalf("upstream model = %v, want trimmed model", body["model"])
	}
	if !reflectDeepEqual(body["messages"], messages) {
		t.Fatalf("messages were not forwarded unchanged: %#v", body["messages"])
	}
	if !reflectDeepEqual(body["tools"], tools) {
		t.Fatalf("tools were not forwarded: %#v", body["tools"])
	}
	for _, key := range []string{
		"temperature", "top_p", "stop", "max_tokens", "max_completion_tokens",
		"presence_penalty", "frequency_penalty", "user", "seed",
		"response_format", "reasoning_effort", "thinking", "enable_thinking",
		"tool_choice", "parallel_tool_calls",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("allowed field %q was dropped: %#v", key, body)
		}
	}
	for _, key := range []string{"api_key", "metadata", "stream_options", "functions", "n", "logit_bias"} {
		if _, ok := body[key]; ok {
			t.Fatalf("disallowed field %q reached upstream", key)
		}
	}
	if rawStream, ok := payload["stream"].(json.RawMessage); !ok || string(rawStream) != "false" {
		t.Fatal("buildChatBody mutated the caller's raw payload")
	}

	header := headers[0]
	for _, key := range []string{
		"Authorization", "Content-Type", "Accept", "X-Client-Platform",
		"X-Client-Version", "X-Raccoon-Language", "Cache-Control",
	} {
		if header.Get(key) == "" {
			t.Fatalf("missing chat header %q in %v", key, header)
		}
	}
	if header.Get("Authorization") != "Bearer "+tokens["u-chat"] {
		t.Fatal("Authorization is not the account bearer")
	}
	if header.Get("Accept") != "text/event-stream" || header.Get("Content-Type") != "application/json" {
		t.Fatalf("chat headers = %v", header)
	}
	if header.Get("X-Client-Platform") != ClientPlatform ||
		header.Get("X-Client-Version") != ClientVersion ||
		header.Get("X-Raccoon-Language") != "zh" ||
		header.Get("Cache-Control") != "no-store" {
		t.Fatalf("desktop identity headers = %v", header)
	}
	if header.Get("X-Org-Code") != "" {
		t.Fatalf("personal identity must not send X-Org-Code: %v", header)
	}
	if auths[0] != tokens["u-chat"] {
		t.Fatal("upstream saw a different bearer token")
	}
}

func TestChatHeaderOrgGate(t *testing.T) {
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-org", "", time.Now().Add(time.Hour), map[string]any{
		"office_identity": "org-code-1",
	})
	p := mustPool(t, dir, Config{Enabled: true, MaxConcurrent: 1})
	p.SetCreditLookbackEnabled(false)
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), "session-org"))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	drainChat(t, stream)
	_ = stream.Close()

	_, _, _, headers, _ := fake.snapshot()
	if len(headers) != 1 || headers[0].Get("X-Org-Code") != "org-code-1" {
		t.Fatalf("org header = %v", headers)
	}

	account := p.Get("u-org")
	account.mu.Lock()
	account.Auth.OfficeIdentity = "personal"
	account.mu.Unlock()
	stream, err = p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), "session-personal"))
	if err != nil {
		t.Fatalf("second OpenChat: %v", err)
	}
	drainChat(t, stream)
	_ = stream.Close()
	_, _, _, headers, _ = fake.snapshot()
	if len(headers) != 2 || headers[1].Get("X-Org-Code") != "" {
		t.Fatalf("personal header = %v", headers)
	}
}

func TestChatBuildBodyValidation(t *testing.T) {
	for name, request := range map[string]providers.ChatRequest{
		"missing model": {
			Raw: map[string]json.RawMessage{
				"messages": rawMessage(t, []map[string]any{{"role": "user", "content": "hi"}}),
			},
		},
		"empty messages": {
			Raw: map[string]json.RawMessage{
				"model":    rawMessage(t, "raccoon-090dd3"),
				"messages": rawMessage(t, []map[string]any{}),
			},
		},
		"non-list messages": {
			Raw: map[string]json.RawMessage{
				"model":    rawMessage(t, "raccoon-090dd3"),
				"messages": rawMessage(t, map[string]any{"role": "user"}),
			},
		},
		"nil raw without model": {},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := buildChatBody(request)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Kind != KindBadRequest || apiErr.Status != http.StatusBadRequest {
				t.Fatalf("buildChatBody error = %v, want 400 APIError", err)
			}
		})
	}

	// Model/Messages fallback when Raw is absent.
	body, model, err := buildChatBody(providers.ChatRequest{
		Model:    " raccoon-090dd3 ",
		Messages: []json.RawMessage{rawMessage(t, map[string]any{"role": "user", "content": "hi"})},
	})
	if err != nil {
		t.Fatalf("fallback buildChatBody: %v", err)
	}
	if model != "raccoon-090dd3" {
		t.Fatalf("fallback model = %q", model)
	}
	if string(body["stream"]) != "true" {
		t.Fatalf("fallback stream = %s", body["stream"])
	}
}

func TestChatSSEAndUsage(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-sse")
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "say hi"}},
	}), "session-sse"))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	account := p.Get("u-sse")
	if got := activeConcurrent(account); got != 1 {
		t.Fatalf("active concurrent after OpenChat = %d, want 1", got)
	}

	frames := drainChat(t, stream)
	if len(frames) != 6 {
		t.Fatalf("frame count = %d, want 6: %q", len(frames), frames)
	}
	blob := bytes.Join(frames, nil)
	if bytes.Count(blob, []byte("[DONE]")) != 1 {
		t.Fatalf("DONE count = %d, want 1", bytes.Count(blob, []byte("[DONE]")))
	}
	for index := 0; index < 5; index++ {
		if !bytes.HasPrefix(frames[index], []byte("data: ")) || !bytes.HasSuffix(frames[index], []byte("\n\n")) {
			t.Fatalf("frame %d is not a complete data frame: %q", index, frames[index])
		}
	}
	if !bytes.Contains(frames[0], []byte(`"role":"assistant"`)) && !bytes.Contains(frames[0], []byte(`"role": "assistant"`)) {
		t.Fatalf("first frame is not the role event: %q", frames[0])
	}
	if !bytes.Contains(frames[1], []byte(`"content":"你"`)) {
		t.Fatalf("content frame = %q", frames[1])
	}
	if !bytes.Contains(frames[3], []byte(`"finish_reason":"stop"`)) {
		t.Fatalf("finish frame = %q", frames[3])
	}
	if !bytes.Contains(frames[4], []byte(`"usage"`)) ||
		!bytes.Contains(frames[4], []byte(`"total_tokens":5`)) {
		t.Fatalf("usage frame = %q", frames[4])
	}
	if string(bytes.TrimSpace(frames[5])) != "data: [DONE]" {
		t.Fatalf("terminal frame = %q", frames[5])
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("active concurrent after drain = %d, want 0", got)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close after drain: %v", err)
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("active concurrent after second close = %d, want 0", got)
	}
}

func TestChatPreservesRawFrames(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-raw-frame")
	raw := "data: {\"id\": \"x\",  \"choices\": [{\"delta\": {\"content\": \"你\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"total_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	fake := &chatFake{sseBody: raw}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := drainChat(t, stream)
	_ = stream.Close()

	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3: %q", len(frames), frames)
	}
	if string(frames[0]) != "data: {\"id\": \"x\",  \"choices\": [{\"delta\": {\"content\": \"你\"}}]}\n\n" {
		t.Fatalf("raw payload was re-encoded: %q", frames[0])
	}
}

func TestStreamCloseReleasesOnce(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-close")
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)
	account := p.Get("u-close")

	open := func() providers.ChatStream {
		t.Helper()
		stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":    "raccoon-090dd3",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}), ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if got := activeConcurrent(account); got != 1 {
			t.Fatalf("active concurrent after open = %d, want 1", got)
		}
		return stream
	}

	// [DONE] is yielded before the credit/EOF bookkeeping. The slot is held until
	// the stream is actually exhausted or closed, matching v1's stream scope.
	stream := open()
	sawDone := false
	for !sawDone {
		frame, err := stream.Next()
		if err != nil {
			t.Fatalf("Next before DONE: %v", err)
		}
		sawDone = bytes.Contains(frame, []byte("[DONE]"))
	}
	if got := activeConcurrent(account); got != 1 {
		t.Fatalf("active concurrent immediately after DONE = %d, want 1 until EOF", got)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after DONE = %v, want io.EOF", err)
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("active concurrent after EOF = %d, want 0", got)
	}
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = stream.Close()
		}()
	}
	wg.Wait()
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("active concurrent after deferred close = %d, want 0", got)
	}
	if !p.Acquire(account) {
		t.Fatal("slot was not returned after DONE")
	}
	p.Release(account)

	// Explicit Close before DONE is also idempotent and releases exactly once.
	stream = open()
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = stream.Close()
		}()
	}
	wg.Wait()
	if err := stream.Close(); err != nil {
		t.Fatalf("repeat Close: %v", err)
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("active concurrent after explicit close = %d, want 0", got)
	}
	if !p.Acquire(account) {
		t.Fatal("slot was not returned after Close")
	}
	p.Release(account)
}

func TestChatDoneFirstFrameReleasesSlot(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-done-first")
	fake := &chatFake{sseBody: "data: [DONE]\n\n"}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	account := p.Get("u-done-first")
	if got := activeConcurrent(account); got != 1 {
		t.Fatalf("active concurrent before first DONE frame = %d, want 1", got)
	}
	frame, err := stream.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(bytes.TrimSpace(frame)) != "data: [DONE]" {
		t.Fatalf("frame = %q", frame)
	}
	if got := activeConcurrent(account); got != 1 {
		t.Fatalf("pending [DONE] released the slot too early: %d", got)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after DONE = %v, want io.EOF", err)
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("EOF after DONE did not release the slot: %d", got)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("deferred Close: %v", err)
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("deferred Close double-released or left the slot: %d", got)
	}
}

func TestChatCredentialsChangedRetriesOnSameSlot(t *testing.T) {
	now := time.Now()
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 2}, "u-cred")
	oldToken := tokens["u-cred"]
	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-cred",
		"exp": now.Add(2 * time.Hour).Unix(),
		"jti": "credential-race-retry",
	})
	fake := &chatFake{statusByToken: map[string]int{oldToken: http.StatusUnauthorized}}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	refreshCalls := 0
	p.chatRefreshHook = func(_ context.Context, account *Account, _ uint64) error {
		refreshCalls++
		if refreshCalls != 1 {
			return nil
		}
		// Simulate a concurrent re-import landing while the refresh flight was
		// in progress: credentials change and the captured generation is stale.
		account.credentialMu.Lock()
		account.mu.Lock()
		account.Auth.AccessToken = newToken
		account.Auth.RefreshToken = "refresh-new"
		account.mu.Unlock()
		account.authGeneration++
		account.credentialMu.Unlock()
		return ErrCredentialsChanged
	}

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat after credential change: %v", err)
	}
	account := p.Get("u-cred")
	// max_concurrent=2: a duplicate Acquire would be visible as 2 here.
	if got := activeConcurrent(account); got != 1 {
		t.Fatalf("active concurrent after retry = %d, want 1 (no duplicate slot)", got)
	}
	frames := drainChat(t, stream)
	_ = stream.Close()
	if len(frames) == 0 || !bytes.Contains(frames[len(frames)-1], []byte("[DONE]")) {
		t.Fatalf("retried stream frames = %q", frames)
	}

	chatCalls, refreshEndpointCalls, _, _, auths := fake.snapshot()
	if chatCalls != 2 || len(auths) != 2 || auths[0] != oldToken || auths[1] != newToken {
		t.Fatalf("chat calls = %d, bearers = %q, want old then new", chatCalls, auths)
	}
	if refreshCalls != 1 || refreshEndpointCalls != 0 {
		t.Fatalf("refresh hook calls = %d, endpoint calls = %d, want 1/0", refreshCalls, refreshEndpointCalls)
	}
	account.mu.Lock()
	gotToken := account.Auth.AccessToken
	account.mu.Unlock()
	if gotToken != newToken {
		t.Fatal("retry did not use the current credentials")
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("active concurrent after DONE = %d, want 0", got)
	}
}

func TestChatCredentialsChangedExhaustionIsAPIError(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-cred-exhaust")
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	ensureCalls := 0
	p.chatEnsureHook = func(_ context.Context, _ *Account) error {
		ensureCalls++
		if ensureCalls <= 2 {
			return ErrCredentialsChanged
		}
		return nil
	}

	_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err == nil {
		t.Fatal("exhausted credential retry unexpectedly succeeded")
	}
	if errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("raw ErrCredentialsChanged reached the caller: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindTransient {
		t.Fatalf("exhaustion error = %v, want transient APIError", err)
	}
	if !strings.Contains(err.Error(), "credentials changed") {
		t.Fatalf("exhaustion message is not readable: %v", err)
	}
	if ensureCalls != 2 {
		t.Fatalf("Ensure calls = %d, want exactly 2 (initial + one retry)", ensureCalls)
	}
	if chatCalls, _, _, _, _ := fake.snapshot(); chatCalls != 0 {
		t.Fatalf("exhausted retry reached upstream %d times", chatCalls)
	}
	if got := activeConcurrent(p.Get("u-cred-exhaust")); got != 0 {
		t.Fatalf("exhausted retry slot = %d, want 0", got)
	}
}

func TestChatAccountDeletedIsTerminal(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-deleted")
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	p.chatEnsureHook = func(_ context.Context, _ *Account) error {
		return ErrAccountDeleted
	}

	_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth {
		t.Fatalf("deleted account error = %v, want auth APIError", err)
	}
	account := p.Get("u-deleted")
	if !accountDisabled(account) {
		t.Fatal("deleted account was not marked terminal")
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("deleted account slot = %d, want 0", got)
	}
	if chatCalls, _, _, _, _ := fake.snapshot(); chatCalls != 0 {
		t.Fatalf("deleted account reached upstream %d times", chatCalls)
	}
}

func TestChat401RefreshRetriesSameAccount(t *testing.T) {
	testChatAuthRefresh(t, http.StatusUnauthorized)
}

func TestChat403RefreshRetriesSameAccount(t *testing.T) {
	testChatAuthRefresh(t, http.StatusForbidden)
}

func testChatAuthRefresh(t *testing.T, rejectedStatus int) {
	t.Helper()
	now := time.Now()
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-auth")
	oldToken := tokens["u-auth"]
	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-auth",
		"exp": now.Add(2 * time.Hour).Unix(),
		"jti": fmt.Sprintf("chat-refresh-%d", rejectedStatus),
	})
	fake := &chatFake{
		statusByToken:   map[string]int{oldToken: rejectedStatus},
		refreshToken:    "refresh-u-auth",
		expectedRefresh: "refresh-u-auth",
		refreshedToken:  newToken,
		refreshedRT:     "refresh-rotated",
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat after %d: %v", rejectedStatus, err)
	}
	frames := drainChat(t, stream)
	_ = stream.Close()
	if len(frames) == 0 || !bytes.Contains(frames[len(frames)-1], []byte("[DONE]")) {
		t.Fatalf("retried stream frames = %q", frames)
	}

	chatCalls, refreshCalls, bodies, _, auths := fake.snapshot()
	if chatCalls != 2 {
		t.Fatalf("chat calls = %d, want 2 (rejected + retry)", chatCalls)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls)
	}
	if auths[0] != oldToken || auths[1] != newToken {
		t.Fatalf("bearer sequence = %q, want [old new]", auths)
	}
	if len(bodies) != 2 || !reflectDeepEqual(bodies[0], bodies[1]) {
		t.Fatal("retry body differs from the first attempt")
	}
	account := p.Get("u-auth")
	account.mu.Lock()
	gotToken := account.Auth.AccessToken
	gotRefresh := account.Auth.RefreshToken
	account.mu.Unlock()
	if gotToken != newToken || gotRefresh != "refresh-rotated" {
		t.Fatalf("account tokens = (%q,%q), want rotated pair", gotToken, gotRefresh)
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("slot after stream = %d, want 0", got)
	}
}

func TestChatEmbeddedErrorDoesNotFailOver(t *testing.T) {
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-embedded-a")
	access := tokens["u-embedded-a"]
	fake := &chatFake{embedded: fmt.Sprintf(`{"error_code":500,"error_msg":"upstream exploded with %s"}`, access)}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), "session-embedded"))
	if stream != nil {
		t.Fatal("embedded error returned a stream")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindUpstream || apiErr.Status != 500 {
		t.Fatalf("embedded error = %v, want upstream 500 APIError", err)
	}
	if strings.Contains(err.Error(), access) {
		t.Fatalf("embedded error leaked the access token: %v", err)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("embedded error message was lost: %v", err)
	}
	chatCalls, _, _, _, _ := fake.snapshot()
	if chatCalls != 1 {
		t.Fatalf("embedded error failover chat calls = %d, want 1", chatCalls)
	}
	if got := activeConcurrent(p.Get("u-embedded-a")); got != 0 {
		t.Fatalf("embedded error slot = %d, want 0", got)
	}
	p.mu.RLock()
	_, bound := p.affinity["session-embedded"]
	p.mu.RUnlock()
	if bound {
		t.Fatal("session was bound before a successful preflight")
	}
}

func TestChatEmbedded401RefreshesAndRetries(t *testing.T) {
	now := time.Now()
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-embedded-auth")
	oldToken := tokens["u-embedded-auth"]
	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-embedded-auth",
		"exp": now.Add(2 * time.Hour).Unix(),
		"jti": "chat-embedded-401",
	})
	fake := &chatFake{
		embeddedByToken: map[string]string{
			oldToken: `{"error_code":401,"error_msg":"token expired"}`,
		},
		refreshToken:    "refresh-u-embedded-auth",
		expectedRefresh: "refresh-u-embedded-auth",
		refreshedToken:  newToken,
		refreshedRT:     "refresh-rotated",
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat embedded 401: %v", err)
	}
	drainChat(t, stream)
	_ = stream.Close()

	chatCalls, refreshCalls, _, _, auths := fake.snapshot()
	if chatCalls != 2 || refreshCalls != 1 {
		t.Fatalf("calls = chat %d refresh %d, want 2/1", chatCalls, refreshCalls)
	}
	if auths[0] != oldToken || auths[1] != newToken {
		t.Fatalf("bearer sequence = %q", auths)
	}
	if got := activeConcurrent(p.Get("u-embedded-auth")); got != 0 {
		t.Fatalf("slot = %d, want 0", got)
	}
}

func TestChatSecondAuthFailureDisables(t *testing.T) {
	now := time.Now()
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-terminal")
	oldToken := tokens["u-terminal"]
	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-terminal",
		"exp": now.Add(2 * time.Hour).Unix(),
		"jti": "chat-terminal",
	})
	fake := &chatFake{
		statusByToken:   map[string]int{oldToken: http.StatusUnauthorized, newToken: http.StatusUnauthorized},
		refreshToken:    "refresh-u-terminal",
		expectedRefresh: "refresh-u-terminal",
		refreshedToken:  newToken,
		refreshedRT:     "refresh-rotated",
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if stream != nil {
		t.Fatal("terminal auth failure returned a stream")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth {
		t.Fatalf("terminal error = %v, want auth APIError", err)
	}
	chatCalls, refreshCalls, _, _, _ := fake.snapshot()
	if chatCalls != 2 || refreshCalls != 1 {
		t.Fatalf("calls = chat %d refresh %d, want 2/1", chatCalls, refreshCalls)
	}
	account := p.Get("u-terminal")
	if !accountDisabled(account) {
		t.Fatal("account was not terminally disabled")
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("slot = %d, want 0", got)
	}
}

func TestChatFailoverOnTransient(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-fail-a", "u-fail-b")
			fake := &chatFake{
				statusByToken: map[string]int{tokens["u-fail-a"]: status},
				errorMessage:  "upstream said " + tokens["u-fail-a"],
			}
			server := httptest.NewServer(fake)
			defer server.Close()
			configureChatPool(t, p, server.URL)

			stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
				"model":    "raccoon-090dd3",
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			}), ""))
			if err != nil {
				t.Fatalf("failover OpenChat: %v", err)
			}
			frames := drainChat(t, stream)
			_ = stream.Close()
			if len(frames) == 0 {
				t.Fatal("failover stream was empty")
			}
			chatCalls, _, _, _, _ := fake.snapshot()
			if chatCalls != 2 {
				t.Fatalf("chat calls = %d, want 2", chatCalls)
			}
			first := p.Get("u-fail-a")
			second := p.Get("u-fail-b")
			first.mu.Lock()
			cooling := first.State.CoolUntil.After(time.Now())
			lastError := first.State.LastError
			first.mu.Unlock()
			if !cooling {
				t.Fatal("transient failure did not cool the first account")
			}
			if lastError == "" {
				t.Fatal("transient failure did not record a readable error")
			}
			if strings.Contains(lastError, tokens["u-fail-a"]) {
				t.Fatalf("account error leaked a token: %q", lastError)
			}
			if activeConcurrent(first) != 0 || activeConcurrent(second) != 0 {
				t.Fatal("failover did not release both slots")
			}
		})
	}
}

func TestChatAuthFailureFailsOverToSecondAccount(t *testing.T) {
	now := time.Now()
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-auth-fail-a", "u-auth-fail-b")
	oldToken := tokens["u-auth-fail-a"]
	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-auth-fail-a",
		"exp": now.Add(2 * time.Hour).Unix(),
		"jti": "chat-auth-failover",
	})
	fake := &chatFake{
		statusByToken: map[string]int{
			oldToken: http.StatusUnauthorized,
			newToken: http.StatusUnauthorized,
		},
		refreshToken:    "refresh-u-auth-fail-a",
		expectedRefresh: "refresh-u-auth-fail-a",
		refreshedToken:  newToken,
		refreshedRT:     "refresh-rotated",
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("auth failover OpenChat: %v", err)
	}
	frames := drainChat(t, stream)
	_ = stream.Close()
	if len(frames) == 0 {
		t.Fatal("auth failover stream was empty")
	}
	chatCalls, refreshCalls, _, _, auths := fake.snapshot()
	if chatCalls != 3 || refreshCalls != 1 {
		t.Fatalf("calls = chat %d refresh %d, want 3/1", chatCalls, refreshCalls)
	}
	if len(auths) != 3 || auths[0] != oldToken || auths[1] != newToken ||
		auths[2] != tokens["u-auth-fail-b"] {
		t.Fatalf("bearer sequence = %q", auths)
	}
	if !accountDisabled(p.Get("u-auth-fail-a")) {
		t.Fatal("auth-failed account was not terminally disabled")
	}
	if got := activeConcurrent(p.Get("u-auth-fail-b")); got != 0 {
		t.Fatalf("second account slot = %d, want 0", got)
	}
}

func TestChatEmbeddedUpstreamFailsOver(t *testing.T) {
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-emb-fail-a", "u-emb-fail-b")
	fake := &chatFake{
		embeddedByToken: map[string]string{
			tokens["u-emb-fail-a"]: `{"error_code":500,"error_msg":"boom"}`,
		},
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("embedded failover OpenChat: %v", err)
	}
	frames := drainChat(t, stream)
	_ = stream.Close()
	if len(frames) == 0 {
		t.Fatal("embedded failover stream was empty")
	}
	chatCalls, _, _, _, auths := fake.snapshot()
	if chatCalls != 2 || len(auths) != 2 || auths[0] != tokens["u-emb-fail-a"] ||
		auths[1] != tokens["u-emb-fail-b"] {
		t.Fatalf("embedded failover calls = %d, bearers = %q", chatCalls, auths)
	}
	first := p.Get("u-emb-fail-a")
	first.mu.Lock()
	cooling := first.State.CoolUntil.After(time.Now())
	first.mu.Unlock()
	if !cooling {
		t.Fatal("embedded upstream failure did not cool the first account")
	}
}

func TestChatNetworkErrorIsTransient(t *testing.T) {
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-network")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	configureChatPool(t, p, "http://"+address)

	_, err = p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindTransient || apiErr.Status != 0 {
		t.Fatalf("network error = %v, want transient APIError", err)
	}
	if !strings.Contains(err.Error(), "cannot reach upstream") {
		t.Fatalf("network message = %v", err)
	}
	if strings.Contains(err.Error(), tokens["u-network"]) {
		t.Fatalf("network error leaked the access token: %v", err)
	}
	account := p.Get("u-network")
	account.mu.Lock()
	cooling := account.State.CoolUntil.After(time.Now())
	account.mu.Unlock()
	if !cooling {
		t.Fatal("network failure did not cool the account")
	}
	if got := activeConcurrent(account); got != 0 {
		t.Fatalf("network failure slot = %d, want 0", got)
	}
}

func TestChatBadRequestDoesNotFailOver(t *testing.T) {
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-bad-a", "u-bad-b")
	fake := &chatFake{
		statusByToken: map[string]int{tokens["u-bad-a"]: http.StatusBadRequest},
		errorMessage:  "bad chat body",
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if stream != nil {
		t.Fatal("400 returned a stream")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindBadRequest || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("400 error = %v", err)
	}
	chatCalls, _, _, _, _ := fake.snapshot()
	if chatCalls != 1 {
		t.Fatalf("400 chat calls = %d, want 1", chatCalls)
	}
	first := p.Get("u-bad-a")
	second := p.Get("u-bad-b")
	first.mu.Lock()
	cooling := first.State.CoolUntil.After(time.Now())
	first.mu.Unlock()
	if cooling {
		t.Fatal("400 must not cool the account")
	}
	if activeConcurrent(first) != 0 || activeConcurrent(second) != 0 {
		t.Fatal("400 did not release the slot")
	}
}

func TestEmbeddedStreamErrorMessagePreservesCodeOnly(t *testing.T) {
	for frame, want := range map[string]string{
		`{"error_code":500}`:     "error_code=500",
		`{"code":500}`:           "error_code=500",
		`{"error_code":"500"}`:   "error_code=500",
		`{"code":"bad_request"}`: "error_code=bad_request",
	} {
		t.Run(want, func(t *testing.T) {
			err := embeddedStreamError([]byte(frame), nil)
			if err == nil {
				t.Fatalf("%s produced no error", frame)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s message = %v, want %q", frame, err, want)
			}
			if strings.Contains(err.Error(), "<redacted>") {
				t.Fatalf("code-only message was redacted into useless text: %v", err)
			}
		})
	}

	secret := "opaque-secret-code-value"
	err := embeddedStreamError([]byte(`{"code":"`+secret+`"}`), []string{secret})
	if err == nil {
		t.Fatal("secret code produced no error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("code-only error leaked the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("secret code was not redacted: %v", err)
	}

	err = embeddedStreamError([]byte(`{"error_code":500,"error_msg":"leak `+secret+`"}`), []string{secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("message redaction = %v", err)
	}
}

func TestChatClassifiesHTTPStatusWhenErrorBodyReadFails(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-short-body")
	account := p.Get("u-short-body")

	for status, wantKind := range map[int]ErrorKind{
		http.StatusBadRequest:          KindBadRequest,
		http.StatusUnauthorized:        KindAuth,
		http.StatusForbidden:           KindAuth,
		http.StatusTooManyRequests:     KindRateLimited,
		http.StatusInternalServerError: KindUpstream,
		http.StatusServiceUnavailable:  KindUpstream,
	} {
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Promise more bytes than are written, then close. The client
				// receives the status/headers but io.ReadAll fails with an
				// unexpected EOF, exercising the status-first classification.
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"truncated`))
			}))
			defer server.Close()
			configureChatPool(t, p, server.URL)

			stream, err := p.sendChatRequest(
				context.Background(), account, p.HTTPClient(), http.Header{},
				[]byte(`{"model":"raccoon-090dd3"}`), "raccoon-090dd3", nil,
				Config{StreamTimeoutSeconds: 5},
			)
			if stream != nil {
				_ = stream.Close()
				t.Fatal("HTTP error status returned a stream")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v, want APIError", err)
			}
			if apiErr.Kind != wantKind || apiErr.Status != status {
				t.Fatalf("error = kind %v status %d, want kind %v status %d",
					apiErr.Kind, apiErr.Status, wantKind, status)
			}
		})
	}
}

func TestChatEmbedded400DoesNotFailOver(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-emb-bad-a", "u-emb-bad-b")
	fake := &chatFake{embedded: `{"code":"invalid_request_error","message":"bad input"}`}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindBadRequest || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("embedded 400 error = %v", err)
	}
	chatCalls, _, _, _, _ := fake.snapshot()
	if chatCalls != 1 {
		t.Fatalf("embedded 400 chat calls = %d, want 1", chatCalls)
	}
}

func TestChatMidStreamEmbeddedError(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-mid")
	fake := &chatFake{
		embedded:      `{"error_code":500,"error_msg":"died after the first chunk"}`,
		embeddedAfter: 1,
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	first, err := stream.Next()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if !bytes.Contains(first, []byte("assistant")) {
		t.Fatalf("first frame = %q", first)
	}
	_, err = stream.Next()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindUpstream {
		t.Fatalf("mid-stream error = %v", err)
	}
	if !strings.Contains(err.Error(), "died after") {
		t.Fatalf("mid-stream message = %v", err)
	}
	if got := activeConcurrent(p.Get("u-mid")); got != 0 {
		t.Fatalf("slot after mid-stream error = %d, want 0", got)
	}
}

func TestChatTruncatedStreamWithoutDone(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-truncated")
	fake := &chatFake{truncateAfter: 2}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	var frames [][]byte
	var streamErr error
	for {
		frame, err := stream.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
		frames = append(frames, frame)
	}
	if len(frames) != 2 {
		t.Fatalf("truncated frames = %d, want 2", len(frames))
	}
	var apiErr *APIError
	if !errors.As(streamErr, &apiErr) || apiErr.Kind != KindUpstream {
		t.Fatalf("truncated error = %v, want upstream", streamErr)
	}
	if got := activeConcurrent(p.Get("u-truncated")); got != 0 {
		t.Fatalf("truncated slot = %d, want 0", got)
	}
}

func TestChatContextCancel(t *testing.T) {
	t.Run("after headers", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-cancel")
		fake := &chatFake{
			sseBody:           "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n",
			blockAfterHeaders: true,
		}
		server := httptest.NewServer(fake)
		defer server.Close()
		configureChatPool(t, p, server.URL)

		ctx, cancel := context.WithCancel(context.Background())
		stream, err := p.OpenChat(ctx, chatRequest(chatRaw(t, map[string]any{
			"model":    "raccoon-090dd3",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}), ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		if _, err := stream.Next(); err != nil {
			t.Fatalf("first frame: %v", err)
		}
		cancel()
		_, err = stream.Next()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next after cancel = %v, want context.Canceled", err)
		}
		if got := activeConcurrent(p.Get("u-cancel")); got != 0 {
			t.Fatalf("slot after cancel = %d, want 0", got)
		}
	})

	t.Run("during preflight", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-cancel-pre")
		started := make(chan struct{})
		fake := &chatFake{chatStarted: started, hang: true}
		server := httptest.NewServer(fake)
		defer server.Close()
		configureChatPool(t, p, server.URL)

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := p.OpenChat(ctx, chatRequest(chatRaw(t, map[string]any{
				"model":    "raccoon-090dd3",
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			}), ""))
			result <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("chat handler did not start")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("OpenChat after cancel = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("OpenChat did not return after cancellation")
		}
		if got := activeConcurrent(p.Get("u-cancel-pre")); got != 0 {
			t.Fatalf("preflight cancel slot = %d, want 0", got)
		}
	})
}

func TestChatDisabledRejects(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: false, MaxConcurrent: 1}, "u-disabled")
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindBadRequest || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("disabled error = %v, want 400 APIError", err)
	}
	if !strings.Contains(err.Error(), "Raccoon 出口未启用") {
		t.Fatalf("disabled message = %v", err)
	}
	if chatCalls, _, _, _, _ := fake.snapshot(); chatCalls != 0 {
		t.Fatalf("disabled provider reached upstream %d times", chatCalls)
	}
}

func TestChatNoAccountReasons(t *testing.T) {
	t.Run("empty pool", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1})
		_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":    "raccoon-090dd3",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}), ""))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("empty pool error = %v, want 503", err)
		}
		if !strings.Contains(err.Error(), "没有可用的 Raccoon 账号") ||
			!strings.Contains(err.Error(), "导入") {
			t.Fatalf("empty pool message = %v", err)
		}
	})

	t.Run("disabled account", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-reason")
		if err := p.Disable("u-reason", "manual reason"); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":    "raccoon-090dd3",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}), ""))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("disabled account error = %v, want 503", err)
		}
		if !strings.Contains(err.Error(), "已停用") || !strings.Contains(err.Error(), "manual reason") {
			t.Fatalf("disabled account message = %v", err)
		}
	})

	t.Run("cooling account", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-cooling")
		account := p.Get("u-cooling")
		account.mu.Lock()
		account.State.CoolUntil = time.Now().Add(time.Minute)
		account.State.LastError = "upstream break"
		account.mu.Unlock()
		_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":    "raccoon-090dd3",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}), ""))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("cooling account error = %v, want 503", err)
		}
		if !strings.Contains(err.Error(), "冷却中") || !strings.Contains(err.Error(), "upstream break") {
			t.Fatalf("cooling account message = %v", err)
		}
	})

	t.Run("busy account", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-busy")
		account := p.Get("u-busy")
		if !p.Acquire(account) {
			t.Fatal("could not pre-acquire the only slot")
		}
		defer p.Release(account)
		_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":    "raccoon-090dd3",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}), ""))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("busy account error = %v, want 503", err)
		}
		if !strings.Contains(err.Error(), "并发已满") {
			t.Fatalf("busy account message = %v", err)
		}
	})
}

func TestChatSessionBindOnlyAfterSuccess(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-bind")
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), "session-bind"))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	drainChat(t, stream)
	_ = stream.Close()
	p.mu.RLock()
	bound := p.affinity["session-bind"]
	p.mu.RUnlock()
	if bound != "u-bind" {
		t.Fatalf("affinity = %q, want u-bind", bound)
	}
}

func TestChatClampsOutputTokens(t *testing.T) {
	t.Run("catalog limit and numeric normalization", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-clamp")
		account := p.Get("u-clamp")
		account.mu.Lock()
		account.Models = []providers.Model{{"id": "raccoon-090dd3", "max_tokens": int64(4096)}}
		account.mu.Unlock()
		fake := &chatFake{}
		server := httptest.NewServer(fake)
		defer server.Close()
		configureChatPool(t, p, server.URL)

		stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":                 "raccoon-090dd3",
			"messages":              []map[string]any{{"role": "user", "content": "hi"}},
			"max_tokens":            "9999",
			"max_completion_tokens": 1.9,
		}), ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		drainChat(t, stream)
		_ = stream.Close()

		_, _, bodies, _, _ := fake.snapshot()
		if len(bodies) != 1 {
			t.Fatalf("chat bodies = %d", len(bodies))
		}
		if bodies[0]["max_tokens"] != float64(4096) {
			t.Fatalf("max_tokens = %#v, want 4096", bodies[0]["max_tokens"])
		}
		if bodies[0]["max_completion_tokens"] != float64(1) {
			t.Fatalf("max_completion_tokens = %#v, want 1", bodies[0]["max_completion_tokens"])
		}
	})

	t.Run("shared cache limit", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-shared-limit")
		p.catalogMu.Lock()
		p.catalog = catalogCache{models: []providers.Model{
			{"id": "custom-shared", "max_tokens": int64(2048)},
		}}
		p.catalogMu.Unlock()
		fake := &chatFake{}
		server := httptest.NewServer(fake)
		defer server.Close()
		configureChatPool(t, p, server.URL)
		stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":      "custom-shared",
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
			"max_tokens": 9999,
		}), ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		drainChat(t, stream)
		_ = stream.Close()
		_, _, bodies, _, _ := fake.snapshot()
		if len(bodies) != 1 || bodies[0]["max_tokens"] != float64(2048) {
			t.Fatalf("shared cache clamp body = %#v", bodies)
		}
	})

	t.Run("seed limit", func(t *testing.T) {
		p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-seed-limit")
		fake := &chatFake{}
		server := httptest.NewServer(fake)
		defer server.Close()
		configureChatPool(t, p, server.URL)
		stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
			"model":                 "sn-glm-5-3",
			"messages":              []map[string]any{{"role": "user", "content": "hi"}},
			"max_tokens":            200000,
			"max_completion_tokens": 200000,
		}), ""))
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		drainChat(t, stream)
		_ = stream.Close()
		_, _, bodies, _, _ := fake.snapshot()
		if len(bodies) != 1 || bodies[0]["max_tokens"] != float64(100000) ||
			bodies[0]["max_completion_tokens"] != float64(100000) {
			t.Fatalf("seed clamp body = %#v", bodies)
		}
	})

	for name, invalid := range map[string]any{
		"bool":        true,
		"zero":        0,
		"negative":    -5,
		"string":      "abc",
		"float-zero":  0.0,
		"object":      map[string]any{"value": 1},
		"negative-fp": -0.5,
	} {
		t.Run("invalid "+name, func(t *testing.T) {
			p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-invalid")
			fake := &chatFake{}
			server := httptest.NewServer(fake)
			defer server.Close()
			configureChatPool(t, p, server.URL)

			_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
				"model":      "raccoon-090dd3",
				"messages":   []map[string]any{{"role": "user", "content": "hi"}},
				"max_tokens": invalid,
			}), ""))
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Kind != KindBadRequest || apiErr.Status != http.StatusBadRequest {
				t.Fatalf("invalid max_tokens error = %v", err)
			}
			if chatCalls, _, _, _, _ := fake.snapshot(); chatCalls != 0 {
				t.Fatalf("invalid max_tokens reached upstream %d times", chatCalls)
			}
			if got := activeConcurrent(p.Get("u-invalid")); got != 0 {
				t.Fatalf("invalid max_tokens slot = %d, want 0", got)
			}
		})
	}
}

func TestChatUsesAccountProxy(t *testing.T) {
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-proxy-chat", "", time.Now().Add(time.Hour), nil)
	p := mustPool(t, dir, Config{Enabled: true, MaxConcurrent: 1})
	p.SetCreditLookbackEnabled(false)

	target := &chatFake{}
	targetServer := httptest.NewServer(target)
	defer targetServer.Close()
	configureChatPool(t, p, targetServer.URL)

	var proxyHits int32
	var proxyMu sync.Mutex
	proxyPath := ""
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		proxyMu.Lock()
		proxyPath = r.URL.Path
		proxyMu.Unlock()
		frames := chatFakeFrames("raccoon-090dd3")
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			_, _ = w.Write(frame)
		}
	}))
	defer proxy.Close()

	p.SetProxySlots([]transport.Slot{{ID: "slot-chat", URL: proxy.URL, Enabled: true}})
	if err := p.SetProxySlot("u-proxy-chat", "slot-chat"); err != nil {
		t.Fatalf("SetProxySlot: %v", err)
	}

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat through proxy: %v", err)
	}
	drainChat(t, stream)
	_ = stream.Close()

	if hits := atomic.LoadInt32(&proxyHits); hits != 1 {
		t.Fatalf("proxy hits = %d, want 1", hits)
	}
	proxyMu.Lock()
	gotProxyPath := proxyPath
	proxyMu.Unlock()
	if gotProxyPath != ChatPath {
		t.Fatalf("proxy path = %q, want %q", gotProxyPath, ChatPath)
	}
	if chatCalls, _, _, _, _ := target.snapshot(); chatCalls != 0 {
		t.Fatalf("direct target was hit %d times despite an account proxy", chatCalls)
	}
}

func TestEmbeddedStreamErrorMapping(t *testing.T) {
	secret := makeUnsignedJWT(t, map[string]any{"sub": "u-map", "exp": time.Now().Add(time.Hour).Unix()})
	cases := []struct {
		name   string
		frame  string
		kind   ErrorKind
		status int
		empty  bool
	}{
		{name: "role chunk", frame: `{"choices":[{"delta":{"role":"assistant"}}]}`, empty: true},
		{name: "usage chunk", frame: `{"choices":[],"usage":{"total_tokens":1}}`, empty: true},
		{name: "normal zero code", frame: `{"code":0,"message":"success","choices":[{"delta":{}}]}`, empty: true},
		{name: "normal zero string code", frame: `{"error_code":"0","choices":[{"delta":{}}]}`, empty: true},
		{name: "invalid json", frame: `not json`, empty: true},
		{name: "status 400", frame: `{"status":400,"message":"bad"}`, kind: KindBadRequest, status: 400},
		{name: "status 401", frame: `{"status_code":401,"error_msg":"expired"}`, kind: KindAuth, status: 401},
		{name: "error type 403", frame: `{"error":{"message":"forbidden","type":"403"}}`, kind: KindAuth, status: 403},
		{name: "code 429", frame: `{"error_code":"429","error_msg":"slow down"}`, kind: KindRateLimited, status: 429},
		{name: "error code 500", frame: `{"error":{"message":"boom","code":500}}`, kind: KindUpstream, status: 500},
		{name: "bad request code", frame: `{"code":"invalid_request_error","message":"bad input"}`, kind: KindBadRequest, status: 400},
		{name: "message only", frame: `{"message":"boom"}`, kind: KindUpstream, status: 0},
		{name: "error string", frame: `{"error":"boom"}`, kind: KindUpstream, status: 0},
		{name: "redaction", frame: fmt.Sprintf(`{"error_code":500,"error_msg":"leak %s"}`, secret), kind: KindUpstream, status: 500},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := embeddedStreamError([]byte(testCase.frame), []string{secret})
			if testCase.empty {
				if err != nil {
					t.Fatalf("normal frame reported an error: %v", err)
				}
				return
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v, want APIError", err)
			}
			if apiErr.Kind != testCase.kind || apiErr.Status != testCase.status {
				t.Fatalf("error = kind %v status %d, want kind %v status %d",
					apiErr.Kind, apiErr.Status, testCase.kind, testCase.status)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("embedded error leaked the secret: %v", err)
			}
		})
	}
}

func TestChatEnsuresTokenBeforeRequest(t *testing.T) {
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-ensure", "", time.Now().Add(30*time.Second), nil)
	p := mustPool(t, dir, Config{Enabled: true, MaxConcurrent: 1})
	p.SetCreditLookbackEnabled(false)
	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-ensure",
		"exp": time.Now().Add(2 * time.Hour).Unix(),
		"jti": "ensure-before-chat",
	})
	fake := &chatFake{
		refreshToken:    "refresh-u-ensure",
		expectedRefresh: "refresh-u-ensure",
		refreshedToken:  newToken,
		refreshedRT:     "refresh-rotated",
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	drainChat(t, stream)
	_ = stream.Close()

	chatCalls, refreshCalls, _, _, auths := fake.snapshot()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if chatCalls != 1 || len(auths) != 1 || auths[0] != newToken {
		t.Fatalf("chat used %v after refresh (calls %d)", auths, chatCalls)
	}
}

func TestChatSessionAffinityAfterFailover(t *testing.T) {
	p, tokens := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-aff-a", "u-aff-b")
	fake := &chatFake{statusByToken: map[string]int{tokens["u-aff-a"]: http.StatusInternalServerError}}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	body := func() providers.ChatRequest {
		return chatRequest(chatRaw(t, map[string]any{
			"model":    "raccoon-090dd3",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}), "session-affinity")
	}
	for attempt := 0; attempt < 2; attempt++ {
		stream, err := p.OpenChat(context.Background(), body())
		if err != nil {
			t.Fatalf("OpenChat %d: %v", attempt, err)
		}
		drainChat(t, stream)
		_ = stream.Close()
	}

	chatCalls, _, _, _, auths := fake.snapshot()
	if chatCalls != 3 {
		t.Fatalf("chat calls = %d, want 3 (a fails, then b twice)", chatCalls)
	}
	if len(auths) != 3 || auths[0] != tokens["u-aff-a"] ||
		auths[1] != tokens["u-aff-b"] || auths[2] != tokens["u-aff-b"] {
		t.Fatalf("bearer sequence = %q", auths)
	}
}

func TestChatStreamTimeoutFromConfig(t *testing.T) {
	p, _ := newChatPool(t, Config{
		Enabled:              true,
		MaxConcurrent:        1,
		StreamTimeoutSeconds: 0.1,
	}, "u-timeout")
	started := make(chan struct{})
	fake := &chatFake{chatStarted: started, hang: true}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	start := time.Now()
	_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err == nil {
		t.Fatal("hanging stream did not time out")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindTransient || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v, want transient timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stream timeout took %v, want about 100ms", elapsed)
	}
	if got := activeConcurrent(p.Get("u-timeout")); got != 0 {
		t.Fatalf("timeout slot = %d, want 0", got)
	}
	select {
	case <-started:
	default:
		t.Fatal("timeout test never reached the handler")
	}
}

func TestChatNilInputs(t *testing.T) {
	var p *Pool
	if _, err := p.OpenChat(context.Background(), providers.ChatRequest{}); err == nil {
		t.Fatal("nil pool did not error")
	}
	p2, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1})
	if _, err := p2.OpenChat(nil, providers.ChatRequest{}); err == nil {
		t.Fatal("nil context did not error")
	}
}

func TestChatEmitsPostDoneCreditUsageFromBill(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-credit")
	p.SetCreditLookbackEnabled(true)
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	first := map[string]any{
		"bill_id":    "bill-one",
		"points":     -2,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	fake.setBillItems([]map[string]any{first})

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := drainChat(t, stream)
	usage, ok := postDoneUsage(t, frames)
	if !ok || usage["credit"] != float64(2) {
		t.Fatalf("post-DONE usage = %#v (ok=%v), want credit 2", usage, ok)
	}

	// The first bill is now marked seen. A new negative bill appears while the
	// old one remains on the page; only the new one may pay for this request.
	second := map[string]any{
		"bill_id":    "bill-two",
		"points":     -1,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	fake.setBillItems([]map[string]any{first, second})
	stream, err = p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("second OpenChat: %v", err)
	}
	usage, ok = postDoneUsage(t, drainChat(t, stream))
	if !ok || usage["credit"] != float64(1) {
		t.Fatalf("second post-DONE usage = %#v (ok=%v), want credit 1", usage, ok)
	}
}

func TestChatPostDoneCreditAmbiguousDoesNotGuess(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-credit-ambiguous")
	p.SetCreditLookbackEnabled(true)
	fake := &chatFake{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	fake.setBillItems([]map[string]any{
		{"bill_id": "bill-a", "points": -2, "created_at": now},
		{"bill_id": "bill-b", "points": -3, "created_at": now},
	})
	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := drainChat(t, stream)
	if usage, ok := postDoneUsage(t, frames); ok {
		t.Fatalf("ambiguous bills produced a guess: %#v", usage)
	}
}

func postDoneUsage(t *testing.T, frames [][]byte) (map[string]any, bool) {
	t.Helper()
	seenDone := false
	for _, frame := range frames {
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(frame)), "data:"))
		if data == "[DONE]" {
			seenDone = true
			continue
		}
		if !seenDone || data == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}
		usage, ok := payload["usage"].(map[string]any)
		if ok && len(usage) > 0 {
			return usage, true
		}
	}
	return nil, false
}

func TestReservePrewarmBlocksSpendWhenBalanceUnknown(t *testing.T) {
	p, _ := newChatPool(t, Config{Enabled: true, MaxConcurrent: 1}, "u-reserve")
	p.SetCreditLookbackEnabled(false)
	fake := &chatFake{
		balance: map[string]any{"available_points": 5},
	}
	fake.setBillItems([]map[string]any{})
	server := httptest.NewServer(fake)
	defer server.Close()
	configureChatPool(t, p, server.URL)

	account := p.Get("u-reserve")
	account.mu.Lock()
	account.ReserveCredits = 10
	account.Credits = nil
	account.mu.Unlock()

	_, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err == nil || !strings.Contains(err.Error(), "预留") {
		t.Fatalf("reserve guard error = %v, want low-reserve explanation", err)
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 0 {
		t.Fatalf("reserve guard spent %d upstream chat calls", calls)
	}
	if account.Credits == nil {
		t.Fatal("prewarm did not fetch the unknown balance")
	}
}

func TestChatStreamPerReadIdleTimeoutDoesNotCutActiveStream(t *testing.T) {
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-long-stream", "", time.Now().Add(time.Hour), nil)
	p := mustPool(t, dir, Config{Enabled: true, MaxConcurrent: 1, StreamTimeoutSeconds: 0.15})
	p.SetCreditLookbackEnabled(false)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for index := 0; index < 6; index++ {
			payload := fmt.Sprintf(
				`data: {"id":"chunk-%d","choices":[{"index":0,"delta":{"content":"%d"}}]}%s`,
				index, index, "\n\n",
			)
			_, _ = w.Write([]byte(payload))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(40 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	configureChatPool(t, p, upstream.URL)

	stream, err := p.OpenChat(context.Background(), chatRequest(chatRaw(t, map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}), ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := drainChat(t, stream)
	content := string(bytes.Join(frames, nil))
	for index := 0; index < 6; index++ {
		if !strings.Contains(content, fmt.Sprintf(`"content":"%d"`, index)) {
			t.Fatalf("frame %d missing from active stream: %s", index, content)
		}
	}
	if !strings.Contains(content, "[DONE]") {
		t.Fatalf("active stream was cut before DONE: %s", content)
	}
}

func TestCreditSeenBillsBounded(t *testing.T) {
	account := &Account{}
	fetched := make([]string, 0, creditSeenBillLimit+50)
	for index := 0; index < creditSeenBillLimit+50; index++ {
		fetched = append(fetched, fmt.Sprintf("bill-%d", index))
	}
	account.rememberCreditBills(fetched, nil)
	account.creditBillMu.Lock()
	size := len(account.creditSeenBills)
	account.creditBillMu.Unlock()
	if size > creditSeenBillLimit {
		t.Fatalf("seen bill set = %d, want <= %d", size, creditSeenBillLimit)
	}
}

func reflectDeepEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	var leftValue, rightValue any
	if json.Unmarshal(leftJSON, &leftValue) != nil || json.Unmarshal(rightJSON, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
