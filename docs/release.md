# Release process

## Current status

This tree is **`2.3.0`** final. The v2 source is published on branch `v2` of
`ayeaaaa/mix2api`; the repository's `main` branch remains v1.6.6. The release is
<https://github.com/ayeaaaa/mix2api/releases/tag/v2.3.0> and the GHCR image is
`ghcr.io/ayeaaaa/mix2api:v2.3.0`. Gate F evidence is in `docs/gate-f.md`; real
Windows SCM update execution remains the only unverified runtime item.

Note: v1 is frozen at `v1.6.6`; its update checks point at the same repository
and will see the newest v2 release. v1's installer expects a v1 asset that v2
does not publish, so a v1 deployment with update checks enabled may see an
update it cannot install. v1 update checks are off by default.

## Prerequisites

- Go 1.26+ (the module pins `go 1.26`).
- `git`, `sha256sum`, and Docker/buildx for optional container verification.
- No network is required for a build after `go mod download` has populated the
  module cache. The release command itself makes no network calls.
- Gate F oracle/smoke evidence for the exact commit being released.

## Build the candidate

```sh
git status --short           # must be clean
VERSION=2.3.0 \
COMMIT="$(git rev-parse HEAD)" \
BUILD_DATE="$(git show -s --format=%cI HEAD)" \
SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)" \
make dist
```

Outputs in `dist/`:

| Asset | Purpose |
| --- | --- |
| `mix2api-<version>-linux-{amd64,arm64,armv7}.tar.gz` | Linux servers/containers |
| `mix2api-<version>-windows-amd64.zip` | Windows server |
| `mix2api-<version>-darwin-{amd64,arm64}.tar.gz` | macOS |
| `mix2api-<version>-openwrt-{amd64,arm64,armv7}.tar.gz` | OpenWrt + init/UCI |
| `mix2api-<version>-portable-{linux-amd64,windows-amd64}` | portable launchers |
| `SHA256SUMS` | checksums for every archive, `sbom.spdx.json`, and `release.json` |
| `sbom.spdx.json` | SPDX 2.3 dependency inventory |
| `release.json` | version/tag/commit/date + asset hashes |

Every archive contains `release-manifest.json` with per-file sha256 and mode.
The update executor verifies that manifest and `SHA256SUMS` before replacing
anything. `SHA256SUMS` covers every deliverable except itself.

`SOURCE_DATE_EPOCH` (or `--archive-date`) fixes archive entry timestamps; when
unset the tool uses `1980-01-01T00:00:00Z`, never "now". An invalid
`SOURCE_DATE_EPOCH` is an error rather than a silent fallback. `BUILD_DATE` is
the injected build identity and must be the commit timestamp for reproducibility.

## Verify

```sh
cd dist
sha256sum -c SHA256SUMS
tar -tzf mix2api-2.3.0-linux-amd64.tar.gz | head
./mix2api version --json   # after extracting, must show the injected values
```

Reproducibility check:

```sh
make clean
# Re-run the same VERSION/COMMIT/BUILD_DATE/SOURCE_DATE_EPOCH command twice.
# Assets must be byte-identical.
```

## Container

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=2.3.0 \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg BUILD_DATE="$(git show -s --format=%cI HEAD)" \
  -t ghcr.io/ayeaaaa/mix2api:v2.3.0 .
```

The image runs as UID/GID 10001 with a BusyBox healthcheck on `/health`.

## Publish

The v2 source lives on branch `v2`; the repository's `main` branch remains
v1.6.6. The release workflow (`.github/workflows/release.yml`) is manual and
refuses to publish unless `publish=true` and `gate_f_signed=yes`. The `version`
input must be a strict semantic version **without** a leading `v` (`2.0.0`, not
`v2.0.0`); a dedicated validation step rejects `v`/`V` prefixes, leading
zeroes, and build metadata before any build starts. It:

1. verifies the requested commit matches the checkout;
2. runs `make check`, race tests, Windows/macOS cross-builds, `make dist`, and
   `sha256sum -c`;
3. builds/pushes `linux/amd64,linux/arm64` to GHCR;
4. creates the `v<version>` tag and GitHub release with the dist assets.

`CHANGELOG.md` is used verbatim as the GitHub release notes. Every version
must include a Chinese `### 中文` section before the English `### English`
section, so published release notes are always bilingual.

There is no push/tag trigger and no scheduled release.

For this repository layout, the equivalent manual publication is:

```sh
git push origin v2
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=2.3.0 \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg BUILD_DATE="$(git show -s --format=%cI HEAD)" \
  --provenance=false --push \
  -t ghcr.io/ayeaaaa/mix2api:v2.3.0 .
gh release create v2.3.0 dist/* \
  --target "$(git rev-parse HEAD)" \
  --title "mix2api v2.3.0" \
  --notes-file CHANGELOG.md
```

The tag points at the v2 commit, not at `main`. Do not move `main` or overwrite
the v1.6.6 tag.

## Rollback a release

- Application update: install the previous archive with
  `mix2api update install --version <previous>`; the executor takes a complete
  backup first.
- Data: `mix2api rollback --from <v2-data> --to <fresh-v1-dir>`, then point
  v1.6.6 at the fresh directory. The command is copy-only, validates the
  v1-format data, and never writes the source. Never share a live directory.
- Container: redeploy the previous image tag/digest.

## Versioning

- Development defaults to the `2.3.0` identity in
  `internal/core/version`.
- Release builds inject `Version`, `Commit`, and `Date` with `-ldflags -X`.
- The current release is `v2.3.0`; future fixes use a new semantic version.
