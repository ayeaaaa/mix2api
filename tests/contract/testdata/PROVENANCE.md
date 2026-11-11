# Contract golden provenance

The JSON/SSE files in this directory are **regression goldens for the v2
foundation**, not outputs captured from a live v1.6.5 oracle run.

- They encode the small surface implemented by the Raccoon vertical slice
  (health, models, chat stream, chat non-stream) and lock down v2's intended
  normalized semantics.
- They are not replaced by the recorded oracle. The selected v1.6.5
  fake-upstream output now lives separately in
  `tests/oracle/recorded/v1.6.5/`; its `PROVENANCE.md` lists the exact v2 delta.
  Gate A remains **partial**: these scoped regression fixtures pass, and the
  recorded selected cases confirm or delimit the v1.6.5 behavior. WorkBuddy and
  deterministic failure matrices are recorded in the sibling fixture
  directories; full replacement of every scoped regression golden remains open.
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
the selected CodeArts v1.6.5 oracle cases are recorded separately in
`tests/oracle/recorded/v1.6.5/`. They cover the scoped CodeArts surface: provider health, the exact
`/v1/models?realm=codearts` union (including `DeepSeek-V4.1-Flash-1017`), chat
non-stream and SSE (text and tool calls), overview with no credential fields,
check-in claim/confirm, credits refresh, models refresh, disabled/error
envelopes, concurrency retry/failover, and credit-exhausted failover/readable
400.

Dynamic values are explicit placeholders: `base_url`, per-second
`token_remaining`, the time-of-day `credits` multiplier on every model whose
peak and off-peak rates differ (`DeepSeek-V4.1-Flash` and
`DeepSeek-V4.1-Flash-1017`), timestamps, and generated IDs. See
`tests/oracle/codearts.md` for the read-only v1.6.5 oracle procedure. The
recorded selected CodeArts cases now match on models (including the v1
`catalog` block), chat (including `elapsed_ms`/`first_chunk_at`), accounts,
overview, check-in, and credits refresh except for two additive v2 fields. Full
oracle replacement of every CodeArts regression fixture remains **pending**; the
goldens lock down v2's scoped semantics and must be reviewed before being
replaced by recorded oracle assertions.

## WorkBuddy fixtures

The `workbuddy_*.golden.json` and `workbuddy_chat_stream*.golden.json` files are
**v2 regression fixtures**, generated from the local fake WorkBuddy upstream
through the real intl/cn providers and API server. They are **not captured
v1.6.5 oracle output**. The v2 regression contract is **complete** for these
scoped fixtures. The selected v1.6.5 WorkBuddy outputs are now recorded in
`tests/oracle/recorded/v1.6.5-workbuddy/` (18 cases), and the failure matrix is
in `tests/oracle/recorded/v1.6.5-failures/workbuddy/` (5 cases). They cover the
scoped WorkBuddy surface: provider health for both realms, per-realm models,
catalog refresh, chat non-stream/SSE (text, tools, reasoning, images), 429 and
401 refresh/retry, 5xx failover, credits refresh, CN check-in, and legacy admin
account listing.

Dynamic values follow the same normalizer as the other providers; WorkBuddy
adds timestamp recognition for `addedAt`, `updated_iso`, `lastCheckin`,
`lastDailyChat`, `lastActivityReport`, and activity `ts`. The fake upstream URL
and temporary accounts path are replaced before comparison. See
`tests/oracle/workbuddy.md` for the read-only v1.6.5 oracle procedure. The
recorded comparison now matches 12 of 18 cases exactly. The remaining
differences are intentional additive fields and one scope difference, not
normalization exceptions: the model aliases `category`, `context_window`,
`free`, `max_output`, `max_tokens`, and `provider`; the health `version` and
neutral `providers` map; the usage ttft/gen/wall timing sums and sample counts;
and v2's durable request log store versus v1's in-memory startup/request text.
Health identity, account-view shape, credits/check-in whole-pool accounts, and
`machineId`/`sessionId` now match; see
`tests/oracle/recorded/v1.6.5-workbuddy/V2-DELTA.txt`.

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
- **v1 chat/model fields now emitted:** `elapsed_ms`, `first_chunk_at`, and the
  models `catalog` block are part of the v2 contract; `elapsed_ms` normalizes
  to `<duration>` because it is a variable wall-clock measurement.