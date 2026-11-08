package integration

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/ayeaaaa/mix2api-v2/internal/providers/raccoon"
)

const sliceVersion = "2.0.0-integration"

// TestCommandPathsFromEnv covers the command's directory wiring directly. The
// helper is shared with cmd so this test exercises the real startup path.
func TestCommandPathsFromEnv(t *testing.T) {
	t.Setenv("ACCOUNTS_DIR", "")
	t.Setenv("USAGE_DIR", "   ")
	paths := app.PathsFromEnv()
	if paths.AccountsDir != app.DefaultAccountsDir {
		t.Fatalf("default AccountsDir = %q, want %q", paths.AccountsDir, app.DefaultAccountsDir)
	}
	if paths.UsageDir != app.DefaultUsageDir {
		t.Fatalf("default UsageDir = %q, want %q", paths.UsageDir, app.DefaultUsageDir)
	}

	t.Setenv("ACCOUNTS_DIR", "  custom/accounts  ")
	t.Setenv("USAGE_DIR", filepath.Join(t.TempDir(), "custom-usage"))
	paths = app.PathsFromEnv()
	if paths.AccountsDir != "custom/accounts" {
		t.Fatalf("AccountsDir = %q, want custom/accounts", paths.AccountsDir)
	}
	if want := os.Getenv("USAGE_DIR"); paths.UsageDir != want {
		t.Fatalf("UsageDir = %q, want %q", paths.UsageDir, want)
	}
}

// TestRaccoonSliceOverHTTP drives the exact command composition: env paths and
// URL, settings store, Raccoon registration, generic API server, and admin
// routes. The fake upstream is local; no test reaches the real network.
func TestRaccoonSliceOverHTTP(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	const uid = "pu-slice"
	accessToken := sliceUnsignedJWT(uid, now.Add(2*time.Hour))
	refreshToken := "refresh-" + uid

	fake := &sliceUpstream{}
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	t.Setenv("WB_RACCOON_BASE_URL", upstream.URL)

	root := t.TempDir()
	setup, err := app.Build(app.Paths{
		AccountsDir: filepath.Join(root, "accounts"),
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	if setup.Store == nil {
		t.Fatal("app.Build returned a nil settings store")
	}
	wantStorePath := filepath.Join(root, "accounts", "settings.json")
	if setup.Store.Path != wantStorePath {
		t.Fatalf("store path = %q, want %q", setup.Store.Path, wantStorePath)
	}
	if len(setup.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(setup.Providers))
	}
	if got := setup.Providers[0].ID(); got != "raccoon" {
		t.Fatalf("provider ID = %q, want raccoon", got)
	}
	if _, ok := setup.Registry.Get("raccoon"); !ok {
		t.Fatal("registry has no raccoon realm")
	}
	for _, dir := range []string{setup.Paths.AccountsDir, setup.Paths.UsageDir} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("runtime directory %q was not created: %v", dir, err)
		}
	}

	server := api.NewServer(api.ServerOptions{
		Version:   sliceVersion,
		Providers: setup.Providers,
	})
	handler := server.Handler

	// 1. Flat auth import must enable the realm and persist it in the store
	// that the provider reads on every request.
	rec := sliceRequest(t, handler, http.MethodPost, "/raccoon/accounts/import", map[string]any{
		"access_token":    accessToken,
		"refresh_token":   refreshToken,
		"office_identity": "personal",
		"office_org_name": "Slice Org",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, body = %s", rec.Code, rec.Body)
	}
	imported := sliceJSON(t, rec.Body.Bytes())
	if imported["ok"] != true || imported["enabled"] != true {
		t.Fatalf("import envelope = %#v", imported)
	}
	if imported["uid"] != uid {
		t.Fatalf("import uid = %#v, want %q", imported["uid"], uid)
	}
	account, ok := imported["account"].(map[string]any)
	if !ok || account["uid"] != uid {
		t.Fatalf("import account = %#v", imported["account"])
	}
	sliceNoSecrets(t, "import", rec.Body.Bytes(), accessToken, refreshToken)

	settings, err := setup.Store.Load()
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	raccoonSettings, ok := settings["raccoon"].(map[string]any)
	if !ok || raccoonSettings["enabled"] != true {
		t.Fatalf("raccoon settings were not auto-enabled: %#v", settings["raccoon"])
	}

	// 2. Overview must see the imported account, enabled health, and no
	// credentials.
	rec = sliceRequest(t, handler, http.MethodGet, "/raccoon/overview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, body = %s", rec.Code, rec.Body)
	}
	overview := sliceJSON(t, rec.Body.Bytes())
	if overview["provider"] != "raccoon" || overview["online"] != true {
		t.Fatalf("overview = %#v", overview)
	}
	accounts, ok := overview["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("overview accounts = %#v", overview["accounts"])
	}
	row, ok := accounts[0].(map[string]any)
	if !ok || row["uid"] != uid {
		t.Fatalf("overview account row = %#v", accounts[0])
	}
	health, ok := overview["health"].(map[string]any)
	if !ok || health["ok"] != true || health["accounts"] != float64(1) {
		t.Fatalf("overview health = %#v", overview["health"])
	}
	stats, ok := overview["stats"].(map[string]any)
	if !ok || stats["total"] != float64(1) {
		t.Fatalf("overview stats = %#v", overview["stats"])
	}
	sliceNoSecrets(t, "overview", rec.Body.Bytes(), accessToken, refreshToken)

	// 3. The generic model catalog resolves the imported account's realm.
	rec = sliceRequest(t, handler, http.MethodGet, "/v1/models?realm=raccoon", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", rec.Code, rec.Body)
	}
	models := sliceJSON(t, rec.Body.Bytes())
	if models["object"] != "list" || models["realm"] != "raccoon" {
		t.Fatalf("models envelope = %#v", models)
	}
	data, ok := models["data"].([]any)
	if !ok || len(data) != 2 {
		t.Fatalf("models data = %#v", models["data"])
	}
	modelIDs := make(map[string]bool, len(data))
	for _, item := range data {
		entry, _ := item.(map[string]any)
		id, _ := entry["id"].(string)
		modelIDs[id] = true
	}
	for _, id := range []string{"raccoon-090dd3", "sn-glm-5-3"} {
		if !modelIDs[id] {
			t.Fatalf("models missing %q: %#v", id, modelIDs)
		}
	}
	sliceNoSecrets(t, "models", rec.Body.Bytes(), accessToken, refreshToken)

	// 4. Non-stream chat must aggregate the upstream SSE stream.
	rec = sliceRequest(t, handler, http.MethodPost, "/v1/chat/completions?realm=raccoon", map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat nonstream status = %d, body = %s", rec.Code, rec.Body)
	}
	completion := sliceJSON(t, rec.Body.Bytes())
	if completion["object"] != "chat.completion" {
		t.Fatalf("completion = %#v", completion)
	}
	choices, ok := completion["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("completion choices = %#v", completion["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] != "你好" {
		t.Fatalf("completion message = %#v", message)
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("completion finish_reason = %#v", choice["finish_reason"])
	}
	usage, ok := completion["usage"].(map[string]any)
	if !ok || usage["total_tokens"] != float64(5) {
		t.Fatalf("completion usage = %#v", completion["usage"])
	}
	sliceNoSecrets(t, "chat nonstream", rec.Body.Bytes(), accessToken, refreshToken)

	// 5. Stream chat must pass SSE frames through and terminate with [DONE].
	rec = sliceRequest(t, handler, http.MethodPost, "/v1/chat/completions?realm=raccoon", map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat stream status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("chat stream content type = %q", got)
	}
	streamText := rec.Body.String()
	if !strings.Contains(streamText, `"content":"你"`) ||
		!strings.HasSuffix(streamText, "data: [DONE]\n\n") {
		t.Fatalf("chat stream body = %q", streamText)
	}
	sliceNoSecrets(t, "chat stream", rec.Body.Bytes(), accessToken, refreshToken)

	// 6. Generic health must include the Raccoon realm.
	rec = sliceRequest(t, handler, http.MethodGet, "/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %s", rec.Code, rec.Body)
	}
	healthRoot := sliceJSON(t, rec.Body.Bytes())
	if healthRoot["ok"] != true || healthRoot["version"] != sliceVersion {
		t.Fatalf("health envelope = %#v", healthRoot)
	}
	providerHealths, ok := healthRoot["providers"].(map[string]any)
	if !ok {
		t.Fatalf("health providers = %#v", healthRoot["providers"])
	}
	raccoonHealth, ok := providerHealths["raccoon"].(map[string]any)
	if !ok || raccoonHealth["ok"] != true || raccoonHealth["accounts"] != float64(1) {
		t.Fatalf("raccoon health = %#v", providerHealths["raccoon"])
	}
	sliceNoSecrets(t, "health", rec.Body.Bytes(), accessToken, refreshToken)

	// The fake upstream proves the generic server used the imported account
	// and always forced the provider stream.
	fake.mu.Lock()
	catalogCalls := fake.catalogCalls
	chatCalls := fake.chatCalls
	chatAuths := append([]string(nil), fake.chatAuths...)
	chatBodies := append([]map[string]any(nil), fake.chatBodies...)
	fake.mu.Unlock()

	if catalogCalls == 0 {
		t.Fatal("fake upstream saw no catalog request")
	}
	if chatCalls != 2 {
		t.Fatalf("fake chat calls = %d, want 2", chatCalls)
	}
	for _, auth := range chatAuths {
		if auth != "Bearer "+accessToken {
			t.Fatal("fake upstream did not receive the imported account bearer")
		}
	}
	for _, body := range chatBodies {
		if body["stream"] != true {
			t.Fatalf("upstream body did not force stream=true: %#v", body)
		}
		if _, leaked := body["realm"]; leaked {
			t.Fatalf("gateway realm leaked upstream: %#v", body)
		}
	}
}

// sliceUpstream is the smallest deterministic Raccoon upstream for this slice:
// the catalog and chat SSE endpoints. Other paths 404, which overview's
// best-effort points polling tolerates.
type sliceUpstream struct {
	mu sync.Mutex

	catalogCalls int
	chatCalls    int
	chatAuths    []string
	chatBodies   []map[string]any
}

func (u *sliceUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == raccoon.CatalogPath:
		u.mu.Lock()
		u.catalogCalls++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sliceCatalog))

	case r.Method == http.MethodPost && r.URL.Path == raccoon.ChatPath:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)

		u.mu.Lock()
		u.chatCalls++
		u.chatAuths = append(u.chatAuths, r.Header.Get("Authorization"))
		u.chatBodies = append(u.chatBodies, body)
		u.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for _, frame := range sliceChatFrames(model) {
			if _, err := w.Write(frame); err != nil {
				return
			}
		}

	default:
		http.NotFound(w, r)
	}
}

func sliceRequest(t *testing.T, handler http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	var request *http.Request
	if reader == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, reader)
		request.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec
}

func sliceJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON %s: %v", body, err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("JSON root = %T, want object", value)
	}
	return root
}

// sliceNoSecrets fails without printing either secret.
func sliceNoSecrets(t *testing.T, where string, body []byte, secrets ...string) {
	t.Helper()
	text := string(body)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("%s leaked a credential", where)
		}
	}
}

func sliceUnsignedJWT(uid string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"sub":%q,"exp":%d}`, uid, expiry.Unix()),
	))
	return header + "." + payload + ".signature"
}

const sliceCatalog = `{
  "code": 0,
  "data": {
    "categories": [
      {
        "type": "chat",
        "default_model": "raccoon-090dd3",
        "models": [
          {
            "name": "raccoon-090dd3",
            "display_name": "Raccoon-Work",
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "tags": ["general"]
          },
          {
            "name": "sn-glm-5-3",
            "display_name": "GLM-5-3",
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "tags": ["code"]
          }
        ]
      }
    ]
  }
}`

// sliceChatFrames mirrors the v1.6.5 semantic stream: role, two text deltas, a
// terminator, a usage-only frame, and [DONE].
func sliceChatFrames(model string) [][]byte {
	chunks := []map[string]any{
		{
			"id": "chatcmpl-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "你"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "好"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
			}},
		},
		{
			"id": "chatcmpl-slice", "object": "chat.completion.chunk", "created": 1,
			"model":   model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5,
			},
		},
	}
	frames := make([][]byte, 0, len(chunks)+1)
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		frames = append(frames, []byte("data: "+string(encoded)+"\n\n"))
	}
	return append(frames, []byte("data: [DONE]\n\n"))
}
