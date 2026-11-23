# Prefix cache routing

The upstream owns the cache. Routing preserves opportunities for reuse; it cannot guarantee a hit percentage, cache lifetime, or long-context support.

## Defaults

| Setting | Default | Behavior |
|---|---:|---|
| `affinity_by_prefix` | true | Derive a session when the client supplies no ID |
| `affinity_stable_key_enabled` | false | Also include the first assistant reply |
| `affinity_max_messages` | 400 | Above this number, rotate within a cohort; 0 disables the threshold |
| `affinity_page_size` | 3 | **Account count**, not partition count; 0 whole-pool rotation, 1 pin |
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

Submit through `POST /settings/save` with panel authentication, or the JSON field in the panel. Partial JSON saves preserve other realm/model entries. Set an entry to `0` to disable it; removing it in the panel submits an explicit zero. Use the upstream model ID (not a new-api display alias). No existing production configuration is changed by this PR.

## Selection

1. Explicit `X-Conversation-Id`, `X-Session-Id`, body `conversation_id`/`session_id`, or supported metadata always wins; prefix fallback does not override it or force it into a model cohort.
2. Otherwise hash canonical leading system/developer messages and the first user. Later history does not change the key. System-only or malformed heads have no derived binding. Stable mode also hashes the first assistant, so the first reply can cause one deliberate rebinding.
3. Ordinary conversations use existing affinity. New conversations may prefer a model's configured cohort. Long derived conversations rotate within a deterministic cohort of up to the configured account count; they do not bind to the last selected account.
4. Membership uses authorized realm accounts, not the temporarily ready subset, so busy/cooldown changes do not reshuffle it. Existing model eligibility, account enablement, guards, concurrency and account allow-lists still filter every candidate. When the cohort has no eligible member, the normal allowed pool can serve the request. Consequently cohort size is a **soft preference**, not a hard concurrency limit or authorization boundary.

CodeArts's upstream `chat_id` and `prompt_cache_key` behavior is unchanged. WorkBuddy's effective key also feeds its upstream conversation/cache IDs. Protocol translation happens before provider-side affinity, so Chat, Responses and Messages all participate.

## Diagnostics

New successful/streamed ledger rows carry `affinity_mode` (`explicit`, `prefix`, `page`, `rotation`), `n_msgs`, `prefix_sha` for derived heads, `affinity_pool_size`, and `affinity_fallback`. They contain no prompt/tool text or raw client session ID. Existing account, model, cached-token and TTFT fields let operators compare identical heads on the same account at similar context sizes.

`remaining_priority_enabled` is retained for compatibility; `remaining_priority_supported=false` honestly reports that its runtime wiring is absent and disables the UI control. The independent expiring-credit selection layer remains intact.

## Hy4 evidence boundary

A short repeated prompt reaching 96% cache hits proves short-prefix caching is available. It does not disprove cache capacity, routing, TTL or other upstream changes for 500K–800K contexts. Investigate long-context Hy4 separately with matched account, prefix hash, context length and timing. Do not call an upstream cluster switch confirmed from aggregate rates alone.

Active nodes continue running official releases. To roll out these changes, publish a new official release; keep accounts, scheduler modes, usage history and existing rollback containers.
