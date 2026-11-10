# Update executor contract

The gateway process is unprivileged. It discovers releases and queues one
validated request; a deployment-owned executor performs any download, backup,
file replacement, service stop/start, or restart. The gateway never runs
`systemctl`, replaces application files, or restarts itself from these routes.

## Detection

- **systemd**: `WB_UPDATE_INSTALL_ENABLED=1`, the request directory exists,
  `/etc/systemd/system/workbuddy2api-hub-updater.path` exists, and its
  `multi-user.target.wants` link is enabled.
- **external**: `WB_UPDATE_INSTALL_ENABLED=1`, the request directory exists, and
  either `WB_UPDATE_EXECUTOR=external` or `<request-dir>/executor.json` exists.
- **portable**: `WB_UPDATE_PORTABLE=1` or `<request-dir>/portable.json` exists.
  The panel may report the capability; portable replacement remains a later
  plan.
- Anything else is `none`; install/rollback requests are refused.

## Request file

`<request-dir>/request.json` is create-only, mode `0600`, and written with
`O_EXCL|O_NOFOLLOW` followed by `fsync`. It is never overwritten. Fields:

```json
{
  "action": "install|rollback",
  "version": "1.2.3",
  "tag": "v1.2.3",
  "requested_at": "2026-01-02 03:04:05",
  "pid": 1234,
  "port": 8788,
  "root": "/srv/mix2api",
  "profile": "app"
}
```

- `version` has no leading `v`; `tag` is a stable `vX.Y.Z`.
- `profile` is optional and only describes an app-layer deployment.
- Rollback keeps `tag: "v<version>"`. Older executors that ignore `action`
  therefore treat the request as reinstalling that tag, which reaches the same
  version.
- The executor must treat an existing request as authoritative and must not
  overwrite it. It should remove the request only after it has accepted it.

## Status file

`<request-dir>/status.json` is a regular file, at most 8192 bytes, mode without
group/world write bits, owned by root (or the current owner only when
`WB_UPDATE_ALLOW_CURRENT_OWNER=1`). Symlinks are refused. Fields:

```json
{"state": "idle", "version": "", "message": "", "updated_at": "2026-01-02 03:04:05"}
```

Visible states: `idle`, `queued`, `downloading`, `verifying`, `backing_up`,
`stopping`, `applying`, `restoring`, `restarting`, `checking`, `succeeded`,
`restored`, `rolling_back`, `rolled_back`, `failed`.

Active states: `queued`, `downloading`, `verifying`, `backing_up`, `stopping`,
`applying`, `restoring`, `restarting`, `checking`, `rolling_back`.

If a request file exists while status is not active, the panel reports
`queued`. Unknown/malformed status is treated as unavailable/idle, never as a
successful install.

## Backups inventory

`<request-dir>/backups.json` has the same ownership/mode/size checks (65536
bytes). It contains at most eight rows and no paths:

```json
{"backups": [
  {"version": "1.2.2", "created_at": "2026-01-02 03:04:05",
   "files": 42, "complete": true, "size_mb": 20}
]}
```

A rollback is queued only when a row is `complete: true` and its version is a
stable `X.Y.Z` different from the running version. Incomplete or arbitrary
versions are rejected with HTTP 409. The panel payload contains versions,
sizes, times and the source (`systemd` or `local`), never filesystem paths.

## Idempotency and safety

- The executor must re-validate `action`, stable tag/version, the fixed
  repository, and the target backup before changing anything.
- One request at a time. The gateway refuses a second request while
  `request.json` exists or status is active.
- All writes are atomic and must preserve the running service if any step
  fails. Backups are taken before application files are replaced.
- The gateway's panel routes are read/queue only: `GET /updates`,
  `GET /updates/backups`, `POST /updates/check`, `POST /updates/install`,
  `POST /updates/rollback`.
