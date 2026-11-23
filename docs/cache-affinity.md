# Prefix cache routing

The upstream owns the cache. Routing preserves opportunities for reuse; it cannot guarantee a hit percentage, cache lifetime, or long-context support.

## Defaults

| Setting | Default | Behavior |
|---|---:|---|
| `affinity_by_prefix` | true | Derive a session when the client supplies no ID |
| `affinity_stable_key_enabled` | false | Also include the first assistant reply |
| `affinity_max_messages` | 400 | Above this normalized upstream-message count, apply long-conversation routing; 0 disables the threshold |
| `affinity_long_strategy` | `cohort` | Global long-conversation strategy: `cohort` = fixed account count (v2.8.16 behavior), `partition` = V1 rank-modulo partitions |
| `affinity_page_size` | 3 | Global count; under `cohort` it is the maximum account count, under `partition` it is the partition count. 0 = whole-pool rotation, 1 = stay sticky |
| `affinity_provider_strategies` | `{}` | Optional per-provider `cohort`/`partition`; missing or empty inherits the global strategy |
| `affinity_provider_page_sizes` | `{}` | Optional per-provider count 0–64; missing or `-1` inherits the global `affinity_page_size` |
| `affinity_model_pool_sizes` | `{}` | Optional preferred account counts per `realm/model`; 0 off, 1–64 |

The existing Models settings section exposes these controls. `WB_AFFINITY_BY_PREFIX`, `WB_AFFINITY_MAX_MSGS`, and `WB_AFFINITY_PAGE_SIZE` supply defaults; an explicit panel setting wins. The stable-key switch is consumed by WorkBuddy, CodeArts and Raccoon, not just saved/displayed.

### Low-traffic DeepSeek example

```json
{
  "affinity_model_pool_sizes": {
    "intl/deepseek-v4.1-flash-sg": 3
  }
}
```

Submit through `POST /settings/save` with panel authentication, or the JSON field in the panel. Partial JSON saves preserve other realm/model entries. Set an entry to `0` to disable it; removing it in the panel submits an explicit zero. Disabled rules are pruned and the combined map is atomically limited to 256 active entries. Use the upstream model ID (not a new-api display alias). No existing production configuration is changed by this PR.

## Selection

1. Explicit `X-Conversation-Id`, `X-Session-Id`, body `conversation_id`/`session_id`, or supported metadata always wins; prefix fallback does not override it or force it into a model cohort.
2. Otherwise hash canonical leading system/developer messages and the first user. Equivalent developer/system roles and text-block/string forms share a key; WorkBuddy hashes its sanitized messages so volatile billing metadata cannot move a conversation. Later history does not change the key. System-only or malformed heads have no derived binding. Stable mode also hashes the first assistant, so the first reply can cause one deliberate rebinding.
3. Ordinary conversations use existing affinity. The **default `cohort` strategy** preserves v2.8.16: a long derived conversation selects at most `affinity_page_size` accounts from the effective authorized set (after optional model-preferred narrowing) by rendezvous ranking seeded by the conversation/model scope, then rotates within that cohort. The page cursor is per conversation/model; a fresh conversation gets a new cohort. This is an account count, not a partition count. WorkBuddy's page cursor wins before manual/expiry priorities, matching v2.8.16; CodeArts/Raccoon select the cursor account before their ordinary least-busy ordering. An unavailable cohort falls back through normal legal selection without binding the page conversation.

4. The optional **`partition` strategy** follows V1. Choose a page index from the first 8 digest hex digits modulo `affinity_page_size`, rank the effective allowed UIDs (authorized realm UIDs after optional model-preferred narrowing) in sorted order, and use accounts whose rank modulo that count equals the page index. Thus 40 accounts with 3 partitions form groups of 14/13/13, not groups of 3 accounts. Real partition pages are not sticky; page cursors are shared by realm/index/count even across different authorized subsets, with bounded LRU state. Existing provider priorities run before ordinary page round robin; WorkBuddy manual/expiry shares within a page span models, as in V1.

5. Partition strategy has V1's small-pool guard: if the partition count covers the full realm pool (for example 2 or 3 accounts with the default 3), normal sticky selection is restored. A key restricted to 2 accounts in a larger realm is different: V1's partition slice covers its whole allowed subset but remains non-sticky. Under `partition`, `page_size=0` drops the long derived key and uses normal pool routing within the effective set; `page_size=1` remains sticky. Under `cohort`, `page_size=0` keeps the derived key but uses whole-pool rotation within the effective set, and `page_size=1` remains sticky.

6. Ranks use the static effective snapshot (authorized UIDs after optional model-pool narrowing) before readiness/exclusion, so busy/cooldown changes do not reshuffle partition membership. Account enablement, model/tier eligibility, guards, concurrency and allow-lists still filter every candidate. An unavailable partition falls back through normal legal pool selection, without binding that page conversation to the fallback account.

7. Optional `affinity_model_pool_sizes` retains its separate meaning: **preferred account count** for that model (default empty/off). It is an opt-in additional filter, not the long-conversation count or V1 partition parameter. Model-cohort weighted shares remain independent across models.

### Strategy hierarchy and examples

`affinity_long_strategy` is the global default. `affinity_provider_strategies` can override it per provider family; `affinity_provider_page_sizes` can separately override the count. Supported provider keys are `workbuddy` (both intl and cn), `codearts`, and `raccoon`. An absent or empty strategy inherits the global strategy; an absent page size or `-1` inherits the global count. Page-size `0` is a real value, not “inherit”. The count's meaning follows the provider's resolved strategy: account count for `cohort`, partition count for `partition`.

```json
{
  "affinity_long_strategy": "cohort",
  "affinity_provider_strategies": {
    "workbuddy": "partition"
  },
  "affinity_provider_page_sizes": {
    "workbuddy": 3
  }
}
```

That example keeps CodeArts and Raccoon on the v2.8.16 account-count strategy while giving WorkBuddy V1 3-way partitions. To use V1 everywhere, set the global strategy to `partition`; to keep the current production behavior, leave it unset or `cohort`.

CodeArts's upstream `chat_id` and `prompt_cache_key` behavior is unchanged. WorkBuddy's effective key also feeds its upstream conversation/cache IDs. Protocol translation happens before provider-side affinity, so Chat, Responses and Messages all participate.

PR #53 originally changed long-conversation routing to V1 partitions by default. The user asked to keep the v2.8.16 account-count behavior as the default and expose V1 as an alternative. This revision therefore keeps `cohort` as the default, adds global and per-provider selection, and leaves V1 as an explicit opt-in. No production configuration was modified as part of this revision.

## Diagnostics

New successful/streamed ledger rows carry `affinity_mode` (`explicit`, `prefix`, `page`, `rotation`), `n_msgs`, `prefix_sha` for derived heads, `affinity_pool_size`, and `affinity_fallback`. They contain no prompt/tool text or raw client session ID. Existing account, model, cached-token and TTFT fields let operators compare identical heads on the same account at similar context sizes.

`remaining_priority_enabled` is retained for compatibility; `remaining_priority_supported=false` honestly reports that its runtime wiring is absent and disables the UI control. The independent expiring-credit selection layer remains intact.

## Hy4 evidence boundary

A short repeated prompt reaching 96% cache hits proves short-prefix caching is available. It does not disprove cache capacity, routing, TTL or other upstream changes for 500K–800K contexts. Investigate long-context Hy4 separately with matched account, prefix hash, context length and timing. Do not call an upstream cluster switch confirmed from aggregate rates alone.

Active nodes continue running official releases. To roll out these changes, publish a new official release; keep accounts, scheduler modes, usage history and existing rollback containers.
