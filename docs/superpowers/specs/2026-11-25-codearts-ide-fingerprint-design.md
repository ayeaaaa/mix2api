# CodeArts IDE Request Fingerprint Alignment Design

Date: 2026-11-25
Status: approved by continuation; application-layer phase first

## 1. Intent

Make CodeArts provider chat requests look like the requests produced by the
installed CodeArts Agent 26.11.300 for the same account, without changing the
OpenAI-compatible API exposed by mix2api.

This is a compatibility/fingerprint change, not a new chat feature. The first
phase aligns the application-layer request: headers, trace/session/message
identifiers, client metadata, model display fields, and the advertised
encryption capability. It does not change the response API.

## 2. Evidence

Observed from the installed Windows client's live kernel log
(`kernel-codeartsdoer-incognito-2026-11-25T132732-24168-0.log`) and the extracted
Bun kernel (`/tmp/opencode/agentkernel-extracted/files/chunk-r3p54r0w.js`):

- The real IDE request to `/api/v2/chat/completions` carries:
  - `x-ot-trace-id`, `x-ot-span-id`, `x-snap-traceid` (`trace_span`)
  - `x-ot-session-id`, `x-ot-parent-session-id`, `user-session-id`
  - `x-session-affinity`
  - `x-ot-function: agent-general`
  - `X-Language: en-us`
  - `user-msg-id`, `x-message-id`, `created-time`
  - `model-id`, `model-name`
  - `x-ot-client-type: IDE`, `x-ot-client-version: 26.11.300`
  - `plugin-name: codemate_vscode`, `plugin-version: 26.11.300`
  - `ide-name: VSCode`, `ide-version: 26.11.300`
  - `client-ip`
  - `X-Encrypt-Capable: sm4-gcm`
  - `User-Agent: opencode/126.11.3-20261124194420`
  - `app-id: CodeAgent3.0`, `is_confidential: false`
- The real IDE session IDs are `ses_...`; message IDs are `msg_...`.
- The real body summary includes `model`, `max_tokens`, `tool_stream`,
  `user_prompt`, `messageCount`, and `tools`; normal UI chat has `tools: []`.
- The Bun kernel has an SM4-GCM request/response encryption path
  (`/api/v1/security/key-exchange`, `X-Encrypt-*`, encrypted SSE), but the live
  key exchange currently returns HTTP 400 and the client falls back to plaintext.
  The observed live request is therefore plaintext with
  `X-Encrypt-Capable: sm4-gcm`.

Current mix2api gaps:

- It sends a smaller header set and no `User-Agent`, no `x-session-affinity`, no
  `x-ot-function`, no model metadata, no `X-Encrypt-Capable`, and its
  `x-snap-traceid` is a bare trace ID rather than `trace_span`.
- It adds `Chat-Id`/`Session-Id` headers not present in the observed IDE request.
- It uses a generic `Go-http-client` User-Agent.
- It does not add `user_prompt` to the body.
- It still uses the legacy `codearts-ot-session` hash instead of a
  `ses_...`-shaped session identifier.

## 3. Goals

1. Add the observed IDE headers with stable, deterministic IDs derived from the
   gateway's existing conversation identity.
2. Set the CodeArts Agent plugin/IDE/client metadata and User-Agent.
3. Add `X-Encrypt-Capable: sm4-gcm` while keeping plaintext, matching the
   observed live fallback behavior.
4. Add `user_prompt` to the upstream body when it can be derived.
5. Preserve the existing account selection, token refresh, signature, stream
   normalization, and error classification.
6. Add focused tests for header presence/values, ID shape/stability, model
   metadata, and `user_prompt`.

## 4. Non-goals

- SM4-GCM key exchange/encryption. The live upstream currently rejects key
  exchange; adding encryption would add complexity without matching an observed
  successful request. The capability header remains.
- TLS/HTTP2/uTLS fingerprint parity. This needs a live ClientHello/ALPN capture
  of the CodeArts Agent Bun client; the existing uTLS profile is WorkBuddy-specific.
- Exact HTTP/1.1 header ordering. Go's transport writes a canonical order; a
  custom writer is a separate phase.
- Exact `SignedHeaders` parity. Fingerprint headers will be set before signing;
  the signature remains valid and the upstream currently accepts extra signed
  headers. A later phase can split signed vs post-sign headers after a live
  SignedHeaders capture.
- Mainland-only behavior changes beyond the shared fingerprint headers.

## 5. Approaches Considered

### A. Application header/metadata parity (chosen)

Add the observed header set and deterministic IDs, keep the body plaintext, and
add the capability header. It matches the observed live request and is fully
testable with a fake upstream. Cost: does not yet cover TLS or SM4; header order
and SignedHeaders are not exact.

### B. Full application parity including SM4

Implement ECDH key exchange, SM4-GCM encryption, and encrypted SSE decoding.
This is the only way to match a future successful encrypted request. Cost: new
SM4 dependency or hand-written cipher, more failure modes, and the current
upstream rejects the key exchange with 400, so the live result would still be
plaintext fallback. Rejected for this phase.

### C. Network-layer fingerprint first

Capture the Bun ClientHello/ALPN and add a CodeArts-specific uTLS profile. This
may matter to an APIG/WAF fingerprint check, but it is independent of the
application metadata and requires a live capture. Deferred to the next phase.

## 6. Design

### 6.1 Fingerprint helper

Create `internal/providers/codearts/fingerprint.go` with:

```go
type ideFingerprint struct {
    TraceID      string
    SpanID       string
    SessionID    string
    MessageID    string
    CreatedAt    string
    ModelID      string
    ModelName    string
    ClientIP     string
    UserAgent    string
}
```

`newIDEFingerprint(account *Account, model, chatID string, messages []any) ideFingerprint`
derives:

- trace: 32 hex chars from `crypto/rand`
- span: 16 hex chars from `crypto/rand`
- session: `ses_` + a base62 encoding of a SHA-256 of
  `account UID + chatID`; stable for the same conversation/account
- message: `msg_` + base62 of a SHA-256 of `chatID + last user message content`;
  stable for the same request
- created-time: current UTC in RFC3339 (`2006-01-02T15:04:05Z`)
- model name: known display map (`glm-5.3` -> `GLM-5.3`, `GLM-5.2` ->
  `GLM-5.2`, `GLM-5.1` -> `GLM-5.1`, `Qwen3-VL-235B` -> `Qwen3-VL-235B`);
  otherwise the account catalogue `name`, then the model ID
- client IP: `localIPv4()`; allow `WB_CODEARTS_CLIENT_IP` override
- user agent: `WB_CODEARTS_IDE_USER_AGENT`, default
  `opencode/126.11.3-20261124194420`

### 6.2 Header set

In `newChatRequest`, after setting Content-Type and before `SignRequest`, set:

```text
User-Agent: opencode/126.11.3-20261124194420
X-Encrypt-Capable: sm4-gcm
x-session-affinity: <ses_...>
x-ot-trace-id: <trace>
x-ot-span-id: <span>
x-snap-traceid: <trace>_<span>
x-ot-session-id: <ses_...>
x-ot-parent-session-id: (empty)
user-session-id: <ses_...>
x-ot-function: agent-general
X-Language: <station locale>
user-msg-id: <msg_...>
x-message-id: <msg_...>
created-time: <RFC3339>
model-id: <canonical model id>
model-name: <display name>
x-ot-client-type: IDE
x-ot-client-version: 26.11.300
plugin-name: codemate_vscode
plugin-version: 26.11.300
ide-name: VSCode
ide-version: 26.11.300
client-ip: <local IPv4 or empty>
app-id: CodeAgent3.0
is_confidential: false
x-auth-token: <mainland CloudDragonToken, if present>
maas_type: <mainland benefit only>
```

Remove the extra `Chat-Id` and `Session-Id` headers. Keep the body's
`chat_id`/`prompt_cache_key` for now to preserve the existing conversation cache
behavior; the real IDE body is not conclusively shown to omit them. Add
`user_prompt` to the body from the last user message.

Headers are set before signing. The fake-upstream tests will assert that the
Authorization SignedHeaders includes the new headers and that no secret is
leaked. A live smoke will confirm the upstream accepts the larger signed set.

### 6.3 Constants and overrides

Use package-level defaults with environment overrides:

```text
WB_CODEARTS_IDE_USER_AGENT       default opencode/126.11.3-20261124194420
WB_CODEARTS_IDE_CLIENT_VERSION   default 26.11.300
WB_CODEARTS_IDE_PLUGIN_NAME      default codemate_vscode
WB_CODEARTS_IDE_IDE_NAME         default VSCode
WB_CODEARTS_CLIENT_IP            default local IPv4
```

These are not panel settings in this phase; they are deployment-level identity
values.

### 6.4 Scope of application

Apply the fingerprint headers to both CodeArts stations. The values come from
the international client evidence; mainland requests will use the same plugin
identity. If the mainland upstream rejects a value, the environment overrides
allow a station-specific deployment without code changes. The existing
`maas_type` and `x-auth-token` behavior remains station/account gated.

## 7. Tests

- `TestIDEFingerprintHeaderSet`: fake upstream captures the chat request; assert
  all header names/values, model display name, and Session/Message ID shapes.
- `TestIDEFingerprintStableSessionAndMessage`: two requests with the same
  account/chat/message produce the same `ses_`/`msg_` IDs; a different message
  changes `msg_` but not `ses_`.
- `TestIDEFingerprintSignedHeaders`: Authorization SignedHeaders includes the
  fingerprint headers and excludes the existing `Chat-Id`/`Session-Id`.
- `TestChatBodyUserPrompt`: body includes the last user text; unknown input
  fields are still stripped.
- `TestMainlandBenefitHeadersRemainGated`: mainland benefit still sends
  `maas_type`; intl does not; mainland sends `x-auth-token`, intl does not.
- Existing chat tests updated for the new header set; no credential markers in
  captured headers/body.

## 8. Rollout and Verification

1. Run focused codearts tests and the full suite.
2. Use the saved international session to run a live chat against the real
   upstream and verify 200, SSE content, and 5M quota consumption.
3. Confirm no regression in model list, account overview, or token refresh.
4. Record the remaining network-layer gap (TLS/HTTP2/header order/SM4) in docs.

### 8.1 Live result (2026-11-25)

The fingerprint build was run against the real international upstream with a
freshly exported session:

- non-stream `codearts-intl/GLM-5.2` returned HTTP 200, content `OK`, usage
  `100` total tokens.
- stream `codearts-intl/glm-5.3` returned HTTP 200 `text/event-stream`, content
  `OK`, reasoning content present.
- `/codearts-intl/overview` still reported `online: true`, the 5M token package,
  and `used: 41612`.
- The upstream accepted the larger signed header set, the IDE User-Agent, and
  `X-Encrypt-Capable: sm4-gcm` with a plaintext body.

The live run did not need a new login. A previously saved session had a
consumed refresh token; it was replaced with a fresh export from the still
logged-in CodeArts Agent before the successful run.

## 9. Risks

- The upstream may use `SignedHeaders` for a stricter check; live smoke decides.
- IDE version constants become stale after a client update; env overrides handle
  it without a code change.
- `client-ip` may be ignored or validated differently; it is a telemetry header,
  not an auth input.
- `X-Encrypt-Capable` advertises SM4 while the gateway sends plaintext, matching
  the currently observed fallback; if the upstream starts requiring encryption,
  this phase will not work and phase B becomes necessary.
- Header order is not exact; phase C remains open.