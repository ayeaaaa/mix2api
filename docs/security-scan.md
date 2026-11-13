# Dependency vulnerability scan — 2026-11-12

Command:

```sh
GOTOOLCHAIN=go1.26.11 govulncheck -show verbose ./...
```

Tool: `govulncheck` v1.9.0, scanning the Go 1.26.11 standard library.

## Result

- **No vulnerabilities found.**
- Symbol results: 0 vulnerabilities affecting the code.
- Package results: 0 vulnerabilities in imported packages.
- Module results: 0 vulnerable required modules.

The uTLS bridge uses `github.com/refraction-networking/utls@v1.8.2` with
`brotli@v1.2.7`, `klauspost/compress@v1.20.4`, `x/net@v0.62.0`,
`x/crypto@v0.59.0`, and `x/sys@v0.50.0`. These versions require the Go 1.26
toolchain, which is now the project build baseline.

Production build environment for the candidate: `go1.26.11 linux/amd64`,
`CGO_ENABLED=0`. Cross-builds for Linux amd64/arm64/armv7, Windows amd64, and
macOS amd64/arm64 pass.
