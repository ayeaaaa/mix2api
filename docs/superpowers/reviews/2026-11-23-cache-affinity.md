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

The user subsequently chose V1's actual rank-modulo partition behavior instead of the earlier real-size-three cohort design. Those old cardinality expectations are intentionally superseded: a 48-account pool with 3 partitions rotates over 16 accounts in one partition; a 6-account pool rotates over 2. Current verification additionally covers small-realm sticky behavior versus small allow-lists, V1 zero/one controls, shared page cursors, and priorities before ordinary round robin. The original findings about semantic heads, policy filters, bounded state and fallback provenance remain fixed.
