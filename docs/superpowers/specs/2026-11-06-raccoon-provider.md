# Raccoon (小浣熊) Native Provider Design

**Status:** approved architecture: native Python, single process
**Target release:** v1.6.0
**Scope owner:** mix2api

## 1. Context

`/tmp/opencode/raccoon-desktop-proxy/` is a Node.js, single-account OpenAI-compatible
proxy for SenseTime Raccoon (商汤小浣熊). It reads the desktop client's
`auth.json`, calls the hosted gateway, and exposes `/v1/models` and
`/v1/chat/completions`.

Verified against a real account:

- IAM JWT access token, about 30 days;
- 9 catalog models, each with `context_window`, `max_tokens`, and tags;
- OpenAI chat non-stream and SSE stream;
- tool calls and image input;
- usage block in the final stream frame;
- no account credit/points endpoint in the proxy.

The Node process is a protocol reference only. `mix2api` must keep its
single-process Python model, so the provider is ported natively.

## 2. Goals

1. Add a fourth realm `raccoon` with its own account pool, model catalog,
   chat transport, usage attribution, and dashboard surface.
2. Keep the existing WorkBuddy (`intl` / `cn`) and CodeArts (`codearts`)
   behavior unchanged.
3. Support multiple Raccoon accounts with enable/disable, cooldown, notes,
   account-level proxy slots, per-key account/model restrictions, and
   failover.
4. Accept accounts by importing an existing `auth.json` and by a local
   loopback OAuth flow. Auto-enable the realm after the first import.
5. Expose original Raccoon model IDs. No `YYLS/`-style namespace and no
   provider prefix in v1.6.0.
6. Keep the OpenAI/Anthropic/Responses surface consistent with the other
   realms: `/v1/models`, `/v1/chat/completions`, `/v1/responses`, and
   `/v1/messages`.

## 3. Non-goals (v1.6.0)

- QR-code login and phone/password login. Both are deferred; QR needs a
  QR image renderer, and password login needs AES-128-CFB. The provider
  does not add third-party Python dependencies.
- Account credits, check-in, or growth tasks. Raccoon exposes none of these
  in the reference proxy.
- Merging Raccoon into `/v1/models?realm=all`. As decided in v1.0.9, the
  gateway serves one exit at a time; a provider prefix can return when
  multiple exits are served simultaneously.
- A Node sidecar. Raccoon runs entirely in the Python gateway.

## 4. Architecture

### 4.1 Module boundary

New module `app/wb_raccoon.py` owns:

- config in `settings.json` under `"raccoon"`;
- account files and runtime state;
- auth normalization, JWT parsing, token refresh;
- catalog fetch/normalization and seed fallback;
- account pool selection, concurrency, cooldown, affinity;
- chat body construction, HTTP transport, SSE wrapper;
- loopback OAuth sessions;
- admin operations used by the panel.

`app/wb_proxy.py` owns routing and HTTP adaptation only. It does not know
Raccoon's auth or wire protocol.

`app/wb_settings.py` adds `"raccoon"` to `REALMS` and keeps the existing
lazy-module pattern (no module-level import of `wb_raccoon`).

`app/wb_upstream_pool.py` remains the transport: connection reuse, proxy
auth, `no_proxy`, and stale-connection retry are inherited.

### 4.2 Config

```json
{
  "raccoon": {
    "enabled": false,
    "timeout_seconds": 60,
    "stream_timeout_seconds": 900,
    "models_ttl_seconds": 600,
    "max_concurrent": 1
  }
}
```

Rules:

- `enabled` gates the realm. The first successful import sets it to true.
- Values are clamped to safe ranges; malformed values fall back to defaults.
- `max_concurrent` is per account (1..10).
- The realm is selectable only when `enabled` is true.

### 4.3 Account data

Directory: `accounts/raccoon/`

- `raccoon-<safe-uid>.json` for each account:
  - `uid`, `access_token`, `refresh_token`, `token_type`,
    `office_identity`, `office_org_name`, `office_org_role`,
    `proxySlot`, `updated_at`.
- `state.json` keyed by UID:
  - `disabled`, `reason`, `cool_until`, `err_count`, `last_error`,
    `note`, `tasks_enabled`.

UID is the JWT `sub` (for example `pu-...`). If the token is opaque, use
`raccoon-<sha256(refresh_token or access_token)[:12]>`. Display name is
`office_org_name`, else a short UID. Tokens never appear in any view,
admin response, log line, or usage row.

### 4.4 Auth

**Import**: accept a flat `auth.json` object (or `{"data": {...}}` from an
auth response), require a non-empty `access_token`, derive UID/expiry, save
atomically, and auto-enable the realm.

**Loopback OAuth**:

1. Start a session with a 32-byte random `state` and a 300-second TTL.
2. Authorize URL:
   `https://xiaohuanxiong.com/authorize?login_source=desktop&appname=办公小浣熊客户端&desktop_callback=loopback&redirect=http://127.0.0.1:<gateway-port>/raccoon/oauth/callback&state=<state>`
3. The gateway's `GET /raccoon/oauth/callback` is exempt from the panel
   session gate but requires a pending state. It exchanges
   `{"authorization_code": code}` at
   `/api/web/auth/v1/login_with_authorization_code`, saves the account,
   and renders a small HTML success/error page.
4. The panel polls `/raccoon/accounts/oauth/poll` with the panel session.
5. Manual callback paste is supported for deployments where the browser
   cannot reach the gateway's loopback; it accepts a full callback URL or
   a raw code plus the session.
6. A state is single-use. Unknown, expired, or replayed states return 400.

Constraint: the browser and the gateway must share loopback. Remote users
should import `auth.json`; the UI states this.

**Refresh**: POST `/api/web/auth/v1/refresh` with
`Authorization: Bearer <access>` and `{"refresh_token": token}`. Accept
`data.access_token` and `data.refresh_token`; save atomically. Refresh is
single-flight per account: a lock plus a double-check means concurrent
401s cause one refresh, not a refresh-token race. A failed refresh marks
the account as needing re-login.

### 4.5 Catalog

`GET /api/web/llm/v2/model_catalog` with the account headers.

Normalize the `chat` category:

- ID from `name` or `model_name`; display name from `display_name` or
  `description`;
- `context_window` / `max_tokens` from `params` or top level;
- tags array.

Cache for `models_ttl_seconds` (module-level, account-independent).
Fallback to a 9-model seed catalog if the fetch fails. Never return a
prefixed ID.

Capabilities:

- `vision` when the tags contain `vision`;
- `reasoning` when the tags contain `reasoning`;
- `tool_calls` true (verified upstream);
- text output only.

No `reasoning_efforts` list is advertised in v1.6.0 because the upstream
effort levels are not verified. The request field is still forwarded.

### 4.6 Chat transport

Request target:
`POST https://xiaohuanxiong.com/api/web/llm/v2/chat/completions`.

Headers:

```
Authorization: Bearer <access_token>
Content-Type: application/json
Accept: text/event-stream
X-Client-Platform: desktop-linux-x64
X-Client-Version: v1.1.5
X-Raccoon-Language: zh
Cache-Control: no-store
X-Org-Code: <office_identity>   # only when not personal/empty
```

The gateway always asks upstream for `stream: true`; non-streaming
clients are served by aggregating the SSE stream. The allowlist is the
OpenAI chat surface: model, messages, tools, tool_choice,
parallel_tool_calls, temperature, top_p, max_tokens, stop,
response_format, seed, penalties, user, reasoning_effort, thinking,
enable_thinking. `max_tokens` is clamped to the catalog's output limit.

`RaccoonStream` wraps the pooled response and yields raw SSE lines. A
`preflight()` reads the first data event so an HTTP 200 with an embedded
error is raised before headers are sent. `close()` closes the response
and releases the pool slot. The gateway's existing
`_chat_stream_response` / `aggregate_stream` handle chunk normalization,
usage, and error frames.

Retry/failover:

- 401/403 → force refresh once, retry the same account; if refresh fails,
  mark terminal and try the next account.
- 429/5xx → soft cooldown and try the next account.
- 400 → return to the client immediately; do not try another account.
- All accounts unavailable → `RaccoonUnavailable` with a readable reason.

### 4.7 Gateway integration

- `REALMS = ("", "intl", "cn", "codearts", "raccoon")`.
- `realm_options()` adds `raccoon` when enabled; persisted realm and
  `/realm` accept it; disabling it reconciles the default exit back to
  `intl`.
- `/health` adds a `raccoon` block (configured, accounts, healthy,
  catalog source).
- `/v1/models` with `realm=raccoon` returns bare IDs from
  `wb_raccoon.model_entries`, key-filtered.
- `/v1/models?realm=all` does not include Raccoon.
- Chat, Responses, and Anthropic Messages branch on `req_realm ==
  "raccoon"` and use the same response handlers as CodeArts.
- `record_usage` writes `cost_policy = "raccoon:unpriced"`; token usage
  is recorded exactly, credit stays 0.
- Account-level allowed lists and key-level model filters apply.
- Proxy slot save/remove rebinds Raccoon accounts together with WorkBuddy
  and CodeArts.

### 4.8 Admin API

Panel-session endpoints:

- `GET /raccoon/overview`, `GET /raccoon/accounts`
- `POST /raccoon/accounts/import` `{auth: {...}}`
- `POST /raccoon/accounts/oauth/start`
- `POST /raccoon/accounts/oauth/poll` `{session_id}`
- `POST /raccoon/accounts/oauth/callback` `{session_id, callback_url}`
- `POST /raccoon/accounts/{enable,disable,delete,note,proxy,test,clear-cooldown}`
- `POST /raccoon/models/refresh`
- `POST /raccoon/keepalive` (refresh expiring tokens + catalog; no check-in)
- `GET /raccoon/oauth/callback` (public, state-gated HTML handler)

All admin responses omit access/refresh tokens. Test/refresh errors are
returned as readable 4xx/502 messages.

### 4.9 Dashboard

- Fourth realm card `Raccoon (小浣熊)`, shown when the realm is enabled.
- Realm switch in the header and on the accounts page.
- Account section with import, OAuth, enable/disable, delete, note,
  proxy slot, test, clear cooldown, refresh catalog.
- Auth modal with two tabs:
  - import `auth.json` (textarea);
  - browser authorization (URL, start, poll, manual callback).
- Settings section: enable, timeout, stream timeout, max concurrency.
- Key binding option `raccoon`.
- Usage/activity labels show `小浣熊`; cost shows unpriced.
- No credits/check-in UI for Raccoon.

## 5. Error and usage policy

Provider errors map to:

| Provider error | HTTP | Meaning |
| --- | --- | --- |
| `RaccoonBadRequest` | 400 | client/request invalid, no failover |
| `RaccoonAuthError` | 502 | re-login required |
| `RaccoonNotFound` | 409 | account/session missing |
| `RaccoonRateLimited` | 429 | upstream rate limit |
| `RaccoonUpstreamError` | 502 | upstream failure |
| `RaccoonUnavailable` | 503 | no usable account |

Every terminal result writes one usage row with the requested model,
upstream account UID, realm `raccoon`, token usage when known, and
`cost_policy = "raccoon:unpriced"`.

## 6. Testing strategy

- A fake Raccoon upstream covers auth exchange/refresh, catalog, SSE
  chat, tools, images, usage, 401/429/5xx, and truncated streams.
- Provider suites cover config, auth import, JWT, refresh single-flight,
  pool selection/cooldown, catalog, body building, and stream behavior.
- Gateway suites cover realm selection, models, key/account filters,
  chat/Responses/Messages, usage rows, and error mapping.
- Admin suites cover panel gating, import, OAuth state validation/replay,
  account actions, proxy slots, and no token leakage.
- JS suites cover the fourth card, account table, auth modal, settings,
  and label mappings.

## 7. Release

- New provider → `v1.6.0`.
- Bump source version strings to `1.6.0`.
- Update README, app README, release notes, compose image, workflow
  default, and the test-suite count.
- Publish a multi-arch GHCR image and the deploy zip.
- Live verification: import the existing local `raccoon-auth.json`,
  list bare models, run non-stream/stream chat, tool call, image read, and
  Anthropic/Responses adapters.

## 8. Open items (deferred)

- QR login (needs a QR renderer).
- Phone/password login (needs AES-128-CFB).
- Raccoon credits/points, if an endpoint is discovered.
- Provider prefixes for simultaneous multi-exit model discovery.
