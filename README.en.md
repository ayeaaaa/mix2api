# mix2api v2

[中文](README.md) | [English](README.en.md)

mix2api v2 is a multi-provider AI gateway rewritten in Go. A single process and
a single binary expose OpenAI Chat Completions, Responses, Anthropic Messages,
SSE, and an embedded dashboard, bringing WorkBuddy International / China,
Huawei Cloud CodeArts（遥遥领先）, and Raccoon（商汤小浣熊） into one API-key and
account-pool surface.

Current version: **2.0.7**. The default branch is `v2`; `main` keeps the frozen
v1.6.6 history.

## Origin

mix2api is based on the WorkBuddy2API-Hub community project, with protocol
references from CodeArts2API.

v1 is the Python single-process version, frozen at 1.6.6. v2 is a Go project
redesigned from the behavior and data formats of v1.6.5 / v1.6.6, not a
line-by-line translation. The Python v1 implementation remains the behavioral
reference; new features, fixes, and releases happen in v2.

## v2 architecture

The main architectural change is moving from “everything in one application” to
“shared core + independent providers”:

```text
Client / SDK
   │  OpenAI Chat / Responses / Anthropic Messages / SSE
   ▼
mix2api v2 (single binary)
   ├─ API / SSE / embedded dashboard
   ├─ core: config, account state, transport, scheduler, usage, logs
   ├─ providers: WorkBuddy International / China, CodeArts（遥遥领先）, Raccoon（商汤小浣熊）
   └─ platform: panel, media, agents, pricing, updates
```

- **Single process, single binary**: production builds use `CGO_ENABLED=0` and
  require no Python, Node, or sidecar runtime.
- **Compile-time provider registration**: each upstream is an independent
  package. Adding an exit only requires a new provider, not changes to the
  shared routing, account, or usage logic.
- **The dashboard is still embedded static HTML/JS**: it is bundled into the
  binary with `go:embed`, with no frontend build chain.
- **Data compatible with v1.6.6**: account files, `state.json`, `settings.json`,
  usage JSONL, and the log database remain readable and writable; unknown
  fields are preserved. Migration is copy-only, and v1 and v2 must not share a
  live data directory.

## Capabilities

- Four exits: WorkBuddy International, WorkBuddy China, CodeArts（遥遥领先）,
  and Raccoon（商汤小浣熊）. API keys can be bound to an exit or follow the
  dashboard default.
- Three protocols: `/v1/chat/completions`, `/v1/responses`, and Anthropic
  Messages, with unified SSE streaming.
- Account pools, concurrency/cooldown handling, proxy slots, model catalogs,
  credit/quota guards, usage, logs, and activity records are managed by the
  same process.
- The dashboard includes account management, tasks, settings, usage, logs, and
  update/rollback entry points.

## Deployment

### Option 1: Docker image (recommended)

Images are published to GHCR for `linux/amd64` and `linux/arm64`:

```sh
docker pull ghcr.io/ayeaaaa/mix2api:v2.0.7
```

Generate two independent credentials in the current shell. Do not write real
values into the repository or commit them to Git:

```sh
export PANEL_PASSWORD="$(openssl rand -base64 24)"
export MIX2API_API_KEY="sk-$(openssl rand -hex 24)"
```

Prepare the data directories. The container runs as `10001:10001` by default:

```sh
mkdir -p data/accounts data/usage data/media
sudo chown -R 10001:10001 data
```

Start the container:

```sh
docker run -d --name mix2api \
  --restart unless-stopped \
  -p 127.0.0.1:58675:58675 \
  -e HOST=0.0.0.0 \
  -e PORT=58675 \
  -e TZ=Asia/Shanghai \
  -e PANEL_PASSWORD \
  -e MIX2API_API_KEY \
  -v "$PWD/data/accounts:/app/accounts" \
  -v "$PWD/data/usage:/app/usage" \
  -v "$PWD/data/media:/app/media" \
  ghcr.io/ayeaaaa/mix2api:v2.0.7
```

This publishes the port only on `127.0.0.1`, suitable for a reverse proxy or an
SSH tunnel. For LAN access, change the listener only within a controlled
network; do not expose the management panel directly to the public internet.

Check health:

```sh
curl -fsS http://127.0.0.1:58675/health
```

### Option 2: Docker Compose

The repository includes a compose file for the prebuilt image:

```sh
cd deploy
cp ../.env.example .env
chmod 600 .env
```

Edit `.env` and replace at least `PANEL_PASSWORD` and `MIX2API_API_KEY` with real
values, then run:

```sh
mkdir -p ../data/accounts ../data/usage ../data/media
sudo chown -R 10001:10001 ../data
docker compose -f docker-compose.image.yml up -d
curl -fsS http://127.0.0.1:58675/health
```

To build the image locally, use `docker-compose.yml`:

```sh
cd deploy
docker compose up -d --build
```

The compose file publishes port `58675` on all interfaces by default. Before a
public deployment, add a reverse proxy and TLS, and restrict access to the
management panel.

### Option 3: Run the binary directly

Download the archive for your platform and `SHA256SUMS` from the
[GitHub Release](https://github.com/ayeaaaa/mix2api/releases), verify it, and
extract it:

```sh
sha256sum -c SHA256SUMS
tar -xzf mix2api-2.0.7-linux-amd64.tar.gz
cd mix2api-2.0.7-linux-amd64
```

Start it. `HOST` defaults to `127.0.0.1`; the data directories default to
`accounts/`, `usage/`, and `media/` under the current directory:

```sh
export PANEL_PASSWORD="$(openssl rand -base64 24)"
export MIX2API_API_KEY="sk-$(openssl rand -hex 24)"
./mix2api
```

To use absolute paths:

```sh
ACCOUNTS_DIR=/srv/mix2api/data/accounts \
USAGE_DIR=/srv/mix2api/data/usage \
WB_MEDIA_DIR=/srv/mix2api/data/media \
./mix2api
```

Windows and macOS archives have the same layout. OpenWrt init/UCI instructions
are in [`packaging/openwrt/README.md`](packaging/openwrt/README.md).

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `HOST` | `127.0.0.1` | Listen address; a non-loopback bind requires both credentials |
| `PORT` | `58675` | HTTP port |
| `PANEL_PASSWORD` | none | Panel password; used for initial bootstrap only, later changes in the dashboard take precedence |
| `MIX2API_API_KEY` | none | `/v1` API key; clients send `Authorization: Bearer <key>` |
| `MIX2API_API_KEY_REALM` | empty | Optional; binds the bootstrap key to one exit |
| `ACCOUNTS_DIR` | `accounts` | Account and settings directory |
| `USAGE_DIR` | `usage` | Usage and log directory |
| `WB_MEDIA_DIR` | `media` | Media task directory |
| `TZ` | system timezone | Local timezone used by the scheduler; setting it explicitly is recommended |

`PANEL_PASSWORD` and `MIX2API_API_KEY` are separate credentials: the former only
opens the dashboard, the latter only calls `/v1`. Use random values in
production and never put real values in the README, compose files, logs, or Git.

### First use

1. Open `http://127.0.0.1:58675/`, sign in with the panel password, and change
   it immediately after the first login.
2. Enable the exits you need in Settings, then import accounts in the matching
   view:
   - WorkBuddy: follow the dashboard OAuth flow;
   - CodeArts（遥遥领先）: Huawei Cloud OAuth;
   - Raccoon（商汤小浣熊）: import the desktop client's `auth.json` or use the
     loopback OAuth flow.
3. Create an API key in Settings and bind it to an exit/model as needed.
4. Verify with `/v1/models` and a minimal chat request:

```sh
curl -fsS http://127.0.0.1:58675/v1/models \
  -H "Authorization: Bearer $MIX2API_API_KEY"
```

### Reverse proxy and security

- Keep mix2api listening on `127.0.0.1` and let Caddy/Nginx provide HTTPS; do
  not expose the panel and `/v1` directly to the public internet.
- The management panel uses `PANEL_PASSWORD`; API clients use
  `MIX2API_API_KEY`.
- Never commit `accounts/`, `usage/`, `media/`, `.env`, or account JSON files.
- Logs and error messages are redacted by default; still restrict access at the
  reverse-proxy layer and cap request body sizes.
- Account risk controls, rate limits, and bans are the user's responsibility.

### Upgrade, migration, and rollback

- **v2 patch upgrade**: back up the data directory, then replace the binary or
  pull the new image. Keep the same three data volumes when recreating the
  container.
- **Migrate from v1**: stop v1 first and use the copy-only migration; the source
  directory is never modified:

  ```sh
  mix2api migrate --from <v1-data-dir> --to <v2-data-dir> --dry-run
  mix2api migrate --from <v1-data-dir> --to <v2-data-dir>
  ```

- **Rollback**: use `mix2api rollback` to produce a fresh directory readable by
  v1. Never point a running v1 and v2 at the same directory. See
  [`docs/migration.md`](docs/migration.md) for all options.
- The update executor's backup, verification, and recovery flow is documented in
  [`docs/update-executor.md`](docs/update-executor.md) and
  [`docs/runbook.md`](docs/runbook.md).

## Maintenance and releases

- `v2` is the default development branch; `main` only keeps the frozen v1.6.6
  history and no longer accepts features.
- Version identity, build metadata, and release assets are managed by
  `internal/core/version`, `make dist`, and
  [`.github/workflows/release.yml`](.github/workflows/release.yml). The image is
  built from the root `Dockerfile`.
- Check the built binary identity:

  ```sh
  ./bin/mix2api version --json
  ```

- Local checks:

  ```sh
  make check   # gofmt + vet + tests
  make race    # race detector
  make build   # build bin/mix2api
  ```

- See [`docs/release.md`](docs/release.md) for the release process and
  [`docs/gate-f.md`](docs/gate-f.md) for the release gate and known runtime
  gaps.

## Documentation

- [Packaging and deployment assets](docs/packaging.md)
- [Migration and rollback](docs/migration.md)
- [Update executor](docs/update-executor.md)
- [Operations runbook](docs/runbook.md)
- [Release process](docs/release.md)
- [Gate F](docs/gate-f.md)
- [Security policy](SECURITY.md)
- [Changelog](CHANGELOG.md)

## Scope and license

This project is for protocol research, study, and personal use only. Prefer the
official clients, do not use a primary account, and do not share or resell
accounts. Risk controls, rate limits, or bans caused by unofficial clients are
the user's responsibility.

MIT License. See [`LICENSE`](LICENSE).