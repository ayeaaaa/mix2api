# Real-archive update install/rollback smoke — 2026-11-11

Scope: local temp install root, real release archive, no live service. This
validates the archive/checksum/manifest/backup/journal/rollback path; it does
not replace a real systemd or Windows service run.

## Setup

- Temp root initially reported `2.0.0-rc.0` (manifest only, for a valid older
  install identity).
- Install archive built locally from the current source as stable `2.0.0`:
  `dist/mix2api-2.0.0-linux-amd64.tar.gz`
- `SHA256SUMS` and `release-manifest.json` used by the executor.
- No stop/start service hook was supplied.

## Results

- `mix2api update install --root <temp> --version 2.0.0 --archive ... --checksums ...`
  - result: `succeeded 2.0.0: 更新完成`
  - manifest after install: `2.0.0`
  - backup created: `2.0.0-rc.0-<timestamp>/backup.json`
- `mix2api update rollback --root <temp> --version 2.0.0-rc.0`
  - result: `rolled_back 2.0.0-rc.0: 回退完成`
  - manifest after rollback: `2.0.0-rc.0`
  - binary restored with expected mode/size.

## Gate F status

This closes the **real archive + local controller install/rollback** path.
Still pending:

- real systemd service stop/start execution
- real Windows service/hook execution
- GitHub Actions dry-run
- multi-arch GHCR push and arm64 container run
