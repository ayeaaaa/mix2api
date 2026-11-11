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
| Multi-arch image / arm64 runtime | PASS (local) | real `linux/arm64` build has AArch64 ELF, runs under QEMU, healthcheck `healthy`; multi-platform OCI index contains amd64+arm64: `docs/smoke/2026-11-11-ci-multiarch.md` |
| Compose assets + data volumes | PASS (inspect) | root/deploy compose files |
| Update install/backup/rollback + journal recovery | PASS (fake) | exact original-schema journal (no `old_path`/`existed`), per-entry persisted recovery, crash-state, initial-journal, RC→final, manifest-less rollback, and prune tests |
| Update on a real service + real archive | **PARTIAL** | Real archive install/rollback passed locally; real systemd stop/start + install/rollback passed: `docs/smoke/2026-11-11-systemd-update.md`; Windows service execution still pending |
| Migration fixture round-trip, unknown keys, partial failure | PASS | `internal/core/migrate` tests |
| Migration against real v1.6.5 data | PASS | 8,422,103 bytes / 11,456 usage rows migrated and rolled back byte-identically; source tree unchanged |
| Oracle harness normalizer/loopback provenance | PASS | `tests/oracle/harness` tests |
| Selected v1.6.5 oracle recording + v2 comparison | **PARTIAL** | Core 24 cases: 14 exact/10 documented; WorkBuddy 18 cases: 12 exact/6 documented (additive model/usage fields + log-store scope); media 13 cases: 11 exact/2 documented (additive task stats and media-usage-row fields); failure suites: Raccoon 5/5, CodeArts 5/5, WorkBuddy 5/5 exact. Fixtures under `tests/oracle/recorded/`. |
| Real-account smoke on dedicated accounts | **PARTIAL (media waived)** | Chat/stream passed on all exits; CN Responses/Messages/usage passed; mutable WorkBuddy CN checkin/growth/streak, CodeArts checkin, Raccoon credits/reward passed idempotently. Real media smoke explicitly waived by owner on 2026-11-11; fake media oracle covers success paths. |
| 200 concurrent SSE p95 first byte | PASS (local fake) | `tests/load` 3-round p95 313–328 ms, p99 324–338 ms, 0 failures, threshold 2000 ms; earlier in-process-fake run p95 162 ms |
| v1.6.5 performance baseline comparison | PASS (local fake) | `tests/load/v1-baseline.json` + `v2-baseline.json`: v1 p95 1520–1571 ms, v2 p95 313–328 ms, ratio 0.208 (target ≤1.2), 3 rounds each, 0 failures |
| Release workflow syntax/gate guards | PASS (local inspect) | YAML parses; publish and container jobs require publish + Gate F; strict no-`v` version gate; `v`-prefixed image/tag identity tested |
| Release workflow dry-run in CI | PASS (local act) | `act` workflow_dispatch verify job at `ed3ab31`: make check/race, Windows/macOS builds, 14-asset dist, checksums, inventory, artifact upload all passed; `publish=false` skipped container/publish. Not a GitHub-hosted run: `docs/smoke/2026-11-11-ci-multiarch.md` |
| v2.0.0 tag / GitHub release / GHCR push | **NOT DONE (by design)** | requires explicit user approval after signoff |

## Gate F decision

**Blocked for v2.0.0 final.** Selected fake-upstream oracle recordings are
complete, and the v1/v2 fake-upstream performance baseline meets the 1.2x p95
target (ratio 0.208; local evidence only). Real-account chat/stream and mutable
flows passed; real media smoke is waived. Real-archive install/rollback passed
on a local root and on a temporary systemd service. CI verify and multi-arch
arm64 container validation passed locally. Remaining manual work: Windows
service/hook update execution, GHCR publication + arm64 registry pull, and
final human signoff. The tree is a `2.0.0-rc.1` release candidate only.

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
