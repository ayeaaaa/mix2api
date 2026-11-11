# Operations runbook

## First deployment

### Native binary

```sh
tar -xzf mix2api-2.0.0-linux-amd64.tar.gz
cd mix2api-2.0.0-linux-amd64
sudo useradd --system --home-dir /srv/mix2api --shell /usr/sbin/nologin mix2api
sudo install -d -o mix2api -g mix2api -m 0750 /srv/mix2api
sudo install -o mix2api -g mix2api -m 0755 mix2api /srv/mix2api/mix2api
sudo install -o mix2api -g mix2api -m 0644 LICENSE README.md release-manifest.json mix2api.service /srv/mix2api/
sudo install -d -o mix2api -g mix2api -m 0700 /srv/mix2api/accounts /srv/mix2api/usage /srv/mix2api/media
```

Set `PANEL_PASSWORD` and `MIX2API_API_KEY` in `/etc/mix2api/env` (mode 0600,
readable only by root; systemd reads it before dropping privileges). A
non-loopback `HOST` refuses to start without both. A hardened unit is provided
at the `mix2api.service` file shipped in the release archive:

```sh
sudo install -m 0644 mix2api.service /etc/systemd/system/mix2api.service
sudo systemctl daemon-reload
sudo systemctl enable --now mix2api
```

The unit runs the binary and all data from `/srv/mix2api` as the `mix2api`
user, keeps the rest of the filesystem read-only, and only permits writes under
`/srv/mix2api`. For the privileged update executor, run `mix2api update run` as
`mix2api` (the owner of the install directory), not through the gateway unit.
If the update hook needs `systemctl`, grant that command through a reviewed
wrapper or run the executor from a root-owned path unit.

### Docker

```sh
cp .env.example .env
mkdir -p data/accounts data/usage data/media
sudo chown -R 10001:10001 data
docker compose up -d --build
curl -fsS http://127.0.0.1:58675/health
```

### OpenWrt

See `packaging/openwrt/README.md`; keep the listener on loopback and expose it
through a reverse proxy.

## Migrate v1.6.5 data

```sh
# Stop v1 first.
mix2api migrate --from /srv/mix2api-v1/data --to /srv/mix2api-v2/data --dry-run
mix2api migrate --from /srv/mix2api-v1/data --to /srv/mix2api-v2/data
mix2api migrate --from /srv/mix2api-v1/data --to /srv/mix2api-v2/data   # refuses reuse
```

Keep the receipt (`<to>.migration.json`). Start v2 with `ACCOUNTS_DIR`,
`USAGE_DIR`, and `WB_MEDIA_DIR` pointing into the new tree. The source remains
untouched.

## Update safely

1. The panel queues a request only when an executor is registered.
2. Run the executor as the install-directory owner:

```sh
mix2api update run \
  --request-dir /run/workbuddy2api-hub-updater \
  --root /srv/mix2api \
  --stop-command /usr/bin/systemctl --stop-arg stop --stop-arg mix2api \
  --start-command /usr/bin/systemctl --start-arg start --start-arg mix2api
```

3. Confirm `/updates` shows `succeeded` and `/updates/backups` lists the
   previous version as complete.
4. If the process crashed mid-apply, run `mix2api update recover --root ...`;
   it restores every journaled file and never leaves a half-applied binary.

Full contract and Windows notes: `docs/update-executor.md`.

## Roll back

### Application version

Use the panel rollback request, or:

```sh
# The normal path is the panel's rollback request plus `mix2api update run`.
# Direct execution must repeat the stop/start hooks:
mix2api update rollback --root /srv/mix2api --version <previous-version> \
  --stop-command /usr/bin/systemctl --stop-arg stop --stop-arg mix2api \
  --start-command /usr/bin/systemctl --start-arg start --start-arg mix2api
```

This verifies the complete backup, stops the service, restores files, removes
files introduced by the newer release, and starts the service. A failed start
returns an error and leaves the backup intact for another attempt.

### Data format

```sh
mix2api rollback --from /srv/mix2api-v2/data --to /srv/mix2api-v1-restored
```

Point v1.6.5 at the restored directory; never at the live v2 directory.

## Health and capacity

- `/health` always answers 200; inspect `providers` and update it through the
  panel. Authorized callers see account readiness.
- Watch `usage.jsonl` growth and the panel log caps (`log_db_max_rows`,
  `log_db_max_days`, `log_db_max_bytes`).
- Run `tests/load` against a staging fake upstream before changing concurrency
  limits; see `tests/load/README.md`.
- Backups are under `<root>/.mix2api-backups` (mode 0700, newest three kept).
  Copy them off-host if the install directory is not durable.

## Incident playbook

### Update failed

- Check `<request-dir>/status.json` and the executor log.
- Check `<root>/.mix2api-update-journal.json`. If present, stop the service and
  run `mix2api update recover`.
- Restore the listed backup with `mix2api update rollback`.
- If the binary is gone, extract the previous release archive manually, restore
  mode 0755, run `mix2api version`, then start.

### Service will not start

- Run `mix2api version` (validates the binary) and then `mix2api` in the
  foreground with the deployment environment.
- Check permissions on `accounts/`, `usage/`, `media/`; the service must own
  them.
- A non-loopback bind without both credentials fails by design.

### Data corruption suspicion

- Stop v2 immediately.
- `mix2api migrate --from <v2-data> --to <new-dir> --dry-run` validates JSON,
  JSONL, and SQLite. Do not copy over the suspect directory.
- Restore from the v1 source or a backup; keep the suspect tree for analysis.

### Lost panel password

- Stop the service, back up `accounts/settings.json`, and set
  `PANEL_PASSWORD` on next start only if the panel password is still the
  fresh-install default. A changed password is not overwritten by the
  environment. Use the documented recovery procedure for the deployment rather
  than editing hashes blindly.

### Suspected secret leak

- Rotate provider credentials at the provider, panel password, and
  `MIX2API_API_KEY`.
- Remove affected files from backups and reports.
- Follow `SECURITY.md`.

## Release checklist

Use `docs/gate-f.md`; do not tag or publish while any row is PENDING.
