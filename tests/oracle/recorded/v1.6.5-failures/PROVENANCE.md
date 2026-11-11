# v1.6.5 failure-mode oracle

## Status

Recorded from the read-only v1.6.5 tree against the loopback fake upstreams in
`tests/oracle/fakes/fake_upstream.py`. No real account, credential, or external
network was used.

- Target: `v1.6.5`
- Commit: `e40c4e8994f9cc6ace3457e350466cdd5d422a37`
- Network: `loopback-only`, private network namespace (`unshare -n`)
- Suites: Raccoon (5 cases), CodeArts (5 cases), WorkBuddy (5 cases)

Each provider has its own `oracle.json` and raw bodies:

| Directory | Base URL | Recorded at |
| --- | --- | --- |
| `raccoon/` | `http://127.0.0.1:18768` | `2026-11-11T02:52:21Z` |
| `codearts/` | `http://127.0.0.1:18770` | `2026-11-11T02:52:45Z` |
| `workbuddy/` | `http://127.0.0.1:18772` | `2026-11-11T02:53:22Z` |

The fake control origin is recorded in each manifest
(`fake_control_url: http://127.0.0.1:40381`). It is loopback-only and is used
for deterministic scenario setup and observation.

## Scenario control and observation

The oracle harness calls `POST /__setup` before a failure case and
`GET /__observe?provider=...` after it. The setup resets the fake's counters and
selects a deterministic failure scenario. The observation contains only call
counts and method/path/status tuples, never credentials or request bodies:

```json
{"provider":"raccoon","chat_calls":2,"refresh_calls":0,"catalog_calls":0,
 "requests":[{"method":"POST","path":"/api/web/llm/v2/chat/completions"}]}
```

The observation is committed as `<case>.observe.json` with its own SHA256 in
`oracle.json`. `CompareAll` compares the observation as well as the response
body, so exactly-once refresh/retry/failover behaviour is part of the oracle
rather than an inference.

The setup/observation hooks are test scaffolding. They are not target requests
and are not part of the v1/v2 wire contract.

## Synthetic accounts

- Raccoon: four accounts, all using the fake's synthetic JWT and
  `refresh-1`; `raccoon.enabled: true`, `max_concurrent: 1`.
- CodeArts: four accounts, all using the fake's synthetic `TOKEN`/`AK`/`SK`,
  a future expiration, and `refresh-1`; `codearts.enabled: true`,
  `max_concurrent: 1`.
- WorkBuddy: four intl accounts using the fake's synthetic intl JWT;
  `workbuddy.intl.enabled: true`, `max_concurrent: 1`.

All settings use `auth_disabled: true` for the disposable run. No real token,
AK/SK, DPoP key, or account identity is present.

## Scenarios and recorded behaviour

### Raccoon

| Case | Scenario | v1 status | Observed upstream calls |
| --- | --- | --- | --- |
| `raccoon_429_failover` | first chat 429 | 200 | 2 chat, 0 refresh; second account serves |
| `raccoon_429_failover_stream` | first chat 429 (SSE) | 200 | 2 chat, 0 refresh; SSE content after failover |
| `raccoon_credit_400` | first chat 400 `insufficient credits` | 400 | 1 chat, 0 refresh; v1 treats 400 as a hard bad request and does not fail over |
| `raccoon_concurrency_400` | first chat 400 `concurrent session limit` | 400 | 1 chat, 0 refresh; v1 does not retry a 400 |
| `raccoon_401_refresh` | first chat 401 | 200 | 2 chat, 1 refresh; refreshes the same account and retries |

### CodeArts

| Case | Scenario | v1 status | Observed upstream calls |
| --- | --- | --- | --- |
| `codearts_429_failover` | first chat 429 | 200 | 2 chat, 0 token; second account serves |
| `codearts_429_failover_stream` | first chat 429 (SSE) | 200 | 2 chat, 0 token; SSE content after failover |
| `codearts_credit_400` | first chat 400 credit-exhausted body | 200 | 2 chat, 0 token; exhausted account parked, next account serves |
| `codearts_concurrency_retry` | first chat 400 concurrency body | 200 | 2 chat, 0 token; same account retried after the 2s backoff |
| `codearts_401_failover` | first chat 401 | 502 | 1 chat, 0 token; v1.6.5 treats a chat 401 as terminal for the credential and stops the request rather than calling the refresh endpoint |

The CodeArts 401 result is a recorded v1 behaviour, not a harness limitation.
v2 matches it exactly. A pre-flight token refresh is exercised by v1's
`_ensure_token` path and by the v2 contract suite; it is not triggered by a chat
401 in either target.

### WorkBuddy

| Case | Scenario | v1 status | Observed upstream calls |
| --- | --- | --- | --- |
| `wb_429_failover` | first chat 429 | 200 | 2 chat, 0 refresh; next intl account serves |
| `wb_429_failover_stream` | first chat 429 (SSE) | 200 | 2 chat, 0 refresh |
| `wb_401_refresh` | first chat 401 | 200 | 2 chat, 1 refresh; same account refreshed and retried |
| `wb_5xx_failover` | first chat 500 | 200 | 2 chat, 0 refresh; next intl account serves |
| `wb_5xx_failover_stream` | first chat 500 (SSE) | 200 | 2 chat, 0 refresh |

## v2 comparison

The same loopback fakes and equivalent synthetic accounts were used against the
`2.0.0-rc.1` candidate:

```sh
tests/oracle/run.sh compare --all \
  --base http://127.0.0.1:18769 \
  --oracle tests/oracle/recorded/v1.6.5-failures/raccoon \
  --fake-control "$CONTROL" --api-key "$API_KEY"
```

| Suite | Exact | Differ |
| --- | --- | --- |
| Raccoon | 5 / 5 | 0 |
| CodeArts | 5 / 5 | 0 |
| WorkBuddy | 5 / 5 | 0 |

The complete path-qualified output is in `V2-DELTA.txt`. The former Raccoon
credit-exhausted difference is resolved: the provider error exposes v1's
optional `gateway_hint` through a generic interface, and the API writes
`error.gateway_hint` exactly when it is set. All three failure suites match
exactly, including the upstream call counts and retry/failover observations.

## Verification

`go test ./tests/oracle/...` loads every manifest, verifies each response and
observation checksum, checks the setup/observe metadata, normalizes every
payload, and scans every committed byte for credentials and non-loopback URLs.
It performs no network access.
