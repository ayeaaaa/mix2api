# Real systemd update/rollback smoke — 2026-11-11

Scope: a temporary systemd service (`mix2api-smoke`) running a v2 install root
under `/tmp/opencode/systemd-smoke`, with a real release archive and real
`systemctl stop/start` hooks. This validates the update executor's service
controller path on the local systemd host; the unit was removed after the test.

## Setup

- Install root initially reported `2.0.0-rc.0` (manifest identity) with the
  RC1 binary payload.
- Target archive: real `mix2api-2.0.0-linux-amd64.tar.gz`, SHA256SUMS entry.
- Hooks: `/usr/bin/systemctl stop mix2api-smoke` / `start mix2api-smoke`
  via repeated `--stop-arg`/`--start-arg`.
- Service was active and `/health` returned 200 before install.

## Results

- `update install --version 2.0.0`:
  - service stopped, files replaced, service started
  - result `succeeded 2.0.0: 更新完成`
  - `systemctl is-active`: `active`
  - `/health`: 200, version `2.0.0`
  - manifest: `2.0.0`
- `update rollback --version 2.0.0-rc.0`:
  - service stopped, backup restored, service started
  - result `rolled_back 2.0.0-rc.0: 回退完成`
  - `systemctl is-active`: `active`
  - `/health`: 200
  - manifest: `2.0.0-rc.0`
- `update recover` with no journal: `idle : 没有中断的更新`
- Test service unit was stopped, disabled, and removed; original v1 repo clean.

## Note

The executor requires explicit `stop`/`start` verbs for systemctl; the correct
invocation is:

```sh
--stop-command /usr/bin/systemctl --stop-arg stop --stop-arg mix2api
--start-command /usr/bin/systemctl --start-arg start --start-arg mix2api
```

The earlier documentation example omitted the verb and failed with
`Unknown command verb`.

## Gate F status

This closes the **real systemd service stop/start + real archive** path.
Windows service/hook execution, CI dry-run, and multi-arch container validation
remain pending.
