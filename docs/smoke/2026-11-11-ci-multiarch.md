# CI dry-run and multi-arch container validation — 2026-11-11

Scope: local GitHub Actions emulation with `act` and local Docker buildx/QEMU.
No tag, GitHub release, registry login, or GHCR push was performed.

## CI dry-run

- Runner emulation: `act 0.2.97`, image `catthehacker/ubuntu:act-latest`,
  `linux/amd64` container architecture, local artifact server.
- Workflow: `.github/workflows/release.yml`, job `verify`, event
  `workflow_dispatch`.
- Inputs: `version=2.0.0-rc.1`, `commit=ed3ab310b16db0c48671be7ba5afda67e92508a7`,
  `gate_f_signed=no`, `prerelease=true`, `publish=false`.
- Result: **job succeeded**.

Passed steps:

- verify requested commit
- validate version input
- format, vet, tests (`make check`)
- race tests (`make race`)
- CGO=0 Windows build
- CGO=0 macOS amd64/arm64 builds
- build release assets (`make dist`, 14 assets)
- verify checksums (`sha256sum -c`)
- verify asset inventory
- upload artifact (`mix2api-dist.zip`, 74,996,772 bytes,
  SHA256 `7de226911e421f6fb0a6de6c2df32cb781392039dc8765b64f693a28325ac03a`)

The `container` and `publish` jobs are intentionally gated on
`publish=true` + `gate_f_signed=yes` and were skipped. This is a local
GitHub Actions emulation, not a GitHub-hosted run.

## Multi-arch container

A real arm64 build exposed a Dockerfile bug: `ARG TARGETARCH=amd64` shadowed
buildx's automatic platform argument, so the `linux/arm64` image contained an
amd64 binary. Fixed by declaring `ARG TARGETOS`/`ARG TARGETARCH` without
defaults and using `${TARGETOS:-linux}`/`${TARGETARCH:-amd64}` fallbacks.
`tests/packaging` now guards against the regression.

Validation at commit `ed3ab31`:

- `docker buildx build --platform linux/arm64` produced an ELF header with
  `e_machine=0x00b7` (AArch64).
- Container reported `uname -m` = `aarch64`, `mix2api version` =
  `go1.22.12 linux/arm64`.
- Container ran as UID/GID `10001:10001`; Docker healthcheck reached `healthy`;
  `/health` returned `ok=true`, version `2.0.0-rc.1`.
- A multi-platform OCI archive (`linux/amd64,linux/arm64`) contains an index
  with both platform entries, and the arm64 image layer's `/app/mix2api` is
  AArch64.

No image was pushed to GHCR. Registry publication remains an explicit release
step requiring approval.

## Gate F status

This closes the local CI verify dry-run and local multi-arch container
validation. Remaining: real Windows service/hook update execution, GHCR
publication/arm64 registry pull, and final human signoff.
