# Packaging and deployment assets

Every release is one CGO-free binary with the dashboard embedded. The same
binary is shipped as:

| Asset | Contents |
| --- | --- |
| `mix2api-<version>-<os>-<arch>` | binary, `LICENSE`, `README.md`, Linux systemd unit, `release-manifest.json` |
| `mix2api-<version>-openwrt-<arch>` | Linux binary, procd init script, UCI config sample, `OPENWRT.md` |
| `mix2api-<version>-portable-<os>-<arch>` | binary, start/stop launchers, `LICENSE`, `README.md` |
| `SHA256SUMS` | sha256 for every archive, SBOM, and release metadata file |
| `sbom.spdx.json` | SPDX 2.3 package inventory from the built binary |
| `release.json` | version, tag, commit, date, and asset checksums |

On Linux the unit expects the install root `/srv/mix2api`, a system
`mix2api` user, and data under `/srv/mix2api/{accounts,usage,media}`; see
`docs/runbook.md`. `systemd-analyze verify` is exercised by the packaging test.

`release-manifest.json` inside each archive lists every payload file with its
sha256 and mode. The update executor verifies that manifest before replacing
anything. `SHA256SUMS` covers all generated deliverables except itself.

## Docker

```sh
cp .env.example .env         # set both credentials
docker compose up -d --build
curl -fsS http://127.0.0.1:58675/health
```

The image is multi-stage and final-stage minimal: `golang:1.22-alpine` builds
with `CGO_ENABLED=0`, and `alpine:3.20` runs as UID/GID `10001`. The healthcheck
uses BusyBox `wget` against the container's `PORT`. The application directory
is read-only; only the three data volumes are writable.

For prebuilt images use `deploy/docker-compose.image.yml`; for a source build
use `deploy/docker-compose.yml`. The root `docker-compose.yml` is the same
example for a repository checkout.

Multi-arch builds:

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=2.0.0-rc.1 \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg BUILD_DATE="$(git show -s --format=%cI HEAD)" \
  -t ghcr.io/ayeaaaa/mix2api-v2:v2.0.0-rc.1 --push .
```

### Volume ownership

The container runs unprivileged. On first use create the host directories and
match the runtime UID/GID (default `10001:10001`):

```sh
mkdir -p data/accounts data/usage data/media
sudo chown -R 10001:10001 data
```

Compose allows `MIX2API_UID`/`MIX2API_GID` overrides. Never run the container as
root just to avoid this step.

## Portable and OpenWrt

Portable archives include start/stop launchers and create `accounts/`, `usage/`,
and `media/` beside the binary. OpenWrt archives include the procd init script
and UCI sample; see `packaging/openwrt/README.md`.

## Reproducible builds

```sh
make dist
```

The release tool uses `-trimpath -buildvcs=false -buildmode=exe`, clears the
linker build id, and writes archives with fixed timestamps/owners and sorted
entries. Set `VERSION`, `COMMIT`, `BUILD_DATE`, and `SOURCE_DATE_EPOCH` to
reproduce an asset byte-for-byte. No network is required after modules are in
the local build cache.
