# Changelog

All notable changes to this project are documented here. The format follows
Keep a Changelog and the project uses semantic versioning.

## [Unreleased]

### Added

- Dashboard top bar shows total quota beside total credits for both WorkBuddy
  realms (`可用 / 总积分 / 总额度`); the v2.0.1 asset only exposed total
  credits.

## [2.0.1] - 2026-11-11

Patch release focused on upstream protocol parity, account safety, and request
fidelity.

### Added

- Persistent `11140` account bans: API dispatch and automatic tasks stop until
  an operator manually enables the account; restart, token refresh, re-import
  and successful probes do not clear it.
- Intl `deepseek-v4.1-flash` outbound alias to `deepseek-v4.1-flash-sg`; the
  logical id remains in the catalog, API-key checks, responses and
  `requested_model`. `WB_INTL_MODEL_ALIAS=0` disables it.
- DSH flow-map `refs:` support for `.credentials.yaml`.
- CN Kimi `$ref` / `$defs` tool-schema inlining.
- Browser identity, web headers and ACP request shape aligned with the official
  web client (per-account browser profiles, `X-Request-ID`, `session/new`,
  `set_mode`, `set_config_option`, full prompt `_meta`, human pause).

### Fixed

- CodeArts model-ID casing fallback on the same account, role-only preflight
  frames, and SSE credit-exhaustion parking; manual disable survives terminal
  credential errors.
- Complete 11151 empty-text guard for list parts and sniffed string arrays.
- Billing-header cleanup is scoped to system/developer messages; tool
  descriptions are sanitized; bare billing keys and `11128` body text no longer
  get the old rewrites.
- Reasoning mirror for legacy-reasoning models and Responses replay fidelity
  (adjacent reasoning merge, inline assistant reasoning/tool calls, tool-boundary
  clearing).
- Profile-wrapper compatibility, desktop identity `5.9.4` / CLI `2.165.0`,
  dashboard `X-Robots-Tag: noindex, nofollow`.

### Verification

- `make check` passes; v1 parity suites and the v2 contract/oracle suites remain
  green.

## [2.0.0] - 2026-11-11

Final release. Source is published on branch `v2` of `ayeaaaa/mix2api`; `main`
remains v1.6.5. Owner approved the final release with the real Windows SCM
update path still unverified; see `docs/gate-f.md`.

### Fixed

- Docker buildx `linux/arm64` builds now inherit the automatic `TARGETARCH`
  instead of shadowing it with an amd64 default; the arm64 image contains an
  AArch64 binary and passes the healthcheck under QEMU.
- Release identity is unified on `ayeaaaa/mix2api` and
  `ghcr.io/ayeaaaa/mix2api`; the embedded dashboard is byte-identical to v1.6.5
  again, including its two repository links.
- Final `2.0.0` is the default build identity; compose and packaging examples
  use the `v2.0.0` tag.

### Verified

- Real systemd update install/rollback, real-archive update, local registry
  multi-arch push/pull, arm64 container runtime, and a local GitHub Actions
  `verify` dry-run.
- Windows amd64 binary and `.cmd` update hooks under Wine; real Windows SCM
  execution remains pending.
- `govulncheck`: no reachable vulnerabilities; one unreachable `x/sys` module
  finding is recorded in `docs/security-scan.md`.

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

- v1.6.5 oracle parity: `/agents` picker semantics, `global_key_set`, and Codex
  path order; `/health` v1 readiness/media/identity fields; `/v1/models`
  `catalog` provenance; non-stream chat `elapsed_ms`/`first_chunk_at`; `/updates`
  null/empty defaults; persisted active-realm usage selection; and nonempty
  request logging for `/logs`.
- Update journal now records original hashes, existence, old paths, and
  completion state before and after each rename; recovery reconstructs the
  `.mix2api-old` sibling, including the exact original schema that omitted
  `old_path`/`existed`, persists verified progress per entry, and keeps the
  journal on mismatch.
- Final `2.0.0` is offered over a running `2.0.0-rc.N`; rollback to a retained
  RC backup is supported.
- ARMv7 GOARM spellings (`7`/`v7`/`armv7`) are compared as one family during
  update extraction, not only in the release helper.
- Migration rejects symlinked destination and receipt parents, treats fresh
  empty/truncated locks as live for a grace period, and keeps dry runs free of
  destination, lock, and receipt writes.
- Migration rejects symlink/real-path overlap, reclaims stale locks, stages the
  receipt before publishing, and leaves no destination when receipt staging
  fails.
- Rollback to a manifest-less version removes the newer manifest and
  synthesizes one when the backup version is known.
- Windows `.cmd`/`.bat` update hooks run through `cmd.exe /d /s /c` with the
  canonical quoted batch command line, so hook paths containing spaces stay
  intact; `.exe` hooks run directly.
- Release identity was unified on `ayeaaaa/mix2api-v2` for this candidate. The
  final `2.0.0` release moved the source, releases, compose examples, and
  updater to `ayeaaaa/mix2api` while keeping the dashboard byte-identical to
  v1.6.5.
- The release workflow rejects `v`-prefixed, leading-zero, build-metadata, or
  otherwise non-strict version inputs before building.
- `SOURCE_DATE_EPOCH` now fixes archive entry timestamps as documented, and an
  invalid value fails the build instead of silently choosing 1980.
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
