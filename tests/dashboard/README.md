# Dashboard tests

`go test ./tests/dashboard` composes the real server (`app.Build` →
`app.BuildPlatform` → `api.New`) over a temporary accounts/usage directory. It
does not use pass-through auth or provider stubs.

## Go suites

| Test | Covers |
|---|---|
| `TestDashboardAPIInventoryIsCovered` | every `fetch`/`getJSON`/`postJSON` literal has a route table entry |
| `TestDashboardAPIRoutesExistAndAreGated` | real route dispatch and fail-closed JSON envelopes |
| `TestDashboardStaticAliases` | `/`, `/dashboard.html`, v1 aliases, favicon, byte hash |
| `TestPanel*` | first paint, login/logout, password rotation, session header |
| `TestSettings*` | settings save, masked rows, reveal, no secret leakage |
| `TestRealmSwitchAndProviderViews` | realm switch and provider/panel views |
| `TestDashboardJS*` | Node harness (CI-only; skipped when `node` is absent) |
| `TestDashboard*Contract` | Gate E static/auth/route/harness artifacts |

## Node prerequisite

The JS suites are development/CI-only. Production builds never import or run
Node. If `node` is unavailable the Go harness reports `SKIP`; it is not a test
failure.

Manual run against the embedded source (no build step):

```sh
DASHBOARD_HTML=$PWD/internal/web/dashboard.html node tests/dashboard/node/run.js
```

`tests/dashboard/node/_dashboard_source.js` accepts `DASHBOARD_HTML`; the Go
harness points it at a temporary copy of the embedded bytes so the suites always
execute exactly what the binary serves. The DOM/sandbox helpers and the seven
selected suites are copied from the MIT-licensed v1.6.5 test tree; the original
repository remains read-only.
