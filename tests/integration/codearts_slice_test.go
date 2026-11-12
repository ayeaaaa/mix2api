package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

const (
	codeartsSliceVersion = "2.0.0-codearts-integration"
	codeartsSliceUID     = "codearts-slice-user"
)

// codeartsSliceLiveServer wires the real api.Server handler over a real
// listener whose port is known before api.NewServer is constructed. That is the
// command's exact listener-port path: the bound port, not the request Host, is
// injected into providers implementing SetListenerPort.
type codeartsSliceLiveServer struct {
	server *httptest.Server
	port   int
}

func newCodeartsSliceLiveServer(t *testing.T, setup *app.Setup, panelPassword, apiKey string) *codeartsSliceLiveServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		t.Fatalf("listener address %T is not TCP", listener.Addr())
	}
	port := tcpAddr.Port

	server := api.NewServer(api.ServerOptions{
		Version:      codeartsSliceVersion,
		ListenerPort: port,
		Providers:    setup.Providers,
		PanelAuth:    api.RequirePanelAuth(panelPassword),
		APIKeyAuth:   api.RequireAPIKeyAuth(apiKey),
	})
	live := httptest.NewUnstartedServer(server.Handler)
	live.Listener = listener
	live.Start()
	t.Cleanup(live.Close)
	return &codeartsSliceLiveServer{server: live, port: port}
}

func codeartsSliceSettings(t *testing.T, accountsDir string, enabled bool) {
	t.Helper()
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("create accounts dir: %v", err)
	}
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"codearts": map[string]any{
			"enabled":                enabled,
			"timeout_seconds":        5,
			"stream_timeout_seconds": 30,
			"benefit_auto_claim":     true,
		},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// TestCodeArtsSliceOverHTTP drives the real command composition: env-selected
// directories, the shared settings store, app.Build registration, the generic
// API server and all CodeArts admin/v1 endpoints. Both the gateway and fake
// upstream are real TCP HTTP servers; no test reaches the real network.
func TestCodeArtsSliceOverHTTP(t *testing.T) {
	fake := newCodeartsFakeUpstream()
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	t.Setenv("WB_CODEARTS_SNAP_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_SNAP_MANAGER", upstream.URL+"/snap-manager")
	t.Setenv("WB_CODEARTS_BENEFIT_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_PORTAL_HOST", "https://portal.example.test/portal")
	// The slice only exercises CodeArts. Keep Raccoon's constructor away from
	// any real endpoint as well.
	t.Setenv("WB_RACCOON_BASE_URL", upstream.URL)

	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	codeartsSliceSettings(t, accountsDir, true)

	setup, err := app.Build(app.Paths{
		AccountsDir: accountsDir,
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	if setup.Store == nil {
		t.Fatal("app.Build returned a nil settings store")
	}
	if got, want := setup.Store.Path, filepath.Join(accountsDir, "settings.json"); got != want {
		t.Fatalf("store path = %q, want %q", got, want)
	}
	codeartsSliceRequireRegistration(t, setup)

	live := newCodeartsSliceLiveServer(t, setup, slicePanelPassword, sliceAPIKey)
	client := &http.Client{Timeout: 15 * time.Second}
	panelHeaders := map[string]string{"X-Panel-Token": slicePanelPassword}
	apiHeaders := map[string]string{"Authorization": "Bearer " + sliceAPIKey}

	now := time.Now().UTC().Truncate(time.Second)
	const uid = codeartsSliceUID
	accessToken := "codearts-cloud-dragon-token-slice-0123456789"
	accessKey := "AK-CODEARTS-SLICE-ACCESS-0123456789"
	secretKey := "codearts-secret-access-key-slice-0123456789"
	refreshToken := "codearts-refresh-token-slice-0123456789"
	expiration := now.Add(4 * time.Hour).Format(time.RFC3339)
	secrets := []string{accessToken, accessKey, secretKey, refreshToken}

	// Unauthenticated panel and v1 requests must be rejected before the
	// provider or its filesystem is touched.
	for _, rejection := range []struct {
		name    string
		method  string
		path    string
		payload any
	}{
		{name: "import", method: http.MethodPost, path: "/codearts/accounts/import", payload: map[string]any{}},
		{name: "overview", method: http.MethodGet, path: "/codearts/overview", payload: nil},
		{name: "chat", method: http.MethodPost, path: "/v1/chat/completions?realm=codearts", payload: map[string]any{
			"model": "DeepSeek-V4.1-Flash-1017", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}},
		{name: "models", method: http.MethodGet, path: "/v1/models?realm=codearts", payload: nil},
	} {
		rec := sliceHTTP(t, client, rejection.method, live.server.URL+rejection.path, rejection.payload, nil)
		sliceRequireStatus(t, "unauthenticated "+rejection.name, rec, http.StatusUnauthorized)
	}

	// 1. Import a realistic v1 auth document. The realm starts enabled, the
	// account lands in the pool, and the canonical auth file is written with
	// an empty proxySlot and 0600 permissions.
	rec := sliceHTTP(t, client, http.MethodPost, live.server.URL+"/codearts/accounts/import", map[string]any{
		"user_id":            uid,
		"user_name":          "Slice User",
		"domain_id":          "domain-slice",
		"cloud_dragon_token": accessToken,
		"access_key_id":      accessKey,
		"secret_access_key":  secretKey,
		"refresh_token":      refreshToken,
		"client_id":          "codearts-agent",
		"expiration":         expiration,
		"proxySlot":          "",
	}, panelHeaders)
	sliceRequireStatus(t, "import", rec, http.StatusOK)
	imported := sliceJSON(t, "import", rec)
	if imported["ok"] != true || imported["enabled"] != true || imported["uid"] != uid {
		t.Fatalf("import envelope: ok=%v enabled=%v uid=%v", imported["ok"], imported["enabled"], imported["uid"])
	}
	account, ok := imported["account"].(map[string]any)
	if !ok || account["uid"] != uid {
		t.Fatalf("import account = %#v", imported["account"])
	}
	if account["proxy_slot"] != "" || account["proxy"] != "" {
		t.Fatalf("import proxy fields = %v/%v, want empty", account["proxy_slot"], account["proxy"])
	}
	sliceNoSecrets(t, "import", rec.Body, secrets...)

	authPath := filepath.Join(accountsDir, "codearts", "codearts-"+uid+".json")
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("imported auth file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("imported auth mode = %o, want 600", perm)
	}
	rawAuth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read imported auth: %v", err)
	}
	var authDoc map[string]any
	if err := json.Unmarshal(rawAuth, &authDoc); err != nil {
		t.Fatalf("decode imported auth: %v", err)
	}
	if authDoc["user_id"] != uid || authDoc["cloud_dragon_token"] != accessToken ||
		authDoc["access_key_id"] != accessKey || authDoc["secret_access_key"] != secretKey ||
		authDoc["refresh_token"] != refreshToken || authDoc["proxySlot"] != "" ||
		authDoc["expiration"] != expiration {
		t.Fatal("imported auth document does not match the submitted realistic credentials")
	}

	settings, err := setup.Store.Load()
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	codeartsSettings, ok := settings["codearts"].(map[string]any)
	if !ok || codeartsSettings["enabled"] != true {
		t.Fatalf("codearts settings = %#v", settings["codearts"])
	}

	// 2. Overview sees the imported account, the fake statistics snapshot and
	// no credentials. Accounts reuses the same public view.
	rec = sliceHTTP(t, client, http.MethodGet, live.server.URL+"/codearts/overview", nil, panelHeaders)
	sliceRequireStatus(t, "overview", rec, http.StatusOK)
	overview := sliceJSON(t, "overview", rec)
	if overview["service"] != "codearts" || overview["online"] != true {
		t.Fatalf("overview identity = %v/%v", overview["service"], overview["online"])
	}
	accounts, ok := overview["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("overview accounts = %#v", overview["accounts"])
	}
	row, ok := accounts[0].(map[string]any)
	if !ok || row["uid"] != uid || row["nickname"] != "Slice User" {
		t.Fatalf("overview account row = %#v", accounts[0])
	}
	stats, ok := overview["stats"].(map[string]any)
	if !ok || stats["total"] != float64(1) || stats["credits"] != float64(900) {
		t.Fatalf("overview stats = %#v", overview["stats"])
	}
	statsHealth, ok := overview["health"].(map[string]any)
	if !ok || statsHealth["ok"] != true || statsHealth["accounts"] != float64(1) {
		t.Fatalf("overview health = %#v", overview["health"])
	}
	sliceNoSecrets(t, "overview", rec.Body, secrets...)

	rec = sliceHTTP(t, client, http.MethodGet, live.server.URL+"/codearts/accounts", nil, panelHeaders)
	sliceRequireStatus(t, "accounts", rec, http.StatusOK)
	accountsBody := sliceJSON(t, "accounts", rec)
	if accountsBody["provider"] != "codearts" {
		t.Fatalf("accounts provider = %v", accountsBody["provider"])
	}
	if list, ok := accountsBody["accounts"].([]any); !ok || len(list) != 1 {
		t.Fatalf("accounts list = %#v", accountsBody["accounts"])
	}
	sliceNoSecrets(t, "accounts", rec.Body, secrets...)

	// 3. Generic model discovery merges agent/builtin/benefit sources with the
	// published catalogue. The display id is exact and the lowercase alias is
	// not advertised by /v1/models.
	rec = sliceHTTP(t, client, http.MethodGet, live.server.URL+"/v1/models?realm=codearts", nil, apiHeaders)
	sliceRequireStatus(t, "models", rec, http.StatusOK)
	models := sliceJSON(t, "models", rec)
	if models["object"] != "list" || models["realm"] != "codearts" {
		t.Fatalf("models object/realm = %v/%v", models["object"], models["realm"])
	}
	data, ok := models["data"].([]any)
	if !ok {
		t.Fatalf("models data = %T", models["data"])
	}
	modelIDs := make(map[string]bool, len(data))
	for _, item := range data {
		entry, _ := item.(map[string]any)
		id, _ := entry["id"].(string)
		if id == "" {
			t.Fatalf("model entry without id: %#v", entry)
		}
		if modelIDs[id] {
			t.Fatalf("duplicate model id %q", id)
		}
		modelIDs[id] = true
	}
	for _, id := range []string{
		"DeepSeek-V4.1-Flash-1017",
		"GLM-5.2",
		"GLM-5.3",
		"deepseek-v4-flash-0731",
		"Agent-Model",
	} {
		if !modelIDs[id] {
			t.Fatalf("models missing exact id %q (have %v)", id, sortedCodeartsModelIDs(modelIDs))
		}
	}
	if modelIDs["deepseek-v4.1-flash-1017"] {
		t.Fatal("/v1/models advertised the lowercase chat alias instead of the display id")
	}
	sliceNoSecrets(t, "models", rec.Body, secrets...)

	// 4. Non-stream chat aggregates the upstream SSE stream.
	rec = sliceHTTP(t, client, http.MethodPost, live.server.URL+"/v1/chat/completions?realm=codearts", map[string]any{
		"model":    "DeepSeek-V4.1-Flash-1017",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   false,
	}, apiHeaders)
	sliceRequireStatus(t, "chat nonstream", rec, http.StatusOK)
	completion := sliceJSON(t, "chat nonstream", rec)
	if completion["object"] != "chat.completion" {
		t.Fatalf("completion object = %v", completion["object"])
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
		t.Fatalf("completion finish_reason = %v", choice["finish_reason"])
	}
	usage, ok := completion["usage"].(map[string]any)
	if !ok || usage["total_tokens"] != float64(5) {
		t.Fatalf("completion usage = %#v", completion["usage"])
	}
	sliceNoSecrets(t, "chat nonstream", rec.Body, secrets...)

	// 5. Stream chat passes normalized SSE through a real chunked response.
	rec = sliceHTTP(t, client, http.MethodPost, live.server.URL+"/v1/chat/completions?realm=codearts", map[string]any{
		"model":    "DeepSeek-V4.1-Flash-1017",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   true,
	}, apiHeaders)
	sliceRequireStatus(t, "chat stream", rec, http.StatusOK)
	if got := rec.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("chat stream content type = %q", got)
	}
	if !sliceChunked(rec.TransferEncoding) {
		t.Fatalf("chat stream transfer encoding = %v, want chunked", rec.TransferEncoding)
	}
	streamText := string(rec.Body)
	if !strings.Contains(streamText, `"content":"你"`) ||
		!strings.HasSuffix(streamText, "data: [DONE]\n\n") {
		t.Fatalf("chat stream framing missing delta or DONE (%s)", rec.summary())
	}
	sliceNoSecrets(t, "chat stream", rec.Body, secrets...)

	// The generic API preserves the provider's HTTPError status mapping: an
	// upstream 400 bad request stays 400 with the safe provider message.
	rec = sliceHTTP(t, client, http.MethodPost, live.server.URL+"/v1/chat/completions?realm=codearts", map[string]any{
		"model":    "codearts-slice-error-model",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   false,
	}, apiHeaders)
	sliceRequireStatus(t, "chat bad request", rec, http.StatusBadRequest)
	failure := codeartsSliceError(t, "chat bad request", rec)
	if failure["message"] != "request param is invalid" || failure["type"] != "invalid_request_error" ||
		failure["code"] != float64(http.StatusBadRequest) {
		t.Fatalf("chat bad request error = %#v", failure)
	}
	sliceNoSecrets(t, "chat bad request", rec.Body, secrets...)

	// 6. Manual check-in claims and confirms the fake daily campaign.
	rec = sliceHTTP(t, client, http.MethodPost, live.server.URL+"/codearts/accounts/checkin",
		map[string]any{"uid": uid}, panelHeaders)
	sliceRequireStatus(t, "checkin", rec, http.StatusOK)
	checkin := sliceJSON(t, "checkin", rec)
	if checkin["ok"] != true || checkin["credits"] != float64(10) {
		t.Fatalf("checkin = %#v", checkin)
	}
	campaigns, ok := checkin["campaigns"].([]any)
	if !ok || len(campaigns) != 1 {
		t.Fatalf("checkin campaigns = %#v", checkin["campaigns"])
	}
	campaign, _ := campaigns[0].(map[string]any)
	if campaign["campaign_id"] != float64(1) || campaign["ok"] != true ||
		campaign["status"] != "CONFIRMED" || campaign["credits"] != float64(10) {
		t.Fatalf("checkin campaign = %#v", campaign)
	}
	sliceNoSecrets(t, "checkin", rec.Body, secrets...)

	// 7. Credits refresh force-refreshes the account statistics and returns
	// the normalized public view.
	rec = sliceHTTP(t, client, http.MethodPost, live.server.URL+"/codearts/credits/refresh",
		map[string]any{}, panelHeaders)
	sliceRequireStatus(t, "credits refresh", rec, http.StatusOK)
	credits := sliceJSON(t, "credits refresh", rec)
	if credits["ok"] != true || credits["refreshed"] != float64(1) {
		t.Fatalf("credits refresh = %#v", credits)
	}
	creditAccounts, ok := credits["accounts"].([]any)
	if !ok || len(creditAccounts) != 1 {
		t.Fatalf("credits accounts = %#v", credits["accounts"])
	}
	creditRow, _ := creditAccounts[0].(map[string]any)
	creditView, _ := creditRow["credits"].(map[string]any)
	if creditRow["uid"] != uid || creditRow["ok"] != true ||
		creditView["remain"] != float64(900) || creditView["unit"] != "credit" {
		t.Fatalf("credits row = %#v", creditRow)
	}
	sliceNoSecrets(t, "credits refresh", rec.Body, secrets...)

	// 8. Generic health includes the registered codearts realm.
	rec = sliceHTTP(t, client, http.MethodGet, live.server.URL+"/health", nil, nil)
	sliceRequireStatus(t, "health", rec, http.StatusOK)
	healthRoot := sliceJSON(t, "health", rec)
	if healthRoot["ok"] != true || healthRoot["version"] != codeartsSliceVersion {
		t.Fatalf("health ok/version = %v/%v", healthRoot["ok"], healthRoot["version"])
	}
	providerHealths, ok := healthRoot["providers"].(map[string]any)
	if !ok {
		t.Fatalf("health providers = %#v", healthRoot["providers"])
	}
	codeartsHealth, ok := providerHealths["codearts"].(map[string]any)
	if !ok || codeartsHealth["ok"] != true || codeartsHealth["accounts"] != float64(1) {
		t.Fatalf("health providers.codearts = %#v", providerHealths["codearts"])
	}
	sliceNoSecrets(t, "health", rec.Body, secrets...)

	// 9. The api server injected the actual bound listener port into CodeArts,
	// not the request Host. OAuth start needs no account and no upstream call.
	rec = codeartsSliceHTTPWithHost(t, client, http.MethodPost, live.server.URL+"/codearts/accounts/oauth/start",
		map[string]any{}, map[string]string{"X-Panel-Token": slicePanelPassword}, "attacker.example:9999")
	sliceRequireStatus(t, "oauth start", rec, http.StatusOK)
	start := sliceJSON(t, "oauth start", rec)
	authURL, _ := start["auth_url"].(string)
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth_url: %v", err)
	}
	if got := parsed.Query().Get("port"); got != fmt.Sprint(live.port) {
		t.Fatalf("oauth start port = %q, want actual listener port %d", got, live.port)
	}
	if strings.Contains(authURL, "attacker.example") {
		t.Fatal("oauth start used the request Host instead of the injected listener port")
	}
	sliceNoSecrets(t, "oauth start", rec.Body, secrets...)

	// The fake covers every source this slice depends on.
	fake.mu.Lock()
	calls := make(map[string]int, len(fake.calls))
	for path, count := range fake.calls {
		calls[path] = count
	}
	chatBodies := append([]map[string]any(nil), fake.chatBodies...)
	chatTokens := append([]string(nil), fake.chatTokens...)
	fake.mu.Unlock()
	for _, path := range []string{
		codeartsFakeAgentListPath,
		codeartsFakeAgentDetailPath,
		codeartsFakeBuiltinPath,
		codeartsFakeBenefitConfigPath,
		codeartsFakeBenefitClaimPath,
		codeartsFakeStatisticsPath,
		codeartsFakeDeliveryPath,
		codeartsFakeOpsClaimPath,
		codeartsFakeOpsConfirmPath,
		codeartsFakeChatPath,
	} {
		if calls[path] < 1 {
			t.Fatalf("fake upstream received no %s request", path)
		}
	}
	if calls[codeartsFakeChatPath] != 3 {
		t.Fatalf("fake chat calls = %d, want 3", calls[codeartsFakeChatPath])
	}
	for _, body := range chatBodies {
		if body["stream"] != true {
			t.Fatalf("upstream body stream = %v, want true", body["stream"])
		}
		switch body["model"] {
		case "deepseek-v4.1-flash-1017":
		case "codearts-slice-error-model":
		default:
			t.Fatalf("upstream body model = %v, want the canonical lowercase chat id", body["model"])
		}
		if _, leaked := body["realm"]; leaked {
			t.Fatal("gateway realm leaked upstream")
		}
	}
	for _, token := range chatTokens {
		if token != accessToken {
			t.Fatal("fake upstream did not receive the imported account's cloud token")
		}
	}
}

// TestCodeArtsCompositionAndDisabledOverview covers composition registration,
// the disabled realm envelope and listener-port injection without fake upstream
// traffic.
func TestCodeArtsCompositionAndDisabledOverview(t *testing.T) {
	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	codeartsSliceSettings(t, accountsDir, false)

	setup, err := app.Build(app.Paths{
		AccountsDir: accountsDir,
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	codeartsSliceRequireRegistration(t, setup)

	live := newCodeartsSliceLiveServer(t, setup, slicePanelPassword, sliceAPIKey)
	client := &http.Client{Timeout: 10 * time.Second}
	panelHeaders := map[string]string{"X-Panel-Token": slicePanelPassword}

	rec := sliceHTTP(t, client, http.MethodGet, live.server.URL+"/codearts/overview", nil, panelHeaders)
	sliceRequireStatus(t, "disabled overview", rec, http.StatusServiceUnavailable)
	failure := codeartsSliceError(t, "disabled overview", rec)
	if failure["message"] != "CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。" ||
		failure["type"] != "invalid_request_error" || failure["code"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("disabled overview error = %#v", failure)
	}

	rec = sliceHTTP(t, client, http.MethodGet, live.server.URL+"/codearts/accounts", nil, panelHeaders)
	sliceRequireStatus(t, "disabled accounts", rec, http.StatusServiceUnavailable)

	rec = sliceHTTP(t, client, http.MethodGet, live.server.URL+"/health", nil, nil)
	sliceRequireStatus(t, "disabled health", rec, http.StatusOK)
	healthRoot := sliceJSON(t, "disabled health", rec)
	providerHealths, _ := healthRoot["providers"].(map[string]any)
	codeartsHealth, ok := providerHealths["codearts"].(map[string]any)
	if !ok || codeartsHealth["ok"] != false || codeartsHealth["configured"] != false {
		t.Fatalf("disabled codearts health = %#v", providerHealths["codearts"])
	}

	rec = codeartsSliceHTTPWithHost(t, client, http.MethodPost, live.server.URL+"/codearts/accounts/oauth/start",
		map[string]any{}, map[string]string{"X-Panel-Token": slicePanelPassword}, "attacker.example:9999")
	sliceRequireStatus(t, "disabled oauth start", rec, http.StatusOK)
	start := sliceJSON(t, "disabled oauth start", rec)
	authURL, _ := start["auth_url"].(string)
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth_url: %v", err)
	}
	if got := parsed.Query().Get("port"); got != fmt.Sprint(live.port) {
		t.Fatalf("oauth start port = %q, want actual listener port %d", got, live.port)
	}
	if strings.Contains(authURL, "attacker.example") {
		t.Fatal("oauth start used the request Host instead of the injected listener port")
	}
}

func codeartsSliceRequireRegistration(t *testing.T, setup *app.Setup) {
	t.Helper()
	if len(setup.Providers) != 5 {
		t.Fatalf("providers = %d, want raccoon, codearts, doubao, and workbuddy intl/cn", len(setup.Providers))
	}
	byID := make(map[string]int, len(setup.Providers))
	for _, provider := range setup.Providers {
		byID[provider.ID()]++
	}
	for _, id := range []string{"raccoon", "codearts", "doubao", "workbuddy-intl", "workbuddy-cn"} {
		if byID[id] != 1 {
			t.Fatalf("provider ID %q count = %d, want exactly 1", id, byID[id])
		}
	}
	for _, realm := range []string{"raccoon", "codearts", "doubao", "intl", "cn"} {
		provider, ok := setup.Registry.Get(realm)
		if !ok || provider == nil {
			t.Fatalf("registry has no %q realm", realm)
		}
		if provider.Realm() != realm {
			t.Fatalf("registry[%q].Realm() = %q", realm, provider.Realm())
		}
	}
}

func codeartsSliceError(t *testing.T, where string, rec *sliceResponse) map[string]any {
	t.Helper()
	body := sliceJSON(t, where, rec)
	failure, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s error envelope = %#v", where, body)
	}
	return failure
}

func sortedCodeartsModelIDs(ids map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// codeartsSliceHTTPWithHost is sliceHTTP with an explicit Request.Host. Go
// ignores a "Host" entry in http.Header, so the listener-port test cannot use
// the shared helper's header map for a spoofed host.
func codeartsSliceHTTPWithHost(
	t *testing.T,
	client *http.Client,
	method, rawURL string,
	payload any,
	headers map[string]string,
	host string,
) *sliceResponse {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s request: %v", method, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if host != "" {
		request.Host = host
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

const (
	codeartsFakeAgentListPath     = "/v1/agent-center/agents/useragents"
	codeartsFakeAgentDetailPath   = "/v1/agent-center/agents/detail"
	codeartsFakeBuiltinPath       = "/v1/model/builtin"
	codeartsFakeBenefitConfigPath = "/api/v1/gateway/config"
	codeartsFakeBenefitClaimPath  = "/api/v1/benefit/claim"
	codeartsFakeStatisticsPath    = "/snap-manager/v1/statistics/plugin"
	codeartsFakeDeliveryPath      = "/v1/ops/delivery"
	codeartsFakeOpsClaimPath      = "/v1/ops/claim"
	codeartsFakeOpsConfirmPath    = "/v1/ops/confirm"
	codeartsFakeChatPath          = "/api/v2/chat/completions"
)

// codeartsFakeUpstream is a deterministic local stand-in for every Huawei
// endpoint this slice touches: agent/builtin/benefit discovery, statistics,
// check-in delivery/claim/confirm, and the chat SSE endpoint.
type codeartsFakeUpstream struct {
	mu sync.Mutex

	calls      map[string]int
	chatBodies []map[string]any
	chatTokens []string
}

func newCodeartsFakeUpstream() *codeartsFakeUpstream {
	return &codeartsFakeUpstream{calls: make(map[string]int)}
}

func (u *codeartsFakeUpstream) record(path string) {
	u.mu.Lock()
	u.calls[path]++
	u.mu.Unlock()
}

func (u *codeartsFakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && path == codeartsFakeAgentListPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{
			"agents": []any{map[string]any{
				"agent_id":         "agent-code",
				"agent_name":       "CodeAgent",
				"show_in_ide":      true,
				"alias":            map[string]any{"alias_zh_cn": "智能体"},
				"is_primary_agent": true,
			}},
		})

	case r.Method == http.MethodGet && path == codeartsFakeAgentDetailPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{
			"gpts": map[string]any{"models": []any{map[string]any{
				"model_name": "Agent-Model",
				"model_parameters": map[string]any{
					"context_window": 131072,
					"max_tokens":     8192,
					"model_desc":     "agent model",
					"model_category": "文本",
				},
			}}},
		})

	case r.Method == http.MethodGet && path == codeartsFakeBuiltinPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{
			"builtinModels": []any{
				map[string]any{
					"model_id":       "GLM-5.2",
					"model_name":     "GLM-5.2",
					"context_window": 131072,
					"max_tokens":     8192,
					"model_desc":     "latest flagship",
					"model_category": "文本",
					"credit": []any{map[string]any{
						"ratio_display": "0.7x", "time_slot_id": "default",
					}},
				},
				map[string]any{
					"model_id":       "deepseek-v4.1-flash-1017",
					"model_name":     "deepseek-v4.1-flash-1017",
					"context_window": 1000000,
					"max_tokens":     128000,
				},
			},
		})

	case r.Method == http.MethodGet && path == codeartsFakeBenefitConfigPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{
			"error_code": "0000",
			"result": map[string]any{"models": []any{map[string]any{
				"model_id":       "deepseek-v4-flash-0731",
				"model_name":     "deepseek-v4-flash-0731",
				"context_window": 65536,
				"max_tokens":     4096,
			}}},
		})

	case r.Method == http.MethodPost && path == codeartsFakeBenefitClaimPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{"error_code": "0000"})

	case r.Method == http.MethodGet && path == codeartsFakeStatisticsPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{
			"metrics": []any{map[string]any{
				"name":                           "usageTotalPackageCredit",
				"package_credit_amount":          1000,
				"package_credit_used":            100,
				"package_credit_remain":          900,
				"package_credit_expiring_amount": 5,
			}},
			"model_metrics": []any{map[string]any{
				"model_id": "GLM-5.2", "model_name": "GLM-5.2",
				"model_desc": "latest flagship", "model_type": "system",
				"enable": true, "status": "normal", "is_luxury": false,
			}},
			"package": map[string]any{
				"spec_code":         "codearts.agent.individual.trial",
				"status":            "normal",
				"package_name_cn":   "体验版",
				"package_name_en":   "Trial",
				"is_credit_package": true,
			},
			"start_date": "2026-10-31",
			"end_date":   "2026-12-01",
		})

	case r.Method == http.MethodGet && path == codeartsFakeDeliveryPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"items": []any{map[string]any{
				"campaignId":         1,
				"title":              "每日签到",
				"type":               "USER_LOGIN",
				"status":             "ELIGIBLE",
				"claimable":          true,
				"benefitUnit":        "CREDIT",
				"pendingTotalAmount": 10,
			}}},
		})

	case r.Method == http.MethodPost && path == codeartsFakeOpsClaimPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{"code": 0, "data": map[string]any{}})

	case r.Method == http.MethodPost && path == codeartsFakeOpsConfirmPath:
		u.record(path)
		writeCodeartsFakeJSON(w, map[string]any{"code": 0, "data": map[string]any{}})

	case r.Method == http.MethodPost && path == codeartsFakeChatPath:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.calls[path]++
		u.chatBodies = append(u.chatBodies, body)
		u.chatTokens = append(u.chatTokens, r.Header.Get("x-auth-token"))
		u.mu.Unlock()

		model, _ := body["model"].(string)
		if model == "codearts-slice-error-model" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_msg":"request param is invalid"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for _, frame := range codeartsFakeChatFrames(model) {
			if _, err := w.Write(frame); err != nil {
				return
			}
		}

	default:
		http.NotFound(w, r)
	}
}

func writeCodeartsFakeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// codeartsFakeChatFrames mirrors the semantic CodeArts stream: a role frame,
// two text deltas, a finish frame, usage, and [DONE].
func codeartsFakeChatFrames(model string) [][]byte {
	chunks := []map[string]any{
		{
			"id": "chatcmpl-codearts-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-codearts-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "你"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-codearts-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "好"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-codearts-slice", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
			}},
		},
		{
			"id": "chatcmpl-codearts-slice", "object": "chat.completion.chunk", "created": 1,
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
