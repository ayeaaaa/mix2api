package integration

import (
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
	"github.com/ayeaaaa/mix2api-v2/internal/providers/workbuddy"
)

const (
	workbuddySliceVersion = "2.0.0-workbuddy-integration"
	workbuddySliceModel   = "hy4-preview"
)

// workbuddySliceUpstream is a deterministic local stand-in for the WorkBuddy
// intl/cn chat, refresh, catalog, billing, task, and activity endpoints used by
// the slice. It never contacts the real network.
type workbuddySliceUpstream struct {
	mu sync.Mutex

	intlChatCalls int
	cnChatCalls   int
	refreshCalls  int

	failAllIntlChat   bool
	failFirstIntlChat bool
	unauthorizedToken string
	refreshedAccess   string
	refreshedRefresh  string
}

func (f *workbuddySliceUpstream) setUnauthorizedToken(token string) {
	f.mu.Lock()
	f.unauthorizedToken = token
	f.mu.Unlock()
}

func (f *workbuddySliceUpstream) counts() (intlChat, cnChat, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.intlChatCalls, f.cnChatCalls, f.refreshCalls
}

func (f *workbuddySliceUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	switch r.URL.Path {
	case "/v2/plugin/auth/token/refresh":
		f.mu.Lock()
		f.refreshCalls++
		access := f.refreshedAccess
		refresh := f.refreshedRefresh
		f.mu.Unlock()
		if access == "" {
			access = "intl-refreshed-access-slice-0123456789"
		}
		if refresh == "" {
			refresh = "intl-refreshed-refresh-slice-0123456789"
		}
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"accessToken":  access,
				"refreshToken": refresh,
				"expiresAt":    4102444800,
			},
		})
	case "/v3/config":
		models := []any{"hy4-preview", "hy3", "deepseek-v4.1-flash", "glm-5.3"}
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"agents": map[string]any{
					"cli": map[string]any{"models": models},
				},
			},
		})
	case "/v2/chat/completions":
		f.handleChat(w, r, token)
	case "/billing/meter/get-user-resource-summary":
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
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
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
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
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"msg":  "签到成功",
			"data": map[string]any{},
		})
	case "/v2/report":
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "ok"})
	case "/v2/activity/growth/tasks":
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
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
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"balance": 10},
		})
	case "/activity/growth/streak", "/v2/activity/growth/streak":
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"streak":            map[string]any{"days": 2},
				"makeup_cards":      map[string]any{"balance": 0},
				"redemption_status": map[string]any{"tier_7d_status": "locked", "tier_14d_status": "locked", "tier_28d_status": "locked"},
			},
		})
	case "/activity/growth/lottery/summary":
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"chances": 1},
		})
	case "/activity/growth/buddy/travel/status":
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"state": "idle"},
		})
	default:
		writeWorkBuddySliceJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}})
	}
}

func (f *workbuddySliceUpstream) handleChat(w http.ResponseWriter, r *http.Request, token string) {
	intl := strings.HasPrefix(token, "intl-")
	f.mu.Lock()
	if intl {
		f.intlChatCalls++
	}
	if strings.HasPrefix(token, "cn-") {
		f.cnChatCalls++
	}
	intlCall := f.intlChatCalls
	failAll := f.failAllIntlChat
	failFirst := f.failFirstIntlChat
	unauthorized := f.unauthorizedToken
	f.mu.Unlock()

	if intl && (failAll || (failFirst && intlCall == 1)) {
		writeWorkBuddySliceJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"message": "usage exceeds frequency limit"},
		})
		return
	}
	if unauthorized != "" && token == unauthorized {
		writeWorkBuddySliceJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "token expired"},
		})
		return
	}

	content := "cn-ok"
	if intl {
		content = "intl-ok"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	frames := []string{
		fmt.Sprintf("data: {\"id\":\"chatcmpl-slice\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", workbuddySliceModel, content),
		"data: {\"id\":\"chatcmpl-slice\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"" + workbuddySliceModel + "\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n",
		"data: [DONE]\n\n",
	}
	for _, frame := range frames {
		if _, err := io.WriteString(w, frame); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func writeWorkBuddySliceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type workbuddySliceFixture struct {
	setup    *app.Setup
	live     *httptest.Server
	fake     *workbuddySliceUpstream
	upstream *httptest.Server
	client   *http.Client
	accounts []string
	secrets  []string
}

type workbuddySliceAccount struct {
	UID   string
	Realm string
	Token string
}

type workbuddySliceOptions struct {
	FallbackEnabled bool
	APIKeyRealm     string
}

func newWorkBuddySliceFixture(
	t *testing.T,
	accounts []workbuddySliceAccount,
	options workbuddySliceOptions,
) *workbuddySliceFixture {
	t.Helper()
	fake := &workbuddySliceUpstream{
		refreshedAccess:  "intl-refreshed-access-slice-0123456789",
		refreshedRefresh: "intl-refreshed-refresh-slice-0123456789",
	}
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	t.Setenv("WB_INTL_CHAT_UPSTREAM", upstream.URL)
	t.Setenv("WB_CN_CHAT_UPSTREAM", upstream.URL)
	t.Setenv("WB_BILLING_UPSTREAM", upstream.URL)
	t.Setenv("WB_TASK_WEB_UPSTREAM", upstream.URL)
	t.Setenv("WB_RACCOON_BASE_URL", upstream.URL)
	t.Setenv("WB_CODEARTS_SNAP_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_SNAP_MANAGER", upstream.URL+"/snap-manager")
	t.Setenv("WB_CODEARTS_BENEFIT_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", upstream.URL)

	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("create accounts dir: %v", err)
	}
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	intlSettings := map[string]any{
		"enabled":                true,
		"timeout_seconds":        5,
		"stream_timeout_seconds": 30,
		"realm_fallback_enabled": options.FallbackEnabled,
	}
	if _, err := store.Merge(map[string]any{
		"workbuddy": map[string]any{
			"intl": intlSettings,
			"cn":   map[string]any{"enabled": true},
		},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	secrets := make([]string, 0, len(accounts)*3)
	uids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.Token == "" {
			account.Token = workbuddySliceOpaqueToken(account.UID)
		}
		saveWorkBuddySliceAccount(t, accountsDir, account)
		secrets = append(secrets, account.Token, "refresh-"+account.UID)
		uids = append(uids, account.UID)
	}

	setup, err := app.Build(app.Paths{
		AccountsDir: accountsDir,
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	for realm, wantID := range map[string]string{"intl": "workbuddy-intl", "cn": "workbuddy-cn"} {
		provider, ok := setup.Registry.Get(realm)
		if !ok || provider == nil || provider.ID() != wantID {
			t.Fatalf("registry[%q] = %v, want %s", realm, provider, wantID)
		}
	}

	optionsForServer := api.ServerOptions{
		Version:    workbuddySliceVersion,
		Providers:  setup.Providers,
		PanelAuth:  api.RequirePanelAuth(slicePanelPassword),
		APIKeyAuth: api.RequireAPIKeyAuth(sliceAPIKey),
	}
	if options.APIKeyRealm != "" {
		realm := options.APIKeyRealm
		optionsForServer.APIKeyRealm = func(*http.Request) string { return realm }
	}
	live := httptest.NewServer(api.NewServer(optionsForServer).Handler)
	t.Cleanup(live.Close)

	return &workbuddySliceFixture{
		setup:    setup,
		live:     live,
		fake:     fake,
		upstream: upstream,
		client:   &http.Client{Timeout: 20 * time.Second},
		accounts: uids,
		secrets:  secrets,
	}
}

func (f *workbuddySliceFixture) do(
	t *testing.T,
	method, path string,
	payload any,
	headers map[string]string,
) *sliceResponse {
	t.Helper()
	return sliceHTTP(t, f.client, method, f.live.URL+path, payload, headers)
}

func (f *workbuddySliceFixture) assertNoSecrets(t *testing.T, where string, response *sliceResponse) {
	t.Helper()
	sliceNoSecrets(t, where, response.Body, f.secrets...)
	for _, extra := range []string{
		"intl-refreshed-access-slice-0123456789",
		"intl-refreshed-refresh-slice-0123456789",
	} {
		sliceNoSecrets(t, where, response.Body, extra)
	}
}

func saveWorkBuddySliceAccount(t *testing.T, dir string, account workbuddySliceAccount) {
	t.Helper()
	domain := "www.workbuddy.ai"
	if account.Realm == "cn" {
		domain = "www.workbuddy.cn"
	}
	token := account.Token
	if token == "" {
		token = workbuddySliceOpaqueToken(account.UID)
	}
	auth := workbuddy.Auth{
		UID:             account.UID,
		Nickname:        "Slice " + account.UID,
		Realm:           account.Realm,
		Domain:          domain,
		AccessToken:     token,
		RefreshToken:    "refresh-" + account.UID,
		ExpiresAt:       time.Now().Add(24 * time.Hour),
		Platform:        "CLI",
		Product:         "workbuddy",
		Source:          "oauth",
		Enabled:         true,
		EnabledSet:      true,
		TasksEnabled:    true,
		TasksEnabledSet: true,
	}
	// Keep the credential opaque: the auth file's explicit realm is
	// authoritative, so no JWT issuer/expiry claim is needed here.
	if _, err := workbuddy.SaveAccount(dir, auth); err != nil {
		t.Fatalf("save workbuddy account %s: %v", account.UID, err)
	}
}

func workbuddySliceOpaqueToken(uid string) string {
	return "wb-" + uid + "-access-slice-0123456789"
}

func workbuddyModelIDs(t *testing.T, root map[string]any) []string {
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

func workbuddyUnionIDs(intl, cn []string) []string {
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

func TestWorkBuddySliceOverHTTP(t *testing.T) {
	accounts := []workbuddySliceAccount{
		{UID: "intl-main", Realm: "intl", Token: "intl-main-access-slice-0123456789"},
		{UID: "cn-main", Realm: "cn", Token: "cn-main-access-slice-0123456789"},
	}
	fixture := newWorkBuddySliceFixture(t, accounts, workbuddySliceOptions{})
	panelHeaders := map[string]string{"X-Panel-Token": slicePanelPassword}
	apiHeaders := map[string]string{"Authorization": "Bearer " + sliceAPIKey}

	// Health is public and reports every registered realm. WorkBuddy health is
	// additive under the neutral provider map.
	rec := fixture.do(t, http.MethodGet, "/health?force=1", nil, nil)
	sliceRequireStatus(t, "health", rec, http.StatusOK)
	fixture.assertNoSecrets(t, "health", rec)
	health := sliceJSON(t, "health", rec)
	if health["ok"] != true || health["version"] != workbuddySliceVersion {
		t.Fatalf("health envelope = %#v", health)
	}
	providerHealth, ok := health["providers"].(map[string]any)
	if !ok {
		t.Fatalf("health providers = %#v", health["providers"])
	}
	for _, realm := range []string{"intl", "cn"} {
		entry, ok := providerHealth[realm].(map[string]any)
		if !ok || entry["ok"] != true || entry["realm"] != realm || entry["provider"] != "workbuddy" {
			t.Fatalf("health providers[%q] = %#v", realm, providerHealth[realm])
		}
		if workbuddyHealthNumber(entry["accounts"]) != "1" {
			t.Fatalf("health providers[%q].accounts = %#v", realm, entry["accounts"])
		}
	}

	// Models: per-realm catalogs and realm=all is their exact deduplicated
	// union. Non-WorkBuddy providers never leak into the union.
	intlRec := fixture.do(t, http.MethodGet, "/v1/models?realm=intl", nil, apiHeaders)
	sliceRequireStatus(t, "models intl", intlRec, http.StatusOK)
	fixture.assertNoSecrets(t, "models intl", intlRec)
	intlModels := sliceJSON(t, "models intl", intlRec)
	intlIDs := workbuddyModelIDs(t, intlModels)
	if intlModels["realm"] != "intl" || len(intlIDs) == 0 {
		t.Fatalf("intl models = %#v", intlModels)
	}

	cnRec := fixture.do(t, http.MethodGet, "/v1/models?realm=cn", nil, apiHeaders)
	sliceRequireStatus(t, "models cn", cnRec, http.StatusOK)
	fixture.assertNoSecrets(t, "models cn", cnRec)
	cnModels := sliceJSON(t, "models cn", cnRec)
	cnIDs := workbuddyModelIDs(t, cnModels)
	if cnModels["realm"] != "cn" || len(cnIDs) == 0 {
		t.Fatalf("cn models = %#v", cnModels)
	}

	allRec := fixture.do(t, http.MethodGet, "/v1/models?realm=all", nil, apiHeaders)
	sliceRequireStatus(t, "models all", allRec, http.StatusOK)
	fixture.assertNoSecrets(t, "models all", allRec)
	allModels := sliceJSON(t, "models all", allRec)
	allIDs := workbuddyModelIDs(t, allModels)
	if allModels["realm"] != "all" {
		t.Fatalf("models all realm = %#v", allModels["realm"])
	}
	wantUnion := workbuddyUnionIDs(intlIDs, cnIDs)
	if strings.Join(allIDs, "\x00") != strings.Join(wantUnion, "\x00") {
		t.Fatalf("models all = %v, want exact union %v", allIDs, wantUnion)
	}
	if !containsWorkBuddyID(intlIDs, workbuddySliceModel) {
		t.Fatalf("intl catalog does not advertise %q: %v", workbuddySliceModel, intlIDs)
	}

	// Chat: explicit intl streaming and cn non-streaming both traverse the real
	// generic API and the real provider.
	streamRec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
		"model":    workbuddySliceModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello intl"}},
		"stream":   true,
	}, apiHeaders)
	sliceRequireStatus(t, "chat intl stream", streamRec, http.StatusOK)
	fixture.assertNoSecrets(t, "chat intl stream", streamRec)
	if !strings.Contains(string(streamRec.Body), "intl-ok") ||
		!strings.Contains(string(streamRec.Body), "data: [DONE]") {
		t.Fatalf("intl stream body = %q", streamRec.Body)
	}

	cnChatRec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=cn", map[string]any{
		"model":    workbuddySliceModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello cn"}},
		"stream":   false,
	}, apiHeaders)
	sliceRequireStatus(t, "chat cn nonstream", cnChatRec, http.StatusOK)
	fixture.assertNoSecrets(t, "chat cn nonstream", cnChatRec)
	cnChat := sliceJSON(t, "chat cn nonstream", cnChatRec)
	if text := workbuddyCompletionText(cnChat); text != "cn-ok" {
		t.Fatalf("cn completion text = %q, payload = %#v", text, cnChat)
	}

	// The intl provider owns the legacy admin paths and delegates ?realm=cn to
	// the twin pool.
	accountsRec := fixture.do(t, http.MethodGet, "/accounts?realm=cn", nil, panelHeaders)
	sliceRequireStatus(t, "accounts cn", accountsRec, http.StatusOK)
	fixture.assertNoSecrets(t, "accounts cn", accountsRec)
	accountsBody := sliceJSON(t, "accounts cn", accountsRec)
	accountRows, ok := accountsBody["accounts"].([]any)
	if !ok || len(accountRows) != 1 {
		t.Fatalf("accounts cn rows = %#v", accountsBody["accounts"])
	}
	cnRow, _ := accountRows[0].(map[string]any)
	if cnRow["uid"] != "cn-main" || cnRow["realm"] != "cn" {
		t.Fatalf("accounts cn row = %#v", cnRow)
	}

	// Credits and check-in use the CN billing/check-in endpoints; tasks use the
	// CN growth-centre endpoints.
	creditsRec := fixture.do(t, http.MethodPost, "/accounts/credits?realm=cn", map[string]any{}, panelHeaders)
	sliceRequireStatus(t, "credits cn", creditsRec, http.StatusOK)
	fixture.assertNoSecrets(t, "credits cn", creditsRec)
	creditsBody := sliceJSON(t, "credits cn", creditsRec)
	results, ok := creditsBody["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("credits results = %#v", creditsBody["results"])
	}
	creditRow, _ := results[0].(map[string]any)
	if creditRow["uid"] != "cn-main" || creditRow["ok"] != true {
		t.Fatalf("credits row = %#v", creditRow)
	}

	checkinRec := fixture.do(t, http.MethodPost, "/accounts/checkin?realm=cn", map[string]any{"uid": "cn-main"}, panelHeaders)
	sliceRequireStatus(t, "checkin cn", checkinRec, http.StatusOK)
	fixture.assertNoSecrets(t, "checkin cn", checkinRec)
	checkinBody := sliceJSON(t, "checkin cn", checkinRec)
	if checkinResults, ok := checkinBody["results"].([]any); !ok || len(checkinResults) != 1 {
		t.Fatalf("checkin results = %#v", checkinBody["results"])
	} else if row, _ := checkinResults[0].(map[string]any); row["ok"] != true {
		t.Fatalf("checkin row = %#v", row)
	}

	tasksRec := fixture.do(t, http.MethodGet, "/tasks?realm=cn", nil, panelHeaders)
	sliceRequireStatus(t, "tasks cn", tasksRec, http.StatusOK)
	fixture.assertNoSecrets(t, "tasks cn", tasksRec)
	tasksBody := sliceJSON(t, "tasks cn", tasksRec)
	if tasks, ok := tasksBody["tasks"].([]any); !ok || len(tasks) != 1 {
		t.Fatalf("tasks = %#v", tasksBody["tasks"])
	}
}

func TestWorkBuddySliceFailoverAndFallback(t *testing.T) {
	apiHeaders := map[string]string{"Authorization": "Bearer " + sliceAPIKey}

	t.Run("429 account failover", func(t *testing.T) {
		fixture := newWorkBuddySliceFixture(t, []workbuddySliceAccount{
			{UID: "intl-429-a", Realm: "intl", Token: "intl-429-a-access-slice-0123456789"},
			{UID: "intl-429-b", Realm: "intl", Token: "intl-429-b-access-slice-0123456789"},
		}, workbuddySliceOptions{})
		fixture.fake.failFirstIntlChat = true
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddySliceModel,
			"messages": []any{map[string]any{"role": "user", "content": "failover"}},
			"stream":   false,
		}, apiHeaders)
		sliceRequireStatus(t, "429 failover", rec, http.StatusOK)
		fixture.assertNoSecrets(t, "429 failover", rec)
		if text := workbuddyCompletionText(sliceJSON(t, "429 failover", rec)); text != "intl-ok" {
			t.Fatalf("failover completion = %q", text)
		}
		if intlChat, _, _ := fixture.fake.counts(); intlChat < 2 {
			t.Fatalf("intl chat attempts = %d, want at least 2", intlChat)
		}
	})

	t.Run("401 refresh then retry", func(t *testing.T) {
		const oldToken = "intl-401-access-slice-0123456789"
		fixture := newWorkBuddySliceFixture(t, []workbuddySliceAccount{
			{UID: "intl-401", Realm: "intl", Token: oldToken},
		}, workbuddySliceOptions{})
		fixture.fake.setUnauthorizedToken(oldToken)
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddySliceModel,
			"messages": []any{map[string]any{"role": "user", "content": "refresh"}},
			"stream":   false,
		}, apiHeaders)
		if rec.Status != http.StatusOK {
			t.Fatalf("401 refresh: status=%d body=%s", rec.Status, rec.Body)
		}
		fixture.assertNoSecrets(t, "401 refresh", rec)
		if text := workbuddyCompletionText(sliceJSON(t, "401 refresh", rec)); text != "intl-ok" {
			t.Fatalf("refresh completion = %q", text)
		}
		_, _, refreshes := fixture.fake.counts()
		if refreshes != 1 {
			t.Fatalf("refresh calls = %d, want 1", refreshes)
		}
	})

	fallbackAccounts := []workbuddySliceAccount{
		{UID: "intl-fallback", Realm: "intl", Token: "intl-fallback-access-slice-0123456789"},
		{UID: "cn-fallback", Realm: "cn", Token: "cn-fallback-access-slice-0123456789"},
	}

	t.Run("cross fallback default off", func(t *testing.T) {
		fixture := newWorkBuddySliceFixture(t, fallbackAccounts, workbuddySliceOptions{})
		fixture.fake.failAllIntlChat = true
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    workbuddySliceModel,
			"messages": []any{map[string]any{"role": "user", "content": "no fallback"}},
			"stream":   false,
		}, apiHeaders)
		sliceRequireStatus(t, "fallback off", rec, http.StatusTooManyRequests)
		fixture.assertNoSecrets(t, "fallback off", rec)
		if _, cnChat, _ := fixture.fake.counts(); cnChat != 0 {
			t.Fatalf("cn fallback calls = %d, want 0", cnChat)
		}
	})

	t.Run("cross fallback enabled", func(t *testing.T) {
		fixture := newWorkBuddySliceFixture(t, fallbackAccounts, workbuddySliceOptions{FallbackEnabled: true})
		fixture.fake.failAllIntlChat = true
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    workbuddySliceModel,
			"messages": []any{map[string]any{"role": "user", "content": "fallback"}},
			"stream":   false,
		}, apiHeaders)
		sliceRequireStatus(t, "fallback on", rec, http.StatusOK)
		fixture.assertNoSecrets(t, "fallback on", rec)
		if text := workbuddyCompletionText(sliceJSON(t, "fallback on", rec)); text != "cn-ok" {
			t.Fatalf("fallback completion = %q", text)
		}
		if _, cnChat, _ := fixture.fake.counts(); cnChat != 1 {
			t.Fatalf("cn fallback calls = %d, want 1", cnChat)
		}
	})

	t.Run("explicit realm suppresses fallback", func(t *testing.T) {
		fixture := newWorkBuddySliceFixture(t, fallbackAccounts, workbuddySliceOptions{FallbackEnabled: true})
		fixture.fake.failAllIntlChat = true
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=intl", map[string]any{
			"model":    workbuddySliceModel,
			"messages": []any{map[string]any{"role": "user", "content": "explicit"}},
			"stream":   false,
		}, apiHeaders)
		sliceRequireStatus(t, "explicit suppress", rec, http.StatusTooManyRequests)
		if _, cnChat, _ := fixture.fake.counts(); cnChat != 0 {
			t.Fatalf("explicit-realm cn fallback calls = %d, want 0", cnChat)
		}
	})

	t.Run("API key realm suppresses fallback", func(t *testing.T) {
		fixture := newWorkBuddySliceFixture(t, fallbackAccounts, workbuddySliceOptions{
			FallbackEnabled: true,
			APIKeyRealm:     "intl",
		})
		fixture.fake.failAllIntlChat = true
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    workbuddySliceModel,
			"messages": []any{map[string]any{"role": "user", "content": "api key"}},
			"stream":   false,
		}, apiHeaders)
		sliceRequireStatus(t, "api key suppress", rec, http.StatusTooManyRequests)
		if _, cnChat, _ := fixture.fake.counts(); cnChat != 0 {
			t.Fatalf("api-key cn fallback calls = %d, want 0", cnChat)
		}
	})

	t.Run("realm all rejects non-WorkBuddy model", func(t *testing.T) {
		fixture := newWorkBuddySliceFixture(t, fallbackAccounts, workbuddySliceOptions{})
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=all", map[string]any{
			"model":    "definitely-not-a-workbuddy-model",
			"messages": []any{map[string]any{"role": "user", "content": "nope"}},
			"stream":   false,
		}, apiHeaders)
		sliceRequireStatus(t, "realm=all reject", rec, http.StatusNotFound)
		fixture.assertNoSecrets(t, "realm=all reject", rec)
	})

	t.Run("realm all resolves a WorkBuddy model", func(t *testing.T) {
		fixture := newWorkBuddySliceFixture(t, fallbackAccounts, workbuddySliceOptions{})
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=all", map[string]any{
			"model":    workbuddySliceModel,
			"messages": []any{map[string]any{"role": "user", "content": "union"}},
			"stream":   false,
		}, apiHeaders)
		sliceRequireStatus(t, "realm=all resolve", rec, http.StatusOK)
		fixture.assertNoSecrets(t, "realm=all resolve", rec)
		if text := workbuddyCompletionText(sliceJSON(t, "realm=all resolve", rec)); text != "intl-ok" {
			t.Fatalf("realm=all completion = %q", text)
		}
	})
}

func workbuddyHealthNumber(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprint(int(typed))
	case int:
		return fmt.Sprint(typed)
	default:
		return ""
	}
}

func containsWorkBuddyID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func workbuddyCompletionText(root map[string]any) string {
	choices, _ := root["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	text, _ := message["content"].(string)
	return text
}
