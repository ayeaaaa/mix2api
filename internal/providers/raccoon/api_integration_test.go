package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func passTestAuth(next http.Handler) http.Handler { return next }

// TestGenericAPIWithRealRaccoonProvider is the cheap end-to-end wiring check:
// the generic API resolves/validates a model against the real Raccoon catalog
// and consumes the real Raccoon SSE stream from a fake upstream.
func TestGenericAPIWithRealRaccoonProvider(t *testing.T) {
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-api", "", poolExpiry(time.Now()), nil)
	pool := mustPool(t, dir, Config{Enabled: true, MaxConcurrent: 1})

	fake := &chatFake{}
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	configureChatPool(t, pool, upstream.URL)

	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"raccoon": map[string]any{"enabled": true, "max_concurrent": 1},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	provider := &Provider{
		store:       store,
		accountsDir: dir,
		pool:        pool,
		baseURL:     upstream.URL,
		now:         time.Now,
	}
	provider.SetCreditLookbackEnabled(false)
	srv := api.NewServer(api.ServerOptions{
		Providers: []providers.Provider{provider},
		PanelAuth: passTestAuth, APIKeyAuth: passTestAuth,
	})

	requestBody := `{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hello"}],"stream":%s}`
	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{name: "nonstream", stream: false},
		{name: "stream", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamValue := "false"
			if tc.stream {
				streamValue = "true"
			}
			body := strings.Replace(requestBody, "%s", streamValue, 1)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?realm=raccoon", strings.NewReader(body))
			srv.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if tc.stream {
				if got := rec.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
					t.Fatalf("content type = %q", got)
				}
				if !strings.Contains(rec.Body.String(), `"content":"你"`) ||
					!strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
					t.Fatalf("stream body = %q", rec.Body)
				}
				return
			}
			var completion map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			if completion["object"] != "chat.completion" {
				t.Fatalf("completion = %#v", completion)
			}
			choices := completion["choices"].([]any)
			message := choices[0].(map[string]any)["message"].(map[string]any)
			if message["content"] != "你好" {
				t.Fatalf("aggregated content = %#v", message["content"])
			}
			usage := completion["usage"].(map[string]any)
			if usage["total_tokens"] != float64(5) {
				t.Fatalf("usage = %#v", usage)
			}
		})
	}
}

func TestGenericAPIMapsRaccoonDisabledError(t *testing.T) {
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-api", "", poolExpiry(time.Now()), nil)
	pool := mustPool(t, dir, Config{Enabled: false, MaxConcurrent: 1})

	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"raccoon": map[string]any{"enabled": false, "max_concurrent": 1},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	provider := &Provider{
		store:       store,
		accountsDir: dir,
		pool:        pool,
		now:         time.Now,
	}
	srv := api.NewServer(api.ServerOptions{
		Providers: []providers.Provider{provider},
		PanelAuth: passTestAuth, APIKeyAuth: passTestAuth,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?realm=raccoon",
		strings.NewReader(`{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hi"}]}`))
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["error"]["type"] != "invalid_request_error" || body["error"]["code"] != float64(http.StatusBadRequest) {
		t.Fatalf("envelope = %#v", body)
	}
	if message, _ := body["error"]["message"].(string); !strings.Contains(message, "未启用") {
		t.Fatalf("message = %#v", body["error"]["message"])
	}
}

func TestGenericAPIModelsDisabledReturns503(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"raccoon": map[string]any{"enabled": false, "max_concurrent": 1},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	pool := mustPool(t, dir, Config{Enabled: false, MaxConcurrent: 1})
	provider := &Provider{store: store, accountsDir: dir, pool: pool, now: time.Now}
	srv := api.NewServer(api.ServerOptions{
		Providers: []providers.Provider{provider},
		PanelAuth: passTestAuth, APIKeyAuth: passTestAuth,
	})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?realm=raccoon", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled models status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["error"]["code"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("disabled models envelope = %#v", body)
	}
}

func TestGenericAPIModelsEnabledWithoutAccountsUsesSeeds(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"raccoon": map[string]any{"enabled": true, "max_concurrent": 1},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	pool := mustPool(t, dir, Config{Enabled: true, MaxConcurrent: 1})
	provider := &Provider{store: store, accountsDir: dir, pool: pool, now: time.Now}
	srv := api.NewServer(api.ServerOptions{
		Providers: []providers.Provider{provider},
		PanelAuth: passTestAuth, APIKeyAuth: passTestAuth,
	})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?realm=raccoon", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed models status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "raccoon-090dd3") {
		t.Fatalf("seed catalog missing: %s", rec.Body)
	}
}

// wireErrorProvider exposes a real Raccoon APIError through the generic chat
// route so the wire envelope (status, type, unprefixed message) is verified.
type wireErrorProvider struct {
	providers.Provider
	err error
}

func (p *wireErrorProvider) ID() string    { return "raccoon" }
func (p *wireErrorProvider) Realm() string { return "raccoon" }
func (p *wireErrorProvider) Routes() []providers.Route {
	return nil
}
func (p *wireErrorProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, p.err
}

func TestGenericAPIRaccoonAPIErrorWireParity(t *testing.T) {
	cases := []struct {
		name    string
		err     *APIError
		status  int
		errType string
		message string
	}{
		{
			name: "bad request", err: &APIError{Kind: KindBadRequest, Status: 400, Message: "bad input"},
			status: http.StatusBadRequest, errType: "invalid_request_error", message: "bad input",
		},
		{
			name: "auth", err: &APIError{Kind: KindAuth, Status: 401, Message: "auth failed"},
			status: http.StatusBadGateway, errType: "invalid_request_error", message: "auth failed",
		},
		{
			name: "rate limited", err: &APIError{Kind: KindRateLimited, Status: 429, Message: "slow down"},
			status: http.StatusTooManyRequests, errType: "rate_limit_error", message: "slow down",
		},
		{
			name: "unavailable", err: &APIError{Kind: KindUnavailable, Status: 503, Message: "no account"},
			status: http.StatusServiceUnavailable, errType: "invalid_request_error", message: "no account",
		},
		{
			name: "not found", err: &APIError{Kind: KindNotFound, Status: 404, Message: "missing"},
			status: http.StatusConflict, errType: "invalid_request_error", message: "missing",
		},
		{
			name: "upstream", err: &APIError{Kind: KindUpstream, Status: 500, Message: "upstream exploded"},
			status: http.StatusBadGateway, errType: "invalid_request_error", message: "upstream exploded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &wireErrorProvider{err: tc.err}
			srv := api.NewServer(api.ServerOptions{
				Providers: []providers.Provider{provider},
				PanelAuth: passTestAuth, APIKeyAuth: passTestAuth,
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?realm=raccoon",
				strings.NewReader(`{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hi"}]}`))
			srv.Handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.status, rec.Body)
			}
			var body map[string]map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			if body["error"]["type"] != tc.errType || body["error"]["message"] != tc.message {
				t.Fatalf("envelope = %#v, want type=%s message=%q", body, tc.errType, tc.message)
			}
			if strings.Contains(rec.Body.String(), "raccoon:") || strings.Contains(rec.Body.String(), "HTTP 500:") {
				t.Fatalf("wire error retained provider prefix: %s", rec.Body)
			}
		})
	}
}
