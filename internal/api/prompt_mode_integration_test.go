package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func promptModeTestServer(t *testing.T, mode, text string) (*Server, *stubProvider) {
	t.Helper()
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{"prompt": map[string]any{"mode": mode, "text": text}}); err != nil {
		t.Fatal(err)
	}
	service := settings.New(settings.Service{Store: store})
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{{"id": "m"}}, openStreams: []providers.ChatStream{newStubStream(chatSSE(textChunks("u", "ok")...)...)}}
	server := New(ServerOptions{Settings: service, Providers: []providers.Provider{provider}, PanelAuth: passThroughAuth, APIKeyAuth: passThroughAuth, AnthropicMessagesEnabled: func() bool { return true }})
	return server, provider
}

func firstMessageContent(t *testing.T, req providers.ChatRequest) string {
	t.Helper()
	var messages []map[string]any
	if err := json.Unmarshal(req.Raw["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 {
		t.Fatal("no messages")
	}
	content, _ := messages[0]["content"].(string)
	return content
}

func TestPromptModeAppliedToChat(t *testing.T) {
	body := `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	for _, tc := range []struct {
		mode string
		want string
	}{{"passthrough", "hi"}, {"custom", "gateway rule"}, {"append", "gateway rule"}} {
		t.Run(tc.mode, func(t *testing.T) {
			server, provider := promptModeTestServer(t, tc.mode, "gateway rule")
			rec := serveRequest(server.Handler(), http.MethodPost, "/v1/chat/completions", body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if len(provider.openRequests) != 1 {
				t.Fatalf("open requests = %d", len(provider.openRequests))
			}
			if got := firstMessageContent(t, provider.openRequests[0]); got != tc.want {
				t.Fatalf("first message = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPromptModeAppliedToMessagesAndResponses(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{"messages", "/v1/messages", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"m","input":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, provider := promptModeTestServer(t, "custom", "gateway rule")
			rec := serveRequest(server.Handler(), http.MethodPost, tc.path, tc.body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if len(provider.openRequests) != 1 {
				t.Fatalf("open requests = %d", len(provider.openRequests))
			}
			if got := firstMessageContent(t, provider.openRequests[0]); !strings.Contains(got, "gateway rule") {
				t.Fatalf("prompt not injected: %q", got)
			}
		})
	}
}
