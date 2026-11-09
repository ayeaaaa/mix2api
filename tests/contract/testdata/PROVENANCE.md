# Contract golden provenance

The JSON/SSE files in this directory are **regression goldens for the v2
foundation**, not outputs captured from a live v1.6.5 oracle run.

- They encode the small surface implemented by the Raccoon vertical slice
  (health, models, chat stream, chat non-stream) and lock down v2's intended
  normalized semantics.
- They have not been diffed against shipped v1.6.5 fake-upstream output in this
  branch. Gate A in the master plan is therefore **partial**: Raccoon
  vertical-slice contract/regression tests pass, but full v1.6.5 oracle parity
  remains open.
- When recording real oracle fixtures, follow `tests/oracle/README.md`, keep the
  v1.6.5 tree read-only and out of git, and replace these files only after
  reviewing semantic differences.

The `health.golden.json` fixture intentionally covers only the v2 foundation
fields; v1's panel/media/key readiness blocks are out of scope for this slice.

## CodeArts fixtures

The `codearts_*.golden.json` and `codearts_chat_stream.golden.json` files are
also **v2 regression fixtures**, generated from the local fake CodeArts upstream
through the real provider and API server. They are **not captured v1.6.5 oracle
output**. The v2 regression contract is **complete** for these scoped fixtures;
v1.6.5 oracle parity is **pending**. They cover the scoped CodeArts surface: provider health, the exact
`/v1/models?realm=codearts` union (including `DeepSeek-V4.1-Flash-1017`), chat
non-stream and SSE (text and tool calls), overview with no credential fields,
check-in claim/confirm, credits refresh, models refresh, disabled/error
envelopes, concurrency retry/failover, and credit-exhausted failover/readable
400.

Dynamic values are explicit placeholders: `base_url`, per-second
`token_remaining`, the time-of-day `credits` multiplier on every model whose
peak and off-peak rates differ (`DeepSeek-V4.1-Flash` and
`DeepSeek-V4.1-Flash-1017`), timestamps, and generated IDs. See
`tests/oracle/codearts.md` for the read-only v1.6.5 oracle procedure. Actual
v1.6.5 parity remains **pending** for CodeArts as well; the goldens lock down
v2's scoped semantics and must be reviewed before being replaced by recorded
oracle assertions.

## WorkBuddy fixtures

The `workbuddy_*.golden.json` and `workbuddy_chat_stream*.golden.json` files are
**v2 regression fixtures**, generated from the local fake WorkBuddy upstream
through the real intl/cn providers and API server. They are **not captured
v1.6.5 oracle output**. The v2 regression contract is **complete** for these
scoped fixtures; v1.6.5 oracle parity is **pending**. They cover the scoped
WorkBuddy surface: provider health for both realms, exact per-realm models and
the WorkBuddy-only `realm=all` union, catalog refresh, chat non-stream/SSE
(text, tools, reasoning, images), 429 and 401 refresh/retry, cross-exit
failover, credits refresh, the daily-token guard, CN check-in, intl daily
activity, CN growth tasks, and legacy admin account listing.

Dynamic values follow the same normalizer as the other providers; WorkBuddy
adds timestamp recognition for `addedAt`, `updated_iso`, `lastCheckin`,
`lastDailyChat`, `lastActivityReport`, and activity `ts`. The fake upstream URL
and temporary accounts path are replaced before comparison. See
`tests/oracle/workbuddy.md` for the read-only v1.6.5 oracle procedure. Actual
v1.6.5 parity remains **pending** for WorkBuddy as well.

### Final-review parity fixes and exclusions

These are v1.6.5-aligned behavior changes, not new golden recordings:

- Chat reloads live settings on entry; disabling the realm rejects `/v1/chat`
  immediately with v1's 400 message. `/codearts/models/refresh` starts a fresh
  discovery instead of joining an invalidated in-flight flight.
- `stream_timeout_seconds` is enforced as a per-read SSE idle bound. A stalled
  upstream returns a classified 502 timeout; a clean EOF before `[DONE]` is v1
  success and synthesizes finish/usage/`[DONE]`. Only non-EOF read failures are
  502.
- OAuth identity enrichment uses signed STS `caller-identity` then
  `snap-manager current/user` (10s, direct) before the JWT/hash fallback. A
  callback with an unknown supplied secret is rejected instead of selecting a
  session by ticket/state.
- Chat no-account/unreachable failures keep v1's 502; only the non-chat
  disabled `/v1/models` gate stays 503. Three concurrency attempts use v1's
  2s/4s backoff, and ordinary 5xx failures use the normal three-strike error
  path rather than a soft 60s cooldown.
- Panel/check-in redaction now includes `access_key_id`; signed upstream
  transports refuse redirects; `CodeArtsStream.closed` is atomic.

Intentional v2 deviations/exclusions (do not encode as v1 output):

- **Auth-path sanitization (security-improving):** v2 replaces every character
  outside `[A-Za-z0-9._-]` with `_` (and keeps the canonical ownership check);
  v1 only replaced path separators. v2 is deliberately stricter against path
  traversal/collisions.
- **Redirect refusal (security-improving):** v2's shared signed transport uses
  `CheckRedirect: ErrUseLastResponse`; v1's urllib path could follow a redirect.
  A 3xx from a token/catalog/chat endpoint is treated as an endpoint error.
- **Proxy binding cleanup exclusion:** v1 clears bindings to removed slots only
  in its `/proxy/slots/save` panel handler via `drop_missing_bindings`. That
  slot-management route is outside the v2 CodeArts surface; v2 therefore keeps
  a missing/disabled slot binding on disk and resolves it to direct, exactly
  like v1's `apply_proxy_slots`.
- **v1-only chat/model fields:** `elapsed_ms`, `first_chunk_at`, and the models
  `catalog` block remain out of the v2 foundation contract.