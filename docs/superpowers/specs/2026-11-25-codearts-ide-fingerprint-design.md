# CodeArts IDE Request Fingerprint Alignment Design

Date: 2026-11-25
Status: application + TLS parity verified against real Windows captures

## 1. Intent

Make the international CodeArts provider's chat request match the request
actually emitted by CodeArts Agent 26.11.300 on Windows: same header names,
values, signed-header subset, body fields, and plaintext fallback behavior.
Mainland CodeArts keeps its existing behavior until a mainland capture exists.

## 2. Evidence

A real chat request was captured with mitmproxy from the running Windows
CodeArts Agent (HTTP/1.1, `POST /api/v2/chat/completions`). The captured wire
headers were:

```text
Authorization
Content-Type: application/json
Host: snap-access.ap-southeast-1.myhuaweicloud.com
User-Agent: ai-sdk/provider-utils/4.0.21 runtime/bun/1.4.4
client-ip: 10.0.1.145
created-time: 2026-11-25T16:47:05Z
ide-name: VSCode
ide-version: 26.11.300
model-id: glm-5.3
model-name: GLM-5.3
plugin-name: codemate_vscode
plugin-version: 26.11.300
user-msg-id: msg_...
user-session-id: ses_...
x-language: en-us
x-ot-client-type: IDE
x-ot-client-version: 26.11.300
x-ot-function: agent-general
x-ot-parent-session-id: (empty)
x-ot-session-id: ses_...
x-ot-span-id: ...
x-ot-trace-id: ...
x-sdk-date: ...
x-security-token: <STS token>
x-snap-traceid: <trace>_<span>
Connection: keep-alive
Accept: */*
Accept-Encoding: gzip, deflate, br, zstd
Content-Length: ...
```

The real `Authorization` header signs exactly this subset:

```text
client-ip;created-time;host;ide-name;ide-version;model-id;model-name;
plugin-name;plugin-version;user-msg-id;user-session-id;x-language;
x-ot-client-type;x-ot-client-version;x-ot-function;x-ot-parent-session-id;
x-ot-session-id;x-ot-span-id;x-ot-trace-id;x-sdk-date;x-security-token;
x-snap-traceid
```

It does **not** sign `Content-Type`, `User-Agent`, `Accept`,
`Accept-Encoding`, `Connection`, or `Content-Length`, and it does **not**
send `X-Sdk-Content-Sha256`. The canonical payload hash is `sha256(body)`,
and the canonical URI has a trailing slash.

The captured chat body keys were:

```text
max_tokens, messages, model, stream, tool_stream, user_prompt
```

plus `tools`, `tool_choice`, and `reasoning_effort` when the request uses
tools. The body does **not** contain `chat_id` or `prompt_cache_key`.

A transparent CONNECT tunnel captured the real TLS handshake to
`snap-access.ap-southeast-1.myhuaweicloud.com` (no MITM certificate). It is
TLS 1.3 with no ALPN, so the actual HTTP protocol is HTTP/1.1. The ClientHello
has the same 18 cipher suites, groups, signature algorithms, and 10 extensions
as the WorkBuddy Node profile, plus a trailing padding extension (21, 230
bytes). The captured JA3 is:

```text
771,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-13-51-45-43-21,29-23-24,0
```

with MD5 `51bddd625044f75a235ba857ac8b0145`. The server selected TLS 1.3 and
no ALPN; no HTTP/2 settings or pseudo-header fingerprint applies here.

The Bun kernel contains an SM4-GCM encryption path, but the live key exchange
returns HTTP 400 and the client falls back to plaintext. The real captured body
is plaintext, so no SM4 implementation is needed for this phase.

## 3. Goals

1. International chat requests use the exact captured header names and values.
2. The signed subset matches the captured `SignedHeaders`; non-signed transport
   headers are added after signing.
3. The body omits gateway-only `chat_id`/`prompt_cache_key` and includes
   `user_prompt`.
4. Session/message/trace IDs use the observed prefixes and shapes.
5. Mainland chat behavior remains on the legacy path.
6. Tests pin the exact header set, signed subset, body fields, and absence of
   non-observed headers.
7. International HTTPS requests use a CodeArts-specific uTLS ClientHello with
   the captured JA3 and no ALPN.

## 4. Non-goals

- SM4-GCM encryption/key exchange. The real client currently fails that path
  and falls back to plaintext.
- HTTP/2 fingerprinting: the real client offers no ALPN and uses HTTP/1.1.
- Byte-for-byte header ordering. Go's `http.Transport` writes a canonical
  order; the captured order is listed above for future custom-writer work.
- Copying the Windows client's actual session/message/trace IDs. Those are
  runtime identities; the gateway derives its own values with the same shape.

## 5. Implementation

### 5.1 International header set

`newChatRequest` branches on `Station.Region == "intl"`. Before signing it sets
only the signed headers:

```text
client-ip, created-time, ide-name, ide-version, model-id, model-name,
plugin-name, plugin-version, user-msg-id, user-session-id, x-language,
x-ot-client-type, x-ot-client-version, x-ot-function, x-ot-parent-session-id,
x-ot-session-id, x-ot-span-id, x-ot-trace-id, x-snap-traceid
```

`SignRequestIDEFingerprint` adds `Host`, `x-sdk-date`, and `x-security-token`,
computes the canonical request with `sha256(body)` and a trailing-slash URI,
and emits the Authorization header. It does not add
`X-Sdk-Content-Sha256`.

After signing, the transport headers are added:

```text
Content-Type: application/json
User-Agent: ai-sdk/provider-utils/4.0.21 runtime/bun/1.4.4
Accept: */*
Accept-Encoding: gzip, deflate, br, zstd
Connection: keep-alive
```

No `X-Encrypt-Capable`, `x-session-affinity`, `x-message-id`, `app-id`,
`is_confidential`, `x-auth-token`, `maas_type`, `Chat-Id`, or `Session-Id`
is sent for international requests.

### 5.2 Body

`buildChatBody` adds `user_prompt` from the last user message. Before an
international request is marshaled, `chat_id` and `prompt_cache_key` are
removed. Mainland keeps those fields and its existing headers.

### 5.3 Runtime IDs

`newIDEFingerprint` derives:

- `ses_` + 26 base62 chars from a SHA-256 of `UID|chatID`, stable per
  conversation/account;
- `msg_` + 26 base62 chars from a SHA-256 of `chatID|last user text`, stable per
  message;
- a 32-hex trace and 16-hex span per request;
- UTC RFC3339 `created-time`;
- `model-name` display map (`glm-5.3` -> `GLM-5.3`, etc.);
- `client-ip` from `WB_CODEARTS_CLIENT_IP` or local IPv4.

The deployment can set `WB_CODEARTS_CLIENT_IP=10.0.1.145` to reproduce the
Windows client's captured value.

### 5.4 TLS profile

The CodeArts provider initializes a separate process-wide uTLS bridge
(`utlsbridge.CodeArtsDefault`) for the international station. Its ClientHello
is the WorkBuddy Node profile plus `UtlsPaddingExtension{PaddingLen: 230,
WillPad: true}`. The bridge creates a fresh `ClientHelloSpec` per connection
because uTLS mutates extension state. The profile advertises no ALPN, so the
HTTP transport stays on HTTP/1.1 (`ForceAttemptHTTP2=false`).

### 5.5 Version identity

Defaults are the captured Windows client values:

```text
WB_CODEARTS_IDE_USER_AGENT    ai-sdk/provider-utils/4.0.21 runtime/bun/1.4.4
WB_CODEARTS_IDE_CLIENT_VERSION 26.11.300
WB_CODEARTS_IDE_PLUGIN_NAME    codemate_vscode
WB_CODEARTS_IDE_PLUGIN_VERSION 26.11.300
WB_CODEARTS_IDE_NAME           VSCode
WB_CODEARTS_IDE_IDE_VERSION    26.11.300
WB_CODEARTS_IDE_FUNCTION       agent-general
```

## 6. Verification

- Unit tests assert the exact header set and values, the exact signed subset,
  absent non-observed headers, body field removal, and `user_prompt`.
- A real international chat was tested after the correction:
  - non-stream `codearts-intl/GLM-5.2`: HTTP 200, content `OK`;
  - stream `codearts-intl/glm-5.3`: HTTP 200 SSE, content `OK`;
  - `/codearts-intl/overview`: 5M token package still present.
- The live test used `WB_CODEARTS_CLIENT_IP=10.0.1.145` to match the Windows
  capture, and the upstream accepted the request.
- The uTLS profile test asserts the captured JA3, MD5, extension order, 32-byte
  session ID, and absence of ALPN.
- Repeated live requests through the uTLS bridge succeeded: two non-stream
  `GLM-5.2` requests and one streamed `glm-5.3` request returned `OK`.

## 7. Remaining gap

Header order is still Go's canonical transport order rather than the captured
order. Exact order requires a custom HTTP/1.1 writer; it is not attempted here.
The SM4 path is not used because the real client's key exchange currently fails
and falls back to plaintext.