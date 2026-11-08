# v1.6.5 oracle for contract goldens

The v2 golden fixtures in `tests/contract/testdata` are derived from the
read-only Python v1.6.5 tree at `/root/opencode/mix2api`. That tree is the
oracle: **never modify it**, and do not commit the copied tree. This directory
is only the place to keep a local, test-only copy when recording new goldens.

## 1. Copy v1.6.5 without touching the source

Pin the tag's commit first. The v1.6.5 tag currently resolves to
`e40c4e8994f9cc6ace3457e350466cdd5d422a37`.

```bash
PIN=e40c4e8994f9cc6ace3457e350466cdd5d422a37
test "$(git -C /root/opencode/mix2api rev-parse 'v1.6.5^{}')" = "$PIN"
rm -rf tests/oracle/v1.6.5
mkdir -p tests/oracle/v1.6.5
git -C /root/opencode/mix2api archive --format=tar v1.6.5 | tar -xf - -C tests/oracle/v1.6.5
```

`tests/oracle/.gitignore` excludes `v1.6.5/` and Python caches, so the copy
cannot be committed accidentally. Do not copy the whole v1 repository into a
tracked v2 path.

## 2. Test-only base URL and identity

Start the v1 fake upstream from the copied tree:

```python
import sys
sys.path.insert(0, "tests/oracle/v1.6.5/app/tests")
from _fake_raccoon import start_server

server, base, state = start_server()
print(base)  # pass this to WB_RACCOON_BASE_URL
```

The only supported source change is a test-only patch in the **copy**. Prefer
configuration/environment first; patch only if the local fake validates an
identity value that v1 does not already send.

- Base URL: `WB_RACCOON_BASE_URL=<fake-base>` is read when `wb_raccoon` is
  imported. If running in-process, set `wb_raccoon.BASE_URL = base` before
  creating the pool.
- Identity: v1 sends `X-Client-Platform: desktop-linux-x64`,
  `X-Client-Version: v1.1.5`, and `X-Raccoon-Language: zh`. Keep those values
  for parity; if a local fake needs another identity, change only the copied
  constants (`wb_raccoon.CLIENT_PLATFORM`, `CLIENT_VERSION`,
  `CLIENT_LANGUAGE`). Never edit `/root/opencode/mix2api`.

Create a throwaway accounts directory. It needs the shared settings file and
one auth document shaped like the real one:

```bash
TMP=$(mktemp -d)
mkdir -p "$TMP/accounts/raccoon"
cat > "$TMP/accounts/settings.json" <<'JSON'
{"raccoon": {"enabled": true, "max_concurrent": 1}, "auth_disabled": true}
JSON

python3 - <<PY
import json, sys, time
sys.path.insert(0, "tests/oracle/v1.6.5/app")
import wb_raccoon
from _fake_raccoon import make_jwt

uid = "pu-contract"
token = make_jwt(uid, 7200)
path = "$TMP/accounts/raccoon/raccoon-%s.json" % uid
with open(path, "w", encoding="utf-8") as fh:
    json.dump({
        "uid": uid,
        "access_token": token,
        "refresh_token": "refresh-" + uid,
        "office_identity": "personal",
        "office_org_name": "Contract Org",
        "expires_at": int(time.time()) + 7200,
        "token_type": "Bearer",
    }, fh)
print(path)
PY
```

`auth_disabled` is only for this disposable oracle run so `curl` can reach the
routes; it is not a v2 default and is not copied into production settings.

Run the v1 gateway against the fake, then record with `curl`:

```bash
ACCOUNTS_DIR="$TMP/accounts" \
WB_RACCOON_BASE_URL="$BASE" \
python3 tests/oracle/v1.6.5/app/wb_proxy.py --port 18765
```

```bash
curl -s 'http://127.0.0.1:18765/health?force=1' > /tmp/health.v1.json
curl -s 'http://127.0.0.1:18765/v1/models?realm=raccoon' > /tmp/models.v1.json
curl -sN -H 'Content-Type: application/json' \
  -d '{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hello"}],"stream":true}' \
  'http://127.0.0.1:18765/v1/chat/completions' > /tmp/chat_stream.v1.sse
curl -s -H 'Content-Type: application/json' \
  -d '{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hello"}],"stream":false}' \
  'http://127.0.0.1:18765/v1/chat/completions' > /tmp/chat_nonstream.v1.json
```

The fake's current default stream is the two-text-delta stream (`你`, `好`),
terminator `stop`, usage `3/2/5`, and `[DONE]`. Recheck the fixture before
updating goldens if the fake changes.

## 3. Normalize dynamic values before comparing

`tests/contract/normalize.go` owns the canonical rules; do not hand-edit ids or
timestamps into a golden unless they are deliberately stable fixture values.

- Remove transport metadata: `Date`, `Server`, request/session/trace IDs.
- Keep timestamp-shaped keys but replace their values with `<timestamp>`.
- Replace UUID values with `<uuid>`, except account/nickname content.
- Replace generated `chatcmpl-*`, `resp_*`, `req_*`, `trace-*`, `session-*`,
  `call_*` IDs with `<dynamic>` only when the value matches a prefix/UUID.
  Semantic model IDs such as `raccoon-090dd3` and `sn-*` must survive.
- Preserve `json.Number` precision, booleans, and nested structure.
- For SSE, ignore chunk/frame boundaries, comments, and keepalives; merge
  role/content/reasoning/tool-call fragments/finish/usage and record `[DONE]`.

## 4. Scoped goldens (not full v1 parity)

v2 foundation deliberately implements a smaller surface than the v1 panel.
Goldens assert only the stable subset below; absent v1 fields are documented
as out of scope, not fabricated.

- **Health** (`testdata/health.golden.json`): top-level `ok` and `version`, plus
  `providers.raccoon` with `ok`, `configured`, `accounts`, `healthy`,
  `catalog_source`, and `models`. v1's top-level `realm`, `crypto_ok`,
  per-realm/key readiness, media, and panel blocks are not v2 foundation
  fields. `base_url` stays out of the golden because the fake port changes; the
  test asserts it directly. The golden is recorded after a models fetch, so it
  reflects the live catalog (`catalog_source: raccoon`, two models).
- **Models** (`testdata/models.golden.json`): envelope `object/data/realm`,
  exact model IDs, and the v1 capability spellings/counts. v2 does not emit
  v1's optional `catalog` provenance block; `created` is dynamic and omitted.
- **Chat SSE** (`testdata/chat_stream.golden.json`): normalized role, merged
  content, finish reason, usage, and the `[DONE]` marker. Chunk ids and
  timestamps are ignored.
- **Chat non-stream** (`testdata/chat_nonstream.golden.json`): OpenAI
  `chat.completion` object/model, assistant message content, finish reason, and
  usage. `id`/`created` are dynamic and omitted.

Run the recorded contract suite with:

```bash
go test ./tests/contract -run TestRaccoonGoldenHealthModelsChat -count=1
```

Any new golden must come from the fake upstream (no real network), and the
v1 tree must remain untouched. Before committing, verify the copy is still
ignored:

```bash
git status --short
```