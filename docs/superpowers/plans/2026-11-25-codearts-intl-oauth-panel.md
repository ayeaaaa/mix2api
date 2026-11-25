# CodeArts International OAuth + Panel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add browser OAuth for the international CodeArts station and expose it as a first-class `codearts-intl` realm in the dashboard, without changing mainland behavior.

**Architecture:** Each CodeArts provider keeps its own OAuth session table. The mainland provider owns the single public `/oauth/callback` route and falls back to the international peer only when its own session table has no matching state/secret. The dashboard gains a `codearts-intl` card, account tab, OAuth/session-import entry, token-quota rendering, and a separate settings section backed by the existing `codearts-intl` settings key.

**Tech Stack:** Go 1.x, `net/http`, existing `OAuthSessions`, existing panel settings provider hooks, vanilla dashboard JS with Node contract tests.

**Spec:** `docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md`

## Global Constraints

- Mainland `/codearts` routes, settings, accounts, OAuth behavior, and check-in/benefit behavior must remain unchanged.
- International accounts live only under `accounts/codearts-intl`; settings live only under `settings["codearts-intl"]`.
- Do not put the international `security_token` into `CloudDragonToken`. International OAuth saves `SecurityToken` and leaves `CloudDragonToken` empty.
- Each provider owns an independent `OAuthSessions` table. Do not share one table between stations.
- Only the mainland provider exposes `GET /oauth/callback`; it dispatches to its international peer only when the callback does not match a mainland session.
- International OAuth start URLs must go through `https://auth.huaweicloud.com/authui/login?site=intl` with the portal authorize URL in `service=`.
- The panel must never auto-open an OAuth URL; copy/open is user-initiated.
- No new dependencies.
- Never return, log, or render authorization codes, refresh tokens, AK/SK, security tokens, PKCE verifiers, or DPoP private keys.
- `codearts-intl` must be an accepted realm for `/realm`, API-key bindings, and dashboard realm labels.

## Review Focus

- An international OAuth state/ticket arriving at `/oauth/callback` must complete against the international provider, not be misread as mainland or dropped.
- A mainland callback must never fall through to the international provider after a real state/code error.
- International OAuth must not write `accounts/codearts` or set `CloudDragonToken`.
- Enabling `codearts-intl` in settings must make the realm available immediately without restarting the server.
- Session JSON import must accept the international session shape (including an absent `stationType`) only on the international route.
- The token quota must render as tokens, not credits, and no check-in or benefit action should appear for `codearts-intl`.
- A saved API key bound to `codearts-intl` must not be normalized back to `gateway`/empty.

---

### Task 1: International OAuth flow and callback dispatch

**Files:**
- Modify: `internal/providers/codearts/oauth.go`
- Modify: `internal/providers/codearts/provider.go`
- Modify: `internal/providers/codearts/routes.go`
- Modify: `internal/app/providers.go`
- Test: `internal/providers/codearts/oauth_test.go`
- Test: `internal/providers/codearts/routes_test.go`

**Interfaces:**
- Consumes: existing `OAuthSessions.Begin(store, port)`, `LoginURL(authorizeURL)`, `saveOAuthAuth`, `Provider.oauthCallback`, `Provider.SetListenerPort`.
- Produces: `func (p *Provider) SetOAuthPeer(peer *Provider)`; `func (p *Provider) oauthCallbackGET(query url.Values) (int, string)` remains the public internal entry point; `func (p *Provider) oauthCallbackGETOnce(query url.Values) (int, string, bool)` reports whether the callback matched this provider.

- [ ] **Step 1: Write failing OAuth tests**

Add to `internal/providers/codearts/oauth_test.go`:

```go
func TestOAuthBeginInternationalUsesLoginWrapper(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	sessions := NewOAuthSessionsForStation(International)
	start, err := sessions.Begin(store, 54321)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := url.Parse(start.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if outer.Host != "auth.huaweicloud.com" || outer.Query().Get("site") != "intl" {
		t.Fatalf("outer login URL = %s", start.AuthURL)
	}
	service, err := url.Parse(outer.Query().Get("service"))
	if err != nil {
		t.Fatal(err)
	}
	if service.Host != "codearts.ap-southeast-1.huaweicloud.com" ||
		!strings.HasSuffix(service.Path, "/portal/authorize") {
		t.Fatalf("service URL = %s", service.String())
	}
	if service.Query().Get("locale") != "en-us" {
		t.Fatalf("service locale = %s", service.Query().Get("locale"))
	}
}
```

Extend `TestOAuthSaveUsesStationAccountPath` with:

```go
	if auth.CloudDragonToken != "" {
		t.Fatalf("international OAuth wrote cloud_dragon_token = %q", auth.CloudDragonToken)
	}
```

Update `TestCodeArtsIntlRoutesUsePrefix` so it still rejects `/oauth/callback` but accepts the three `/codearts-intl/accounts/oauth/*` admin routes.

Add to `internal/providers/codearts/routes_test.go` a two-provider browser-callback test:

```go
func TestCodeArtsIntlOAuthBrowserCallbackViaMainlandPeer(t *testing.T) {
	fake, server := newOAuthFake(t)

	intl := International
	intl.STS = server.URL
	intl.SnapHost = server.URL
	intl.Portal = "https://portal.example.test/portal"
	intl.LoginURL = "https://auth.example.test/authui/login?site=intl"

	store := newTestStore(t)
	mainland, err := NewWithStation(store, Mainland)
	if err != nil {
		t.Fatal(err)
	}
	intlProvider, err := NewWithStation(store, intl)
	if err != nil {
		t.Fatal(err)
	}
	mainland.SetListenerPort(54321)
	intlProvider.SetListenerPort(54321)
	mainland.SetOAuthPeer(intlProvider)
	mux := http.NewServeMux()
	api.RegisterProviderRoutes(mux, mainland)
	api.RegisterProviderRoutes(mux, intlProvider)

	startReq := httptest.NewRequest(http.MethodPost, "/codearts-intl/accounts/oauth/start", strings.NewReader(`{}`))
	startReq.Header.Set("Content-Type", "application/json")
	startRec := httptest.NewRecorder()
	mux.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("intl start status=%d body=%s", startRec.Code, startRec.Body)
	}
	start := decodeRoutesJSON(t, startRec)
	outer, err := url.Parse(routesText(start["auth_url"]))
	if err != nil {
		t.Fatal(err)
	}
	service, err := url.Parse(outer.Query().Get("service"))
	if err != nil {
		t.Fatal(err)
	}
	ticket := service.Query().Get("ticket_id")
	if ticket == "" {
		t.Fatalf("intl start has no ticket: %s", startRec.Body)
	}

	callbackReq := httptest.NewRequest(http.MethodGet,
		"/oauth/callback?code=oauth-code&ticket_id="+url.QueryEscape(ticket), nil)
	callbackRec := httptest.NewRecorder()
	mux.ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusOK || !strings.Contains(callbackRec.Body.String(), "登录成功") {
		t.Fatalf("intl browser callback status=%d body=%s", callbackRec.Code, callbackRec.Body)
	}
	if _, err := LoadAuthForStation(filepath.Join(filepath.Dir(store.Path), "codearts-intl", "codearts-u1.json"), intl); err != nil {
		t.Fatalf("intl auth file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Path), "codearts", "codearts-u1.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mainland auth file was written: %v", err)
	}
}
```

- [ ] **Step 2: Run tests and verify the expected failures**

Run: `go test ./internal/providers/codearts -run 'TestOAuthBeginInternationalUsesLoginWrapper|TestOAuthSaveUsesStationAccountPath|TestCodeArtsIntlOAuthBrowserCallbackViaMainlandPeer' -count=1`

Expected: FAIL because `Begin` returns the raw portal URL, intl OAuth sets `CloudDragonToken`, and no OAuth routes/peer dispatch exist.

- [ ] **Step 3: Implement the station-aware OAuth start and save**

In `internal/providers/codearts/oauth.go`:

- In `Begin`, wrap `authURL` with `s.LoginURL(authURL)` before assigning it to `session.AuthURL` and `OAuthStart.AuthURL`.
- In `saveOAuthAuth`, replace the unconditional `CloudDragonToken` assignment with:
  - mainland: `auth.CloudDragonToken = token.Credentials.SecurityToken`
  - non-mainland: `auth.Station = s.Station().ID` and `auth.SecurityToken = token.Credentials.SecurityToken`, leaving `CloudDragonToken` empty.

In `internal/providers/codearts/provider.go`, add:

```go
// oauthPeer lets the single public callback route look up a session owned by
// the sibling station. The peer is set once during app composition.
oauthPeer *Provider

// SetOAuthPeer assigns the sibling provider used for public callback dispatch.
func (p *Provider) SetOAuthPeer(peer *Provider) {
	if p == nil {
		return
	}
	p.oauthPeer = peer
}
```

In `internal/app/providers.go`, after both providers are registered:

```go
codeartsProvider.SetOAuthPeer(codeartsIntlProvider)
```

- [ ] **Step 4: Implement intl OAuth admin routes and peer callback dispatch**

In `internal/providers/codearts/routes.go`:

- Register `oauth/start`, `oauth/poll`, and `oauth/callback` for every station under `prefix`.
- Register `GET /oauth/callback` only when `p.Station().ID == Mainland.ID`.
- Refactor `oauthCallbackGET` into:

```go
func (p *Provider) oauthCallbackGET(query url.Values) (int, string) {
	status, body, handled := p.oauthCallbackGETOnce(query)
	if !handled && p.oauthPeer != nil && p.oauthPeer != p {
		status, body, handled = p.oauthPeer.oauthCallbackGETOnce(query)
	}
	if !handled {
		return http.StatusBadRequest, callbackHTML("登录会话不存在或已过期，请回到看板重新发起。")
	}
	return status, body
}

func (p *Provider) oauthCallbackGETOnce(query url.Values) (int, string, bool) {
	// Existing body: parse ticket_id/secret/state, build loopback callbackURL,
	// call p.oauthCallback, render success/continue HTML.
	// Return handled=false only when the failure is oauthSessionMissingMessage.
	// Missing identifiers and all other failures return handled=true with the
	// existing human-readable error.
}
```

Do not recurse through `oauthCallbackGET` from the peer; call `oauthCallbackGETOnce` directly.

- [ ] **Step 5: Run the focused OAuth tests**

Run: `go test ./internal/providers/codearts -run 'TestOAuth|TestCodeArtsIntlOAuth|TestCodeArtsOAuth' -count=1`

Expected: PASS. Confirm no secrets appear in callback HTML or callback JSON.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/codearts/oauth.go internal/providers/codearts/provider.go internal/providers/codearts/routes.go internal/providers/codearts/oauth_test.go internal/providers/codearts/routes_test.go internal/app/providers.go
git commit -m "feat(codearts): add international oauth callback routing"
```

---

### Task 2: Register `codearts-intl` as a realm and API-key binding

**Files:**
- Modify: `internal/platform/settings/realm.go`
- Modify: `internal/platform/settings/service.go`
- Modify: `internal/platform/keys/store.go`
- Test: `internal/platform/settings/settings_test.go`
- Test: `internal/platform/keys/store_test.go`

**Interfaces:**
- Consumes: existing `RealmService.Options`, `RealmService.Set`, `Service.realmAllowed`, `cleanEntry`.
- Produces: `codearts-intl` accepted by all three normalization paths.

- [ ] **Step 1: Write failing realm and key-store tests**

Add to `internal/platform/settings/settings_test.go`:

```go
func TestRealmOptionsIncludeCodeartsIntl(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	workbuddy := &settingsFakeProvider{realm: "intl", active: "intl"}
	codeartsIntl := &settingsRealmOnlyProvider{realm: "codearts-intl", available: true}
	service := NewRealm(store, dir, []providers.Provider{workbuddy, codeartsIntl})
	options := service.Options()
	want := []string{"intl", "cn", "codearts-intl"}
	if len(options) != len(want) {
		t.Fatalf("options = %#v, want %#v", options, want)
	}
	for i := range want {
		if options[i] != want[i] {
			t.Fatalf("options = %#v, want %#v", options, want)
		}
	}
	if err := service.Set(context.Background(), "codearts-intl"); err != nil {
		t.Fatal(err)
	}
	if got := service.Active(context.Background()); got != "codearts-intl" {
		t.Fatalf("active realm = %q", got)
	}
}
```

Add to `internal/platform/keys/store_test.go`:

```go
func TestKeyStoreAcceptsCodeartsIntlRealm(t *testing.T) {
	store, _ := testStore(t)
	if _, err := store.Save([]Entry{{
		ID: "intl-codearts", Name: "intl-codearts", Key: "key-intl-codearts",
		Realm: "codearts-intl",
	}}, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Realm != "codearts-intl" {
		t.Fatalf("entries = %#v", entries)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/platform/settings ./internal/platform/keys -run 'TestRealmOptionsIncludeCodeartsIntl|TestKeyStoreAcceptsCodeartsIntlRealm' -count=1`

Expected: FAIL because both normalizers drop `codearts-intl`.

- [ ] **Step 3: Add the realm cases**

- In `internal/platform/settings/realm.go`, add `case "codearts-intl": return "codearts-intl"` to `normalizeRealm`.
- In `internal/platform/keys/store.go`, add `"codearts-intl"` to the allowed realm switch in `cleanEntry`.
- In `internal/platform/settings/service.go`:
  - add `"codearts-intl"` to `realmAllowed`;
  - add a `realm == "codearts-intl"` provider-registration check parallel to `codearts`;
  - update the save error message to include `codearts-intl`.

- [ ] **Step 4: Run the focused tests**

Run: `go test ./internal/platform/settings ./internal/platform/keys -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/settings/realm.go internal/platform/settings/service.go internal/platform/keys/store.go internal/platform/settings/settings_test.go internal/platform/keys/store_test.go
git commit -m "feat(codearts): register international realm for panel bindings"
```

---

### Task 3: Panel CodeArts international card, account view, and token quota

**Files:**
- Modify: `internal/web/dashboard.html`
- Create: `tests/dashboard/node/_test_codearts_intl_panel.js`
- Modify: `tests/dashboard/contract_test.go`

**Interfaces:**
- Consumes: backend routes `/codearts-intl/overview`, `/codearts-intl/accounts/*`, `/codearts-intl/test`; `window.REALM_OPTIONS`.
- Produces: dashboard functions `codeartsRealm()`, `codeartsEndpoint()`, `loadCodeartsOverviewFor(realm, force)`, `codeartsQuotaLabel(overview)`; `window.CODEARTS_OVERVIEWS` keyed by realm.

- [ ] **Step 1: Write the failing dashboard contract test**

Create `tests/dashboard/node/_test_codearts_intl_panel.js`:

```js
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

for(const id of ['tabCodeartsIntl', 'accountsRealmCodeartsIntl',
                 'tabCodeartsIntlHealthy', 'tabCodeartsIntlCredits']){
  assert.ok(html.includes('id="' + id + '"'), 'dashboard missing #' + id);
}
assert.ok(html.includes("switchViewRealm('codearts-intl')"), 'international tab is not wired');
assert.ok(script.includes('function codeartsEndpoint'), 'missing codeartsEndpoint');
assert.ok(script.includes("/codearts-intl/overview"), 'missing international overview request');
assert.ok(script.includes('CODEARTS_OVERVIEWS'), 'missing per-realm overview map');

const calls = [];
const intlOverview = {
  service: 'codearts-intl', region: 'intl', online: true,
  stats: {total: 1, healthy: 1, cooling: 0, disabled: 0, credits: 4958830},
  models: [{id: 'glm-5.3'}],
  accounts: [{uid: 'intl-1', nickname: 'Intl', enabled: true,
              credits: {remain: 4958830, used: 41170, unit: 'token', package: '体验版'},
              active_concurrent: 0, max_concurrent: 1}],
};
const page = loadDashboard({
  fetch: (url, opts) => {
    const target = String(url);
    calls.push(target);
    let payload = {};
    if(target.indexOf('/codearts-intl/overview') === 0) payload = intlOverview;
    if(target.indexOf('/realm') === 0) payload = {current: 'codearts-intl', options: ['intl', 'cn', 'codearts', 'codearts-intl']};
    return Promise.resolve({status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload))});
  },
});
page.REALM_OPTIONS = ['intl', 'cn', 'codearts', 'codearts-intl'];
page.VIEW_REALM = 'codearts-intl';
page.CODEARTS_OVERVIEWS = {'codearts-intl': intlOverview};
page.updateUI();
assert.equal(dom.byId('tabCodeartsIntl').style.display, '', 'intl tab hidden');
assert.equal(dom.byId('accountsRealmCodeartsIntl').style.display, '', 'intl account tab hidden');
assert.equal(dom.byId('codeartsAccountsSection').style.display, '', 'shared account section hidden');
assert.ok(/Token/.test(dom.byId('tabCodeartsIntlCredits').textContent || ''));
```

Also add the new test to the `tests/dashboard/contract_test.go` list of node test filenames.

- [ ] **Step 2: Run the test to verify failure**

Run: `go test ./tests/dashboard -run TestDashboardNodeHarness -count=1`

Expected: FAIL because the IDs and `codeartsEndpoint` helper do not exist.

- [ ] **Step 3: Add the top card and account tab HTML**

In `internal/web/dashboard.html`:

- Add `tabCodeartsIntl` after `tabCodearts`, with `onclick="switchViewRealm('codearts-intl')"`, display none, host `ap-southeast-1`, default/view/offline badges, and IDs `tabCodeartsIntlHealthy`, `tabCodeartsIntlTotal`, `tabCodeartsIntlModels`, `tabCodeartsIntlCredits`, `tabCodeartsIntlKeyBadge`.
- Add `<button id="accountsRealmCodeartsIntl" ... onclick="switchViewRealm('codearts-intl')">CodeArts 国际</button>` to `accountsRealmSwitch`, display none.
- Add an international settings section later in Task 5; this task only adds the IDs needed by the card/account view.

- [ ] **Step 4: Add realm helpers and overview map**

In the dashboard script near `window.CODEARTS_OVERVIEW`:

```js
window.CODEARTS_OVERVIEWS = {};
function codeartsRealm(){
  return window.VIEW_REALM === 'codearts-intl' ? 'codearts-intl' : 'codearts';
}
function codeartsEndpoint(){
  return codeartsRealm() === 'codearts-intl' ? '/codearts-intl' : '/codearts';
}
function codeartsOverviewFor(realm){
  return (window.CODEARTS_OVERVIEWS || {})[realm] || null;
}
function codeartsQuotaLabel(overview){
  const accounts = (overview && overview.accounts) || [];
  const token = accounts.some(a => a && a.credits && a.credits.unit === 'token');
  return token ? 'Token' : '积分';
}
```

Change `loadCodeartsOverview(force)` to delegate to:

```js
async function loadCodeartsOverviewFor(realm, force){
  const endpoint = realm === 'codearts-intl' ? '/codearts-intl' : '/codearts';
  const r = await getJSON(endpoint + '/overview' + (force ? '?force=1' : ''));
  window.CODEARTS_OVERVIEWS[realm] = r;
  if(window.VIEW_REALM === realm) window.CODEARTS_OVERVIEW = r;
  if(window.VIEW_REALM === realm){
    renderCodeartsCards(r);
    renderCodeartsAccounts(r);
  }
  window.updateUI();
}
async function loadCodeartsOverview(force){
  return loadCodeartsOverviewFor(codeartsRealm(), force);
}
```

On error, clear the matching overview map entry and render the error only when the failed realm is the current view.

- [ ] **Step 5: Update `updateUI`, `initRealm`, and `switchViewRealm` for the second CodeArts realm**

- Add `caIntlEnabled = (window.REALM_OPTIONS || []).includes("codearts-intl")`.
- Accept `codearts-intl` in `validView`, gateway validity, and `initRealm`'s URL view list.
- Treat `VIEW_REALM === 'codearts' || 'codearts-intl'` as a CodeArts view for showing the shared account section and hiding WorkBuddy sections.
- Set `tabCodeartsIntl` display/active state, account tab state, default badge, view badge, key count, and card stats from `codeartsOverviewFor('codearts-intl')`.
- Add labels `CodeArts 国际` / `（CodeArts 国际）` to realm names and section tags.
- In `switchViewRealm`, handle both CodeArts realms by calling `loadCodeartsOverview(true)`, `loadModels()`, `refreshAnalyticsScopeData()`, and `refresh()`.
- At boot, if both CodeArts options are present, kick off `loadCodeartsOverviewFor` for each so both cards can show counts.

- [ ] **Step 6: Parameterize CodeArts account endpoints, cards, and actions**

- Replace hardcoded `/codearts/overview`, `/codearts/accounts/*`, `/codearts/credits/refresh`, `/codearts/keepalive`, and `/codearts/test` in the CodeArts functions with `codeartsEndpoint()` + the suffix.
- In `renderCodeartsCards`, use `codeartsQuotaLabel(r)` for the quota card label.
- In `renderCodeartsAccounts`, use the current realm to:
  - label the quota column `tokens` for international;
  - omit the check-in column and check-in action for international;
  - omit the tasks action for international;
  - keep enable/disable, relogin, clear-cooldown, test, note, proxy, and delete.
- Add IDs to the toolbar check-in and credits buttons and hide/relabel them for international.
- Make `codeartsRelogin`, `codeartsEditNote`, and `openCodeartsConsole` use the current realm’s overview/endpoint.

- [ ] **Step 7: Run the dashboard test**

Run: `go test ./tests/dashboard -run TestDashboardNodeHarness -count=1`

Expected: PASS for the new test and all existing dashboard tests.

- [ ] **Step 8: Commit**

```bash
git add internal/web/dashboard.html tests/dashboard/node/_test_codearts_intl_panel.js tests/dashboard/contract_test.go
git commit -m "feat(codearts): add international realm to panel"
```

---

### Task 4: International OAuth modal and session JSON import

**Files:**
- Modify: `internal/web/dashboard.html`
- Modify: `tests/dashboard/node/_test_codearts_oauth_ui.js`

**Interfaces:**
- Consumes: `codeartsEndpoint()` from Task 3; backend OAuth admin routes and `POST /codearts-intl/accounts/import`.
- Produces: `importCodeartsSessionJson(btn)`, dynamic OAuth modal title/help, and realm-aware OAuth endpoints.

- [ ] **Step 1: Write failing OAuth UI assertions**

Extend `tests/dashboard/node/_test_codearts_oauth_ui.js`:

```js
assert.ok(html.includes('id="codeartsSessionJson"'), 'missing session JSON import field');
assert.ok(script.includes('function importCodeartsSessionJson'), 'missing session JSON import handler');
assert.ok(script.includes('codeartsEndpoint() + "/accounts/oauth/start"'), 'OAuth start is not realm-aware');
assert.ok(script.includes('codeartsEndpoint() + "/accounts/oauth/poll"'), 'OAuth poll is not realm-aware');
assert.ok(script.includes('codeartsEndpoint() + "/accounts/oauth/callback"'), 'OAuth callback is not realm-aware');
assert.ok(script.includes("codearts-intl"), 'OAuth UI has no international branch');
```

- [ ] **Step 2: Run the test to verify failure**

Run: `go test ./tests/dashboard -run TestDashboardNodeHarness -count=1`

Expected: FAIL on the missing session JSON field and realm-aware endpoints.

- [ ] **Step 3: Add the session JSON field and dynamic OAuth copy**

In `internal/web/dashboard.html`:

- Add a textarea `codeartsSessionJson` and button `导入 session JSON` to the existing `codeartsOauthModal`.
- Add a short help line explaining that the field accepts the CodeArts Agent session JSON and that international sessions may omit `stationType`.
- Give the modal heading an ID `codeartsOauthTitle` and description an ID `codeartsOauthHelp`.
- `openCodeartsOauthModal` must set the title to `添加 CodeArts 账号` or `添加 CodeArts 国际账号` based on `codeartsRealm()`, and update the help text.

- [ ] **Step 4: Make all OAuth calls realm-aware and add JSON import**

- `startCodeartsOAuth`: POST `codeartsEndpoint() + "/accounts/oauth/start"`.
- `pollCodeartsOAuth`: POST `codeartsEndpoint() + "/accounts/oauth/poll"`.
- `importCodeartsCallback`: POST `codeartsEndpoint() + "/accounts/oauth/callback"`.
- Add `async function importCodeartsSessionJson(btn)`:
  - parse the textarea value with `JSON.parse`;
  - on parse failure set `codeartsOauthStatus` and return;
  - POST `codeartsEndpoint() + "/accounts/import"` with `{session_json: parsed}`;
  - clear the textarea on success;
  - set a status containing the returned UID;
  - call `refreshActiveRealm()` and `loadCodeartsOverview(true)`;
  - close the modal after a short delay only on success.
- Ensure the raw JSON is never echoed into the status, toast, or console.

- [ ] **Step 5: Run dashboard tests**

Run: `go test ./tests/dashboard -run TestDashboardNodeHarness -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/dashboard.html tests/dashboard/node/_test_codearts_oauth_ui.js
git commit -m "feat(codearts): add international oauth and session import ui"
```

---

### Task 5: International settings and API-key labels

**Files:**
- Modify: `internal/web/dashboard.html`
- Create: `tests/dashboard/node/_test_codearts_intl_settings.js`
- Modify: `tests/dashboard/contract_test.go`
- Test: existing dashboard settings tests and key catalog tests

**Interfaces:**
- Consumes: `/settings` payload section `codearts-intl` from `Provider.SettingsView`; `/settings/save` patch key `codearts-intl`; `/codearts-intl/test`.
- Produces: `renderCodeartsIntlSettings(cfg)`, `saveCodeartsIntlSettings(btn)`, `testCodeartsIntlConnection(btn)`, and `codearts-intl` realm labels in key/model views.

- [ ] **Step 1: Write failing settings tests**

Create `tests/dashboard/node/_test_codearts_intl_settings.js`:

```js
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();
for(const id of ['codeartsIntlSettingsSection', 'setCodeartsIntlEnabled',
                 'setCodeartsIntlMaxConcurrent', 'setCodeartsIntlState']){
  assert.ok(html.includes('id="' + id + '"'), 'dashboard missing #' + id);
}
assert.ok(script.includes('function saveCodeartsIntlSettings'), 'missing international settings save');
assert.ok(script.includes('function renderCodeartsIntlSettings'), 'missing international settings render');
assert.ok(script.includes('codearts-intl'), 'missing international realm labels');

const posts = [];
const page = loadDashboard({
  fetch: (url, opts) => {
    const body = opts && opts.body ? JSON.parse(String(opts.body)) : null;
    if(opts && opts.method === 'POST') posts.push({url: String(url), body: body});
    let payload = {current: 'intl', options: ['intl', 'cn', 'codearts-intl']};
    if(String(url).indexOf('/settings/save') === 0){
      payload = {'codearts-intl': Object.assign({enabled: true, accounts: 1, healthy: 1}, body && body['codearts-intl'])};
    }
    return Promise.resolve({status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload))});
  },
});
(async () => {
  page.renderCodeartsIntlSettings({
    enabled: true, max_concurrent: 1, timeout_seconds: 30,
    stream_timeout_seconds: 600, models_ttl_seconds: 300,
    accounts: 1, healthy: 1,
  });
  assert.equal(dom.byId('setCodeartsIntlEnabled').checked, true);
  assert.equal(dom.byId('setCodeartsIntlState').textContent, '(已启用)');
  dom.byId('setCodeartsIntlMaxConcurrent').value = '2';
  await page.saveCodeartsIntlSettings();
  const saved = posts.filter(p => p.url === '/settings/save').pop();
  assert.ok(saved && saved.body['codearts-intl'], 'international settings not saved under codearts-intl');
  assert.equal(saved.body['codearts-intl'].max_concurrent, 2);
})().catch(err => {
  console.error(err.stack || err);
  process.exit(1);
});
```

Add the new filename to `tests/dashboard/contract_test.go`.

- [ ] **Step 2: Run the test to verify failure**

Run: `go test ./tests/dashboard -run TestDashboardNodeHarness -count=1`

Expected: FAIL on the missing settings section and functions.

- [ ] **Step 3: Add the international settings section**

In `internal/web/dashboard.html`, after the mainland CodeArts settings section:

- Add `<section data-cat="network" id="codeartsIntlSettingsSection">`.
- Heading `CodeArts 国际接入` with `id="setCodeartsIntlState"`.
- Description: `site=intl` OAuth, token quota, no check-in/benefit.
- Inputs: `setCodeartsIntlEnabled` checkbox, `setCodeartsIntlMaxConcurrent`, `setCodeartsIntlTimeout`, `setCodeartsIntlStreamTimeout`, `setCodeartsIntlModelsTtl`.
- Buttons `saveCodeartsIntlSettings(this)` and `testCodeartsIntlConnection(this)`.
- Hint `setCodeartsIntlHint`.

- [ ] **Step 4: Add render/save/test functions and load-time wiring**

- `renderCodeartsIntlSettings(cfg)` mirrors `renderCodeartsSettings` for the common fields, sets `setCodeartsIntlState`, and shows `accounts/healthy` plus `token` quota when available.
- `saveCodeartsIntlSettings(btn)` posts `{ "codearts-intl": payload }` with `enabled`, `max_concurrent`, `timeout_seconds`, `stream_timeout_seconds`, `models_ttl_seconds`; re-renders from `r["codearts-intl"]`; calls `refreshActiveRealm()` and `updateUI()`.
- `testCodeartsIntlConnection(btn)` posts `/codearts-intl/test`.
- In the settings load handler, call `renderCodeartsIntlSettings(data["codearts-intl"])`.

- [ ] **Step 5: Add international realm labels to key/model views**

- Add `codearts-intl` to `KEY_MODEL_REALMS`.
- Add a `keyRealmCell` branch rendering `CodeArts 国际`.
- Add `codearts-intl` to `analyticsRealmValue`, `analyticsScopeLabel`, `realmLabel`, `nextRealmLabel`, and the growth-task hide condition.
- Ensure realm badges/labels distinguish `CodeArts 国内` and `CodeArts 国际`.

- [ ] **Step 6: Run dashboard tests and the settings package**

Run: `go test ./tests/dashboard ./internal/platform/settings ./internal/api -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web/dashboard.html tests/dashboard/node/_test_codearts_intl_settings.js tests/dashboard/contract_test.go
git commit -m "feat(codearts): add international settings and key labels"
```

---

### Task 6: Full regression, docs, and live verification

**Files:**
- Modify: `docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md`
- Test: full Go suite, dashboard Node harness, race tests

**Interfaces:**
- Consumes: all prior tasks.
- Produces: verified branch and updated status notes.

- [ ] **Step 1: Run the focused package tests**

Run: `go test ./internal/providers/codearts ./internal/platform/settings ./internal/platform/keys ./internal/api ./tests/dashboard -count=1`

Expected: PASS.

- [ ] **Step 2: Run the full suite and vet**

Run: `go test ./... -count=1`

Run: `go vet ./...`

Expected: PASS, no new failures.

- [ ] **Step 3: Run race tests for the touched packages**

Run: `go test -race ./internal/providers/codearts ./internal/platform/settings ./internal/platform/keys ./internal/api -count=1`

Expected: PASS.

- [ ] **Step 4: Update the design/status document**

In `docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md`:

- Mark Phase 5 international OAuth as implemented.
- Record that the public callback is mainland-owned with peer dispatch.
- Record that international OAuth stores `SecurityToken`, not `CloudDragonToken`.
- Record that the panel has separate international card/account/settings views with token quota labels.
- Keep the live browser-login approval as a manual verification note if no human completes it in this session.

- [ ] **Step 5: Live smoke if a fresh session/browser is available**

1. Build the branch binary.
2. Start a temporary server with the `codearts-intl` realm enabled.
3. Start OAuth via `POST /codearts-intl/accounts/oauth/start`.
4. Confirm the returned URL host is `auth.huaweicloud.com` and `site=intl`.
5. Complete the browser login or paste a fresh callback into the panel.
6. Confirm the account lands in `accounts/codearts-intl`, `/v1/models?realm=codearts-intl` returns the four models, and `/codearts-intl/overview` shows the token package.

If a fresh browser session is unavailable, record the live OAuth step as blocked and rely on the fake-STS callback test plus the already-passing session-import/chat live smoke.

- [ ] **Step 6: Commit the docs and verification notes**

```bash
git add docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md
git commit -m "docs(codearts): record international oauth and panel status"
```

---

## Plan Self-Review

- **Spec coverage:** Phase 5 OAuth (`site=intl`, callback URL paste) is Task 1/4. Phase 4 panel (two realms, hidden domestic features, token unit, session JSON import) is Task 3/5. API-key realm behavior is Task 2/5.
- **Isolation:** Each provider still owns its own `OAuthSessions`; no shared table is introduced.
- **Security:** Callback peer dispatch only occurs when the primary provider has no matching session; all other errors remain terminal. Intl OAuth writes no `CloudDragonToken`.
- **Panel behavior:** The international card is only visible when `REALM_OPTIONS` contains `codearts-intl`, which the backend only reports when the realm is enabled.
- **Live boundary:** The fake-STS tests prove routing and persistence. A real browser consent cannot be fabricated; Task 6 records it as manual/blocked if unavailable.