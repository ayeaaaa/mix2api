# Platform services oracle (Gate D)

This document records how the v2 platform-services contract is compared with
v1.6.5 and what is deliberately excluded. The oracle is **instructions only**:
the original repository `/root/opencode/mix2api` is read-only. A test-only copy
may be made under a temporary directory for manual comparison; it is never
patched, never run against the live data directory, and never committed.

## Included routes

Every route below is mounted exactly once by `internal/api`. Provider-owned
routes (`/accounts`, `/codearts/*`, `/raccoon/*`, `/tasks`, `/activity`,
check-in/growth/admin routes) remain provider-owned and are only checked for
single dispatch, auth class, and preserved shape.

| Group | Routes |
|---|---|
| health/models | `GET /health`, `GET /models`, `GET /v1/models` |
| chat | `POST /chat/completions`, `/v1/chat/completions`, `/completions`, `/v1/completions` |
| responses | `POST /responses`, `/v1/responses`, `/responses/compact`, `/v1/responses/compact` |
| messages | `POST /messages`, `/v1/messages`, `/messages/count_tokens`, `/v1/messages/count_tokens` |
| pricing | `GET /pricing`, `POST /pricing/refresh`, `POST /pricing/mapping` |
| usage | `GET /usage`, `/v1/usage`, `/usage/recent`, `/usage/analytics`, `/usage/perf`, `/usage/timeseries`, `/usage/by-account`, `/usage/remaining` |
| logs | `GET /logs`, `POST /logs/clear`, `GET /logs/export` |
| settings/panel | `GET /settings`, `POST /settings/save`, `GET /settings/reveal`, `GET/POST /realm`, `GET /panel/status`, `POST /panel/login`, `POST /panel/logout`, `POST /panel/password` |
| proxy | `GET /proxy/slots`, `POST /proxy/slots/save`, `POST /proxy/slots/probe`, `POST /proxy/slots/discover` |
| keys | key rows are part of `/settings` (`api_keys`, `deleted_api_keys`, `api_key_masked`, `api_key_set`, `auth_required`) |
| media | `POST /v1/images/generations`, `/images/generations`, `/v1/images/edits`, `/images/edits`, `POST /v1/videos`, `/videos`, `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content`, `POST /v1/videos/{id}/cancel`, `GET /v1/media/tasks`, `GET /media/models`, `/v1/media/models` |
| agents | `GET /agents`, `/agents/available`, `/agents/model-selection`, `POST /agents/model-selection`, `POST /agents/apply`, `POST /agents/restore`, `GET /agents/cli` |
| probes | `POST /quota-scan/run` |
| updates | `GET /updates`, `/updates/backups`, `POST /updates/check`, `POST /updates/install`, `POST /updates/rollback` |

## Excluded routes

- `/console/state` and `/console/toggle` are host-specific UI controls and are
  not mounted by this plan (Plan 5 owns the dashboard surface).
- `/usage/export` is not a v1.6.5 public route. Internal NDJSON export remains
  available to tooling/tests; the public exports are `/accounts/export`
  (provider-owned) and `/logs/export`.
- Dashboard HTML/CSS/JS and language/theme UI (Plan 5).
- Console window controls, shutdown, process restart (host-specific; Plan 5/6).
- Real update file replacement, portable packaging, Docker/Windows/OpenWrt
  packaging, and performance comparison (Plan 6).
- `usage-summary.json` regeneration; v1.6.5 documents it as delete-safe and
  never reads it. v2 must not write it.

## Manual oracle procedure

1. Copy v1.6.5 source to a temporary directory outside the repository:
   `cp -a /root/opencode/mix2api /tmp/mix2api-v1-oracle` (the copy is
   read-only in spirit; never edit the original).
2. Create a synthetic accounts/usage directory from
   `tests/contract/testdata/platform/` and set `ACCOUNTS_DIR`, `USAGE_DIR`,
   `WB_UPDATE_REQUEST_DIR` to it. Never point at production data.
3. Start v1 with a fake upstream (`tests/integration` fixtures or an
   `httptest.Server` that serves deterministic SSE) and a loopback-only bind.
4. Record responses for the included routes, normalizing with
   `tests/contract/normalize.go` semantics: ignore key order, `Date`, `Server`,
   dynamic IDs/timestamps, elapsed values; retain field presence, types,
   numeric values, error codes, SSE event types, text concatenation, finish
   reasons, tool calls, and usage.
5. Run the v2 contract tests:
   `go test ./tests/contract -run 'TestPlatform|TestGateD'`.
6. Compare only semantic values. A missing field is a failure; a new additive
   field is acceptable only when an old client can ignore it.

## Gate D evidence

The contract suite covers health/models/chat/responses/messages, admin
media/agents/update/usage/logs/settings/keys/proxy, provider check-in/growth
dispatch, usage recording for Chat/Responses/Messages, daily guard application,
v1-shaped settings/usage/logs/pricing/media/agents/update data, no network, and
secret scans. The daily-limit oracle must show WorkBuddy-only application:
CodeArts and Raccoon never receive WorkBuddy credit or token counters.

## No-network and no-secret rules

- All tests use fake upstreams, `httptest`, and injected transports. No test
  may contact GitHub, OpenRouter, WorkBuddy, CodeArts, Raccoon, image/video
  hosts, or any real network.
- Fixtures are synthetic/desensitized. No credential, token, prompt, header,
  or production usage row is committed.
- The usage ledger and log export must never contain `MIX2API_API_KEY`,
  panel session tokens, authorization headers, or prompts.
- The checked-in contract files are `tests/contract/platform_services_test.go`,
  `tests/contract/platform_data_test.go`, `tests/contract/platform_golden_test.go`,
  `tests/integration/platform_services_test.go`, `cmd/mix2api/options_test.go`
  (production composition), and the fixture directory
  `tests/contract/testdata/platform/`.
