# V1 Page-Parity as an Optional Strategy — Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans inline. PR #53 was
> originally a v2.8.16 follow-up replacing long-page behavior; the user then
> asked to keep the v2.8.16 default and expose V1 as an option. This plan covers
> the revision on the same PR branch.

**Goal:** Add a global and per-provider long-conversation strategy selector,
keeping the v2.8.16 account-count cohort as the default and V1 rank-modulo
partitioning as an opt-in. Preserve all cache-routing fixes.

**Architecture:** `AffinityOptions` resolves global `affinity_long_strategy`
(`cohort`/`partition`) and `affinity_page_size`, then applies optional
per-provider strategy/page-size overrides through `ForProvider`. Route policies
carry `Page`/`Partition` so provider pools keep the v2.8.16 branch for cohort
mode and the V1 branch for partition mode. Settings validation, the panel and
operator docs expose the hierarchy.

**Tech Stack:** Existing Go providers/settings, embedded HTML/JS, local fake
upstreams.

**Spec:** `docs/superpowers/specs/2026-11-23-v1-page-parity.md`

## Global Constraints

- Default `affinity_long_strategy=cohort`; max_messages=400; page_size=3.
- `cohort` preserves v2.8.16; `partition` preserves V1 rank-modulo behavior.
- Per-provider overrides: `workbuddy` (both realms), `codearts`, `raccoon`;
  unset/empty/`-1` inherits global.
- Preserve authorization, limits, concurrency, strict 11140, semantic prefixes
  and default-off optional model cohorts.
- No live container/config/data changes; do not deploy custom binaries.

## Review Focus

- Default cohort must not silently acquire V1's partition count or small-pool
  sticky guard.
- Cohort page cursor ordering must match v2.8.16 (WorkBuddy cursor before
  priorities; CodeArts/Raccoon cursor account before ordinary ordering).
- Partition mode must keep V1 ranks, shared cursors, priorities-before-cursor,
  small-realm guard and safe non-sticky fallback.
- Per-provider resolution must be applied by every provider caller and be
  merge-safe/clearable in settings.
- Panel must render/save global and per-provider choices, with clear labels.

### Task 1: Shared options and policy
- [x] RED: option defaults/overrides, global cohort policy, V1 policy,
  small-realm differences, zero-page key behavior.
- [x] GREEN: add `LongStrategy`, provider maps, `ForProvider`, strategy-aware
  `Policy`, `Page`/`Partition`, and conditional zero-mode key handling.

### Task 2: Provider selection
- [x] RED: WorkBuddy default cohort, provider partition override, cohort cursor
  before manual priority; CodeArts/Raccoon default cohort and V1 override.
- [x] GREEN: resolve options per provider; keep v2.8.16 cohort selection and use
  `AffinityPolicy.UsesV1PriorityOrder()` (`Partition && (Page || Rotate)`) for
  partition pages and the partition zero-page model-cohort rotation.

### Task 3: Settings, panel, docs
- [x] RED: settings defaults/save/validation/merge-clear; panel render/save.
- [x] GREEN: settings view/validation; global dropdown, per-provider controls,
  dynamic labels/help; docs/changelog/spec.
- [x] Run focused suites, `make check`, `make race`, panel suites and review.
- [x] Commit, update PR #53 title/body, and push.
- [x] Final focused review: ready to merge after the provider count-label and placeholder fixes.
- [ ] Wait for owner merge and an official release; do not deploy a custom build.

## Historical review fix record

The original V1-only branch's focused review found cursor membership leaking
into page identity; it was fixed and covered by
`TestV1PageCursorSharedAcrossAllowedSubsets`. Weighted page shares were also
aligned with V1 across models. Those fixes remain valid for partition mode.
The cohort branch retains the v2.8.16 cursor identity and priority ordering.
