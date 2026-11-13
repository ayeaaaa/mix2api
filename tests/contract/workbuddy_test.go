// WorkBuddy v2 regression contract suite.
//
// These tests lock down the v2 scoped WorkBuddy intl/cn behavior against a
// local fake upstream and the real provider/API composition. They are not
// captured v1.6.5 oracle output: v2 regression contract coverage is complete
// here, while v1.6.5 oracle parity remains pending. See
// tests/oracle/workbuddy.md.
package contract

import (
	"bytes"
	"encoding/json"
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
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/workbuddy"
)

const (
	workbuddyContractVersion    = "v2-workbuddy-contract"
	workbuddyContractPanelToken = "workbuddy-contract-panel-token"
	workbuddyContractAPIKey     = "workbuddy-contract-api-key"
	workbuddyContractModel      = "hy4-preview"
)

type workbuddyContractAccount struct {
	UID   string
	Realm string
	Token string
}

type workbuddyContractOptions struct {
	FallbackEnabled bool
}

type workbuddyContractFixture struct {
	server *http.Server
	intl   *workbuddy.Provider
	cn     *workbuddy.Provider
	store  *config.Store
	dir    string

	upstream *httptest.Server
	fake     *workbuddyContractUpstream

	accounts []workbuddyContractAccount
	secrets  []string
}

func newWorkBuddyContractFixture(
	t *testing.T,
	options workbuddyContractOptions,
	accounts ...workbuddyContractAccount,
) *workbuddyContractFixture {
	t.Helper()

	fake := newWorkBuddyContractUpstream()
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	t.Setenv("WB_INTL_CHAT_UPSTREAM", upstream.URL)
	t.Setenv("WB_CN_CHAT_UPSTREAM", upstream.URL)
	t.Setenv("WB_BILLING_UPSTREAM", upstream.URL)
	t.Setenv("WB_TASK_WEB_UPSTREAM", upstream.URL)

	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("create accounts dir: %v", err)
	}
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"daily_chat_web": false,
		"workbuddy": map[string]any{
			"intl": map[string]any{
				"enabled":                true,
				"timeout_seconds":        5,
				"stream_timeout_seconds": 30,
				"models_ttl_seconds":     300,
				"max_concurrent":         4,
				"realm_fallback_enabled": options.FallbackEnabled,
			},
			"cn": map[string]any{"enabled": true, "max_concurrent": 4},
		},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	secrets := make([]string, 0, len(accounts)*3)
	for _, account := range accounts {
		if account.Token == "" {
			account.Token = workbuddyContractOpaqueToken(account.UID)
		}
		saveWorkBuddyContractAccount(t, accountsDir, account)
		secrets = append(secrets, account.Token, "refresh-"+account.UID)
	}

	intl, err := workbuddy.New("intl", store)
	if err != nil {
		t.Fatalf("workbuddy.New(intl): %v", err)
	}
	cn, err := workbuddy.New("cn", store)
	if err != nil {
		t.Fatalf("workbuddy.New(cn): %v", err)
	}
	intl.SetTwin(cn)
	cn.SetTwin(intl)
	sharedLimit := intl.Config().MaxConcurrent
	if cnLimit := cn.Config().MaxConcurrent; cnLimit > sharedLimit {
		sharedLimit = cnLimit
	}
	sharedAdmission := workbuddy.NewAdmission(sharedLimit)
	intl.SetAdmission(sharedAdmission)
	cn.SetAdmission(sharedAdmission)
	activityPath := filepath.Join(accountsDir, workbuddy.ActivityHistoryFile)
	intl.SetActivityPath(activityPath)
	cn.SetActivityPath(activityPath)

	server := api.NewServer(api.ServerOptions{
		Version:    workbuddyContractVersion,
		Providers:  []providers.Provider{intl, cn},
		PanelAuth:  api.RequirePanelAuth(workbuddyContractPanelToken),
		APIKeyAuth: api.RequireAPIKeyAuth(workbuddyContractAPIKey),
	})

	return &workbuddyContractFixture{
		server:   server,
		intl:     intl,
		cn:       cn,
		store:    store,
		dir:      accountsDir,
		upstream: upstream,
		fake:     fake,
		accounts: append([]workbuddyContractAccount(nil), accounts...),
		secrets:  secrets,
	}
}

func saveWorkBuddyContractAccount(t *testing.T, dir string, account workbuddyContractAccount) {
	t.Helper()
	domain := "www.workbuddy.ai"
	if account.Realm == "cn" {
		domain = "www.workbuddy.cn"
	}
	token := account.Token
	if token == "" {
		token = workbuddyContractOpaqueToken(account.UID)
	}
	if _, err := workbuddy.SaveAccount(dir, workbuddy.Auth{
		UID:             account.UID,
		Nickname:        "Contract " + account.UID,
		Realm:           account.Realm,
		Domain:          domain,
		AccessToken:     token,
		RefreshToken:    "refresh-" + account.UID,
		ExpiresAt:       time.Now().Add(4 * time.Hour),
		Platform:        "CLI",
		Product:         "workbuddy",
		Source:          "oauth",
		Enabled:         true,
		EnabledSet:      true,
		TasksEnabled:    true,
		TasksEnabledSet: true,
	}); err != nil {
		t.Fatalf("save workbuddy account %s: %v", account.UID, err)
	}
}

func workbuddyContractOpaqueToken(uid string) string {
	return "wb-contract-" + uid + "-access-0123456789"
}

func (f *workbuddyContractFixture) do(
	t *testing.T,
	method, path string,
	body any,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	f.server.Handler.ServeHTTP(recorder, request)
	return recorder
}

func (f *workbuddyContractFixture) doPanel(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, body, map[string]string{
		"X-Panel-Token": workbuddyContractPanelToken,
	})
}

func (f *workbuddyContractFixture) doAPI(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, body, map[string]string{
		"Authorization": "Bearer " + workbuddyContractAPIKey,
	})
}

func (f *workbuddyContractFixture) assertNoSecrets(t *testing.T, where string, body []byte) {
	t.Helper()
	text := string(body)
	for _, secret := range f.secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("%s leaked a credential", where)
		}
	}
	for _, extra := range []string{
		"intl-contract-refreshed-access-0123456789",
		"intl-contract-refreshed-refresh-0123456789",
	} {
		if strings.Contains(text, extra) {
			t.Fatalf("%s leaked a refreshed credential", where)
		}
	}
	for _, field := range []string{
		"accessToken", "refreshToken", "access_token", "refresh_token",
	} {
		if strings.Contains(text, field) {
			t.Fatalf("%s leaked credential field %q", where, field)
		}
	}
}

const (
	workbuddyFakeChatPath    = "/v2/chat/completions"
	workbuddyFakeRefreshPath = "/v2/plugin/auth/token/refresh"
	workbuddyFakeCatalogPath = "/v3/config"
)

// workbuddyContractUpstream is the deterministic local stand-in for the
// WorkBuddy fake upstream. Per-token modes select text, reasoning, tools, or
// images without any real network or account.
type workbuddyContractUpstream struct {
	mu sync.Mutex

	calls []workbuddyContractCall

	modes             map[string]string
	failAllIntlChat   bool
	unauthorizedToken string
	refreshCalls      int
	chatCalls         int
}

type workbuddyContractCall struct {
	Method string
	Path   string
	Token  string
	Body   map[string]any
	Raw    []byte
}

func newWorkBuddyContractUpstream() *workbuddyContractUpstream {
	return &workbuddyContractUpstream{modes: make(map[string]string)}
}

func (f *workbuddyContractUpstream) setMode(uid, mode string) {
	f.mu.Lock()
	f.modes[uid] = mode
	f.mu.Unlock()
}

func (f *workbuddyContractUpstream) setUnauthorizedToken(token string) {
	f.mu.Lock()
	f.unauthorizedToken = token
	f.mu.Unlock()
}

func (f *workbuddyContractUpstream) counts() (chat, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatCalls, f.refreshCalls
}

func (f *workbuddyContractUpstream) countPath(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call.Path == path {
			count++
		}
	}
	return count
}

func (f *workbuddyContractUpstream) snapshot() []workbuddyContractCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]workbuddyContractCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *workbuddyContractUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	f.mu.Lock()
	f.calls = append(f.calls, workbuddyContractCall{
		Method: r.Method,
		Path:   r.URL.Path,
		Token:  token,
		Body:   body,
		Raw:    append([]byte(nil), raw...),
	})
	f.mu.Unlock()

	switch r.URL.Path {
	case workbuddyFakeRefreshPath:
		f.mu.Lock()
		f.refreshCalls++
		f.mu.Unlock()
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"accessToken":  "intl-contract-refreshed-access-0123456789",
				"refreshToken": "intl-contract-refreshed-refresh-0123456789",
				"expiresAt":    4102444800,
			},
		})
	case workbuddyFakeCatalogPath:
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"agents": map[string]any{
					"cli": map[string]any{"models": []any{
						workbuddyContractModel, "hy3", "deepseek-v4.1-flash", "glm-5.3", "remote-only-contract",
					}},
				},
				"models": []any{
					map[string]any{
						"id": "remote-only-contract", "name": "Remote Only Contract",
						"descriptionEn": "live remote membership", "credits": "x0.5",
						"supportsToolCall": true, "supportsReasoning": true,
					},
				},
			},
		})
	case workbuddyFakeChatPath:
		f.handleChat(w, r, token, raw)
	case "/billing/meter/get-user-resource-summary":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"IsPaidUser": false,
				"Packages": []any{
					map[string]any{
						"CycleTotalCapacity":  "100",
						"CycleRemainCapacity": "60",
						"CycleUsedCapacity":   "40",
					},
				},
			},
		})
	case "/v2/billing/meter/checkin-activity-status":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"today_checked_in": false,
				"streak_days":      2,
				"daily_credit":     5,
				"today_credit":     0,
				"active":           true,
			},
		})
	case "/v2/billing/meter/daily-checkin":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0, "msg": "签到成功", "data": map[string]any{},
		})
	case "/v2/report":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "ok"})
	case "/v2/activity/growth/tasks":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"tasks": []any{
					map[string]any{
						"task_code":     "daily_chat",
						"title":         "Daily chat",
						"description":   "Send one daily message",
						"accept_status": "accepted",
						"progress":      map[string]any{"current": 1, "target": 1},
						"reward_credit": 5,
					},
				},
			},
		})
	case "/v2/activity/growth/energy":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"balance": 10},
		})
	case "/activity/growth/streak", "/v2/activity/growth/streak":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"streak":            map[string]any{"days": 2},
				"makeup_cards":      map[string]any{"balance": 0},
				"redemption_status": map[string]any{"tier_7d_status": "locked", "tier_14d_status": "locked", "tier_28d_status": "locked"},
			},
		})
	case "/activity/growth/lottery/summary":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"chances": 1},
		})
	case "/activity/growth/buddy/travel/status":
		writeWorkBuddyContractJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"state": "idle"},
		})
	default:
		writeWorkBuddyContractJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "unknown fake endpoint: " + r.URL.Path},
		})
	}
}

func (f *workbuddyContractUpstream) handleChat(w http.ResponseWriter, r *http.Request, token string, raw []byte) {
	f.mu.Lock()
	f.chatCalls++
	intl := strings.HasPrefix(token, "wb-contract-") && !strings.Contains(token, "-cn-")
	failAll := f.failAllIntlChat
	unauthorized := f.unauthorizedToken
	mode := f.modeForTokenLocked(token)
	f.mu.Unlock()

	if intl && failAll {
		writeWorkBuddyContractJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"message": "usage exceeds frequency limit"},
		})
		return
	}
	if unauthorized != "" && token == unauthorized {
		writeWorkBuddyContractJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "token expired"},
		})
		return
	}

	content := "hello workbuddy"
	switch mode {
	case "reasoning":
		content = "answer"
	case "image":
		content = "image-ok"
	case "failover":
		content = "cn-ok"
	}
	if strings.Contains(token, "-cn-") && mode == "" {
		content = "cn-ok"
	}
	_ = raw

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	switch mode {
	case "reasoning":
		f.writeFrames(w,
			`{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,"model":"`+workbuddyContractModel+`","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking "},"finish_reason":null}]}`,
			`{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,"model":"`+workbuddyContractModel+`","choices":[{"index":0,"delta":{"content":"`+content+`"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,"model":"`+workbuddyContractModel+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)
	case "tool":
		f.writeFrames(w,
			`{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,"model":"`+workbuddyContractModel+`","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_contract","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,"model":"`+workbuddyContractModel+`","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)
	default:
		f.writeFrames(w,
			`{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,"model":"`+workbuddyContractModel+`","choices":[{"index":0,"delta":{"role":"assistant","content":"`+content+`"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-contract","object":"chat.completion.chunk","created":1700000000,"model":"`+workbuddyContractModel+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)
	}
}

func (f *workbuddyContractUpstream) modeForTokenLocked(token string) string {
	for uid, mode := range f.modes {
		if strings.Contains(token, uid) {
			return mode
		}
	}
	return ""
}

func (f *workbuddyContractUpstream) writeFrames(w http.ResponseWriter, payloads ...string) {
	for _, payload := range payloads {
		_, _ = io.WriteString(w, "data: "+payload+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeWorkBuddyContractJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func workbuddyContractModelIDs(t *testing.T, root map[string]any) []string {
	t.Helper()
	data, ok := root["data"].([]any)
	if !ok {
		t.Fatalf("models data = %T", root["data"])
	}
	ids := make([]string, 0, len(data))
	for _, raw := range data {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("model entry = %T", raw)
		}
		id, _ := entry["id"].(string)
		if id == "" {
			t.Fatalf("model entry without id: %#v", entry)
		}
		ids = append(ids, id)
	}
	return ids
}

func workbuddyContractUnion(intl, cn []string) []string {
	seen := make(map[string]bool, len(intl)+len(cn))
	out := make([]string, 0, len(intl)+len(cn))
	for _, id := range append(append([]string(nil), intl...), cn...) {
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, id)
	}
	return out
}

func workbuddyContractHasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func workbuddyContractFindModel(root map[string]any, id string) map[string]any {
	data, _ := root["data"].([]any)
	for _, raw := range data {
		entry, _ := raw.(map[string]any)
		if entry != nil && entry["id"] == id {
			return entry
		}
	}
	return nil
}

func workbuddyContractCompletion(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	root := decodeJSONBody(t, recorder.Body.Bytes())
	choices, _ := root["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v", root["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return message
}

func workbuddyContractPrepareUpstream(body []byte, upstreamURL string) []byte {
	return bytes.ReplaceAll(body, []byte(upstreamURL), []byte("<upstream>"))
}

func workbuddyContractPrepareAccounts(body []byte, accountsDir string) []byte {
	return bytes.ReplaceAll(body, []byte(accountsDir), []byte("<accounts>"))
}

func workbuddyContractAssertGoldenJSON(
	t *testing.T,
	actual []byte,
	name string,
	prepare func([]byte) []byte,
) {
	t.Helper()
	prepared := actual
	if prepare != nil {
		prepared = prepare(actual)
	}
	if os.Getenv("UPDATE_WORKBUDDY_GOLDENS") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(filepath.Join("testdata", name), prepared, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if err := normalizedJSONMismatch(prepared, raw); err != nil {
		t.Fatalf("golden %s mismatch: %v", name, err)
	}
}

func workbuddyContractAssertGoldenSSE(t *testing.T, actual []byte, name string) {
	t.Helper()
	events, err := NormalizeSSE(actual)
	if err != nil {
		t.Fatalf("NormalizeSSE: %v", err)
	}
	prepared, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		t.Fatalf("marshal normalized SSE: %v", err)
	}
	prepared = append(prepared, '\n')
	if os.Getenv("UPDATE_WORKBUDDY_GOLDENS") == "1" {
		if err := os.WriteFile(filepath.Join("testdata", name), prepared, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var want []Event
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&want); err != nil {
		t.Fatalf("decode golden %s: %v", name, err)
	}
	gotMarshaled, _ := json.Marshal(events)
	wantMarshaled, _ := json.Marshal(want)
	if string(gotMarshaled) != string(wantMarshaled) {
		t.Fatalf("SSE golden %s mismatch:\ngot:  %s\nwant: %s", name, gotMarshaled, wantMarshaled)
	}
}

func TestWorkBuddyContractHealthModelsCatalog(t *testing.T) {
	t.Run("health", func(t *testing.T) {
		fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-contract", Realm: "intl"},
			workbuddyContractAccount{UID: "cn-contract", Realm: "cn"},
		)
		rec := fixture.doAPI(t, http.MethodGet, "/health?force=1", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("health status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "health", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_health.golden.json", func(body []byte) []byte {
			return workbuddyContractPrepareUpstream(body, fixture.upstream.URL)
		})
	})

	t.Run("models", func(t *testing.T) {
		fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-contract", Realm: "intl"},
			workbuddyContractAccount{UID: "cn-contract", Realm: "cn"},
		)
		intlRec := fixture.doAPI(t, http.MethodGet, "/v1/models?realm=intl", nil)
		if intlRec.Code != http.StatusOK {
			t.Fatalf("models intl status = %d body=%s", intlRec.Code, intlRec.Body)
		}
		fixture.assertNoSecrets(t, "models intl", intlRec.Body.Bytes())
		intlRoot := decodeJSONBody(t, intlRec.Body.Bytes())
		intlIDs := workbuddyContractModelIDs(t, intlRoot)
		if intlRoot["realm"] != "intl" || len(intlIDs) == 0 {
			t.Fatalf("intl models envelope = %#v", intlRoot)
		}
		workbuddyContractAssertGoldenJSON(t, intlRec.Body.Bytes(), "workbuddy_models_intl.golden.json", nil)

		cnRec := fixture.doAPI(t, http.MethodGet, "/v1/models?realm=cn", nil)
		if cnRec.Code != http.StatusOK {
			t.Fatalf("models cn status = %d body=%s", cnRec.Code, cnRec.Body)
		}
		fixture.assertNoSecrets(t, "models cn", cnRec.Body.Bytes())
		cnRoot := decodeJSONBody(t, cnRec.Body.Bytes())
		cnIDs := workbuddyContractModelIDs(t, cnRoot)
		if cnRoot["realm"] != "cn" || len(cnIDs) == 0 {
			t.Fatalf("cn models envelope = %#v", cnRoot)
		}
		if !workbuddyContractHasID(intlIDs, "remote-only-contract") {
			t.Fatalf("intl models did not consume the live remote-only model: %v", intlIDs)
		}
		if entry := workbuddyContractFindModel(intlRoot, "remote-only-contract"); entry == nil ||
			entry["name"] != "Remote Only Contract" || entry["description"] != "live remote membership" {
			t.Fatalf("remote-only model metadata = %#v", entry)
		}
		if calls := fixture.fake.countPath(workbuddyFakeCatalogPath); calls < 2 {
			t.Fatalf("live catalog calls = %d, want one per realm", calls)
		}

		allRec := fixture.doAPI(t, http.MethodGet, "/v1/models?realm=all", nil)
		if allRec.Code != http.StatusOK {
			t.Fatalf("models all status = %d body=%s", allRec.Code, allRec.Body)
		}
		fixture.assertNoSecrets(t, "models all", allRec.Body.Bytes())
		allRoot := decodeJSONBody(t, allRec.Body.Bytes())
		allIDs := workbuddyContractModelIDs(t, allRoot)
		want := workbuddyContractUnion(intlIDs, cnIDs)
		if strings.Join(allIDs, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("models all = %v, want exact union %v", allIDs, want)
		}
		if !workbuddyContractHasID(allIDs, "glm-5.2") || workbuddyContractHasID(intlIDs, "glm-5.2") {
			t.Fatalf("realm=all did not include the distinct CN-only glm-5.2: intl=%v cn=%v", intlIDs, cnIDs)
		}
		workbuddyContractAssertGoldenJSON(t, allRec.Body.Bytes(), "workbuddy_models_all.golden.json", nil)
	})

	t.Run("catalog_refresh", func(t *testing.T) {
		fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-contract", Realm: "intl"},
			workbuddyContractAccount{UID: "cn-contract", Realm: "cn"},
		)
		rec := fixture.doPanel(t, http.MethodPost, "/catalog/refresh", map[string]any{"realm": "intl"})
		if rec.Code != http.StatusOK {
			t.Fatalf("catalog refresh status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "catalog refresh", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_catalog_refresh.golden.json", nil)
	})
}

func TestWorkBuddyContractChatSSEToolsImagesReasoning(t *testing.T) {
	newFixture := func(t *testing.T, mode string) *workbuddyContractFixture {
		t.Helper()
		fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-chat", Realm: "intl"},
		)
		fixture.fake.setMode("intl-chat", mode)
		return fixture
	}

	t.Run("nonstream", func(t *testing.T) {
		fixture := newFixture(t, "text")
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddyContractModel,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   false,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("chat nonstream status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "chat nonstream", rec.Body.Bytes())
		message := workbuddyContractCompletion(t, rec)
		if message["content"] != "hello workbuddy" {
			t.Fatalf("chat content = %#v", message["content"])
		}
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_chat_nonstream.golden.json", nil)
	})

	t.Run("stream", func(t *testing.T) {
		fixture := newFixture(t, "text")
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddyContractModel,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   true,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("chat stream status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "chat stream", rec.Body.Bytes())
		if !bytes.Contains(rec.Body.Bytes(), []byte("data: [DONE]")) {
			t.Fatal("chat stream has no [DONE]")
		}
		workbuddyContractAssertGoldenSSE(t, rec.Body.Bytes(), "workbuddy_chat_stream.golden.json")
	})

	t.Run("tools", func(t *testing.T) {
		fixture := newFixture(t, "tool")
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddyContractModel,
			"messages": []any{map[string]any{"role": "user", "content": "look it up"}},
			"stream":   true,
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "lookup", "parameters": map[string]any{"type": "object"},
				},
			}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("chat tools status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "chat tools", rec.Body.Bytes())
		workbuddyContractAssertGoldenSSE(t, rec.Body.Bytes(), "workbuddy_chat_stream_tool.golden.json")
	})

	t.Run("reasoning", func(t *testing.T) {
		fixture := newFixture(t, "reasoning")
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":            workbuddyContractModel,
			"messages":         []any{map[string]any{"role": "user", "content": "think"}},
			"stream":           true,
			"reasoning_effort": "high",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("chat reasoning status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "chat reasoning", rec.Body.Bytes())
		events, err := NormalizeSSE(rec.Body.Bytes())
		if err != nil || len(events) == 0 || events[0].ReasoningContent != "thinking " || events[0].Content != "answer" {
			t.Fatalf("reasoning events = %#v err=%v", events, err)
		}
		workbuddyContractAssertGoldenSSE(t, rec.Body.Bytes(), "workbuddy_chat_stream_reasoning.golden.json")
	})

	t.Run("images", func(t *testing.T) {
		fixture := newFixture(t, "image")
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model": workbuddyContractModel,
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what is this?"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
				},
			}},
			"stream": false,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("chat images status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "chat images", rec.Body.Bytes())
		if message := workbuddyContractCompletion(t, rec); message["content"] != "image-ok" {
			t.Fatalf("image completion = %#v", message)
		}
		if !workbuddyContractSawImagePart(fixture.fake.snapshot()) {
			t.Fatal("image content part was not forwarded upstream")
		}
	})
}

func workbuddyContractSawImagePart(calls []workbuddyContractCall) bool {
	for _, call := range calls {
		if call.Path != workbuddyFakeChatPath {
			continue
		}
		if bytes.Contains(call.Raw, []byte("image_url")) && bytes.Contains(call.Raw, []byte("data:image/png")) {
			return true
		}
	}
	return false
}

func TestWorkBuddyContractErrors429401Failover(t *testing.T) {
	t.Run("429 error", func(t *testing.T) {
		fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-429", Realm: "intl"},
		)
		fixture.fake.failAllIntlChat = true
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddyContractModel,
			"messages": []any{map[string]any{"role": "user", "content": "429"}},
			"stream":   false,
		})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("429 status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "429", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_error_429.golden.json", nil)
	})

	t.Run("401 refresh", func(t *testing.T) {
		const oldToken = "wb-contract-intl-401-access-0123456789"
		fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-401", Realm: "intl", Token: oldToken},
		)
		fixture.fake.setUnauthorizedToken(oldToken)
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddyContractModel,
			"messages": []any{map[string]any{"role": "user", "content": "401"}},
			"stream":   false,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("401 refresh status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "401 refresh", rec.Body.Bytes())
		if message := workbuddyContractCompletion(t, rec); message["content"] != "hello workbuddy" {
			t.Fatalf("401 refresh completion = %#v", message)
		}
		_, refreshCalls := fixture.fake.counts()
		if refreshCalls != 1 {
			t.Fatalf("refresh calls = %d, want 1", refreshCalls)
		}
	})

	t.Run("cross-exit failover", func(t *testing.T) {
		fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{FallbackEnabled: true},
			workbuddyContractAccount{UID: "intl-failover", Realm: "intl"},
			workbuddyContractAccount{UID: "cn-failover", Realm: "cn"},
		)
		fixture.fake.failAllIntlChat = true
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    workbuddyContractModel,
			"messages": []any{map[string]any{"role": "user", "content": "fallback"}},
			"stream":   false,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("failover status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "failover", rec.Body.Bytes())
		if message := workbuddyContractCompletion(t, rec); message["content"] != "cn-ok" {
			t.Fatalf("failover completion = %#v", message)
		}
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_chat_failover.golden.json", nil)
	})
}

func TestWorkBuddyContractCreditsGuardsCheckinDailyGrowth(t *testing.T) {
	newTaskFixture := func(t *testing.T) *workbuddyContractFixture {
		t.Helper()
		return newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-task", Realm: "intl"},
			workbuddyContractAccount{UID: "cn-task", Realm: "cn"},
		)
	}

	t.Run("credits", func(t *testing.T) {
		fixture := newTaskFixture(t)
		rec := fixture.doPanel(t, http.MethodPost, "/accounts/credits?realm=cn", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("credits status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "credits", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_credits_refresh.golden.json", func(body []byte) []byte {
			return workbuddyContractPrepareAccounts(body, fixture.dir)
		})
	})

	t.Run("guard", func(t *testing.T) {
		guarded := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
			workbuddyContractAccount{UID: "intl-guard", Realm: "intl"},
		)
		guarded.intl.ApplyLimits(
			map[string]any{"daily_token_limit": map[string]any{"intl": 1}},
			map[string]any{"tokens": map[string]any{"intl-guard": 1}},
		)
		rec := guarded.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddyContractModel,
			"messages": []any{map[string]any{"role": "user", "content": "guarded"}},
			"stream":   false,
		})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("guard status = %d body=%s", rec.Code, rec.Body)
		}
		guarded.assertNoSecrets(t, "guard", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_error_guard.golden.json", nil)
	})

	t.Run("checkin", func(t *testing.T) {
		fixture := newTaskFixture(t)
		rec := fixture.doPanel(t, http.MethodPost, "/accounts/checkin?realm=cn", map[string]any{"uid": "cn-task"})
		if rec.Code != http.StatusOK {
			t.Fatalf("checkin status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "checkin", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_checkin.golden.json", func(body []byte) []byte {
			return workbuddyContractPrepareAccounts(body, fixture.dir)
		})
	})

	t.Run("daily", func(t *testing.T) {
		fixture := newTaskFixture(t)
		rec := fixture.doPanel(t, http.MethodPost, "/accounts/daily-chat", map[string]any{"uid": "intl-task"})
		if rec.Code != http.StatusOK {
			t.Fatalf("daily status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "daily", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_daily.golden.json", func(body []byte) []byte {
			return workbuddyContractPrepareAccounts(body, fixture.dir)
		})
	})

	t.Run("growth", func(t *testing.T) {
		fixture := newTaskFixture(t)
		rec := fixture.doPanel(t, http.MethodGet, "/tasks?realm=cn", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("growth status = %d body=%s", rec.Code, rec.Body)
		}
		fixture.assertNoSecrets(t, "growth", rec.Body.Bytes())
		workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_growth.golden.json", func(body []byte) []byte {
			return workbuddyContractPrepareAccounts(body, fixture.dir)
		})
	})
}

func TestWorkBuddyContractAdmin(t *testing.T) {
	fixture := newWorkBuddyContractFixture(t, workbuddyContractOptions{},
		workbuddyContractAccount{UID: "intl-admin", Realm: "intl"},
		workbuddyContractAccount{UID: "cn-admin", Realm: "cn"},
	)
	rec := fixture.doPanel(t, http.MethodGet, "/accounts?realm=all", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin accounts status = %d body=%s", rec.Code, rec.Body)
	}
	fixture.assertNoSecrets(t, "admin accounts", rec.Body.Bytes())
	root := decodeJSONBody(t, rec.Body.Bytes())
	accounts, _ := root["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("admin accounts = %#v", root["accounts"])
	}
	workbuddyContractAssertGoldenJSON(t, rec.Body.Bytes(), "workbuddy_admin_accounts.golden.json", func(body []byte) []byte {
		return workbuddyContractPrepareAccounts(body, fixture.dir)
	})
}
