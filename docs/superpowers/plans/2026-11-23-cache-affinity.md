# Complete Cache Affinity Implementation Plan

The long-page choice in this completed plan was extended by the user's V1-parity request in `2026-11-23-v1-page-parity.md`. That V1 behavior is now an optional global/per-provider strategy; the v2.8.16 cohort remains the default.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans. Track progress here.

**Goal:** Finish PR #52 without a deferred cache-routing phase.
**Architecture:** Shared affinity options/routing plans and deterministic cohort helpers feed provider-specific selection, preserving authorization and upstream contracts. Settings/UI and privacy-safe usage diagnostics expose actual behavior.
**Tech Stack:** Go, existing JSON config store, embedded dashboard, local HTTP test servers.
**Spec:** `docs/superpowers/specs/2026-11-23-cache-affinity-design.md`

## Global Constraints
- Official v2.8.15 endpoints untouched; no live deployments until official release.
- Defaults: prefix=true, stable=false, max_messages=400, page_size=3, model_pool_sizes={}; size bounds 0–64.
- Explicit IDs/policy restrictions/11140 win; no credential or usage-history edits.

## Review Focus
- Multiple leading system/developer messages and user-only transcripts must not collide or change keys on later turns.
- Busy, rate-limited, disabled, non-paid, or non-authorized accounts must never bypass existing filters.
- Changing cohort membership/other models must not reset the current page cursor each turn.
- Raw protocol fields and synthesized Messages/Responses transcripts must yield the same provider routing.
- Logs must contain only irreversible prefix/session hashes, not prompt text or identifiers.

### Task 1: Shared routing primitives
- [x] RED: tests for semantic stable heads, malformed/no-user input, options/defaults/env precedence, explicit keys, long threshold, deterministic size-3 cohorts.
- [x] GREEN: `internal/providers/session.go` and `affinity.go`: shared options, route and cohort helpers; keep existing public helpers compatible.
- [x] Verify `go test ./internal/providers`.

### Task 2: Provider selection and diagnostics
- [x] RED: multi-account provider tests for bounded long rotation, explicit precedence, fallback and scoped model cohorts.
- [x] GREEN: provider chat/pool paths consume options and preferred cohorts; preserve upstream IDs; add safe routing snapshots/ledger fields.
- [x] Verify provider tests plus protocol integration regressions (Chat/Responses/Messages).

### Task 3: Settings and UI
- [x] RED: defaults/save validation/preservation tests and dashboard wiring tests; unsupported remaining priority explicitly false.
- [x] GREEN: shared config save/view, existing automation controls with page/model cohort settings and accurate hints; synchronize asset digest.
- [x] Add operator docs and changelog (no unsupported performance guarantees).

### Task 4: Verification and delivery
- [x] Run `make check`, `make race`, `git diff --check`.
- [x] Independent whole-branch review and address concrete findings; reviewer reproductions and persistent regression tests now pass.
- [x] Commit only owned changes, push/update PR #52; report precise test and deployment status.
