# v1.6.5 WorkBuddy oracle

## Status

Recorded from the read-only v1.6.5 tree with a loopback fake WorkBuddy
upstream plus the existing Raccoon and CodeArts fakes. No real account,
credential, or external network was used.

- Target: `v1.6.5`
- Commit: `e40c4e8994f9cc6ace3457e350466cdd5d422a37`
- Recorded at: `2026-11-11T02:54:24Z`
- Target base URL: `http://127.0.0.1:18767`
- Network: `loopback-only`, private network namespace (`unshare -n`)
- Cases: 18; raw bodies, observations, and per-case SHA256 sums are in
  `oracle.json`

The v1 tree was copied with `git archive`; the copy is ignored by
`tests/oracle/.gitignore` and is not committed:

```sh
git -C /root/opencode/mix2api archive --format=tar v1.6.5 |
  tar -xf - -C tests/oracle/v1.6.5
```

## Test-only endpoint patch

v1.6.5 has no standalone WorkBuddy fake and hardcodes the intl origin. Only the
ignored copy under `tests/oracle/v1.6.5` was patched; the original tree at
`/root/opencode/mix2api` was never modified.

`tests/oracle/patches/apply-v1.6.5-test-patches.sh` applies exactly these
edits to the copy:

- `app/wb_identity.py`: `endpoint_for()` reads `WB_INTL_CHAT_UPSTREAM` for the
  intl chat/catalog/task origin. This is the hook the per-account
  `chat_base_url()` actually uses.
- `app/wb_accounts.py`: the intl `chat_upstream`/`billing_upstream` and the CN
  `billing_upstream` read `WB_INTL_CHAT_UPSTREAM`/`WB_BILLING_UPSTREAM`.
- `app/wb_tasks.py`: `BILL_BASE` reads `WB_BILLING_UPSTREAM`.
- `app/tests/_fake_raccoon.py`: adds `chat_status_body` so the failure fixture
  can return deterministic 400 credit/concurrency bodies. The WorkBuddy suite
  does not use this hook.

The CN chat origin uses v1's existing `WB_CN_CHAT_UPSTREAM` support. The
production UA/version values are unchanged. The patch script is idempotent and
asserts that each target text exists before replacing it.

## Fake upstream

`tests/oracle/fakes/fake_upstream.py` starts four loopback servers:

| Server | Purpose |
| --- | --- |
| Raccoon | imports v1.6.5 `app/tests/_fake_raccoon.py` |
| CodeArts | imports v1.6.5 `app/tests/_fake_codearts.py` |
| WorkBuddy | `/v3/config`, `/v2/chat/completions` SSE, token refresh, billing summary/packages, check-in status/daily check-in, report |
| Control | `POST /__setup` and `GET /__observe` for failure scenarios |

The WorkBuddy fake returns a one-model live catalogue (`hy4-preview`), an SSE
stream with role/content/finish/usage/`[DONE]`, a rotating synthetic JWT on
refresh, and deterministic billing snapshots. All tokens are unsigned
JWT-shaped strings with `sub`/`exp` claims. They are synthetic and are not
stored in this directory.

The recorder was pointed at the fake with:

```sh
WB_CN_CHAT_UPSTREAM="$WB"
WB_INTL_CHAT_UPSTREAM="$WB"
WB_BILLING_UPSTREAM="$WB"
WB_RACCOON_BASE_URL="$RACCOON"
WB_CODEARTS_SNAP_HOST="$CODEARTS"
WB_CODEARTS_SNAP_MANAGER="$CODEARTS/snap-manager"
WB_CODEARTS_PORTAL_HOST="$CODEARTS/portal"
WB_CODEARTS_STS_HOST="$CODEARTS"
WB_CODEARTS_BENEFIT_HOST="$CODEARTS"
```

## Synthetic accounts

Two WorkBuddy accounts were seeded under a disposable `accounts/` directory:

| Realm | UID | Token source |
| --- | --- | --- |
| intl | `oracle-intl` | fake `make_jwt("oracle-intl")` |
| cn | `oracle-cn` | fake `make_jwt("oracle-cn")` |

`settings.json` set `auth_disabled: true` so the recorder could reach `/v1`
without a panel key, `pricing_refresh_minutes: 0` and
`update_check_enabled: false` to disable background network work, and
`enable_anthropic_messages: true` for the `/v1/messages` case.
`auth_disabled` is oracle-only and is not a v2 default.

## Recording command

```sh
tests/oracle/run.sh record \
  --suite workbuddy \
  --base http://127.0.0.1:18767 \
  --out /tmp/mix2api-oracle-workbuddy \
  --version 1.6.5 \
  --commit e40c4e8994f9cc6ace3457e350466cdd5d422a37 \
  --fake-upstream \
  --fake-control "$CONTROL" \
  --panel-token "$PANEL" \
  --api-key "$API_KEY" \
  --note "WorkBuddy intl/cn via test-only patched v1 copy and loopback fake upstream; loopback only"
```

The panel token and API key are synthetic values supplied to the disposable
run; they are not stored in this directory. The recorder refuses a non-loopback
target and refuses to run without `--fake-upstream`.

## Cases

| Case | Method | Path | Status |
| --- | --- | --- | --- |
| wb_models_intl | GET | `/v1/models?realm=intl` | 200 |
| wb_models_cn | GET | `/v1/models?realm=cn` | 200 |
| wb_health | GET | `/health?force=1` | 200 |
| wb_chat | POST | `/v1/chat/completions?realm=intl` | 200 |
| wb_chat_stream | POST | `/v1/chat/completions?realm=intl` (SSE) | 200 |
| wb_chat_cn | POST | `/v1/chat/completions?realm=cn` | 200 |
| wb_chat_stream_cn | POST | `/v1/chat/completions?realm=cn` (SSE) | 200 |
| wb_responses | POST | `/v1/responses?realm=intl` | 200 |
| wb_responses_stream | POST | `/v1/responses?realm=intl` (SSE) | 200 |
| wb_messages | POST | `/v1/messages?realm=intl` | 200 |
| wb_accounts | GET | `/accounts?realm=all` | 200 |
| wb_usage_intl | GET | `/usage?realm=intl` | 200 |
| wb_usage_cn | GET | `/usage?realm=cn` | 200 |
| wb_logs | GET | `/logs` | 200 |
| wb_credits_intl | POST | `/accounts/credits` (`{"realm":"intl"}`) | 200 |
| wb_credits_cn | POST | `/accounts/credits` (`{"realm":"cn"}`) | 200 |
| wb_checkin_cn | POST | `/accounts/checkin` (`{"uid":"oracle-cn"}`) | 200 |
| wb_catalog_refresh_intl | POST | `/catalog/refresh` (`{"realm":"intl"}`) | 200 |

Models is recorded before health so v1 reports the live remote catalogue
(`catalog.source: remote`) rather than the bundled fallback.

## Normalization

`tests/contract/normalize.go` is the single normalizer. The WorkBuddy cases add
no new rules. Generated IDs, timestamps, account/session UUIDs, transport
headers, and SSE chunk boundaries follow the existing contract rules; model
IDs, account UIDs, content, finish reasons, usage, and `[DONE]` survive.

The fixture contains the synthetic absolute state paths
(`/tmp/opencode/oracle-wb/accounts-v1`, `/tmp/opencode/oracle-wb/usage-v1`) so
the v2 comparison used the same fixed layout. They contain no account data.

## v2 comparison

Compared against the `2.0.0-rc.1` parity-fix candidate with the same loopback
fakes and equivalent synthetic accounts:

```sh
tests/oracle/run.sh compare --all \
  --base http://127.0.0.1:18766 \
  --oracle tests/oracle/recorded/v1.6.5-workbuddy \
  --fake-control "$CONTROL" \
  --panel-token "$PANEL" \
  --api-key "$API_KEY"
```

Result: 12 of 18 cases match exactly; 6 differ only by the documented
intentional/additive fields or the log-store scope below. The complete
path-qualified output is committed as `V2-DELTA.txt`. No normalization rule
was added or weakened to hide a stable field.

Matching: `wb_chat`, `wb_chat_stream`, `wb_chat_cn`, `wb_chat_stream_cn`,
`wb_responses`, `wb_responses_stream`, `wb_messages`,
`wb_catalog_refresh_intl`, `wb_accounts`, `wb_credits_intl`, `wb_credits_cn`,
`wb_checkin_cn`.

### Remaining differences

- `wb_models_intl`, `wb_models_cn`: v2 adds the aliases `category`,
  `context_window`, `free`, `max_output`, `max_tokens`, and `provider` to each
  model entry. The v1 capability fields and model IDs match. These fields are
  intentional additive v2 metadata, not normalized away.
- `wb_health`: v1's current-account identity block
  (`uid`, `domain`, `issuer`, `credential_file`, `expires_at`) and the
  key-readiness counts are restored, including the deterministic
  representative account (`oracle-cn` by UID order). v2 still adds `version`
  and the neutral `providers` map; the unconfigured CodeArts block now keeps
  v1's small `configured`/`base_url` shape.
- `wb_usage_intl`, `wb_usage_cn`: the v1 `accounts_map` and current-account
  block are populated from the live provider pools, and `?realm=intl|cn`
  selects v1's representative account. v2 still adds `ttft_ms_sum`,
  `ttft_samples`, `gen_ms_sum`, `gen_samples`, `wall_ms_sum`, and
  `wall_samples`; these timing sums are intentional additive measurements.
- `wb_logs`: v1 records startup/request text in memory; v2 records its own
  request lines in the durable store. The entry shape
  (`id`, `ts`, `time`, `level`, `tag`, `msg`) matches, but v1's startup lines
  are not fabricated into the v2 store. This is a log-store scope difference,
  not a normalization exception.
- `wb_accounts`, `wb_credits_intl`, `wb_credits_cn`, `wb_checkin_cn`: now
  match exactly. v2 removed the v1-extra `activeConcurrent`, `maxConcurrent`,
  and `reason` view fields, serializes unset `lastCheckin`/`lastDailyChat`/
  `lastActivityReport` as `null`, returns the whole WorkBuddy pool in the
  `accounts` array while keeping results target-filtered, and projects the
  check-in result onto v1's `uid/nickname/ok/state/code/msg/data` shape.
- `machineId`/`sessionId`: v2's `deriveViewID` is byte-for-byte the v1
  `wb_fingerprint.derive_id` algorithm (MD5 of `salt:uid`, UUID-v4-shaped
  substitution). The oracle comparison matches both `oracle-cn` and
  `oracle-intl` values exactly; no algorithm difference remains.

No stable field was removed or projected out to make the comparison pass.

## Verification

`go test ./tests/oracle/...` loads this manifest, verifies every checksum and
observation, checks the case set and payloads, and scans every committed byte
for credentials and non-loopback URLs. It performs no network access.
