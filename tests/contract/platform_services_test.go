package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	platformPanelToken = "platform-contract-panel-token"
	platformAPIKey     = "platform-contract-api-key"
	platformModel      = "contract-model"
)

// platformContractProvider is a deterministic provider used to pin the
// platform wire surface without accounts or network. It carries the optional
// usage reporter and daily-limit hook so the same fixture proves recorder and
// guard wiring.
type platformContractProvider struct {
	realm string

	mu       sync.Mutex
	calls    int
	applied  int
	values   map[string]any
	usage    map[string]any
	streams  int
	lastRaw  map[string]json.RawMessage
	open     func(context.Context, providers.ChatRequest) (providers.ChatStream, error)
	noRoutes bool
}

func (p *platformContractProvider) ID() string    { return "contract" }
func (p *platformContractProvider) Realm() string { return p.realm }
func (p *platformContractProvider) Overview(context.Context, bool) (any, error) {
	return map[string]any{"realm": p.realm}, nil
}
func (p *platformContractProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true, "accounts": 1}, nil
}
func (p *platformContractProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return []providers.Model{{
		"id": platformModel, "object": "model", "context_window": 1000,
		"max_output": 123, "owned_by": "contract",
	}}, nil
}
func (p *platformContractProvider) OpenChat(ctx context.Context, req providers.ChatRequest) (providers.ChatStream, error) {
	if p.open != nil {
		return p.open(ctx, req)
	}
	p.mu.Lock()
	p.calls++
	p.streams++
	p.lastRaw = map[string]json.RawMessage{}
	for key, value := range req.Raw {
		p.lastRaw[key] = append(json.RawMessage(nil), value...)
	}
	p.mu.Unlock()
	return &platformContractStream{snapshot: providers.UsageSnapshot{
		Realm: p.realm, Account: "contract-uid", Model: "billed-contract-model",
		PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6,
		Credit: 0.25, CreditKnown: true, Free: true, CostPolicy: "credits", Kind: "chat",
	}}, nil
}
func (p *platformContractProvider) Routes() []providers.Route {
	if p.noRoutes {
		return nil
	}
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
	return []providers.Route{
		{Method: http.MethodGet, Path: "/checkin", Auth: providers.AuthPanel, Handler: ok},
		{Method: http.MethodGet, Path: "/growth", Auth: providers.AuthPanel, Handler: ok},
	}
}
func (p *platformContractProvider) Jobs() []scheduler.Job { return nil }
func (p *platformContractProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		MediaGeneration: true, MediaImages: true, MediaVideos: true,
		ImageInputInChat: true, AgentModelSpecs: true, WindowProbe: true,
		QuotaScan: true, UsageLimits: true,
	}
}
func (p *platformContractProvider) MediaModels(context.Context, string) ([]providers.Model, error) {
	return []providers.Model{
		{"id": "image-model", "object": "model", "tags": []any{"text-to-image", "image-to-image"}},
		{"id": "video-model", "object": "model", "tags": []any{"text-to-video"}, "videoGeneration": map[string]any{"resolutions": []any{"720p"}}},
	}, nil
}
func (p *platformContractProvider) GenerateImage(context.Context, providers.MediaRequest) (providers.MediaImageResult, error) {
	credit := 1.0
	return providers.MediaImageResult{URL: "https://example.invalid/image.png", Credit: &credit, Kind: "image"}, nil
}
func (p *platformContractProvider) EditImage(context.Context, providers.MediaRequest) (providers.MediaImageResult, error) {
	return p.GenerateImage(context.Background(), providers.MediaRequest{})
}
func (p *platformContractProvider) SubmitVideo(context.Context, providers.MediaRequest) (providers.MediaVideoSubmit, error) {
	return providers.MediaVideoSubmit{UpstreamID: "video-1"}, nil
}
func (p *platformContractProvider) PollVideo(context.Context, string, string) (providers.MediaVideoState, error) {
	return providers.MediaVideoState{Status: "completed", Videos: []string{"https://example.invalid/video.mp4"}}, nil
}
func (p *platformContractProvider) DownloadVideo(context.Context, string, string, string) error {
	return nil
}
func (p *platformContractProvider) CancelVideo(context.Context, string, string) error { return nil }
func (p *platformContractProvider) ApplyLimits(values, usage map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applied++
	p.values = values
	p.usage = usage
}

type platformContractStream struct {
	frames   [][]byte
	index    int
	snapshot providers.UsageSnapshot
}

func (s *platformContractStream) Next() ([]byte, error) {
	if s.index == 0 {
		s.frames = platformChatFrames()
	}
	if s.index >= len(s.frames) {
		return nil, io.EOF
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}
func (s *platformContractStream) Close() error                           { return nil }
func (s *platformContractStream) UsageSnapshot() providers.UsageSnapshot { return s.snapshot }

func platformChatFrames() [][]byte {
	chunk := func(choices string, usage string) []byte {
		body := `{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,` +
			`"model":"` + platformModel + `","choices":` + choices
		if usage != "" {
			body += `,"usage":` + usage
		}
		body += `}`
		return []byte("data: " + body + "\n\n")
	}
	return [][]byte{
		chunk(`[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]`, ""),
		chunk(`[{"index":0,"delta":{},"finish_reason":"stop"}]`,
			`{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6,"credit":0.25}`),
		[]byte("data: [DONE]\n\n"),
	}
}

type platformContractFixture struct {
	root       string
	accounts   string
	usage      string
	store      *config.Store
	platform   *app.Platform
	provider   *platformContractProvider
	keyStore   *keys.Store
	server     *api.Server
	panelToken string
	apiKey     string
}

func newPlatformContractFixture(t *testing.T) *platformContractFixture {
	t.Helper()
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	usageDir := filepath.Join(root, "usage")
	mediaDir := filepath.Join(root, "media")
	updateDir := filepath.Join(root, "updates")
	for _, dir := range []string{accounts, usageDir, mediaDir, updateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store := config.New(filepath.Join(accounts, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"enable_anthropic_messages": true,
		"limits": map[string]any{
			"daily_token_limit": map[string]any{"global": 100000},
		},
		"quota_scan_models":    []any{platformModel},
		"update_check_enabled": false,
		"media":                map[string]any{"enabled": true, "realm": "intl"},
	}); err != nil {
		t.Fatal(err)
	}
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{{
		ID: "k1", Name: "contract", Key: platformAPIKey, Realm: "intl",
		Enabled: true, ConcurrencyLimit: 4,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &platformContractProvider{realm: "intl"}
	cnProvider := &platformContractProvider{realm: "cn", noRoutes: true}
	registry := providers.NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(cnProvider); err != nil {
		t.Fatal(err)
	}
	setup := &app.Setup{
		Paths:     app.Paths{AccountsDir: accounts, UsageDir: usageDir},
		Store:     store,
		Registry:  registry,
		Providers: []providers.Provider{provider, cnProvider},
	}
	platform, err := app.BuildPlatform(setup, app.PlatformOptions{
		Paths:         setup.Paths,
		Version:       "2.0.0-contract",
		PanelPassword: "admin",
		MediaDir:      mediaDir,
		UpdateDir:     updateDir,
		Home:          root,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	server := api.New(api.ServerOptions{
		Version:                  "2.0.0-contract",
		Providers:                []providers.Provider{provider, cnProvider},
		Platform:                 platform.HTTPServices(),
		PanelAuth:                api.RequirePanelAuth(platformPanelToken),
		APIKeyAuth:               api.RequireAPIKeyAuth(platformAPIKey),
		StaticAPIKey:             platformAPIKey,
		AnthropicMessagesEnabled: platform.AnthropicMessagesEnabled,
		APIKeyRealm:              nil,
	})
	return &platformContractFixture{
		root: root, accounts: accounts, usage: usageDir, store: store,
		platform: platform, provider: provider, keyStore: keyStore,
		server: server, panelToken: platformPanelToken, apiKey: platformAPIKey,
	}
}

func (f *platformContractFixture) request(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return f.requestFrom(t, method, path, body, headers, "")
}

func (f *platformContractFixture) requestFrom(t *testing.T, method, path, body string, headers map[string]string, remote string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *platformContractFixture) panelHeaders() map[string]string {
	return map[string]string{"X-Panel-Token": f.panelToken}
}

func (f *platformContractFixture) apiHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.apiKey}
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return value
}

func TestPlatformRoutesContract(t *testing.T) {
	f := newPlatformContractFixture(t)

	t.Run("health", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/health", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("health = %d", rec.Code)
		}
		if got := rec.Header().Get("Server"); !strings.HasPrefix(got, "mix2api/") {
			t.Fatalf("Server header = %q", got)
		}
		body := decodeResponse(t, rec)
		if body["ok"] != true {
			t.Fatalf("health body = %v", body)
		}
	})

	t.Run("models_and_cors", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/v1/models", "", f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("models = %d body=%s", rec.Code, rec.Body)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal("API CORS header missing")
		}
		body := decodeResponse(t, rec)
		if _, ok := body["data"].([]any); !ok {
			t.Fatalf("models body = %v", body)
		}
		rec = f.request(t, http.MethodGet, "/settings", "", f.panelHeaders())
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("management route advertised CORS")
		}
		for _, path := range []string{"/messages", "/v1/messages", "/responses", "/v1/responses"} {
			rec = f.request(t, http.MethodOptions, path, "", nil)
			if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatalf("%s CORS = %d %q", path, rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
			}
		}
	})

	t.Run("chat_nonstream_and_usage", func(t *testing.T) {
		payload := `{"model":"` + platformModel + `","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
		rec := f.request(t, http.MethodPost, "/v1/chat/completions", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("chat = %d body=%s", rec.Code, rec.Body)
		}
		body := decodeResponse(t, rec)
		if body["object"] != "chat.completion" {
			t.Fatalf("chat body = %v", body)
		}
		rows := readUsageRows(t, f.usage)
		if len(rows) != 1 {
			t.Fatalf("usage rows = %d, want 1", len(rows))
		}
		row := rows[0]
		if row["realm"] != "intl" || row["account"] != "contract-uid" || row["key"] != "k1" {
			t.Fatalf("usage attribution = %v", row)
		}
		if row["model"] != "billed-contract-model" || row["requested_model"] != platformModel {
			t.Fatalf("billed/requested model = %v", row)
		}
		if row["reasoning_effort"] != "high" || row["free"] != true {
			t.Fatalf("request attribution = %v", row)
		}
		if row["outcome"] != "completed" {
			t.Fatalf("usage outcome/policy = %v", row)
		}
	})

	t.Run("chat_stream_and_no_double_record", func(t *testing.T) {
		before := len(readUsageRows(t, f.usage))
		payload := `{"model":"` + platformModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		rec := f.request(t, http.MethodPost, "/v1/chat/completions", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("stream = %d body=%s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "data: [DONE]") {
			t.Fatalf("stream missing DONE: %q", rec.Body.String())
		}
		rows := readUsageRows(t, f.usage)
		if len(rows) != before+1 {
			t.Fatalf("usage rows = %d, want exactly one more", len(rows))
		}
		if rows[len(rows)-1]["stream"] != true {
			t.Fatalf("stream row = %v", rows[len(rows)-1])
		}
	})

	t.Run("responses", func(t *testing.T) {
		payload := `{"model":"` + platformModel + `","input":"hi"}`
		rec := f.request(t, http.MethodPost, "/v1/responses", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("responses = %d body=%s", rec.Code, rec.Body)
		}
		body := decodeResponse(t, rec)
		if body["object"] != "response" {
			t.Fatalf("responses body = %v", body)
		}
		payload = `{"model":"` + platformModel + `","input":"hi","stream":true}`
		rec = f.request(t, http.MethodPost, "/v1/responses", payload, f.apiHeaders())
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "response.completed") {
			t.Fatalf("responses stream = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("messages_and_count_tokens", func(t *testing.T) {
		payload := `{"model":"` + platformModel + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		rec := f.request(t, http.MethodPost, "/v1/messages", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("messages = %d body=%s", rec.Code, rec.Body)
		}
		body := decodeResponse(t, rec)
		if body["type"] != "message" {
			t.Fatalf("messages body = %v", body)
		}
		rowsBefore := len(readUsageRows(t, f.usage))
		rec = f.request(t, http.MethodPost, "/v1/messages/count_tokens", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("count_tokens = %d body=%s", rec.Code, rec.Body)
		}
		if got := len(readUsageRows(t, f.usage)); got != rowsBefore {
			t.Fatalf("count_tokens recorded a usage row (%d -> %d)", rowsBefore, got)
		}
	})

	t.Run("messages_stream", func(t *testing.T) {
		payload := `{"model":"` + platformModel + `","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		rec := f.request(t, http.MethodPost, "/v1/messages", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("messages stream = %d body=%s", rec.Code, rec.Body)
		}
		body := rec.Body.String()
		for _, want := range []string{"message_start", "content_block_delta", "message_stop"} {
			if !strings.Contains(body, want) {
				t.Fatalf("messages stream missing %s: %q", want, body)
			}
		}
	})

	t.Run("responses_compact_and_aliases", func(t *testing.T) {
		payload := `{"model":"` + platformModel + `","input":"summary please"}`
		rec := f.request(t, http.MethodPost, "/responses/compact", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("compact = %d body=%s", rec.Code, rec.Body)
		}
		body := decodeResponse(t, rec)
		if body["object"] != "response" {
			t.Fatalf("compact body = %v", body)
		}
		for _, path := range []string{"/models", "/v1/models"} {
			rec := f.request(t, http.MethodGet, path, "", f.apiHeaders())
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d", path, rec.Code)
			}
		}
		rec = f.request(t, http.MethodPost, "/chat/completions",
			`{"model":"`+platformModel+`","messages":[{"role":"user","content":"hi"}]}`, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("chat alias = %d", rec.Code)
		}
	})

	t.Run("default_max_output_tokens", func(t *testing.T) {
		payload := `{"model":"` + platformModel + `","input":"hi"}`
		rec := f.request(t, http.MethodPost, "/v1/responses", payload, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("responses = %d", rec.Code)
		}
		f.provider.mu.Lock()
		raw := f.provider.lastRaw["max_tokens"]
		f.provider.mu.Unlock()
		if string(raw) != "123" {
			t.Fatalf("default max_tokens = %q, want 123", raw)
		}
	})

	t.Run("settings_key_save", func(t *testing.T) {
		rec := f.request(t, http.MethodPost, "/settings/save",
			`{"api_keys":[{"id":"k1","name":"contract","key":"`+platformAPIKey+`","realm":"intl","enabled":true},{"id":"k2","name":"second","key":"second-key","realm":"intl","enabled":true}]}`,
			f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("key save = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodGet, "/v1/models", "", map[string]string{"Authorization": "Bearer second-key"})
		if rec.Code != http.StatusOK {
			t.Fatalf("second key = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("usage_admin_and_etag", func(t *testing.T) {
		for _, path := range []string{
			"/usage", "/usage/recent", "/usage/analytics", "/usage/perf",
			"/usage/timeseries", "/usage/by-account", "/usage/remaining",
		} {
			rec := f.request(t, http.MethodGet, path, "", f.panelHeaders())
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d body=%s", path, rec.Code, rec.Body)
			}
		}
		rec := f.request(t, http.MethodGet, "/usage", "", f.panelHeaders())
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatal("usage response has no ETag")
		}
		headers := f.panelHeaders()
		headers["If-None-Match"] = etag
		rec = f.request(t, http.MethodGet, "/usage", "", headers)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("usage 304 = %d", rec.Code)
		}
	})

	t.Run("pricing", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/pricing", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("pricing = %d body=%s", rec.Code, rec.Body)
		}
		body := decodeResponse(t, rec)
		if _, ok := body["interval_minutes"]; !ok {
			t.Fatalf("pricing status = %v", body)
		}
	})

	t.Run("logs", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/logs", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("logs = %d body=%s", rec.Code, rec.Body)
		}
		body := decodeResponse(t, rec)
		if body["mode"] != "memory" {
			t.Fatalf("logs mode = %v", body)
		}
	})

	t.Run("settings_realm_proxy_keys", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/settings", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("settings = %d", rec.Code)
		}
		rec = f.request(t, http.MethodPost, "/settings/save",
			`{"daily_token_limit":42}`, f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("settings save = %d body=%s", rec.Code, rec.Body)
		}
		if f.provider.applied == 0 {
			t.Fatal("limit save did not force a guard refresh")
		}
		rec = f.request(t, http.MethodGet, "/realm", "", f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("realm = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodGet, "/proxy/slots", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy slots = %d", rec.Code)
		}
		rec = f.request(t, http.MethodPost, "/proxy/slots/save", `{"slots":[]}`, f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy save = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodGet, "/settings/reveal?id=k1", "", f.panelHeaders())
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), platformAPIKey) {
			t.Fatalf("reveal = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("panel_login_status_logout", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/panel/status", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("panel status = %d", rec.Code)
		}
		rec = f.request(t, http.MethodPost, "/panel/login", `{"password":"admin"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("panel login = %d body=%s", rec.Code, rec.Body)
		}
		body := decodeResponse(t, rec)
		token, _ := body["token"].(string)
		if token == "" {
			t.Fatalf("login token = %v", body)
		}
		rec = f.request(t, http.MethodPost, "/panel/logout", "", map[string]string{"X-Panel-Token": token})
		if rec.Code != http.StatusOK {
			t.Fatalf("panel logout = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("agents", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/agents", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("agents = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodGet, "/agents/available", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("agents available = %d", rec.Code)
		}
		rec = f.request(t, http.MethodGet, "/agents/model-selection", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("agents selection = %d", rec.Code)
		}
	})

	t.Run("updates", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/updates", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("updates = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodGet, "/updates/backups", "", f.panelHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("update backups = %d", rec.Code)
		}
		rec = f.request(t, http.MethodPost, "/updates/install", "", f.panelHeaders())
		if rec.Code != http.StatusConflict {
			t.Fatalf("update install without executor = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodPost, "/updates/rollback", "", f.panelHeaders())
		if rec.Code != http.StatusConflict {
			t.Fatalf("update rollback without backup = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("media_and_agents", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/v1/media/tasks", "", f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("media tasks = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodPost, "/v1/images/generations",
			`{"model":"image-model","prompt":"a cat"}`, f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("image generation = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodPost, "/v1/videos",
			`{"model":"video-model","prompt":"a cat","seconds":8}`, f.apiHeaders())
		if rec.Code != http.StatusAccepted {
			t.Fatalf("video submit = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodGet, "/media/models?realm=intl", "", f.apiHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("media models = %d body=%s", rec.Code, rec.Body)
		}
		rec = f.request(t, http.MethodGet, "/agents/cli", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("agents cli = %d", rec.Code)
		}
		rec = f.requestFrom(t, http.MethodPost, "/agents/apply", `{}`, f.panelHeaders(), "203.0.113.9:1234")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("remote agent apply = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("provider_panel_routes", func(t *testing.T) {
		for _, path := range []string{"/checkin", "/growth"} {
			rec := f.request(t, http.MethodGet, path, "", f.panelHeaders())
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d", path, rec.Code)
			}
			rec = f.request(t, http.MethodGet, path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s without panel = %d", path, rec.Code)
			}
		}
	})

	t.Run("api_key_required", func(t *testing.T) {
		rec := f.request(t, http.MethodGet, "/v1/models", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("models without key = %d", rec.Code)
		}
		rec = f.request(t, http.MethodGet, "/v1/models", "", map[string]string{"Authorization": "Bearer wrong"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("models with wrong key = %d", rec.Code)
		}
	})
}

func TestUsageRecordCallSites(t *testing.T) {
	f := newPlatformContractFixture(t)
	payload := `{"model":"` + platformModel + `","input":"hi"}`
	if rec := f.request(t, http.MethodPost, "/v1/responses", payload, f.apiHeaders()); rec.Code != http.StatusOK {
		t.Fatalf("responses = %d", rec.Code)
	}
	payload = `{"model":"` + platformModel + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	if rec := f.request(t, http.MethodPost, "/v1/messages", payload, f.apiHeaders()); rec.Code != http.StatusOK {
		t.Fatalf("messages = %d", rec.Code)
	}
	rows := readUsageRows(t, f.usage)
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d, want 2", len(rows))
	}
	protos := map[string]bool{}
	for _, row := range rows {
		if row["realm"] != "intl" || row["account"] != "contract-uid" {
			t.Fatalf("row attribution = %v", row)
		}
		if clientAborted(row) {
			continue
		}
		protos["billed-contract-model"] = true
	}
	if !protos["billed-contract-model"] {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestPlatformClientAbortRecordsExactlyOnce(t *testing.T) {
	f := newPlatformContractFixture(t)
	started := make(chan struct{})
	var once sync.Once
	f.provider.open = func(ctx context.Context, _ providers.ChatRequest) (providers.ChatStream, error) {
		return &blockingChatStream{ctx: ctx, started: started, once: &once}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	payload := `{"model":"` + platformModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		f.server.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream stream did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after client abort")
	}
	rows := readUsageRows(t, f.usage)
	if len(rows) != 1 {
		t.Fatalf("client abort recorded %d rows, want exactly 1", len(rows))
	}
	if rows[0]["outcome"] != "client_aborted" {
		t.Fatalf("client abort row = %+v", rows[0])
	}
	if rows[0]["error"] == true {
		t.Fatalf("client abort row was marked as an error: %+v", rows[0])
	}
}

type blockingChatStream struct {
	ctx     context.Context
	started chan struct{}
	once    *sync.Once
}

func (s *blockingChatStream) Next() ([]byte, error) {
	s.once.Do(func() { close(s.started) })
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}
func (s *blockingChatStream) Close() error { return nil }

func readUsageRows(t *testing.T, dir string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	rows := make([]map[string]any, 0)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var row map[string]any
		if err := decoder.Decode(&row); err != nil {
			t.Fatalf("decode usage row: %v", err)
		}
		rows = append(rows, row)
	}
	return rows
}

func clientAborted(row map[string]any) bool {
	outcome, _ := row["outcome"].(string)
	return outcome == "client_aborted"
}

func TestGateDContractNoNetworkAndSecrets(t *testing.T) {
	f := newPlatformContractFixture(t)
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	blocked := 0
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		host := request.URL.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			blocked++
			return nil, fmt.Errorf("contract test attempted network access to %s", host)
		}
		return oldTransport.RoundTrip(request)
	})
	payload := `{"model":"` + platformModel + `","messages":[{"role":"user","content":"hi"}]}`
	if rec := f.request(t, http.MethodPost, "/v1/chat/completions", payload, f.apiHeaders()); rec.Code != http.StatusOK {
		t.Fatalf("chat = %d", rec.Code)
	}
	if blocked != 0 {
		t.Fatalf("test performed %d non-loopback requests", blocked)
	}
	// Secrets must not appear in the usage ledger or the admin views.
	raw, err := os.ReadFile(filepath.Join(f.usage, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{platformAPIKey, platformPanelToken} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("usage ledger leaked a secret")
		}
	}
	for _, path := range []string{"/usage", "/logs", "/settings"} {
		rec := f.request(t, http.MethodGet, path, "", f.panelHeaders())
		if strings.Contains(rec.Body.String(), platformAPIKey) {
			t.Fatalf("%s leaked the API key", path)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

var _ = time.Now

func TestUsageRecordChatFailuresCarryStatusAndMessage(t *testing.T) {
	f := newPlatformContractFixture(t)
	lastRow := func(t *testing.T) map[string]any {
		t.Helper()
		rows := readUsageRows(t, f.usage)
		if len(rows) == 0 {
			t.Fatal("no usage row")
		}
		return rows[len(rows)-1]
	}
	assertFailure := func(t *testing.T, rec *httptest.ResponseRecorder, status int, label string) map[string]any {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("%s status = %d body=%s", label, rec.Code, rec.Body)
		}
		row := lastRow(t)
		if row["error"] != true || int(apiInt(row["status"])) != status {
			t.Fatalf("%s row = %+v", label, row)
		}
		if strings.TrimSpace(fmt.Sprint(row["message"])) == "" {
			t.Fatalf("%s row has no message: %+v", label, row)
		}
		if row["key"] != "k1" {
			t.Fatalf("%s key attribution = %+v", label, row)
		}
		return row
	}

	rec := f.request(t, http.MethodPost, "/v1/chat/completions", `{bad`, f.apiHeaders())
	assertFailure(t, rec, http.StatusBadRequest, "parse failure")

	rec = f.request(t, http.MethodPost, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}]}`, f.apiHeaders())
	assertFailure(t, rec, http.StatusBadRequest, "missing model")

	// Key model allow-list rejection.
	if _, err := f.keyStore.Save([]keys.Entry{{
		ID: "k1", Name: "contract", Key: platformAPIKey, Realm: "intl",
		Models: []string{"other-model"}, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	rec = f.request(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"`+platformModel+`","messages":[{"role":"user","content":"hi"}]}`, f.apiHeaders())
	assertFailure(t, rec, http.StatusBadRequest, "key model rejection")

	// Unknown provider realm fails before a provider exists.
	if _, err := f.keyStore.Save([]keys.Entry{{
		ID: "k1", Name: "contract", Key: platformAPIKey, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	headers := f.apiHeaders()
	headers["X-Realm"] = "unknown-realm"
	rec = f.request(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"`+platformModel+`","messages":[{"role":"user","content":"hi"}]}`, headers)
	assertFailure(t, rec, http.StatusNotFound, "realm rejection")

	// A key bound to the WorkBuddy-only "all" alias is rejected by key policy.
	if _, err := f.keyStore.Save([]keys.Entry{{
		ID: "k1", Name: "contract", Key: platformAPIKey, Realm: "codearts", Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	rec = f.request(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"`+platformModel+`","messages":[{"role":"user","content":"hi"}]}`, f.apiHeaders())
	assertFailure(t, rec, http.StatusBadRequest, "key realm rejection")

}

func TestUsageRecordChatResolveFailureCarriesStatus(t *testing.T) {
	f := newPlatformContractFixture(t)
	if _, err := f.keyStore.Save([]keys.Entry{{
		ID: "k1", Name: "contract", Key: platformAPIKey, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := f.keyStore.List(false)
	t.Logf("keys after unbound save: %+v", entries)
	rec := f.request(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`, f.apiHeaders())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("resolve failure status = %d body=%s", rec.Code, rec.Body)
	}
	rows := readUsageRows(t, f.usage)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row["error"] != true || int(apiInt(row["status"])) != http.StatusNotFound {
		t.Fatalf("resolve failure row = %+v", row)
	}
	if strings.TrimSpace(fmt.Sprint(row["message"])) == "" {
		t.Fatalf("resolve failure row has no message: %+v", row)
	}
}

func apiInt(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		number, _ := typed.Int64()
		return number
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	}
	return 0
}

func TestUsageRecordProtocolFailuresCarryStatus(t *testing.T) {
	f := newPlatformContractFixture(t)
	if _, err := f.keyStore.Save([]keys.Entry{{
		ID: "k1", Name: "contract", Key: platformAPIKey, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	headers := f.apiHeaders()
	headers["X-Realm"] = "unknown-realm"
	rec := f.request(t, http.MethodPost, "/v1/responses",
		`{"model":"`+platformModel+`","input":"hi"}`, headers)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("responses realm failure = %d body=%s", rec.Code, rec.Body)
	}
	rec = f.request(t, http.MethodPost, "/v1/messages",
		`{"model":"`+platformModel+`","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, headers)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("messages realm failure = %d body=%s", rec.Code, rec.Body)
	}
	rows := readUsageRows(t, f.usage)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row["error"] != true || int(apiInt(row["status"])) != http.StatusNotFound {
			t.Fatalf("protocol failure row = %+v", row)
		}
		if strings.TrimSpace(fmt.Sprint(row["message"])) == "" {
			t.Fatalf("protocol failure row has no message: %+v", row)
		}
	}
}
