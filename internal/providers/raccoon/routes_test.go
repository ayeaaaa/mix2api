package raccoon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// routesFake is a deterministic upstream for the admin/OAuth suite. It serves
// the login exchange and catalog itself and delegates the points, refresh, and
// login-reward endpoints to the existing pointsFake. No test reaches the network.
type routesFake struct {
	mu sync.Mutex

	points *pointsFake

	exchangeCalls  int
	exchangeStatus int
	exchangeBody   map[string]any
	exchangeToken  string
	exchangeCode   string
	lastExchange   map[string]any

	catalogCalls  int
	catalogStatus int
	catalogBody   []byte
}

func newRoutesFake(now time.Time, token, uid string) *routesFake {
	points := newPointsFake(now)
	points.allowed[token] = true
	points.refreshExpiry = now.Add(time.Hour)
	return &routesFake{
		points:        points,
		exchangeToken: makeUnsignedJWTForTest(uid, now.Add(time.Hour)),
		exchangeCode:  "oauth-code",
		catalogBody:   []byte(catalogFixture),
	}
}

func makeUnsignedJWTForTest(uid string, expiry time.Time) string {
	return makeUnsignedJWTNoT(uid, expiry)
}

// makeUnsignedJWTNoT builds the same unsigned JWT fixture as the auth tests
// without needing a *testing.T from inside a fake constructor.
func makeUnsignedJWTNoT(uid string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"sub":"` + uid + `","exp":` + strconv.FormatInt(expiry.Unix(), 10) + `}`,
	))
	return header + "." + payload + ".signature"
}

func (f *routesFake) allow(token string) {
	f.points.update(func(points *pointsFake) { points.allowed[token] = true })
}

func (f *routesFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == LoginPath:
		f.serveExchange(w, r)
	case r.Method == http.MethodGet && r.URL.Path == CatalogPath:
		f.serveCatalog(w, r)
	default:
		f.points.ServeHTTP(w, r)
	}
}

func (f *routesFake) serveExchange(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.exchangeCalls++
	status := f.exchangeStatus
	response := f.exchangeBody
	token := f.exchangeToken
	expected := f.exchangeCode
	last := body
	f.lastExchange = last
	f.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		if response == nil {
			response = map[string]any{"message": "exchange rejected"}
		}
		writeCatalogJSON(w, status, response)
		return
	}
	code, _ := body["authorization_code"].(string)
	if expected != "" && code != expected {
		writeCatalogJSON(w, http.StatusBadRequest, map[string]any{"message": "invalid authorization code"})
		return
	}
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"data": map[string]any{
			"access_token":    token,
			"refresh_token":   "refresh-oauth",
			"token_type":      "Bearer",
			"office_identity": "personal",
			"office_org_name": "OAuth 账号",
		},
	})
}

func (f *routesFake) serveCatalog(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.catalogCalls++
	status := f.catalogStatus
	body := append([]byte(nil), f.catalogBody...)
	f.mu.Unlock()
	if status != 0 && status != http.StatusOK {
		writeCatalogJSON(w, status, map[string]any{"message": "catalog failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (f *routesFake) exchangeSnapshot() (int, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchangeCalls, f.lastExchange
}

func (f *routesFake) catalogSnapshot() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogCalls
}

type routesFixture struct {
	p      *Provider
	store  *config.Store
	dir    string
	fake   *routesFake
	server *httptest.Server
	mux    *http.ServeMux
	now    time.Time
	uid    string
	token  string
}

func newRoutesFixture(t *testing.T) *routesFixture {
	return newRoutesFixtureFor(t, "pu-routes", time.Hour)
}

func newRoutesFixtureFor(t *testing.T, uid string, expiresIn time.Duration) *routesFixture {
	return newRoutesFixtureWithSetup(t, uid, expiresIn, nil)
}

func newRoutesFixtureWithSetup(
	t *testing.T,
	uid string,
	expiresIn time.Duration,
	setup func(dir string),
) *routesFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	token := makeUnsignedJWTNoT(uid, now.Add(expiresIn))
	fake := newRoutesFake(now, token, "pu-oauth")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("WB_RACCOON_BASE_URL", server.URL)

	dir := t.TempDir()
	if setup != nil {
		setup(dir)
	}
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixture := &routesFixture{
		p: provider, store: store, dir: dir, fake: fake, server: server,
		now: now, uid: uid, token: token,
	}
	provider.SetNow(func() time.Time { return fixture.now })
	provider.SetListenerPort(54321)
	fixture.mux = http.NewServeMux()
	api.RegisterProviderRoutes(fixture.mux, provider)
	return fixture
}

func (f *routesFixture) do(method, path string, payload any, host string) *httptest.ResponseRecorder {
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
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *routesFixture) importFlat(t *testing.T, uid, token string, extra map[string]any) map[string]any {
	t.Helper()
	f.fake.allow(token)
	payload := map[string]any{
		"access_token":    token,
		"refresh_token":   "refresh-" + uid,
		"office_identity": "personal",
		"office_org_name": "Route " + uid,
	}
	for key, value := range extra {
		payload[key] = value
	}
	rec := f.do(http.MethodPost, "/raccoon/accounts/import", payload, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", rec.Code, rec.Body)
	}
	return decodeRouteJSON(t, rec)
}

func decodeRouteJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body, err)
	}
	return body
}

func routeText(value any) string {
	text, _ := value.(string)
	return text
}

func routeErrorBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	errObject, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %#v", body)
	}
	return errObject
}

func routeInt(value any) int {
	return testInt(value)
}

func TestRaccoonRoutesRequirePanel(t *testing.T) {
	// This test intentionally asserts route metadata only. The 401 panel gate
	// is enforced by external API middleware, not by the provider or adapter.
	f := newRoutesFixture(t)
	required := []string{
		"/raccoon/overview", "/raccoon/accounts", "/raccoon/accounts/import",
		"/raccoon/accounts/enable", "/raccoon/accounts/disable",
		"/raccoon/accounts/delete", "/raccoon/accounts/clear-cooldown",
		"/raccoon/accounts/proxy", "/raccoon/accounts/note",
		"/raccoon/accounts/tasks", "/raccoon/accounts/test",
		"/raccoon/accounts/grant-desktop", "/raccoon/accounts/grant-mobile",
		"/raccoon/credits/refresh", "/raccoon/keepalive",
		"/raccoon/models/refresh", "/raccoon/test",
		"/raccoon/accounts/oauth/start", "/raccoon/accounts/oauth/poll",
		"/raccoon/accounts/oauth/callback", "/auth/callback",
	}

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
		if route.Path == "/auth/callback" {
			if route.Auth != providers.AuthNone {
				t.Fatalf("callback auth = %v, want AuthNone", route.Auth)
			}
			continue
		}
		if route.Auth != providers.AuthPanel {
			t.Fatalf("route %s auth = %v, want AuthPanel", key, route.Auth)
		}
	}
	for _, path := range required {
		found := false
		for _, route := range routes {
			if route.Path == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("required route %s is missing", path)
		}
	}
}

func TestOverviewContainsNoTokens(t *testing.T) {
	f := newRoutesFixture(t)
	imported := f.importFlat(t, f.uid, f.token, nil)
	if routeText(imported["uid"]) != f.uid {
		t.Fatalf("import uid = %v, want %s", imported["uid"], f.uid)
	}
	if imported["enabled"] != true {
		t.Fatalf("import did not auto-enable: %#v", imported)
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !isTrue(settings["raccoon"].(map[string]any)["enabled"]) {
		t.Fatalf("raccoon.enabled not saved: %#v", settings)
	}

	rec := f.do(http.MethodGet, "/raccoon/overview?force=1", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", rec.Code, rec.Body)
	}
	overview := decodeRouteJSON(t, rec)
	if overview["provider"] != "raccoon" {
		t.Fatalf("provider = %v", overview["provider"])
	}
	if overview["online"] != true {
		t.Fatalf("online = %v", overview["online"])
	}
	if overview["auto_claim_login"] != false {
		t.Fatalf("auto_claim_login = %v", overview["auto_claim_login"])
	}
	if routeText(overview["base_url"]) != f.server.URL {
		t.Fatalf("base_url = %v, want %s", overview["base_url"], f.server.URL)
	}
	accounts, _ := overview["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", overview["accounts"])
	}
	account := accounts[0].(map[string]any)
	if account["uid"] != f.uid || account["disabled"] != false {
		t.Fatalf("account = %#v", account)
	}
	credits, _ := account["credits"].(map[string]any)
	if routeInt(credits["available"]) != 1234 {
		t.Fatalf("credits = %#v", account["credits"])
	}
	if stats, ok := overview["stats"].(map[string]any); !ok || routeInt(stats["total"]) != 1 {
		t.Fatalf("stats = %#v", overview["stats"])
	}
	if models, ok := overview["models"].([]any); !ok || len(models) < 2 {
		t.Fatalf("models = %#v", overview["models"])
	}
	health, ok := overview["health"].(map[string]any)
	if !ok || health["ok"] != true || health["accounts"] == nil {
		t.Fatalf("health = %#v", overview["health"])
	}
	if health["catalog_source"] != "raccoon" {
		t.Fatalf("catalog_source = %v", health["catalog_source"])
	}
	if routeInt(health["models"]) < 2 {
		t.Fatalf("health models = %v", health["models"])
	}

	blob := rec.Body.String()
	if strings.Contains(blob, f.token) || strings.Contains(blob, "refresh-"+f.uid) {
		t.Fatalf("overview leaked a credential: %s", blob)
	}
	if strings.Contains(blob, `"access_token"`) || strings.Contains(blob, `"refresh_token"`) {
		t.Fatalf("overview leaked a token key: %s", blob)
	}

	accountsRec := f.do(http.MethodGet, "/raccoon/accounts", nil, "")
	if accountsRec.Code != http.StatusOK {
		t.Fatalf("accounts status=%d body=%s", accountsRec.Code, accountsRec.Body)
	}
	accountsBody := decodeRouteJSON(t, accountsRec)
	for _, key := range []string{"provider", "accounts", "stats", "models", "base_url", "health"} {
		if _, ok := accountsBody[key]; !ok {
			t.Fatalf("accounts missing %q: %#v", key, accountsBody)
		}
	}
	accountsBlob := accountsRec.Body.String()
	if strings.Contains(accountsBlob, f.token) || strings.Contains(accountsBlob, `"access_token"`) {
		t.Fatalf("accounts leaked a credential: %s", accountsBlob)
	}

	healthPayload, err := f.p.Health(context.Background(), true)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if healthPayload["ok"] != true || healthPayload["configured"] != true ||
		routeInt(healthPayload["accounts"]) != 1 || routeInt(healthPayload["healthy"]) != 1 {
		t.Fatalf("health payload = %#v", healthPayload)
	}
	if healthPayload["base_url"] != f.server.URL {
		t.Fatalf("health base_url = %v", healthPayload["base_url"])
	}
}

func TestImportFlatAndNestedAuth(t *testing.T) {
	f := newRoutesFixture(t)

	nestedUID := "pu-nested"
	nestedToken := makeUnsignedJWTNoT(nestedUID, f.now.Add(time.Hour))
	f.fake.allow(nestedToken)
	nested := map[string]any{
		"auth": map[string]any{
			"data": map[string]any{
				"access_token":    "Bearer " + nestedToken,
				"refresh_token":   "refresh-" + nestedUID,
				"office_org_name": "Nested Org",
				"proxySlot":       "slot-a",
			},
		},
	}
	rec := f.do(http.MethodPost, "/raccoon/accounts/import", nested, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("nested import status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeRouteJSON(t, rec)
	if body["uid"] != nestedUID || body["enabled"] != true || body["ok"] != true {
		t.Fatalf("nested import = %#v", body)
	}
	account, _ := body["account"].(map[string]any)
	if account["nickname"] != "Nested Org" || account["proxy_slot"] != "slot-a" {
		t.Fatalf("nested account = %#v", account)
	}
	blob := rec.Body.String()
	if strings.Contains(blob, nestedToken) || strings.Contains(blob, "refresh-"+nestedUID) ||
		strings.Contains(blob, `"access_token"`) || strings.Contains(blob, `"refresh_token"`) {
		t.Fatalf("import leaked a token: %s", blob)
	}

	path, err := AuthPath(f.dir, nestedUID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("auth file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth mode = %o, want 600", info.Mode().Perm())
	}
	saved, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != nestedToken || saved.ProxySlot != "slot-a" {
		t.Fatalf("saved auth = %#v", saved)
	}

	// A flat payload may carry the token in "token" or "auth_token".
	flatUID := "pu-flat-token"
	flatToken := makeUnsignedJWTNoT(flatUID, f.now.Add(time.Hour))
	f.fake.allow(flatToken)
	flat := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{
		"token":        "Bearer " + flatToken,
		"auth_token":   "",
		"refreshToken": "ignored",
	}, "")
	if flat.Code != http.StatusOK {
		t.Fatalf("flat token import status=%d body=%s", flat.Code, flat.Body)
	}
	if decodeRouteJSON(t, flat)["uid"] != flatUID {
		t.Fatalf("flat token import = %#v", decodeRouteJSON(t, flat))
	}

	missing := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{"auth": map[string]any{}}, "")
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing access status=%d body=%s", missing.Code, missing.Body)
	}
	errObject := routeErrorBody(t, decodeRouteJSON(t, missing))
	if !strings.Contains(strings.ToLower(routeText(errObject["message"])), "access_token") {
		t.Fatalf("missing access message = %v", errObject["message"])
	}
}

func TestImportAuthPathCollisionDoesNotOverwrite(t *testing.T) {
	f := newRoutesFixtureWithSetup(t, "pu-routes", time.Hour, func(dir string) {
		writePoolAuth(t, dir, "a_b", "", time.Now().Add(time.Hour), nil)
	})
	existingPath, err := AuthPath(f.dir, "a_b")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	existing := f.p.pool.Get("a_b")
	if existing == nil {
		t.Fatal("existing collision fixture account was not loaded")
	}

	collidingUID := "a/b"
	collidingToken := makeUnsignedJWTNoT(collidingUID, f.now.Add(time.Hour))
	f.fake.allow(collidingToken)
	rec := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{
		"access_token":  collidingToken,
		"refresh_token": "refresh-" + collidingUID,
	}, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("colliding import succeeded: %s", rec.Body)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("colliding import status=%d body=%s", rec.Code, rec.Body)
	}
	if after, err := os.ReadFile(existingPath); err != nil {
		t.Fatal(err)
	} else if string(before) != string(after) {
		t.Fatal("colliding import overwrote the existing auth file")
	}
	if strings.Contains(rec.Body.String(), f.dir) ||
		strings.Contains(rec.Body.String(), existingPath) ||
		strings.Contains(rec.Body.String(), "raccoon-a_b.json") {
		t.Fatalf("colliding import leaked a filesystem path: %s", rec.Body)
	}
	errObject := routeErrorBody(t, decodeRouteJSON(t, rec))
	if errObject["message"] != "raccoon: auth file belongs to a different account" {
		t.Fatalf("colliding import message = %#v", errObject["message"])
	}
	if f.p.pool.Get(collidingUID) != nil {
		t.Fatal("colliding import created a phantom account")
	}
	if f.p.pool.Get("a_b") != existing {
		t.Fatal("colliding import replaced the existing account")
	}
	if saved, err := LoadAuth(existingPath); err != nil || saved.UID != "a_b" {
		t.Fatalf("existing auth file changed: auth=%#v err=%v", saved, err)
	}
	if raccoonEnabled(t, f) {
		t.Fatal("colliding import enabled the realm")
	}
}

func TestOAuthExchangeAuthPathCollisionDoesNotOverwrite(t *testing.T) {
	f := newRoutesFixtureWithSetup(t, "pu-routes", time.Hour, func(dir string) {
		writePoolAuth(t, dir, "a_b", "", time.Now().Add(time.Hour), nil)
	})
	existingPath, err := AuthPath(f.dir, "a_b")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}

	f.fake.mu.Lock()
	f.fake.exchangeCode = "collision-code"
	f.fake.exchangeToken = makeUnsignedJWTNoT("a/b", f.now.Add(time.Hour))
	f.fake.mu.Unlock()
	started := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	callback := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
		"session_id": routeText(started["session_id"]),
		"code":       "collision-code",
	}, "")
	if callback.Code == http.StatusOK {
		t.Fatalf("colliding OAuth exchange succeeded: %s", callback.Body)
	}
	if callback.Code != http.StatusConflict {
		t.Fatalf("colliding OAuth exchange status=%d body=%s", callback.Code, callback.Body)
	}
	if after, err := os.ReadFile(existingPath); err != nil {
		t.Fatal(err)
	} else if string(before) != string(after) {
		t.Fatal("colliding OAuth exchange overwrote the existing auth file")
	}
	if strings.Contains(callback.Body.String(), f.dir) ||
		strings.Contains(callback.Body.String(), existingPath) ||
		strings.Contains(callback.Body.String(), "raccoon-a_b.json") {
		t.Fatalf("colliding OAuth exchange leaked a filesystem path: %s", callback.Body)
	}
	errObject := routeErrorBody(t, decodeRouteJSON(t, callback))
	if errObject["message"] != "raccoon: auth file belongs to a different account" {
		t.Fatalf("colliding OAuth exchange message = %#v", errObject["message"])
	}
	if f.p.pool.Get("a/b") != nil {
		t.Fatal("colliding OAuth exchange created a phantom account")
	}
	if f.p.pool.Get("a_b") == nil {
		t.Fatal("colliding OAuth exchange removed the existing account")
	}
	if raccoonEnabled(t, f) {
		t.Fatal("colliding OAuth exchange enabled the realm")
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 1 {
		t.Fatalf("OAuth exchange calls = %d, want 1", calls)
	}
	poll := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/poll",
		map[string]any{"session_id": routeText(started["session_id"])}, ""))
	if poll["status"] != "error" {
		t.Fatalf("failed OAuth session = %#v", poll)
	}
}

func TestOAuthCollisionCallbackHTMLHasNoPaths(t *testing.T) {
	f := newRoutesFixtureWithSetup(t, "pu-routes", time.Hour, func(dir string) {
		writePoolAuth(t, dir, "a_b", "", time.Now().Add(time.Hour), nil)
	})
	existingPath, err := AuthPath(f.dir, "a_b")
	if err != nil {
		t.Fatal(err)
	}
	f.fake.mu.Lock()
	f.fake.exchangeCode = "html-collision-code"
	f.fake.exchangeToken = makeUnsignedJWTNoT("a/b", f.now.Add(time.Hour))
	f.fake.mu.Unlock()
	started := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	state := parsedState(t, routeText(started["auth_url"]))
	callback := f.do(http.MethodGet, "/auth/callback?"+url.Values{
		"code": {"html-collision-code"}, "state": {state},
	}.Encode(), nil, "")
	if callback.Code != http.StatusBadRequest {
		t.Fatalf("collision callback status=%d body=%s", callback.Code, callback.Body)
	}
	html := callback.Body.String()
	if strings.Contains(html, f.dir) || strings.Contains(html, existingPath) ||
		strings.Contains(html, "raccoon-a_b.json") || strings.Contains(html, "auth path") {
		t.Fatalf("collision callback HTML leaked a filesystem path: %s", html)
	}
	if !strings.Contains(html, "登录失败") ||
		!strings.Contains(html, "auth file belongs to a different account") {
		t.Fatalf("collision callback HTML = %s", html)
	}
	poll := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/poll",
		map[string]any{"session_id": routeText(started["session_id"])}, ""))
	if poll["status"] != "error" {
		t.Fatalf("collision poll = %#v", poll)
	}
	pollBlobBytes, _ := json.Marshal(poll)
	pollBlob := string(pollBlobBytes)
	if strings.Contains(pollBlob, f.dir) || strings.Contains(pollBlob, existingPath) {
		t.Fatalf("collision poll leaked a filesystem path: %s", pollBlob)
	}
}

func TestImportRollbackOnEnableFailure(t *testing.T) {
	t.Run("new account", func(t *testing.T) {
		f := newRoutesFixture(t)
		token := makeUnsignedJWTNoT("pu-enable-fail", f.now.Add(time.Hour))
		f.fake.allow(token)
		if err := os.MkdirAll(f.store.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		rec := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{
			"access_token": token,
		}, "")
		if rec.Code == http.StatusOK {
			t.Fatalf("import succeeded despite enable failure: %s", rec.Body)
		}
		if strings.Contains(rec.Body.String(), f.dir) ||
			strings.Contains(rec.Body.String(), f.store.Path) {
			t.Fatalf("enable failure leaked a filesystem path: %s", rec.Body)
		}
		path, err := AuthPath(f.dir, "pu-enable-fail")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("enable failure left the auth file: %v", err)
		}
		if f.p.pool.Get("pu-enable-fail") != nil {
			t.Fatal("enable failure left a phantom pool account")
		}
	})

	t.Run("existing account", func(t *testing.T) {
		f := newRoutesFixture(t)
		firstToken := makeUnsignedJWTNoT("pu-reimport-fail", f.now.Add(time.Hour))
		f.fake.allow(firstToken)
		first := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{
			"access_token":    firstToken,
			"refresh_token":   "refresh-old",
			"office_org_name": "Old Org",
		}, "")
		if first.Code != http.StatusOK {
			t.Fatalf("first import status=%d body=%s", first.Code, first.Body)
		}
		path, err := AuthPath(f.dir, "pu-reimport-fail")
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.Remove(f.store.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(f.store.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		newToken := makeUnsignedJWTNoT("pu-reimport-fail", f.now.Add(2*time.Hour))
		f.fake.allow(newToken)
		rec := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{
			"access_token":    newToken,
			"refresh_token":   "refresh-new",
			"office_org_name": "New Org",
		}, "")
		if rec.Code == http.StatusOK {
			t.Fatalf("re-import succeeded despite enable failure: %s", rec.Body)
		}
		if strings.Contains(rec.Body.String(), f.dir) ||
			strings.Contains(rec.Body.String(), f.store.Path) {
			t.Fatalf("re-import enable failure leaked a filesystem path: %s", rec.Body)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("enable failure did not restore the previous auth file")
		}
		account := f.p.pool.Get("pu-reimport-fail")
		if account == nil {
			t.Fatal("enable failure removed the existing account")
		}
		if account.Auth.OfficeOrgName != "Old Org" || account.Auth.AccessToken != firstToken {
			t.Fatalf("enable failure did not restore account credentials: %#v", account.Auth)
		}
	})
}

func TestImportRollbackOnAddFailure(t *testing.T) {
	f := newRoutesFixture(t)
	statePath := filepath.Join(f.dir, "raccoon", "state.json")
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
	token := makeUnsignedJWTNoT("pu-add-fail", f.now.Add(time.Hour))
	f.fake.allow(token)
	rec := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{
		"access_token": token,
	}, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("import succeeded despite state failure: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), f.dir) ||
		strings.Contains(rec.Body.String(), f.store.Path) {
		t.Fatalf("add failure leaked a filesystem path: %s", rec.Body)
	}
	path, err := AuthPath(f.dir, "pu-add-fail")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("add failure left the auth file: %v", err)
	}
	if f.p.pool.Get("pu-add-fail") != nil {
		t.Fatal("add failure left a phantom pool account")
	}
	if raccoonEnabled(t, f) {
		t.Fatal("add failure enabled the realm")
	}
}

func TestImportAppliesSettingsToNewAccount(t *testing.T) {
	f := newRoutesFixture(t)
	if _, err := f.store.Merge(map[string]any{
		"proxy_slots": []any{
			map[string]any{
				"id": "slot-new", "url": "http://127.0.0.1:7890", "enabled": true,
			},
		},
		"limits": map[string]any{
			"reserve_credits": map[string]any{"global": 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	uid := "pu-import-settings"
	token := makeUnsignedJWTNoT(uid, f.now.Add(time.Hour))
	f.fake.allow(token)
	rec := f.do(http.MethodPost, "/raccoon/accounts/import", map[string]any{
		"access_token": token, "proxySlot": "slot-new",
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", rec.Code, rec.Body)
	}
	account := decodeRouteJSON(t, rec)["account"].(map[string]any)
	if account["proxy_slot"] != "slot-new" || account["proxy"] != "http://127.0.0.1:7890" {
		t.Fatalf("import did not resolve proxy slot: %#v", account)
	}
	if routeInt(account["reserve_credits"]) != 10 {
		t.Fatalf("import did not apply limits: %#v", account)
	}
}

func raccoonEnabled(t *testing.T, f *routesFixture) bool {
	t.Helper()
	settings, err := f.store.Load()
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	raw, _ := settings["raccoon"].(map[string]any)
	return isTrue(raw["enabled"])
}

func TestAdminErrorStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		errType string
	}{
		{
			name:    "network transport",
			err:     &APIError{Kind: KindTransient, Message: "cannot reach upstream"},
			status:  http.StatusServiceUnavailable,
			errType: "invalid_request_error",
		},
		{
			name:    "upstream 500",
			err:     &APIError{Kind: KindUpstream, Status: 500, Message: "upstream failed"},
			status:  http.StatusBadGateway,
			errType: "invalid_request_error",
		},
		{
			name:    "bad request",
			err:     &APIError{Kind: KindBadRequest, Status: 400, Message: "bad"},
			status:  http.StatusBadRequest,
			errType: "invalid_request_error",
		},
		{
			name:    "auth",
			err:     &APIError{Kind: KindAuth, Status: 401, Message: "auth"},
			status:  http.StatusBadGateway,
			errType: "invalid_request_error",
		},
		{
			name:    "rate limited",
			err:     &APIError{Kind: KindRateLimited, Status: 429, Message: "slow down"},
			status:  http.StatusTooManyRequests,
			errType: "rate_limit_error",
		},
		{
			name:    "unavailable",
			err:     &APIError{Kind: KindUnavailable, Status: 503, Message: "no account"},
			status:  http.StatusServiceUnavailable,
			errType: "invalid_request_error",
		},
		{
			name:    "not found",
			err:     &APIError{Kind: KindNotFound, Status: 404, Message: "missing"},
			status:  http.StatusConflict,
			errType: "invalid_request_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := adminErrorFrom(tc.err)
			if failure.Status != tc.status || failure.Type != tc.errType {
				t.Fatalf("adminErrorFrom = %#v, want status %d type %s",
					failure, tc.status, tc.errType)
			}
		})
	}
}

func TestAdminErrorStatusMappingRoutes(t *testing.T) {
	t.Run("network is 503", func(t *testing.T) {
		f := newRoutesFixture(t)
		f.importFlat(t, f.uid, f.token, nil)
		f.p.SetBaseURL("http://127.0.0.1:1")
		rec := f.do(http.MethodPost, "/raccoon/accounts/grant-desktop",
			map[string]any{"uid": f.uid}, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("network grant status=%d body=%s", rec.Code, rec.Body)
		}
		errObject := routeErrorBody(t, decodeRouteJSON(t, rec))
		if routeInt(errObject["code"]) != http.StatusServiceUnavailable {
			t.Fatalf("network envelope = %#v", errObject)
		}
	})

	t.Run("upstream 5xx is 502", func(t *testing.T) {
		f := newRoutesFixture(t)
		f.importFlat(t, f.uid, f.token, nil)
		f.fake.points.update(func(points *pointsFake) {
			points.desktopStatus = http.StatusInternalServerError
		})
		rec := f.do(http.MethodPost, "/raccoon/accounts/grant-desktop",
			map[string]any{"uid": f.uid}, "")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("upstream grant status=%d body=%s", rec.Code, rec.Body)
		}
		errObject := routeErrorBody(t, decodeRouteJSON(t, rec))
		if routeInt(errObject["code"]) != http.StatusBadGateway {
			t.Fatalf("upstream envelope = %#v", errObject)
		}
	})
}

func TestImportOverviewAndGrant(t *testing.T) {
	f := newRoutesFixture(t)
	imported := f.importFlat(t, f.uid, f.token, nil)
	if imported["enabled"] != true {
		t.Fatalf("import = %#v", imported)
	}

	_, balanceBefore, tasksBefore, _, _, _ := f.fake.points.counts()
	rec := f.do(http.MethodPost, "/raccoon/accounts/grant-desktop", map[string]any{"uid": f.uid}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status=%d body=%s", rec.Code, rec.Body)
	}
	result := decodeRouteJSON(t, rec)
	if result["granted"] != true || routeInt(result["points"]) != 3000 {
		t.Fatalf("grant = %#v", result)
	}
	credits, _ := result["credits"].(map[string]any)
	if routeInt(credits["available"]) != 4234 {
		t.Fatalf("grant credits = %#v", result["credits"])
	}
	tasks, _ := result["tasks"].(map[string]any)
	completed := false
	for _, raw := range tasks["new_user"].([]any) {
		item := raw.(map[string]any)
		if item["key"] == "desktop_login" && item["status"] == "completed" {
			completed = true
		}
	}
	if !completed {
		t.Fatalf("desktop task not completed: %#v", tasks)
	}
	if !strings.Contains(routeText(result["message"]), "已领取") {
		t.Fatalf("grant message = %v", result["message"])
	}
	_, balanceAfter, tasksAfter, _, _, _ := f.fake.points.counts()
	if balanceAfter <= balanceBefore || tasksAfter <= tasksBefore {
		t.Fatalf("grant did not force-refresh credits/tasks: before=%d/%d after=%d/%d",
			balanceBefore, tasksBefore, balanceAfter, tasksAfter)
	}
	blob := rec.Body.String()
	if strings.Contains(blob, f.token) || strings.Contains(blob, "refresh-"+f.uid) {
		t.Fatalf("grant leaked a token: %s", blob)
	}

	again := f.do(http.MethodPost, "/raccoon/accounts/grant-desktop", map[string]any{"uid": f.uid}, "")
	againBody := decodeRouteJSON(t, again)
	if againBody["granted"] != false || routeInt(againBody["points"]) != 0 {
		t.Fatalf("second grant = %#v", againBody)
	}
	if credits, _ := againBody["credits"].(map[string]any); routeInt(credits["available"]) != 4234 {
		t.Fatalf("second grant double-paid: %#v", againBody)
	}
	if !strings.Contains(routeText(againBody["message"]), "已领取过") {
		t.Fatalf("second grant message = %v", againBody["message"])
	}

	missing := f.do(http.MethodPost, "/raccoon/accounts/grant-desktop", map[string]any{}, "")
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("grant missing uid status=%d", missing.Code)
	}

	mobileFixture := newRoutesFixture(t)
	mobileFixture.importFlat(t, mobileFixture.uid, mobileFixture.token, nil)
	mobile := mobileFixture.do(http.MethodPost, "/raccoon/accounts/grant-mobile",
		map[string]any{"uid": mobileFixture.uid}, "")
	mobileBody := decodeRouteJSON(t, mobile)
	if mobile.Code != http.StatusOK || mobileBody["granted"] != true || routeInt(mobileBody["points"]) != 3000 {
		t.Fatalf("mobile grant = %d %#v", mobile.Code, mobileBody)
	}
	if mobileFixture.fake.points.header("X-Client-Platform", "mobile") != "app-android" ||
		mobileFixture.fake.points.header("X-Client-Version", "mobile") != "v1.1.0" {
		t.Fatalf("mobile grant headers were not applied")
	}
}

func TestUnknownRaccoonRoute404(t *testing.T) {
	f := newRoutesFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := f.do(method, "/raccoon/not-a-route", map[string]any{}, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s unknown status=%d body=%s", method, rec.Code, rec.Body)
		}
		body := decodeRouteJSON(t, rec)
		errObject := routeErrorBody(t, body)
		if errObject["type"] != "invalid_request_error" || routeInt(errObject["code"]) != 404 {
			t.Fatalf("%s error envelope = %#v", method, body)
		}
		if routeText(errObject["message"]) == "" {
			t.Fatalf("%s missing error message", method)
		}
		if routeText(errObject["message"]) != "unknown raccoon endpoint" {
			t.Fatalf("%s unknown message = %q, want provider semantics",
				method, errObject["message"])
		}
	}

	// Go's ServeMux may answer a known path with the wrong method as 404 or
	// 405 depending on pattern specificity; v1 also only promises a 4xx. When
	// 405 is chosen, the adapter still returns the JSON envelope.
	methodMismatch := f.do(http.MethodPost, "/raccoon/overview", map[string]any{}, "")
	if methodMismatch.Code != http.StatusNotFound && methodMismatch.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method mismatch status=%d body=%s", methodMismatch.Code, methodMismatch.Body)
	}
	if methodMismatch.Code == http.StatusMethodNotAllowed {
		errObject := routeErrorBody(t, decodeRouteJSON(t, methodMismatch))
		if routeInt(errObject["code"]) != http.StatusMethodNotAllowed {
			t.Fatalf("method mismatch envelope = %#v", errObject)
		}
	}

	malformed := f.do(http.MethodPost, "/raccoon/accounts/import", `{"auth":`, "")
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed status=%d body=%s", malformed.Code, malformed.Body)
	}
	errObject := routeErrorBody(t, decodeRouteJSON(t, malformed))
	if errObject["message"] != "invalid JSON body" {
		t.Fatalf("malformed message = %v", errObject["message"])
	}
	notObject := f.do(http.MethodPost, "/raccoon/accounts/import", `[]`, "")
	if notObject.Code != http.StatusBadRequest {
		t.Fatalf("non-object status=%d body=%s", notObject.Code, notObject.Body)
	}
}

func TestOAuthStartPollCallback(t *testing.T) {
	f := newRoutesFixture(t)
	f.fake.mu.Lock()
	f.fake.exchangeCode = "oauth-code-success"
	f.fake.mu.Unlock()
	start := f.do(http.MethodPost, "/raccoon/accounts/oauth/start", map[string]any{},
		"127.0.0.1:54321")
	if start.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body)
	}
	started := decodeRouteJSON(t, start)
	if started["ok"] != true || routeInt(started["expires_in"]) != 300 {
		t.Fatalf("start = %#v", started)
	}
	sessionID := routeText(started["session_id"])
	authURL := routeText(started["auth_url"])
	if len(sessionID) != 16 || len(authURL) == 0 || routeText(started["message"]) == "" {
		t.Fatalf("start fields = %#v", started)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Hostname() != "xiaohuanxiong.com" || parsed.Path != "/authorize" {
		t.Fatalf("authorize url = %s", authURL)
	}
	query := parsed.Query()
	if query.Get("login_source") != "desktop" ||
		query.Get("appname") != "办公小浣熊客户端" ||
		query.Get("desktop_callback") != "loopback" ||
		query.Get("redirect") != "http://127.0.0.1:54321/auth/callback" {
		t.Fatalf("authorize query = %#v", query)
	}
	state := query.Get("state")
	if len(state) != 64 || !isHex(state) {
		t.Fatalf("state = %q", state)
	}
	blob := start.Body.String()
	if strings.Contains(blob, "access_token") || strings.Contains(blob, "refresh_token") ||
		strings.Contains(blob, `"state"`) || strings.Contains(blob, `"code"`) {
		t.Fatalf("start leaked protocol secrets: %s", blob)
	}

	poll := f.do(http.MethodPost, "/raccoon/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	if poll.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", poll.Code, poll.Body)
	}
	pending := decodeRouteJSON(t, poll)
	if pending["status"] != "pending" || pending["ok"] != true ||
		pending["auth_url"] != authURL || routeText(pending["session_id"]) != sessionID {
		t.Fatalf("pending poll = %#v", pending)
	}
	if _, ok := pending["account"]; ok {
		t.Fatalf("pending poll has account: %#v", pending)
	}

	callbackPath := "/auth/callback?" + url.Values{
		"code":  {"oauth-code-success"},
		"state": {state},
	}.Encode()
	callback := f.do(http.MethodGet, callbackPath, nil, "")
	if callback.Code != http.StatusOK {
		t.Fatalf("callback status=%d body=%s", callback.Code, callback.Body)
	}
	html := callback.Body.String()
	if !strings.Contains(html, "登录成功") {
		t.Fatalf("callback html = %s", html)
	}
	if strings.Contains(html, "oauth-code-success") || strings.Contains(html, state) ||
		strings.Contains(html, "access_token") || strings.Contains(html, "refresh_token") {
		t.Fatalf("callback leaked secrets: %s", html)
	}
	if callback.Header().Get("Cache-Control") != "no-store" ||
		callback.Header().Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(callback.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("callback headers = %#v", callback.Header())
	}

	poll = f.do(http.MethodPost, "/raccoon/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	done := decodeRouteJSON(t, poll)
	if done["status"] != "done" || done["ok"] != true {
		t.Fatalf("done poll = %#v", done)
	}
	account, _ := done["account"].(map[string]any)
	if account["uid"] != "pu-oauth" || routeText(account["nickname"]) == "" {
		t.Fatalf("done account = %#v", done["account"])
	}
	doneBlob := poll.Body.String()
	if strings.Contains(doneBlob, "access_token") || strings.Contains(doneBlob, "refresh_token") ||
		strings.Contains(doneBlob, "oauth-code-success") ||
		strings.Contains(doneBlob, `"state"`) || strings.Contains(doneBlob, `"code"`) {
		t.Fatalf("done poll leaked secrets: %s", doneBlob)
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 1 {
		t.Fatalf("exchange calls = %d, want 1", calls)
	}
	if f.p.pool.Get("pu-oauth") == nil {
		t.Fatal("callback did not save the account")
	}
	settings, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !isTrue(settings["raccoon"].(map[string]any)["enabled"]) {
		t.Fatal("callback did not enable the realm")
	}
}

func isHex(value string) bool {
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func TestOAuthStateValidationAndSingleUse(t *testing.T) {
	f := newRoutesFixture(t)
	f.fake.mu.Lock()
	f.fake.exchangeCode = "state-code"
	f.fake.mu.Unlock()
	started := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	sessionID := routeText(started["session_id"])
	parsed, _ := url.Parse(routeText(started["auth_url"]))
	state := parsed.Query().Get("state")

	wrong := f.do(http.MethodGet, "/auth/callback?"+url.Values{
		"code": {"state-code"}, "state": {strings.Repeat("de", 32)},
	}.Encode(), nil, "")
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong state status=%d body=%s", wrong.Code, wrong.Body)
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 0 {
		t.Fatalf("wrong state exchanged: %d", calls)
	}

	missingCode := f.do(http.MethodGet, "/auth/callback?"+url.Values{
		"state": {state},
	}.Encode(), nil, "")
	if missingCode.Code != http.StatusBadRequest {
		t.Fatalf("missing code status=%d", missingCode.Code)
	}
	poll := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, ""))
	if poll["status"] != "pending" {
		t.Fatalf("missing code burned the session: %#v", poll)
	}

	good := f.do(http.MethodGet, "/auth/callback?"+url.Values{
		"code": {"state-code"}, "state": {state},
	}.Encode(), nil, "")
	if good.Code != http.StatusOK {
		t.Fatalf("good callback status=%d body=%s", good.Code, good.Body)
	}
	replay := f.do(http.MethodGet, "/auth/callback?"+url.Values{
		"code": {"state-code"}, "state": {state},
	}.Encode(), nil, "")
	if replay.Code != http.StatusBadRequest {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body)
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 1 {
		t.Fatalf("replay exchanged again: %d", calls)
	}

	missingState := f.do(http.MethodGet, "/auth/callback?code=state-code", nil, "")
	if missingState.Code != http.StatusBadRequest {
		t.Fatalf("missing state status=%d", missingState.Code)
	}
}

func TestOAuthTTLCapAndPortValidation(t *testing.T) {
	f := newRoutesFixture(t)
	f.p.SetListenerPort(80)
	badPort := f.do(http.MethodPost, "/raccoon/accounts/oauth/start", map[string]any{},
		"127.0.0.1:54321")
	if badPort.Code != http.StatusBadRequest {
		t.Fatalf("privileged listener port status=%d body=%s", badPort.Code, badPort.Body)
	}
	f.p.SetListenerPort(0)
	unset := f.do(http.MethodPost, "/raccoon/accounts/oauth/start", map[string]any{},
		"127.0.0.1:54321")
	if unset.Code != http.StatusBadRequest {
		t.Fatalf("unset listener port status=%d body=%s", unset.Code, unset.Body)
	}
	f.p.SetListenerPort(54321)

	// A portless reverse-proxy Host must not influence the loopback redirect.
	fallback := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "proxy.internal"))
	parsed, _ := url.Parse(routeText(fallback["auth_url"]))
	if parsed.Query().Get("redirect") != "http://127.0.0.1:54321/auth/callback" {
		t.Fatalf("fallback redirect = %q", parsed.Query().Get("redirect"))
	}
	foreign := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:9999"))
	foreignURL, _ := url.Parse(routeText(foreign["auth_url"]))
	if foreignURL.Query().Get("redirect") != "http://127.0.0.1:54321/auth/callback" {
		t.Fatalf("foreign Host changed redirect = %q", foreignURL.Query().Get("redirect"))
	}

	started := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	sessionID := routeText(started["session_id"])
	state := parsedState(t, routeText(started["auth_url"]))
	f.now = f.now.Add(301 * time.Second)
	expired := f.do(http.MethodPost, "/raccoon/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	if expired.Code != http.StatusConflict {
		t.Fatalf("expired poll status=%d body=%s", expired.Code, expired.Body)
	}
	expiredCallback := f.do(http.MethodGet, "/auth/callback?"+url.Values{
		"code": {"oauth-code"}, "state": {state},
	}.Encode(), nil, "")
	if expiredCallback.Code != http.StatusBadRequest {
		t.Fatalf("expired callback status=%d body=%s", expiredCallback.Code, expiredCallback.Body)
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 0 {
		t.Fatalf("expired session exchanged: %d", calls)
	}

	capFixture := newRoutesFixture(t)
	for index := 0; index < OAUTHMaxSessions+5; index++ {
		rec := capFixture.do(http.MethodPost, "/raccoon/accounts/oauth/start",
			map[string]any{}, "127.0.0.1:54321")
		if rec.Code != http.StatusOK {
			t.Fatalf("start %d status=%d body=%s", index, rec.Code, rec.Body)
		}
	}
	capFixture.p.oauthMu.Lock()
	count := len(capFixture.p.oauthSessions)
	capFixture.p.oauthMu.Unlock()
	if count > OAUTHMaxSessions {
		t.Fatalf("OAuth sessions = %d, want <= %d", count, OAUTHMaxSessions)
	}
}

func parsedState(t *testing.T, authURL string) string {
	t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get("state")
}

func TestOAuthManualCallbackValidation(t *testing.T) {
	f := newRoutesFixture(t)
	f.fake.mu.Lock()
	f.fake.exchangeCode = "manual-code"
	f.fake.mu.Unlock()
	started := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	sessionID := routeText(started["session_id"])
	state := parsedState(t, routeText(started["auth_url"]))
	query := url.Values{"code": {"manual-code"}, "state": {state}}.Encode()

	badURLs := []string{
		"https://127.0.0.1:54321/auth/callback?" + query,
		"http://evil.invalid:54321/auth/callback?" + query,
		"http://127.0.0.1:54321/wrong/path?" + query,
		"http://127.0.0.1:54322/auth/callback?" + query,
		"http://127.0.0.1:54321/auth/callback?" + url.Values{
			"code": {"manual-code"}, "state": {strings.Repeat("ff", 32)},
		}.Encode(),
		"http://user:pass@127.0.0.1:54321/auth/callback?" + query,
		"http://127.0.0.1:54321/raccoon/oauth/callback?" + query,
	}
	for _, callbackURL := range badURLs {
		rec := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
			"session_id": sessionID, "callback_url": callbackURL,
		}, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad callback %s status=%d body=%s", callbackURL, rec.Code, rec.Body)
		}
	}
	noCode := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
		"session_id": sessionID,
	}, "")
	if noCode.Code != http.StatusBadRequest {
		t.Fatalf("callback without code status=%d body=%s", noCode.Code, noCode.Body)
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 0 {
		t.Fatalf("bad callbacks exchanged: %d", calls)
	}

	raw := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
		"session_id": sessionID, "code": "manual-code",
	}, "")
	if raw.Code != http.StatusOK {
		t.Fatalf("raw callback status=%d body=%s", raw.Code, raw.Body)
	}
	if decodeRouteJSON(t, raw)["status"] != "done" {
		t.Fatalf("raw callback = %s", raw.Body)
	}
	replay := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
		"session_id": sessionID, "code": "manual-code",
	}, "")
	if replay.Code != http.StatusConflict {
		t.Fatalf("done session replay status=%d body=%s", replay.Code, replay.Body)
	}

	second := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	secondState := parsedState(t, routeText(second["auth_url"]))
	secondURL := "http://127.0.0.1:54321/auth/callback?" + url.Values{
		"code": {"manual-code"}, "state": {secondState},
	}.Encode()
	urlCallback := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
		"session_id": routeText(second["session_id"]), "callback_url": secondURL,
	}, "")
	if urlCallback.Code != http.StatusOK {
		t.Fatalf("url callback status=%d body=%s", urlCallback.Code, urlCallback.Body)
	}
	parsed, _ := url.Parse(secondURL)
	if parsed.Query().Get("state") != secondState {
		t.Fatal("test callback URL lost state")
	}
}

func TestOAuthConcurrentClaimExchangesOnce(t *testing.T) {
	f := newRoutesFixture(t)
	f.fake.mu.Lock()
	f.fake.exchangeCode = "concurrent-code"
	f.fake.mu.Unlock()
	started := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	sessionID := routeText(started["session_id"])

	const callbacks = 8
	codes := make(chan int, callbacks)
	var group sync.WaitGroup
	for index := 0; index < callbacks; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			rec := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
				"session_id": sessionID,
				"code":       "concurrent-code",
			}, "")
			codes <- rec.Code
		}()
	}
	group.Wait()
	close(codes)

	successes, conflicts := 0, 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("callback status = %d", code)
		}
	}
	if successes != 1 || conflicts != callbacks-1 {
		t.Fatalf("concurrent claims success=%d conflict=%d", successes, conflicts)
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 1 {
		t.Fatalf("concurrent claim exchanged %d times", calls)
	}
	if f.p.pool.Get("pu-oauth") == nil {
		t.Fatal("concurrent claim did not save exactly one account")
	}
}

func TestOAuthCallbackHTMLRedaction(t *testing.T) {
	f := newRoutesFixture(t)
	f.fake.mu.Lock()
	f.fake.exchangeCode = "secret-code-html"
	f.fake.exchangeStatus = http.StatusBadRequest
	f.fake.exchangeBody = map[string]any{
		"message": "bad <b>request</b> echoed secret-code-html",
	}
	f.fake.mu.Unlock()
	started := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/accounts/oauth/start",
		map[string]any{}, "127.0.0.1:54321"))
	sessionID := routeText(started["session_id"])
	state := parsedState(t, routeText(started["auth_url"]))
	callback := f.do(http.MethodGet, "/auth/callback?"+url.Values{
		"code": {"secret-code-html"}, "state": {state},
	}.Encode(), nil, "")
	if callback.Code != http.StatusBadRequest {
		t.Fatalf("exchange error callback status=%d body=%s", callback.Code, callback.Body)
	}
	html := callback.Body.String()
	if strings.Contains(html, "secret-code-html") {
		t.Fatalf("callback HTML leaked the code: %s", html)
	}
	if strings.Contains(html, "<b>request</b>") || !strings.Contains(html, "&lt;b&gt;request&lt;/b&gt;") {
		t.Fatalf("callback HTML did not escape the error: %s", html)
	}
	if callback.Header().Get("Cache-Control") != "no-store" ||
		callback.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("callback error headers = %#v", callback.Header())
	}

	poll := f.do(http.MethodPost, "/raccoon/accounts/oauth/poll",
		map[string]any{"session_id": sessionID}, "")
	pollBody := decodeRouteJSON(t, poll)
	if pollBody["status"] != "error" || pollBody["ok"] != false {
		t.Fatalf("error poll = %#v", pollBody)
	}
	if strings.Contains(poll.Body.String(), "secret-code-html") {
		t.Fatalf("poll leaked the code: %s", poll.Body)
	}
	replay := f.do(http.MethodPost, "/raccoon/accounts/oauth/callback", map[string]any{
		"session_id": sessionID, "code": "secret-code-html",
	}, "")
	if replay.Code != http.StatusConflict {
		t.Fatalf("error session replay status=%d body=%s", replay.Code, replay.Body)
	}
	if calls, _ := f.fake.exchangeSnapshot(); calls != 1 {
		t.Fatalf("error session exchanged again: %d", calls)
	}
}

func TestCreditsRefreshEnvelope(t *testing.T) {
	f := newRoutesFixture(t)
	f.importFlat(t, f.uid, f.token, nil)
	rec := f.do(http.MethodPost, "/raccoon/credits/refresh", map[string]any{}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("credits refresh status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeRouteJSON(t, rec)
	if body["ok"] != true {
		t.Fatalf("credits refresh = %#v", body)
	}
	rows, _ := body["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("credits rows = %#v", body["accounts"])
	}
	row := rows[0].(map[string]any)
	credits, _ := row["credits"].(map[string]any)
	if row["ok"] != true || row["uid"] != f.uid || routeInt(credits["available"]) != 1234 {
		t.Fatalf("credits row = %#v", row)
	}
	if !strings.Contains(routeText(body["message"]), "已刷新") {
		t.Fatalf("credits message = %v", body["message"])
	}
	unknown := f.do(http.MethodPost, "/raccoon/credits/refresh", map[string]any{"uid": "missing"}, "")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown credits uid status=%d body=%s", unknown.Code, unknown.Body)
	}
}

func TestAccountActions(t *testing.T) {
	f := newRoutesFixture(t)
	f.importFlat(t, f.uid, f.token, nil)

	action := func(path string, payload map[string]any) *httptest.ResponseRecorder {
		return f.do(http.MethodPost, path, payload, "")
	}
	if rec := action("/raccoon/accounts/disable", map[string]any{"uid": f.uid}); rec.Code != 200 {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body)
	}
	overview := decodeRouteJSON(t, f.do(http.MethodGet, "/raccoon/overview", nil, ""))
	if overview["accounts"].([]any)[0].(map[string]any)["disabled"] != true {
		t.Fatalf("overview after disable = %#v", overview["accounts"])
	}
	if rec := action("/raccoon/accounts/enable", map[string]any{"uid": f.uid}); rec.Code != 200 {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body)
	}
	overview = decodeRouteJSON(t, f.do(http.MethodGet, "/raccoon/overview", nil, ""))
	if overview["accounts"].([]any)[0].(map[string]any)["disabled"] != false {
		t.Fatalf("overview after enable = %#v", overview["accounts"])
	}

	note := decodeRouteJSON(t, action("/raccoon/accounts/note",
		map[string]any{"uid": f.uid, "note": "hello"}))
	if note["note"] != "hello" || note["uid"] != f.uid {
		t.Fatalf("note = %#v", note)
	}
	tasks := decodeRouteJSON(t, action("/raccoon/accounts/tasks",
		map[string]any{"uid": f.uid, "enabled": false}))
	if tasks["tasks_enabled"] != false {
		t.Fatalf("tasks = %#v", tasks)
	}
	tasksDefault := decodeRouteJSON(t, action("/raccoon/accounts/tasks", map[string]any{"uid": f.uid}))
	if tasksDefault["tasks_enabled"] != true {
		t.Fatalf("tasks default = %#v", tasksDefault)
	}

	account := f.p.pool.Get(f.uid)
	if err := f.p.pool.NoteError(account, "rate limited", true, false); err != nil {
		t.Fatal(err)
	}
	if !account.View().Cooling {
		t.Fatal("test setup did not set a cooldown")
	}
	cleared := decodeRouteJSON(t, action("/raccoon/accounts/clear-cooldown", map[string]any{"uid": f.uid}))
	if cleared["ok"] != true {
		t.Fatalf("clear cooldown = %#v", cleared)
	}
	if account.View().Cooling {
		t.Fatal("cooldown was not cleared")
	}

	settings, err := f.store.Merge(map[string]any{"proxy_slots": []any{
		map[string]any{
			"id": "slot-1", "name": "one",
			"url": "http://alice:hunter2@127.0.0.1:7890", "enabled": true,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = settings
	proxy := decodeRouteJSON(t, action("/raccoon/accounts/proxy",
		map[string]any{"uid": f.uid, "slot_id": "slot-1"}))
	if proxy["proxy_slot"] != "slot-1" {
		t.Fatalf("proxy = %#v", proxy)
	}
	proxyText := routeText(proxy["proxy"])
	if strings.Contains(proxyText, "hunter2") || !strings.Contains(proxyText, "<redacted>") {
		t.Fatalf("proxy was not masked: %q", proxyText)
	}
	tokenPath, _ := AuthPath(f.dir, f.uid)
	saved, err := LoadAuth(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProxySlot != "slot-1" {
		t.Fatalf("proxy slot not persisted: %#v", saved)
	}
	if direct := decodeRouteJSON(t, action("/raccoon/accounts/proxy",
		map[string]any{"uid": f.uid, "slot_id": ""})); direct["proxy_slot"] != "" {
		t.Fatalf("proxy direct = %#v", direct)
	}

	tested := decodeRouteJSON(t, action("/raccoon/accounts/test", map[string]any{"uid": f.uid}))
	if tested["ok"] != true || routeInt(tested["models"]) < 2 ||
		!strings.Contains(routeText(tested["message"]), "连接正常") {
		t.Fatalf("account test = %#v", tested)
	}
	if credits, _ := tested["credits"].(map[string]any); routeInt(credits["available"]) != 1234 {
		t.Fatalf("account test credits = %#v", tested["credits"])
	}
	if _, ok := tested["token_seconds"]; !ok {
		t.Fatalf("account test missing token_seconds: %#v", tested)
	}

	if rec := action("/raccoon/accounts/enable", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing uid status=%d", rec.Code)
	}
	if rec := action("/raccoon/accounts/proxy",
		map[string]any{"uid": "missing", "slot_id": "slot-1"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account status=%d body=%s", rec.Code, rec.Body)
	}

	deleted := decodeRouteJSON(t, action("/raccoon/accounts/delete", map[string]any{"uid": f.uid}))
	if deleted["deleted"] != true || deleted["ok"] != true {
		t.Fatalf("delete = %#v", deleted)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("auth file survived delete: %v", err)
	}
	if rec := action("/raccoon/accounts/delete", map[string]any{"uid": f.uid}); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestKeepaliveModelsRefreshAndConnectionTest(t *testing.T) {
	f := newRoutesFixtureFor(t, "pu-keep", -10*time.Second)
	f.importFlat(t, f.uid, f.token, map[string]any{"refresh_token": "refresh-" + f.uid})
	keepalive := f.do(http.MethodPost, "/raccoon/keepalive", map[string]any{}, "")
	if keepalive.Code != http.StatusOK {
		t.Fatalf("keepalive status=%d body=%s", keepalive.Code, keepalive.Body)
	}
	body := decodeRouteJSON(t, keepalive)
	if routeInt(body["refreshed"]) < 1 || routeInt(body["models"]) < 2 || body["ok"] != true {
		t.Fatalf("keepalive = %#v", body)
	}
	catalog, _ := body["catalog"].(map[string]any)
	if catalog["source"] != "raccoon" {
		t.Fatalf("keepalive catalog = %#v", catalog)
	}
	if !strings.Contains(routeText(body["message"]), "续期") {
		t.Fatalf("keepalive message = %v", body["message"])
	}

	refresh := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/models/refresh", map[string]any{}, ""))
	if refresh["ok"] != true || routeInt(refresh["models"]) < 2 {
		t.Fatalf("models refresh = %#v", refresh)
	}
	if refresh["message"] != "模型目录已刷新" {
		t.Fatalf("models refresh message = %v", refresh["message"])
	}
	catalog, _ = refresh["catalog"].(map[string]any)
	if catalog["source"] != "raccoon" {
		t.Fatalf("models refresh catalog = %#v", catalog)
	}

	connection := decodeRouteJSON(t, f.do(http.MethodPost, "/raccoon/test", map[string]any{}, ""))
	if connection["ok"] != true || routeInt(connection["models"]) < 2 ||
		connection["catalog_source"] != "raccoon" ||
		!strings.Contains(routeText(connection["message"]), "连接正常") {
		t.Fatalf("connection test = %#v", connection)
	}

	disabledFixture := newRoutesFixture(t)
	disabled := decodeRouteJSON(t, disabledFixture.do(http.MethodPost, "/raccoon/test", map[string]any{}, ""))
	if disabled["ok"] != false || routeInt(disabled["models"]) != 0 ||
		routeText(disabled["message"]) == "" {
		t.Fatalf("disabled test = %#v", disabled)
	}

	failureFixture := newRoutesFixture(t)
	failureFixture.importFlat(t, failureFixture.uid, failureFixture.token, nil)
	failureFixture.fake.mu.Lock()
	failureFixture.fake.catalogStatus = http.StatusInternalServerError
	failureFixture.fake.mu.Unlock()
	failure := failureFixture.do(http.MethodPost, "/raccoon/test", map[string]any{}, "")
	if failure.Code != http.StatusBadGateway {
		t.Fatalf("connection failure status=%d body=%s", failure.Code, failure.Body)
	}
	errObject := routeErrorBody(t, decodeRouteJSON(t, failure))
	if routeText(errObject["message"]) == "" {
		t.Fatalf("connection failure error = %#v", errObject)
	}
	failedRefresh := failureFixture.do(http.MethodPost, "/raccoon/models/refresh", map[string]any{}, "")
	if failedRefresh.Code != http.StatusOK {
		t.Fatalf("failed refresh status=%d body=%s", failedRefresh.Code, failedRefresh.Body)
	}
	failedBody := decodeRouteJSON(t, failedRefresh)
	if failedBody["ok"] != false || routeText(failedBody["message"]) == "" {
		t.Fatalf("failed refresh = %#v", failedBody)
	}
}

func TestOverviewAutoClaimLogin(t *testing.T) {
	f := newRoutesFixture(t)
	if _, err := SaveConfig(f.store, map[string]any{"auto_claim_login": true}); err != nil {
		t.Fatal(err)
	}
	f.importFlat(t, f.uid, f.token, nil)
	overview := decodeRouteJSON(t, f.do(http.MethodGet, "/raccoon/overview?force=1", nil, ""))
	if overview["auto_claim_login"] != true {
		t.Fatalf("auto_claim_login = %v", overview["auto_claim_login"])
	}
	account := overview["accounts"].([]any)[0].(map[string]any)
	credits, _ := account["credits"].(map[string]any)
	if routeInt(credits["available"]) != 7234 {
		t.Fatalf("auto claim balance = %#v", account["credits"])
	}
	desktop, mobile := 0, 0
	f.fake.points.mu.Lock()
	desktop = f.fake.points.desktopGrants
	mobile = f.fake.points.mobileGrants
	f.fake.points.mu.Unlock()
	if desktop != 1 || mobile != 1 {
		t.Fatalf("auto claim grants desktop=%d mobile=%d", desktop, mobile)
	}
}

func TestAdminErrorsNeverExposeFilesystemDetail(t *testing.T) {
	pathErr := errors.New("open /root/private/accounts/raccoon/raccoon-u.json: permission denied")
	failure := adminErrorFrom(pathErr)
	if failure.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", failure.Status)
	}
	rec := httptest.NewRecorder()
	writeAdminError(rec, failure)
	blob := rec.Body.String()
	for _, needle := range []string{"/root/private", "raccoon-u.json", "permission denied"} {
		if strings.Contains(blob, needle) {
			t.Fatalf("admin error leaked %q: %s", needle, blob)
		}
	}

	if got := redactedPointsError(pathErr, nil, maxPointsErrorRunes); strings.Contains(got, "/root/private") {
		t.Fatalf("points message leaked a path: %q", got)
	}
	apiErr := &APIError{Kind: KindUpstream, Status: 500, Message: "upstream failed"}
	if got := adminErrorFrom(apiErr).Message; got != "upstream failed" {
		t.Fatalf("API error message = %q, want unprefixed upstream message", got)
	}

	// The delete failure response has no relative filesystem hint either.
	deleteFailure := &adminError{
		Status: http.StatusInternalServerError, Type: "server_error",
		Message: "账号文件删除失败，账号仍保留；请检查账号文件权限",
	}
	rec = httptest.NewRecorder()
	writeAdminError(rec, deleteFailure)
	if strings.Contains(rec.Body.String(), "accounts/") {
		t.Fatalf("delete error leaked a data path: %s", rec.Body)
	}
}
