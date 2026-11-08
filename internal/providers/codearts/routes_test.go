package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const routesDisabledMessage = "CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。"

type codeartsRoutesFixture struct {
	p     *Provider
	store *config.Store
	dir   string
	mux   *http.ServeMux
}

func newCodeArtsRoutesFixture(t *testing.T, enabled bool) *codeartsRoutesFixture {
	t.Helper()
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if enabled {
		if _, err := SaveConfig(store, map[string]any{"enabled": true}); err != nil {
			t.Fatalf("enable config: %v", err)
		}
	}
	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider.SetListenerPort(54321)
	mux := http.NewServeMux()
	api.RegisterProviderRoutes(mux, provider)
	return &codeartsRoutesFixture{p: provider, store: store, dir: dir, mux: mux}
}

func (f *codeartsRoutesFixture) do(method, path string, payload any, host string) *httptest.ResponseRecorder {
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
	if host != "" {
		request.Host = host
	}
	recorder := httptest.NewRecorder()
	f.mux.ServeHTTP(recorder, request)
	return recorder
}

func decodeRoutesJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(recorder.Body.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return body
}

func routesErrorBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	object, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %#v", body)
	}
	return object
}

func routesText(value any) string {
	text, _ := value.(string)
	return text
}

func routeNumber(value any) float64 {
	number, _ := numericValue(value)
	return number
}

func routeAuthPayload(t *testing.T, auth Auth) map[string]any {
	t.Helper()
	raw, err := json.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal auth payload: %v", err)
	}
	return payload
}

func addRouteAccount(t *testing.T, p *Provider, uid string) *Account {
	t.Helper()
	auth := testAuth(uid)
	auth.Expiration = time.Now().Add(4 * time.Hour)
	account, err := p.Pool().Add(auth)
	if err != nil {
		t.Fatalf("Pool.Add(%s): %v", uid, err)
	}
	return account
}

// adminUpstream combines the Task 7 credits fake and the Task 4 catalog fake
// behind one httptest server so SNAP_HOST, the snap-manager base, and the
// benefit host all resolve to the same local origin.
type adminUpstream struct {
	credits *creditFake
	catalog *catalogUpstream
}

func (u *adminUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/snap-manager/"):
		u.credits.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/ops/"):
		u.credits.ServeHTTP(w, r)
	default:
		u.catalog.ServeHTTP(w, r)
	}
}

// newAdminUpstreams wires the Task 7 credits fake and Task 4 catalog fake onto
// the three CodeArts hosts. The credits endpoint lives under snap-manager while
// model discovery and benefit discovery use the snap/benefit hosts.
func newAdminUpstreams(t *testing.T) (*creditFake, *catalogUpstream) {
	t.Helper()
	credits := newCreditFake()
	catalog := newCatalogUpstream()
	server := httptest.NewServer(&adminUpstream{credits: credits, catalog: catalog})
	t.Cleanup(server.Close)
	t.Setenv("WB_CODEARTS_SNAP_MANAGER", server.URL+"/snap-manager")
	t.Setenv("WB_CODEARTS_SNAP_HOST", server.URL)
	t.Setenv("WB_CODEARTS_BENEFIT_HOST", server.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", server.URL)
	return credits, catalog
}

func assertNoSecrets(t *testing.T, label, raw string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(raw, secret) {
			t.Fatalf("%s leaked %q: %s", label, secret, raw)
		}
	}
}

func TestCodeArtsRoutesPanelMetadata(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	routes := f.p.Routes()
	seen := make(map[string]providers.Route, len(routes))
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if _, exists := seen[key]; exists {
			t.Fatalf("duplicate route %s", key)
		}
		seen[key] = route
		if route.Handler == nil {
			t.Fatalf("route %s has no handler", key)
		}
		if route.Path == "/oauth/callback" {
			if route.Auth != providers.AuthNone {
				t.Fatalf("callback auth = %v, want AuthNone", route.Auth)
			}
			continue
		}
		if route.Auth != providers.AuthPanel {
			t.Fatalf("route %s auth = %v, want AuthPanel", key, route.Auth)
		}
	}

	required := []string{
		"GET /codearts/overview",
		"GET /codearts/accounts",
		"POST /codearts/accounts/import",
		"POST /codearts/accounts/oauth/start",
		"POST /codearts/accounts/oauth/poll",
		"POST /codearts/accounts/oauth/callback",
		"POST /codearts/accounts/enable",
		"POST /codearts/accounts/disable",
		"POST /codearts/accounts/clear-cooldown",
		"POST /codearts/accounts/note",
		"POST /codearts/accounts/tasks",
		"POST /codearts/accounts/test",
		"POST /codearts/accounts/proxy",
		"POST /codearts/accounts/delete",
		"POST /codearts/accounts/checkin",
		"POST /codearts/checkin",
		"POST /codearts/credits/refresh",
		"POST /codearts/keepalive",
		"POST /codearts/models/refresh",
		"POST /codearts/test",
		"GET /oauth/callback",
	}
	for _, key := range required {
		if _, ok := seen[key]; !ok {
			t.Fatalf("required route %s is missing", key)
		}
	}
	if got := f.p.RouteNotFoundMessage(); got != "unknown codearts endpoint" {
		t.Fatalf("RouteNotFoundMessage = %q", got)
	}
	if got := f.p.ID(); got != "codearts" {
		t.Fatalf("ID = %q", got)
	}
	if got := f.p.Realm(); got != "codearts" {
		t.Fatalf("Realm = %q", got)
	}
	if jobs := f.p.Jobs(); jobs != nil {
		t.Fatalf("Jobs = %#v, want nil", jobs)
	}
	if _, ok := any(f.p).(providers.Provider); !ok {
		t.Fatal("Provider does not implement providers.Provider")
	}
}

func TestCodeArtsOverviewNoTokensAndStats(t *testing.T) {
	credits, _ := newAdminUpstreams(t)
	credits.setPlugin("u2", map[string]any{})
	f := newCodeArtsRoutesFixture(t, true)
	first := addRouteAccount(t, f.p, "u1")
	second := addRouteAccount(t, f.p, "u2")
	if err := f.p.Pool().Disable("u2", "manual"); err != nil {
		t.Fatalf("disable u2: %v", err)
	}
	second.mu.Lock()
	second.Models = []Model{{"id": "Kimi-K3", "name": "Kimi-K3"}}
	second.mu.Unlock()

	recorder := f.do(http.MethodGet, "/codearts/overview?force=1", nil, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", recorder.Code, recorder.Body)
	}
	raw := recorder.Body.String()
	assertNoSecrets(t, "overview",
		raw,
		first.Auth.CloudDragonToken, first.Auth.SecretAccessKey,
		first.Auth.AccessKeyID, first.Auth.RefreshToken,
	)
	body := decodeRoutesJSON(t, recorder)
	if body["service"] != "codearts" || body["region"] != "cn" || body["online"] != true {
		t.Fatalf("overview identity = %#v", body)
	}
	if routesText(body["base_url"]) != snapHost() {
		t.Fatalf("base_url = %v, want %s", body["base_url"], snapHost())
	}
	stats, _ := body["stats"].(map[string]any)
	if routeNumber(stats["total"]) != 2 || routeNumber(stats["healthy"]) != 1 ||
		routeNumber(stats["disabled"]) != 1 || routeNumber(stats["cooling"]) != 0 {
		t.Fatalf("stats = %#v", stats)
	}
	if _, ok := stats["limited"]; !ok {
		t.Fatalf("stats has no limited counter: %#v", stats)
	}
	if routeNumber(stats["credits"]) != 4499.19 ||
		routeNumber(stats["credits_total"]) != 4500 ||
		routeNumber(stats["credits_used"]) != 0.81 {
		t.Fatalf("credit stats = %#v", stats)
	}
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v", accounts)
	}
	models, _ := body["models"].([]any)
	modelByID := make(map[string]map[string]any, len(models))
	for _, item := range models {
		entry, _ := item.(map[string]any)
		modelByID[routesText(entry["id"])] = entry
	}
	if entry := modelByID["GLM-5.2"]; entry == nil ||
		entry["name"] != "GLM-5.2" || entry["description"] != "最新旗舰模型" {
		t.Fatalf("info model = %#v (all %#v)", entry, models)
	}
	if entry := modelByID["Kimi-K3"]; entry == nil || entry["name"] != nil {
		t.Fatalf("catalog fallback model = %#v", entry)
	}
	schedule, _ := body["schedule"].(map[string]any)
	watch, _ := schedule["watch"].(map[string]any)
	if watch["enabled"] != false {
		t.Fatalf("schedule = %#v", schedule)
	}
	health, _ := body["health"].(map[string]any)
	if health["ok"] != true || routeNumber(health["accounts"]) != 2 ||
		routeNumber(health["healthy"]) != 1 {
		t.Fatalf("health = %#v", health)
	}
	// v1 refreshes statistics for disabled accounts too, not only the live
	// subset used for scheduling.
	if got := credits.callCountFor("u2", creditPluginPath); got != 1 {
		t.Fatalf("disabled account statistics calls = %d, want 1", got)
	}
}

func TestCodeArtsOverviewDisabledAndHealth(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, false)
	recorder := f.do(http.MethodGet, "/codearts/overview", nil, "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled overview status=%d body=%s", recorder.Code, recorder.Body)
	}
	failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
	if failure["message"] != routesDisabledMessage || failure["type"] != "invalid_request_error" ||
		routeNumber(failure["code"]) != http.StatusServiceUnavailable {
		t.Fatalf("disabled overview error = %#v", failure)
	}
	recorder = f.do(http.MethodGet, "/codearts/accounts", nil, "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled accounts status=%d body=%s", recorder.Code, recorder.Body)
	}

	health, err := f.p.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health["ok"] != false || health["configured"] != false ||
		routeNumber(health["accounts"]) != 0 || routeNumber(health["healthy"]) != 0 {
		t.Fatalf("disabled health = %#v", health)
	}
	if routesText(health["base_url"]) != snapHost() {
		t.Fatalf("health base_url = %#v", health["base_url"])
	}

	if _, err := SaveConfig(f.store, map[string]any{"enabled": true}); err != nil {
		t.Fatalf("enable config: %v", err)
	}
	if _, err := f.p.applySettings(); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	recorder = f.do(http.MethodGet, "/codearts/overview", nil, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("enabled overview status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["online"] != true {
		t.Fatalf("enabled overview online = %#v", body["online"])
	}
	models, _ := body["models"].([]any)
	if len(models) != len(seedModels) {
		t.Fatalf("seed fallback models = %d, want %d", len(models), len(seedModels))
	}
}

func TestCodeArtsDisabledAdminGating(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, false)
	addRouteAccount(t, f.p, "u1")

	getRoutes := []string{"/codearts/overview", "/codearts/accounts"}
	for _, path := range getRoutes {
		recorder := f.do(http.MethodGet, path, nil, "")
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s disabled status=%d body=%s", path, recorder.Code, recorder.Body)
		}
		failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
		if failure["message"] != routesDisabledMessage {
			t.Fatalf("%s disabled error = %#v", path, failure)
		}
	}

	postRoutes := []string{
		"/codearts/test",
		"/codearts/models/refresh",
		"/codearts/keepalive",
		"/codearts/credits/refresh",
		"/codearts/accounts/checkin",
		"/codearts/checkin",
		"/codearts/accounts/enable",
		"/codearts/accounts/disable",
		"/codearts/accounts/clear-cooldown",
		"/codearts/accounts/note",
		"/codearts/accounts/tasks",
		"/codearts/accounts/test",
		"/codearts/accounts/proxy",
		"/codearts/accounts/delete",
	}
	for _, path := range postRoutes {
		recorder := f.do(http.MethodPost, path, map[string]any{}, "")
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s disabled status=%d body=%s", path, recorder.Code, recorder.Body)
		}
		failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
		if failure["message"] != routesDisabledMessage ||
			failure["type"] != "invalid_request_error" ||
			routeNumber(failure["code"]) != http.StatusServiceUnavailable {
			t.Fatalf("%s disabled error = %#v", path, failure)
		}
	}

	// Import is the deliberate bootstrap extension: it can run while the
	// realm is disabled and auto-enables it.
	recorder := f.do(http.MethodPost, "/codearts/accounts/import",
		routeAuthPayload(t, testAuth("u2")), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("bootstrap import status=%d body=%s", recorder.Code, recorder.Body)
	}
	if !f.p.Config().Enabled {
		t.Fatal("bootstrap import did not enable the realm")
	}
	if f.p.Pool().Get("u2") == nil {
		t.Fatal("bootstrap import did not add the account")
	}
}

func TestCodeArtsHealthFiveSecondCacheAndForce(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	setCatalogNow(t, f.p, now)
	addRouteAccount(t, f.p, "u1")

	health, err := f.p.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if routeNumber(health["accounts"]) != 1 {
		t.Fatalf("initial health = %#v", health)
	}

	addRouteAccount(t, f.p, "u2")
	health, err = f.p.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("cached Health: %v", err)
	}
	if routeNumber(health["accounts"]) != 1 {
		t.Fatalf("health cache was not used: %#v", health)
	}
	health, err = f.p.Health(context.Background(), true)
	if err != nil {
		t.Fatalf("forced Health: %v", err)
	}
	if routeNumber(health["accounts"]) != 2 {
		t.Fatalf("forced health = %#v", health)
	}

	now = now.Add(6 * time.Second)
	health, err = f.p.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("expired-cache Health: %v", err)
	}
	if routeNumber(health["accounts"]) != 2 {
		t.Fatalf("expired health cache = %#v", health)
	}
}

func TestCodeArtsHealthCacheInvalidatedByAdminRoutes(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	addRouteAccount(t, f.p, "u1")

	health, err := f.p.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if routeNumber(health["accounts"]) != 1 {
		t.Fatalf("initial health = %#v", health)
	}

	recorder := f.do(http.MethodPost, "/codearts/accounts/import",
		routeAuthPayload(t, testAuth("u2")), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", recorder.Code, recorder.Body)
	}
	health, err = f.p.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("Health after import: %v", err)
	}
	if routeNumber(health["accounts"]) != 2 {
		t.Fatalf("import did not invalidate the health cache: %#v", health)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/disable",
		map[string]any{"uid": "u2"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", recorder.Code, recorder.Body)
	}
	health, err = f.p.Health(context.Background(), false)
	if err != nil {
		t.Fatalf("Health after disable: %v", err)
	}
	if routeNumber(health["healthy"]) != 1 || routeNumber(health["accounts"]) != 2 {
		t.Fatalf("disable did not invalidate the health cache: %#v", health)
	}
}

func TestCodeArtsImportFlatNestedAndCollision(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, false)
	flat := routeAuthPayload(t, testAuth("u1"))
	recorder := f.do(http.MethodPost, "/codearts/accounts/import", flat, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("flat import status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["ok"] != true || body["enabled"] != true || body["uid"] != "u1" {
		t.Fatalf("flat import = %#v", body)
	}
	if f.p.Pool().Get("u1") == nil {
		t.Fatal("flat import did not add the account")
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	codeartsConfig, _ := settings["codearts"].(map[string]any)
	if codeartsConfig["enabled"] != true {
		t.Fatalf("import did not auto-enable: %#v", settings)
	}

	nested := map[string]any{"auth": routeAuthPayload(t, testAuth("u2"))}
	recorder = f.do(http.MethodPost, "/codearts/accounts/import", nested, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("nested import status=%d body=%s", recorder.Code, recorder.Body)
	}
	body = decodeRoutesJSON(t, recorder)
	if body["uid"] != "u2" || body["enabled"] != true {
		t.Fatalf("nested import = %#v", body)
	}
	if len(f.p.Pool().Accounts()) != 2 {
		t.Fatalf("pool size = %d", len(f.p.Pool().Accounts()))
	}

	// The sanitized path codearts-a_b.json already belongs to a_b. Importing
	// a/b must conflict instead of overwriting that credential file.
	writeTestAuth(t, f.dir, "a_b", "")
	conflict := routeAuthPayload(t, testAuth("a/b"))
	recorder = f.do(http.MethodPost, "/codearts/accounts/import", conflict, "")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("collision status=%d body=%s", recorder.Code, recorder.Body)
	}
	failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
	if !strings.Contains(routesText(failure["message"]), "belongs to a different account") {
		t.Fatalf("collision error = %#v", failure)
	}
	path, err := AuthPath(f.dir, "a_b")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := LoadAuth(path)
	if err != nil {
		t.Fatalf("load existing auth: %v", err)
	}
	if stored.UserID != "a_b" {
		t.Fatalf("collision overwrote auth file: %+v", stored)
	}
}

func TestCodeArtsAccountActionsAndDelete(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	account := addRouteAccount(t, f.p, "u1")

	recorder := f.do(http.MethodPost, "/codearts/accounts/disable", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", recorder.Code, recorder.Body)
	}
	if body := decodeRoutesJSON(t, recorder); body["message"] != "已停用 u1" {
		t.Fatalf("disable = %#v", body)
	}
	if view := account.View(); !view.Disabled || !view.ManualDisabled {
		t.Fatalf("account not disabled: %#v", view)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/enable", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", recorder.Code, recorder.Body)
	}
	if body := decodeRoutesJSON(t, recorder); body["message"] != "已启用 u1" {
		t.Fatalf("enable = %#v", body)
	}

	account.mu.Lock()
	account.State.CoolUntil = unixSeconds(time.Now().Add(time.Hour))
	account.State.ErrCount = 2
	account.mu.Unlock()
	recorder = f.do(http.MethodPost, "/codearts/accounts/clear-cooldown", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear-cooldown status=%d body=%s", recorder.Code, recorder.Body)
	}
	if body := decodeRoutesJSON(t, recorder); body["message"] != "已清冷却 u1" {
		t.Fatalf("clear-cooldown = %#v", body)
	}
	account.mu.Lock()
	coolUntil, errCount := account.State.CoolUntil, account.State.ErrCount
	account.mu.Unlock()
	if coolUntil != 0 || errCount != 0 {
		t.Fatalf("cooldown not cleared: %v/%d", coolUntil, errCount)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/note",
		map[string]any{"uid": "u1", "note": "  hello  "}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("note status=%d body=%s", recorder.Code, recorder.Body)
	}
	if body := decodeRoutesJSON(t, recorder); body["note"] != "hello" {
		t.Fatalf("note = %#v", body)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/tasks", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("tasks status=%d body=%s", recorder.Code, recorder.Body)
	}
	if body := decodeRoutesJSON(t, recorder); body["tasks_enabled"] != true {
		t.Fatalf("tasks default = %#v", body)
	}
	recorder = f.do(http.MethodPost, "/codearts/accounts/tasks",
		map[string]any{"uid": "u1", "enabled": false}, "")
	if body := decodeRoutesJSON(t, recorder); body["tasks_enabled"] != false {
		t.Fatalf("tasks false = %#v", body)
	}

	if _, err := f.store.Merge(map[string]any{"proxy_slots": []any{
		map[string]any{"id": "slot-1", "name": "one", "url": "http://127.0.0.1:7890", "enabled": true},
	}}); err != nil {
		t.Fatalf("save proxy slot: %v", err)
	}
	recorder = f.do(http.MethodPost, "/codearts/accounts/proxy",
		map[string]any{"uid": "u1", "slot_id": "slot-1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["proxy_slot"] != "slot-1" || body["proxy"] != "http://127.0.0.1:7890" ||
		body["message"] != "出口已更新" {
		t.Fatalf("proxy = %#v", body)
	}
	path, err := AuthPath(f.dir, "u1")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatalf("load auth: %v", err)
	}
	if auth.ProxySlot != "slot-1" {
		t.Fatalf("proxy slot not persisted: %+v", auth)
	}

	for _, action := range []string{"enable", "disable", "clear-cooldown", "note", "tasks", "proxy", "delete"} {
		recorder = f.do(http.MethodPost, "/codearts/accounts/"+action, map[string]any{}, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s missing uid status=%d body=%s", action, recorder.Code, recorder.Body)
		}
		failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
		if failure["message"] != "uid required" || routeNumber(failure["code"]) != http.StatusBadRequest {
			t.Fatalf("%s missing uid error = %#v", action, failure)
		}
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/enable", map[string]any{"uid": "nope"}, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown enable status=%d body=%s", recorder.Code, recorder.Body)
	}
	failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
	if failure["message"] != "account not found" || routeNumber(failure["code"]) != http.StatusNotFound {
		t.Fatalf("unknown enable error = %#v", failure)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/delete", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", recorder.Code, recorder.Body)
	}
	if body := decodeRoutesJSON(t, recorder); body["deleted"] != true || body["message"] != "已删除 u1" {
		t.Fatalf("delete = %#v", body)
	}
	if f.p.Pool().Get("u1") != nil {
		t.Fatal("delete left the account in the pool")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete left the auth file: %v", err)
	}
	state, err := LoadState(filepath.Join(f.dir, "codearts", "state.json"))
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, exists := state["u1"]; exists {
		t.Fatalf("delete left state entry: %#v", state)
	}
	recorder = f.do(http.MethodPost, "/codearts/accounts/delete", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d body=%s", recorder.Code, recorder.Body)
	}
}

func TestCodeArtsAccountTestRoute(t *testing.T) {
	credits, _ := newAdminUpstreams(t)
	f := newCodeArtsRoutesFixture(t, true)
	addRouteAccount(t, f.p, "u1")

	recorder := f.do(http.MethodPost, "/codearts/accounts/test", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("test status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["ok"] != true || body["uid"] != "u1" || body["message"] != "连接正常" {
		t.Fatalf("test = %#v", body)
	}
	if routeNumber(body["models"]) < 1 || routeNumber(body["credits"]) != 4499.19 {
		t.Fatalf("test models/credits = %#v", body)
	}

	credits.failPlugin("u1", http.StatusInternalServerError, `{"message":"statistics down"}`)
	recorder = f.do(http.MethodPost, "/codearts/accounts/test", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("failed test status=%d body=%s", recorder.Code, recorder.Body)
	}
	body = decodeRoutesJSON(t, recorder)
	if body["ok"] != false || body["uid"] != "u1" || routesText(body["message"]) == "" {
		t.Fatalf("failed test = %#v", body)
	}
	if _, hasError := body["error"]; hasError {
		t.Fatalf("account test used an error envelope: %#v", body)
	}

	if recorder := f.do(http.MethodPost, "/codearts/accounts/test", map[string]any{}, ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("test missing uid status=%d", recorder.Code)
	}
	if recorder := f.do(http.MethodPost, "/codearts/accounts/test", map[string]any{"uid": "nope"}, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("test unknown uid status=%d body=%s", recorder.Code, recorder.Body)
	}
}

func TestCodeArtsCreditsAndCheckinRoutes(t *testing.T) {
	credits, _ := newAdminUpstreams(t)
	f := newCodeArtsRoutesFixture(t, true)
	addRouteAccount(t, f.p, "u1")
	campaign := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "ELIGIBLE", nil)
	campaign["pendingTotalAmount"] = 1000
	credits.setDelivery("u1", []any{campaign})

	recorder := f.do(http.MethodPost, "/codearts/credits/refresh", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("credits status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["ok"] != true || routeNumber(body["refreshed"]) != 1 {
		t.Fatalf("credits refresh = %#v", body)
	}
	rows, _ := body["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("credits rows = %#v", body)
	}
	row, _ := rows[0].(map[string]any)
	rowCredits, _ := row["credits"].(map[string]any)
	if row["uid"] != "u1" || routeNumber(rowCredits["remain"]) != 4499.19 {
		t.Fatalf("credits row = %#v", row)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/checkin", map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("checkin status=%d body=%s", recorder.Code, recorder.Body)
	}
	body = decodeRoutesJSON(t, recorder)
	if body["ok"] != true || routeNumber(body["credits"]) != 1000 {
		t.Fatalf("checkin = %#v", body)
	}
	campaignRows, _ := body["campaigns"].([]any)
	if len(campaignRows) != 1 {
		t.Fatalf("checkin campaigns = %#v", body)
	}

	recorder = f.do(http.MethodPost, "/codearts/checkin", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("all checkin status=%d body=%s", recorder.Code, recorder.Body)
	}
	body = decodeRoutesJSON(t, recorder)
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("all checkin = %#v", body)
	}
	if _, hasCampaigns := body["campaigns"]; hasCampaigns {
		t.Fatalf("batch checkin leaked campaigns: %#v", body)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/checkin", map[string]any{"uid": "nope"}, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("checkin unknown status=%d body=%s", recorder.Code, recorder.Body)
	}
	failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
	if failure["message"] != "account not found" {
		t.Fatalf("checkin unknown error = %#v", failure)
	}
}

func TestCodeArtsModelsRefreshAndConnectionTest(t *testing.T) {
	_, _ = newAdminUpstreams(t)
	f := newCodeArtsRoutesFixture(t, true)
	account := addRouteAccount(t, f.p, "u1")
	account.mu.Lock()
	account.Models = []Model{{"id": "Stale-Model"}}
	account.modelsFetchedAt = time.Now()
	account.mu.Unlock()

	recorder := f.do(http.MethodPost, "/codearts/models/refresh", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("models refresh status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["ok"] != true || routeNumber(body["models"]) < 1 {
		t.Fatalf("models refresh = %#v", body)
	}
	if _, hasMessage := body["message"]; hasMessage {
		t.Fatalf("models refresh has a v2-only message field: %#v", body)
	}
	catalog, _ := body["catalog"].(map[string]any)
	if catalog["source"] != "codearts" || routeNumber(catalog["accounts"]) != 1 || catalog["stale"] != false {
		t.Fatalf("models catalog = %#v", catalog)
	}
	if catalog["fetched_at"] == nil {
		t.Fatalf("models catalog fetched_at is nil: %#v", catalog)
	}
	account.mu.Lock()
	models := cloneModels(account.Models)
	account.mu.Unlock()
	foundLive := false
	for _, model := range models {
		id := routesText(model["id"])
		if id == "Stale-Model" {
			t.Fatalf("stale model survived refresh: %#v", models)
		}
		if id == "GLM-5.2" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("live catalog was not adopted: %#v", models)
	}

	recorder = f.do(http.MethodPost, "/codearts/test", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("connection test status=%d body=%s", recorder.Code, recorder.Body)
	}
	body = decodeRoutesJSON(t, recorder)
	if body["ok"] != true || routeNumber(body["models"]) < 1 {
		t.Fatalf("connection test = %#v", body)
	}
	stats, _ := body["stats"].(map[string]any)
	if routeNumber(stats["total"]) != 1 {
		t.Fatalf("connection test stats = %#v", stats)
	}
	health, _ := body["health"].(map[string]any)
	if health["ok"] != true {
		t.Fatalf("connection test health = %#v", health)
	}
}

func TestCodeArtsKeepaliveRoute(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	addRouteAccount(t, f.p, "u1")
	recorder := f.do(http.MethodPost, "/codearts/keepalive", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("keepalive status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["ok"] != true || routesText(body["message"]) == "" {
		t.Fatalf("keepalive = %#v", body)
	}
	if _, ok := body["refreshed"]; !ok {
		t.Fatalf("keepalive has no refreshed count: %#v", body)
	}
}

func TestCodeArtsUnknownRouteAndErrorEnvelopes(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	recorder := f.do(http.MethodGet, "/codearts/nope", nil, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown route status=%d body=%s", recorder.Code, recorder.Body)
	}
	failure := routesErrorBody(t, decodeRoutesJSON(t, recorder))
	if failure["message"] != "unknown codearts endpoint" || routeNumber(failure["code"]) != http.StatusNotFound {
		t.Fatalf("unknown route error = %#v", failure)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("unknown route content type = %q", contentType)
	}

	cases := []struct {
		name   string
		err    error
		status int
		kind   string
	}{
		{"not found", ErrAccountNotFound, http.StatusNotFound, "invalid_request_error"},
		{"collision", ErrAuthPathCollision, http.StatusBadGateway, "invalid_request_error"},
		{"rate limit", &APIError{Kind: KindRateLimited, Message: "limited"}, http.StatusBadGateway, "invalid_request_error"},
		{"unavailable", &APIError{Kind: KindUnavailable, Message: "offline"}, http.StatusBadGateway, "invalid_request_error"},
		{"credit exhausted", &APIError{Kind: KindCreditExhausted, Message: "empty"}, http.StatusBadGateway, "invalid_request_error"},
		{"bad request", &APIError{Kind: KindBadRequest, Message: "bad"}, http.StatusBadRequest, "invalid_request_error"},
		{"oauth replay", ErrOAuthReplay, http.StatusBadGateway, "invalid_request_error"},
		{"plain", errors.New("boom"), http.StatusBadGateway, "invalid_request_error"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			failure := adminErrorFrom(testCase.err)
			if failure.Status != testCase.status || failure.Type != testCase.kind {
				t.Fatalf("adminErrorFrom(%v) = %#v", testCase.err, failure)
			}
			if strings.TrimSpace(failure.Message) == "" {
				t.Fatalf("empty error message for %v", testCase.err)
			}
		})
	}
}

func TestCodeArtsOAuthStartPanelCallbackAndBrowserCallback(t *testing.T) {
	fake := &oauthFake{}
	const (
		securitySentinel = "sentinel-security-token-0123456789"
		accessSentinel   = "sentinel-access-key-0123456789"
		secretSentinel   = "sentinel-secret-key-0123456789"
		refreshSentinel  = "sentinel-refresh-token-0123456789"
	)
	fake.tokenFn = func() map[string]any {
		payload := defaultTokenPayload()
		payload["refresh_token"] = refreshSentinel
		payload["credentials"] = map[string]any{
			"access_key_id":     accessSentinel,
			"secret_access_key": secretSentinel,
			"security_token":    securitySentinel,
			"expiration":        "2030-01-02T03:04:05Z",
		}
		return payload
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("WB_CODEARTS_STS_HOST", server.URL)
	t.Setenv("WB_CODEARTS_SNAP_MANAGER", server.URL+"/snap-manager")
	t.Setenv("WB_CODEARTS_PORTAL_HOST", "https://portal.example.test/portal")

	f := newCodeArtsRoutesFixture(t, false)

	recorder := f.do(http.MethodPost, "/codearts/accounts/oauth/start", map[string]any{},
		"attacker.example:9999")
	if recorder.Code != http.StatusOK {
		t.Fatalf("oauth start status=%d body=%s", recorder.Code, recorder.Body)
	}
	start := decodeRoutesJSON(t, recorder)
	sessionID := routesText(start["session_id"])
	authURL := routesText(start["auth_url"])
	if sessionID == "" || !strings.Contains(authURL, "port=54321") {
		t.Fatalf("oauth start = %#v", start)
	}
	if strings.Contains(authURL, "attacker.example") || strings.Contains(authURL, "9999") {
		t.Fatalf("oauth start used request Host: %s", authURL)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("oauth poll status=%d body=%s", recorder.Code, recorder.Body)
	}
	poll := decodeRoutesJSON(t, recorder)
	if poll["status"] != "pending" {
		t.Fatalf("oauth poll = %#v", poll)
	}

	ticket := parsed.Query().Get("ticket_id")
	callbackURL := fmt.Sprintf("http://127.0.0.1:54321/oauth/callback?code=oauth-code&ticket_id=%s", ticket)
	recorder = f.do(http.MethodPost, "/codearts/accounts/oauth/callback", map[string]any{
		"session_id":   sessionID,
		"callback_url": callbackURL,
	}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("oauth callback status=%d body=%s", recorder.Code, recorder.Body)
	}
	callback := decodeRoutesJSON(t, recorder)
	if callback["status"] != "done" || callback["ok"] != true {
		t.Fatalf("oauth callback = %#v", callback)
	}
	account, _ := callback["account"].(map[string]any)
	if account["uid"] != "u1" || account["nickname"] != "账号一" {
		t.Fatalf("oauth callback account = %#v", callback)
	}
	assertNoSecrets(t, "oauth callback", recorder.Body.String(),
		securitySentinel, accessSentinel, secretSentinel, refreshSentinel)
	if f.p.Pool().Get("u1") == nil {
		t.Fatal("oauth callback did not add the account to the pool")
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	codeartsConfig, _ := settings["codearts"].(map[string]any)
	if codeartsConfig["enabled"] != true {
		t.Fatalf("oauth callback did not enable config: %#v", settings)
	}

	// A second browser-driven flow exercises the public GET callback path.
	recorder = f.do(http.MethodPost, "/codearts/accounts/oauth/start", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("second oauth start status=%d body=%s", recorder.Code, recorder.Body)
	}
	secondStart := decodeRoutesJSON(t, recorder)
	secondParsed, err := url.Parse(routesText(secondStart["auth_url"]))
	if err != nil {
		t.Fatal(err)
	}
	secondTicket := secondParsed.Query().Get("ticket_id")
	recorder = f.do(http.MethodGet,
		"/oauth/callback?code=oauth-code&ticket_id="+url.QueryEscape(secondTicket), nil, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("browser callback status=%d body=%s", recorder.Code, recorder.Body)
	}
	html := recorder.Body.String()
	if !strings.Contains(html, "登录成功") {
		t.Fatalf("browser callback html = %s", html)
	}
	assertNoSecrets(t, "browser callback html", html,
		securitySentinel, accessSentinel, secretSentinel, refreshSentinel)
	if recorder.Header().Get("Cache-Control") != "no-store" ||
		recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("browser callback headers = %#v", recorder.Header())
	}

	recorder = f.do(http.MethodGet, "/oauth/callback?code=oauth-code", nil, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("callback without session status=%d body=%s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "回调缺少会话标识") {
		t.Fatalf("missing-session html = %s", recorder.Body.String())
	}
	recorder = f.do(http.MethodGet,
		"/oauth/callback?state="+url.QueryEscape("<script>alert(1)</script>"), nil, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown-state callback status=%d body=%s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "<script>") {
		t.Fatalf("callback HTML was not escaped: %s", recorder.Body.String())
	}
}

func TestCodeArtsOAuthBrowserCallbackPersistsAccountAndEnablesRealm(t *testing.T) {
	fake, _ := newOAuthFake(t)
	const tokenSentinel = "browser-callback-security-token-0123456789"
	fake.tokenFn = func() map[string]any {
		payload := defaultTokenPayload()
		payload["credentials"] = map[string]any{
			"access_key_id":     "browser-access",
			"secret_access_key": "browser-secret",
			"security_token":    tokenSentinel,
			"expiration":        "2030-01-02T03:04:05Z",
		}
		return payload
	}
	f := newCodeArtsRoutesFixture(t, false)

	recorder := f.do(http.MethodPost, "/codearts/accounts/oauth/start", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("oauth start status=%d body=%s", recorder.Code, recorder.Body)
	}
	start := decodeRoutesJSON(t, recorder)
	parsed, err := url.Parse(routesText(start["auth_url"]))
	if err != nil {
		t.Fatal(err)
	}
	ticket := parsed.Query().Get("ticket_id")
	recorder = f.do(http.MethodGet,
		"/oauth/callback?code=oauth-code&ticket_id="+url.QueryEscape(ticket), nil, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("browser callback status=%d body=%s", recorder.Code, recorder.Body)
	}
	assertNoSecrets(t, "browser callback", recorder.Body.String(), tokenSentinel)
	if f.p.Pool().Get("u1") == nil {
		t.Fatal("browser callback did not add the account to the pool")
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	codeartsConfig, _ := settings["codearts"].(map[string]any)
	if codeartsConfig["enabled"] != true {
		t.Fatalf("browser callback did not enable the realm: %#v", settings)
	}
}

func TestCodeArtsOAuthTicketFallbackPollReloadsPool(t *testing.T) {
	_, _ = newOAuthFake(t)
	f := newCodeArtsRoutesFixture(t, false)

	recorder := f.do(http.MethodPost, "/codearts/accounts/oauth/start", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("oauth start status=%d body=%s", recorder.Code, recorder.Body)
	}
	start := decodeRoutesJSON(t, recorder)
	sessionID := routesText(start["session_id"])
	parsed, err := url.Parse(routesText(start["auth_url"]))
	if err != nil {
		t.Fatal(err)
	}
	ticket := parsed.Query().Get("ticket_id")

	storeKey, err := oauthStoreKey(f.store)
	if err != nil {
		t.Fatal(err)
	}
	f.p.oauthSessions.mu.Lock()
	session := f.p.oauthSessions.byStore[storeKey][sessionID]
	secret := ""
	if session != nil {
		secret = session.Secret
	}
	f.p.oauthSessions.mu.Unlock()
	if secret == "" {
		t.Fatal("OAuth session secret not found")
	}

	redirect := "https://portal.example.test/portal/callback?ticket_id=" + url.QueryEscape(ticket)
	callbackURL := "http://127.0.0.1:54321/oauth/callback?secret=" +
		url.QueryEscape(secret) + "&redirect=" + url.QueryEscape(redirect)
	recorder = f.do(http.MethodPost, "/codearts/accounts/oauth/callback", map[string]any{
		"session_id":   sessionID,
		"callback_url": callbackURL,
	}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("fallback callback status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["status"] != "continue" || routesText(body["next_url"]) == "" {
		t.Fatalf("fallback callback = %#v", body)
	}

	recorder = f.do(http.MethodPost, "/codearts/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("fallback poll status=%d body=%s", recorder.Code, recorder.Body)
	}
	poll := decodeRoutesJSON(t, recorder)
	if poll["status"] != "done" {
		t.Fatalf("fallback poll = %#v", poll)
	}
	if f.p.Pool().Get("u1") == nil {
		t.Fatal("ticket-fallback poll did not reload the account pool")
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	codeartsConfig, _ := settings["codearts"].(map[string]any)
	if codeartsConfig["enabled"] != true {
		t.Fatalf("ticket-fallback poll did not enable config: %#v", settings)
	}
}

func beginRouteTicketFallback(t *testing.T, f *codeartsRoutesFixture) (sessionID, ticket, secret string) {
	t.Helper()
	recorder := f.do(http.MethodPost, "/codearts/accounts/oauth/start", map[string]any{}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("oauth start status=%d body=%s", recorder.Code, recorder.Body)
	}
	start := decodeRoutesJSON(t, recorder)
	sessionID = routesText(start["session_id"])
	parsed, err := url.Parse(routesText(start["auth_url"]))
	if err != nil {
		t.Fatal(err)
	}
	ticket = parsed.Query().Get("ticket_id")

	storeKey, err := oauthStoreKey(f.store)
	if err != nil {
		t.Fatal(err)
	}
	f.p.oauthSessions.mu.Lock()
	session := f.p.oauthSessions.byStore[storeKey][sessionID]
	if session != nil {
		secret = session.Secret
	}
	f.p.oauthSessions.mu.Unlock()
	if secret == "" {
		t.Fatal("OAuth session secret not found")
	}

	redirect := "https://portal.example.test/portal/callback?ticket_id=" + url.QueryEscape(ticket)
	callbackURL := "http://127.0.0.1:54321/oauth/callback?secret=" +
		url.QueryEscape(secret) + "&redirect=" + url.QueryEscape(redirect)
	recorder = f.do(http.MethodPost, "/codearts/accounts/oauth/callback", map[string]any{
		"session_id":   sessionID,
		"callback_url": callbackURL,
	}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("fallback callback status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	if body["status"] != "continue" || routesText(body["next_url"]) == "" {
		t.Fatalf("fallback callback = %#v", body)
	}
	return sessionID, ticket, secret
}

func TestCodeArtsOAuthTicketFallbackReimportsExistingAccount(t *testing.T) {
	fake, _ := newOAuthFake(t)
	const (
		newToken  = "re-login-security-token-0123456789"
		newAccess = "re-login-access-key"
		newSecret = "re-login-secret-key"
	)
	fake.ticketFn = func() map[string]any {
		payload := defaultTokenPayload()
		payload["credentials"] = map[string]any{
			"access_key_id":     newAccess,
			"secret_access_key": newSecret,
			"security_token":    newToken,
			"expiration":        "2030-01-02T03:04:05Z",
		}
		return payload
	}
	f := newCodeArtsRoutesFixture(t, false)
	existing := addRouteAccount(t, f.p, "u1")
	existing.mu.Lock()
	oldToken := existing.Auth.CloudDragonToken
	existing.mu.Unlock()

	sessionID, _, _ := beginRouteTicketFallback(t, f)
	existing.mu.Lock()
	stillOld := existing.Auth.CloudDragonToken
	existing.mu.Unlock()
	if stillOld != oldToken {
		t.Fatalf("credentials changed before ticket poll: %q", stillOld)
	}

	recorder := f.do(http.MethodPost, "/codearts/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("fallback poll status=%d body=%s", recorder.Code, recorder.Body)
	}
	poll := decodeRoutesJSON(t, recorder)
	if poll["status"] != "done" {
		t.Fatalf("fallback poll = %#v", poll)
	}
	account := f.p.Pool().Get("u1")
	if account == nil {
		t.Fatal("fallback poll removed the existing account")
	}
	account.mu.Lock()
	reloadedToken := account.Auth.CloudDragonToken
	reloadedAccess := account.Auth.AccessKeyID
	account.mu.Unlock()
	if reloadedToken != newToken || reloadedAccess != newAccess {
		t.Fatalf("existing account was not reimported: token=%q access=%q",
			reloadedToken, reloadedAccess)
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	codeartsConfig, _ := settings["codearts"].(map[string]any)
	if codeartsConfig["enabled"] != true {
		t.Fatalf("re-login did not enable the disabled realm: %#v", settings)
	}
}

func TestCodeArtsOAuthTicketFallbackRetriesPartialEnable(t *testing.T) {
	_, _ = newOAuthFake(t)
	f := newCodeArtsRoutesFixture(t, false)
	sessionID, _, _ := beginRouteTicketFallback(t, f)

	settingsPath := f.store.Path
	if err := os.RemoveAll(settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(settingsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(settingsPath) })

	recorder := f.do(http.MethodPost, "/codearts/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("partial-enable poll status=%d body=%s", recorder.Code, recorder.Body)
	}
	if f.p.Pool().Get("u1") == nil {
		t.Fatal("partial-enable poll did not add the account")
	}
	if f.p.Config().Enabled {
		t.Fatal("settings write unexpectedly succeeded while settings.json was a directory")
	}

	if err := os.RemoveAll(settingsPath); err != nil {
		t.Fatal(err)
	}
	recorder = f.do(http.MethodPost, "/codearts/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("retry poll status=%d body=%s", recorder.Code, recorder.Body)
	}
	if !f.p.Config().Enabled {
		t.Fatal("retry poll did not finish enabling the realm")
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	codeartsConfig, _ := settings["codearts"].(map[string]any)
	if codeartsConfig["enabled"] != true {
		t.Fatalf("retry poll did not persist enabled=true: %#v", settings)
	}
}

func TestCodeArtsOAuthStartRequiresInjectedListenerPort(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	f.p.SetListenerPort(0)
	recorder := f.do(http.MethodPost, "/codearts/accounts/oauth/start", map[string]any{}, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unset listener port status=%d body=%s", recorder.Code, recorder.Body)
	}
}

func TestCodeArtsDeleteFailureUsesAccurateMessage(t *testing.T) {
	f := newCodeArtsRoutesFixture(t, true)
	addRouteAccount(t, f.p, "u1")

	folder := f.p.pool.folder
	backup := folder + ".bak"
	if err := os.Rename(folder, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(folder, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(folder)
		_ = os.Rename(backup, folder)
	})

	recorder := f.do(http.MethodPost, "/codearts/accounts/delete",
		map[string]any{"uid": "u1"}, "")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("delete failure status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := decodeRoutesJSON(t, recorder)
	failure := routesErrorBody(t, body)
	if failure["type"] != "server_error" {
		t.Fatalf("delete failure envelope = %#v", failure)
	}
	message := routesText(failure["message"])
	if !strings.Contains(message, "账号文件删除失败") {
		t.Fatalf("delete failure message = %q", message)
	}
	if strings.Contains(message, "仍保留") {
		t.Fatalf("delete failure still claims the account remains: %q", message)
	}
	if strings.Contains(recorder.Body.String(), f.dir) {
		t.Fatalf("delete failure leaked a filesystem path: %s", recorder.Body.String())
	}
}

func TestCodeArtsAdminErrorsNeverExposeFilesystemDetail(t *testing.T) {
	// Panel error messages must never echo local filesystem paths or raw
	// permission errors. The concrete failure is logged server-side only.
	failure := adminErrorFrom(fmt.Errorf(
		"codearts: remove auth file /srv/private/accounts/codearts-u1.json: permission denied",
	))
	if failure.Status != http.StatusBadGateway || failure.Type != "invalid_request_error" {
		t.Fatalf("filesystem error mapped to %#v", failure)
	}
	if strings.Contains(failure.Message, "/srv/private") ||
		strings.Contains(failure.Message, "permission denied") {
		t.Fatalf("filesystem detail leaked: %q", failure.Message)
	}

	recorder := httptest.NewRecorder()
	writeAdminError(recorder, failure)
	body := routesErrorBody(t, decodeRoutesJSON(t, recorder))
	if strings.Contains(routesText(body["message"]), "/srv/private") {
		t.Fatalf("response leaked a filesystem path: %#v", body)
	}
}
