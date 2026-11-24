# Wire the six persisted-only settings

## Intent

The settings panel exposes six controls that are saved and displayed but have
no runtime consumer in v2. The user asked to actually implement them, with two
decisions already made:

- **风控连坐暂停 (`restriction_hold_seconds`)**: do NOT implement. Keep the
  stricter v2 11140 policy (per-account restriction windows only). The panel
  will be changed to honestly report "not supported, config retained" and
  disabled, like `remaining_priority_enabled`.
- **免费额度耗尽回退积分版 (`free_model_credit_fallback`)**: implement as
  **same model, switch to a paid account** when the current free-model account
  cannot serve the request because its credits/reserve/daily budget are
  exhausted. No model ID substitution.

All six land in one PR based on official v2.8.19. Official release only; no
custom images, no production deployment in this branch.

## Goals

- Each setting's panel switch changes observable gateway behavior.
- When a setting is off, behavior stays exactly as v2.8.19.
- Settings remain merge-safe and configuration keys are preserved.
- No relaxation of authorization, key model allow-lists (except the explicit
  `filter_models_by_key` listing behavior), 11140 per-account restrictions,
  concurrency caps or proxy credentials.

## Non-goals / anti-goals

- v1's system-prompt degrade gate (403 → midnight neutral prompt retry) is NOT
  implemented; only `passthrough`/`append`/`custom` rewriting.
- No realm-wide cross-account restriction hold.
- No free→paid **model** substitution.
- No change to provider transport or upstream request contracts beyond the
  fields named below.

## Design

### 1. `filter_models_by_key` — models listing

Read via the existing settings service in `Server.handleModels`
(`internal/api/models.go`). When true **and** the request carries a key with an
explicit model allow-list, filter every `/v1/models` response (single realm,
`realm=all`, gateway, and CodeArts branch) with the existing
`keys.AllowsModelAny` check. When false, keep full catalog. Chat/Messages/
Responses already enforce the allow-list unconditionally and are untouched.

Tests: settings service store with a restricted key; `/v1/models` on/off.

### 2. `reasoning_effort_map` — effort normalization

Default true. Shared helper `settingsEnabledDefaultTrue(store, key)` in a small
`internal/providers/settingflags` package (or duplicated per provider if
simpler) so nil means true.

- WorkBuddy: gate the existing `mapChatReasoningEffort` call in
  `normalizeChatBody` (`workbuddy/chat.go:1507`).
- CodeArts: thread a bool from `OpenChat` into `buildChatBody`; when off, pass
  the requested effort through unchanged and skip the 400 validation.
- Raccoon: add a new mapping in `buildChatBody` using `raccoonReasoningEfforts`
  and `allowed[0]` fallback; when off, pass through.
- Doubao: no client effort path, no change.

Key spelling: preserve the original key where the provider already does
(WorkBuddy); CodeArts keeps its current normalized `reasoning_effort`.

### 3. `prompt` — global system prompt

New `internal/api/prompt_mode.go`:

- `promptConfig(ctx, s) (mode, text string)` reads `s.settings.Store.Load()`
  fail-open, normalizes via the same rules as `promptView`.
- `applyPromptMode(messages []any, mode, text string) []any`:
  - `passthrough`: unchanged
  - `custom`: drop all `system`/`developer` messages, prepend one
    `{role:"system", content:text}`
  - `append`: insert one system message after the leading contiguous
    `system`/`developer` block
  - built-in default text when `text` is blank for `custom`/`append`
- Bridges for `[]json.RawMessage` (Chat) and `map[string]any` messages.

Hook once per protocol, before `providers.ChatRequest` is built:
Chat `HandleChat` (~chat.go:136), Responses `serveResponses` (~:1534),
Messages `serveMessagesRequest` (~:697). Do NOT hook
`messagesToChatWithDefault` or webtools round builders (would double-apply or
affect `count_tokens`).

### 4. `egress_backoff_enabled` — per-egress transient backoff

New shared tracker in `internal/core/transport` keyed by normalized proxy URL
(direct = ``). State is in-process, bounded, with sweep on access:

- `Allow(proxyURL) (bool, time.Duration)`: false while backing off.
- `RecordSuccess(proxyURL)`: clear.
- `RecordTransient(proxyURL)`: 1s → 2s → 4s → … cap 15s with jitter.
- Only transport errors and 502/503/504 count; 401/403/429/500 do not.

Integration point: provider pool account selection. For each candidate, resolve
its bound proxy slot URL via the existing `Slots`/proxy binding, and skip
accounts whose egress is currently backing off. On upstream result, feed success
or transient failure back to the tracker. Concretely:

- WorkBuddy `accountUsable` / `openChat` result handling.
- CodeArts and Raccoon equivalent paths.

When the setting is false, tracker calls are no-ops and no account is skipped.
When all usable accounts' egresses are backing off, fall back to normal
selection (fail-open) rather than returning "no account".

Risk: this is the most invasive change. It stays behind the default-off flag.

### 5. `free_model_credit_fallback` — same model, paid account

When a request targets a model that the tier map marks as requiring a paid /
non-free account (`RequiresPro`), and the affinite/pool-selected account cannot
serve it because of credits/reserve/daily limits, retry selection among eligible
accounts that **can** serve the same model and are paid. Reuse the existing
`ModelPoolAvailable(realm, model) (usable, paid)` contract and per-account tier
checks. No model ID changes, no authorization changes.

Implementation location: provider chat retry loop (WorkBuddy primary; CodeArts/
Raccoon where the same tier hook exists). Default off preserves current
behavior. Tests use fake accounts with free/paid tiers and exhausted credits.

### 6. `restriction_hold_seconds` — honest disable

- Add `restriction_hold_supported: false` to the settings view (next to
  `remaining_priority_supported`).
- Panel: disable the seconds input, both buttons, show "(当前未支持，配置保留)",
  and keep storing the raw value. Remove the misleading live "保护中"/alert
  rendering.
- The no-op `/settings/restriction/reset` endpoint can stay (already honest in
  its message) but the panel no longer offers it.

## UI/docs

- Remove "已启用" claims from disabled controls.
- Update help text for the five wired settings to describe the new behavior.
- `docs/` note the egress backoff scope, prompt modes and same-model fallback.
- CHANGELOG Unreleased section.

## Verification

TDD per feature: RED test first, then minimal wiring.

- models: /v1/models key filtering on/off, all branches.
- effort: WorkBuddy existing mapping test gated off; CodeArts invalid effort
  pass-through; Raccoon new mapping.
- prompt: pure rewrite tests + one integration test per protocol with a fake
  provider capturing the outgoing `ChatRequest`.
- egress tracker: unit test timeouts/cap/sweep/success reset; pool integration
  test skipping a backing-off egress and fail-open when all are backing off.
- free fallback: fake free/paid accounts, exhausted credits, same model.
- disabled control: view flag + panel test.
- Full `make check`, `make race`, dashboard JS suites, focused review.
