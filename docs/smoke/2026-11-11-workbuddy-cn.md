# Real-account smoke — WorkBuddy CN — 2026-11-11

Scope: one dedicated WorkBuddy CN OAuth account copied to a temporary v2 accounts directory. The source v1.6.5 data directory was never opened by v2 and was not modified. No check-in, reward, task, media, or other mutable operation was run.

## Environment

- Binary: `/tmp/opencode/mix2api-v2-smoke` built from `2.0.0-rc.1` source
- Temp `ACCOUNTS_DIR` / `USAGE_DIR` under `/tmp/opencode/wb-real-smoke`
- CN account: OAuth, `product=cli`, enabled, direct egress; identity/token redacted from this record
- `PANEL_PASSWORD`/`MIX2API_API_KEY`: test-only values
- `WB_ENABLE_ANTHROPIC_MESSAGES=1` for the Messages leg

## Results

- `GET /health`: 200; CN provider `ok`, `healthy=1`, `accounts=1`
- `GET /v1/models?realm=cn`: 200; v1 CN catalogue IDs and metadata returned
- Non-stream Chat `POST /v1/chat/completions` (`hy4-preview-f`): 200; content `连接正常`, `finish_reason=stop`, usage `prompt=60`, `completion=39`, `total=99`
- Streaming Chat: 200 SSE; full frames, finish/usage before `[DONE]`; content `正常`; `[DONE]` present
- Responses `POST /v1/responses`: 200; `status=completed`, `output_text=连接正常`, reasoning + message output items, usage `input=60`, `output=57`, `total=117`
- Anthropic Messages `POST /v1/messages`: 200; type `message`, thinking + text content, text `连接正常`, `stop_reason=end_turn`, usage `input=60`, `output=240`
- Usage ledger: 5 real rows written to `usage.jsonl`, with realm/account/model/requested_model/outcome/tokens/credit/finish timing. `GET /usage/recent?realm=cn` returned all 5 rows.
- `GET /usage?realm=cn`: 5 requests.
- No access/refresh tokens appear in the recorded outputs.

## Observations

- A bare `/usage` request in this temporary setup defaulted to the empty intl realm, so it reported 0; `realm=cn` or `realm=all` returned the rows. This is a default-realm/configuration observation, not a lost-write: the JSONL and realm-filtered APIs contain all 5 rows.
- Logs returned empty because this temporary configuration did not enable/initialize a log SQLite store; chat/usage behavior was unaffected.
- The free CN model still reported a small `usage.credit` value (0.02–0.08) while `free=true`; this matches upstream quota reporting seen in the captured rows and is worth checking against v1 before final parity signoff.

## Gate F status

This closes the **WorkBuddy CN real-account chat/Responses/Messages/usage** smoke only. Gate F still requires the selected v1.6.5 oracle recordings, dedicated WorkBuddy intl/Huawei/Raccoon smoke, real update/service tests, CI dry-run, and multi-arch container validation.
