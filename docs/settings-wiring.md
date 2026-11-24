# Settings wiring

The v2.8.19 panel persisted several controls that had no runtime consumer. This
document records the ones wired in the follow-up and the one deliberately left
disabled.

## Wired

| Setting | Behavior |
|---|---|
| `filter_models_by_key` | When true and the request carries a key with a model allow-list, `/v1/models` returns only allowed entries across realm, `all`, and `gateway` responses. Chat/Messages/Responses enforce the key allow-list unconditionally. |
| `reasoning_effort_map` | Default true. Unsupported `reasoning_effort` values are replaced with the model's supported/default effort in WorkBuddy, CodeArts and Raccoon. When false the requested value passes through unchanged. Doubao has no client-supplied effort path. |
| `prompt.mode` / `prompt.text` | `passthrough` leaves messages untouched; `custom` removes system/developer messages and prepends the gateway prompt; `append` inserts it after the leading system/developer block. Applied once per request for Chat, Responses and Messages. Blank text uses the built-in v1-style prompt. The v1 403 degrade/retry path is not implemented. |
| `egress_backoff_enabled` | In-process per-proxy-URL tracker. Transport errors and 502/503/504 back off 1s → 2s → … → 15s; any success clears it. Accounts whose resolved proxy URL is backing off are skipped; if every eligible account is backing off, selection fails open. |
| `free_model_credit_fallback` | WorkBuddy only. When a rate-limited failure happens on a free account for a free model, later attempts prefer a paid account for the same model: the pool is asked with free accounts excluded for that selection, and if no usable paid account exists the request fails open to the normal candidate set. No model ID substitution. |

## Deliberately not supported

`restriction_hold_seconds` remains stored but has no runtime effect. v2 keeps
its per-account 11140/restriction windows instead of v1's realm-wide circuit
breaker. The settings view exposes the stored value read-only (or the v1
default 1800 when absent), reports `restriction_hold_supported=false`, and the
panel disables the input and buttons and labels the value as
configuration-only.

## Verification

`make check`, `make race`, dashboard JS suites and targeted tests cover the
models filter, prompt rewriting per protocol, effort mapping gates, egress
tracker and WorkBuddy free-model fallback.
