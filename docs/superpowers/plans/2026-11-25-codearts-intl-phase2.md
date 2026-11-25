# CodeArts International Realm (Phase 2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Register a `codearts-intl` provider, support station-aware account persistence and CodeArts Agent session-JSON import, and prove the international catalog/chat path against fake upstreams.

**Architecture:** Phase 1 already added `Station`, separate settings/account folders, station-aware OAuth/chat/catalog/usage. Phase 2 adds the missing station-aware Auth lifecycle and import path, route prefixes, and app registration. International OAuth remains deferred; import is the only intl credential path in this phase.

**Tech Stack:** Go 1.x, existing mix2api provider/core packages, standard `testing`.

**Spec:** `docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md`

## Global Constraints

- Keep mainland `codearts` behavior, routes, settings key, account directory, API-key binding, and tests unchanged.
- Do not register an international `/oauth/callback` route in this phase; intl OAuth is deferred.
- `refresh_token` is one-time; no second refresher may be introduced.
- `Auth` documents without a `station` field mean mainland.
- International accounts must not load into the mainland pool, and vice versa.
- No secrets in code, tests, logs, or docs.
- Every task ends with its named test command passing.

## Review Focus

- Existing mainland auth files without `station` must still load and save without churn.
- International auth with only `security_token` must not be rejected for missing `cloud_dragon_token`.
- A `session_json` import must resolve a UID before writing; a wrong UID can collide with another account.
- International route registration must not collide with mainland `/codearts/...` or the global `/oauth/callback`.
- International catalog/chat must never inherit mainland benefit headers or model seeds.

---

### Task 1: Station-aware Auth lifecycle

**Files:**
- Modify: `internal/providers/codearts/auth.go`
- Modify: `internal/providers/codearts/pool.go`
- Modify: `internal/providers/codearts/routes.go`
- Modify: `internal/providers/codearts/oauth.go`
- Modify: `internal/providers/codearts/token.go`
- Test: `internal/providers/codearts/auth_test.go`, `internal/providers/codearts/pool_test.go`

**Interfaces:**
- Produces: `Auth.Station string`, `LoadAuthForStation(path string, station Station) (Auth, error)`, `SaveAuthForStation(path string, auth Auth, station Station) error`, `verifyAuthOwnershipForStation(path, uid string, station Station) error`.
- Keeps: `LoadAuth`, `SaveAuth`, `verifyAuthOwnership` as mainland wrappers.

- [ ] **Step 1: Write failing tests**

```go
func TestAuthStationLifecycle(t *testing.T) {
    intlAuth := Auth{UserID: "u1", SecurityToken: "ST", Station: "international"}
    path := filepath.Join(t.TempDir(), "codearts-u1.json")
    if err := SaveAuthForStation(path, intlAuth, International); err != nil { t.Fatal(err) }
    got, err := LoadAuthForStation(path, International)
    if err != nil || got.Station != "international" || got.SecurityToken != "ST" { t.Fatalf("got=%#v err=%v", got, err) }
    if _, err := LoadAuthForStation(path, Mainland); err == nil { t.Fatal("mainland loaded international auth") }
    if err := SaveAuthForStation(path, Auth{UserID:"u2"}, International); err == nil { t.Fatal("intl save accepted no signing token") }
}
```

Add a pool test:
```go
func TestPoolAddRejectsWrongStation(t *testing.T) {
    dir := t.TempDir()
    pool, _ := NewPoolForStation(dir, International, defaultConfig())
    _, err := pool.Add(Auth{UserID:"u1", SecurityToken:"ST", Station:"mainland"})
    if err == nil { t.Fatal("intl pool accepted mainland auth") }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/codearts -run 'TestAuthStationLifecycle|TestPoolAddRejectsWrongStation' -v`
Expected: FAIL because the station-aware functions/field are missing.

- [ ] **Step 3: Implement**

- Add `Station string json:"station,omitempty"`; add it to known keys, MarshalJSON (skip empty), and UnmarshalJSON.
- Add `authStation(auth)` (empty -> `"mainland"`), `authBelongsToStation`, `authHasSigningToken` helpers.
- `LoadAuthForStation` decodes, requires `UserID` and a signing token, then rejects a station mismatch.
- `LoadAuth` calls `LoadAuthForStation(..., Mainland)`.
- `SaveAuthForStation` requires `UserID`; mainland requires `CloudDragonToken`, international requires `SigningSecurityToken()` **and an explicit `Station=international`**; rejects a blank or mismatched station. Do not stamp a blank station: legacy mainland docs must remain stationless.
- `verifyAuthOwnershipForStation` uses `LoadAuthForStation`; keep wrapper.
- Pool `loadAuthFiles`/`Add`/`addNewAccount`/reimport use station-aware load/save/verify and reject wrong station.
- OAuth save and token refresh use the station-aware path/verify; refresh preserves `Station`.
- `authFromRefreshToken` keeps `Station` (already copies), and `sameCredentialIdentity` includes `Station`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestAuth|TestPool|TestToken|TestOAuth' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/auth.go internal/providers/codearts/pool.go internal/providers/codearts/routes.go internal/providers/codearts/oauth.go internal/providers/codearts/token.go internal/providers/codearts/auth_test.go internal/providers/codearts/pool_test.go
git commit -m "feat(codearts): station-aware auth lifecycle"
```

### Task 2: CodeArts Agent session-JSON import

**Files:**
- Create: `internal/providers/codearts/session_import.go`
- Modify: `internal/providers/codearts/routes.go`
- Test: `internal/providers/codearts/session_import_test.go`, `internal/providers/codearts/routes_test.go`

**Interfaces:**
- Produces: `authFromSessionJSON(payload map[string]any, station Station) (Auth, *adminError)`.
- Import route accepts `session_json` as an object or a JSON string.

- [ ] **Step 1: Write failing tests**

```go
func TestSessionJSONImportMapsIntlFields(t *testing.T) {
    payload := map[string]any{
        "account": map[string]any{"id":"acct-1","label":"u1"},
        "accessKey":"AK","secretKey":"SK","securityToken":"ST",
        "refresh_token":"refresh", "domainId":"domain",
        "expiresAt":"2026-12-01T00:00:00Z",
        "loginContext": map[string]any{
            "pkcePair": map[string]any{"codeVerifier":"verifier"},
            "dpopKeyPair": map[string]any{"privateKeyJwk": map[string]any{"kty":"EC"}},
        },
        "stationType":"international",
    }
    auth, failure := authFromSessionJSON(payload, International)
    if failure != nil { t.Fatalf("failure=%v", failure) }
    if auth.UserID != "acct-1" || auth.SecurityToken != "ST" || auth.Station != "international" || auth.CodeVerifier != "verifier" { t.Fatalf("auth=%#v", auth) }
}
```

Add a JWT UID fallback test using a dummy unsigned JWT whose payload contains `{"user_profile":{"user_id":"jwt-uid"}}`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/codearts -run 'TestSessionJSONImport' -v`
Expected: FAIL because `authFromSessionJSON` is undefined.

- [ ] **Step 3: Implement**

- Add `session_import.go` with:
  - `decodeSessionJSON(any) (map[string]any, *adminError)` for object/string input.
  - `authFromSessionJSON` mapping both camelCase and snake_case keys.
  - UID precedence: `account.id`, `id`, `user_id`, then refresh-token JWT `user_profile.user_id`/`sub`.
  - `expiresAt`/`expires_at` via `parseExpirationText`.
  - nested `loginContext.pkcePair.codeVerifier` and `loginContext.dpopKeyPair.privateKeyJwk`.
- `handleImport` checks `payload["session_json"]` first, then falls back to flat/nested auth.
- `authFromAdminPayload` takes `station`, requires an explicit intl station, and rejects a mainland session on the intl route.
- Handler test: call `p.handleImport` directly with a `session_json` body; it writes into `accounts/codearts-intl` and enables the realm. Route-prefix integration belongs to Task 3.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestSessionJSONImport|TestCodeArtsIntlImportHandler' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/session_import.go internal/providers/codearts/routes.go internal/providers/codearts/session_import_test.go internal/providers/codearts/routes_test.go
git commit -m "feat(codearts): import international session JSON"
```

### Task 3: International routes, overview identity, and app registration

**Files:**
- Modify: `internal/providers/codearts/routes.go`
- Modify: `internal/providers/codearts/provider.go`
- Modify: `internal/providers/codearts/station.go`
- Modify: `internal/app/providers.go`
- Test: `internal/providers/codearts/routes_test.go`, `internal/app/providers_test.go`

**Interfaces:**
- Produces: station `Region` field (`cn`/`intl`); `Routes()` prefixes all admin paths with `p.station.RoutePrefix`; intl routes omit OAuth start/poll/callback and `/oauth/callback`.

- [ ] **Step 1: Write failing tests**

```go
func TestCodeArtsIntlRoutesUsePrefix(t *testing.T) {
    p, _ := NewWithStation(newTestStore(t), International)
    for _, route := range p.Routes() {
        if !strings.HasPrefix(route.Path, "/codearts-intl/") { t.Fatalf("route %q", route.Path) }
        if route.Path == "/oauth/callback" || strings.Contains(route.Path, "/oauth/") { t.Fatalf("intl oauth route %q", route.Path) }
    }
}
```

Add an app test that `Build` returns six providers and `Registry.Get("codearts-intl")` succeeds.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/codearts -run TestCodeArtsIntlRoutesUsePrefix -v`
Run: `go test ./internal/app -run TestBuild -v`
Expected: FAIL because routes are still `/codearts/...` and intl is not registered.

- [ ] **Step 3: Implement**

- Add `Region` to `Station` (`cn` / `intl`).
- Build route paths from `p.station.RoutePrefix`; add OAuth routes only when `station.ID == Mainland.ID`; add global `/oauth/callback` only for mainland.
- `Overview`/`Health` use `p.ID()` and `p.Station().Region`.
- `handleAccounts` uses `p.ID()`.
- `internal/app/providers.go`: create `codearts.NewWithStation(store, codearts.International)` and register it; update the pool-composition comment; make `codeartsProxySlots` variadic and pass both providers to WorkBuddy.
- Update `TestBuild` expected provider count from 5 to 6 and add `codearts-intl`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestCodeArtsIntlRoutes|TestCodeArtsRoutes' -v`
Run: `go test ./internal/app -run TestBuild -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/routes.go internal/providers/codearts/provider.go internal/providers/codearts/station.go internal/providers/codearts/routes_test.go internal/app/providers.go internal/app/providers_test.go
git commit -m "feat(codearts): register international realm"
```

### Task 4: International catalog and chat against a fake upstream

**Files:**
- Modify: `internal/providers/codearts/chat.go` (benefit gate)
- Test: `internal/providers/codearts/catalog_test.go`, `internal/providers/codearts/chat_test.go`, `internal/providers/codearts/models_test.go`

**Interfaces:**
- Produces: intl catalog/chat behavior with no mainland benefit request or `maas_type` header; models wire `owned_by=codearts-intl`.

- [ ] **Step 1: Write failing tests**

Add `internal/api/chat_test.go` coverage that a bare model advertised by both `codearts` and `codearts-intl` is rejected with both qualified candidates; an exit prefix, realm hint, or key binding must remain the way to choose a station.

```go
func TestIntlCatalogFetchSkipsBenefitUpstream(t *testing.T) {
    p, _ := NewWithStation(newTestStore(t), International)
    p.station.SnapHost = fakeIntl.CatalogURL()
    account := &Account{Auth: testAuth("u1")}
    account.Auth.CloudDragonToken = ""
    account.Auth.SecurityToken = "ST"
    if _, err := p.FetchModels(context.Background(), account, true); err != nil { t.Fatal(err) }
    if fakeIntl.BenefitCalls() != 0 { t.Fatalf("benefit calls = %d", fakeIntl.BenefitCalls()) }
    if fakeIntl.ChatHost() != ... { ... }
}
```

Add a chat test asserting no `maas_type` header for an intl benefit-looking model, and a `/v1/models` test asserting `owned_by == "codearts-intl"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/codearts -run 'TestIntlCatalogFetchSkipsBenefitUpstream|TestIntlChatHasNoBenefitHeader|TestIntlModelsOwner' -v`
Expected: FAIL because chat still sets `maas_type` from `account.IsBenefit`.

- [ ] **Step 3: Implement**

- Gate the benefit header on `p.Station().SupportsBenefit` in `newChatRequest`.
- Ensure intl catalog never calls `fetchBenefitModels` (already guarded by empty `benefitHost`; add a regression test).
- Ensure intl models `owned_by/provider` are `codearts-intl` (already parameterized; add test).
- Add a fail-closed bare-model guard in `internal/api/chat.go` for the two CodeArts realms; do not change resolution for other providers.
- If the fake agent detail requires a `real_agent_id` and `selectAgentID` returns none, add a station default agent ID field only if the live intl endpoint proves it is required.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestIntlCatalog|TestIntlChat|TestIntlModels' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/chat.go internal/providers/codearts/catalog_test.go internal/providers/codearts/chat_test.go internal/providers/codearts/models_test.go
git commit -m "fix(codearts): keep intl catalog/chat station-clean"
```

### Task 5: End-to-end verification and live intl smoke

**Files:**
- Test: full repository tests.
- Optional: `docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md` only if a verified behavior changes.

- [ ] **Step 1: Run the full suite**

Run: `go test ./... -count=1`
Expected: PASS.

- [ ] **Step 2: Run vet and race**

Run: `go vet ./internal/providers/codearts ./internal/app`
Run: `go test -race ./internal/providers/codearts`
Expected: PASS.

- [ ] **Step 3: Live intl smoke (manual, only if a fresh session is available)**

1. Export a fresh CodeArts Agent international session JSON from Windows.
2. Start/point mix2api at the worktree build.
3. Import via `POST /codearts-intl/accounts/import` with `{"session_json": ...}`.
4. Verify `GET /v1/models?realm=codearts-intl` lists GLM-5.3/5.2/5.1 and Qwen3-VL.
5. Verify `POST /v1/chat/completions` with `model: "codearts-intl/GLM-5.2"` streams.
6. Verify `/codearts-intl/overview` shows the 5M token package.

If no fresh session is available, record the live smoke as blocked; fake-upstream tests still cover the code path.

- [ ] **Step 4: Commit final verification notes**

```bash
git add docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md
git commit -m "docs(codearts): phase 2 verification notes"
```