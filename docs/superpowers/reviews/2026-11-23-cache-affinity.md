# Cache-affinity review and verification

An independent read-only review examined `44fdb05..b22b5d2`. Its initial verdict was **request changes**, with five Important findings and no Critical findings. This is a record of the implementer's single fix pass, not a claim that the reviewer re-reviewed the final code.

## Findings addressed

| Finding | Regression evidence |
|---|---|
| Volatile billing/raw protocol representation caused rebinding | `TestAffinityBillingMetadataDoesNotRebind`, `TestAffinityEquivalentProtocolHeadsShareAccount`: failed before the correction, pass after sanitized semantic projection |
| Derived key became "explicit" on realm fallback | `TestAffinityRealmFallbackRetainsDerivedPage`: failed with one pinned peer account, passes with three rotating peer accounts and correct diagnostics |
| Interleaved model cohorts reset scheduling state | `TestAffinityModelCohortsDoNotResetEachOther`: failed with one repeated account, passes with independent shares |
| Cursor capacity cleared all active pages | `TestAffinityCursorChurnKeepsRecentlyUsedPage`: failed under insertion pressure, passes with incremental LRU |
| Case-insensitive models selected different cohorts | `TestAffinityPolicyCaseAndScopeBound`: failed on spelling variants, passes with normalized/bounded scopes |

The original reviewer's Go-overlay reproductions also pass after the fix. Extra regressions cover semantic image/text blocks, long weighted-state retention under config churn, and existing policy restrictions.

## Decisions

- Use real account-count cohorts, not v1's rank-modulo partition-count behavior. Temporary ineligibility may cause safe group-external fallback.
- Model pool sizes are opt-in; defaults do not silently constrain production accounts. New first turns for unrelated prefixes can still be cold.
- Retain the unconnected remaining-priority value and clearly disable its UI; do not invent a new credit estimator within this cache repair.
- Regrade injected Raccoon config-path and merged-rule accumulation issues as Important: they ignore supported settings or eventually prevent legitimate panel saves. Both now have RED→GREEN tests; disabled rules are pruned and combined active rules validated atomically.
- Oversized routing keys and weighted scheduler state are bounded. Explicit client identifiers are never logged; CodeArts upstream chat IDs remain unchanged.
- Short-prefix experiments do not establish Hy4 long-context support or upstream cache TTL. No real cache-rate guarantee, live config edits, account changes, or production deployment was performed.

No review findings are deferred. After the fix pass, `make check` and `make race` pass (42 test packages, dashboard harness 16/16). Linux builds are verified separately; all tests here use local fixtures, not private production prompts.

## Subsequent user-directed V1 parity

The user originally asked for V1's actual rank-modulo partition behavior instead of the earlier real-size-three cohort design. They then chose to keep the v2.8.16 account-count cohort as the default and expose V1 as an optional global/per-provider strategy. Verification now covers both branches: a 48-account default cohort uses 3 accounts; a V1 partition override with 3 partitions uses 16 accounts in one partition; a 6-account cohort uses 3 accounts, while a V1 partition override uses 2. Tests also cover small-realm sticky behavior versus small allow-lists, V1 zero/one controls, shared page cursors, cohort cursor-before-priority ordering, per-provider settings and priorities before ordinary partition round robin. The original findings about semantic heads, policy filters, bounded state and fallback provenance remain fixed.
