# Windows binary update/rollback under Wine — 2026-11-11

Scope: cross-compiled Windows amd64 binary, Windows release zip, and `.cmd`
stop/start hooks under Wine 9.0. This validates the Windows code path, archive
handling, batch-hook invocation, backup, and rollback. **It is not a real
Windows service/SCM test.**

## Setup

- Wine 9.0 (`wine64`), fresh `win64` prefix.
- Install root initially reported `2.0.0-rc.0` with the RC1 Windows binary.
- Target archive: locally built `mix2api-2.0.0-windows-amd64.zip`.
- Hooks: `stop-service.cmd` and `start-service.cmd`, each appending to
  `hooks.log` and exiting 0.

## Results

- `mix2api.exe version` (before): `windows/amd64`, `2.0.0-rc.1`.
- `update install --version 2.0.0`:
  - result `succeeded 2.0.0: 更新完成`
  - hooks log contained `stop` then `start`
  - `mix2api.exe version` after install: `2.0.0`
  - manifest after install: `2.0.0`
- `update rollback --version 2.0.0-rc.0`:
  - result `rolled_back 2.0.0-rc.0: 回退完成`
  - hooks log contained another `stop` then `start`
  - binary restored to `2.0.0-rc.1`
  - manifest after rollback: `2.0.0-rc.0`
- Windows server smoke under Wine:
  - started `mix2api.exe` on `127.0.0.1:58685`
  - `/health` returned `ok=true`, version `2.0.0-rc.1`
  - log: `listening on 127.0.0.1:58685 with 4 provider(s)`

## Limitation

Wine provides a Windows-compatible process environment, not the real Windows
Service Control Manager. The hooks are process wrappers, not `sc stop/start`
against a real service. Real Windows service/hook execution remains pending.
