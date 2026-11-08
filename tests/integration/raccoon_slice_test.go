package integration

import (
	"bytes"
	"encoding/base64"
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
	"github.com/ayeaaaa/mix2api-v2/internal/providers/raccoon"
)

const (
	sliceVersion       = "2.0.0-integration"
	slicePanelPassword = "integration-panel-password"
	sliceAPIKey        = "integration-api-key"
)

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

// TestRaccoonSliceOverHTTP drives the real command composition through a real
// TCP listener: env paths and URL, settings store, Raccoon registration, the
// generic API server, and admin routes. The API client and fake upstream are
// both httptest servers, so HTTP framing and chunked SSE reads are exercised;
// no test reaches the real network.
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
	for _, provider := range setup.Providers {
		if raccoonProvider, ok := provider.(*raccoon.Provider); ok {
			// The slice's fake upstream has no bill endpoint; keep the stream
			// terminal marker immediate while the dedicated provider test covers
			// the real post-DONE credit lookup.
			raccoonProvider.SetCreditLookbackEnabled(false)
		}
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
	if info, err := os.Stat(setup.Paths.AccountsDir); err != nil || !info.IsDir() {
		t.Fatalf("accounts directory %q was not created: %v", setup.Paths.AccountsDir, err)
	}
	if _, err := os.Stat(setup.Paths.UsageDir); !os.IsNotExist(err) {
		t.Fatalf("unused usage directory was created: %v", err)
	}

	server := api.NewServer(api.ServerOptions{
		Version:    sliceVersion,
		Providers:  setup.Providers,
		PanelAuth:  api.RequirePanelAuth(slicePanelPassword),
		APIKeyAuth: api.RequireAPIKeyAuth(sliceAPIKey),
	})
	live := httptest.NewServer(server.Handler)
	t.Cleanup(live.Close)
	client := &http.Client{Timeout: 10 * time.Second}
	panelHeaders := map[string]string{"X-Panel-Token": slicePanelPassword}
	apiHeaders := map[string]string{"Authorization": "Bearer " + sliceAPIKey}

	// Unauthenticated import/delete/chat must be rejected before any provider
	// or filesystem work. The delete path is a known panel route; even its
	// method-mismatch subtree is behind panel auth.
	for _, rejection := range []struct {
		name    string
		method  string
		path    string
		payload any
	}{
		{name: "import", method: http.MethodPost, path: "/raccoon/accounts/import", payload: map[string]any{}},
		{name: "delete", method: http.MethodPost, path: "/raccoon/accounts/delete", payload: map[string]any{"uid": uid}},
		{name: "chat", method: http.MethodPost, path: "/v1/chat/completions?realm=raccoon", payload: map[string]any{
			"model": "raccoon-090dd3", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}},
	} {
		rec := sliceHTTP(t, client, rejection.method, live.URL+rejection.path, rejection.payload, nil)
		sliceRequireStatus(t, "unauthenticated "+rejection.name, rec, http.StatusUnauthorized)
	}

	// 1. Flat auth import must enable the realm and persist it in the store
	// that the provider reads on every request.
	rec := sliceHTTP(t, client, http.MethodPost, live.URL+"/raccoon/accounts/import", map[string]any{
		"access_token":    accessToken,
		"refresh_token":   refreshToken,
		"office_identity": "personal",
		"office_org_name": "Slice Org",
	}, panelHeaders)
	sliceRequireStatus(t, "import", rec, http.StatusOK)
	imported := sliceJSON(t, "import", rec)
	if imported["ok"] != true || imported["enabled"] != true {
		t.Fatalf("import envelope: ok=%v enabled=%v", imported["ok"], imported["enabled"])
	}
	if imported["uid"] != uid {
		t.Fatalf("import uid = %v, want %q", imported["uid"], uid)
	}
	account, ok := imported["account"].(map[string]any)
	if !ok {
		t.Fatal("import account is not an object")
	}
	if account["uid"] != uid {
		t.Fatalf("import account uid = %v, want %q", account["uid"], uid)
	}
	sliceNoSecrets(t, "import", rec.Body, accessToken, refreshToken)

	settings, err := setup.Store.Load()
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	raccoonSettings, ok := settings["raccoon"].(map[string]any)
	if !ok {
		t.Fatal("raccoon settings is not an object")
	}
	if raccoonSettings["enabled"] != true {
		t.Fatalf("raccoon settings enabled = %v, want true", raccoonSettings["enabled"])
	}

	// 2. Overview must see the imported account, enabled health, and no
	// credentials.
	rec = sliceHTTP(t, client, http.MethodGet, live.URL+"/raccoon/overview", nil, panelHeaders)
	sliceRequireStatus(t, "overview", rec, http.StatusOK)
	overview := sliceJSON(t, "overview", rec)
	if overview["provider"] != "raccoon" || overview["online"] != true {
		t.Fatalf("overview provider/online = %v/%v", overview["provider"], overview["online"])
	}
	accounts, ok := overview["accounts"].([]any)
	if !ok {
		t.Fatal("overview accounts is not an array")
	}
	if len(accounts) != 1 {
		t.Fatalf("overview accounts = %d, want 1", len(accounts))
	}
	row, ok := accounts[0].(map[string]any)
	if !ok {
		t.Fatal("overview account row is not an object")
	}
	if row["uid"] != uid {
		t.Fatalf("overview account uid = %v, want %q", row["uid"], uid)
	}
	health, ok := overview["health"].(map[string]any)
	if !ok {
		t.Fatal("overview health is not an object")
	}
	if health["ok"] != true || health["accounts"] != float64(1) {
		t.Fatalf("overview health ok/accounts = %v/%v", health["ok"], health["accounts"])
	}
	stats, ok := overview["stats"].(map[string]any)
	if !ok {
		t.Fatal("overview stats is not an object")
	}
	if stats["total"] != float64(1) {
		t.Fatalf("overview stats total = %v, want 1", stats["total"])
	}
	sliceNoSecrets(t, "overview", rec.Body, accessToken, refreshToken)

	// 3. The generic model catalog resolves the imported account's realm.
	rec = sliceHTTP(t, client, http.MethodGet, live.URL+"/v1/models?realm=raccoon", nil, apiHeaders)
	sliceRequireStatus(t, "models", rec, http.StatusOK)
	models := sliceJSON(t, "models", rec)
	if models["object"] != "list" || models["realm"] != "raccoon" {
		t.Fatalf("models object/realm = %v/%v", models["object"], models["realm"])
	}
	data, ok := models["data"].([]any)
	if !ok {
		t.Fatal("models data is not an array")
	}
	if len(data) != 2 {
		t.Fatalf("models data = %d, want 2", len(data))
	}
	modelIDs := make(map[string]bool, len(data))
	for _, item := range data {
		entry, _ := item.(map[string]any)
		id, _ := entry["id"].(string)
		modelIDs[id] = true
	}
	for _, id := range []string{"raccoon-090dd3", "sn-glm-5-3"} {
		if !modelIDs[id] {
			t.Fatalf("models missing %q", id)
		}
	}
	sliceNoSecrets(t, "models", rec.Body, accessToken, refreshToken)

	// 4. Non-stream chat must aggregate the upstream SSE stream.
	rec = sliceHTTP(t, client, http.MethodPost, live.URL+"/v1/chat/completions?realm=raccoon", map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   false,
	}, apiHeaders)
	sliceRequireStatus(t, "chat nonstream", rec, http.StatusOK)
	completion := sliceJSON(t, "chat nonstream", rec)
	if completion["object"] != "chat.completion" {
		t.Fatalf("completion object = %v", completion["object"])
	}
	choices, ok := completion["choices"].([]any)
	if !ok {
		t.Fatal("completion choices is not an array")
	}
	if len(choices) != 1 {
		t.Fatalf("completion choices = %d, want 1", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["role"] != "assistant" {
		t.Fatalf("completion role = %v", message["role"])
	}
	if message["content"] != "你好" {
		t.Fatalf("completion content = %v", message["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("completion finish_reason = %v", choice["finish_reason"])
	}
	usage, ok := completion["usage"].(map[string]any)
	if !ok {
		t.Fatal("completion usage is not an object")
	}
	if usage["total_tokens"] != float64(5) {
		t.Fatalf("completion usage total = %v, want 5", usage["total_tokens"])
	}
	sliceNoSecrets(t, "chat nonstream", rec.Body, accessToken, refreshToken)

	// 5. Stream chat must pass SSE frames through a real chunked response and
	// terminate with [DONE].
	rec = sliceHTTP(t, client, http.MethodPost, live.URL+"/v1/chat/completions?realm=raccoon", map[string]any{
		"model":    "raccoon-090dd3",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   true,
	}, apiHeaders)
	sliceRequireStatus(t, "chat stream", rec, http.StatusOK)
	if got := rec.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("chat stream content type = %q", got)
	}
	if !sliceChunked(rec.TransferEncoding) {
		t.Fatalf("chat stream transfer encoding = %v, want chunked", rec.TransferEncoding)
	}
	streamText := string(rec.Body)
	hasDelta := strings.Contains(streamText, `"content":"你"`)
	hasDone := strings.HasSuffix(streamText, "data: [DONE]\n\n")
	if !hasDelta || !hasDone {
		t.Fatalf("chat stream framing: delta=%v done=%v (%s)", hasDelta, hasDone, rec.summary())
	}
	sliceNoSecrets(t, "chat stream", rec.Body, accessToken, refreshToken)

	// 6. Generic health must include the Raccoon realm.
	rec = sliceHTTP(t, client, http.MethodGet, live.URL+"/health", nil, nil)
	sliceRequireStatus(t, "health", rec, http.StatusOK)
	healthRoot := sliceJSON(t, "health", rec)
	if healthRoot["ok"] != true || healthRoot["version"] != sliceVersion {
		t.Fatalf("health ok/version = %v/%v", healthRoot["ok"], healthRoot["version"])
	}
	providerHealths, ok := healthRoot["providers"].(map[string]any)
	if !ok {
		t.Fatal("health providers is not an object")
	}
	raccoonHealth, ok := providerHealths["raccoon"].(map[string]any)
	if !ok {
		t.Fatal("health providers.raccoon is not an object")
	}
	if raccoonHealth["ok"] != true || raccoonHealth["accounts"] != float64(1) {
		t.Fatalf("raccoon health ok/accounts = %v/%v", raccoonHealth["ok"], raccoonHealth["accounts"])
	}
	sliceNoSecrets(t, "health", rec.Body, accessToken, refreshToken)

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
			t.Fatalf("upstream body stream = %v, want true", body["stream"])
		}
		if _, leaked := body["realm"]; leaked {
			t.Fatal("gateway realm leaked upstream")
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

// sliceResponse captures only what the no-secrets diagnostics need; raw bodies
// are never printed on failure.
type sliceResponse struct {
	Status           int
	Header           http.Header
	Body             []byte
	TransferEncoding []string
}

func (r *sliceResponse) summary() string {
	if r == nil {
		return "<nil response>"
	}
	return fmt.Sprintf(
		"status=%d content-type=%q transfer-encoding=%v body-bytes=%d",
		r.Status, r.Header.Get("Content-Type"), r.TransferEncoding, len(r.Body),
	)
}

func sliceHTTP(t *testing.T, client *http.Client, method, url string, payload any, headers map[string]string) *sliceResponse {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s request: %v", method, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s request failed: %v", method, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s response body read failed: %v", method, err)
	}
	return &sliceResponse{
		Status:           response.StatusCode,
		Header:           response.Header.Clone(),
		Body:             body,
		TransferEncoding: append([]string(nil), response.TransferEncoding...),
	}
}

func sliceRequireStatus(t *testing.T, where string, response *sliceResponse, want int) {
	t.Helper()
	if response.Status != want {
		t.Fatalf("%s: want HTTP %d, got %s", where, want, response.summary())
	}
}

func sliceJSON(t *testing.T, where string, response *sliceResponse) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("%s: decode JSON failed: %v (%s)", where, err, response.summary())
	}
	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s: JSON root = %T, want object (%s)", where, value, response.summary())
	}
	return root
}

func sliceChunked(encodings []string) bool {
	for _, encoding := range encodings {
		if strings.EqualFold(encoding, "chunked") {
			return true
		}
	}
	return false
}

// sliceNoSecrets fails without printing either secret or the response body.
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
