# CodeArts Station Extraction (Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce a `Station` profile for CodeArts and route all host/settings/account/folder differences through it, without changing domestic behavior or registering the international provider yet.

**Architecture:** One `codearts` implementation owns a `Station` value. `New(store)` remains the mainland-compatible constructor; `NewWithStation(store, station)` is the new constructor. Existing package-level helpers keep mainland defaults for tests, while provider methods use the instance station. Phase 2 will add the international profile and second registry instance.

**Tech Stack:** Go 1.x, existing mix2api provider/core packages, standard `testing`.

**Spec:** `docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md`

## Global Constraints

- Domestic `codearts` behavior, routes, settings key, account directory, API-key realm binding, and tests must not change.
- International provider is **not registered** in this phase.
- Do not copy a second `codearts` package.
- Do not change mainland `stop` normalization; existing behavior is locked.
- Do not change domestic model seeds or benefit/checkin behavior.
- No secrets in code, tests, logs, or docs.
- Every task ends with `go test ./internal/providers/codearts` (or the narrower package under change) passing.

## Review Focus

- A mainland provider must still use `data/accounts/codearts` and `settings["codearts"]`; a regression here would orphan existing accounts.
- Package-level helpers such as `snapHost()` and `portalHost()` must keep mainland defaults so existing tests and callers do not break.
- OAuth state/secret/ticket routing must remain per-provider; do not share mutable session tables between future stations.
- `Auth` JSON must round-trip old documents without a `station` field.
- Error text and redaction must not start including station-specific secrets.

---

### Task 1: Station type and profiles

**Files:**
- Create: `internal/providers/codearts/station.go`
- Test: `internal/providers/codearts/station_test.go`

**Interfaces:**
- Produces: `type Station struct { ... }`, `var Mainland Station`, `var International Station`, `func (s Station) SnapManagerBase() string`.

- [ ] **Step 1: Write the failing test**

```go
func TestStationProfiles(t *testing.T) {
    if Mainland.Realm != "codearts" || Mainland.SettingsKey != "codearts" || Mainland.AccountsFolder != "codearts" {
        t.Fatalf("mainland identity = %#v", Mainland)
    }
    if Mainland.SnapHost != "https://snap-access.cn-north-4.myhuaweicloud.com" ||
        Mainland.Portal != "https://codearts.huaweicloud.com/portal" ||
        Mainland.Locale != "zh-cn" ||
        !Mainland.SupportsBenefit || !Mainland.SupportsCheckin {
        t.Fatalf("mainland profile = %#v", Mainland)
    }
    if International.Realm != "codearts-intl" || International.SettingsKey != "codearts-intl" || International.AccountsFolder != "codearts-intl" {
        t.Fatalf("international identity = %#v", International)
    }
    if International.SnapHost != "https://snap-access.ap-southeast-1.myhuaweicloud.com" ||
        International.Portal != "https://codearts.ap-southeast-1.huaweicloud.com/portal" ||
        International.LoginURL != "https://auth.huaweicloud.com/authui/login?site=intl" ||
        International.Locale != "en-us" ||
        International.SupportsBenefit || International.SupportsCheckin {
        t.Fatalf("international profile = %#v", International)
    }
    if got := International.SnapManagerBase(); got != International.SnapHost+"/snap-manager" {
        t.Fatalf("SnapManagerBase = %q", got)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/codearts -run TestStationProfiles -v`
Expected: FAIL because `Station`/`Mainland`/`International` are undefined.

- [ ] **Step 3: Implement `station.go`**

Define at least these fields:

```go
type Station struct {
    ID              string // "mainland" / "international"
    ProviderID      string // "codearts" / "codearts-intl"
    Realm           string
    SettingsKey     string
    AccountsFolder  string
    SnapHost        string
    BenefitHost     string // empty for international
    Portal          string
    LoginURL        string // empty for mainland; intl auth page
    Locale          string
    SupportsBenefit bool
    SupportsCheckin bool
    QuotaUnit       string // "credit" / "token"
    RoutePrefix     string // "/codearts" / "/codearts-intl"
    ModelSeeds      []string
}
```

`Mainland` uses today's constants. `International` uses the values in the spec. `SnapManagerBase` trims the trailing slash and appends `/snap-manager`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/providers/codearts -run TestStationProfiles -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/station.go internal/providers/codearts/station_test.go
git commit -m "feat(codearts): add station profiles"
```

### Task 2: Provider station wiring and constructors

**Files:**
- Modify: `internal/providers/codearts/provider.go`
- Test: `internal/providers/codearts/provider_test.go` (create if absent)

**Interfaces:**
- Produces: `func New(store *config.Store) (*Provider, error)` (unchanged mainland behavior), `func NewWithStation(store *config.Store, station Station) (*Provider, error)`, `func (p *Provider) Station() Station`, `func (p *Provider) ID() string`, `func (p *Provider) Realm() string`.

- [ ] **Step 1: Write the failing test**

```go
func TestProviderUsesStationIdentity(t *testing.T) {
    store := newTestStore(t)
    cn, err := New(store)
    if err != nil { t.Fatal(err) }
    if cn.ID() != "codearts" || cn.Realm() != "codearts" || cn.Station().ID != "mainland" {
        t.Fatalf("mainland identity = %q/%q/%q", cn.ID(), cn.Realm(), cn.Station().ID)
    }
    intl, err := NewWithStation(store, International)
    if err != nil { t.Fatal(err) }
    if intl.ID() != "codearts-intl" || intl.Realm() != "codearts-intl" || intl.Station().ID != "international" {
        t.Fatalf("intl identity = %q/%q/%q", intl.ID(), intl.Realm(), intl.Station().ID)
    }
}
```

Use the existing test store helper if one exists; otherwise create `config.New(filepath.Join(t.TempDir(), "settings.json"))`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/codearts -run TestProviderUsesStationIdentity -v`
Expected: FAIL because `NewWithStation`, `Station()`, or the station fields are missing.

- [ ] **Step 3: Implement**

- Add `station Station` to `Provider`.
- `New(store)` calls `NewWithStation(store, Mainland)`.
- Move the existing constructor body to `NewWithStation`; set `station: station`.
- `ID()` returns `p.station.ProviderID`; `Realm()` returns `p.station.Realm`.
- `Station()` returns `p.station`.
- Leave `ProviderID`/`ProviderRealm` constants for domestic tests/back-compat.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestProviderUsesStationIdentity|TestCodeArts' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/provider.go internal/providers/codearts/provider_test.go
git commit -m "feat(codearts): wire provider station identity"
```

### Task 3: Settings key parameterization

**Files:**
- Modify: `internal/providers/codearts/config.go`
- Modify: `internal/providers/codearts/provider.go`
- Modify: `internal/providers/codearts/routes.go` (SaveConfig call sites)
- Test: `internal/providers/codearts/config_test.go`

**Interfaces:**
- Produces: `func LoadConfigForStation(store *config.Store, station Station) Config`, `func SaveConfigForStation(store *config.Store, station Station, patch map[string]any) (Config, error)`.
- Keeps: `LoadConfig(store)`, `SaveConfig(store, patch)` as mainland wrappers.

- [ ] **Step 1: Write the failing test**

```go
func TestConfigUsesStationSettingsKey(t *testing.T) {
    store := newTestStore(t)
    if _, err := SaveConfigForStation(store, International, map[string]any{"enabled": true, "max_concurrent": 2}); err != nil { t.Fatal(err) }
    intl := LoadConfigForStation(store, International)
    if !intl.Enabled || intl.MaxConcurrent != 2 { t.Fatalf("intl = %#v", intl) }
    cn := LoadConfigForStation(store, Mainland)
    if cn.Enabled { t.Fatalf("mainland enabled = true, want false") }
    settings, _ := store.Load()
    if _, ok := settings["codearts"]; ok { t.Fatalf("mainland section unexpectedly written: %#v", settings["codearts"]) }
    if _, ok := settings["codearts-intl"]; !ok { t.Fatalf("codearts-intl section missing") }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/codearts -run TestConfigUsesStationSettingsKey -v`
Expected: FAIL because `LoadConfigForStation`/`SaveConfigForStation` are undefined.

- [ ] **Step 3: Implement**

- Add `settingsKey string` parameter internally; `LoadConfig(store)` -> `LoadConfigForStation(store, Mainland)`.
- `SaveConfig(store, patch)` -> `SaveConfigForStation(store, Mainland, patch)`.
- Replace `settings["codearts"]` with `settings[station.SettingsKey]`.
- Replace `store.Merge(map[string]any{"codearts": ...})` with `map[string]any{station.SettingsKey: ...}`.
- Provider `applySettings` uses `LoadConfigForStation(p.store, p.station)`.
- `saveAccountAndEnable` and route handlers use `SaveConfigForStation(p.store, p.station, patch)`.
- Keep the domestic-only keys in `Config`; international defaults can ignore them in this phase.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestConfig|TestSaveConfig|TestProviderUsesStationIdentity' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/config.go internal/providers/codearts/provider.go internal/providers/codearts/routes.go internal/providers/codearts/config_test.go
git commit -m "feat(codearts): parameterize settings by station"
```

### Task 4: Account folder parameterization

**Files:**
- Modify: `internal/providers/codearts/pool.go`
- Modify: `internal/providers/codearts/auth.go`
- Modify: `internal/providers/codearts/provider.go`
- Modify: `internal/providers/codearts/routes.go`
- Test: `internal/providers/codearts/pool_test.go`, `internal/providers/codearts/auth_test.go`

**Interfaces:**
- Produces: `func NewPoolForStation(dir string, station Station, cfg Config) (*Pool, error)`, `func AuthPathForStation(accountsDir string, station Station, uid string) (string, error)`.
- Keeps: `NewPool(dir, cfg)`, `AuthPath(accountsDir, uid)` as mainland wrappers.

- [ ] **Step 1: Write the failing test**

```go
func TestPoolUsesStationFolder(t *testing.T) {
    dir := t.TempDir()
    cn, err := NewPoolForStation(dir, Mainland, defaultConfig())
    if err != nil { t.Fatal(err) }
    intl, err := NewPoolForStation(dir, International, defaultConfig())
    if err != nil { t.Fatal(err) }
    if cn.folder == intl.folder { t.Fatalf("folders equal: %q", cn.folder) }
    if cn.folder != filepath.Join(dir, "codearts") { t.Fatalf("cn folder = %q", cn.folder) }
    if intl.folder != filepath.Join(dir, "codearts-intl") { t.Fatalf("intl folder = %q", intl.folder) }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/codearts -run TestPoolUsesStationFolder -v`
Expected: FAIL because the constructor is undefined.

- [ ] **Step 3: Implement**

- Add `accountsFolder string` to `Pool`.
- `NewPoolForStation` sets `folder = filepath.Join(dir, station.AccountsFolder)` and `statePath = filepath.Join(folder, "state.json")`.
- `NewPool` wraps `NewPoolForStation(dir, Mainland, cfg)`.
- `AuthPathForStation` uses `filepath.Join(accountsDir, station.AccountsFolder, "codearts-"+safeUID+".json")`; keep `AuthPath` mainland wrapper.
- Replace `AuthPath(p.dir, ...)` in pool with `AuthPathForStation(p.dir, p.station, ...)`; add `station` to `Pool`.
- Provider constructs `NewPoolForStation(accountsDir, p.station, cfg)`.
- Route import `AuthPath(p.accountsDir, uid)` -> `AuthPathForStation(p.accountsDir, p.station, uid)`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestPool|TestAuth|TestProviderUsesStationIdentity' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/pool.go internal/providers/codearts/auth.go internal/providers/codearts/provider.go internal/providers/codearts/routes.go internal/providers/codearts/pool_test.go internal/providers/codearts/auth_test.go
git commit -m "feat(codearts): isolate account folders by station"
```

### Task 5: OAuth host/portal/STS parameterization

**Files:**
- Modify: `internal/providers/codearts/oauth.go`
- Modify: `internal/providers/codearts/provider.go`
- Test: `internal/providers/codearts/oauth_test.go`

**Interfaces:**
- Produces: `func NewOAuthSessionsForStation(station Station) *OAuthSessions`, `func (s *OAuthSessions) Station() Station`.
- Keeps: `NewOAuthSessions()` as mainland wrapper; package-level `portalHost()`, `snapHost()`, `stsHost()` as mainland-compatible helpers.

- [ ] **Step 1: Write the failing test**

```go
func TestOAuthAuthorizeURLUsesStation(t *testing.T) {
    intl := NewOAuthSessionsForStation(International)
    url, err := intl.AuthorizeURL(12345, "challenge", "ticket")
    if err != nil { t.Fatal(err) }
    if !strings.Contains(url, "https://codearts.ap-southeast-1.huaweicloud.com/portal/authorize?") { t.Fatalf("url = %s", url) }
    if !strings.Contains(url, "locale=en-us") { t.Fatalf("url = %s", url) }
    if strings.Contains(url, "site=intl") || strings.Contains(url, "auth.huaweicloud.com") { t.Fatalf("authorize URL should stay raw; login wrapper is separate: %s", url) }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/codearts -run TestOAuthAuthorizeURLUsesStation -v`
Expected: FAIL because `NewOAuthSessionsForStation`/`AuthorizeURL` are undefined.

- [ ] **Step 3: Implement**

- Add `station Station` to `OAuthSessions`; `NewOAuthSessions()` uses `Mainland`; `NewOAuthSessionsForStation` sets it.
- Turn `oauthAuthorizeURL` into a method using `s.station.Portal`, `s.station.Locale`, and `s.station.ProviderID`/client id.
- `Begin` uses `s.station`.
- `pollTicket` uses `s.station.SnapManagerBase()`.
- `Continue` validates redirect against `s.station.Portal`.
- identity reads use `s.station` host.
- `RequestToken` uses `s.station.STS`.
- Provider creates `NewOAuthSessionsForStation(p.station)`.
- Add `station.LoginURL` support as a **composer** for Phase 5, not active yet: `func (s *OAuthSessions) LoginURL(authorizeURL string) string` returns the auth-page wrapper when `LoginURL` is non-empty.
- Keep package-level helpers returning mainland defaults for existing tests.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestOAuth|TestProviderUsesStationIdentity' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/oauth.go internal/providers/codearts/provider.go internal/providers/codearts/oauth_test.go
git commit -m "feat(codearts): parameterize oauth by station"
```

### Task 6: Chat host/header/security-token parameterization

**Files:**
- Modify: `internal/providers/codearts/chat.go`
- Modify: `internal/providers/codearts/auth.go` (add `SecurityToken`)
- Test: `internal/providers/codearts/chat_test.go`, `internal/providers/codearts/auth_test.go`

**Interfaces:**
- Produces: `Auth.SecurityToken string`; `SignCredentials` uses `SecurityToken` when present, otherwise `CloudDragonToken`; chat uses `p.station.SnapHost` and `p.station.Locale`.
- Keeps: mainland `x-auth-token` and `Content-Type` signing behavior.

- [ ] **Step 1: Write the failing tests**

```go
func TestIntlChatUsesStationHostAndSecurityToken(t *testing.T) {
    p, _ := NewWithStation(newTestStore(t), International)
    account := testAccount(t)
    account.Auth.CloudDragonToken = ""
    account.Auth.SecurityToken = "sts"
    req, err := p.newChatRequest(context.Background(), account, "GLM-5.2", "chat", "session", []byte(`{"model":"GLM-5.2","messages":[]}`))
    if err != nil { t.Fatal(err) }
    if req.URL.Host != "snap-access.ap-southeast-1.myhuaweicloud.com" { t.Fatalf("host = %s", req.URL.Host) }
    if req.Header.Get("X-Language") != "en-us" { t.Fatalf("language = %q", req.Header.Get("X-Language")) }
    if req.Header.Get("x-auth-token") != "" { t.Fatalf("x-auth-token = %q, want empty", req.Header.Get("x-auth-token")) }
    if req.Header.Get("X-Security-Token") != "sts" { t.Fatalf("security token = %q", req.Header.Get("X-Security-Token")) }
}
```

Add a round-trip test: old Auth JSON without `security_token` still loads and falls back to `CloudDragonToken`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/codearts -run 'TestIntlChatUsesStationHostAndSecurityToken|TestAuth.*Security' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Add `SecurityToken` with JSON `security_token,omitempty`, known-key handling, and unknown-field preservation.
- `newChatRequest` uses `p.station.SnapHost`, `p.station.Locale`.
- `x-auth-token` only when `CloudDragonToken != ""`.
- `SignCredentials.SecurityToken` uses `SecurityToken` if non-empty, else `CloudDragonToken`.
- Keep current `stop` normalization; do not add station-specific behavior.
- Keep `Content-Type` signing unchanged; add `SignOptions` only if Task 7's golden test proves it is needed.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestChat|TestAuth|TestProviderUsesStationIdentity' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/chat.go internal/providers/codearts/auth.go internal/providers/codearts/chat_test.go internal/providers/codearts/auth_test.go
git commit -m "feat(codearts): station-aware chat auth"
```

### Task 7: Catalog host/benefit parameterization

**Files:**
- Modify: `internal/providers/codearts/catalog.go`
- Modify: `internal/providers/codearts/models.go`
- Test: `internal/providers/codearts/catalog_test.go`, `internal/providers/codearts/models_test.go` (create if absent)

**Interfaces:**
- Produces: provider-level `snapHost()`, `benefitHost()`, `snapManagerBase()` helpers using `p.station`; package-level helpers remain mainland defaults.
- Keeps: domestic catalog sources and seeds unchanged.

- [ ] **Step 1: Write the failing tests**

```go
func TestIntlCatalogSkipsBenefitHost(t *testing.T) {
    p, _ := NewWithStation(newTestStore(t), International)
    if p.benefitHost() != "" { t.Fatalf("benefit host = %q, want empty", p.benefitHost()) }
    if p.snapHost() != International.SnapHost { t.Fatalf("snap host = %q", p.snapHost()) }
}
```

Add a test that the mainland provider still reports the existing benefit host and seeds.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/codearts -run 'TestIntlCatalogSkipsBenefitHost|TestCatalog.*Mainland' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Add provider methods `snapHost()`, `benefitHost()`, `snapManagerBase()`.
- Replace provider call sites in `catalog.go`, `credits.go`, `checkin.go`, `provider.go`, `routes.go` with the methods.
- If `p.station.BenefitHost == ""`, skip benefit fetch/claim entirely.
- Keep `officialCoreModels`, `seedKnownModels`, and domestic model rows unchanged.
- Add `p.station.ModelSeeds` only as a cold-start fallback; do not merge it into every response.
- Do not register intl yet.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestCatalog|TestModels|TestCredits|TestCheckin' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/catalog.go internal/providers/codearts/models.go internal/providers/codearts/catalog_test.go internal/providers/codearts/models_test.go
git commit -m "feat(codearts): station-aware catalog hosts"
```

### Task 8: Usage/checkin/jobs station gating

**Files:**
- Modify: `internal/providers/codearts/credits.go`
- Modify: `internal/providers/codearts/checkin.go`
- Modify: `internal/providers/codearts/jobs.go`
- Modify: `internal/providers/codearts/provider.go`
- Test: `internal/providers/codearts/credits_test.go`, `internal/providers/codearts/jobs_test.go`

**Interfaces:**
- Produces: `Provider` checks `station.SupportsCheckin` before registering/executing check-in jobs; usage uses station quota unit.
- Keeps: mainland credit parsing/check-in unchanged.

- [ ] **Step 1: Write failing tests**

```go
func TestIntlHasNoCheckinJob(t *testing.T) {
    p, _ := NewWithStation(newTestStore(t), International)
    for _, job := range p.Jobs() {
        if strings.Contains(strings.ToLower(job.Name()), "checkin") { t.Fatalf("intl job = %#v", job) }
    }
}
```

Add a token-quota parsing test with a fixture from the spec; assert `Credits.Total.Unit == "token"` and remain/limit.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/codearts -run 'TestIntlHasNoCheckinJob|TestTokenQuota' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `Jobs()` returns no check-in job when `!p.station.SupportsCheckin`.
- `handleCheckin` / check-in service returns a clear “not available on this station” error for intl.
- Add `parseTokenQuota` in `credits.go` for `is_token_package` + `usageTokenChatMessages`; set `AccountCredits.Total` with `Unit: "token"`.
- `reserveBlockedLocked` only applies `reserveCredits` when `Info.Credits.Unit != "token"`.
- Keep domestic credit parsing unchanged.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/providers/codearts -run 'TestCredits|TestCheckin|TestJobs|TestTokenQuota' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codearts/credits.go internal/providers/codearts/checkin.go internal/providers/codearts/jobs.go internal/providers/codearts/provider.go internal/providers/codearts/credits_test.go internal/providers/codearts/jobs_test.go
git commit -m "feat(codearts): station-aware jobs and quota"
```

### Task 9: Phase 1 verification and documentation

**Files:**
- Modify: `docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md` only if implementation changes a decision.
- Test: full package tests.

- [ ] **Step 1: Run package tests**

Run: `go test ./internal/providers/codearts ./internal/app`
Expected: PASS.

- [ ] **Step 2: Run vet**

Run: `go vet ./internal/providers/codearts ./internal/app`
Expected: PASS.

- [ ] **Step 3: Run broader regression**

Run: `go test ./internal/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/providers/codearts internal/app docs/superpowers/specs/2026-11-25-codearts-intl-station-design.md
git commit -m "refactor(codearts): extract station profile"
```