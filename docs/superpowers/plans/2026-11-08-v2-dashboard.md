# mix2api v2 Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans.

**Goal:** Serve the existing v1.6.5 dashboard unchanged from the v2 binary, preserve panel login/settings/provider views and all API routes the inline dashboard calls, and migrate its JS tests to run against the embedded asset without a Node runtime in production.

**Architecture:** New `internal/web` package embeds `dashboard.html` with `go:embed` and serves `/`, `/dashboard.html`, `/favicon.ico`. Existing `internal/api`/platform routes provide the APIs. JS tests run in development/CI only.

**Spec:** `docs/superpowers/specs/2026-11-08-mix2api-v2-design.md`
**Prior plans:** Foundation/Raccoon, CodeArts, WorkBuddy, Platform services.

## Global Constraints

- v1.6.5 `app/dashboard.html` is copied byte-for-byte into `internal/web/dashboard.html`; the original repo is read-only.
- `/` and `/dashboard.html` serve exact dashboard HTML; no build step or Node runtime in the binary.
- All dashboard fetch paths must exist or return the documented v1 gated/404 envelope; no concrete provider imports in `internal/web`.
- Panel login/logout/password/status, settings, realm, accounts, tasks, activity, usage, logs, pricing, media, agents, updates, proxy, scheduler routes must remain compatible.
- JS tests are CI-only using Node; production `CGO_ENABLED=0` binary must not require Node.
- Preserve CORS/auth behavior and no secret leakage in HTML/static responses.

## Review Focus

1. HTML byte fidelity and embed correctness.
2. Dashboard API inventory: every `fetch`/route referenced by embedded HTML is present and auth-compatible.
3. Panel first-paint/login/logout/password flows.
4. Static asset caching/ETag/content-type and no directory traversal.
5. JS tests actually execute inline dashboard code (no-toolchain runtime dependency).

---

### Task 1: Embedded dashboard and static server

**Files:** `internal/web/dashboard.html`, `internal/web/assets.go`, `internal/api/server.go`, tests.
- Copy v1 dashboard exact; `go:embed`; serve `/`, `/dashboard.html` with text/html, no-store/cache headers matching v1; favicon 404/placeholder; reject traversal.
- Tests hash equality, content type, methods, ETag, no provider imports.

### Task 2: Dashboard API compatibility inventory

**Files:** `tests/dashboard/routes_test.go`, fixes in platform/api routes.
- Parse dashboard HTML for API literals/templates; table of required paths; start real server with auth; assert routes exist (not 404 plain) and envelopes gated.
- Implement/fix missing routes: `/agents/available`, `/accounts/credits/detail`, `/proxy/discover`, `/pricing/mapping`, `/updates`/backups, aliases.
- Tests panel/api-key gates.

### Task 3: Panel flow and provider views

**Files:** `tests/dashboard/panel_test.go`, fixes.
- Login/default password/bootstrap, session cookie/header, logout, password rotation invalidates old, first paint `/panel/status`, settings reveal/save, realm switch, provider account views, no tokens.
- Test with real composition.

### Task 4: JS test migration

**Files:** `tests/dashboard/js_harness_test.go`, `tests/dashboard/node/*`, CI docs.
- Extract inline JS from embedded dashboard; run selected v1 JS tests under Node with DOM stub; skip cleanly if Node absent.
- Cover navigation, panel gate first paint, accounts realm switch, settings load, daily pending, update UI.
- No production Node dependency.

### Task 5: Dashboard contract and Gate E

**Files:** `tests/dashboard/contract_test.go`, `tests/oracle/dashboard.md`.
- Contract for static HTML, auth, routes, JS harness; record v1 dashboard hash/provenance; Gate E checklist.
- Full tests/race/vet/gofmt/CGO=0/Windows build.

## Gate E

Dashboard HTML hash matches v1; all referenced routes present; panel flows pass; JS harness green or documented Node prerequisite; no secrets; binary CGO=0.
