package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
		provider.acpLinkValidator = func(string) error { return nil }
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
		"POST /accounts/credits/fetch",
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
		"POST /settings/restriction/reset",
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
	for _, key := range []string{"restrictedFor", "softStreak", "unscopedStreak", "breakerFor", "degradeFor", "fails", "failWindow", "lastFailFor", "machineId", "sessionId"} {
		if _, exists := rows[0][key]; !exists {
			t.Fatalf("account view missing %q: %#v", key, rows[0])
		}
	}
	if len(wbRouteText(rows[0]["machineId"])) != 36 || len(wbRouteText(rows[0]["sessionId"])) != 36 {
		t.Fatalf("account fingerprint ids = %q/%q", rows[0]["machineId"], rows[0]["sessionId"])
	}
	if rows[0]["softStreak"].(json.Number).String() != "0" ||
		rows[0]["unscopedStreak"].(json.Number).String() != "0" ||
		rows[0]["fails"].(json.Number).String() != "0" ||
		rows[0]["failWindow"].(json.Number).String() != "600" ||
		rows[0]["breakerFor"] != nil || rows[0]["degradeFor"] != nil || rows[0]["lastFailFor"] != nil {
		t.Fatalf("account parity placeholders = %#v", rows[0])
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

func TestRoutesImportCNChecksInAfterAdd(t *testing.T) {
	f := newWBRoutesFixture(t)
	token := wbRouteToken(t, "uid-import-cn", realmCN)
	rec := f.do(http.MethodPost, "/accounts/import", map[string]any{
		"data": map[string]any{"accounts": []any{map[string]any{
			"uid": "uid-import-cn", "accessToken": token, "refreshToken": "refresh-import-cn",
			"realm": realmCN, "nickname": "Imported CN",
		}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("cn import status=%d body=%s", rec.Code, rec.Body)
	}
	if f.cn.Pool().Get("uid-import-cn") == nil {
		t.Fatal("cn import did not reach the cn pool")
	}
	if got := f.fake.count(http.MethodPost, tasksCheckinPath); got != 1 {
		t.Fatalf("cn import initial check-in calls = %d, want 1", got)
	}
	rows := readActivityLines(t, f.cn.activityHistoryPath())
	found := false
	for _, row := range rows {
		if row.Trigger == taskTriggerAccountImport && row.UID == "uid-import-cn" && row.Task == taskCheckinTask {
			found = true
		}
	}
	if !found {
		t.Fatalf("cn import activity rows = %+v, want account_import check-in", rows)
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
	if got := f.fake.count(http.MethodPost, tasksCheckinPath); got != 1 {
		t.Fatalf("OAuth success initial check-in calls = %d, want 1", got)
	}
	loginAccount := f.cn.Pool().Get("uid-login")
	loginAccount.mu.RLock()
	lastCheckin := loginAccount.Auth.LastCheckin
	loginAccount.mu.RUnlock()
	if !strings.HasPrefix(lastCheckin, time.Now().Format("2006-01-02")) {
		t.Fatalf("OAuth success lastCheckin = %q", lastCheckin)
	}
	rows := readActivityLines(t, f.cn.activityHistoryPath())
	foundAdd := false
	for _, row := range rows {
		if row.Trigger == taskTriggerAccountAdd && row.Task == taskCheckinTask && row.UID == "uid-login" {
			foundAdd = true
		}
	}
	if !foundAdd {
		t.Fatalf("OAuth success activity rows = %+v, want account_add check-in", rows)
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

func TestRoutesAccountsTestPinnedAndMulti(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-probe-a", realmIntl, nil)
	wbRouteAddAccount(t, f.intl, "uid-probe-b", realmIntl, nil)
	wbRouteAddAccount(t, f.cn, "uid-probe-cn", realmCN, nil)

	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		mu.Lock()
		seen[uid]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	serverURL := server.URL
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.chatBaseURL = serverURL
		provider.chatDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
			return server.Client().Do(req)
		})
		setFakePublicNetwork(provider)
	}

	rec := f.do(http.MethodPost, "/accounts/test", map[string]any{"uid": "uid-probe-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("single probe status=%d body=%s", rec.Code, rec.Body)
	}
	row := decodeWBRoutesJSON(t, rec)
	if row["ok"] != true || wbRouteText(row["reply"]) != "hello world" ||
		row["status"].(json.Number).String() != "200" || row["network"] != false {
		t.Fatalf("single probe row = %#v", row)
	}
	mu.Lock()
	if seen["uid-probe-a"] != 1 || seen["uid-probe-b"] != 0 || seen["uid-probe-cn"] != 0 {
		t.Fatalf("single probe requests = %#v, want only uid-probe-a", seen)
	}
	mu.Unlock()

	rec = f.do(http.MethodPost, "/accounts/test", map[string]any{"realm": "intl"})
	if rec.Code != http.StatusOK {
		t.Fatalf("multi probe status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	rows := wbRouteObjects(t, body["results"])
	if len(rows) != 2 {
		t.Fatalf("multi probe rows = %#v", rows)
	}
	summary := body["summary"].(map[string]any)
	if summary["tested"].(json.Number).String() != "2" || summary["passed"].(json.Number).String() != "2" ||
		summary["failed"].(json.Number).String() != "0" || summary["networkDown"] != false {
		t.Fatalf("multi probe summary = %#v", summary)
	}
	network := body["network"].(map[string]any)
	if _, ok := network["ok"]; !ok {
		t.Fatalf("multi probe network = %#v", network)
	}
	if wbRouteText(network["host"]) == "" || wbRouteText(network["ip"]) == "" ||
		network["dns_ms"] == nil || network["tcp_ms"] == nil || network["ok"] != true {
		t.Fatalf("multi probe network fields = %#v", network)
	}
	mu.Lock()
	if seen["uid-probe-a"] != 2 || seen["uid-probe-b"] != 1 || seen["uid-probe-cn"] != 0 {
		t.Fatalf("multi probe requests = %#v", seen)
	}
	mu.Unlock()
}

func TestRoutesCreditsAliasRealmAndRestrictionReset(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-alias", realmIntl, nil)

	rec := f.do(http.MethodPost, "/accounts/credits/fetch", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("credits/fetch status=%d body=%s", rec.Code, rec.Body)
	}
	// v1's POST sweep response has no busy key unless a sweep is already
	// running; GET is the snapshot endpoint that always reports it.
	if _, ok := decodeWBRoutesJSON(t, rec)["busy"]; ok {
		t.Fatalf("credits/fetch unexpectedly carried busy: %s", rec.Body)
	}
	rec = f.do(http.MethodGet, "/accounts/credits", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("credits GET status=%d body=%s", rec.Code, rec.Body)
	}
	if busy, ok := decodeWBRoutesJSON(t, rec)["busy"]; !ok || busy != false {
		t.Fatalf("credits GET busy = %#v (present=%v)", busy, ok)
	}

	realmContext := context.Background()
	if got := f.intl.ActiveRealm(realmContext); got != realmIntl || len(f.intl.RealmOptions()) != 2 {
		t.Fatalf("realm state = %q options=%v", got, f.intl.RealmOptions())
	}
	if err := f.intl.SetActiveRealm(realmContext, "cn"); err != nil {
		t.Fatalf("realm switch: %v", err)
	}
	if got := f.intl.activeRealm(); got != realmCN {
		t.Fatalf("active realm = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "active_realm.json"))
	if err != nil || !strings.Contains(string(data), `"realm": "cn"`) {
		t.Fatalf("active_realm.json = %q, err=%v", data, err)
	}
	if err := f.intl.SetActiveRealm(realmContext, "bogus"); err == nil {
		t.Fatal("bad realm accepted")
	}

	rec = f.do(http.MethodPost, "/settings/restriction/reset", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("restriction reset status=%d body=%s", rec.Code, rec.Body)
	}
	reset := decodeWBRoutesJSON(t, rec)
	if reset["ok"] != true {
		t.Fatalf("restriction reset = %#v", reset)
	}
	if !strings.Contains(wbRouteText(reset["message"]), "unchanged") {
		t.Fatalf("restriction reset message overclaims: %q", reset["message"])
	}
}

func TestRoutesProductSemantics(t *testing.T) {
	f := newWBRoutesFixture(t)
	account := wbRouteAddAccount(t, f.intl, "uid-product", realmIntl, nil)
	account.mu.Lock()
	account.State.CoolUntil = time.Now().Add(time.Hour)
	account.State.ErrCount = 2
	account.State.LastError = "rate limited"
	if account.Auth.Extra == nil {
		account.Auth.Extra = map[string]json.RawMessage{}
	}
	account.Auth.Extra["modelCaps"] = json.RawMessage(`{"free-model":{"at":1,"reset":9999999999}}`)
	account.mu.Unlock()
	cnAccount := wbRouteAddAccount(t, f.cn, "uid-product-cn", realmCN, nil)

	rec := f.do(http.MethodPost, "/accounts/product", map[string]any{"uid": "uid-product", "product": "cli"})
	if rec.Code != http.StatusOK {
		t.Fatalf("product status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	changed := body["changed"].([]any)
	if len(changed) != 1 || wbRouteText(changed[0]) != "uid-prod" {
		t.Fatalf("product changed = %#v", changed)
	}
	account.mu.RLock()
	product := account.Auth.Product
	cooling := account.State.CoolUntil
	fails := account.State.ErrCount
	lastError := account.State.LastError
	caps := string(account.Auth.Extra["modelCaps"])
	account.mu.RUnlock()
	if product != productCLI || !cooling.IsZero() || fails != 0 || lastError != "" {
		t.Fatalf("product state product=%q cooling=%v fails=%d lastError=%q", product, cooling, fails, lastError)
	}
	if !strings.Contains(caps, "free-model") {
		t.Fatalf("product cleared modelCaps: %q", caps)
	}

	rec = f.do(http.MethodPost, "/accounts/product", map[string]any{"uid": "uid-product", "product": "cli"})
	if rec.Code != http.StatusOK || len(decodeWBRoutesJSON(t, rec)["changed"].([]any)) != 0 {
		t.Fatalf("same product should not report a change: %s", rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/product", map[string]any{"uid": "missing", "product": "cli"})
	if rec.Code != http.StatusOK || len(decodeWBRoutesJSON(t, rec)["changed"].([]any)) != 0 {
		t.Fatalf("unknown product uid = %d %s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/product", map[string]any{"uid": "uid-product-cn", "product": "vscode"})
	if rec.Code != http.StatusOK {
		t.Fatalf("cn product status=%d body=%s", rec.Code, rec.Body)
	}
	cnAccount.mu.RLock()
	cnProduct := cnAccount.Auth.Product
	cnAccount.mu.RUnlock()
	if cnProduct != productVSCode {
		t.Fatalf("cn product = %q by uid", cnProduct)
	}
	rec = f.do(http.MethodPost, "/accounts/product", map[string]any{"product": "workbuddy"})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk product status=%d body=%s", rec.Code, rec.Body)
	}
	body = decodeWBRoutesJSON(t, rec)
	if len(body["changed"].([]any)) != 2 {
		t.Fatalf("bulk product changed = %#v", body["changed"])
	}
	if accounts := wbRouteObjects(t, body["accounts"]); len(accounts) != 2 {
		t.Fatalf("bulk product response scope = %#v", body["accounts"])
	}
}

func TestRoutesExportMasksNamesAndStripsExtras(t *testing.T) {
	f := newWBRoutesFixture(t)
	account := wbRouteAddAccount(t, f.intl, "uid-export", realmIntl, nil)
	account.mu.Lock()
	account.Auth.Nickname = "老王 13812345678"
	if account.Auth.Extra == nil {
		account.Auth.Extra = map[string]json.RawMessage{}
	}
	account.Auth.Extra["proxy"] = json.RawMessage(`"http://user:pass@proxy.local:8080"`)
	account.Auth.Extra["deviceToken"] = json.RawMessage(`"device-secret"`)
	account.Auth.Extra["nestedSecrets"] = json.RawMessage(`{"accessToken":"nested-hidden"}`)
	account.mu.Unlock()

	rec := f.do(http.MethodGet, "/accounts/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body)
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("export Cache-Control = %q", cache)
	}
	rows := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	if wbRouteText(rows[0]["nickname"]) != "老王 138****5678" {
		t.Fatalf("export nickname = %q", rows[0]["nickname"])
	}
	if wbRouteText(rows[0]["accessToken"]) == "" {
		t.Fatal("export default should retain credentials")
	}

	rec = f.do(http.MethodGet, "/accounts/export?secrets=0", nil)
	rows = wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	for _, key := range []string{"accessToken", "refreshToken", "proxy", "deviceToken"} {
		if _, present := rows[0][key]; present {
			t.Fatalf("secrets=0 export retained %q: %#v", key, rows[0])
		}
	}
	if strings.Contains(rec.Body.String(), "nested-hidden") {
		t.Fatalf("secrets=0 export retained a nested credential: %s", rec.Body)
	}
	rec = f.do(http.MethodGet, "/accounts", nil)
	rows = wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	if wbRouteText(rows[0]["nickname"]) != "老王 138****5678" {
		t.Fatalf("panel nickname = %q", rows[0]["nickname"])
	}
}

func TestRoutesGrowthSnapshotInvalidatedAfterClaim(t *testing.T) {
	f := newWBRoutesFixture(t)
	account := wbRouteAddAccount(t, f.cn, "uid-claim", realmCN, map[string]any{
		"growthTasks": map[string]any{"tasks": []any{}, "summary": map[string]any{"energy": 1}},
	})
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/claim") {
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"credit": 25}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	}
	rec := f.do(http.MethodPost, "/tasks/claim", map[string]any{"uid": "uid-claim", "code": "create_canvas"})
	if rec.Code != http.StatusOK || decodeWBRoutesJSON(t, rec)["ok"] != true {
		t.Fatalf("claim = %d %s", rec.Code, rec.Body)
	}
	account.mu.RLock()
	_, cached := account.Auth.Extra["growthTasks"]
	_, claims := account.Auth.Extra["growthClaims"]
	account.mu.RUnlock()
	if cached {
		t.Fatal("claim left the stale growthTasks snapshot")
	}
	if !claims {
		t.Fatal("claim did not persist growthClaims")
	}
}

func TestRoutesActivityRetentionRedactionAndCaps(t *testing.T) {
	f := newWBRoutesFixture(t)
	now := time.Now()
	path := f.intl.activityHistoryPath()
	var body strings.Builder
	for i := 0; i < activityMaxRecords+5; i++ {
		row := ActivityEvent{
			TS: now.Format(time.RFC3339), UID: strings.Repeat("u", 120), Nickname: "13812345678",
			Realm: realmCN, Task: taskCheckinTask, Trigger: taskTriggerScheduler, OK: true,
			Message: "token=supersecret Bearer abcdefghijk sk-abcdefghijklmnop 0123456789abcdef0123456789abcdef",
		}
		raw, _ := json.Marshal(row)
		body.Write(raw)
		body.WriteByte('\n')
	}
	old := ActivityEvent{TS: now.Add(-100 * 24 * time.Hour).Format(time.RFC3339), UID: "old", Realm: realmCN, Task: taskCheckinTask, Trigger: taskTriggerScheduler, OK: true}
	raw, _ := json.Marshal(old)
	body.Write(raw)
	body.WriteByte('\n')
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := f.do(http.MethodGet, "/activity/history?range=all&limit=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", rec.Code, rec.Body)
	}
	result := decodeWBRoutesJSON(t, rec)
	rows := wbRouteObjects(t, result["rows"])
	if len(rows) != 1 {
		t.Fatalf("activity rows = %#v", rows)
	}
	if len([]rune(wbRouteText(rows[0]["uid"]))) != 80 {
		t.Fatalf("activity uid cap = %d", len([]rune(wbRouteText(rows[0]["uid"]))))
	}
	message := wbRouteText(rows[0]["message"])
	for _, secret := range []string{"supersecret", "abcdefghijk", "sk-abcdefghijklmnop", "0123456789abcdef"} {
		if strings.Contains(message, secret) {
			t.Fatalf("activity retained secret %q: %s", secret, message)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > activityMaxRecords {
		t.Fatalf("activity file retained %d rows, want <= %d", len(lines), activityMaxRecords)
	}
	if strings.Contains(string(data), `"uid":"old"`) || strings.Contains(string(data), `"uid": "old"`) {
		t.Fatal("activity retention kept the >90d row")
	}
}

func TestRoutesDailyChatRunsWebLeg(t *testing.T) {
	fake, provider, _, _ := setupDailyWebFake(t, realmIntl, nil)
	mux := http.NewServeMux()
	api.RegisterProviderRoutes(mux, provider)
	request := httptest.NewRequest(http.MethodPost, "/accounts/daily-chat", strings.NewReader(`{"uid":"web-daily"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("daily-chat status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeWBRoutesJSON(t, recorder)
	rows := wbRouteObjects(t, body["results"])
	if len(rows) != 1 || rows[0]["ok"] != true {
		t.Fatalf("daily-chat rows = %#v", rows)
	}
	web, _ := rows[0]["web"].(map[string]any)
	if web == nil || wbRouteText(web["conversation"]) == "" || web["ok"] != true {
		t.Fatalf("daily-chat web leg = %#v", rows[0]["web"])
	}
	_ = fake
}

func TestRoutesCreditsBusyAggregate(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-busy-a", realmIntl, nil)
	wbRouteAddAccount(t, f.intl, "uid-busy-b", realmIntl, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == creditsSummaryPath {
			once.Do(func() { close(started) })
			<-release
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"remain": 10, "used": 0, "size": 10, "packages": []any{},
			}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- f.do(http.MethodPost, "/accounts/credits", map[string]any{"realm": "intl"})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first credits sweep never reached upstream")
	}
	second := f.do(http.MethodPost, "/accounts/credits", map[string]any{"realm": "intl"})
	if second.Code != http.StatusOK {
		t.Fatalf("busy sweep status=%d body=%s", second.Code, second.Body)
	}
	if decodeWBRoutesJSON(t, second)["busy"] != true {
		t.Fatalf("second sweep = %s, want busy", second.Body)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first credits sweep did not finish")
	}
}

func TestRoutesDailyChatSkipsWebOnDesktopRestriction(t *testing.T) {
	dir := t.TempDir()
	writeTaskAccount(t, dir, "intl-restricted", realmIntl, nil)
	fake, server := newTaskTestUpstream(t, func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == chatPath {
			return http.StatusForbidden, map[string]any{
				"code": 11140, "msg": "账号存在异常行为，已被限制使用",
			}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider, pool := setupTaskProvider(t, realmIntl, dir, server.URL)
	account := accountByUID(t, pool, "intl-restricted")
	result, err := provider.runDailyActivity(context.Background(), account, taskTriggerManual)
	if err != nil {
		t.Fatalf("runDailyActivity: %v", err)
	}
	if !result.Restricted || result.OK || result.Web != nil {
		t.Fatalf("restricted daily result = %+v", result)
	}
	if got := fake.count(http.MethodPost, "/console/as/conversations/"); got != 0 {
		t.Fatalf("web conversation created after desktop restriction: %d", got)
	}
}

func TestRoutesDailyChatWebBusyPropagates(t *testing.T) {
	_, provider, pool, _ := setupDailyWebFake(t, realmIntl, nil)
	provider.dailyWebRun = func(context.Context, *Account, string) (DailyResult, error) {
		return DailyResult{Busy: true, Msg: taskBusyMessage}, nil
	}
	account := accountByUID(t, pool, "web-daily")
	result, err := provider.runDailyActivity(context.Background(), account, taskTriggerManual)
	if err != nil {
		t.Fatalf("runDailyActivity: %v", err)
	}
	if !result.Busy || result.OK || result.Web == nil || !result.Web.Busy {
		t.Fatalf("busy web result = %+v", result)
	}
	account.mu.RLock()
	stamped := account.Auth.LastDailyChat
	account.mu.RUnlock()
	if !strings.HasPrefix(stamped, time.Now().Format("2006-01-02")) {
		t.Fatalf("busy web leg rolled back desktop stamp = %q", stamped)
	}
	if data, err := os.ReadFile(provider.activityHistoryPath()); err == nil && strings.Contains(string(data), `"ok":false`) {
		t.Fatalf("busy web leg recorded a failure row: %s", data)
	}
}

func TestRoutesManualWebDailyDoesNotStampScheduledDaily(t *testing.T) {
	_, provider, pool, _ := setupDailyWebFake(t, realmIntl, nil)
	account := accountByUID(t, pool, "web-daily")
	account.mu.RLock()
	before := account.Auth.LastDailyChat
	account.mu.RUnlock()
	if before != "" {
		t.Fatalf("initial lastDailyChat = %q", before)
	}
	request := httptest.NewRequest(http.MethodPost, "/accounts/daily-chat-web",
		strings.NewReader(`{"uid":"web-daily"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	provider.handleAccountsDailyChatWeb(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("manual web daily status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeWBRoutesJSON(t, recorder)
	rows := wbRouteObjects(t, body["results"])
	if len(rows) != 1 || rows[0]["ok"] != true {
		t.Fatalf("manual web daily results = %#v", rows)
	}
	account.mu.RLock()
	lastDaily := account.Auth.LastDailyChat
	lastWeb := authExtraString(account.Auth, "lastDailyChatWeb")
	account.mu.RUnlock()
	if lastDaily != "" {
		t.Fatalf("manual web daily stamped lastDailyChat = %q", lastDaily)
	}
	if !strings.HasPrefix(lastWeb, time.Now().Format("2006-01-02")) {
		t.Fatalf("manual web daily lastDailyChatWeb = %q", lastWeb)
	}
}

func TestRoutesDailyChatWebFailureRetriesAndKeepsOneRow(t *testing.T) {
	webFail := true
	_, provider, pool, _ := setupDailyWebFake(t, realmIntl, func(_ *taskTestUpstream, _ int) (int, any) {
		if webFail {
			return http.StatusServiceUnavailable, map[string]any{"code": 14220, "msg": "try later"}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "conv-1"}}
	})
	account := accountByUID(t, pool, "web-daily")
	first, err := provider.runDailyActivity(context.Background(), account, taskTriggerManual)
	if err != nil {
		t.Fatalf("first runDailyActivity: %v", err)
	}
	if first.OK || first.Web == nil || first.Web.OK {
		t.Fatalf("first result = %+v, want web failure", first)
	}
	account.mu.RLock()
	stamped := account.Auth.LastDailyChat
	account.mu.RUnlock()
	if stamped != "" {
		t.Fatalf("failed web leg stamped lastDailyChat = %q", stamped)
	}
	rows := readActivityLines(t, provider.activityHistoryPath())
	if len(rows) != 1 || rows[0].OK || !strings.Contains(rows[0].Message, "网页通道失败") {
		t.Fatalf("first activity rows = %+v", rows)
	}
	if _, err := provider.runDailyActivity(context.Background(), account, taskTriggerManual); err != nil {
		t.Fatalf("second failed runDailyActivity: %v", err)
	}
	rows = readActivityLines(t, provider.activityHistoryPath())
	if len(rows) != 1 || rows[0].OK {
		t.Fatalf("repeated failure rows = %+v, want one failure row", rows)
	}

	webFail = false
	second, err := provider.runDailyActivity(context.Background(), account, taskTriggerManual)
	if err != nil {
		t.Fatalf("retry runDailyActivity: %v", err)
	}
	if !second.OK || second.Web == nil || !second.Web.OK {
		t.Fatalf("retry result = %+v, want success", second)
	}
	account.mu.RLock()
	stamped = account.Auth.LastDailyChat
	account.mu.RUnlock()
	if !strings.HasPrefix(stamped, time.Now().Format("2006-01-02")) {
		t.Fatalf("retry lastDailyChat = %q", stamped)
	}
	rows = readActivityLines(t, provider.activityHistoryPath())
	if len(rows) != 1 || !rows[0].OK || !strings.Contains(rows[0].Message, "网页通道") {
		t.Fatalf("retry activity rows = %+v, want one replaced success row", rows)
	}
}

func readActivityLines(t *testing.T, path string) []ActivityEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read activity history: %v", err)
	}
	rows := []ActivityEvent{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row ActivityEvent
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode activity row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRoutesAccountsTestExpiredTokenRefreshAnd401Retry(t *testing.T) {
	f := newWBRoutesFixture(t)
	expired := Auth{
		UID: "uid-expired", Nickname: "Expired", Realm: realmIntl,
		AccessToken:  wbRouteToken(t, "uid-expired", realmIntl),
		RefreshToken: "refresh-expired", Platform: "CLI", Source: sourceOAuth,
		Enabled: true, EnabledSet: true, TasksEnabled: true, TasksEnabledSet: true,
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	if _, err := f.intl.Pool().Add(expired); err != nil {
		t.Fatalf("add expired account: %v", err)
	}
	wbRouteAddAccount(t, f.intl, "uid-401", realmIntl, nil)
	rotated := makeJWT(t, map[string]any{
		"sub": "uid-expired", "iss": "https://www.workbuddy.ai/auth/realms/copilot",
		"exp": 4102444800, "jti": "rotated-expired",
	})
	rotated401 := makeJWT(t, map[string]any{
		"sub": "uid-401", "iss": "https://www.workbuddy.ai/auth/realms/copilot",
		"exp": 4102444800, "jti": "rotated-401",
	})
	var refreshCalls, chat401Calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenRefreshPath {
			refreshCalls++
			token := rotated
			if r.Header.Get("X-User-Id") == "uid-401" {
				token = rotated401
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"accessToken": token, "refreshToken": "rotated", "expiresAt": 4102444800000,
			}})
			return
		}
		if r.Header.Get("X-User-Id") == "uid-401" {
			chat401Calls++
			if chat401Calls == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"expired"}`)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.chatBaseURL = server.URL
		provider.chatDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
			return server.Client().Do(req)
		})
	}
	f.intl.tokenBaseURL = server.URL

	rec := f.do(http.MethodPost, "/accounts/test", map[string]any{"uid": "uid-expired"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expired probe status=%d body=%s", rec.Code, rec.Body)
	}
	row := decodeWBRoutesJSON(t, rec)
	if row["ok"] != true || wbRouteText(row["reply"]) != "ok" {
		t.Fatalf("expired probe = %#v", row)
	}
	if refreshCalls != 1 {
		t.Fatalf("expired probe refresh calls = %d, want 1", refreshCalls)
	}

	rec = f.do(http.MethodPost, "/accounts/test", map[string]any{"uid": "uid-401"})
	row = decodeWBRoutesJSON(t, rec)
	if row["ok"] != true || wbRouteText(row["reply"]) != "ok" {
		t.Fatalf("401 probe = %#v", row)
	}
	if chat401Calls != 2 || refreshCalls != 2 {
		t.Fatalf("401 probe chat=%d refresh=%d, want exactly one retry", chat401Calls, refreshCalls)
	}
}

func TestRoutesAccountsTest401RefreshGuards(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-no-refresh", realmIntl, nil)
	account := f.intl.Pool().Get("uid-no-refresh")
	account.mu.Lock()
	account.Auth.RefreshToken = ""
	account.mu.Unlock()
	unchanged := wbRouteToken(t, "uid-unchanged", realmIntl)
	unchangedAuth := Auth{
		UID: "uid-unchanged", Nickname: "Unchanged", Realm: realmIntl,
		AccessToken: unchanged, RefreshToken: "refresh-unchanged", Platform: "CLI",
		Source: sourceOAuth, Enabled: true, EnabledSet: true,
		TasksEnabled: true, TasksEnabledSet: true,
	}
	if _, err := f.intl.Pool().Add(unchangedAuth); err != nil {
		t.Fatalf("add unchanged account: %v", err)
	}
	apiKey := Auth{
		UID: "apikey-guard", Nickname: "API Key", Realm: realmCN,
		Domain: "www.workbuddy.cn", AccessToken: "ck_guard.secret", APIKey: true,
		Platform: "CLI", Source: sourceAPIKey, Enabled: true, EnabledSet: true,
		TasksEnabled: false, TasksEnabledSet: true,
	}
	if _, err := f.cn.Pool().Add(apiKey); err != nil {
		t.Fatalf("add api key: %v", err)
	}

	var mu sync.Mutex
	refreshes := map[string]int{}
	chats := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		if r.URL.Path == tokenRefreshPath {
			mu.Lock()
			refreshes[uid]++
			mu.Unlock()
			token := unchanged
			if uid != "uid-unchanged" {
				token = wbRouteToken(t, uid, realmIntl)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"accessToken": token, "refreshToken": "rotated", "expiresAt": 4102444800000,
			}})
			return
		}
		mu.Lock()
		chats[uid]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"expired"}`)
	}))
	t.Cleanup(server.Close)
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.chatBaseURL = server.URL
		provider.chatDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
			return server.Client().Do(req)
		})
		provider.tokenBaseURL = server.URL
	}

	for _, uid := range []string{"uid-no-refresh", "apikey-guard", "uid-unchanged"} {
		rec := f.do(http.MethodPost, "/accounts/test", map[string]any{"uid": uid})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", uid, rec.Code, rec.Body)
		}
		row := decodeWBRoutesJSON(t, rec)
		if row["ok"] != false || row["status"].(json.Number).String() != "401" {
			t.Fatalf("%s row = %#v", uid, row)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshes["uid-no-refresh"] != 0 || refreshes["apikey-guard"] != 0 {
		t.Fatalf("unrefreshable accounts hit refresh: %#v", refreshes)
	}
	if chats["uid-no-refresh"] != 1 || chats["apikey-guard"] != 1 {
		t.Fatalf("unrefreshable chat attempts = %#v", chats)
	}
	if refreshes["uid-unchanged"] != 1 || chats["uid-unchanged"] != 1 {
		t.Fatalf("no-op refresh retried the same 401: refresh=%#v chats=%#v", refreshes, chats)
	}
}

func TestRoutesAccountsTestPropagatesAbortedSSE(t *testing.T) {
	f := newWBRoutesFixture(t)
	account := wbRouteAddAccount(t, f.intl, "uid-abort", realmIntl, nil)
	account.mu.Lock()
	account.State.ErrCount = 2
	account.mu.Unlock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "500")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	t.Cleanup(server.Close)
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.chatBaseURL = server.URL
		provider.chatDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
			return server.Client().Do(req)
		})
	}
	rec := f.do(http.MethodPost, "/accounts/test", map[string]any{"uid": "uid-abort"})
	if rec.Code != http.StatusOK {
		t.Fatalf("aborted probe status=%d body=%s", rec.Code, rec.Body)
	}
	row := decodeWBRoutesJSON(t, rec)
	if row["ok"] != false || row["status"].(json.Number).String() == "200" || wbRouteText(row["error"]) == "" {
		t.Fatalf("aborted probe row = %#v", row)
	}
	account.mu.RLock()
	fails := account.State.ErrCount
	account.mu.RUnlock()
	if fails != 2 {
		t.Fatalf("aborted stream changed success/error counters: fails=%d", fails)
	}
}

func TestRoutesPanelPhoneMaskBoundaries(t *testing.T) {
	f := newWBRoutesFixture(t)
	account := wbRouteAddAccount(t, f.intl, "uid-phone-mask", realmIntl, nil)
	account.mu.Lock()
	account.Auth.Nickname = "老王 138-1234-5678"
	account.mu.Unlock()
	long := wbRouteAddAccount(t, f.intl, "uid-long-digits", realmIntl, nil)
	long.mu.Lock()
	long.Auth.Nickname = "13812345678901234567890"
	long.mu.Unlock()
	order := wbRouteAddAccount(t, f.cn, "uid-order-id", realmCN, nil)
	order.mu.Lock()
	order.Auth.Nickname = "6ef20bf4-76fa-4085-b179-97517476cc1e"
	order.mu.Unlock()

	rec := f.do(http.MethodGet, "/accounts", nil)
	rows := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	byUID := map[string]map[string]any{}
	for _, row := range rows {
		byUID[wbRouteText(row["uid"])] = row
	}
	if got := wbRouteText(byUID["uid-phone-mask"]["nickname"]); got != "老王 138****5678" {
		t.Fatalf("single-account nickname = %q", got)
	}
	if got := wbRouteText(byUID["uid-long-digits"]["nickname"]); got != "13812345678901234567890" {
		t.Fatalf("long digit nickname was masked: %q", got)
	}

	rec = f.do(http.MethodGet, "/accounts?realm=cn", nil)
	rows = wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	if got := wbRouteText(rows[0]["nickname"]); got != "6ef20bf4-76fa-4085-b179-97517476cc1e" {
		t.Fatalf("order id nickname was masked: %q", got)
	}
	nested := maskPanelNames(map[string]any{
		"account": map[string]any{"nickname": "13812345678"},
		"uid":     "6ef20bf4-76fa-4085-b179-97517476cc1e",
	}).(map[string]any)
	accountMap := nested["account"].(map[string]any)
	if wbRouteText(accountMap["nickname"]) != "138****5678" {
		t.Fatalf("nested account nickname = %#v", accountMap)
	}
	if wbRouteText(nested["uid"]) != "6ef20bf4-76fa-4085-b179-97517476cc1e" {
		t.Fatalf("identity uid was masked: %#v", nested["uid"])
	}
}

func TestRoutesActivityReadErrorIsNotSilentlyEmpty(t *testing.T) {
	f := newWBRoutesFixture(t)
	dir := filepath.Join(f.dir, "activity-as-dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.intl.SetActivityPath(dir)
	rec := f.do(http.MethodGet, "/activity/history?range=all", nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "unreadable") {
		t.Fatalf("unreadable activity status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRoutesStandaloneWebSuccessReplacesDailyFailure(t *testing.T) {
	webFail := true
	_, provider, pool, _ := setupDailyWebFake(t, realmIntl, func(_ *taskTestUpstream, _ int) (int, any) {
		if webFail {
			return http.StatusServiceUnavailable, map[string]any{"code": 14220, "msg": "try later"}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "conv-1"}}
	})
	account := accountByUID(t, pool, "web-daily")
	if result, err := provider.runDailyActivity(context.Background(), account, taskTriggerManual); err != nil || result.OK {
		t.Fatalf("initial failed activity = %+v, %v", result, err)
	}
	if rows := readActivityLines(t, provider.activityHistoryPath()); len(rows) != 1 || rows[0].OK {
		t.Fatalf("initial rows = %+v", rows)
	}
	webFail = false
	web, err := provider.runDailyChatWeb(context.Background(), account, taskTriggerManual)
	if err != nil || !web.OK {
		t.Fatalf("standalone web = %+v, %v", web, err)
	}
	provider.recordDailyActivity(web.Activity)
	rows := readActivityLines(t, provider.activityHistoryPath())
	if len(rows) != 1 || !rows[0].OK || !strings.Contains(rows[0].Message, "网页通道") {
		t.Fatalf("standalone web rows = %+v, want one replacement success row", rows)
	}
}

func TestRoutesDailyActivityRealmCollisionKeepsRows(t *testing.T) {
	f := newWBRoutesFixture(t)
	now := time.Now().Format(time.RFC3339)
	f.intl.recordDailyActivity(&ActivityEvent{
		TS: now, UID: "same-uid", Realm: realmCN, Task: taskDailyChatTask,
		Trigger: taskTriggerManual, OK: false, Message: "cn failure",
	})
	f.intl.recordDailyActivity(&ActivityEvent{
		TS: now, UID: "same-uid", Realm: realmIntl, Task: taskDailyChatTask,
		Trigger: taskTriggerManual, OK: true, Message: "intl success",
	})
	rows := readActivityLines(t, f.intl.activityHistoryPath())
	if len(rows) != 2 || rows[0].Realm != realmCN || rows[1].Realm != realmIntl {
		t.Fatalf("realm collision rows = %+v, want separate realm rows", rows)
	}
}

func setFakePublicNetwork(provider *Provider) {
	provider.networkURL = "https://public.example"
	provider.networkLookup = func(context.Context, string) ([]string, error) {
		return []string{"93.184.216.34", "8.8.8.8"}, nil
	}
	provider.networkDial = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}
}

func TestRoutesProbeNetworkGuardsAndAllAddresses(t *testing.T) {
	f := newWBRoutesFixture(t)
	provider := f.intl

	provider.networkURL = "http://public.example"
	info := provider.probeNetwork(context.Background(), realmIntl)
	if info["ok"] != false || !strings.Contains(wbRouteText(info["error"]), "https") {
		t.Fatalf("http network probe = %#v", info)
	}

	provider.networkURL = "https://public.example"
	provider.networkLookup = func(context.Context, string) ([]string, error) {
		return []string{"93.184.216.34", "127.0.0.1"}, nil
	}
	provider.networkDial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("mixed-public resolution was dialed")
		return nil, nil
	}
	info = provider.probeNetwork(context.Background(), realmIntl)
	if info["ok"] != false || !strings.Contains(wbRouteText(info["error"]), "非公网") {
		t.Fatalf("private network probe = %#v", info)
	}

	dialed := []string{}
	provider.networkLookup = func(context.Context, string) ([]string, error) {
		return []string{"93.184.216.34", "8.8.8.8"}, nil
	}
	provider.networkDial = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		if strings.HasPrefix(address, "93.184.216.34:") {
			return nil, errors.New("first public address failed")
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}
	info = provider.probeNetwork(context.Background(), realmIntl)
	if info["ok"] != true || wbRouteText(info["ip"]) != "8.8.8.8" {
		t.Fatalf("all-address network probe = %#v", info)
	}
	if len(dialed) != 2 || !strings.HasPrefix(dialed[0], "93.184.216.34:") || !strings.HasPrefix(dialed[1], "8.8.8.8:") {
		t.Fatalf("dialed addresses = %#v, want both public addresses in order", dialed)
	}
	if addresses := info["addresses"].([]string); len(addresses) != 2 {
		t.Fatalf("resolved addresses = %#v", addresses)
	}
}

func TestRoutesLegacyUnqualifiedAdminActionsSpanRealms(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "uid-intl", realmIntl, nil)
	wbRouteAddAccount(t, f.cn, "uid-cn", realmCN, nil)
	tokens := map[string]string{
		"uid-intl": wbRouteToken(t, "uid-intl", realmIntl),
		"uid-cn":   wbRouteToken(t, "uid-cn", realmCN),
	}
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		uid := r.Header.Get("X-User-Id")
		switch r.URL.Path {
		case creditsSummaryPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"IsPaidUser": true,
				"Packages": []any{map[string]any{
					"PackageCode": "pkg", "ProductName": "Plan",
					"CycleTotalCapacity": 10, "CycleRemainCapacity": 7,
					"CycleUsedCapacity": 3,
				}},
			}}
		case tokenRefreshPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"accessToken": tokens[uid], "refreshToken": "rotated-" + uid,
				"expiresAt": 4102444800000,
			}}
		default:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
		}
	}

	assertUIDs := func(label string, value any, want ...string) {
		t.Helper()
		rows := wbRouteObjects(t, value)
		got := make([]string, 0, len(rows))
		for _, row := range rows {
			got = append(got, wbRouteText(row["uid"]))
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s uids = %v, want %v", label, got, want)
		}
	}

	// v1 dashboard payloads: credit fill and the full sweep carry no realm.
	rec := f.do(http.MethodGet, "/accounts/credits", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("credits status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	assertUIDs("unqualified credits", body["results"], "uid-intl", "uid-cn")
	if accounts := wbRouteObjects(t, body["accounts"]); len(accounts) != 2 {
		t.Fatalf("unqualified credits accounts = %#v, want both realms", accounts)
	}
	rec = f.do(http.MethodPost, "/accounts/credits", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("credits post status=%d body=%s", rec.Code, rec.Body)
	}
	assertUIDs("unqualified credit sweep", decodeWBRoutesJSON(t, rec)["results"], "uid-intl", "uid-cn")

	// The fake upstream returns no package rows; seed a snapshot on each realm
	// so grants/detail exercise the cross-realm lookup rather than upstream
	// package parsing.
	for _, item := range []struct {
		provider *Provider
		uid      string
	}{{f.intl, "uid-intl"}, {f.cn, "uid-cn"}} {
		if err := item.provider.Pool().SetCredits(item.uid, &CreditSnapshot{
			Remain: 7, Used: 3, Size: 10,
			Packages: []CreditPackage{{
				Name: "Plan", PackageCode: "pkg", ProductName: "Plan",
				Remain: 7, Used: 3, Size: 10, Unit: "credit", Status: 3,
			}},
		}); err != nil {
			t.Fatalf("seed credits %s: %v", item.uid, err)
		}
	}

	// Grants are snapshot-only and must include both realms without ?realm.
	rec = f.do(http.MethodGet, "/accounts/credits/grants", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("grants status=%d body=%s", rec.Code, rec.Body)
	}
	grants := decodeWBRoutesJSON(t, rec)
	assertUIDs("unqualified grants", grants["rows"], "uid-intl", "uid-cn")

	// Detail resolves a CN uid even though the legacy route belongs to intl.
	rec = f.do(http.MethodGet, "/accounts/credits/detail?uid=uid-cn", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body)
	}
	detail := decodeWBRoutesJSON(t, rec)
	if detail["ok"] != true || wbRouteText(detail["uid"]) != "uid-cn" || wbRouteText(detail["realm"]) != realmCN {
		t.Fatalf("unqualified detail = %#v", detail)
	}
	rec = f.do(http.MethodGet, "/accounts/credits/detail?realm=intl&uid=uid-cn", nil)
	detail = decodeWBRoutesJSON(t, rec)
	if detail["ok"] != true || wbRouteText(detail["realm"]) != realmCN {
		t.Fatalf("cross-realm explicit detail = %#v", detail)
	}
	rec = f.do(http.MethodPost, "/accounts/credits", map[string]any{"realm": "intl", "uid": "uid-cn"})
	if rec.Code != http.StatusOK {
		t.Fatalf("cross-realm credit uid status=%d body=%s", rec.Code, rec.Body)
	}
	assertUIDs("cross-realm credit uid", decodeWBRoutesJSON(t, rec)["results"], "uid-cn")

	// Refresh without a uid refreshes both realms' accounts.
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.tokenBaseURL = f.server.URL
	}
	rec = f.do(http.MethodPost, "/accounts/refresh", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", rec.Code, rec.Body)
	}
	results := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["results"])
	if len(results) != 2 {
		t.Fatalf("unqualified refresh results = %#v, want both realms", results)
	}
	for _, row := range results {
		if row["ok"] != true {
			t.Fatalf("unqualified refresh row = %#v", row)
		}
	}

	// set-all without realm (and without group) is whole-pool in v1.
	rec = f.do(http.MethodPost, "/accounts/set-all", map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-all status=%d body=%s", rec.Code, rec.Body)
	}
	if accounts := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"]); len(accounts) != 2 {
		t.Fatalf("set-all accounts = %#v, want both realms", accounts)
	}
	for _, provider := range []*Provider{f.intl, f.cn} {
		for _, account := range provider.Pool().Accounts() {
			if account.View().Enabled {
				t.Fatalf("set-all left %s enabled", account.UID())
			}
		}
	}

	// GET /accounts follows the persisted active exit when no realm is named.
	if err := f.intl.SetActiveRealm(context.Background(), realmCN); err != nil {
		t.Fatalf("realm switch: %v", err)
	}
	rec = f.do(http.MethodGet, "/accounts", nil)
	rows := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	if len(rows) != 1 || wbRouteText(rows[0]["uid"]) != "uid-cn" {
		t.Fatalf("active-realm accounts = %#v, want cn only", rows)
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
	for _, key := range []string{"mode", "mode_cn", "mode_intl", "last_run_time", "next_run_time", "windows", "quota_scans", "daily_chat", "logs"} {
		if _, exists := status[key]; !exists {
			t.Fatalf("scheduler missing %q: %#v", key, status)
		}
	}
	if _, ok := status["daily_chat"].(map[string]any); !ok {
		t.Fatalf("scheduler daily_chat = %#v", status["daily_chat"])
	}
	f.intl.schedulerRunMu.Lock()
	rec = f.do(http.MethodPost, "/scheduler/trigger", map[string]any{"job": "workbuddy-checkin"})
	f.intl.schedulerRunMu.Unlock()
	if rec.Code != http.StatusOK {
		t.Fatalf("busy scheduler trigger status=%d body=%s", rec.Code, rec.Body)
	}
	busy := decodeWBRoutesJSON(t, rec)
	if busy["ok"] != false || !strings.Contains(wbRouteText(busy["msg"]), "已有巡检") {
		t.Fatalf("busy scheduler trigger = %#v", busy)
	}
	assertWBNoSecret(t, "scheduler", rec.Body.String(), "refresh-uid-intl")

	if _, err := f.store.Merge(map[string]any{
		"schedule":       map[string]any{"checkin": []any{10, 22}, "daily_chat": []any{"08:30", "20:30"}},
		"daily_activity": true,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	settings, err := f.intl.SettingsView(context.Background())
	if err != nil {
		t.Fatalf("settings view: %v", err)
	}
	schedule, _ := settings["schedule"].(map[string]any)
	if len(scheduleHoursAny(schedule["checkin"])) != 2 {
		t.Fatalf("settings schedule = %#v", schedule)
	}
	settingsJSON, _ := json.Marshal(settings)
	assertWBNoSecret(t, "settings", string(settingsJSON), "refresh-uid-intl", "accessToken", "refreshToken")

	if err := f.intl.SaveSettings(context.Background(), map[string]any{
		"schedule": map[string]any{"checkin": []any{7, 19}},
	}); err != nil {
		t.Fatalf("settings save: %v", err)
	}
	if got := f.intl.scheduleHours("checkin", []int{9, 21}); len(got) != 2 || got[0] != 7 || got[1] != 19 {
		t.Fatalf("saved schedule = %v", got)
	}
	if err := f.intl.SaveSettings(context.Background(), map[string]any{
		"workbuddy": map[string]any{"intl": map[string]any{"timeout_seconds": 5, "max_concurrent": 3}},
	}); err != nil {
		t.Fatalf("workbuddy settings save: %v", err)
	}
	settings, err = f.intl.SettingsView(context.Background())
	if err != nil {
		t.Fatalf("settings view: %v", err)
	}
	workbuddy := settings["workbuddy"].(map[string]any)
	intlConfig := workbuddy["intl"].(map[string]any)
	if intlConfig["timeout_seconds"].(float64) != 5 ||
		intlConfig["max_concurrent"].(int) != 3 {
		t.Fatalf("saved workbuddy config = %#v", intlConfig)
	}
	if err := f.intl.SaveSettings(context.Background(), map[string]any{
		"auto_switch_product": true,
		"upstream":            map[string]any{"header_timeout_seconds": 42},
	}); err != nil {
		t.Fatalf("global settings save: %v", err)
	}
	settings, err = f.intl.SettingsView(context.Background())
	if err != nil {
		t.Fatalf("settings view: %v", err)
	}
	upstream, _ := settings["upstream"].(map[string]any)
	if settings["auto_switch_product"] != true || upstream["header_timeout_seconds"] != float64(42) {
		t.Fatalf("global settings = %#v", settings)
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
	if got := f.fake.count(http.MethodPost, tasksCheckinPath); got != 0 {
		t.Fatalf("API-key add ran %d check-in calls, want 0", got)
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
	secondDir := filepath.Join(f.dir, "desktop-auth-2")
	encryptedDir := filepath.Join(f.dir, "desktop-encrypted")
	symlinkDir := filepath.Join(f.dir, "desktop-symlink")
	outsideDir := filepath.Join(f.dir, "outside")
	for _, dir := range []string{desktopDir, secondDir, encryptedDir, symlinkDir, outsideDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(t, filepath.Join(desktopDir, "workbuddy-desktop-ai.info"), map[string]any{
		"auth": map[string]any{"accessToken": wbRouteToken(t, "uid-desktop", realmIntl)},
		"account": map[string]any{
			"uid": "uid-desktop", "nickname": "Desktop User",
		},
	})
	// Filename hints CN, the token says intl: the scan must report realmHint.
	writeJSON(t, filepath.Join(secondDir, "workbuddy-desktop.info"), map[string]any{
		"auth":    map[string]any{"accessToken": wbRouteToken(t, "uid-desktop-2", realmIntl)},
		"account": map[string]any{"uid": "uid-desktop-2", "nickname": "Second Desk"},
	})
	writeJSON(t, filepath.Join(encryptedDir, "workbuddy-desktop-ai.info"), map[string]any{
		"auth":    map[string]any{"accessToken": map[string]any{"$wbEncrypted": 1, "envelope": "ciphertext"}},
		"account": map[string]any{"uid": "uid-encrypted"},
	})
	writeJSON(t, filepath.Join(outsideDir, "workbuddy-desktop-ai.info"), map[string]any{
		"auth":    map[string]any{"accessToken": wbRouteToken(t, "uid-symlink", realmIntl)},
		"account": map[string]any{"uid": "uid-symlink"},
	})
	if err := os.Symlink(filepath.Join(outsideDir, "workbuddy-desktop-ai.info"), filepath.Join(symlinkDir, "workbuddy-desktop-ai.info")); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.desktopDirs = []string{desktopDir, secondDir, encryptedDir, symlinkDir}
	}

	rec := f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", rec.Code, rec.Body)
	}
	scan := decodeWBRoutesJSON(t, rec)
	detected := wbRouteObjects(t, scan["detected"])
	if len(detected) != 3 {
		t.Fatalf("desktop scan = %#v", detected)
	}
	byFile := map[string]map[string]any{}
	for _, entry := range detected {
		byFile[wbRouteText(entry["path"])] = entry
	}
	first := byFile[filepath.Join(desktopDir, "workbuddy-desktop-ai.info")]
	if first == nil || first["realmName"] != "国际版 (Global)" || first["expiresIn"] == nil {
		t.Fatalf("desktop metadata = %#v", first)
	}
	second := byFile[filepath.Join(secondDir, "workbuddy-desktop.info")]
	if second == nil || wbRouteText(second["realm"]) != realmIntl || wbRouteText(second["realmHint"]) != realmCN {
		t.Fatalf("desktop realm hint = %#v", second)
	}
	encrypted := byFile[filepath.Join(encryptedDir, "workbuddy-desktop-ai.info")]
	if encrypted == nil || encrypted["needsKey"] != true || wbRouteText(encrypted["recoveryHint"]) == "" {
		t.Fatalf("encrypted desktop metadata = %#v", encrypted)
	}
	assertWBNoSecret(t, "desktop scan", rec.Body.String(), "ciphertext")

	rec = f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{"path": filepath.Join(outsideDir, "workbuddy-desktop-ai.info")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("outside desktop path status=%d body=%s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{"path": filepath.Join(symlinkDir, "workbuddy-desktop-ai.info")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("symlink desktop path status=%d body=%s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{"path": f.dir + "/not-a-credential.json"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad desktop path status=%d body=%s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{"all": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("desktop all status=%d body=%s", rec.Code, rec.Body)
	}
	imported := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["imported"])
	if len(imported) != 2 {
		t.Fatalf("desktop imported = %#v", imported)
	}
	if f.intl.Pool().Get("uid-desktop") == nil || f.intl.Pool().Get("uid-desktop-2") == nil {
		t.Fatal("desktop import did not sync the pool")
	}
	if f.intl.Pool().Get("uid-symlink") != nil || f.intl.Pool().Get("uid-encrypted") != nil {
		t.Fatal("desktop import accepted a symlink/encrypted credential")
	}
}

func TestRoutesDesktopBulkImportFiltersRealmBeforeWriteAndChecksIn(t *testing.T) {
	f := newWBRoutesFixture(t)
	intlDir := filepath.Join(f.dir, "desktop-intl")
	cnDir := filepath.Join(f.dir, "desktop-cn")
	for _, dir := range []string{intlDir, cnDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(t, filepath.Join(intlDir, "workbuddy-desktop-ai.info"), map[string]any{
		"auth":    map[string]any{"accessToken": wbRouteToken(t, "uid-desk-intl", realmIntl)},
		"account": map[string]any{"uid": "uid-desk-intl", "nickname": "Intl Desk"},
	})
	writeJSON(t, filepath.Join(cnDir, "workbuddy-desktop.info"), map[string]any{
		"auth":    map[string]any{"accessToken": wbRouteToken(t, "uid-desk-cn", realmCN)},
		"account": map[string]any{"uid": "uid-desk-cn", "nickname": "CN Desk"},
	})
	for _, provider := range []*Provider{f.intl, f.cn} {
		provider.desktopDirs = []string{intlDir, cnDir}
	}

	rec := f.do(http.MethodPost, "/accounts/import/desktop", map[string]any{
		"all": true, "realm": realmCN,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("desktop import status=%d body=%s", rec.Code, rec.Body)
	}
	imported := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["imported"])
	if len(imported) != 1 || wbRouteText(imported[0]["uid"]) != "uid-desk-cn" {
		t.Fatalf("realm-filtered desktop import = %#v, want cn only", imported)
	}
	intlPath, err := AccountPath(f.dir, "uid-desk-intl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(intlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intl candidate was written before the realm filter: %v", err)
	}
	if f.intl.Pool().Get("uid-desk-intl") != nil {
		t.Fatal("intl candidate entered the pool")
	}
	if f.cn.Pool().Get("uid-desk-cn") == nil {
		t.Fatal("cn candidate was not imported")
	}
	if got := f.fake.count(http.MethodPost, tasksCheckinPath); got != 1 {
		t.Fatalf("desktop import initial check-in calls = %d, want 1", got)
	}
	cnAccount := f.cn.Pool().Get("uid-desk-cn")
	cnAccount.mu.RLock()
	lastCheckin := cnAccount.Auth.LastCheckin
	cnAccount.mu.RUnlock()
	if !strings.HasPrefix(lastCheckin, time.Now().Format("2006-01-02")) {
		t.Fatalf("desktop import lastCheckin = %q", lastCheckin)
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

func scheduleHoursAny(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []int:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = item
		}
		return out
	default:
		return nil
	}
}

func TestRouteOwnershipMigration(t *testing.T) {
	f := newWBRoutesFixture(t)
	for _, route := range f.intl.Routes() {
		switch route.Path {
		case "/settings", "/settings/save", "/realm":
			t.Fatalf("WorkBuddy still owns migrated route %s %s", route.Method, route.Path)
		}
	}
	if view, err := f.intl.SettingsView(context.Background()); err != nil || view["workbuddy"] == nil {
		t.Fatalf("SettingsView = %#v err=%v", view, err)
	}
	if err := f.intl.SaveSettings(context.Background(), map[string]any{
		"daily_chat_web": true,
	}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got := f.intl.ActiveRealm(context.Background()); got != realmIntl {
		t.Fatalf("ActiveRealm = %q", got)
	}
	if err := f.intl.SetActiveRealm(context.Background(), realmCN); err != nil || f.intl.ActiveRealm(context.Background()) != realmCN {
		t.Fatalf("SetActiveRealm = %v", err)
	}
	if options := f.intl.RealmOptions(); len(options) != 2 {
		t.Fatalf("RealmOptions = %#v", options)
	}
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

func TestRoutesCreditsAndCheckinReturnWholePoolV1Shape(t *testing.T) {
	f := newWBRoutesFixture(t)
	intl := wbRouteAddAccount(t, f.intl, "oracle-intl", realmIntl, nil)
	cn := wbRouteAddAccount(t, f.cn, "oracle-cn", realmCN, nil)
	cn.SetCredits(&CreditSnapshot{
		Remain: 60, Used: 40, Size: 100, UsedPercent: "40.0%", RemainPercent: "60.0%",
		Checkin:  &CheckinStatus{TodayCheckedIn: false, StreakDays: 2, DailyCredit: 5, Active: true},
		Packages: []CreditPackage{},
	})
	intl.SetCredits(&CreditSnapshot{
		Remain: 60, Used: 40, Size: 100, UsedPercent: "40.0%", RemainPercent: "60.0%",
		Packages: []CreditPackage{},
	})

	rec := f.do(http.MethodPost, "/accounts/credits", map[string]any{"realm": "intl"})
	if rec.Code != http.StatusOK {
		t.Fatalf("credits status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeWBRoutesJSON(t, rec)
	accounts := wbRouteObjects(t, body["accounts"])
	if len(accounts) != 2 || wbRouteText(accounts[0]["uid"]) != "oracle-cn" || wbRouteText(accounts[1]["uid"]) != "oracle-intl" {
		t.Fatalf("credits accounts = %#v, want the whole pool", accounts)
	}
	results := wbRouteObjects(t, body["results"])
	if len(results) != 1 || wbRouteText(results[0]["uid"]) != "oracle-intl" {
		t.Fatalf("credits results = %#v, want only the requested realm", results)
	}
	if _, present := body["busy"]; present {
		t.Fatalf("normal POST credits carried busy: %#v", body)
	}

	rec = f.do(http.MethodPost, "/accounts/checkin", map[string]any{"uid": "oracle-cn"})
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin status=%d body=%s", rec.Code, rec.Body)
	}
	body = decodeWBRoutesJSON(t, rec)
	accounts = wbRouteObjects(t, body["accounts"])
	if len(accounts) != 2 {
		t.Fatalf("checkin accounts = %#v, want the whole pool", accounts)
	}
	var cnView map[string]any
	for _, view := range accounts {
		if wbRouteText(view["uid"]) == "oracle-cn" {
			cnView = view
		}
	}
	if cnView == nil {
		t.Fatalf("checkin accounts missing cn: %#v", accounts)
	}
	credits, _ := cnView["credits"].(map[string]any)
	checkin, _ := credits["checkin"].(map[string]any)
	if checkin == nil || checkin["today_checked_in"] != false {
		t.Fatalf("checkin refreshed the pre-action credit snapshot: %#v", cnView["credits"])
	}
	if wbRouteText(cnView["lastCheckin"]) == "" {
		t.Fatalf("checkin did not update lastCheckin: %#v", cnView)
	}
	results = wbRouteObjects(t, body["results"])
	if len(results) != 1 {
		t.Fatalf("checkin results = %#v", results)
	}
	for _, extra := range []string{"realm", "activity", "checkin", "credits", "skipped", "busy"} {
		if _, present := results[0][extra]; present {
			t.Fatalf("checkin result carried v2-only %q: %#v", extra, results[0])
		}
	}
	for _, key := range []string{"uid", "nickname", "ok", "state", "code", "msg", "data"} {
		if _, present := results[0][key]; !present {
			t.Fatalf("checkin result missing v1 field %q: %#v", key, results[0])
		}
	}
}

func TestAccountViewMatchesV1FingerprintAndNullActivity(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "oracle-intl", realmIntl, nil)
	wbRouteAddAccount(t, f.cn, "oracle-cn", realmCN, nil)

	rec := f.do(http.MethodGet, "/accounts?realm=all", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts status=%d body=%s", rec.Code, rec.Body)
	}
	accounts := wbRouteObjects(t, decodeWBRoutesJSON(t, rec)["accounts"])
	byUID := make(map[string]map[string]any, len(accounts))
	for _, view := range accounts {
		byUID[wbRouteText(view["uid"])] = view
	}
	for uid, want := range map[string][2]string{
		"oracle-cn":   {"ae947ff4-00e1-47b7-a143-f5410a8c8fd9", "528843fd-b012-4fd1-a0ab-58c27b008e64"},
		"oracle-intl": {"3f1925b7-f5ca-457e-afef-bf4a9acd12c2", "f1a31870-3798-40be-a9f8-f99476ae2242"},
	} {
		view := byUID[uid]
		if view == nil {
			t.Fatalf("missing account %s: %#v", uid, accounts)
		}
		if wbRouteText(view["machineId"]) != want[0] || wbRouteText(view["sessionId"]) != want[1] {
			t.Fatalf("%s fingerprint = %q/%q, want %q/%q", uid,
				view["machineId"], view["sessionId"], want[0], want[1])
		}
		for _, key := range []string{"lastCheckin", "lastDailyChat", "lastActivityReport"} {
			if value, present := view[key]; !present || value != nil {
				t.Fatalf("%s %s = %#v (present=%v), want null", uid, key, value, present)
			}
		}
		for _, key := range []string{"activeConcurrent", "maxConcurrent", "reason"} {
			if _, present := view[key]; present {
				t.Fatalf("%s has v1-extra field %q: %#v", uid, key, view)
			}
		}
	}
}

func TestAccountSummariesExposeLivePool(t *testing.T) {
	f := newWBRoutesFixture(t)
	wbRouteAddAccount(t, f.intl, "oracle-intl", realmIntl, nil)
	wbRouteAddAccount(t, f.cn, "oracle-cn", realmCN, nil)

	var rows []providers.AccountSummary
	for _, provider := range []*Provider{f.intl, f.cn} {
		got, err := provider.AccountSummaries(context.Background())
		if err != nil {
			t.Fatalf("AccountSummaries: %v", err)
		}
		rows = append(rows, got...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })
	if len(rows) != 2 || rows[0].UID != "oracle-cn" || rows[1].UID != "oracle-intl" {
		t.Fatalf("summaries = %#v", rows)
	}
	for _, row := range rows {
		if !row.Enabled || !row.Ready {
			t.Fatalf("%s readiness = enabled:%v ready:%v", row.UID, row.Enabled, row.Ready)
		}
		if row.Domain == "" || row.Issuer == "" || row.CredentialFile != row.UID+".json" {
			t.Fatalf("%s summary = %#v", row.UID, row)
		}
	}
}
