# mix2api v2 Packaging, Parity, and Release Plan

**Goal:** Produce a reproducible `v2.0.0` release candidate with multi-platform binaries/containers, safe update/rollback, migration/rollback tooling, oracle/parity gates, performance evidence, and release documentation.

**Spec:** `docs/superpowers/specs/2026-11-08-mix2api-v2-design.md`
**Prior plans:** Foundation/Raccoon, CodeArts, WorkBuddy, Platform, Dashboard.

## Global Constraints

- Original v1.6.5 repo/data remains untouched; no live account directory sharing.
- Single CGO-free binary; dashboard embedded; pure-Go SQLite.
- Preserve old data formats; migration is copy/validate, rollback is restore/run v1.6.5.
- No v2.0.0 release without real v1.6.5 oracle recordings and real-account smoke on dedicated accounts.
- Automated tests are offline/fake; release checks run only in CI/manual.
- Version/commit/date injected with ldflags; reproducible builds; checksums/SBOM.
- Multi-arch: Linux amd64/arm64, Windows amd64, macOS amd64/arm64, OpenWrt/portable assets as v1 supported.
- Update/rollback must validate fixed repository/assets, backup completeness, atomic replacement, and permissions.

## Review Focus

1. Build/reproducibility and asset completeness.
2. Update/rollback safety and Windows/Unix portability.
3. Data migration/rollback and v1 round-trip.
4. Oracle/parity honesty and real-account smoke gates.
5. Performance/load and release provenance.

---

### Task 1: Versioning, build scripts, reproducibility

- Add ldflags-injected version/commit/date, `version.Version` default dev, `mix2api version`.
- `Makefile`/scripts build Linux/Windows/macOS/OpenWrt with `CGO_ENABLED=0`, zip/tar/checksums/SBOM.
- Tests verify version output and binary assets deterministic metadata.
- Commit `build: add reproducible release builds`.

### Task 2: Docker, compose, portable/OpenWrt assets

- Dockerfile multi-stage CGO=0, non-root, dashboard, healthcheck, multi-arch.
- docker-compose example, data volumes, env docs.
- OpenWrt/portable packages include binary, dashboard embedded, init scripts, LICENSE.
- Test inspect Dockerfile/scripts/assets references; `docker build` optional CI.
- Commit `build: add container and portable packaging`.

### Task 3: Update executor and rollback

- Implement real update check/install/rollback for Unix/Windows using validated local backup, atomic replace, service restart hook, permissions, no path traversal/symlink.
- Tests fake release/backup; interrupted install rollback; Windows cross-build.
- Commit `feat(update): add safe install and rollback executor`.

### Task 4: Migration and rollback CLI

- `mix2api migrate --from accounts --to accounts-v2` copy-only, validate settings/accounts/state/usage/logs/pricing/media/agent files with v1-compatible readers, lock, checksum, dry-run.
- `mix2api rollback` restore v1 directory and verify.
- Tests v1 fixture round-trip, unknown keys, no source mutation, partial failure rollback.
- Commit `feat(migrate): add v1-to-v2 migration and rollback`.

### Task 5: Oracle/parity and real-account smoke gates

- Script/harness records selected v1.6.5 fake-upstream outputs for health/models/chat/responses/messages/admin/media/agents/update/usage/logs and compares normalized v2.
- Document real-account smoke checklist (dedicated accounts only), Gate F signoff, known exclusions.
- Tests oracle normalizer/no-network provenance; CI cannot fetch network.
- Commit `test(parity): add v1 oracle and smoke gates`.

### Task 6: Performance/load and release candidate

- Add benchmarks/load harness: 200 concurrent SSE, p95 first byte, memory/CPU, SQLite/usage writes; compare v1 baseline if available.
- Release docs, CHANGELOG, security notes, migration/rollback runbook, GHCR/GitHub release workflow, v2.0.0 version.
- Tests workflow YAML/asset list/checksums; manual release only after Gate F.
- Commit `docs: prepare v2.0.0 release candidate`.

## Gate F

All builds/checksums/assets; update/rollback and migration round-trip; oracle parity recorded; real-account smoke signed off; performance targets met; release workflow dry-run. Only then tag/release v2.0.0.
