# V1 Page-Parity Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans inline; user already chose V1 behavior. PR #52 merged during the adjustment; delivery is a separate v2.8.16-based follow-up.

**Goal:** Align long-conversation selection with V1 without reverting the cache-routing fixes.
**Architecture:** Shared route policy computes deterministic rank-modulo partitions and the small-realm guard; provider pools run existing priorities before page round robin. Update callers, tests and panel descriptions together.
**Tech Stack:** Existing Go providers/settings, embedded HTML/JS, local fake upstreams.
**Spec:** `docs/superpowers/specs/2026-11-23-v1-page-parity.md`

## Global Constraints
- max_messages=400; page_size=3 means partitions; explicit IDs win; page_size=0/1 and threshold=0 follow V1.
- Preserve authorization, limits, concurrency, strict 11140, semantic prefixes and default-off optional model cohorts.
- No live container/config/data changes; follow up merged PR #52 from v2.8.16.

## Review Focus
- Disabled/cooling/excluded accounts must not shift partition ranks.
- Small realm and small authorized subset must not be conflated.
- Weight/expiry priority must run before page cursor, and unsuccessful page selection must not pin the session.
- Same partition must share a cursor across models/conversations without unbounded state.
- Panel copy must distinguish partition count from optional model account count.

### Task 1: Route and provider selection
- [x] RED: rank-modulo, small-pool/allow-list, shared-page scope and zero-mode tests.
- [x] GREEN: update AffinityRoute.Policy with optional full realm count, deterministic partitions, sticky guard and page eligibility helper; update provider callers/selection ordering.
- [x] Verify provider suites with updated V1 expectations and policy tests.

### Task 2: UI, documentation, delivery
- [x] Update labels/hints, operator guide and PR amendment; synchronize embedded digest.
- [x] Run make check/make race and focused independent review; correct findings. Final full check/race passed after the fixes.
- [x] Commit owned files, push/create follow-up PR #53; preserve worktree and official endpoints. Rebased by cherry-picking only this adjustment onto v2.8.16; historical release notes retained unchanged.

## Review fix record

Focused read-only review found one Important issue: cursor keys still included authorized cohort membership despite equal page scope. `TestV1PageCursorSharedAcrossAllowedSubsets` failed before removing membership from real page cursor identity and passes after.

The review also identified a minor weighted-share discrepancy when alternating models inside one page. The user explicitly requested V1 behavior, so page weighted shares now span models too; `TestV1PageWeightedSharesSpanModels` covers manual and expiry priorities RED→GREEN. Optional low-traffic model cohorts retain their independent model scope. No live data/config/container was modified.
