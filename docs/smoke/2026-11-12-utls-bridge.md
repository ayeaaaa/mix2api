# uTLS bridge verification — 2026-11-12

Branch: `feat/utls-bridge`

Commits:

- `2b03cd0` — `feat(workbuddy): add optional uTLS fingerprint bridge`
- `2e58634` — `build: require Go 1.26 and uTLS 1.8.2`
- `e7c26cf` — `fix(utlsbridge): address review findings`
- `cec7490` — `fix(utlsbridge): harden lifecycle and defaults`

## Automated tests

```sh
GOTOOLCHAIN=go1.26.11 make check
GOTOOLCHAIN=go1.26.11 make race
```

Both passed. The new package tests cover:

- exact JA3 `71dc8c533dd919ae9f4963224a4ba8fd`
- exact JA4 `t13d120a00_5d04281c6031_78e6aca7449b`
- TLS record length 255, session id length 32, no ALPN, exact sigalg and
  supported-version order
- unauthenticated CONNECT returns 403
- direct HTTPS, HTTP proxy (including Basic auth), HTTPS proxy, and SOCKS5
  proxy (including RFC 1929 auth)
- HTTP targets keep the normal proxy path
- keep-alive connection reuse
- certificate verification failure is reported with detail
- `Bridge.Close` returns when an upstream stalls
- unsupported proxy schemes fail without fallback
- control characters in CONNECT hosts are rejected
- repeated `Default` / `CloseDefault` calls are idempotent

## Live handshake

```sh
MIX2API_UTLS_LIVE=1 GOTOOLCHAIN=go1.26.11 \
  go test ./internal/core/transport/utlsbridge \
  -run TestLiveWorkBuddyHandshake -count=1 -v
```

Result: PASS for both `www.workbuddy.ai:443` and `www.workbuddy.cn:443`.
The test completes the uTLS handshake and verifies the upstream certificate;
it does not send a business request.

## Security scan

```sh
GOTOOLCHAIN=go1.26.11 govulncheck -show verbose ./...
```

Result: `No vulnerabilities found` (symbol, package, and module results all
zero). The scan uses Go 1.26.11 and uTLS 1.8.2.

## Cross-builds

`CGO_ENABLED=0` builds passed for:

- linux/amd64
- linux/arm64
- linux/armv7
- windows/amd64
- darwin/amd64
- darwin/arm64

## Review fixes

Independent review found and this branch fixed:

- JA4 was computed with the cipher/extension counts reversed and sigalgs
  sorted; the corrected value is `t13d120a00_5d04281c6031_78e6aca7449b`.
- `Bridge.Close` could hang on a stalled upstream; upstream connections are now
  tracked and closed, and the wait is bounded.
- Docker Compose now passes both `MIX2API_TLS_FINGERPRINT` and the
  `WB_TLS_FINGERPRINT` alias.
- Certificate/proxy failures now include sanitized detail; invalid modes fail
  startup instead of leaving a healthy-looking process.
- Transient `Accept` errors no longer stop the bridge permanently.
- `Default` can restart after `CloseDefault`; repeated tests and a future
  in-process restart no longer silently fall back to native TLS.
- Control characters in CONNECT hosts are rejected, and the half-close
  capability that the bridge did not honor was removed.
- OpenWrt UCI exposes `tls_fingerprint`.

## Release matrix

```sh
OUT=/tmp/opencode/dist-utls-final4 GOTOOLCHAIN=go1.26.11 \
  make dist VERSION=2.1.2-utls-test4
cd /tmp/opencode/dist-utls-final4 && sha256sum -c SHA256SUMS
```

The tool wrote 14 assets (11 archives plus `SHA256SUMS`, `release.json`, and
`sbom.spdx.json`); all checksum entries passed. The SBOM contains
`refraction-networking/utls`.

## Container smoke

Built `mix2api:utls-test4` from commit `cec7490` with
`golang:1.26-alpine`, then ran it with `MIX2API_TLS_FINGERPRINT=node` and an
empty data directory. `/health` returned `ok:true` and
`version:"2.1.1-utls-test"`; startup logged
`uTLS bridge: enabled on 127.0.0.1:<port>` and four providers.

The `WB_TLS_FINGERPRINT=node` alias alone also enabled the bridge. An invalid
mode (`MIX2API_TLS_FINGERPRINT=nod`) exited with code 1 and
`startup: uTLS bridge: ... must be node or python`.

## Scope

This evidence covers the transport/fingerprint path. It does not prove a real
WorkBuddy chat with a production account; that still requires a dedicated
account smoke. The feature is opt-in and defaults to `python` (off).
