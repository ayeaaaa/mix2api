# Gate F status

Gate F is the final release gate for **v2.0.x**. This document is intentionally
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
| Multi-arch image / arm64 runtime | PASS (local) | real `linux/arm64` build has AArch64 ELF, runs under QEMU, healthcheck `healthy`; multi-platform OCI index contains amd64+arm64; local registry push/pull and arm64 pull/run passed: `docs/smoke/2026-11-11-ci-multiarch.md`, `docs/smoke/2026-11-11-local-registry-multiarch.md` |
| Compose assets + data volumes | PASS (inspect) | root/deploy compose files |
| Update install/backup/rollback + journal recovery | PASS (fake) | exact original-schema journal (no `old_path`/`existed`), per-entry persisted recovery, crash-state, initial-journal, RC→final, manifest-less rollback, and prune tests |
| Update on a real service + real archive | **PARTIAL** | Real archive install/rollback passed locally; real systemd stop/start + install/rollback passed: `docs/smoke/2026-11-11-systemd-update.md`; Windows amd64 binary + `.cmd` hooks install/rollback passed under Wine: `docs/smoke/2026-11-11-windows-wine-update.md` |
| Real Windows SCM/service execution | **PENDING** | Requires a Windows host/VM; Wine only validates the binary and `.cmd` hook path |
| Migration fixture round-trip, unknown keys, partial failure | PASS | `internal/core/migrate` tests |
| Migration against real v1.6.5 data | PASS | 8,422,103 bytes / 11,456 usage rows migrated and rolled back byte-identically; source tree unchanged |
| Oracle harness normalizer/loopback provenance | PASS | `tests/oracle/harness` tests |
| Selected v1.6.5 oracle recording + v2 comparison | **PARTIAL** | Core 24 cases: 14 exact/10 documented; WorkBuddy 18 cases: 12 exact/6 documented (additive model/usage fields + log-store scope); media 13 cases: 11 exact/2 documented (additive task stats and media-usage-row fields); failure suites: Raccoon 5/5, CodeArts 5/5, WorkBuddy 5/5 exact. Fixtures under `tests/oracle/recorded/`. |
| Real-account smoke on dedicated accounts | **PARTIAL (media waived)** | Chat/stream passed on all exits; CN Responses/Messages/usage passed; mutable WorkBuddy CN checkin/growth/streak, CodeArts checkin, Raccoon credits/reward passed idempotently. Real media smoke explicitly waived by owner on 2026-11-11; fake media oracle covers success paths. |
| 200 concurrent SSE p95 first byte | PASS (local fake) | `tests/load` 3-round p95 313–328 ms, p99 324–338 ms, 0 failures, threshold 2000 ms; earlier in-process-fake run p95 162 ms |
| v1.6.5 performance baseline comparison | PASS (local fake) | `tests/load/v1-baseline.json` + `v2-baseline.json`: v1 p95 1520–1571 ms, v2 p95 313–328 ms, ratio 0.208 (target ≤1.2), 3 rounds each, 0 failures |
| Dependency vulnerability scan | PASS (no reachable) | `govulncheck -show verbose ./...`: 0 reachable code/package vulnerabilities; one unreachable module finding `GO-2026-5024` in `x/sys@v0.28.0` (fixed `v0.44.0`), recorded in `docs/security-scan.md` |
| Release workflow syntax/gate guards | PASS (local inspect) | YAML parses; publish and container jobs require publish + Gate F; strict no-`v` version gate; `v`-prefixed image/tag identity tested |
| Release workflow dry-run in CI | PASS (local act) | `act` workflow_dispatch verify job at `ed3ab31`: make check/race, Windows/macOS builds, 14-asset dist, checksums, inventory, artifact upload all passed; `publish=false` skipped container/publish. Not a GitHub-hosted run: `docs/smoke/2026-11-11-ci-multiarch.md` |
| v2.1.1 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.1.1>; tag/commit `065281a`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:12900de...`; arm64 pulled from GHCR, ran `2.1.1`; local containers healthy: `docs/smoke/2026-11-12-release-2.1.1.md` |
| v2.1.0 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.1.0>; tag/commit `f998d89`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:33826ba...`; arm64 pulled from GHCR, ran `2.1.0`; local containers healthy: `docs/smoke/2026-11-11-release-2.1.0.md` |
| v2.0.7 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.7>; tag/commit `ac01e77`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:7b54830...`; arm64 pulled from GHCR, ran `2.0.7`; local containers healthy: `docs/smoke/2026-11-11-release-2.0.7.md` |
| v2.0.7 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.7>; tag/commit `ac01e77`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:7b54830...`; arm64 pulled from GHCR, ran `2.0.7`; local containers healthy: `docs/smoke/2026-11-11-release-2.0.7.md` |
| v2.0.6 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.6>; tag/commit `f7f4904`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:5381e1e...`; arm64 pulled from GHCR, ran `2.0.6`; local containers healthy: `docs/smoke/2026-11-11-release-2.0.6.md` |
| v2.0.5 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.5>; tag/commit `0ebc996`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:2e0e9db...`; arm64 pulled from GHCR, ran `2.0.5`; local containers healthy: `docs/smoke/2026-11-11-release-2.0.5.md` |
| v2.0.4 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.4>; tag/commit `32c6334`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:7e08f6e...`; arm64 pulled from GHCR, ran `2.0.4`; local containers healthy: `docs/smoke/2026-11-11-release-2.0.4.md` |
| v2.0.3 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.3>; tag/commit `1f66ba9`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:ff8d8be...`; arm64 pulled from GHCR, ran `2.0.3`; local containers healthy: `docs/smoke/2026-11-11-release-2.0.3.md` |
| v2.0.2 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.2>; tag/commit `76e525c`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:6114327...`; arm64 pulled from GHCR, ran `2.0.2`, and `/health` passed: `docs/smoke/2026-11-11-release-2.0.2.md` |
| v2.0.1 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.1>; tag/commit `f6c500d`; 14 assets checksum-verified; GHCR index `sha256:bba08e0...`; arm64 pulled from GHCR and healthy: `docs/smoke/2026-11-11-release-2.0.1.md` |
| v2.0.0 tag / GitHub release / GHCR push | **PASS** | Release: <https://github.com/ayeaaaa/mix2api/releases/tag/v2.0.0>; tag/commit `917f683`; 14 assets re-downloaded and checksum-verified; GHCR index `sha256:edb4686...`; arm64 pulled from GHCR and healthy: `docs/smoke/2026-11-11-release.md` |

## Gate F decision

**Released by owner approval on 2026-11-11 with one accepted runtime gap.**
Selected fake-upstream oracle recordings are complete, and the v1/v2
fake-upstream performance baseline meets the 1.2x p95 target (ratio 0.208;
local evidence only). Real-account chat/stream and mutable flows passed; real
media smoke is waived. Real-archive install/rollback passed on a local root, on
a temporary systemd service, and with the Windows binary + `.cmd` hooks under
Wine. CI verify, multi-arch arm64 runtime, and a local registry push/pull
passed. Dependency scanning found no reachable vulnerabilities. The owner
explicitly approved the final `2.0.0` release while accepting that real Windows
SCM/service execution remains unverified. The v2 source is published on branch
`v2` of `ayeaaaa/mix2api`; `main` remains v1.6.6 (frozen).

**v2.0.2 addendum:** the patch release publishes the dashboard total-quota
column and the Raccoon daily-activity check-in ported from PR #3. The new
behavior is covered by unit, route, scheduler, dashboard-contract, and fake-
upstream tests; the existing v1.6.5 oracle baseline predates it and was not
re-recorded for this patch.

**v2.0.3 addendum:** this patch fixes migrated usage-cache adoption. Copied
checkpoints are validated by size/offset/tail, and v1 global analytics/series
checkpoints are reused only by the all-realm view. The change is covered by
cache, realm-isolation, analytics, and series tests; the full `make check` and
`make race` suites pass.

**v2.0.4 addendum:** this patch removes the post-midnight cold scan. A missing
current-day analytics bucket is treated as empty, and migrated v1 series
caches can serve open windows whose current minute/hour has no rows yet.
Regression tests cover both cases; `make check` and `make race` pass.

**v2.0.5 addendum:** this patch removes the dominant full-scan CPU hotspot.
Pricing uses an immutable timeline internally and a per-scan `RowPricer`; a
19.5k-row local full `/usage` scan drops from about 75.7s to about 0.82s.
`make check` and `make race` pass.

**v2.0.6 addendum:** aggregation scans now decode through a compact `scanRow`
struct, avoiding a per-line `json.Decoder` and most interface boxing while
keeping `Row` as the compatibility boundary. The same local full `/usage` scan
drops from about 0.82s to about 0.60s; `make check` and `make race` pass.

**v2.0.7 addendum:** analytics now has a persistent SQLite rollup keyed by
day/realm/account/model/key. It advances by log offset and is only used when the
analytics view cache is invalid; missing/corrupt data falls back to the scan.
On a local 19.5k-row ledger the first build takes about 0.64s and later reads
take about 3ms. `make check` and `make race` pass.

**v2.1.0 addendum:** optional dashboard branding is covered by settings
validation tests, dashboard contract/hash tests, JS harness tests and a real
browser smoke (tab title, sidebar title/mark, favicon). API, health and update
identity are unchanged. `make check` and `make race` pass.

**v2.1.1 addendum:** cache entries preserve other realms, unchanged
checkpoints skip rewrites, Chat Completions recovers declared printed DSML tool
calls, in-band stream errors fail closed, and CodeArts/Raccoon preserve legacy
`functions` / `tool_choice=none` declarations. Covered by Go tests and the full
`make check` / `make race` suites.

**v2.2.0 addendum:** WorkBuddy International/China can opt into an in-process
uTLS bridge that reproduces the official Node/OpenSSL ClientHello (18 ciphers,
10 extensions, no ALPN). JA3/JA4, unauthenticated CONNECT, direct,
HTTP/HTTPS/SOCKS5 proxy, keep-alive, certificate failure, and live upstream
handshakes pass. The build baseline moved to Go 1.26 and uTLS 1.8.2;
`govulncheck` reports 0 vulnerabilities. Real-account WorkBuddy China chat and
SSE through the bridge passed on 2026-11-12. Published as `v2.2.0` at
`de1d650`; evidence: `docs/smoke/2026-11-12-release-2.2.0.md`.

**v2.3.0 addendum:** the dashboard is now single-layout, with module-grouped
navigation and provider-scoped accounts/settings; CodeArts uses the shared
daily check-in scheduler; WorkBuddy account views expose finite-expiry credit
totals; and the Usage page can scope every analytics request to the selected
realm with stale-response guards. `make check`, `make race`, and the 9/9
dashboard JS suites pass. Published as `v2.3.0` at `62fddc7`; evidence:
`docs/smoke/2026-11-12-release-2.3.0.md`. No real media or Windows SCM execution
was added; the existing media waiver and Windows gate remain unchanged.

**v2.3.1 addendum:** an API key without a bound realm now prefers the persisted
active realm when multiple providers expose the same model, then falls back to
the previous provider order if the active catalog does not contain it. Explicit
realm and bound-key precedence are unchanged. Regression tests and `make check`
/ `make race` pass. Published as `v2.3.1` at `8a26e9a`; evidence:
`docs/smoke/2026-11-12-release-2.3.1.md`. No media or Windows SCM gate changed.

**v2.3.2 addendum:** the Buddy check-in badge now treats a successful
`lastCheckin` record for the current day as authoritative, so an earlier
scheduler retry failure cannot overwrite it; failures remain in the activity
history. Dashboard asset hash/size metadata and a 10th JS suite cover the case.
`make check`, `make race`, and the 10/10 dashboard JS harness pass. Published as
`v2.3.2` at `e9b11ea`; evidence: `docs/smoke/2026-11-12-release-2.3.2.md`. No
media or Windows SCM gate changed.

**v2.4.0 addendum:** exit-qualified model IDs route Chat, Responses, and
Messages exactly to the named exit while sending bare IDs upstream; a gateway
catalog covers all exits and `realm=all` remains WorkBuddy-only. Key allow-lists
match canonical and upstream IDs, and usage records canonical requested models.
WorkBuddy credit packages are now fully paged with used/expired statuses and
official cumulative totals. `make check`, `make race`, and the 12/12 dashboard
JS harness pass. Published as `v2.4.0` at `13260d1`; evidence:
`docs/smoke/2026-11-12-release-2.4.0.md`. No media or Windows SCM gate changed.

**v2.5.0 addendum:** adds Doubao Work as the fifth native provider (cookies/device
import, account pool, affinity, concurrency/cooling, proxy slots, streaming and
non-streaming chat, multi-turn, credits/tasks, opt-in check-in scheduling, and a
desktop check-in probe), plus all-provider `gateway` key binding. Doubao reward
submission still requires desktop eligibility; web-only cookies receive a clear
400 rather than a fabricated success. `make check`, `make race`, and the 13/13
dashboard JS harness pass. Published as `v2.5.0` at `b04750f`; evidence:
`docs/smoke/2026-11-13-release-2.5.0.md`. No media or Windows SCM gate changed.

**v2.6.0 addendum:** Doubao Work can claim completed campaign rewards through
`POST /doubao/tasks/claim`; the dashboard exposes a Claim button for completed
tasks, cached task identifiers are retained, and upstream `biz_error` responses
are surfaced instead of faked as success. `make check`, `make race`, and the
13/13 dashboard JS harness pass. Published as `v2.6.0` at `20bdbd4`; evidence:
`docs/smoke/2026-11-13-release-2.6.0.md`. No media or Windows SCM gate changed.

**v2.6.1 addendum:** documentation-only release marking Doubao Work as a 256K
context, maintenance-only exit. Published as `v2.6.1` at `5f24268`; evidence:
`docs/smoke/2026-11-13-release-2.6.1.md`. No runtime gate changed.

**v2.6.2 addendum:** CodeArts panel OAuth callbacks with an authenticated
`session_id` no longer require Huawei's rewritten `state`; direct browser
callbacks still validate `state`/`ticket_id`. The add-account UI now displays and
copies the authorization link instead of auto-opening it. `make check`,
`make race`, and the 14/14 dashboard JS harness pass. Published as `v2.6.2` at
`c75bfe1`; evidence: `docs/smoke/2026-11-13-release-2.6.2.md`. No media or
Windows SCM gate changed.

**v2.0.7 addendum:** analytics now has a persistent SQLite rollup keyed by
day/realm/account/model/key. It advances by log offset and is only used when the
analytics view cache is invalid; missing/corrupt data falls back to the scan.
On a local 19.5k-row ledger the first build takes about 0.64s and later reads
take about 3ms. `make check` and `make race` pass.

## Required signoff

| Role | Name | Date | Result |
| --- | --- | --- | --- |
| Build/reproducibility | evidence | 2026-11-11 | PASS |
| Data migration | evidence | 2026-11-11 | PASS |
| Oracle/parity | evidence | 2026-11-11 | PASS with documented deltas |
| Real-account smoke | evidence | 2026-11-11 | PASS (media waived) |
| Performance | evidence | 2026-11-11 | PASS |
| Security review | evidence | 2026-11-11 | PASS (no reachable vulns; 1 unreachable module finding) |
| Release owner | ayeaaaa | 2026-11-11 | APPROVED final v2.0.0; accepts real Windows SCM gap |
| Release owner | ayeaaaa | 2026-11-11 | APPROVED v2.0.2 patch (dashboard total quota + Raccoon daily activity) |
