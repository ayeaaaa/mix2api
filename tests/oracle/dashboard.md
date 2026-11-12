# Dashboard oracle (Gate E)

The v2 binary serves the v1.6.5 dashboard plus the top-bar total-quota
columns (`tabIntlTotal` / `tabCnTotal`) and the Raccoon daily-activity
settings/task controls. This document records the asset
provenance, the static contract, the API inventory the page uses, and the
Gate E checks. The original repository
`/root/opencode/mix2api` is read-only; the copy below is the only committed
asset.

## Asset provenance

| Field | Value |
|---|---|
| v1 release | `mix2api 1.6.5` |
| v1 path | `app/dashboard.html` |
| v2 path | `internal/web/dashboard.html` |
| v1 SHA-256 | `70aa3ddf8c9a1164f140d64e785941d99a7754c3c44887b044a50127e0a96793` |
| v2 SHA-256 | `c83cb50b3ae1696bb7acc4f1db9dc0f4aad2b062ba040fe07a1d227a3ddf40a6` |
| v1 size | 1,284,387 bytes |
| v2 size | 1,309,599 bytes |
| Delta | top-bar total-quota columns (`tabIntlTotal` / `tabCnTotal`), Raccoon daily-activity controls and panel branding controls; otherwise identical to v1.6.5 |
| Embedding | `//go:embed dashboard.html` in `internal/web/assets.go` |
| License | MIT, preserved from the upstream project |

`internal/web/assets.go` exposes `web.DashboardSHA256` and the original
`web.DashboardV1SHA256`; `internal/web/assets_test.go`,
`tests/dashboard/contract_test.go`, and the JS harness input all re-check the
served digest. The v1 hash is kept beside the v2 hash; the intentional
deltas from v1.6.5 are the top-bar total-quota columns, Raccoon
daily-activity controls and panel branding controls.

## Static contract

| Path | Behavior |
|---|---|
| `GET /` | embedded dashboard, `text/html; charset=utf-8` |
| `GET /dashboard.html` | same bytes |
| `GET /dashboard`, `/ui`, `/classic`, `/classic/` | v1-compatible aliases, same bytes |
| `GET /favicon.ico` | `404`, `Cache-Control: no-store` (v1 has no favicon asset) |
| Other paths | `404`; traversal/encoded traversal paths are never opened from disk |
| `HEAD` | same headers and `Content-Length`, no body |
| Other methods | `405`, `Allow: GET, HEAD` |

Headers match v1's static-page contract: `Cache-Control: no-cache` plus a strong
content `ETag`; `If-None-Match` (including weak/list forms and `*`) returns
`304` without a body or `Content-Type`. There is no `Last-Modified` because the
response body is the complete representation. Dashboard and management
responses are same-origin and deliberately carry no
`Access-Control-Allow-Origin`.

The v1 server substituted the instance `ui_language` into the
`data-ui-language` placeholder. v2 serves the embedded bytes exactly as shipped;
the page's own settings load still drives the language selector, and the
placeholder is the documented zh default for the first paint. Runtime patching
would still break the provenance contract above.

## Dashboard API inventory

The inline JavaScript was scanned for `fetch`, `getJSON`, and `postJSON`
literals. `tests/dashboard/routes_test.go` keeps the authoritative
method/path table and fails when a new literal appears without a matching
route. Groups:

| Group | Dashboard calls |
|---|---|
| panel | `GET /panel/status`, `POST /panel/login`, `POST /panel/logout`, `POST /panel/password` |
| settings | `GET /settings`, `POST /settings/save`, `GET /settings/reveal?id=`, `POST /settings/restriction/reset`, `GET/POST /realm` |
| accounts (WorkBuddy) | `GET /accounts`, `GET /accounts/export`, `GET /accounts/login/poll`, `GET /accounts/credits/grants`; POST add-api-key, checkin, credits, credits/detail, daily-chat, daily-chat-web, delete, import, import/desktop, login/cancel, login/start, product, refresh, set, set-all, sync-profile, test |
| activity/tasks | `GET /activity/history`, `GET /tasks`, `POST /tasks/claim`, `/tasks/run`, `/tasks/streak-bonus`, `/tasks/travel` |
| scheduler | `GET /scheduler`, `POST /scheduler/toggle`, `/scheduler/trigger` |
| CodeArts | `GET /codearts/overview`; account actions checkin, clear-cooldown, delete, disable, enable, note, tasks, test, proxy, oauth/start, oauth/poll, oauth/callback; credits/refresh, keepalive, test, models/refresh |
| Raccoon | `GET /raccoon/overview`; account actions clear-cooldown, daily-active, delete, disable, enable, grant-desktop, grant-mobile, import, note, tasks, test, proxy, oauth/start, oauth/poll, oauth/callback; credits/refresh, test, models/refresh |
| usage | `GET /usage`, `/usage/analytics`, `/usage/by-account`, `/usage/perf`, `/usage/recent`, `/usage/remaining`, `/usage/timeseries` |
| logs/pricing | `GET /logs`, `POST /logs/clear`, `GET /pricing`, `POST /pricing/refresh`, `POST /pricing/mapping` |
| media | `GET /v1/media/models`, `GET /v1/media/tasks`, `POST /v1/videos/{id}/cancel` |
| models/agents | `GET /v1/models`, `/agents`, `/agents/available`, `/agents/model-selection`; `POST /agents/model-selection`, `/agents/apply`, `/agents/restore` |
| proxy/updates | `GET /proxy/slots`, `POST /proxy/slots/save`, `/proxy/slots/test`, `/proxy/discover`; `GET /updates`, `/updates/backups`, `POST /updates/check`, `/updates/install`, `/updates/rollback` |
| host controls | `POST /shutdown`; `GET /console/state`, `POST /console/toggle` (kept for old panel builds) |
| static/monitor | `GET /`, `/dashboard.html`, `/dashboard?tab=logs`, `/health` |

Provider-owned routes remain provider-owned; `internal/web` imports only the
standard library and cannot pull a provider into the static surface.

## Auth and no-secrets rules

- `/`, `/dashboard.html`, aliases, `/favicon.ico`, `/health`, and
  `/panel/status` are public.
- Every management route above fails closed with the v1 JSON error envelope
  when no panel session or API key is supplied; unknown paths under a
  management prefix also fail closed.
- Panel sessions travel in `X-Panel-Token`; the server never sets a cookie.
  Password rotation revokes every existing session and mints a new token.
- `/settings` returns masked key rows only. The clear key is available solely
  through the panel-gated `GET /settings/reveal?id=` response. Dashboard
  static bytes, `/panel/status`, provider overviews, and usage/log responses
  never contain the panel token or a clear API key.
- Cross-origin management responses omit CORS headers, and unsafe panel
  requests with a foreign `Origin`/`Sec-Fetch-Site` are rejected.

## JS harness

`tests/dashboard/js_harness_test.go` writes `web.DashboardHTML()` to a temporary
file and runs `node tests/dashboard/node/run.js` with `DASHBOARD_HTML` pointing
at it. Node is CI/development-only: the Go test skips cleanly when `node` is
absent, and no production package imports it. Selected v1 suites cover page
navigation, panel gate first paint, account realm switching, settings load,
daily pending check-in, settings/update UI, and update rollback UI. The copied
fixtures retain their upstream MIT attribution.

Run locally with:

```sh
DASHBOARD_HTML=$PWD/internal/web/dashboard.html node tests/dashboard/node/run.js
```

## Gate E checklist

- [x] Dashboard SHA-256 matches the documented v2 digest
      (`80daa14a...`); the v2 asset adds the top-bar total-quota columns,
daily-activity and branding controls. HTML is
      embedded, not generated.
- [x] `/`, `/dashboard.html`, and the v1 aliases serve identical bytes with
      `text/html`, `no-cache`, strong `ETag`/`304`, and no CORS.
- [x] Every dashboard `fetch`/`getJSON`/`postJSON` path has a registered route;
      unknown paths under managed prefixes stay gated or 404.
- [x] Panel first paint, default-password login, logout, session header,
      password rotation, settings save/reveal, realm switch, provider views,
      and shutdown seam pass against the real composed server.
- [x] Selected inline dashboard JS suites pass under Node; the Go test skips
      cleanly without Node and production has no Node dependency.
- [x] Static/manage responses contain no panel session token or clear API key.
- [x] `CGO_ENABLED=0` Linux and Windows builds succeed.
