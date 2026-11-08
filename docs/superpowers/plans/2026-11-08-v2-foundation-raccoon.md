# mix2api v2 Foundation + Raccoon Vertical Slice Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Go v2 foundation, compile-time Provider registry, shared core, and a working Raccoon vertical slice (auth import, catalog, credits/tasks, chat/SSE, admin/overview, health/models) that matches v1.6.5 behavior.

**Architecture:** Single Go process. `internal/core` has shared settings/state/secrets/transport. `internal/providers` defines `Provider` and registry; `internal/providers/raccoon` owns all Raccoon logic. `internal/api` exposes generic `/health`, `/v1/models`, `/v1/chat/completions` and provider-registered `/raccoon/*` routes. Dashboard/static assets are defered to the dashboard plan.

**Tech Stack:** Go 1.22+ (local toolchain 1.22.2), standard library, `net/http/httptest`, `encoding/json`, `crypto`, `sync`, `context`.

**Spec:** `docs/superpowers/specs/2026-11-08-mix2api-v2-design.md`

## Global Constraints

- Original repo at `/root/opencode/mix2api` is read-only reference; never modify it.
- New project root: `/root/opencode/mix2api-v2`.
- No external runtime; standard library only in this plan.
- Preserve old raccoon file names/formats: `accounts/raccoon/raccoon-*.json`, `state.json`, `settings.json` keys under `raccoon`.
- Provider modules may import `core`; never each other.
- Automated tests use fake upstreams only; no real Raccoon account.
- All new code must pass `go test ./...` and `go vet ./...`.
- All HTTP/wire fields must follow v1.6.5; no additive/renamed fields without a contract test.

## Review Focus

1. **Streaming termination/cancellation** — client disconnect and upstream EOF must release the account exactly once and emit/close SSE correctly.
2. **Account state races** — manual disable, token refresh, model refresh, zero-credit and task fetching must not re-enable an account unexpectedly.
3. **Data-format corruption** — settings/state writes must preserve unknown nested keys and file permissions.
4. **Provider semantic leakage** — generic API must not know Raccoon-specific fields; Raccoon module must not import CodeArts/WorkBuddy.
5. **Oracle drift** — contract tests must compare semantic values, not dynamic timestamps/IDs.

---

### Task 1: Go module, version, and generic health server

**Files:**
- Create: `go.mod`
- Create: `cmd/mix2api/main.go`
- Create: `internal/core/version/version.go`
- Create: `internal/api/server.go`
- Test: `internal/api/server_test.go`
- Test: `internal/core/version/version_test.go`

**Interfaces:**
- Produces:
  - `version.Version string` = `"2.0.0-dev"`
  - `api.NewServer(opts ServerOptions) *http.Server`
  - `ServerOptions{Addr string; Version string}`
  - `GET /health` returns `200 {"ok":true,"version":"2.0.0-dev"}`

- [ ] **Step 1: Write failing tests**

`internal/api/server_test.go`:
```go
func TestHealth(t *testing.T) {
    srv := NewServer(ServerOptions{Addr: "127.0.0.1:0", Version: "2.0.0-dev"})
    rec := httptest.NewRecorder()
    srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
    if rec.Code != 200 { t.Fatalf("status=%d", rec.Code) }
    if !strings.Contains(rec.Body.String(), `"ok":true`) { t.Fatalf("body=%s", rec.Body) }
}
```

`internal/core/version/version_test.go`:
```go
func TestVersion(t *testing.T) {
    if Version == "" || !strings.HasPrefix(Version, "2.") { t.Fatal(Version) }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api ./internal/core/version`
Expected: FAIL, missing `NewServer`/`Version`.

- [ ] **Step 3: Implement minimal version and server**

- `version.Version = "2.0.0-dev"`.
- `NewServer` returns `&http.Server{Addr: opts.Addr, Handler: mux}`; mux handles `/health` with `Content-Type: application/json`, exact key `ok` and `version`.
- `cmd/mix2api/main.go` parses `PORT` (default `58675`), calls `NewServer`, logs `listening`, `ListenAndServe`.

- [ ] **Step 4: Run tests and vet**

Run: `go test ./... && go vet ./...`
Expected: PASS, no output from vet.

- [ ] **Step 5: Commit**

```bash
git add go.mod cmd internal
git commit -m "feat: scaffold go server and health endpoint"
```

---

### Task 2: Settings store with unknown-key preservation

**Files:**
- Create: `internal/core/config/store.go`
- Test: `internal/core/config/store_test.go`

**Interfaces:**
- Produces:
```go
type Store struct { Path string }
func New(path string) *Store
func (s *Store) Load() (map[string]any, error)
func (s *Store) Merge(patch map[string]any) (map[string]any, error)
```
- `Load` returns empty map if file missing or malformed; uses `json.Decoder.UseNumber`.
- `Merge` is recursive for maps; scalar/array values replace; preserves all unknown keys; writes atomically `0600`.

- [ ] **Step 1: Write failing tests**

```go
func TestMergePreservesUnknownNestedKeys(t *testing.T) {
    dir := t.TempDir(); p := filepath.Join(dir, "settings.json")
    os.WriteFile(p, []byte(`{"raccoon":{"enabled":true,"future":{"x":1}},"other":{"keep":2}}`), 0600)
    s := New(p)
    got, err := s.Merge(map[string]any{"raccoon": map[string]any{"enabled": false}})
    if err != nil { t.Fatal(err) }
    if got["raccoon"].(map[string]any)["future"].(map[string]any)["x"] != json.Number("1") { t.Fatal(got) }
    if got["other"].(map[string]any)["keep"] != json.Number("2") { t.Fatal(got) }
    st, _ := os.Stat(p); if st.Mode().Perm() != 0600 { t.Fatal(st.Mode()) }
}
func TestMalformedSettingsFallback(t *testing.T) { ... Load returns empty map, no error ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/core/config`
Expected: FAIL, missing `Store`.

- [ ] **Step 3: Implement**

- Use `json.Decoder.UseNumber`; recursive `mergeMaps`.
- Atomic write: temp in same dir, fsync, chmod 0600, rename.
- Do not reorder unknown keys intentionally; JSON order is not semantic.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/core/config`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/config
git commit -m "feat: add settings store preserving unknown keys"
```

---

### Task 3: Secret redaction and private file helpers

**Files:**
- Create: `internal/core/secrets/redact.go`
- Test: `internal/core/secrets/redact_test.go`

**Interfaces:**
- Produces:
```go
func Redact(text string, values ...string) string
func Mask(value string) string
func WritePrivate(path string, data []byte) error
```
- `Redact` replaces non-empty values ≥ 4 chars with `<redacted>`; also replaces URL userinfo passwords; never logs empty/short values.
- `WritePrivate` atomic write + `0600` on Unix; no-op chmod on Windows.

- [ ] **Step 1: Write failing tests**

```go
func TestRedactKnownSecrets(t *testing.T) {
    got := Redact("Authorization: Bearer abcdefgh refresh=zzzzzzzz", "abcdefgh", "zzzzzzzz")
    if strings.Contains(got, "abcdefgh") || strings.Contains(got, "zzzzzzzz") { t.Fatal(got) }
    if !strings.Contains(got, "<redacted>") { t.Fatal(got) }
}
func TestMask(t *testing.T) { if Mask("abcdefgh") == "abcdefgh" { t.Fatal("not masked") } }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/secrets`
Expected: FAIL, missing functions.

- [ ] **Step 3: Implement**

- `Redact` uses `strings.ReplaceAll` for each value; also a regex for `://user:pass@`.
- `Mask` returns a fixed `"<redacted>"` or short prefix/suffix; do not leak full value.
- `WritePrivate` is a small shared writer; Task 2 may later reuse it if desired, but don't refactor Task 2 in this task.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/core/secrets`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/secrets
git commit -m "feat: add secret redaction and private file writer"
```

---

### Task 4: Generic account state and state store

**Files:**
- Create: `internal/core/accountstate/state.go`
- Create: `internal/core/accountstate/store.go`
- Test: `internal/core/accountstate/state_test.go`

**Interfaces:**
- Produces:
```go
type State struct {
    UID string
    Disabled bool
    ManualDisabled bool
    Reason string
    CoolUntil time.Time
    LastUsed time.Time
    Note string
    TasksEnabled bool
    ActiveConcurrent int
    MaxConcurrent int
}
func (s State) Healthy(now time.Time) bool
func LoadStore(path string) (map[string]State, error)
func SaveStore(path string, states map[string]State) error
```
- `Healthy` false when disabled or now < CoolUntil.
- Store JSON is keyed by UID; timestamps use RFC3339; unknown state keys preserved? State file is provider-specific; generic store can preserve unknown top-level keys per provider later. For this task, only known fields.

- [ ] **Step 1: Write failing tests**

```go
func TestHealthy(t *testing.T) {
    now := time.Unix(1000,0)
    if !(State{}).Healthy(now) { t.Fatal("default should be healthy") }
    if (State{Disabled:true}).Healthy(now) { t.Fatal("disabled") }
    if (State{CoolUntil: now.Add(time.Second)}).Healthy(now) { t.Fatal("cooling") }
}
func TestStoreRoundTrip(t *testing.T) { ... SaveStore, LoadStore, compare UID/ManualDisabled/Note ... }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/accountstate`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Use `secrets.WritePrivate` for save.
- `LoadStore` returns empty map on missing/invalid; tolerant like v1.
- `SaveStore` sorts UIDs for deterministic output.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/core/accountstate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/accountstate
git commit -m "feat: add generic account state store"
```

---

### Task 5: Shared HTTP transport and proxy slots

**Files:**
- Create: `internal/core/transport/client.go`
- Test: `internal/core/transport/client_test.go`

**Interfaces:**
- Produces:
```go
type Slot struct {
    ID string
    Name string
    URL string
    Enabled bool
}
func SlotsFromSettings(raw map[string]any) []Slot
func (s Slots) URL(id string) string

type ClientOptions struct {
    Timeout time.Duration
}
type Client struct { ... }
func NewClient(opts ClientOptions) *Client
func (c *Client) Do(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error)
```
- Empty proxy means direct and ignores `HTTP_PROXY`/`HTTPS_PROXY`; non-empty proxy uses that URL for both HTTP/HTTPS.
- Connection reuse per proxy URL; close idle connections when `Client.Close()`.
- `SlotsFromSettings` reads `proxy_slots` entries from settings; disabled/missing slot resolves to `""` (direct).

- [ ] **Step 1: Write failing tests**

```go
func TestDirectRequest(t *testing.T) { ... }
func TestProxyRequestUsesAbsoluteURI(t *testing.T) { ... }
func TestSlotResolution(t *testing.T) { ... }
func TestDirectIgnoresEnvironmentProxy(t *testing.T) { ... }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/transport`
Expected: FAIL, missing `NewClient`.

- [ ] **Step 3: Implement**

- Use `http.Transport{Proxy: nil}` for direct, `http.ProxyURL(url)` for proxy.
- Cache transports by normalized proxy URL, `sync.Mutex`, close idle on close.
- Do not log proxy credentials; use `secrets.Redact` when errors include URLs.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/core/transport`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/transport
git commit -m "feat: add shared transport and proxy slot resolution"
```

---

### Task 6: Provider contract and registry

**Files:**
- Create: `internal/providers/base.go`
- Create: `internal/providers/registry.go`
- Create: `internal/core/scheduler/job.go`
- Test: `internal/providers/registry_test.go`

**Interfaces:**
- Produces:
```go
type Provider interface {
    ID() string
    Realm() string
    Overview(ctx context.Context, force bool) (any, error)
    Health(ctx context.Context, force bool) (Health, error)
    Models(ctx context.Context, force bool) ([]Model, error)
    OpenChat(ctx context.Context, req ChatRequest) (ChatStream, error)
    Routes() []Route
    Jobs() []scheduler.Job
}

type ChatRequest struct {
    Raw        map[string]json.RawMessage
    Headers    http.Header
    Model      string
    Messages   []json.RawMessage
    SessionKey string
}
type ChatStream interface { Next() ([]byte, error); Close() error }
type Health = map[string]any
type Model = map[string]any

// internal/core/scheduler/job.go
type Job interface {
    Name() string
    Run(ctx context.Context) error
}

type AuthClass int
const (
    AuthNone AuthClass = iota
    AuthAPIKey
    AuthPanel
)
type Route struct {
    Method string
    Path string
    Auth AuthClass
    Handler http.HandlerFunc
}

type Registry struct { ... }
func (r *Registry) Register(p Provider) error
func (r *Registry) Get(realm string) (Provider, bool)
func (r *Registry) All() []Provider
```
- `Register` errors on empty realm, duplicate realm, nil provider.
- `scheduler.Job` is defined in `internal/core/scheduler`; that package must not import providers. `internal/api` may import this package, but this package must not import `internal/api` or concrete providers.
- Provider config and pool are internal to each provider; they are not part of the core interface.

- [ ] **Step 1: Write failing tests**

```go
func TestRegistryRegisterAndDuplicate(t *testing.T) {
    r := NewRegistry()
    p := fakeProvider{id:"raccoon", realm:"raccoon"}
    if err := r.Register(p); err != nil { t.Fatal(err) }
    if _, ok := r.Get("raccoon"); !ok { t.Fatal("missing") }
    if err := r.Register(p); err == nil { t.Fatal("duplicate accepted") }
}
func TestRegistryRejectsEmptyRealm(t *testing.T) { ... }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Thread-safe registry with `sync.RWMutex`.
- Preserve registration order for `All`.
- WorkBuddy will register two provider instances with realms `intl` and `cn`; registry sees them as distinct.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers
git commit -m "feat: add compile-time provider contract and registry"
```

---

### Task 7: Raccoon config and auth file model

**Files:**
- Create: `internal/providers/raccoon/config.go`
- Create: `internal/providers/raccoon/auth.go`
- Test: `internal/providers/raccoon/config_test.go`
- Test: `internal/providers/raccoon/auth_test.go`

**Interfaces:**
- Produces:
```go
type Config struct {
    Enabled bool
    TimeoutSeconds float64
    StreamTimeoutSeconds float64
    ModelsTTLSeconds float64
    MaxConcurrent int
    AutoClaimLogin bool
}
func LoadConfig(store *config.Store) Config
func SaveConfig(store *config.Store, patch map[string]any) (Config, error)

type Auth struct {
    UID string
    AccessToken string
    RefreshToken string
    OfficeIdentity string
    OfficeOrgName string
    ExpiresAt time.Time
}
func LoadAuth(path string) (Auth, error)
func SaveAuth(path string, auth Auth) error
func UIDFromAccessToken(token string) string
```
- Defaults match v1: enabled false, timeout 60, stream 900, models TTL 600, max concurrent 1, auto claim false.
- Auth file path `accounts/raccoon/raccoon-<safe-uid>.json`; JSON keys match v1 (`uid`, `access_token`, `refresh_token`, `office_identity`, `office_org_name`, `expires_at`).
- Secrets never printed in errors.

- [ ] **Step 1: Write failing tests**

```go
func TestConfigDefaults(t *testing.T) { ... }
func TestAuthUIDFromJWT(t *testing.T) {
    tok := "eyJ..."; // hand-built unsigned JWT fixture
    if UIDFromAccessToken(tok) != "pu-test" { t.Fatal(...) }
}
func TestAuthRoundTrip(t *testing.T) { ... mode 0600 ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/raccoon`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Config merge uses `config.Store.Merge` under `raccoon` key; preserve unknown keys.
- JWT claims decode base64url payload; read `sub`; do not verify signature.
- Auth save uses `secrets.WritePrivate`; reject empty access token.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/raccoon`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/raccoon
git commit -m "feat: add raccoon config and auth files"
```

---

### Task 8: Raccoon HTTP client, token refresh, and single-flight

**Files:**
- Create: `internal/providers/raccoon/token.go`
- Create: `internal/providers/raccoon/http.go`
- Test: `internal/providers/raccoon/token_test.go`

**Interfaces:**
- Produces:
```go
type TokenManager struct { ... }
func (m *TokenManager) Ensure(ctx context.Context, a *Account) error
func (m *TokenManager) Refresh(ctx context.Context, a *Account) error
```
- Refresh endpoint `/api/web/auth/v1/refresh`; body `{"refresh_token": "..."}`; update access/refresh/expiry and persist auth.
- Concurrent refreshes for one account coalesce; a forced refresh after 401 only happens once per generation.
- 401/403 refresh failure marks account terminal; network failure is transient.

- [ ] **Step 1: Write failing tests**

`token_test.go` uses `httptest.Server`:
```go
func TestRefreshRotatesAndPersists(t *testing.T) { ... }
func TestConcurrentEnsureRefreshesOnce(t *testing.T) { ... }
func TestRefreshErrorRedactsTokens(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/raccoon -run TestRefresh`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Use `core/transport.Client` from Task 5; do not invent a second transport. Inject it into the token manager for tests.
- Use `sync.Mutex` or `singleflight`-style coalescing per account. No external dependency.
- Decode `data.access_token`/`refresh_token`; derive UID from access token; persist via `SaveAuth`.
- Refresh window: 120 seconds before `ExpiresAt` (matching v1).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/raccoon -run 'TestRefresh|TestConcurrentEnsure'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/raccoon/http.go internal/providers/raccoon/token.go internal/providers/raccoon/token_test.go
git commit -m "feat: add raccoon token refresh with single-flight"
```

---

### Task 9: Raccoon account and pool

**Files:**
- Create: `internal/providers/raccoon/account.go`
- Create: `internal/providers/raccoon/pool.go`
- Test: `internal/providers/raccoon/pool_test.go`

**Interfaces:**
- Produces:
```go
type Account struct {
    Auth Auth
    State accountstate.State
    Credits *CreditsSnapshot
    PointsTasks *PointsTasks
    ...
}
func (a *Account) View() AccountView
func (a *Account) Healthy(now time.Time) bool

type Pool struct { ... }
func NewPool(dir string, cfg Config) (*Pool, error)
func (p *Pool) Pick(model, sessionKey string, exclude map[string]bool) (*Account, error)
func (p *Pool) Acquire(a *Account) bool
func (p *Pool) Release(a *Account)
func (p *Pool) Disable(uid, reason string) error
func (p *Pool) Enable(uid string) error
```
- Disabled/manual/cooldown/reserve/daily/model guards exclude accounts.
- Session affinity per session key; stable `last_used` ordering; max concurrent.
- Manual disable survives token refresh; account state persists in `state.json`.
- No provider-specific credit or tasks logic in generic `core`; this package owns it.

- [ ] **Step 1: Write failing tests**

```go
func TestPickSkipsDisabledAndZeroReserve(t *testing.T) { ... }
func TestManualDisableSurvivesApplyLimits(t *testing.T) { ... }
func TestAcquireRespectsMaxConcurrent(t *testing.T) { ... }
func TestSessionAffinity(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/raccoon -run TestPick`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Load auth files from `accounts/raccoon`; deterministic UID ordering.
- State storage via `accountstate.LoadStore`/`SaveStore`.
- Apply limits from `settings.json` keys used by v1 (reserve/daily/token/model limits); store fields on Account.
- `View()` returns v1-compatible keys `name, uid, nickname, disabled, cooling, until, err_count, reason, last_error, active_concurrent, max_concurrent, token_remaining, expires_at, models, credits, points_tasks, guard, ...`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/raccoon`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/raccoon/account.go internal/providers/raccoon/pool.go internal/providers/raccoon/pool_test.go
git commit -m "feat: add raccoon account pool and guards"
```

---

### Task 10: Raccoon catalog

**Files:**
- Create: `internal/providers/raccoon/catalog.go`
- Test: `internal/providers/raccoon/catalog_test.go`

**Interfaces:**
- Produces:
```go
func (p *Pool) FetchCatalog(ctx context.Context, force bool) ([]Model, error)
func (p *Pool) Models(ctx context.Context, force bool) ([]Model, error)
```
- Endpoint `/api/web/llm/v2/model_catalog`; bare IDs, no prefix.
- Seed fallback has the nine v1 IDs; cache TTL 600s.
- Model entries expose v1 capability fields; failure keeps last successful catalog.

- [ ] **Step 1: Write failing tests**

```go
func TestCatalogBareIDs(t *testing.T) { ... }
func TestCatalogCacheAndForce(t *testing.T) { ... }
func TestCatalogSeedFallback(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/raccoon -run TestCatalog`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Use fake upstream fixture from v1 `_fake_raccoon.py` translated to Go; do not hit network.
- Normalize IDs exactly; do not add provider prefix.
- Cache per provider/pool with mutex; `force` bypasses TTL.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/raccoon -run TestCatalog`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/raccoon/catalog.go internal/providers/raccoon/catalog_test.go
git commit -m "feat: add raccoon model catalog"
```

---

### Task 11: Raccoon chat and SSE

**Files:**
- Create: `internal/providers/raccoon/chat.go`
- Create: `internal/providers/raccoon/stream.go`
- Test: `internal/providers/raccoon/chat_test.go`

**Interfaces:**
- Produces:
```go
func (p *Pool) OpenChat(ctx context.Context, req ChatRequest) (ChatStream, error)
```
- `ChatStream` yields normalized bytes/frames and closes/releases exactly once.
- v1 allowlist fields: model/messages/tools/tool_choice/parallel_tool_calls/temperature/top_p/stream/stop/max_tokens/max_completion_tokens/presence_penalty/frequency_penalty/user/seed/response_format/reasoning_effort/thinking/enable_thinking.
- Force upstream `stream:true`; OpenAI-style SSE; preflight first data line; embedded error frames map to provider errors; `[DONE]` closes.

- [ ] **Step 1: Write failing tests**

```go
func TestChatForcesStreamAndAllowlist(t *testing.T) { ... }
func TestChatSSEAndUsage(t *testing.T) { ... }
func TestStreamCloseReleasesOnce(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/raccoon -run TestChat`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Use `httptest.Server` fake SSE with role/content/finish/usage.
- `Close` idempotent with `sync.Once`; release account once.
- Redact upstream error bodies; propagate context cancellation.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/raccoon -run TestChat`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/raccoon/chat.go internal/providers/raccoon/stream.go internal/providers/raccoon/chat_test.go
git commit -m "feat: add raccoon chat and sse stream"
```

---

### Task 12: Raccoon credits, points tasks, and login rewards

**Files:**
- Create: `internal/providers/raccoon/credits.go`
- Create: `internal/providers/raccoon/tasks.go`
- Create: `internal/providers/raccoon/rewards.go`
- Test: `internal/providers/raccoon/credits_test.go`
- Test: `internal/providers/raccoon/tasks_test.go`

**Interfaces:**
- Produces:
```go
func (p *Pool) FetchCredits(ctx context.Context, a *Account, force bool) (*CreditsSnapshot, error)
func (p *Pool) RefreshCredits(ctx context.Context, uid string) ([]CreditsResult, error)
func (p *Pool) FetchTasks(ctx context.Context, a *Account, force bool) (*PointsTasks, error)
func (p *Pool) GrantDesktopReward(ctx context.Context, a *Account) (*GrantResult, error)
func (p *Pool) GrantMobileReward(ctx context.Context, a *Account) (*GrantResult, error)
func (p *Pool) AutoClaim(ctx context.Context, a *Account) (AutoClaimResult, error)
```
- Balance `/api/web/points/v1/balance`, bills `/api/web/points/v2/bills`, tasks `/api/web/points/v1/tasks`.
- Desktop `/api/web/desktop/v1/login/points/grant`; mobile `/api/web/mobile/v1/login/points/grant` with `app-android`, `v1.1.0`.
- Reward idempotency; auto-claim TTL 5 min; no invite code exposure; no double pay.

- [ ] **Step 1: Write failing tests**

```go
func TestCreditsSnapshotAndBills(t *testing.T) { ... }
func TestTasksStripInviteCode(t *testing.T) { ... }
func TestGrantDesktopAndMobileIdempotent(t *testing.T) { ... }
func TestAutoClaimRespectsTasksToggle(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/raccoon -run 'TestCredits|TestTasks|TestGrant|TestAuto'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Fake upstream responses mirror v1 `_fake_raccoon.py`; update balances/tasks.
- Use provider-specific locks for credits/tasks/auto-claim.
- JSON shapes must match v1 public view.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/raccoon`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/raccoon/credits.go internal/providers/raccoon/tasks.go internal/providers/raccoon/rewards.go internal/providers/raccoon/*_test.go
git commit -m "feat: add raccoon credits tasks and rewards"
```

---

### Task 13: Raccoon provider routes and overview

**Files:**
- Create: `internal/providers/raccoon/routes.go`
- Create: `api/routes.go` adapter (or `internal/api/routes.go`)
- Test: `internal/providers/raccoon/routes_test.go`

**Interfaces:**
- Produces:
```go
func (p *Provider) Routes() []Route
// GET /raccoon/overview
// GET /raccoon/accounts
// POST /raccoon/accounts/import
// POST /raccoon/accounts/enable|disable|delete|clear-cooldown|proxy|note|tasks|test
// POST /raccoon/accounts/grant-desktop|grant-mobile
// POST /raccoon/credits/refresh
// POST /raccoon/keepalive
// POST /raccoon/models/refresh
// POST /raccoon/accounts/oauth/start|poll|callback
// GET  /auth/callback (loopback, official path only)
```
- Panel auth enforced by generic API middleware; routes return v1 JSON envelopes/status codes.
- OAuth start/poll/callback use a session table with TTL and one-time state; official callback path `/auth/callback`; legacy path is not reintroduced.

- [ ] **Step 1: Write failing tests**

```go
func TestRaccoonRoutesRequirePanel(t *testing.T) { ... }
func TestOverviewContainsNoTokens(t *testing.T) { ... }
func TestImportOverviewAndGrant(t *testing.T) { ... }
func TestUnknownRaccoonRoute404(t *testing.T) { ... }
func TestOAuthStartPollCallback(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/raccoon -run TestRaccoon`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Reuse Task 7 auth/config and Task 9 pool.
- Import accepts flat auth and `{"auth": {...}}`.
- Never return `access_token`/`refresh_token`; panel token middleware is API layer's responsibility.
- `overview` includes accounts/stats/models/base_url/health/auto_claim_login.
- OAuth session table: 300s TTL, max 64, state compare, one-time exchange; poll returns `pending|done|error`; callback validates state before exchange; no token in logs.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/raccoon`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/raccoon/routes.go internal/providers/raccoon/routes_test.go internal/api/routes.go
git commit -m "feat: add raccoon admin routes"
```

---

### Task 14: Generic API chat/models/health and Raccoon integration

**Files:**
- Modify: `internal/api/server.go`
- Create: `internal/api/auth.go`
- Create: `internal/api/routes/chat.go`
- Create: `internal/api/routes/models.go`
- Test: `internal/api/chat_test.go`
- Test: `internal/api/models_test.go`

**Interfaces:**
- Produces:
```go
func RegisterProviderRoutes(mux *http.ServeMux, p Provider)
func (s *Server) HandleChat(w http.ResponseWriter, r *http.Request, p Provider, model string)
```
- `POST /v1/chat/completions`: parse JSON, resolve realm/provider, validate model, call `Provider.OpenChat`; stream SSE if `stream`, aggregate for non-stream; map provider errors to v1 status/error body.
- `GET /v1/models?realm=raccoon`: return provider Models; `realm=all` remains WorkBuddy-only (in this plan it may return 400/empty until WorkBuddy provider exists; lock behavior).
- Panel/API key auth classes are declared; full key storage follows platform plan.

- [ ] **Step 1: Write failing tests**

```go
func TestChatStreamingRaccoon(t *testing.T) { ... }
func TestChatNonStreamingRaccoon(t *testing.T) { ... }
func TestModelsRealmRaccoon(t *testing.T) { ... }
func TestUnknownRealm404(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Use `httptest` with fake Raccoon provider; no network.
- Use `http.Flusher`; preserve SSE bytes; non-stream aggregate text/reasoning/tool calls.
- Error mapping uses provider error classes; no Raccoon fields in generic code.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api ./internal/providers/raccoon`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api
git commit -m "feat: add generic chat and models routes with raccoon"
```

---

### Task 15: Contract harness for health/models/chat/SSE

**Files:**
- Create: `tests/contract/normalize.go`
- Create: `tests/contract/raccoon_test.go`
- Create: `tests/oracle/README.md`
- Test: `tests/contract/...`

**Interfaces:**
- Produces:
```go
func NormalizeJSON([]byte) (any, error)
func NormalizeSSE([]byte) ([]Event, error)
```
- `NormalizeJSON` removes `Date`, `Server`, timestamps, UUID/session/request IDs; preserves numbers/types.
- `NormalizeSSE` captures event type, role, finish, text concatenation, tool structure, usage.
- Oracle instructions: copy v1.6.5 to `tests/oracle/v1.6.5`, test-only patch base URL/identity; never modify original.

- [ ] **Step 1: Write failing tests**

```go
func TestNormalizeJSONIgnoresDynamicFields(t *testing.T) { ... }
func TestNormalizeSSEMergesChunks(t *testing.T) { ... }
func TestRaccoonGoldenHealthModelsChat(t *testing.T) { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tests/contract`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Golden fixtures from v1.6.5 outputs (recorded manually or from oracle test).
- Compare normalized semantic values.
- No real network; fake upstream.

- [ ] **Step 4: Run tests**

Run: `go test ./tests/contract`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tests/contract tests/oracle
git commit -m "test: add contract normalization and raccoon parity"
```

---

### Task 16: Vertical-slice command and smoke test

**Files:**
- Modify: `cmd/mix2api/main.go`
- Modify: `cmd/providers.go`
- Create: `tests/integration/raccoon_slice_test.go`
- Test: `tests/integration/...`

**Interfaces:**
- Produces: `go run ./cmd/mix2api` starts server on `PORT`, registry includes Raccoon, health/models/chat work through HTTP.
- No dashboard yet; `/` may 404 or serve a placeholder; dashboard plan replaces.

- [ ] **Step 1: Write failing test**

```go
func TestRaccoonSliceOverHTTP(t *testing.T) {
    // start server with fake upstream env, POST import, GET overview, POST chat
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tests/integration -run TestRaccoonSlice`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Register Raccoon in `cmd/providers.go`.
- Wire server/config dirs from env/flags: `PORT`, `ACCOUNTS_DIR`, `USAGE_DIR`.
- Ensure server logs no secrets.

- [ ] **Step 4: Run tests and full slice**

Run: `go test ./... && go vet ./...`
Expected: PASS.

Run: `PORT=58676 ACCOUNTS_DIR=/tmp/... go run ./cmd/mix2api`, then `curl /health`.
Expected: 200.

- [ ] **Step 5: Commit**

```bash
git add cmd tests/integration
git commit -m "feat: wire raccoon vertical slice into command"
```

---

## Self-Review

- Spec coverage for plan 1: Go scaffold, shared core, provider contract, Raccoon auth/catalog/chat/credits/tasks/rewards/admin, generic chat/models/health, contract harness and smoke.
- No WorkBuddy/CodeArts behavior is implemented here; review focus still applies globally.
- Type names/signatures in tasks 5–15 are consistent with the master interfaces.
- Steps are test-first; each task ends with a commit.
- Deferred to later plans: dashboard, full panel auth/key storage, full usage/pricing/media/agents/update, CodeArts/WorkBuddy, packaging.