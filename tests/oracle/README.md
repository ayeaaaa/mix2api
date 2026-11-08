# v1.6.5 oracle for contract goldens

The v2 golden fixtures in `tests/contract/testdata` are currently hand-written
v2 regression goldens, **not captured v1.6.5 oracle output** (see
`tests/contract/testdata/PROVENANCE.md`). The procedure below is how to record
real oracle fixtures later.

The read-only Python v1.6.5 tree at `/root/opencode/mix2api` is the intended
oracle: **never modify it**, and do not commit the copied tree. This directory
is only the place to keep a local, test-only copy when recording new goldens.

All commands below run from `/root/opencode/mix2api-v2` in one shell. `$TMP`
must stay set while recording; the scripts deliberately leave the fake upstream
and v1 gateway running until the shell exits.

## 1. Copy v1.6.5 without touching the source

The v1.6.5 tag currently resolves to
`e40c4e8994f9cc6ace3457e350466cdd5d422a37`. The copy is read-only from the
source tree's point of view.

```bash
set -euo pipefail
PIN=e40c4e8994f9cc6ace3457e350466cdd5d422a37
test "$(git -C /root/opencode/mix2api rev-parse 'v1.6.5^{}')" = "$PIN"
rm -rf tests/oracle/v1.6.5
mkdir -p tests/oracle/v1.6.5
git -C /root/opencode/mix2api archive --format=tar v1.6.5 | tar -xf - -C tests/oracle/v1.6.5
```

`tests/oracle/.gitignore` excludes `v1.6.5/` and Python caches, so the copy
cannot be committed accidentally. Do not copy the whole v1 repository into a
tracked v2 path.

## 2. Start the fake upstream (foreground script, background process)

`_fake_raccoon` lives in `app/tests`, not `app`; both paths are added below
because `start_server()` imports sibling provider modules from `app`.

```bash
TMP=$(mktemp -d)
cat > "$TMP/fake_upstream.py" <<'PY'
import json
import os
import sys
import time

ROOT = os.getcwd()
sys.path.insert(0, os.path.join(ROOT, "tests/oracle/v1.6.5/app/tests"))
sys.path.insert(0, os.path.join(ROOT, "tests/oracle/v1.6.5/app"))

from _fake_raccoon import start_server  # noqa: E402

server, base, state = start_server()
with open(os.environ["FAKE_INFO"], "w", encoding="utf-8") as fh:
    json.dump(
        {
            "base": base,
            "uid": state.uid,                 # pu-raccoon-test
            "token": state.token,             # current FakeState bearer
            "refresh_token": state.refresh_token,  # refresh-1
        },
        fh,
    )
print(base, flush=True)

# start_server() runs the HTTP server on a daemon thread. Keep this process in
# the foreground (it is launched with & below) so the server stays alive.
while True:
    time.sleep(1)
PY

FAKE_INFO="$TMP/fake.json" python3 "$TMP/fake_upstream.py" > "$TMP/fake.log" 2>&1 &
FAKE_PID=$!
GATEWAY_PID=""
trap 'kill "$GATEWAY_PID" "$FAKE_PID" 2>/dev/null || true' EXIT
for _ in $(seq 1 100); do
  [ -s "$TMP/fake.json" ] && break
  sleep 0.1
done
test -s "$TMP/fake.json"
BASE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["base"])' "$TMP/fake.json")
printf 'fake upstream: %s\n' "$BASE"
```

The base URL is read from the environment when `wb_raccoon` is imported; no
source change is needed for it. The fake server's credential pair is whatever
`FakeState` generated (`pu-raccoon-test` / `refresh-1` initially).

If a local fake validates an identity value that v1 does not already send,
change only the copied constants (`wb_raccoon.CLIENT_PLATFORM`,
`CLIENT_VERSION`, `CLIENT_LANGUAGE`). Keep the production values for parity:
`desktop-linux-x64`, `v1.1.5`, `zh`. Never edit `/root/opencode/mix2api`.

## 3. Seed accounts with the fake's credentials

```bash
mkdir -p "$TMP/accounts/raccoon"
python3 - "$TMP/fake.json" "$TMP/accounts" <<'PY'
import json
import os
import sys
import time

fake_path, accounts = sys.argv[1], sys.argv[2]
fake = json.load(open(fake_path, encoding="utf-8"))
uid = fake["uid"]
folder = os.path.join(accounts, "raccoon")
os.makedirs(folder, exist_ok=True)

with open(os.path.join(accounts, "settings.json"), "w", encoding="utf-8") as fh:
    json.dump(
        {
            "raccoon": {"enabled": True, "max_concurrent": 1},
            "auth_disabled": True,
        },
        fh,
    )

payload = {
    "uid": uid,
    "access_token": fake["token"],
    "refresh_token": fake["refresh_token"],
    "office_identity": "personal",
    "office_org_name": "Raccoon Test",
    "expires_at": int(time.time()) + 3600,
    "token_type": "Bearer",
}
path = os.path.join(folder, "raccoon-%s.json" % uid)
with open(path, "w", encoding="utf-8") as fh:
    json.dump(payload, fh)
print(path)
PY
```

`auth_disabled` is only for this disposable oracle run so `curl` can reach the
routes; it is not a v2 default and is not copied into production settings.

## 4. Run v1 and record the responses

```bash
ACCOUNTS_DIR="$TMP/accounts" \
WB_RACCOON_BASE_URL="$BASE" \
python3 tests/oracle/v1.6.5/app/wb_proxy.py --port 18765 > "$TMP/gateway.log" 2>&1 &
GATEWAY_PID=$!

for _ in $(seq 1 100); do
  curl -fsS 'http://127.0.0.1:18765/health' >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS 'http://127.0.0.1:18765/health' >/dev/null

mkdir -p "$TMP/recorded"
curl -fsS 'http://127.0.0.1:18765/v1/models?realm=raccoon' \
  -o "$TMP/recorded/models.v1.json"
# Record health after the models fetch: v1 reports catalog_source=raccoon and
# the live model count only once the account catalog has been adopted.
curl -fsS 'http://127.0.0.1:18765/health?force=1' \
  -o "$TMP/recorded/health.v1.json"
curl -fsS -N -H 'Content-Type: application/json' \
  --data '{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hello"}],"stream":true}' \
  'http://127.0.0.1:18765/v1/chat/completions?realm=raccoon' \
  -o "$TMP/recorded/chat_stream.v1.sse"
curl -fsS -H 'Content-Type: application/json' \
  --data '{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hello"}],"stream":false}' \
  'http://127.0.0.1:18765/v1/chat/completions?realm=raccoon' \
  -o "$TMP/recorded/chat_nonstream.v1.json"
printf 'recorded under %s\n' "$TMP/recorded"
```

Both chat URLs must carry `?realm=raccoon`; otherwise v1 falls back to the
global realm (`intl` by default) instead of the Raccoon branch. The fake's
current default stream is the two-text-delta stream (`你`, `好`), terminator
`stop`, usage `3/2/5`, and `[DONE]`. Recheck `_fake_raccoon._chat_chunks` before
updating goldens if that fixture changes.

Inspect the captured payloads with `python3 -m json.tool
"$TMP/recorded/models.v1.json"` and `head -n 20
"$TMP/recorded/chat_stream.v1.sse"`. If the gateway fails to start, read
`"$TMP/gateway.log"`.

## 5. Normalize dynamic values before comparing

`tests/contract/normalize.go` owns the canonical rules; do not hand-edit ids or
timestamps into a golden unless they are deliberately stable fixture values.

- Remove transport metadata: `Date`, `Server`, request/session/trace IDs.
- Keep timestamp-shaped keys but replace their values with `<timestamp>`.
- Replace generated `chatcmpl-*`, `resp_*`, `req_*`, `trace-*`, `session-*`,
  `call_*` IDs with `<dynamic>` only when the key is an identifier and the
  value matches a prefix.
- Replace UUIDs only in known dynamic ID/UUID contexts (for example `id` on a
  generated chat object, `uuid`, `trace_uuid`, request/session/trace IDs).
  UUID-shaped model IDs and account IDs are semantic and must survive, as must
  arbitrary UUID-shaped non-ID content.
- Preserve `json.Number` precision, booleans, and nested structure.
- For SSE, ignore chunk/frame boundaries, comments, and keepalives; merge
  role/content/reasoning/tool-call fragments/finish/usage and record `[DONE]`.
  The parser covers the fields the contract needs (`data`, `event`, comments,
  CRLF/CR, multi-line data); `id`/`retry`/unknown fields are ignored.

## 6. Scoped goldens (not full v1 parity)

v2 foundation deliberately implements a smaller surface than the v1 panel.
Every JSON golden contains **all stable fields** for its scoped payload after
normalization; `assertGoldenJSON` runs `NormalizeJSON` on both the recorded
golden and the actual API response and compares the complete normalized maps.
An extra or missing stable field fails. There is no key projection.

- **Health** (`testdata/health.golden.json`): all v2 health fields: top-level
  `ok` and `version`, plus `providers.raccoon` with `ok`, `configured`,
  `accounts`, `healthy`, `base_url`, `catalog_source`, and `models`. v1's
  top-level `realm`, `crypto_ok`, per-realm/key readiness, media, and panel
  blocks are not v2 foundation fields and are not fabricated into the golden.
  `base_url` is recorded as `<upstream>` because the fake port changes; the test
  substitutes the actual fake URL before normalization. The golden is recorded
  after a models fetch, so it reflects the live catalog (`catalog_source:
  raccoon`, two models).
- **Models** (`testdata/models.golden.json`): the complete scoped envelope and
  entries, including `object`, `realm`, every model ID, all v1 capability
  spellings/counts, and `created: "<timestamp>"`. v2 does not emit v1's optional
  `catalog` provenance block, so it is out of scope.
- **Chat SSE** (`testdata/chat_stream.golden.json`): normalized role, merged
  content, finish reason, usage, and the `[DONE]` marker. Chunk ids and
  timestamps do not enter the semantic event.
- **Chat non-stream** (`testdata/chat_nonstream.golden.json`): the complete
  scoped completion: `id: "<dynamic>"`, `object`, `created: "<timestamp>"`,
  `model`, choice message content/role, finish reason, and usage.

Run the recorded contract suite with:

```bash
go test ./tests/contract -run TestRaccoonGoldenHealthModelsChat -count=1
```

Any new golden must come from the fake upstream (no real network), and the
v1 tree must remain untouched. Before committing, verify the copy is still
ignored and no original file changed:

```bash
git -C /root/opencode/mix2api status --short
git status --short
```