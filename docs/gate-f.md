# Gate F status

Gate F is the final release gate for **v2.0.0**. This document is intentionally
not a signoff: it records what is proven by automated tests and what still
requires a manual run.

| Gate | Status | Evidence |
| --- | --- | --- |
| CGO=0 Linux amd64/arm64/armv7 builds | PASS | `make dist` builds `linux-amd64`, `linux-arm64`, `linux-armv7`; GOARM is embedded and validated |
| Windows amd64 / macOS amd64+arm64 builds | PASS | release matrix + Windows cross-build |
| OpenWrt amd64/arm64/armv7 assets | PASS (inspect) | archives contain init script, UCI config, binary |
| Portable Unix/Windows launchers | PASS (inspect) | start/stop scripts in portable archives |
| SHA256SUMS + SPDX SBOM + release manifest | PASS | checksums cover all archives, SBOM, and `release.json`; `sha256sum -c` passes |
| Reproducible binary for fixed metadata | PASS | two fixed-ldflags builds are byte-identical |
| Dockerfile multi-stage CGO=0 non-root healthcheck | PASS | local `docker build` + healthcheck as UID 10001; `/app` is read-only |
| Compose assets + data volumes | PASS (inspect) | root/deploy compose files |
| Update install/backup/rollback + journal recovery | PASS (fake) | journal crash-state, initial-journal, legacy `.mix2api-old`, RC→final, manifest-less rollback, and prune tests |
| Update on a real service + real archive | **PENDING** | requires dedicated host |
| Migration fixture round-trip, unknown keys, partial failure | PASS | `internal/core/migrate` tests |
| Migration against real v1.6.5 data | PASS | 8,422,103 bytes / 11,456 usage rows migrated and rolled back byte-identically; source tree unchanged |
| Oracle harness normalizer/loopback provenance | PASS | `tests/oracle/harness` tests |
| Selected v1.6.5 oracle recording + v2 comparison | **PENDING** | no captured fixture committed |
| Real-account smoke on dedicated accounts | **PARTIAL** | Chat/stream passed on all exits: intl, CN, CodeArts, Raccoon; CN Responses/Messages/usage passed. See `docs/smoke/2026-11-11-all-exits.md` and `docs/smoke/2026-11-11-workbuddy-cn.md`. Mutable task/media/update smoke still pending |
| 200 concurrent SSE p95 first byte | PASS (local fake) | `tests/load` recorded p95 162 ms, p99 163 ms, 0 failures, threshold 2000 ms |
| v1.6.5 performance baseline comparison | **PENDING** | no captured baseline |
| Release workflow syntax/gate guards | PASS (local inspect) | YAML parses; publish and container jobs require publish + Gate F; `v`-prefixed image/tag identity tested |
| Release workflow dry-run in CI | **PENDING** | workflow added; GitHub Actions run not executed here |
| v2.0.0 tag / GitHub release / GHCR push | **NOT DONE (by design)** | requires explicit user approval after signoff |

## Gate F decision

**Blocked for v2.0.0 final.** Oracle recordings, real-account smoke, real
executor install, and a CI workflow dry-run remain manual. The tree is a
`2.0.0-rc.1` release candidate only.

## Required signoff

| Role | Name | Date | Result |
| --- | --- | --- | --- |
| Build/reproducibility | PENDING | | |
| Data migration | PENDING | | |
| Oracle/parity | PENDING | | |
| Real-account smoke | PENDING | | |
| Performance | PENDING | | |
| Security review | PENDING | | |

Do not create `v2.0.0` or push an image until every row is signed.
