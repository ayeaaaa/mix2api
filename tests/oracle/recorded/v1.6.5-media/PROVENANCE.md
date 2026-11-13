# v1.6.5 media oracle

## Status

Recorded from the read-only v1.6.5 tree with a loopback fake WorkBuddy
upstream plus the existing Raccoon and CodeArts fakes. No real account,
credential, or external network was used.

- Target: `v1.6.5`
- Commit: `e40c4e8994f9cc6ace3457e350466cdd5d422a37`
- Recorded at: `2026-11-11T03:51:57Z`
- Target base URL: `http://127.0.0.1:18780`
- Fake control: `http://127.0.0.1:40391`
- Network: `loopback-only`, private network namespace (`unshare -n`)
- Cases: 13; raw bodies, the raw video artifact, and per-case SHA256 sums are
  in `oracle.json`

The v1 tree was copied with `git archive`; the copy is ignored by
`tests/oracle/.gitignore` and is not committed:

```sh
git -C /root/opencode/mix2api archive --format=tar v1.6.5 |
  tar -xf - -C tests/oracle/v1.6.5
```

## Test-only patches

Only the ignored copy under `tests/oracle/v1.6.5` is patched; the original
tree at `/root/opencode/mix2api` was never modified.

`tests/oracle/patches/apply-v1.6.5-test-patches.sh` applies the WorkBuddy
endpoint hooks documented in `tests/oracle/workbuddy.md`:

- `app/wb_identity.py`: intl `endpoint_for()` reads
  `WB_INTL_CHAT_UPSTREAM`.
- `app/wb_accounts.py`: intl chat/billing and CN billing origins read
  `WB_INTL_CHAT_UPSTREAM`/`WB_BILLING_UPSTREAM`.
- `app/wb_tasks.py`: `BILL_BASE` reads `WB_BILLING_UPSTREAM`.

The media protocol layer (`wb_media.py`, `wb_media_client.py`,
`wb_media_tasks.py`) needed no patch: it already uses the account's
`chat_base_url()` for `/v3/config`, image, edit, video submit, and video task
calls, so the same intl endpoint hook points every media request at the fake.

## Fake upstream

`tests/oracle/fakes/fake_upstream.py` starts four loopback servers:

| Server | Purpose |
| --- | --- |
| Raccoon | imports v1.6.5 `app/tests/_fake_raccoon.py` |
| CodeArts | imports v1.6.5 `app/tests/_fake_codearts.py` |
| WorkBuddy | chat, refresh, billing, catalog, and the media success endpoints |
| Control | `POST /__setup` and `GET /__observe` |

The media fixture was recorded with fixed ports so the result URLs are
reproducible:

```sh
python3 tests/oracle/fakes/fake_upstream.py \
  --info "$TMP/fake.json" \
  --workbuddy-port 40390 \
  --control-port 40391
```

The WorkBuddy fake answers:

- `GET /v3/config`: the one-model chat catalog plus four media entries when the
  control scenario is `media*`:
  `hunyuan-image-alpha` and `gemini-image-oracle` (`text-to-image`),
  `hunyuan-image-alpha-edit` (`image-to-image`), and `kling-video-v3`
  (`text-to-video`, `image-to-video`) with a `videoGeneration` block containing
  `minSeconds`/`maxSeconds`/`defaultSeconds`/`resolutions`/`defaultResolution`/
  `aspectRatios`/`defaultAspectRatio`/`maxPromptLength`/`audioMode`/
  `defaultEnableAudio`.
- `POST /v2/images/generations`: `hunyuan-*` returns a deterministic
  `http://127.0.0.1:40390/oracle/media/image.png` URL with
  `usage.credit=5.71`; `gemini-*` returns a deterministic `b64_json` value.
- `POST /v2/images/edits`: the same image bytes at
  `/oracle/media/image-edit.png` with `usage.credit=6.14`.
- `POST /v2/videos/generations`: a deterministic upstream task id
  (`task-oracle-video-N`).
- `POST /v2/videos/tasks`: `completed` with
  `http://127.0.0.1:40390/oracle/media/video.mp4` and
  `usage.credit=128.57`; the `media_running` scenario returns `running` so a
  queued local task can be cancelled deterministically.
- `GET /oracle/media/image.png`, `/oracle/media/image-edit.png`: a fixed 1x1
  PNG; `GET /oracle/media/video.mp4`: the fixed bytes
  `oracle-media-video-bytes-v1\n`.

All media responses are envelope-shaped (`{code, msg, data}`) exactly like the
upstream protocol; the gateway never needs a real account or external host.

## Synthetic accounts and settings

One intl WorkBuddy account was seeded under a disposable `accounts/` directory:

| Realm | UID | Token source |
| --- | --- | --- |
| intl | `oracle-intl` | fake `make_jwt("oracle-intl")` |

`settings.json` set:

- `auth_disabled: true` so the recorder could reach `/v1` without a real
  login; this is oracle-only and is not a v2 default;
- one synthetic `api_keys` row (`oracle-media-key`) so both targets attribute
  usage to the same stable key id;
- `media: {enabled: true, realm: "intl", identity: "desktop",
  image_max_per_minute: 6, video_max_seconds: 15,
  daily_credits_per_key: 0, video_concurrency: 1}`;
- `pricing_enabled: false`, `pricing_refresh_minutes: 0`, and
  `update_check_enabled: false` to disable background network work;
- `tasksEnabled: false` on the account so scheduled check-in/daily jobs do not
  add unrelated upstream calls.

The account is a synthetic JWT-shaped credential; it is not stored in this
directory. The panel token and API key are synthetic values supplied to the
disposable run and are not stored either.

The fixture contains the synthetic absolute usage path
`/tmp/opencode/media-oracle/usage/usage.jsonl` in the `/usage` snapshot. The v2
comparison used the same fixed usage path (with the ledger cleared between
runs) so the `log_file` field is compared exactly rather than normalized away.

## Recording command

```sh
tests/oracle/run.sh record \
  --suite media \
  --base http://127.0.0.1:18780 \
  --out /tmp/mix2api-oracle-media \
  --version 1.6.5 \
  --commit e40c4e8994f9cc6ace3457e350466cdd5d422a37 \
  --fake-upstream \
  --fake-control "$CONTROL" \
  --panel-token "$PANEL" \
  --api-key "$API_KEY" \
  --note "WorkBuddy media success paths via test-only patched v1 copy and loopback fake upstream; loopback only"
```

The recorder refuses a non-loopback target and refuses to run without
`--fake-upstream`.

## Cases

| Case | Method | Path | Status |
| --- | --- | --- | --- |
| media_models | GET | `/v1/media/models` | 200 |
| media_image_generation | POST | `/v1/images/generations` | 200 |
| media_image_edit | POST | `/v1/images/edits` | 200 |
| media_image_b64 | POST | `/v1/images/generations` (`gemini-image-oracle`) | 200 |
| media_video_wait | POST | `/v1/videos?wait=1` | 202 |
| media_video_status | GET | `/v1/videos/{captured id}` | 200 |
| media_video_content | GET | `{captured signed content URL}` | 200 (raw) |
| media_video_running | POST | `/v1/videos` | 202 |
| media_video_queued | POST | `/v1/videos` (local queue) | 202 |
| media_video_cancel | POST | `/v1/videos/{captured queued id}/cancel` | 200 |
| media_tasks | GET | `/v1/media/tasks` | 200 |
| media_usage | GET | `/usage?realm=intl` | 200 |
| media_usage_recent | GET | `/usage/recent?realm=intl&limit=10` | 200 |

The harness captures the generated task id and signed content URL from the
`media_video_wait` response, and the queued task id from
`media_video_queued`, so later cases address the same resource without
hand-editing the fixture. `media_video_content` is recorded as a raw body and
compared byte-for-byte.

## Normalization

`tests/contract/normalize.go` is the single normalizer. The media cases add:

- `vid_`/`vid-` generated task ids to the existing dynamic-id prefixes;
- timestamp normalization for usage-row `at`/`iso` (in addition to the
  existing `created`/`completed_at`/`started`/`since` rules);
- the harness replaces only the generated task id and signed token inside a
  `/v1/videos/<id>/content?i=<n>&t=<token>` URL and the task id embedded in
  the queued-submit `hint`. The route, index, and query-key shape survive, so
  a different URL shape still fails.

Stable fields are not projected: model ids, tags, `videoGeneration` values,
credits, usage `kind`, `cost_policy`, account/key ids, task statuses, and the
raw video bytes are compared in full.

## v2 comparison

Compared against the `2.0.0-rc.1` parity-fix candidate with the same loopback
fake process and equivalent synthetic settings:

```sh
tests/oracle/run.sh compare --all \
  --base http://127.0.0.1:18781 \
  --oracle tests/oracle/recorded/v1.6.5-media \
  --fake-control "$CONTROL" \
  --panel-token "$PANEL" \
  --api-key "$API_KEY"
```

Result: 11 of 13 cases match exactly; 2 differ by the documented additive or
zero-valued fields below. The complete path-qualified output is committed as
`V2-DELTA.txt`. No normalization rule was added or weakened to hide a stable
field.

Matching: `media_models`, `media_image_generation`, `media_image_edit`,
`media_image_b64`, `media_video_wait`, `media_video_status`,
`media_video_content`, `media_video_running`, `media_video_queued`,
`media_video_cancel`, `media_usage`.

### Remaining differences

- `media_tasks`: v2's `stats` adds `submitting`, `max_pending`, and
  `max_tasks`. v2's `workers` is the fixed TaskStore default `3`, while v1
  sets `workers = video_concurrency` (`1` in this fixture). Task rows, status
  mapping, credits, and signed URLs match exactly.
- `media_usage_recent`: v2's media rows serialize `ttft_ms` and `gen_ms` as
  `0` where v1 writes `null`; the completed video row also writes
  `elapsed_ms: 0` where v1 writes `null`. v2 adds `resolution` to image rows.
  The row `kind` (`image`/`video`), `cost_policy: media`, model, account, key,
  and credit values match exactly.

These are stable-field differences, not normalization exceptions. They are
reported rather than silently aligned.

## Verification

`go test ./tests/oracle/...` loads this manifest, verifies every checksum and
the raw video bytes, checks the case set and payloads, asserts the image/video
semantics and usage row kinds, and scans every committed byte for credentials
and non-loopback URLs. It performs no network access.
