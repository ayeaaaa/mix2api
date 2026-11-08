# mix2api v2.0 Master Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a complete Go v2.0 replacement for mix2api v1.6.5, with compile-time provider modules, full feature parity, and a safe cutover.

**Architecture:** Single Go process. `internal/core` holds shared infrastructure; `internal/providers/{workbuddy,codearts,raccoon}` are compile-time modules registered in `cmd/mix2api`; `internal/api` owns protocol routes; `internal/web` embeds the existing dashboard assets.

**Tech Stack:** Go (latest stable at implementation start), standard library first, pure-Go SQLite for `logs.sqlite3` compatibility, `go:embed`, `net/http`, `httptest`, `go test -race`.

**Spec:** `docs/superpowers/specs/2026-11-08-mix2api-v2-design.md`

## Global Constraints

- Original `mix2api` repository stays at v1.6.5 and is never modified by v2 work.
- Single process, single binary, `CGO_ENABLED=0`; no Node/Python runtime.
- Compile-time provider modules only; no Go `plugin`, no hot reload.
- Preserve old paths, JSON field names, status codes, error bodies and SSE semantics.
- Preserve old data files and formats; never share a live account directory between v1 and v2.
- `realm=all` remains WorkBuddy-only; no cross-provider fallback or model merging.
- Provider modules may depend on `core`, never on each other.
- Automated tests use fake upstreams and synthetic/desensitized fixtures only.
- Real-account smoke tests are manual and may mutate state only on dedicated test accounts.
- v2 releases initially come from the new repository; v1.6.5 remains the old repo's latest.
- No MVP release; v2.0.0 ships only after full parity gates pass.

## Subsystem Plans

This program is split into independently testable plans. Execute in order; each plan produces a working slice. Detailed plans are written immediately before their phase, after the previous gate passes. Plan 1 is written now.

1. **Foundation + Raccoon** (`2026-11-08-v2-foundation-raccoon.md`, written) — Go scaffold, core, provider contract, Raccoon vertical slice, generic chat/SSE, contract harness.
2. **CodeArts** (`2026-11-08-v2-codearts.md`, written) — OAuth/DPoP, catalog, chat, credits, check-in, keepalive, zero-credit/failover, admin routes.
3. **WorkBuddy** (`2026-11-08-v2-workbuddy.md`, written) — intl/cn accounts, OAuth, identity, catalog, credits/packages, check-in, daily activity, growth tasks, Buddy/cat/streak, failover.
4. **Platform services** (to write after Gate C) — API protocols beyond chat, usage, pricing, logs, activity, media, agents, web tools, probes, update.
5. **Dashboard** (after Gate D) — embedded dashboard compatibility, panel auth/settings, provider views, JS test migration.
6. **Packaging / parity / release** (after Gate E) — scheduler wiring, Docker/Windows/OpenWrt, update/rollback, oracle parity suite, performance, migration, v2.0.0 release.

## Cross-Plan Interfaces

```go
// internal/providers/base.go
type Provider interface {
    ID() string
    Realm() string
    Overview(context.Context, bool) (any, error)
    Health(context.Context, bool) (Health, error)
    Models(context.Context, bool) ([]Model, error)
    OpenChat(context.Context, ChatRequest) (ChatStream, error)
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
type ChatStream interface {
    Next() ([]byte, error)
    Close() error
}
type Health = map[string]any
type Model = map[string]any

type AuthClass int
const (
    AuthNone AuthClass = iota
    AuthAPIKey
    AuthPanel
)
type Route struct {
    Method  string
    Path    string
    Auth    AuthClass
    Handler http.HandlerFunc
}
```

```go
// internal/core/accountstate
type State struct {
    UID              string
    Disabled         bool
    ManualDisabled   bool
    Reason           string
    CoolUntil        time.Time
    LastUsed         time.Time
    Note             string
    TasksEnabled     bool
    ActiveConcurrent int
    MaxConcurrent    int
}
```

The `Route` type lives in the provider contract package, not `internal/api`, so `internal/api` can import providers without a cycle. `scheduler.Job` lives in `internal/core/scheduler`; that package must not import providers.

`ChatRequest.Raw` and `ChatRequest.Headers` always carry the original request. Providers own provider-specific normalization.

## Global Review Focus

These are the five failure classes most likely to break a real deployment; every owning plan must test them:

1. **Streaming termination/cancellation** — client disconnect, upstream EOF, keepalive, finish frame, `[DONE]`, no double close/release.
2. **Account state races** — manual disable, token refresh, model discovery, zero-credit, cooldown, check-in all touching the same account.
3. **Data-format corruption** — unknown settings keys, account state, usage append, SQLite logs, v1↔v2 round-trip.
4. **Provider semantic leakage** — WorkBuddy credits/identity/tasks used for CodeArts/Raccoon, or cross-provider model/failover bleed.
5. **Packaging drift** — embedding dashboard, SQLite choice, multi-arch builds, update/rollback, missing files in portable/OpenWrt assets.

## Program Gates

- Gate A (after plan 1): Raccoon vertical slice passes contract tests against v1.6.5 oracle.
- Gate B (after plan 2): CodeArts parity passes provider + contract tests.
- Gate C (after plan 3): WorkBuddy parity passes provider + contract tests.
- Gate D (after plan 4): all non-UI features pass; media/agents/update contract green.
- Gate E (after plan 5): dashboard and panel parity green.
- Gate F (after plan 6): packaging, performance, migration, real-account smoke, release candidate.

## Execution Recommendation

Use **subagent-driven development**: each plan task gets a fresh implementer and a fresh reviewer; provider and packaging tasks depend on shared interfaces and are high-risk. Do not batch unrelated providers into one task.

## Self-Review

- Spec coverage: every design section maps to one of the six plans; media/pricing/agents/update are in plan 4 and 6; dashboard in 5; packaging/parity in 6.
- Type consistency: `Provider`, `State`, `Route`, `ChatRequest` are defined once here and referenced by all plans.
- Review Focus entries are assigned to tests in the owning tasks.
- No production code is written by this master plan.