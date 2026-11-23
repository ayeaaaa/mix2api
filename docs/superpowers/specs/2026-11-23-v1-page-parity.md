# V1 long-conversation parity amendment

The user explicitly chose the local V1 implementation after comparing it with PR #52. This supersedes the earlier choice of real-size-three cohorts for long conversations. Prefix canonicalization, safe account filters, bounded state, diagnostics, optional low-traffic model preferences, and official-release-only deployment constraints remain.

## Required behavior
- Defaults remain max_messages=400 and page_size=3, but page_size means **partition count**, as V1 actually implements it.
- For a derived long conversation, compute index = first 8 hex digits of its head digest modulo partition count. Sort realm/allowed account UIDs, then select rank % count == index. Readiness/exclusion must not alter ranks.
- V1's small-realm guard checks the realm's full pool before the key allow-list: if count >= full realm pool size, return to normal sticky selection. An allow-list smaller than count inside a larger realm instead rotates across its whole allowed subset, matching `_page_slice`.
- Within a page, preserve provider priorities (including WorkBuddy manual weights, expiring credits and remaining-weight hooks). Only ordinary fallback is round robin. Page cursors are shared by realm/index/count, not keyed by conversation or model; weighted-state safeguards are preserved.
- No sticky binding for real pages. If the page is unavailable, use normal legal pool fallback, without permanently pinning that page conversation. Explicit session IDs always use existing sticky behavior.
- page_size=1 pins, page_size=0 releases derived affinity for long conversations, max_messages=0 disables the length threshold. Small realm cases also remain sticky.
- Optional low-traffic model preferred pools are an additional opt-in filter, not V1 page semantics; default empty preserves parity.
- UI/docs/PR must say partition count and explain roughly N/count accounts per partition, not promise 3 accounts or a guaranteed cache rate. Preserve strict 11140 and all account/key policies.

## Verification
Literal rank-modulo fixtures for 40 UIDs/3 partitions; small realm vs small key allow-list; shared partition cursors; boundary controls; real provider tests for page rotation, manual/expiry priority, unauthorized/banned/excluded/busy fallback; full make check/make race. Do not mutate live data or deploy custom binaries.
