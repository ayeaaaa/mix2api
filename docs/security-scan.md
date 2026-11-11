# Dependency vulnerability scan — 2026-11-11

Command:

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest -show verbose ./...
```

Tool: `govulncheck` v1.9.0 (the scanner auto-selected Go 1.26.11; production
builds remain Go 1.22).

## Result

- **No reachable vulnerabilities found.**
- Symbol results: 0 vulnerabilities affecting the code.
- Package results: 0 vulnerabilities in imported packages.
- Module results: 1 vulnerability in a required module that is not called:
  - `GO-2026-5024`, `golang.org/x/sys@v0.28.0`
  - fixed in `golang.org/x/sys@v0.44.0`
  - `govulncheck`: code is affected by 0 vulnerabilities; this module
    vulnerability is not reachable.

## Disposition

The required-module finding is not reachable from mix2api code. It is recorded
as a residual dependency item for the security signoff; no RC behavior change
was made solely to silence an unreachable finding.

Production build environment for the candidate: `go1.22.2 linux/amd64`,
`CGO_ENABLED=0`.
