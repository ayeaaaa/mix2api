# mix2api v2

Go rewrite of the mix2api multi-provider AI gateway. One CGO-free binary serves
OpenAI Chat Completions, Responses, Anthropic Messages, SSE, the dashboard, and
the provider admin surfaces for WorkBuddy (intl/cn), CodeArts, and Raccoon.

**Status: `2.0.1` final (2026-11-11).** The v2 source is published on branch
`v2` of `ayeaaaa/mix2api`; `main` remains v1.6.5. Gate F evidence is recorded in
[`docs/gate-f.md`](docs/gate-f.md); real Windows SCM update execution remains
the only unverified runtime item.

## Quick start

```sh
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o mix2api ./cmd/mix2api
./mix2api version
PANEL_PASSWORD='change-me' MIX2API_API_KEY='change-me-too' ./mix2api
```

- `HOST` defaults to `127.0.0.1`, `PORT` to `58675`.
- A non-loopback `HOST` requires both `PANEL_PASSWORD` and
  `MIX2API_API_KEY`; startup fails otherwise.
- `ACCOUNTS_DIR` defaults to `accounts`, `USAGE_DIR` to `usage`,
  `WB_MEDIA_DIR` to `media`.
- The dashboard is embedded in the binary; no separate asset directory is
  needed.

## Commands

```sh
mix2api version [--json|--short]   # injected version/commit/date
mix2api migrate --from ... --to ... [--dry-run]
mix2api rollback --from ... --to ... [--dry-run]
mix2api update run --request-dir ... --root ...
mix2api update install --root ... --version ... --archive ... --checksums ...
mix2api update rollback --root ... --version ...
mix2api update recover --root ...
```

See [`docs/migration.md`](docs/migration.md),
[`docs/update-executor.md`](docs/update-executor.md), and
[`docs/runbook.md`](docs/runbook.md).

## Build and package

```sh
make build          # local binary in bin/
make check          # gofmt check, vet, tests
make race           # race tests
make dist           # full reproducible release matrix
```

`make dist` writes Linux amd64/arm64/armv7, Windows amd64, macOS amd64/arm64,
OpenWrt amd64/arm64/armv7, and portable archives plus `SHA256SUMS`, an SPDX
SBOM, and `release.json`. Version, commit, and date come from `VERSION`,
`COMMIT`, and `BUILD_DATE`/`SOURCE_DATE_EPOCH`. See
[`docs/release.md`](docs/release.md) and [`docs/packaging.md`](docs/packaging.md).

Example release build:

```sh
VERSION=2.0.1 \
COMMIT="$(git rev-parse HEAD)" \
BUILD_DATE="$(git show -s --format=%cI HEAD)" \
SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)" \
make dist
```

## Docker

The root [`Dockerfile`](Dockerfile) is multi-stage, builds with `CGO_ENABLED=0`,
and runs as UID/GID `10001` with a `/health` healthcheck.

```sh
cp .env.example .env
mkdir -p data/accounts data/usage data/media
sudo chown -R 10001:10001 data
docker compose up -d --build
curl -fsS http://127.0.0.1:58675/health
```

See [`docs/packaging.md`](docs/packaging.md) for the image/compose variants and
volume ownership.

## Data compatibility

Account files, `state.json`, `settings.json`, usage JSONL, and `logs.sqlite3`
keep their v1.6.5 formats; unknown fields survive reads and writes. Migration is
copy-only and the original v1.6.5 tree remains untouched. Keep v1 and v2 from
sharing a live data directory: stop one process before starting the other, and
use `mix2api rollback` to produce a fresh v1-compatible directory when needed.

## Tests

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/mix2api
```

- `tests/contract`: scoped v2 regression goldens (not a captured full v1.6.5
  oracle; see `tests/contract/testdata/PROVENANCE.md`).
- `tests/oracle`: loopback oracle harness and real-account smoke checklist.
- `tests/load`: 200-concurrent SSE harness, skipped unless `MIX2API_LOAD=1`.
- `tests/packaging`: Docker/compose/portable/OpenWrt/version/reproducibility
  checks.

## Documentation

- [Packaging](docs/packaging.md)
- [Release process](docs/release.md)
- [Migration and rollback](docs/migration.md)
- [Update executor](docs/update-executor.md)
- [Operations runbook](docs/runbook.md)
- [Gate F](docs/gate-f.md)
- [Security policy](SECURITY.md)
- [Changelog](CHANGELOG.md)

## License

MIT. See [`LICENSE`](LICENSE). The v1.6.5 project remains the behavior oracle
and is not modified by this repository.
