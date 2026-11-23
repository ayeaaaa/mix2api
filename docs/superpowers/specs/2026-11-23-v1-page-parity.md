# V1 long-conversation strategy (optional)

The user originally chose local V1's rank-modulo paging after comparing it with
PR #52. They then asked to keep the already-shipped v2.8.16 behavior as the
default and expose V1 as an alternative, selectable globally and per provider.
Prefix canonicalization, safe account filters, bounded state, diagnostics,
optional low-traffic model preferences, and official-release-only deployment
constraints remain.

## Required behavior

- Defaults remain `affinity_max_messages=400` and `affinity_page_size=3`.
  The new `affinity_long_strategy` defaults to `cohort`, which preserves the
  v2.8.16 at-most-N-account behavior. `partition` selects V1 rank-modulo mode.
- `affinity_provider_strategies` optionally overrides the strategy for
  `workbuddy` (both intl and cn), `codearts`, or `raccoon`. Missing or empty
  values inherit the global strategy. `affinity_provider_page_sizes` optionally
  overrides the count; missing or `-1` inherits the global count. The count's
  meaning follows the provider's resolved strategy.
- Cohort mode preserves v2.8.16: select at most N accounts by rendezvous
  ranking seeded by the conversation/model scope; the page cursor is per
  conversation/model; WorkBuddy's cursor precedes manual/expiry priorities;
  CodeArts/Raccoon select the cursor account before their ordinary ordering.
  An unavailable cohort falls back safely without sticky binding.
- Partition mode matches V1: index = first 8 digest hex digits modulo partition
  count; sort realm/allowed UIDs and select rank % count == index. Readiness and
  exclusions do not alter ranks. V1's small-realm guard uses the full realm pool
  before the key allow-list; a smaller allow-list in a larger realm rotates its
  whole allowed subset without pinning. Provider priorities run before the
  shared page cursor (WorkBuddy manual/expiry shares span models).
- No sticky binding for real pages. If a page is unavailable, use normal legal
  pool fallback. Explicit session IDs always use existing sticky behavior.
- In partition mode, `page_size=1` pins, `page_size=0` releases derived
  affinity; in cohort mode, `1` pins and `0` keeps the derived key but uses
  whole-pool rotation. `max_messages=0` disables the threshold in both modes.
- Optional low-traffic model preferred pools remain an opt-in filter, not long
  page/partition semantics; default empty preserves existing behavior.
- UI/docs/PR must label the default as account count and the optional V1 mode as
  partition count. Preserve strict 11140 and all account/key policies. Do not
  mutate live data or deploy custom binaries.

## Verification

Tests cover default cohort behavior, V1 rank-modulo fixtures, global and
per-provider overrides, small realm versus small allow-list, shared partition
cursors, zero-page key handling, priority ordering, settings validation/view,
panel render/save payloads, and full `make check`/`make race`.
