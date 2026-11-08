# mix2api v2 WorkBuddy Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement WorkBuddy international and China providers with v1.6.5 behavior: account/OAuth/desktop import, identity/endpoints, token refresh, catalog/tiers, credits/packages/guards, pool scheduling, chat/SSE/tools/images, check-in, daily activity, growth tasks, Buddy/cat/streak, cross-exit fallback, and all admin routes.

**Architecture:** One `internal/providers/workbuddy` implementation registered twice (`intl`, `cn`). Provider-specific behavior stays here; generic API/core only sees `Provider` and `Route`.

**Tech Stack:** Go standard library, `crypto`, `net/http`, `httptest`, `compress/gzip`, `encoding/json`.

**Spec:** `docs/superpowers/specs/2026-11-08-mix2api-v2-design.md`
**Foundation plan:** `docs/superpowers/plans/2026-11-08-v2-foundation-raccoon.md`

## Global Constraints

- v1.6.5 `app/wb_accounts.py`, `app/wb_tasks.py`, `app/wb_identity.py`, `app/wb_catalog.py`, `app/wb_tiers.py`, `app/wb_scheduler.py` and their tests are the behavior reference.
- Two provider instances: realms `intl`, `cn`; same code, different endpoints/UA/price/tier.
- Preserve old account JSON, settings keys, identity headers, model IDs and API routes.
- No cross-provider model merging; `realm=all` is WorkBuddy-only.
- International→China fallback is WorkBuddy-internal and default off.
- Automated tests use fake upstreams and synthetic fixtures; no real account.

## Review Focus

1. Identity/header compatibility — UA/version, `X-*`, conversation/message/trace IDs, gzip, and CN endpoint overrides.
2. Account state races — refresh, model discovery, enable/disable, cooldown, credits/guards, check-in, group actions.
3. Streaming/retry/failover — SSE finish/usage, transient errors, 429 windows, cross-exit fallback when explicitly enabled.
4. Credit/package semantics — WorkBuddy packages vs daily credits vs expiring priority vs guards; never reuse values for CodeArts/Raccoon.
5. Scheduler idempotency — daily activity, check-in, growth tasks, cat/streak, manual triggers, catch-up after restart.

---

### Task 1: Realm config, endpoints, identity

**Files:**
- Create: `internal/providers/workbuddy/config.go`
- Create: `internal/providers/workbuddy/identity.go`
- Test: `internal/providers/workbuddy/identity_test.go`

**Interfaces:**
```go
type Config struct {
    Enabled bool
    Realm string // intl|cn
    TimeoutSeconds, StreamTimeoutSeconds, ModelsTTLSeconds float64
    MaxConcurrent int
    AccountMaxConcurrent int
    AutoSwitchProduct bool
}
func New(realm string) *Provider
func (p *Provider) ID() string // workbuddy-intl|workbuddy-cn
func (p *Provider) Realm() string
func endpointFor(realm, product string) string
func desktopUA(realm string) string
func desktopVersion(realm string) string
func buildIdentityHeaders(...) http.Header
```
- Defaults from v1; CN chat URL env `WB_CN_CHAT_UPSTREAM`; desktop UA/version `5.8.4` intl, `5.8.3` cn; identity overrides from settings.
- Conversation/message/turn IDs UUIDv7/stable; gzip body; trace headers; no secrets.

- [ ] **Step 1: Write failing tests** for realm endpoints, UA/version, identity headers, UUID stable per session, gzip body.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestIdentity`; expect FAIL.
- [ ] **Step 3: Implement** from v1 `wb_identity.py`; keep 5.8.4/5.8.3 defaults.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add realm config and identity`.

---

### Task 2: Account auth, import, desktop credentials, OAuth

**Files:**
- Create: `internal/providers/workbuddy/auth.go`
- Create: `internal/providers/workbuddy/oauth.go`
- Create: `internal/providers/workbuddy/desktop.go`
- Test: `internal/providers/workbuddy/auth_test.go`
- Test: `internal/providers/workbuddy/oauth_test.go`

**Interfaces:**
```go
type Auth struct {
    UID, Nickname, Realm, Domain string
    AccessToken, RefreshToken string
    ExpiresAt time.Time
    Platform, EnterpriseID string
    Source string
}
func LoadAccounts(dir string) ([]Auth, error)
func SaveAccount(dir string, a Auth) (Auth, error)
func DeleteAccount(dir, uid string) error
func ResolveDeviceToken(dir string) (string, error)
func ScanDesktopCredentials() ([]Auth, error)
func FetchProfile(ctx, a *Auth) error
func DetectRealmFromToken(token, domain string) string
func BeginBrowserOAuth(...) (OAuthStart, error)
```
- Preserve v1 file names/fields, 0600, API-key account markers, device token login proxy slot.
- OAuth state/PKCE and callback behavior; no token logs; duplicate re-login preserves proxy slot and notes.
- Desktop credential scan reads local client data; no secrets returned to panel.

- [ ] **Step 1: Write failing tests** for file round-trip, slug/filename, realm detection, duplicate import, device token, profile, OAuth start/callback.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run 'TestAuth|TestOAuth'`; expect FAIL.
- [ ] **Step 3: Implement** with `core/secrets`; mock HTTP profile.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add account auth and oauth`.

---

### Task 3: Token refresh and keepalive

**Files:**
- Create: `internal/providers/workbuddy/token.go`
- Test: `internal/providers/workbuddy/token_test.go`

**Interfaces:**
```go
func (p *Provider) EnsureToken(ctx, a *Account) error
func (p *Provider) Keepalive(ctx) (KeepaliveResult, error)
```
- Refresh before expiration (v1 skew); single-flight; refresh token rotation/persist; terminal auth marks disabled/needs re-login.
- Keepalive skips disabled; counts refreshed; no check-in side effect.
- Manual disable remains after refresh.

- [ ] **Step 1: Write failing tests** for expiry, concurrent refresh, rotated refresh token, terminal 401, manual disable preservation.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestToken`; expect FAIL.
- [ ] **Step 3: Implement** with `core/transport.Client`.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add token refresh and keepalive`.

---

### Task 4: Account model and pool scheduling

**Files:**
- Create: `internal/providers/workbuddy/account.go`
- Create: `internal/providers/workbuddy/pool.go`
- Create: `internal/providers/workbuddy/groups.go`
- Test: `internal/providers/workbuddy/pool_test.go`

**Interfaces:**
```go
type Account struct {
    Auth Auth
    State accountstate.State
    Credits *CreditSnapshot
    DisabledModels map[string]bool
    Group string
    Weight float64
    Tier string
    ExpiringWindowDays int
}
func (p *Provider) NewPool() *Pool
func (p *Pool) Pick(model, sessionKey string, exclude map[string]bool) (*Account, error)
func (p *Pool) Enable/Disable/ClearCooldown/SetNote/SetTasks/SetWeight/SetGroup(...)
```
- Pick filters realm, disabled, manual, cooldown, model-disabled, guard-blocked, account concurrency.
- Session affinity; stable affinity key option; weighted round-robin; expiring priority; remaining-usage priority; tier/Pro-only; fallback.
- Account groups operate on subset; account test all; notes/penalties.
- Preserve state/persistence and no provider-specific fields in core.

- [ ] **Step 1: Write failing tests** for concurrency, affinity, weights, expiring/remaining priority, disabled model, groups, guards, manual disable.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestPool`; expect FAIL.
- [ ] **Step 3: Implement** from v1 `wb_accounts.AccountPool`; keep deterministic selection.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add account pool and scheduling`.

---

### Task 5: Catalog, tiers, model metadata

**Files:**
- Create: `internal/providers/workbuddy/catalog.go`
- Create: `internal/providers/workbuddy/tiers.go`
- Test: `internal/providers/workbuddy/catalog_test.go`

**Interfaces:**
```go
func (p *Provider) Models(ctx, force bool) ([]Model, error)
func (p *Provider) RefreshCatalog(ctx) (CatalogResult, error)
func capabilitiesFor(modelID, realm string) map[string]any
func requiresPro(modelID string) bool
```
- Live `/v3/config` with desktop cache fallback and bundled fallback; intl/cn separate catalogs.
- Model IDs/aliases; context/output limits; vision/tool/reasoning; credits peak/offpeak; Pro-only entitlement; free model detection.
- Cache TTL 300s; daily background refresh hook; manual refresh cooldown.
- Agent picker must include all current realm models; WorkBuddy catalog remains source for WorkBuddy.

- [ ] **Step 1: Write failing tests** for live/cache/bundled sources, realm separation, aliases, limits, Pro-only, vision/reasoning, refresh cooldown.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestCatalog`; expect FAIL.
- [ ] **Step 3: Implement** using v1 fixtures; no network in tests.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add catalog and tier metadata`.

---

### Task 6: Credits, packages, grants, guards

**Files:**
- Create: `internal/providers/workbuddy/credits.go`
- Create: `internal/providers/workbuddy/grants.go`
- Test: `internal/providers/workbuddy/credits_test.go`

**Interfaces:**
```go
type CreditSnapshot struct { Remain, Used, Size float64; Packages []CreditPackage; ... }
func (p *Provider) RefreshCredits(ctx) (CreditsRefreshResult, error)
func (p *Provider) FetchGrants(ctx) ([]GrantRow, error)
func (p *Provider) ApplyLimits(values, usage map[string]any)
```
- Parse credit packages, expiry (`DeductionEndTime`, no-expiry), package status; WorkBuddy-compatible remain/amount/used.
- Refresh all/per account; preserve daily/weekly usage; grants history.
- Guards: reserve credits, daily credits, daily token, model daily token; global intl/cn overrides; expiring window; no effect on CodeArts/Raccoon.
- Never use credits for another provider.

- [ ] **Step 1: Write failing tests** for package parsing, no-expiry, refresh, grants, reserve/daily guards, realm override, unknown data.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run 'TestCredits|TestGuards'`; expect FAIL.
- [ ] **Step 3: Implement** from v1 `wb_accounts` and `wb_settings` fixtures.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add credits packages and guards`.

---

### Task 7: Chat, SSE, tools/images, fallback

**Files:**
- Create: `internal/providers/workbuddy/chat.go`
- Create: `internal/providers/workbuddy/stream.go`
- Create: `internal/providers/workbuddy/errors.go`
- Test: `internal/providers/workbuddy/chat_test.go`

**Interfaces:**
```go
func (p *Provider) OpenChat(ctx, req ChatRequest) (ChatStream, error)
```
- Intl/cn chat paths, identity/headers/gzip, conversation IDs, prompt cache; tools/tool streams, images, reasoning, usage frame; Responses/Anthropic handled by generic API.
- Transient/network retry; 429/window cooldown; 401 refresh once; account switch; no failover on 400.
- Cross-exit fallback intl→cn only when enabled and model exists both; explicit realm/key suppresses.
- SSE finish/usage/trailing empty choices; client cancel releases once.
- Usage row fields: realm, account, model, tokens, credit optional, cost policy.

- [ ] **Step 1: Write failing tests** for intl/cn headers, tools/images, reasoning, 429 failover, 401 refresh, cross-exit, SSE finish/usage, close-once.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestChat`; expect FAIL.
- [ ] **Step 3: Implement** from v1 `wb_accounts`/`wb_proxy` fake fixtures.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add chat sse and fallback`.

---

### Task 8: Check-in, daily activity, growth tasks, Buddy/cat/streak

**Files:**
- Create: `internal/providers/workbuddy/checkin.go`
- Create: `internal/providers/workbuddy/daily.go`
- Create: `internal/providers/workbuddy/growth.go`
- Create: `internal/providers/workbuddy/fun.go`
- Test: `internal/providers/workbuddy/tasks_test.go`

**Interfaces:**
```go
func (p *Provider) Checkin(ctx, uid string) (CheckinResult, error)
func (p *Provider) DailyChat(ctx, uid string) (DailyResult, error)
func (p *Provider) DailyChatWeb(ctx, uid string) (DailyResult, error)
func (p *Provider) GrowthTasks(ctx, uid string, autoClaim bool) (GrowthResult, error)
func (p *Provider) BuddyCheckin(ctx, uid string) (BuddyResult, error)
func (p *Provider) CatTravel(ctx, uid string) (CatResult, error)
func (p *Provider) StreakBonus(ctx, uid string) (StreakResult, error)
```
- Check-in slots 09:00/21:00; daily activity once; web retry backoff; growth task accept/report/claim; auto-claim toggle; Buddy/cat/streak; task flags; disabled accounts skipped; manual actions force.
- No duplicate check-in/claim; activity history events; idempotency across scheduler restart.
- No CodeArts/Raccoon semantics.

- [ ] **Step 1: Write failing tests** for daily once, web retry, task accept/claim, auto-claim off, Buddy/cat/streak, task participation opt-out, duplicate prevention.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestTasks`; expect FAIL.
- [ ] **Step 3: Implement** from `wb_tasks.py` and v1 tests; fake upstream.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add checkin daily and growth tasks`.

---

### Task 9: Admin routes and account management

**Files:**
- Create: `internal/providers/workbuddy/routes.go`
- Test: `internal/providers/workbuddy/routes_test.go`

**Interfaces:**
```go
func (p *Provider) Routes() []Route
// Existing v1 account/login/checkin/credits/tasks/activity/export routes
```
- Account list/import/export/login/test/enable/disable/note/tasks/weight/group/delete/clear cooldown.
- OAuth/device login, desktop import, API key accounts; panel auth; no token leakage.
- Checkin, daily activity, growth task, Buddy/cat/streak endpoints; activity/credits/grants; model refresh; settings.
- Preserve status codes and messages.

- [ ] **Step 1: Write failing tests** for panel auth, account actions, export/import, login, activity, credits, task actions, no secrets.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestRoutes`; expect FAIL.
- [ ] **Step 3: Implement** routes.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add admin routes`.

---

### Task 10: Scheduler jobs and manual triggers

**Files:**
- Create: `internal/providers/workbuddy/jobs.go`
- Modify: `internal/core/scheduler/...`
- Test: `internal/providers/workbuddy/jobs_test.go`

**Interfaces:**
```go
func (p *Provider) Jobs() []scheduler.Job
```
- Jobs: check-in, daily activity, growth/task summary, cat, streak, keepalive, catalog refresh.
- Config hours, only enabled accounts, no duplicates, force manual.
- Job errors never crash gateway; per-account isolation.

- [ ] **Step 1: Write failing tests** for job registration, schedule due, disabled skip, manual force, idempotency, error isolation.
- [ ] **Step 2: Run** `go test ./internal/providers/workbuddy -run TestJobs`; expect FAIL.
- [ ] **Step 3: Implement** job wrappers around Task 8/3/5.
- [ ] **Step 4: Run tests**; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): add scheduler jobs`.

---

### Task 11: Provider registration and generic API integration

**Files:**
- Modify: `cmd/providers.go`
- Modify: `internal/api/routes.go`
- Test: `tests/integration/workbuddy_slice_test.go`

**Interfaces:** registry `intl`, `cn`; `/health`, models, chat/responses/messages; API Key realm binding; `realm=all` WorkBuddy-only.

- [ ] **Step 1: Write failing integration tests** with fake upstreams for both realms.
- [ ] **Step 2: Run** `go test ./tests/integration -run TestWorkBuddy`; expect FAIL.
- [ ] **Step 3: Implement** registration/wiring.
- [ ] **Step 4: Run** `go test ./...`; expect PASS.
- [ ] **Step 5: Commit** `feat(workbuddy): wire providers into gateway`.

---

### Task 12: WorkBuddy contract and parity

**Files:**
- Create: `tests/contract/workbuddy_test.go`
- Create: `tests/oracle/workbuddy.md`
- Test: `tests/contract/...`

**Interfaces:** foundation normalization.
- Cases: health/models/catalog, chat/SSE, tools/images, reasoning, 429/401/failover, credits/guards, checkin/daily/growth, admin.
- Oracle: v1.6.5 test copy; fake upstream; no original changes.
- Normalize IDs/timestamps; compare semantic values.

- [ ] **Step 1: Write failing contract tests/fixtures**.
- [ ] **Step 2: Run** `go test ./tests/contract -run TestWorkBuddy`; expect FAIL.
- [ ] **Step 3: Implement/fix provider.**
- [ ] **Step 4: Run** `go test ./tests/contract`; expect PASS.
- [ ] **Step 5: Commit** `test(workbuddy): add parity contract suite`.

---

## Self-Review

- Covers intl/cn identity/catalog/chat/credits/pool/tasks/routes/jobs/parity.
- Provider-specific media/pricing/agents hooks remain in platform plan; WorkBuddy-specific behavior stays here.
- Review Focus mapped to Tasks 1/2/3/4/6/7/8/10/12.
- No CodeArts/Raccoon imports or shared credit semantics.