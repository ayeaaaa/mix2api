# Complete cache-affinity repair

## Intent
Finish PR #52 in one change: prevent unlabelled conversations from spraying account-scoped prefixes over the pool, bound long-conversation fan-out, support opt-in low-traffic model cohorts, and make diagnostic/settings behavior honest.

## Approaches
- Explicit client session IDs alone: precise, but existing clients omit them; insufficient.
- Copy v1 verbatim: includes a rank-modulo "page size" that actually means partition count, not account count; rejected.
- Provider-side prefix routing plus bounded deterministic cohorts: chosen by the implementer under the user's request to finish without another phase. Existing filters/failover remain authoritative.

## Design
- Shared affinity settings: `affinity_by_prefix` (default true), `affinity_stable_key_enabled` (existing default false), `affinity_max_messages` (400; 0 disables threshold), `affinity_page_size` (3; 0 whole-pool rotation, 1 pin), `affinity_model_pool_sizes` (map of `realm/model` to size, default empty; 0 off, 1–64 accounts).
- Explicit session IDs win, unchanged. Derived keys use a canonical JSON hash of leading system/developer messages plus the first user, optionally the first assistant. Do not key a system-only request or hash future turns. Missing user/malformed head fails open to ordinary selection, not a shared bogus key.
- Apply to WorkBuddy, CodeArts, Raccoon after protocol translation. Preserve CodeArts upstream chat_id/cache-key behavior. WorkBuddy effective key reaches both affinity and cache/conversation IDs.
- Long derived conversations (>400 messages) rotate among exactly up to 3 accounts using a bounded cursor per model/cohort, not sticky binding. Membership is deterministic from authorized realm accounts and routing seed, independent of temporary busy/cooldown state. If no preferred account is eligible, use the existing allowed pool; never override allow-lists, model bans, concurrency, guards, tiers, or 11140 restrictions.
- Model cohorts are opt-in soft preferences; used for new conversations (same realm/model chooses the same cohort). Existing valid explicit affinity wins; long page membership stays within the preferred model cohort where configured.
- Use shared settings for stable-key behavior across all three providers; environment aliases `WB_AFFINITY_BY_PREFIX`, `WB_AFFINITY_MAX_MSGS`, `WB_AFFINITY_PAGE_SIZE` supply defaults only, explicit panel settings win.
- Keep `remaining_priority_enabled` stored but expose `remaining_priority_supported=false` and disable its UI. Do not invent credit-based weights or alter the independent expiring-credit policy.
- Add privacy-safe head/count/routing diagnostics to provider snapshots and usage rows. No prompts, tool contents, credentials, or raw client session identifiers in logs. Unknown cached-token reporting must not be treated as proof of no upstream cache.

## Constraints
Active endpoints stay on official v2.8.15 until an official release is published. No data backfill, scheduler change, credential edits, or relaxation of 11140. Preserve user work and existing model aliases/pool boundaries. Hy4 long-context upstream behavior remains an evidence question, not a promised fix.

## Verification
Unit tests for canonical semantic heads, shared live settings, cohort cardinality/stability/rotation/fallback/policy boundaries, and explicit-key precedence. In-process end-to-end Chat/Responses/Messages tests reach real provider selection against local fake upstreams. Full `make check` and `make race`, branch review; isolated local verification only, no active container replacement.
