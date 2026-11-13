# WorkBuddy media success oracle

The committed fixture is `tests/oracle/recorded/v1.6.5-media/` (13 cases).
Its `PROVENANCE.md` records the exact fake endpoints, synthetic settings,
recording command, and v2 delta. This page is the short procedure.

## 1. Copy and patch v1.6.5

The source tree `/root/opencode/mix2api` is read-only. Copy the tag and apply
the existing test-only WorkBuddy endpoint patches:

```sh
set -euo pipefail
PIN=e40c4e8994f9cc6ace3457e350466cdd5d422a37
test "$(git -C /root/opencode/mix2api rev-parse 'v1.6.5^{}')" = "$PIN"
rm -rf tests/oracle/v1.6.5
mkdir -p tests/oracle/v1.6.5
git -C /root/opencode/mix2api archive --format=tar v1.6.5 | tar -xf - -C tests/oracle/v1.6.5
tests/oracle/patches/apply-v1.6.5-test-patches.sh
```

No media-layer patch is needed: `wb_media_client` already uses the account's
`chat_base_url()`.

## 2. Start the fake with fixed ports

```sh
python3 tests/oracle/fakes/fake_upstream.py \
  --info "$TMP/fake.json" \
  --workbuddy-port 40390 \
  --control-port 40391
```

The `media*` control scenarios add the media catalog entries and, for
`media_running`, keep video task polling in `running` so a locally queued task
can be cancelled deterministically.

## 3. Seed one intl account and media settings

Use the fake's `workbuddy_intl_token` as `accessToken`. `settings.json` needs:

```json
{
  "auth_disabled": true,
  "api_keys": [{
    "id": "oracle-media-key", "name": "Oracle Media",
    "key": "<synthetic api key>", "realm": "", "models": [],
    "enabled": true, "created_at": "", "deleted_at": ""
  }],
  "media": {
    "enabled": true, "realm": "intl", "identity": "desktop",
    "image_max_per_minute": 6, "video_max_seconds": 15,
    "daily_credits_per_key": 0, "video_concurrency": 1
  },
  "pricing_enabled": false,
  "pricing_refresh_minutes": 0,
  "update_check_enabled": false
}
```

Set `tasksEnabled: false` on the synthetic account so scheduled jobs do not
add unrelated upstream calls. `auth_disabled` is oracle-only and is not a v2
default.

## 4. Record v1

```sh
WB_INTL_CHAT_UPSTREAM="$WB" WB_CN_CHAT_UPSTREAM="$WB" \
WB_BILLING_UPSTREAM="$WB" WB_MEDIA_ENABLED=1 \
ACCOUNTS_DIR="$ACCOUNTS_V1" WB_PROXY_USAGE_DIR="$USAGE" \
WB_MEDIA_DIR="$MEDIA_V1" \
python3 tests/oracle/v1.6.5/app/wb_proxy.py --port 18780 \
  --panel-password "$PANEL_PASSWORD"

tests/oracle/run.sh record --suite media \
  --base http://127.0.0.1:18780 --out "$OUT/recorded" \
  --version 1.6.5 --commit "$PIN" \
  --fake-upstream --fake-control "$CONTROL" \
  --panel-token "$PANEL" --api-key "$API_KEY"
```

## 5. Compare v2

Start the v2 candidate against the same fake and an equivalent synthetic
settings/account directory, then:

```sh
tests/oracle/run.sh compare --all \
  --base http://127.0.0.1:18781 \
  --oracle tests/oracle/recorded/v1.6.5-media \
  --fake-control "$CONTROL" \
  --panel-token "$PANEL" --api-key "$API_KEY"
```

The reviewed result is 11 exact / 2 documented differences; see
`recorded/v1.6.5-media/V2-DELTA.txt`. Do not normalize a stable field away to
make a case pass.

## 6. Verify

```sh
go test ./tests/oracle/...
go test ./...
go vet ./...
gofmt -l .
git -C /root/opencode/mix2api status --short
git status --short
git check-ignore tests/oracle/v1.6.5
```

The fixture test loads the manifest, verifies every checksum and the raw video
bytes, asserts image/video semantics and usage row kinds, and scans every
committed byte for credentials and non-loopback URLs. It performs no network
access.
