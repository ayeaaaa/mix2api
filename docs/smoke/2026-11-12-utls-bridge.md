# uTLS bridge verification — 2026-11-12

Branch: `feat/utls-bridge`

Commits:

- `2b03cd0` — `feat(workbuddy): add optional uTLS fingerprint bridge`
- `2e58634` — `build: require Go 1.26 and uTLS 1.8.2`

## Automated tests

```sh
GOTOOLCHAIN=go1.26.11 make check
GOTOOLCHAIN=go1.26.11 make race
```

Both passed. The new package tests cover:

- exact JA3 `71dc8c533dd919ae9f4963224a4ba8fd`
- exact JA4 `t13d0a1200_5d04281c6031_b76c5ee2507d`
- ClientHello length 255, session id length 32, no ALPN
- unauthenticated CONNECT returns 403
- direct HTTPS, HTTP proxy, HTTPS proxy, and SOCKS5 proxy
- HTTP targets keep the normal proxy path
- unsupported proxy schemes fail without fallback

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

## Release matrix

```sh
GOTOOLCHAIN=go1.26.11 make dist VERSION=2.1.2-utls-test
cd dist && sha256sum -c SHA256SUMS
```

The tool wrote 14 assets (11 archives plus `SHA256SUMS`, `release.json`, and
`sbom.spdx.json`); all checksum entries passed. The SBOM contains
`refraction-networking/utls`.

## Container smoke

Built `mix2api:utls-test` from the working tree with
`golang:1.26-alpine`, then ran it with `MIX2API_TLS_FINGERPRINT=node` and an
empty data directory. `/health` returned `ok:true` and
`version:"2.1.1-utls-test"`; the process logged four providers and no bridge
startup error.

## Scope

This evidence covers the transport/fingerprint path. It does not prove a real
WorkBuddy chat with a production account; that still requires a dedicated
account smoke. The feature is opt-in and defaults to `python` (off).
