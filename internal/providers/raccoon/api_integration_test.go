package raccoon

import (
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
	srv := api.NewServer(api.ServerOptions{Providers: []providers.Provider{provider}})

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
	srv := api.NewServer(api.ServerOptions{Providers: []providers.Provider{provider}})

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
