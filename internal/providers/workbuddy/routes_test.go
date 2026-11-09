package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type wbRoutesFixture struct {
	intl   *Provider
	cn     *Provider
	store  *config.Store
	dir    string
	fake   *taskTestUpstream
	server *httptest.Server
	mux    *http.ServeMux
}

func newWBRoutesFixture(t *testing.T) *wbRoutesFixture {
	t.Helper()
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	intl, err := New(realmIntl, store)
	if err != nil {
		t.Fatalf("New(intl): %v", err)
	}
	cn, err := New(realmCN, store)
	if err != nil {
		t.Fatalf("New(cn): %v", err)
	}
	intl.SetTwin(cn)
	cn.SetTwin(intl)

	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		switch r.URL.Path {
		case creditsCheckinPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"today_checked_in": false,
				"streak_days":      1,
			}}
		case creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"IsPaidUser": false,
				"Packages":   []any{},
			}}
		case "/v2/billing/meter/daily-checkin":
			return http.StatusOK, map[string]any{"code": 0, "msg": "签到成功", "data": map[string]any{}}
		case chatPath:
			return http.StatusOK, taskSSEResponse{
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:   "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	})
	for _, provider := range []*Provider{intl, cn} {
		provider.tasksChatBaseURL = server.URL
		provider.tasksBillingBaseURL = server.URL
		provider.tasksWebBaseURL = server.URL
		provider.creditsBillingBaseURL = server.URL
		provider.creditsCheckinBaseURL = server.URL
		provider.tasksGap = -1
		provider.tasksBackoff = -1
		provider.tasksBudget = 2 * time.Second
		provider.tasksTimeout = 2 * time.Second
		provider.tasksTurnWait = 25 * time.Millisecond
		provider.tasksPollInterval = time.Millisecond
		provider.tasksSleep = func(time.Duration) {}
	}
	fixture := &wbRoutesFixture{
		intl: intl, cn: cn, store: store, dir: dir, fake: fake, server: server,
	}
	fixture.mux = http.NewServeMux()
	api.RegisterProviderRoutes(fixture.mux, intl)
	return fixture
}

func (f *wbRoutesFixture) do(method, path string, payload any) *httptest.ResponseRecorder {
	var reader io.Reader
	switch value := payload.(type) {
	case nil:
	case string:
		reader = strings.NewReader(value)
	default:
		data, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		reader = strings.NewReader(string(data))
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.mux.ServeHTTP(recorder, request)
	return recorder
}

func decodeWBRoutesJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(recorder.Body.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return body
}

func wbRouteObjects(t *testing.T, value any) []map[string]any {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("value is not an array: %#v", value)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("array item is not an object: %#v", item)
		}
		out = append(out, object)
	}
	return out
}

func wbRouteText(value any) string {
	text, _ := value.(string)
	return text
}

func wbRouteToken(t *testing.T, uid, realm string) string {
	t.Helper()
	issuer := "https://www.workbuddy.ai/auth/realms/copilot"
	if realm == realmCN {
		issuer = "https://www.workbuddy.cn/auth/realms/copilot"
	}
	return makeJWT(t, map[string]any{
		"sub": uid,
		"iss": issuer,
		"exp": 4102444800,
	})
}

func wbRouteAddAccount(t *testing.T, provider *Provider, uid, realm string, extra map[string]any) *Account {
	t.Helper()
	auth := Auth{
		UID:             uid,
		Nickname:        "Nick " + uid,
		Realm:           realm,
		AccessToken:     wbRouteToken(t, uid, realm),
		RefreshToken:    "refresh-" + uid,
		Platform:        "CLI",
		Source:          sourceOAuth,
		Enabled:         true,
		EnabledSet:      true,
		TasksEnabled:    true,
		TasksEnabledSet: true,
	}
	if realm == realmCN {
		auth.Domain = "www.workbuddy.cn"
	} else {
		auth.Domain = "www.workbuddy.ai"
	}
	for key, value := range extra {
		switch key {
		case "enabled":
			auth.SetEnabled(value.(bool))
		case "tasksEnabled":
			auth.SetTasksEnabled(value.(bool))
		case "note":
			auth.Note, _ = value.(string)
		case "group":
			auth.Group, _ = value.(string)
		case "weight":
			auth.Weight = value.(int)
		case "proxySlot":
			auth.ProxySlot, _ = value.(string)
		default:
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshal extra %s: %v", key, err)
			}
			if auth.Extra == nil {
				auth.Extra = map[string]json.RawMessage{}
			}
			auth.Extra[key] = raw
		}
	}
	account, err := provider.Pool().Add(auth)
	if err != nil {
		t.Fatalf("add account %s: %v", uid, err)
	}
	return account
}

func assertWBNoSecret(t *testing.T, label, raw string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(raw, secret) {
			t.Fatalf("%s leaked %q: %s", label, secret, raw)
		}
	}
}

func TestRoutesWorkBuddyPanelMetadataAndTwinOwnership(t *testing.T) {
	f := newWBRoutesFixture(t)
	routes := f.intl.Routes()
	if len(routes) == 0 {
		t.Fatal("intl Routes returned no routes")
	}
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if seen[key] {
			t.Fatalf("duplicate route %s", key)
		}
		seen[key] = true
		if route.Handler == nil {
			t.Fatalf("route %s has no handler", key)
		}
		if route.Auth != providers.AuthPanel {
			t.Fatalf("route %s auth = %v, want AuthPanel", key, route.Auth)
		}
	}
	if routes := f.cn.Routes(); len(routes) != 0 {
		t.Fatalf("cn Routes = %d, want none (admin routes live on intl)", len(routes))
	}
	required := []string{
		"GET /accounts",
		"GET /accounts/export",
		"GET /accounts/credits",
		"POST /accounts/credits",
		"GET /accounts/credits/detail",
		"GET /accounts/credits/grants",
		"POST /accounts/import",
		"POST /accounts/import/desktop",
		"POST /accounts/add-api-key",
		"POST /accounts/login/start",
		"GET /accounts/login/poll",
		"POST /accounts/login/cancel",
		"POST /accounts/checkin",
		"POST /accounts/daily-chat",
		"POST /accounts/daily-chat-web",
		"POST /accounts/test",
		"POST /accounts/set",
		"POST /accounts/set-all",
		"POST /accounts/product",
		"POST /accounts/sync-profile",
		"POST /accounts/refresh",
		"POST /accounts/delete",
		"GET /tasks",
		"POST /tasks/run",
		"POST /tasks/claim",
		"POST /tasks/travel",
		"POST /tasks/streak-bonus",
		"GET /activity/history",
		"POST /catalog/refresh",
		"GET /scheduler",
		"GET /settings",
	}
	for _, key := range required {
		if !seen[key] {
			t.Fatalf("missing route %s", key)
		}
	}
	if health, err := f.intl.Health(context.Background(), true); err != nil {
		t.Fatalf("Health: %v", err)
	} else {
		for _, key := range []string{"ok", "configured", "accounts", "healthy"} {
			if _, exists := health[key]; !exists {
				t.Fatalf("health missing %q: %#v", key, health)
			}
		}
	}
}

func TestRoutesWorkBuddyAccountsRealmAndNoTokenLeak(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-intl", realmIntl, map[string]any{"note": "intl note"})
	wbRouteAddAccount(t, f.cn, "uid-cn", realmCN, map[string]any{"note": "cn note"})

	rec := f.do(http.MethodGet, "/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	rows := wbRouteObjects(t, body["accounts"])
	if len(rows) != 1 || wbRouteText(rows[0]["uid"]) != "uid-intl" {
		t.Fatalf("default intl accounts = %#v", rows)
	}
	assertWBNoSecret(t, "accounts", rec.Body.String(), "refresh-uid-intl", "refresh-uid-cn")

	rec = f.do(http.MethodGet, "/accounts?realm=cn", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cn accounts status=%d body=%s", rec.Code, rec.Body)
	}
	body = decodeWBRoutesJSON(t, rec)
	rows = wbRouteObjects(t, body["accounts"])
	if len(rows) != 1 || wbRouteText(rows[0]["uid"]) != "uid-cn" {
		t.Fatalf("cn accounts = %#v", rows)
	}
	assertWBNoSecret(t, "cn accounts", rec.Body.String(), "refresh-uid-intl", "refresh-uid-cn")

	rec = f.do(http.MethodGet, "/accounts?realm=all", nil)
	rows = wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	if len(rows) != 2 {
		t.Fatalf("all accounts = %#v", rows)
	}
}

func TestRoutesWorkBuddyAccountSetAndValidation(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-set", realmIntl, nil)

	rec := f.do(http.MethodPost, "/accounts/set", map[string]any{
		"uid":            "uid-set",
		"note":           "fleet",
		"group":          "team-a",
		"weight":         7,
		"disabledModels": []any{"Model-A", "model-b"},
		"tasksEnabled":   false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	account, ok := body["account"].(map[string]any)
	if !ok {
		t.Fatalf("set response = %#v", body)
	}
	if wbRouteText(account["note"]) != "fleet" || wbRouteText(account["group"]) != "team-a" ||
		account["weight"].(json.Number).String() != "7" || account["tasksEnabled"] != false {
		t.Fatalf("account after set = %#v", account)
	}
	if values, ok := account["disabledModels"].([]any); !ok || len(values) != 2 {
		t.Fatalf("disabledModels = %#v", account["disabledModels"])
	}

	rec = f.do(http.MethodPost, "/accounts/set", map[string]any{"uid": "missing", "note": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing set status=%d body=%s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/set", map[string]any{"uid": "uid-set", "weight": 101})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "weight") {
		t.Fatalf("invalid weight status=%d body=%s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/set", map[string]any{"uid": "uid-set"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty set status=%d body=%s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/set", map[string]any{"uid": "uid-set", "enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body)
	}
	view := decodeWBRoutesJSON(t, rec)["account"].(map[string]any)
	if view["enabled"] != false {
		t.Fatalf("disable view = %#v", view)
	}
}

func TestRoutesWorkBuddyExportShapesAndSecretsToggle(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-intl", realmIntl, nil)
	wbRouteAddAccount(t, f.cn, "uid-cn", realmCN, nil)

	rec := f.do(http.MethodGet, "/accounts/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body)
	}
	doc := decodeWBRoutesJSON(t, rec)
	if wbRouteText(doc["format"]) != "workbuddy-accounts" || doc["version"].(json.Number).String() != "1" {
		t.Fatalf("export header = %#v", doc)
	}
	rows := wbRouteObjects(t, doc["accounts"])
	if len(rows) != 2 {
		t.Fatalf("export rows = %d, want 2", len(rows))
	}
	foundToken := false
	for _, row := range rows {
		if strings.TrimSpace(wbRouteText(row["accessToken"])) != "" {
			foundToken = true
		}
	}
	if !foundToken {
		t.Fatal("export omitted credentials even though secrets default to on")
	}

	rec = f.do(http.MethodGet, "/accounts/export?secrets=0", nil)
	if strings.Contains(rec.Body.String(), "accessToken") || strings.Contains(rec.Body.String(), "refreshToken") {
		t.Fatalf("secrets=0 export still contains credential fields: %s", rec.Body)
	}
	rec = f.do(http.MethodGet, "/accounts/export?uid=uid-cn", nil)
	rows = wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	if len(rows) != 1 || wbRouteText(rows[0]["uid"]) != "uid-cn" {
		t.Fatalf("uid export rows = %#v", rows)
	}
	rec = f.do(http.MethodGet, "/accounts/export?uid=missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing export status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRoutesWorkBuddyImportDryRunOverwriteAndGroup(t *testing.T) {
	f := newWBRoutesFixture(t)
	token := wbRouteToken(t, "uid-import", realmIntl)
	document := map[string]any{
		"format": "workbuddy-accounts",
		"accounts": []any{map[string]any{
			"uid": "uid-import", "accessToken": token, "refreshToken": "refresh-import",
			"realm": "intl", "nickname": "Imported",
		}},
	}
	rec := f.do(http.MethodPost, "/accounts/import", map[string]any{
		"data": document, "dryRun": true, "group": "batch-a",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("dryRun status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	if body["dryRun"] != true {
		t.Fatalf("dryRun body = %#v", body)
	}
	result, _ := body["result"].(map[string]any)
	if values, ok := result["added"].([]any); !ok || len(values) != 1 {
		t.Fatalf("dryRun added = %#v", result["added"])
	}
	if f.intl.Pool().Get("uid-import") != nil {
		t.Fatal("dryRun wrote the account")
	}

	rec = f.do(http.MethodPost, "/accounts/import", map[string]any{
		"data": document, "group": "batch-a",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", rec.Code, rec.Body)
	}
	assertWBNoSecret(t, "import response", rec.Body.String(), "refresh-import", token)
	account := f.intl.Pool().Get("uid-import")
	if account == nil {
		t.Fatal("import did not sync the intl pool")
	}
	account.mu.RLock()
	group := account.Group
	account.mu.RUnlock()
	if group != "batch-a" {
		t.Fatalf("imported group = %q", group)
	}

	rec = f.do(http.MethodPost, "/accounts/import", map[string]any{"data": map[string]any{"nope": true}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad import status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRoutesWorkBuddyLoginStartPollCancelDelegatesRealm(t *testing.T) {
	f := newWBRoutesFixture(t)
	token := wbRouteToken(t, "uid-login", realmCN)
	var polls int
	fake := &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return http.StatusOK, `{"code":0,"data":{"state":"st-login","authUrl":"https://browser.test/authorize?state=st-login"}}`, nil
		case "/v2/plugin/auth/token":
			polls++
			if polls == 1 {
				return http.StatusOK, `{"code":11217,"msg":"waiting for browser login"}`, nil
			}
			return http.StatusOK, `{"code":0,"data":{"accessToken":"` + token + `","refreshToken":"refresh-login","expiresAt":4102444800000,"domain":"www.workbuddy.cn"}}`, nil
		case "/v2/plugin/login/account":
			return http.StatusOK, `{"code":0,"data":{"uid":"uid-login","nickname":"Login User"}}`, nil
		default:
			return http.StatusNotFound, `{}`, nil
		}
	}}
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.oauthBaseURL = "https://oauth.test"
		provider.oauthClient = fake
	}

	rec := f.do(http.MethodPost, "/accounts/login/start", map[string]any{
		"platform": "CLI", "realm": "cn",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body)
	}
	started := decodeWBRoutesJSON(t, rec)
	if wbRouteText(started["state"]) != "st-login" || wbRouteText(started["realm"]) != realmCN {
		t.Fatalf("start response = %#v", started)
	}
	assertWBNoSecret(t, "login start", rec.Body.String(), token, "refresh-login")

	rec = f.do(http.MethodGet, "/accounts/login/poll?state=st-login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body)
	}
	if wbRouteText(decodeWBRoutesJSON(t, rec)["status"]) != "pending" {
		t.Fatalf("first poll = %s", rec.Body)
	}

	rec = f.do(http.MethodGet, "/accounts/login/poll?state=st-login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second poll status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	if wbRouteText(body["status"]) != "ok" {
		t.Fatalf("second poll = %#v", body)
	}
	assertWBNoSecret(t, "login poll", rec.Body.String(), token, "refresh-login")
	if f.cn.Pool().Get("uid-login") == nil {
		t.Fatal("OAuth success did not sync the cn pool")
	}

	rec = f.do(http.MethodPost, "/accounts/login/cancel", map[string]any{"state": "unknown"})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["cancelled"] != false {
		t.Fatalf("cancel unknown = %s", rec.Body)
	}
}

func TestRoutesWorkBuddyCheckinTasksAndActivity(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.cn, "uid-checkin", realmCN, nil)

	rec := f.do(http.MethodPost, "/accounts/checkin", map[string]any{"uid": "uid-checkin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	results := wbRouteObjects(t, body["results"])
	if len(results) != 1 || results[0]["ok"] != true {
		t.Fatalf("checkin results = %#v", results)
	}
	assertWBNoSecret(t, "checkin", rec.Body.String(), "refresh-uid-checkin")

	rec = f.do(http.MethodGet, "/tasks?realm=cn&uid=uid-checkin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks status=%d body=%s", rec.Code, rec.Body)
	}
	tasks := decodeWBRoutesJSON(t, rec)
	if account, _ := tasks["account"].(map[string]any); wbRouteText(account["uid"]) != "uid-checkin" {
		t.Fatalf("tasks account = %#v", tasks["account"])
	}
	assertWBNoSecret(t, "tasks", rec.Body.String(), "refresh-uid-checkin")

	rec = f.do(http.MethodPost, "/tasks/claim", map[string]any{"uid": "uid-checkin"})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["ok"] != false {
		t.Fatalf("claim without code = %s", rec.Body)
	}

	rec = f.do(http.MethodGet, "/activity/history?range=all", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", rec.Code, rec.Body)
	}
	history := decodeWBRoutesJSON(t, rec)
	rows := wbRouteObjects(t, history["rows"])
	if len(rows) == 0 || wbRouteText(rows[0]["task"]) != "checkin" {
		t.Fatalf("activity rows = %#v", rows)
	}
	assertWBNoSecret(t, "activity", rec.Body.String(), "refresh-uid-checkin")

	rec = f.do(http.MethodGet, "/activity/history?task=bogus", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad activity filter status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRoutesWorkBuddyCreditsGrantsCatalogSchedulerSettings(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-intl", realmIntl, nil)

	rec := f.do(http.MethodGet, "/accounts/credits", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("credits status=%d body=%s", rec.Code, rec.Body)
	}
	assertWBNoSecret(t, "credits", rec.Body.String(), "refresh-uid-intl")
	rec = f.do(http.MethodGet, "/accounts/credits/grants", nil)
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["ok"] != true {
		t.Fatalf("grants = %s", rec.Body)
	}

	// Point the catalog fetch at the same fake and return a minimal live list.
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == "/v3/config" {
			return http.StatusOK, catalogPayload([]string{"deepseek-v4.1-flash", "hy3"}, nil, nil)
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	}
	f.intl.catalogBaseURL = f.server.URL
	f.intl.catalogDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
		return f.server.Client().Do(req)
	})
	rec = f.do(http.MethodPost, "/catalog/refresh", map[string]any{"realm": "intl"})
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body)
	}
	catalog := decodeWBRoutesJSON(t, rec)
	if catalog["ok"] != true || catalog["count"].(json.Number).String() == "0" {
		t.Fatalf("catalog response = %#v", catalog)
	}
	assertWBNoSecret(t, "catalog", rec.Body.String(), "refresh-uid-intl")

	rec = f.do(http.MethodGet, "/scheduler", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("scheduler status=%d body=%s", rec.Code, rec.Body)
	}
	status := decodeWBRoutesJSON(t, rec)
	jobs := wbRouteObjects(t, status["jobs"])
	if len(jobs) == 0 {
		t.Fatalf("scheduler jobs = %#v", status)
	}
	assertWBNoSecret(t, "scheduler", rec.Body.String(), "refresh-uid-intl")

	if _, err := f.store.Merge(map[string]any{
		"schedule":       map[string]any{"checkin": []any{10, 22}, "daily_chat": []any{"08:30", "20:30"}},
		"daily_activity": true,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	rec = f.do(http.MethodGet, "/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", rec.Code, rec.Body)
	}
	settings := decodeWBRoutesJSON(t, rec)
	schedule, _ := settings["schedule"].(map[string]any)
	if len(schedule["checkin"].([]any)) != 2 {
		t.Fatalf("settings schedule = %#v", schedule)
	}
	assertWBNoSecret(t, "settings", rec.Body.String(), "refresh-uid-intl", "accessToken", "refreshToken")

	rec = f.do(http.MethodPost, "/settings/save", map[string]any{
		"schedule": map[string]any{"checkin": []any{7, 19}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("settings save status=%d body=%s", rec.Code, rec.Body)
	}
	if got := f.intl.scheduleHours("checkin", []int{9, 21}); len(got) != 2 || got[0] != 7 || got[1] != 19 {
		t.Fatalf("saved schedule = %v", got)
	}
}

func TestRoutesWorkBuddySyncProfile(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-profile", realmIntl, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/account" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"uid":"uid-profile","nickname":"Profile Name","phoneNumber":"secret-phone"}`)
	}))
	t.Cleanup(server.Close)
	oldOrigin := profileOriginFor
	oldTransport := defaultProfileTransport
	profileOriginFor = func(string) string { return server.URL }
	defaultProfileTransport = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
		return server.Client().Do(req)
	})
	t.Cleanup(func() {
		profileOriginFor = oldOrigin
		defaultProfileTransport = oldTransport
	})
	rec := f.do(http.MethodPost, "/accounts/sync-profile", map[string]any{"uid": "uid-profile"})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync-profile status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	updated := wbRouteObjects(t, body["updated"])
	if len(updated) != 1 || wbRouteText(updated[0]["nickname"]) != "Profile Name" {
		t.Fatalf("sync-profile updated = %#v", updated)
	}
	if strings.Contains(rec.Body.String(), "secret-phone") {
		t.Fatalf("sync-profile leaked a private profile field: %s", rec.Body)
	}
	account := f.intl.Pool().Get("uid-profile")
	account.mu.RLock()
	nickname := account.Auth.Nickname
	account.mu.RUnlock()
	if nickname != "Profile Name" {
		t.Fatalf("persisted nickname = %q", nickname)
	}
}

func TestRoutesWorkBuddyAddAPIKey(t *testing.T) {
	f := newWBRoutesFixture(t)
	key := "ck_routekey.secret-value"
	rec := f.do(http.MethodPost, "/accounts/add-api-key", map[string]any{
		"key": key, "nickname": "Key Account",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add-api-key status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	if body["ok"] != true || wbRouteText(body["uid"]) != "apikey-routekey" || body["updated"] != false {
		t.Fatalf("add-api-key response = %#v", body)
	}
	assertWBNoSecret(t, "add-api-key", rec.Body.String(), key, "secret-value")
	account := f.cn.Pool().Get("apikey-routekey")
	if account == nil {
		t.Fatal("API key account was not added to the cn pool")
	}
	view := account.View()
	if view.APIKey != true || view.TasksEnabled != false || view.Enabled != true {
		t.Fatalf("API key account view = %#v", view)
	}
	rec = f.do(http.MethodPost, "/accounts/add-api-key", map[string]any{"key": "not-a-key"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad key status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRoutesWorkBuddyRefreshAccount(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-refresh", realmIntl, nil)
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == tokenRefreshPath {
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"accessToken":  wbRouteToken(t, "uid-refresh", realmIntl),
				"refreshToken": "refresh-rotated",
				"expiresAt":    4102444800000,
			}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	}
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.tokenBaseURL = f.server.URL
	}
	rec := f.do(http.MethodPost, "/accounts/refresh", map[string]any{"uid": "uid-refresh"})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", rec.Code, rec.Body)
	}
	results := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["results"])
	if len(results) != 1 || results[0]["ok"] != true {
		t.Fatalf("refresh results = %#v", results)
	}
	assertWBNoSecret(t, "refresh", rec.Body.String(), "refresh-rotated")
	account := f.intl.Pool().Get("uid-refresh")
	account.mu.RLock()
	refreshToken := account.Auth.RefreshToken
	account.mu.RUnlock()
	if refreshToken != "refresh-rotated" {
		t.Fatalf("rotated refresh token = %q", refreshToken)
	}
}

func TestRoutesWorkBuddyAccountBulkActions(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-a", realmIntl, map[string]any{"group": "team"})
	wbRouteAddAccount(t, f.intl, "uid-b", realmIntl, map[string]any{"group": "team"})

	rec := f.do(http.MethodPost, "/accounts/disable", map[string]any{"uid": "uid-a", "reason": "maintenance"})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body)
	}
	view := decodeWBRoutesJSON(t, rec)["account"].(map[string]any)
	if view["enabled"] != false || view["tasksEnabled"] != false {
		t.Fatalf("disable view = %#v", view)
	}
	rec = f.do(http.MethodPost, "/accounts/enable", map[string]any{"uid": "uid-a"})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["account"].(map[string]any)["enabled"] != true {
		t.Fatalf("enable = %s", rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/tasks", map[string]any{"uid": "uid-a", "enabled": false})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["account"].(map[string]any)["tasksEnabled"] != false {
		t.Fatalf("tasks = %s", rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/note", map[string]any{"uid": "uid-a", "note": "alias note"})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["account"].(map[string]any)["note"] != "alias note" {
		t.Fatalf("note = %s", rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/weight", map[string]any{"uid": "uid-a", "weight": 5})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["account"].(map[string]any)["weight"].(json.Number).String() != "5" {
		t.Fatalf("weight = %s", rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/group", map[string]any{"uid": "uid-a", "group": "team"})
	if rec.Code != http.StatusOK {
		t.Fatalf("group = %s", rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/clear-cooldown", map[string]any{"uid": "uid-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear-cooldown = %s", rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/product", map[string]any{"uid": "uid-a", "product": "vscode"})
	if rec.Code != http.StatusOK {
		t.Fatalf("product = %s", rec.Body)
	}
	account := f.intl.Pool().Get("uid-a")
	account.mu.RLock()
	product := account.Auth.Product
	account.mu.RUnlock()
	if product != productVSCode {
		t.Fatalf("product persisted = %q", product)
	}
	rec = f.do(http.MethodPost, "/accounts/set-all", map[string]any{"group": "team", "enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-all = %s", rec.Body)
	}
	for _, uid := range []string{"uid-a", "uid-b"} {
		if f.intl.Pool().Get(uid).View().Enabled {
			t.Fatalf("set-all left %s enabled", uid)
		}
	}
	rec = f.do(http.MethodPost, "/accounts/delete", map[string]any{"uid": "uid-a"})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["deleted"] != true {
		t.Fatalf("delete = %s", rec.Body)
	}
	if f.intl.Pool().Get("uid-a") != nil {
		t.Fatal("delete left the account in the pool")
	}
}

func TestRoutesWorkBuddyDesktopImportScanAndValidation(t *testing.T) {
	f := newWBRoutesFixture(t)
	desktopDir := filepath.Join(f.dir, "desktop-auth")
	if err := os.MkdirAll(desktopDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(desktopDir, "workbuddy-desktop-ai.info"), map[string]any{
		"auth": map[string]any{"accessToken": wbRouteToken(t, "uid-desktop", realmIntl)},
		"account": map[string]any{
			"uid": "uid-desktop", "nickname": "Desktop User",
		},
	})
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.desktopDirs = []string{desktopDir}
	}

	rec := f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", rec.Code, rec.Body)
	}
	scan := decodeWBRoutesJSON(t, rec)
	if len(wbRouteObjects(t, scan["detected"])) != 1 {
		t.Fatalf("desktop scan = %#v", scan)
	}
	assertWBNoSecret(t, "desktop scan", rec.Body.String(), "uid-desktop-secret")

	rec = f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{"path": f.dir + "/not-a-credential.json"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad desktop path status=%d body=%s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{"all": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("desktop all status=%d body=%s", rec.Code, rec.Body)
	}
	imported := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["imported"])
	if len(imported) != 1 || wbRouteText(imported[0]["uid"]) != "uid-desktop" {
		t.Fatalf("desktop imported = %#v", imported)
	}
	if f.intl.Pool().Get("uid-desktop") == nil {
		t.Fatal("desktop import did not sync the pool")
	}
}

func TestRoutesWorkBuddyRealmUnavailableIsExplicit(t *testing.T) {
	f := newWBRoutesFixture(t)
	f.intl.SetTwin(nil)
	rec := f.do(http.MethodGet, "/accounts?realm=cn", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("cn without twin status=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "cn realm") {
		t.Fatalf("cn without twin message = %s", rec.Body)
	}
}

func TestRoutesWorkBuddyHealthCacheInvalidation(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-cache", realmIntl, nil)
	first, err := f.intl.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if first["accounts"].(int) != 1 {
		t.Fatalf("health accounts = %#v", first["accounts"])
	}
	wbRouteAddAccount(t, f.intl, "uid-cache-2", realmIntl, nil)
	if _, err := f.intl.Health(context.Background(), false); err != nil {
		t.Fatalf("cached health: %v", err)
	}
	f.intl.invalidateHealth()
	refreshed, err := f.intl.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("refreshed health: %v", err)
	}
	if refreshed["accounts"].(int) != 2 {
		t.Fatalf("refreshed health accounts = %#v", refreshed["accounts"])
	}
}

func TestRoutesWorkBuddyImportRejectsNonObjectRow(t *testing.T) {
	f := newWBRoutesFixture(t)
	rec := f.do(http.MethodPost, "/accounts/import", []any{"nope"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-object import status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRoutesWorkBuddyOverviewNeverLeaksCredentials(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-overview", realmIntl, nil)
	value, err := f.intl.Overview(context.Background(), false)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	assertWBNoSecret(t, "overview", string(raw), "refresh-uid-overview")
	for _, key := range []string{"provider", "realm", "stats", "accounts", "health", "models"} {
		if _, exists := value.(map[string]any)[key]; !exists {
			t.Fatalf("overview missing %q: %s", key, raw)
		}
	}
}

func TestRoutesWorkBuddyLoginAutoExcludesExternalSlots(t *testing.T) {
	f := newWBRoutesFixture(t)
	if _, err := f.store.Merge(map[string]any{
		"proxy_slots": []any{
			map[string]any{"id": "slot-a", "url": "http://127.0.0.1:1111"},
			map[string]any{"id": "slot-b", "url": "http://127.0.0.1:2222"},
			map[string]any{"id": "slot-c", "url": "http://127.0.0.1:3333"},
		},
	}); err != nil {
		t.Fatalf("save slots: %v", err)
	}
	wbRouteAddAccount(t, f.intl, "uid-slot", realmIntl, map[string]any{"proxySlot": "slot-a"})
	f.intl.SetLoginTakenSlots(func() []string { return []string{"slot-b"} })
	slot, _, reserved, err := f.intl.reserveLoginProxy(loginSlotAuto)
	if err != nil {
		t.Fatalf("reserveLoginProxy: %v", err)
	}
	if !reserved || slot != "slot-c" {
		t.Fatalf("auto slot = %q reserved=%v, want slot-c", slot, reserved)
	}
	f.intl.releaseLoginProxy(slot)
}

func TestRoutesWorkBuddyTwinCompositionAndNotFound(t *testing.T) {
	f := newWBRoutesFixture(t)
	if f.intl.Peer() != f.cn || f.cn.Peer() != f.intl {
		t.Fatal("SetTwin did not wire both provider sides")
	}
	if !strings.Contains(f.intl.RouteNotFoundMessage(), "workbuddy") {
		t.Fatalf("not-found message = %q", f.intl.RouteNotFoundMessage())
	}
	rec := f.do(http.MethodGet, "/accounts/not-a-route", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "workbuddy") {
		t.Fatalf("unknown route status=%d body=%s", rec.Code, rec.Body)
	}
}
