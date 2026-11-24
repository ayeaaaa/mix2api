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
SCM/service execution remains unverified. The source is published on `main` of
`ayeaaaa/mix2api`; v1.6.6 is archived as a tag/release and history.

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

**v2.8.0 addendum:** adds server-side recent-request filters/failure details,
one-shot Messages affinity, bounded Key/CodeArts concurrency waits, opt-in loose
billing cleanup, and operational campaign automation (portal tasks, Starbucks /
Reporter's Day, real skill-share channels, real template scenes). `make check`,
`make race`, and dashboard JS suites pass. Published as `v2.8.0` at `58fcbcc`;
evidence: `docs/smoke/2026-11-13-release-2.8.0.md`. No media or Windows SCM gate
changed.

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

**v2.6.3 addendum:** the key card and analytics table now render a CodeArts
realm binding as “CodeArts” instead of “Follow panel”. `make check`, `make race`,
and the 14/14 dashboard JS harness pass. Published as `v2.6.3` at `f70eb9a`;
evidence: `docs/smoke/2026-11-13-release-2.6.3.md`. No media or Windows SCM gate
changed.

**v2.7.0 addendum:** CodeArts usage now settles the cumulative used-credit delta
per request (explicit credit wins; sampling is best effort; concurrent requests
share a high-water mark). API keys support a daily token limit enforced before
concurrency, and both providers harden tool-schema keyword types to avoid
upstream code 11129. `make check`, `make race`, and the 14/14 dashboard JS
harness pass. Published as `v2.7.0` at `c24b744`; evidence:
`docs/smoke/2026-11-13-release-2.7.0.md`. No media or Windows SCM gate changed.

**v2.0.7 addendum:** analytics now has a persistent SQLite rollup keyed by
day/realm/account/model/key. It advances by log offset and is only used when the
analytics view cache is invalid; missing/corrupt data falls back to the scan.
On a local 19.5k-row ledger the first build takes about 0.64s and later reads
take about 3ms. `make check` and `make race` pass.

**v2.8.1 addendum:** Raccoon daily activity now triggers `daily_grant` by
refreshing the login session instead of sending a chat, so it no longer spends
points; failed refreshes persist `refreshed=false` and the dashboard stays
consistent with the bill. `make check`, `make race`, and the 15/15 dashboard JS
harness pass. Published as `v2.8.1` at `4d6a24a`; evidence:
`docs/smoke/2026-11-13-release-2.8.1.md`. No media or Windows SCM gate changed.

**v2.8.2 addendum:** Anthropic Messages MAX (`budget_tokens=31999`), CodeArts
thinking effort, and Raccoon 1017 reasoning efforts now preserve `max`.
Raccoon streaming requests drain and record post-DONE credit after client
disconnect across Chat, Messages, and Responses, with a bounded Responses
drain. The repository is now `main`-only; v1.6.6 is archived as a tag/release.
`make check`, `make race`, and the 15/15 dashboard JS harness pass. Published as
`v2.8.2` at `e35e3fc`; evidence:
`docs/smoke/2026-11-14-release-2.8.2.md`. No media or Windows SCM gate changed.

**v2.8.3 addendum:** Raccoon daily activity now triggers `daily_grant` through
the authenticated web `GET /api/web/office/v3/setting_info` startup request;
token refresh is only used to repair a rejected session before retrying. The
dashboard now reports triggered / failed / not-triggered states instead of
claiming a login refresh grants the reward. `make check`, `make race`, and the
15/15 dashboard JS harness pass. Published as `v2.8.3` at `5363670`; evidence:
`docs/smoke/2026-11-14-release-2.8.3.md`. No media or Windows SCM gate changed.

**v2.8.4 addendum:** provider schedule switches moved to the Automation page;
the account action column was widened; WorkBuddy international activity now
runs `/v2/report`; Raccoon daily activity retries once per four-hour window;
CodeArts browser callbacks accept exact nested `ticket_id` matching while
rejecting the sole-pending fallback. `make check`, `make race`, and the 15/15
dashboard JS harness pass. Published as `v2.8.4` at `af3a1d7`; evidence:
`docs/smoke/2026-11-16-release-2.8.4.md`. No media or Windows SCM gate changed.

**v2.8.5 addendum:** desktop account-table columns were rebalanced so all data
columns fit at 1440px and the action column no longer covers them; between
641–1399px the action column is non-sticky during horizontal scrolling.
`make check`, `make race`, and the 15/15 dashboard JS harness pass. Published as
`v2.8.5` at `30e9c95`; evidence: `docs/smoke/2026-11-16-release-2.8.5.md`. No
media or Windows SCM gate changed.

**v2.8.6 addendum:** account lists now use a complete card layout below
1440px; the card CSS was moved into the active stylesheet after the initial
PR #37 placement in a disabled `media="not all"` block. 1440px+ retains the
desktop table. `make check`, `make race`, and live 1440/820/390px checks for
WorkBuddy 国内 and 国际 pass without horizontal overflow. Published as
`v2.8.6` at `ca5ebaf`; evidence: `docs/smoke/2026-11-16-release-2.8.6.md`. No
media or Windows SCM gate changed.

**v2.8.7 addendum:** WorkBuddy web activity now uses the full 300-prompt
built-in pool, with settings view/save support and selected prompts applied to
web conversation creation and ACP turns. The release workflow now extracts the
current version's CHANGELOG section for GitHub Release notes. `make check`,
`make race`, and the 15/15 dashboard JS harness pass. Published as `v2.8.7` at
`0be783a`; evidence: `docs/smoke/2026-11-16-release-2.8.7.md`. No media or
Windows SCM gate changed.

**v2.8.8 addendum:** the desktop account column is wider and account names,
badges, OAuth, expiry, and device details wrap instead of being clipped; other
data and action columns are unchanged, and `<1440px` keeps the card layout.
`make check`, `make race`, and the 15/15 dashboard JS harness pass. Published as
`v2.8.8` at `726e1de`; evidence: `docs/smoke/2026-11-17-release-2.8.8.md`. No
media or Windows SCM gate changed.

**v2.8.9 addendum:** the WorkBuddy account list now uses the page scrollbar
only; its section cap and table-wrapper vertical overflow are disabled while
horizontal table scrolling remains, and other capped sections are unchanged.
`make check`, `make race`, and the 15/15 dashboard JS harness pass. Published as
`v2.8.9` at `de07b03`; evidence: `docs/smoke/2026-11-18-release-2.8.9.md`. No
media or Windows SCM gate changed.

**v2.8.10 addendum:** WorkBuddy International and CN refresh stale credits in
the background every 30 minutes, one oldest enabled account per tick. TTL is
controlled by `credits_refresh_hours` (default 12h; 0 disables); failed
accounts are parked for six hours and skipped so one broken account cannot
block the queue. `make check`, `make race`, and the 15/15 dashboard JS harness
pass. Published as `v2.8.10` at `2d9390c`; evidence:
`docs/smoke/2026-11-18-release-2.8.10.md`. No media or Windows SCM gate changed.

**v2.8.11 addendum:** `/v1/models` now honors `models_only_available` across
concrete, `all`, and `gateway` scopes: realms with no usable account advertise
no models and PRO-gated models require a paid account. Availability ignores
transient cooldowns/quotas. `make check`, `make race`, and the 15/15 dashboard
JS harness pass. Published as `v2.8.11` at `b0feaf8`; evidence:
`docs/smoke/2026-11-19-release-2.8.11.md`. No media or Windows SCM gate changed.

**v2.8.12 addendum:** CodeArts check-in status is date-aware and the provider
now applies shared account limits (reserve credits, daily tokens/credits,
per-model tokens) during selection with a `guard` view field. `make check`,
`make race`, and the 15/15 dashboard JS harness pass. Published as `v2.8.12` at
`9d84299`; evidence: `docs/smoke/2026-11-20-release-2.8.12.md`. No media or
Windows SCM gate changed.

**v2.8.13 addendum:** WorkBuddy CLI identity is aligned with CodeBuddy
2.185.0 (wire-shape change), and provider scheduling supports independent daily
check-in times with same-day 30-minute retries after failed check-ins for
WorkBuddy, CodeArts, and Doubao. `make check`, `make race`, and the 15/15
dashboard JS harness pass. Published as `v2.8.13` at `5fabb3c`; evidence:
`docs/smoke/2026-11-22-release-2.8.13.md`. No media or Windows SCM gate changed.

**v2.8.14 addendum:** same-day check-in retry interval is configurable per
provider (default 30 minutes, 0 disables), and `/v1/responses` translates flat
function tools and `tool_choice` into the nested Chat Completions shape for
strict upstreams such as Raccoon. `make check`, `make race`, and the 15/15
dashboard JS harness pass. Published as `v2.8.14` at `afbb7a5`; evidence:
`docs/smoke/2026-11-23-release-2.8.14.md`. No media or Windows SCM gate changed.

**v2.8.15 addendum:** CodeArts and Raccoon usage snapshots now read cached
tokens from nested `prompt_tokens_details.cached_tokens` and legacy aliases, so
cache-hit percentages are no longer reported as zero. Historical usage rows are
not backfilled. `make check`, `make race`, and the 15/15 dashboard JS harness
pass. Published as `v2.8.15` at `ad977c9`; evidence:
`docs/smoke/2026-11-23-release-2.8.15.md`. No media or Windows SCM gate changed.

**v2.8.16 addendum:** requests without an explicit session ID now derive prefix
affinity from leading system/developer messages plus the first user across Chat,
Responses, and Messages on WorkBuddy, CodeArts, and Raccoon; derived
conversations above 400 messages rotate within at most 3 accounts, optional
per-model cohorts are supported, models settings expose the routing controls,
and usage rows gain privacy-safe affinity diagnostics. Remaining-usage priority
stays stored but is reported unsupported and disabled. `make check`, `make
race`, and the 16/16 dashboard JS harness pass. Published as `v2.8.16` at
`07401b4`; evidence: `docs/smoke/2026-11-23-release-2.8.16.md`. No media or
Windows SCM gate changed.

**v2.8.17 addendum:** long-conversation routing is now selectable. The default
`cohort` strategy keeps the v2.8.16 fixed-account rotation, while the optional
`partition` strategy enables V1 UID-sorted rank-modulo partitions globally or
per WorkBuddy/CodeArts/Raccoon, with shared page cursors, in-page priorities,
the small-realm sticky guard, and safe group-external fallback. `page_size`
meaning follows the resolved strategy, and the dashboard exposes the new
controls. `make check`, `make race`, and the 16/16 dashboard JS harness pass.
Published as `v2.8.17` at `1c1ee4e`; evidence:
`docs/smoke/2026-11-23-release-2.8.17.md`. No media or Windows SCM gate changed.

**v2.8.18 addendum:** proxy slots now perform real HTTP / HTTPS(CONNECT) /
SOCKS5 exit probing and mihomo port discovery, wired into the production
composition; the panel adds a structured protocol/server/port/credentials form,
stores privacy-safe geo metadata, sanitizes probe errors, and documents the
Docker loopback caveat. Failures stay per slot/candidate and never block saving
or account binding. `make check`, `make race`, and the 17/17 dashboard JS harness
pass. Published as `v2.8.18` at `de86865`; evidence:
`docs/smoke/2026-11-23-release-2.8.18.md`. No media or Windows SCM gate changed.

**v2.8.19 addendum:** proxy slots are now edited as inline structured rows for
protocol, server, port, username, and password, with a masked password and an
expandable raw-URL editor for paths, queries, legacy forms, and special
credentials. Save/test validate all rows, reject duplicate normalized URLs, and
save dirty state before probing. No API, storage, transport, or account-binding
changes. `make check`, `make race`, and the 17/17 dashboard JS harness pass.
Published as `v2.8.19` at `49a4fb6`; evidence:
`docs/smoke/2026-11-24-release-2.8.19.md`. No media or Windows SCM gate changed.

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
