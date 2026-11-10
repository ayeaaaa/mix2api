# Changelog

All notable changes to this project are documented here. The format follows
Keep a Changelog and the project uses semantic versioning.

## [2.0.0-rc.1] - 2026-11-10

Release candidate. **Not a final v2.0.0 release.** Gate F oracle, real-account
smoke, and CI workflow dry-run are still pending; see `docs/gate-f.md`.

### Added

- Go rewrite of the v1.6.5 gateway with a single CGO-free binary and embedded
  dashboard.
- Provider modules for WorkBuddy intl/cn, CodeArts, and Raccoon, including
  account pools, proxy slots, model catalogs, check-in/activity, credits,
  media, agents, probes, usage, logs, pricing, and update discovery.
- `mix2api version [--json|--short]` with linker-injected version, commit, and
  build date.
- Reproducible release tooling (`make dist`) for Linux amd64/arm64/armv7,
  Windows amd64, macOS amd64/arm64, OpenWrt amd64/arm64/armv7, and portable
  Unix/Windows archives, with `SHA256SUMS`, SPDX SBOM, and per-archive release
  manifests.
- Multi-stage Dockerfile (CGO=0, non-root UID 10001, healthcheck), compose
  examples for build/image deployments, `.env.example`, and deployment docs.
- Safe update executor (`mix2api update run/install/rollback/recover`) with
  fixed repository validation, checksum/manifest verification, complete
  backups, atomic replacement, transaction journal recovery, and stop/start
  hooks on Unix and Windows.
- Copy-only migration (`mix2api migrate`) and v1-compatible rollback
  (`mix2api rollback`) with JSON/JSONL/SQLite validation, source snapshots,
  destination locks, checksum receipts, dry-run, and atomic publish.
- Oracle harness for selected v1.6.5 outputs plus a dedicated-account smoke
  checklist and Gate F signoff template.
- 200-concurrent SSE load harness with first-byte percentiles, memory/CPU,
  usage rows, and SQLite log append evidence.
- Manual release workflow with explicit `publish` and `gate_f_signed` guards,
  multi-arch GHCR build, checksum verification, and release asset upload.

### Fixed

- Update journal now records original hashes, existence, old paths, and
  completion state before and after each rename; recovery reconstructs the
  `.mix2api-old` sibling, verifies the restored original, and keeps the journal
  on mismatch.
- Final `2.0.0` is offered over a running `2.0.0-rc.N`; rollback to a retained
  RC backup is supported.
- Migration rejects symlink/real-path overlap, reclaims stale locks, stages the
  receipt before publishing, and leaves no destination when receipt staging
  fails.
- Rollback to a manifest-less version removes the newer manifest and
  synthesizes one when the backup version is known.
- Windows `.cmd`/`.bat` update hooks run through `cmd.exe /d /s /c`; `.exe`
  hooks run directly.
- Release identity is unified on `ayeaaaa/mix2api-v2` for GitHub releases,
  GHCR, compose, and the updater. ARMv7 GOARM is propagated and validated.
- OpenWrt packages carry the target `PKGARCH`, append env vars through procd,
  and install the UCI config as `0600`.
- `SHA256SUMS` now covers the SBOM and release metadata in addition to every
  archive.

### Changed

- v2 defaults to the `2.0.0-rc.1` build identity; release builds override it
  with `-ldflags -X`.
- Packaging uses fixed archive timestamps/owners and sorted entries; fixed
  metadata builds are byte-identical.
- OpenWrt builds clamp the 100 GiB log-size cap to the platform `int` maximum
  so 32-bit armv7 compiles.

### Data compatibility

- Existing settings, account, state, usage, logs, pricing, media, agent, and
  update files keep their v1 shapes and unknown keys.
- `usage.jsonl` appends remain v1 JSONL; `logs.sqlite3` keeps the v1 schema.
- Migration copies data only; it never rewrites the source or converts values.
- Rollback produces a fresh v1-compatible directory; v1 and v2 never share a
  live directory.

### Security

- Release assets are checksum/manifest verified before update.
- Update extraction rejects traversal, symlinks, hardlinks, devices, special
  mode bits, oversized archives, and unlisted files.
- Update requests are create-only and validated; status/backup inventory files
  are bounded and owner-checked.
- Migration refuses symlinks/special files and detects source mutation during
  the copy.
- Added `SECURITY.md` with reporting and deployment hardening guidance.

### Known gaps

- Selected v1.6.5 oracle recordings have not been captured/committed.
- Real-account smoke on dedicated accounts has not been signed.
- Real service update install and Windows update hooks have not been exercised
  outside fake tests.
- v1.6.5 performance baseline comparison has not been recorded.
- The release workflow has not been dry-run in CI.

## [1.6.5] - upstream baseline

The Python v1.6.5 gateway remains the behavioral oracle and is not modified by
this project. See the original repository for its history.
