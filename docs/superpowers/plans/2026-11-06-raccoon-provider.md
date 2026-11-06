# Raccoon Native Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Raccoon (小浣熊) as a fourth native realm in mix2api: multi-account auth, model catalog, OpenAI/Responses/Anthropic chat, usage attribution, dashboard, and release v1.6.0.

**Architecture:** A new `app/wb_raccoon.py` owns the provider protocol, account pool, catalog, token refresh, and loopback OAuth; `wb_proxy.py` only routes the `raccoon` realm and adapts the upstream SSE into the existing chat/Responses/Messages handlers. `wb_upstream_pool` supplies transport and proxy-slot support. No Node runtime and no new Python dependencies.

**Tech Stack:** Python 3.9+ standard library, existing `http.server` gateway, existing vanilla-JS dashboard, GitHub Actions, GHCR.

**Spec:** `docs/superpowers/specs/2026-11-06-raccoon-provider.md`

## Global Constraints

- Python 3.9+ standard library only; do not add `cryptography`, `qrcode`, `requests`, or a Node sidecar.
- Single process. Raccoon auth, catalog, chat, and refresh run inside the gateway.
- Default port remains `58675`; do not change WorkBuddy/CodeArts behavior.
- External Raccoon model IDs are the original bare IDs. No `YYLS/` or `provider/` prefix in v1.6.0.
- `/v1/models?realm=all` must not include Raccoon.
- Access tokens, refresh tokens, and authorization codes must never appear in admin responses, logs, activity history, usage rows, or error messages.
- All outbound Raccoon HTTP goes through `wb_upstream_pool.urlopen` so proxy slots, `no_proxy`, and stale-connection retry behave like CodeArts.
- Account files are written atomically. On POSIX, create them with mode 0600 where practical.
- Release is `v1.6.0`; both source version strings must equal `1.6.0`.
- Update README suite counts after adding tests; `_test_release_engineering.py` pins the exact string.

## Review Focus

These are the inputs/failure modes most likely to break users; each line has a test in the named task.

1. **Concurrent 401 refresh** — two requests hit an expired token; only one refresh runs, the refresh token is not consumed twice, and both requests succeed. Task 1/3.
2. **OAuth state mismatch, replay, and expiry** — a callback with a wrong state, a reused state, or a state older than 300s must not create or replace an account. Task 5.
3. **Account failover and cooldown** — a 401/429/5xx account is skipped, the next healthy account serves the request, and the failed account is not hammered. Task 1/3/4.
4. **Stream truncation and usage** — an upstream stream that dies mid-body must produce a terminal error frame and a usage/error row, not a silent clean EOF. Task 3/4.
5. **Key/exit boundaries** — a key bound to `raccoon` gets bare Raccoon models; key model/account restrictions reject before upstream; `realm=all` never leaks Raccoon models. Task 4.

---

### Task 1: Fake upstream, provider config, auth import, refresh, account pool

**Files:**
- Create: `app/wb_raccoon.py`
- Create: `app/tests/_fake_raccoon.py`
- Create: `app/tests/_test_raccoon_native.py`
- Modify: `app/wb_settings.py` (add `"raccoon"` to `REALMS` only; provider config is read/written by `wb_raccoon`)
- Test: `app/tests/_test_raccoon_native.py`

**Interfaces:**
- Consumes: `wb_settings.load/save`, `wb_settings.proxy_slots`, `wb_upstream_pool.urlopen`, `wb_settings._atomic_write` pattern (copy locally, do not import a private helper).
- Produces:
  - `load_config(accounts_dir: str) -> dict`
  - `save_config(accounts_dir: str, patch: dict) -> dict`
  - `configured(accounts_dir: str) -> bool`
  - `configured_config(config: dict) -> bool`
  - `masked_config(accounts_dir: str) -> dict`
  - `auth_from_payload(payload: dict) -> dict`
  - `jwt_claims(token: str) -> dict`
  - `uid_from_auth(auth: dict) -> str`
  - `expires_at(auth: dict) -> float`
  - `load_auths(accounts_dir: str) -> list[dict]`
  - `save_auth(accounts_dir: str, auth: dict) -> dict`
  - `delete_auth(accounts_dir: str, uid: str) -> bool`
  - `refresh_auth(auth: dict, timeout: float = 30.0, proxy: str = "") -> dict`
  - `ensure_token(acct: RaccoonAccount, force: bool = False) -> None`
  - `class RaccoonAccount` with `uid`, `nickname`, `proxy_slot`, `proxy`, `disabled`, `cool_until`, `err_count`, `last_error`, `note`, `tasks_enabled`, `active_concurrent`, `max_concurrent`, `view()`, `expires_at()`, `token_remaining()`, `expiring_soon(skew=120.0)`, `healthy()`, `update_auth(data)`
  - `class RaccoonPool` with `add(auth)`, `get(uid)`, `list()`, `pick(model, session_key=None, exclude=None, allowed_accounts=None)`, `acquire(acct)`, `release(acct)`, `note_success(acct)`, `note_error(acct, message, soft=False, terminal=False)`, `apply_proxy_slots(slots=None)`, `save_state()`, `stats()`
  - `pool_for(accounts_dir)`, `invalidate_pool(accounts_dir=None)`
  - `import_auth(config: dict, payload: dict) -> dict`
  - `overview(config: dict, force: bool = False) -> dict`
  - `health(config: dict, force: bool = False) -> dict`
  - error classes `RaccoonError`, `RaccoonUnavailable`, `RaccoonAuthError`, `RaccoonBadRequest`, `RaccoonNotFound`, `RaccoonRateLimited`, `RaccoonUpstreamError`

- [ ] **Step 1: Build the fake upstream skeleton**

`app/tests/_fake_raccoon.py` exposes `start_server() -> (server, base, state)`, `stop_server(server)`, and `make_jwt(uid, expires_in=3600)`. Routes:

- `POST /api/web/auth/v1/login_with_authorization_code`
- `POST /api/web/auth/v1/refresh`
- `GET /api/web/llm/v2/model_catalog`
- `POST /api/web/llm/v2/chat/completions` (Task 3 fills the SSE body; Task 1 only needs a 501 placeholder)
- `GET /__requests` for test inspection

State fields: `uid`, `token`, `refresh_token`, `refresh_calls`, `catalog_calls`, `requests`, `last_headers`, `refresh_status`, `refresh_body`. Validate `Authorization: Bearer <token>` on catalog/chat; 401 otherwise. Tokens are unsigned JWT-shaped strings with `sub` and `exp`.

- [ ] **Step 2: Write the failing foundation tests**

`_test_raccoon_native.py` uses `_isolated_dirs`, sets `wb_raccoon.BASE_URL = base`, and checks:

```python
def test_config_defaults_and_clamping():
    # load_config returns enabled False, timeout 60, stream 900,
    # models_ttl 600, max_concurrent 1; invalid values clamp.
def test_import_auth_derives_uid_and_auto_enables():
    # save auth -> uid from JWT sub, config enabled, view has no tokens.
def test_import_rejects_missing_access_token():
    # RaccoonBadRequest for {} and for {"auth": {}}
def test_refresh_rotates_tokens_and_persists():
    # refresh_auth returns new access/refresh; file on disk updated.
def test_concurrent_refresh_single_flight():
    # 8 threads call ensure_token on an expired account; refresh_calls == 1.
def test_pool_state_and_actions():
    # add two accounts, pick respects allowed_accounts/exclude, disable/enable,
    # cooldown, note/tasks persistence, remove.
def test_proxy_slot_binding():
    # bind slot-1, apply_proxy_slots, unknown/disabled slot -> direct.
def test_view_never_contains_secret():
    # "access_token" and "refresh_token" not in json.dumps(view/overview).
def test_terminal_error_marks_disabled_and_soft_marks_cooldown():
    # note_error(... terminal=True) disables; soft=True sets cool_until.
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd /root/opencode/mix2api/app && python3 tests/_test_raccoon_native.py`
Expected: import error / missing functions.

- [ ] **Step 4: Implement config, auth, refresh, and pool**

In `wb_raccoon.py`:

- Config defaults/clamping; `save_config` merges under `"raccoon"` while preserving other settings.
- `auth_from_payload` accepts flat or `{"data": ...}`; strips `Bearer `; requires `access_token`.
- `jwt_claims` base64url-decodes the payload; `uid_from_auth` uses `sub` or hash fallback.
- Atomic writes with `tempfile`/`os.replace`; use `os.chmod(path, 0o600)` on POSIX.
- `refresh_auth` POSTs to `BASE_URL + "/api/web/auth/v1/refresh"` with Bearer + refresh token; raises `RaccoonAuthError` on 401/invalid.
- `ensure_token` is single-flight per account with a lock and double-check; save on success.
- Pool state file, `pick` (healthy, allowed, not excluded, least active then oldest last_used, session affinity), acquire/release, cooldown/terminal state.
- `apply_proxy_slots` resolves `proxySlot` against `wb_settings.proxy_slots`.
- `overview` returns accounts/models/stats/base_url, no secrets.

- [ ] **Step 5: Run the suite and commit**

Run: `python3 tests/_test_raccoon_native.py`
Expected: PASS=all.

```bash
git add app/wb_raccoon.py app/wb_settings.py app/tests/_fake_raccoon.py app/tests/_test_raccoon_native.py
git commit -m "feat(raccoon): add provider config, accounts, refresh and pool"
```

---

### Task 2: Catalog normalization and model entries

**Files:**
- Modify: `app/wb_raccoon.py`
- Modify: `app/tests/_fake_raccoon.py`
- Modify: `app/tests/_test_raccoon_native.py`
- Test: `app/tests/_test_raccoon_native.py`

**Interfaces:**
- Consumes: Task 1 account/pool/auth.
- Produces:
  - `fetch_catalog(acct: RaccoonAccount, timeout=20.0, force=False) -> dict`
  - `ensure_catalog(acct, force=False, timeout=20.0) -> list[dict]`
  - `model_entries(config: dict) -> list[dict]`
  - `models_meta(config: dict) -> dict`
  - `invalidate_models()`
  - `refresh_models(config: dict) -> dict`

- [ ] **Step 1: Add catalog fixtures to the fake upstream**

State gets:

```python
catalog = {
  "data": {"categories": [{"type": "chat", "default_model": "raccoon-090dd3", "models": [
    {"name": "raccoon-090dd3", "display_name": "Raccoon-Work",
     "params": {"context_window": 1000000, "max_tokens": 100000},
     "tags": ["general","code","vision","reasoning"]},
    {"name": "sn-glm-5-3", "display_name": "GLM-5-3",
     "params": {"context_window": 1000000, "max_tokens": 100000},
     "tags": ["code","reasoning"]},
  ]}]}
}
```

Add `catalog_error` (status) and `catalog_calls`.

- [ ] **Step 2: Write failing catalog tests**

Add to `_test_raccoon_native.py`:

```python
def test_catalog_normalizes_ids_limits_and_tags():
    # fetch_catalog -> two models; context/max; vision true for raccoon-090dd3,
    # false for sn-glm-5-3; reasoning true.
def test_catalog_401_refreshes_once_then_retries():
    # expired token + valid refresh -> catalog succeeds; refresh_calls == 1.
def test_catalog_seed_fallback_when_upstream_fails():
    # 500 -> 9 seed entries, source "seed", no exception.
def test_model_entries_are_bare_and_capability_complete():
    # ids contain no "/" and no "YYLS/"; provider == "raccoon";
    # supports_images true only for vision tags; tool_calls true.
def test_model_entries_respect_ttl_and_refresh():
    # two calls inside TTL -> one catalog_calls; refresh_models forces a second.
```

- [ ] **Step 3: Run to verify failure**

Run: `python3 tests/_test_raccoon_native.py`
Expected: catalog tests fail with missing functions.

- [ ] **Step 4: Implement catalog and entries**

- Normalize only the first category whose `type == "chat"`.
- Cache `{models, default_model, fetched_at, source}` module-level with TTL.
- `model_entries` uses the union across accounts' catalogs, else seed; emits the same capability spellings as CodeArts (`capabilities`, `supports_vision`, `supports_images`, `supports_tool_calls`, `vision`, `multimodal`, `abilities`, `input_modalities`, `output_modalities`, `modalities`, `architecture`).
- `context_window/context_length/max_input_tokens`, `max_tokens/max_output_tokens/max_completion_tokens/max_output`.
- `models_meta`: source `raccoon` or `seed`, fetched_at, accounts, warning.
- `refresh_models` invalidates and refetches from the first healthy account.

- [ ] **Step 5: Run and commit**

Run: `python3 tests/_test_raccoon_native.py`
Expected: PASS=all.

```bash
git add app/wb_raccoon.py app/tests/_fake_raccoon.py app/tests/_test_raccoon_native.py
git commit -m "feat(raccoon): normalize model catalog and expose bare model ids"
```

---

### Task 3: Chat transport, SSE stream, failover, usage

**Files:**
- Modify: `app/wb_raccoon.py`
- Modify: `app/tests/_fake_raccoon.py`
- Create: `app/tests/_test_raccoon_chat.py`
- Test: `app/tests/_test_raccoon_chat.py`

**Interfaces:**
- Consumes: Task 1/2 pool, token, catalog.
- Produces:
  - `build_chat_body(payload: dict, session_key=None) -> dict`
  - `class RaccoonStream` (`__enter__`, `__exit__`, `preflight()`, `__iter__`, `close()`, `_wb_billed_model`)
  - `open_chat(config, body, session_key=None, allowed_accounts=None) -> (RaccoonStream, RaccoonAccount, str)`
  - `_client_chat(acct, body, session_key) -> (RaccoonStream, str)`
  - `_chat_headers(acct) -> dict`
  - `test_account(config, uid) -> dict`
  - `keepalive(config) -> dict`

- [ ] **Step 1: Implement the fake chat SSE**

The fake `POST /api/web/llm/v2/chat/completions` validates the body has `model` and `messages`; records `last_body`/`last_headers`; then emits:

- role chunk;
- content chunks (`state.chat_text`, default "你好");
- optional `reasoning_content` when `state.reasoning_text`;
- optional `tool_calls` when `state.tool_calls`;
- final finish chunk;
- a choices-less usage chunk;
- `data: [DONE]`.

State modes: `chat_status` (401/429/500), `truncate_after` (close mid-stream), `embedded_error`, `chat_calls`. For image bodies, set `state.saw_image` if any content part has `image_url`.

- [ ] **Step 2: Write failing transport tests**

`_test_raccoon_chat.py`:

```python
def test_build_chat_body_allows_openai_fields_and_forces_stream():
    # tools, image_url, reasoning_effort, stop, response_format preserved;
    # unknown/dangerous fields dropped; stream forced True.
def test_open_chat_nonstream_aggregates_text_usage():
    # aggregate_stream(stream) -> "你好"; usage total 5; model bare.
def test_stream_passes_raw_chunks_and_close_releases_slot():
    # list(stream) contains [DONE]; acct.active_concurrent back to 0.
def test_tools_are_forwarded_and_returned():
    # tool_calls in request body; fake returns tool_calls; body records tools.
def test_image_content_is_forwarded_unchanged():
    # data URL survives; saw_image True.
def test_401_refreshes_and_retries_same_account():
    # state.token rotated; chat succeeds; refresh_calls == 1.
def test_429_fails_over_to_second_account():
    # account A 429, account B serves; A cool_until > now.
def test_bad_request_does_not_fail_over():
    # 400 raises RaccoonBadRequest immediately; second account not called.
def test_truncated_stream_raises_or_ends_with_error():
    # truncate_after -> iter raises IncompleteRead/UpstreamError; close releases.
def test_empty_accounts_raises_unavailable():
    # no accounts -> RaccoonUnavailable with readable message.
```

- [ ] **Step 3: Run to verify failure**

Run: `python3 tests/_test_raccoon_chat.py`
Expected: failures for missing functions.

- [ ] **Step 4: Implement transport**

- Build an allowlist body; force `stream=True`; clamp output tokens.
- Headers exactly as in the spec; use `wb_upstream_pool.urlopen`.
- `RaccoonStream` stores `raw`, `model`, `acct`, `pool`; `preflight` caches the first `data:` line; `__iter__` yields raw lines; `close` closes and releases exactly once.
- 401/403: `ensure_token(force=True)` once, retry; second failure raises `RaccoonAuthError`.
- 429 -> `RaccoonRateLimited`; 5xx -> `RaccoonUpstreamError`; 400 -> `RaccoonBadRequest`.
- `open_chat` loops accounts, `acquire`, `_ensure_token`, `_client_chat`, notes success/error, binds session; `max_concurrent` respected.
- `test_account` forces catalog refresh and returns token remaining/model count.
- `keepalive` refreshes expiring tokens; returns counts, no check-in.

- [ ] **Step 5: Run and commit**

Run: `python3 tests/_test_raccoon_chat.py`
Expected: PASS=all.

```bash
git add app/wb_raccoon.py app/tests/_fake_raccoon.py app/tests/_test_raccoon_chat.py
git commit -m "feat(raccoon): add chat SSE transport, retry and account failover"
```

---

### Task 4: Gateway realm, models, chat/Responses/Messages, usage

**Files:**
- Modify: `app/wb_proxy.py`
- Modify: `app/wb_settings.py`
- Modify: `app/wb_pricing.py`
- Modify: `app/wb_activity.py`
- Create: `app/tests/_test_raccoon_realm.py`
- Create: `app/tests/_test_raccoon_models.py`
- Create: `app/tests/_test_raccoon_routing.py`
- Test: the three new suites

**Interfaces:**
- Consumes: `wb_raccoon` from Tasks 1-3.
- Produces:
  - `realm_options()` includes `raccoon` when enabled
  - `reconcile_raccoon_realm() -> bool`
  - `_get_raccoon_models()`
  - `_open_raccoon_chat(payload, session_key)`
  - `_handle_raccoon_chat(payload, session_key, realm)`
  - `_handle_raccoon_responses(...)`
  - `_handle_raccoon_messages(...)`
  - `_raccoon_error(...)`, `_raccoon_anthropic_error(...)`

- [ ] **Step 1: Write failing realm/model/routing tests**

`_test_raccoon_realm.py`:

```python
def test_realms_include_raccoon_and_key_binding_keeps_it()
def test_realm_options_shrink_and_grow_with_enabled()
def test_persisted_realm_round_trip_and_disable_reconcile()
def test_raccoon_bypasses_exclusive_realm_check()
```

`_test_raccoon_models.py`:

```python
def test_models_returns_bare_ids_and_capabilities()
def test_key_model_filter_applies_to_raccoon_models()
def test_disabled_realm_returns_503()
def test_realm_all_does_not_include_raccoon()
```

`_test_raccoon_routing.py`:

```python
def test_chat_nonstream_and_stream_via_key_bound_raccoon()
def test_responses_and_anthropic_messages_via_raccoon()
def test_usage_rows_have_realm_account_and_unpriced_policy()
def test_tools_and_image_requests_pass_through()
def test_401_refresh_and_429_failover_are_visible_to_gateway()
def test_key_allowed_accounts_restriction()
```

- [ ] **Step 2: Run to verify failure**

Run: `python3 tests/_test_raccoon_realm.py && python3 tests/_test_raccoon_models.py && python3 tests/_test_raccoon_routing.py`
Expected: realm/model/routing failures.

- [ ] **Step 3: Implement gateway integration**

- Add `"raccoon"` to `REALMS`; `_clean_key_entry` accepts it via `REALMS`.
- `_realm_selectable("raccoon") -> wb_raccoon.configured(ACCOUNTS_DIR)`.
- `realm_options()` appends; `load_persisted_realm`/`save_persisted_realm`/`_route_realm` accept it.
- `reconcile_raccoon_realm()` and call it after settings saves; warning text.
- `/health` adds `raccoon`; `/panel/status` adds `raccoon` masked config.
- `_get_v1_models` branches before WorkBuddy; `_get_raccoon_models` filters by key.
- `_dispatch_chat_post` branches; `_handle_responses` and `_handle_messages` branch.
- Handlers open `wb_raccoon.open_chat`, use existing `_chat_stream_response` / `_chat_nonstream_response` / `_responses_*` / `_messages_*` with `realm="raccoon"`.
- `_codearts_error` pattern for error mapping; use account/proxy fields for usage.
- `record_usage`: `cost_policy = "raccoon:unpriced"`.
- `wb_pricing.cost_for_row`: treat `raccoon:unpriced` like CodeArts.
- `wb_activity.REALMS` add `"raccoon"`.
- `proxy_slots_view` counts Raccoon accounts; slot save calls `wb_raccoon.drop_missing_bindings` / `apply_proxy_slots`.

- [ ] **Step 4: Run targeted suites**

Run: `python3 tests/_test_raccoon_realm.py && python3 tests/_test_raccoon_models.py && python3 tests/_test_raccoon_routing.py`
Expected: PASS=all.

- [ ] **Step 5: Run the full suite and commit**

Run: `python3 tests/run_all.py --jobs 4`
Expected: all pass (new + existing).

```bash
git add app/wb_proxy.py app/wb_settings.py app/wb_pricing.py app/wb_activity.py app/tests/_test_raccoon_realm.py app/tests/_test_raccoon_models.py app/tests/_test_raccoon_routing.py
git commit -m "feat(raccoon): wire the fourth realm into models, chat and usage"
```

---

### Task 5: Admin API, import, loopback OAuth, account actions

**Files:**
- Modify: `app/wb_raccoon.py`
- Modify: `app/wb_proxy.py`
- Create: `app/tests/_test_raccoon_admin.py`
- Test: `app/tests/_test_raccoon_admin.py`

**Interfaces:**
- Consumes: Tasks 1-4.
- Produces:
  - `oauth_start(config, port) -> dict`
  - `oauth_poll(config, session_id) -> dict`
  - `oauth_callback(config, session_id=None, callback_url="", code="", state="") -> dict`
  - `oauth_callback_get(config, query) -> (status, html)`
  - `enable(config, uid)`, `disable(config, uid, reason="")`, `remove(config, uid)`, `clear_cooldown(config, uid)`, `set_note(config, uid, note)`, `set_tasks_enabled(config, uid, enabled)`, `set_proxy_slot(config, uid, slot_id)`, `drop_missing_bindings(accounts_dir, slots)`
  - `overview(config, force=False)`, `health(config, force=False)`
  - Gateway `_get_raccoon_overview`, `_get_raccoon_accounts`, `_get_raccoon_oauth_callback`, `_handle_raccoon`

- [ ] **Step 1: Add OAuth/authorize fixtures to the fake upstream**

`login_with_authorization_code` validates `authorization_code == state.code`, returns a new access/refresh token with the configured UID. Add `exchange_calls`, `exchange_error`.

- [ ] **Step 2: Write failing admin/OAuth tests**

`_test_raccoon_admin.py` starts the fake upstream and gateway, logs into the panel, then checks:

```python
def test_overview_and_accounts_require_panel_session()
def test_overview_never_leaks_tokens()
def test_import_auth_creates_account_and_enables_realm()
def test_oauth_start_builds_loopback_url_and_state()
def test_oauth_callback_creates_account_and_poll_reports_done()
def test_oauth_callback_rejects_bad_state_replay_and_expiry()
def test_manual_callback_url_creates_account()
def test_account_actions_enable_disable_clear_note_proxy_test()
def test_delete_removes_account_file()
def test_models_refresh_and_keepalive()
def test_admin_errors_are_readable_and_secret_free()
```

- [ ] **Step 3: Run to verify failure**

Run: `python3 tests/_test_raccoon_admin.py`
Expected: 401/404/missing functions.

- [ ] **Step 4: Implement admin and OAuth**

- OAuth sessions dict + lock; TTL 300; `public()` has status/message/auth_url/session_id, never code/token.
- `oauth_start` builds the exact authorize URL; require port >= 1024; redirect `http://127.0.0.1:<port>/raccoon/oauth/callback`.
- `oauth_callback_get` validates `state`, extracts `code`, exchanges, saves, auto-enables, HTML success; errors become HTML 400 and session error.
- Manual callback validates loopback URL, path, state/port; or accepts raw code with `session_id`.
- Gateway routes:
  - `GET /raccoon/oauth/callback` before `_panel_ok`.
  - `/raccoon/*` panel routes; `_is_panel_route` includes `/raccoon`.
  - `_handle_raccoon` dispatches all admin actions; maps errors.
- Import accepts `{auth: {...}}` or a flat object; auto-enables.
- `overview` includes `provider`, `accounts`, `stats`, `models`, `base_url`, `health`.
- `test_account`/`keepalive`/`refresh_models` return readable results.

- [ ] **Step 5: Run and commit**

Run: `python3 tests/_test_raccoon_admin.py`
Expected: PASS=all.

```bash
git add app/wb_raccoon.py app/wb_proxy.py app/tests/_fake_raccoon.py app/tests/_test_raccoon_admin.py
git commit -m "feat(raccoon): add admin API and loopback OAuth"
```

---

### Task 6: Dashboard realm card and account table

**Files:**
- Modify: `app/dashboard.html`
- Create: `app/tests/_test_raccoon_switch.js`
- Create: `app/tests/_test_raccoon_accounts.js`

**Interfaces:**
- Consumes: admin endpoints from Task 5.
- Produces DOM/JS:
  - `#tabRaccoon`, `#tabRaccoonViewBadge`, `#tabRaccoonDefaultBadge`, `#tabRaccoonKeyBadge`, `#tabRaccoonHealthy`, `#tabRaccoonTotal`, `#tabRaccoonModels`
  - `#accountsRealmRaccoon`
  - `#raccoonSummary`, `#raccoonCards`, `#raccoonAccountsSection`, `#raccoonAccounts`, `#raccoonAcctCount`
  - `loadRaccoonOverview(force)`, `renderRaccoonAccounts()`, `raccoonAccountAction(action, uid, extra)`, `raccoonSetProxy(uid, slotId, el)`, `raccoonEditNote(uid)`

- [ ] **Step 1: Write the failing switch test**

`_test_raccoon_switch.js` parses `dashboard.html` and asserts:

- `id="tabRaccoon"` with `onclick="switchViewRealm('raccoon')"`;
- `id="accountsRealmRaccoon"` with the same handler;
- `switchViewRealm` recognizes `raccoon` and calls `loadRaccoonOverview`;
- `window.updateUI` handles `VIEW_REALM === "raccoon"` and hides WorkBuddy/CodeArts-only sections;
- `realmLabel("raccoon")` returns a Raccoon label;
- no syntax error when the script is evaluated with a minimal DOM stub.

- [ ] **Step 2: Write the failing account-table test**

`_test_raccoon_accounts.js` stubs `getJSON/postJSON` and calls `renderRaccoonAccounts` with a fixture. Asserts:

- table rows show nickname/UID, state, token remaining, proxy slot;
- buttons call enable/disable/test/note/proxy/delete;
- no token fields are rendered;
- empty state tells the user to import `auth.json`;
- `raccoonSetProxy` posts `/raccoon/accounts/proxy`.

- [ ] **Step 3: Run to verify failure**

Run: `cd /root/opencode/mix2api/app && node tests/_test_raccoon_switch.js && node tests/_test_raccoon_accounts.js`
Expected: missing IDs/functions.

- [ ] **Step 4: Implement the card, realm switching, and account table**

- Add `tabRaccoon` after CodeArts, hidden until `REALM_OPTIONS` includes `raccoon`; badge CSS `realm-badge-raccoon`.
- Add `accountsRealmRaccoon` button.
- Add `raccoonSummary` cards and account section after CodeArts section.
- `updateUI`: `isRaccoonView`; show/hide sections; active default badge; key counts; gateway name/endpoint; toggle button cycle name/label/color.
- `switchViewRealm`: branch `raccoon`; load overview/models/analytics/refresh; don't call WorkBuddy growth/scheduler.
- `initRealm` accepts `?view=raccoon`.
- `refreshActiveRealm` includes `raccoon`.
- `renderRaccoonAccounts` table with actions; `codeartsSlotSelectHtml`-style slot select; no credits/checkin.
- `loadRaccoonOverview` populates `window.RACCOON_OVERVIEW`.

- [ ] **Step 5: Run and commit**

Run: `node tests/_test_raccoon_switch.js && node tests/_test_raccoon_accounts.js`
Expected: PASS.

```bash
git add app/dashboard.html app/tests/_test_raccoon_switch.js app/tests/_test_raccoon_accounts.js
git commit -m "feat(raccoon): add dashboard realm card and account table"
```

---

### Task 7: Dashboard auth modal, settings, key binding, labels

**Files:**
- Modify: `app/dashboard.html`
- Modify: `app/wb_proxy.py` (only if panel status/settings wiring is missing)
- Create: `app/tests/_test_raccoon_oauth.js`
- Create: `app/tests/_test_raccoon_settings.js`

**Interfaces:**
- Consumes: Task 5 admin endpoints, Task 6 card.
- Produces DOM/JS:
  - `#raccoonAuthModal`, `#raccoonAuthInput`, `#raccoonAuthUrl`, `#raccoonAuthStatus`, `#raccoonAuthCallback`
  - `openRaccoonAuth()`, `closeRaccoonAuth()`, `importRaccoonAuth()`, `startRaccoonOauth()`, `pollRaccoonOauth()`, `submitRaccoonCallback()`
  - `#raccoonEnabled`, `#raccoonTimeout`, `#raccoonStreamTimeout`, `#raccoonMaxConcurrent`, `saveRaccoonSettings()`, `testRaccoon()`
  - key form option `raccoon`
  - `ACTIVITY_REALM_LABEL.raccoon`, usage realm labels

- [ ] **Step 1: Write failing auth-modal tests**

`_test_raccoon_oauth.js` asserts:

- clicking "添加账号" opens `#raccoonAuthModal`;
- import posts `{auth: {...}}` to `/raccoon/accounts/import` and reloads;
- start posts `/raccoon/accounts/oauth/start`, renders the URL, and calls poll;
- poll stops on `done`/`error`, refreshes accounts, closes after success;
- manual callback posts `{session_id, callback_url}`;
- the modal never displays token fields.

- [ ] **Step 2: Write failing settings/label tests**

`_test_raccoon_settings.js` asserts:

- settings render from `raccoon` config; save posts `{raccoon: {...}}` to `/settings/save`;
- test posts `/raccoon/test`;
- key realm options include `固定小浣熊出口` / `raccoon`;
- usage/analytics/activity labels map `raccoon` to `小浣熊`;
- no CodeArts-specific reset button is shown for Raccoon.

- [ ] **Step 3: Run to verify failure**

Run: `cd /root/opencode/mix2api/app && node tests/_test_raccoon_oauth.js && node tests/_test_raccoon_settings.js`
Expected: missing IDs/functions.

- [ ] **Step 4: Implement modal, settings, and labels**

- Add modal markup near the CodeArts modal; two tabs: import and browser authorization.
- Import textarea accepts auth.json; parse errors toast.
- Browser auth: start, show URL, copy/open; poll every 1500 ms for 300s; manual callback input.
- Settings section near CodeArts settings: enable, timeout, stream timeout, max concurrent; save/test.
- Key form realm list adds `raccoon` when enabled; label "固定小浣熊出口".
- Usage filter/labels: add `raccoon` maps; analytics realm tag CSS; activity label.
- `renderRaccoonSettings` called from panel status.
- No credits/check-in UI.

- [ ] **Step 5: Run and commit**

Run: `node tests/_test_raccoon_oauth.js && node tests/_test_raccoon_settings.js`
Expected: PASS.

```bash
git add app/dashboard.html app/wb_proxy.py app/tests/_test_raccoon_oauth.js app/tests/_test_raccoon_settings.js
git commit -m "feat(raccoon): add dashboard auth modal, settings and labels"
```

---

### Task 8: Docs, version, full regression, release v1.6.0

**Files:**
- Modify: `README.md`, `app/README.md`, `deploy/docker-compose.image.yml`, `.github/workflows/docker.yml`
- Modify: `app/wb_proxy.py` (version literals `1.6.0`)
- Create: `docs/releases/v1.6.0.md`
- Test: `app/tests/_test_release_engineering.py`, full suite

- [ ] **Step 1: Update version and docs**

- Change both `"version": "1.6.0"` and `server_version = "mix2api/1.6.0"`.
- Root/app README: add Raccoon provider, auth (import + loopback), nine models, tools/images, no credits, QR/password deferred, no `realm=all` merge.
- README badges/current image refs to `v1.6.0`; compose image tag; workflow default tag.
- Add v1.6.0 changelog entry and release notes with Docker, verification, and migration.
- Update the exact `N 个套件：X 个 Python + Y 个 JS` string after counting the tree.
- Confirm no `YYLS/` is reintroduced.

- [ ] **Step 2: Run the full suite**

Run: `cd /root/opencode/mix2api/app && python3 tests/run_all.py --jobs 4`
Expected: all suites pass; record count.

- [ ] **Step 3: Build and verify the deploy zip**

Reuse the release packaging command; exclude `data/`, credentials, `__pycache__`, `.pytest_cache`; include `app/`, `deploy/`, `docs/`, root README/LICENSE/UPSTREAM/Makefile. Verify the zip has no `YYLS/` in code and the compose image is `v1.6.0`.

- [ ] **Step 4: Commit, tag, publish**

```bash
git add README.md app/README.md app/wb_proxy.py deploy/docker-compose.image.yml .github/workflows/docker.yml docs/releases/v1.6.0.md
git commit -m "docs(release): publish mix2api v1.6.0 with the Raccoon provider"
git tag -a v1.6.0 -m "mix2api v1.6.0"
git push origin main
git push origin v1.6.0
```

Create the GitHub release with `mix2api-1.6.0-deploy.zip`; wait for the multi-arch Docker workflow; verify `v1.6.0` and `latest` share a digest and the image starts.

- [ ] **Step 5: Live verification with the existing local auth**

- Import `/tmp/opencode/raccoon-auth.json` through `/raccoon/accounts/import` (never commit it).
- `GET /v1/models?realm=raccoon` returns nine bare IDs.
- Chat non-stream and stream through a key bound to `raccoon`.
- Tool call returns `ping`.
- Image test reads `47`.
- Responses and Anthropic Messages return text.
- Usage rows show `realm=raccoon`, `cost_policy=raccoon:unpriced`, and no token fields.

- [ ] **Step 6: Final report**

Report the release URL, asset, image digest, test count, and live checks. Do not push to `wb-hub`.
