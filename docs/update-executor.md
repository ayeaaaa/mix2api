# Running the update executor

The gateway only queues requests (`docs/update-executor-contract.md`). This
document covers the privileged command that consumes them.

## Requirements

- Run as the owner of the install directory. The packaged systemd unit owns
  `/srv/mix2api` as the unprivileged `mix2api` user, so run the executor as
  that user (or as root only when the deployment deliberately root-owns the
  tree). The executor never needs to run inside the gateway process.
- The install root contains `mix2api`/`mix2api.exe`, `LICENSE`, `README.md`,
  and `release-manifest.json`.
- The request directory is writable by the executor and by the gateway account
  (the gateway creates one create-only `request.json`).
- Stop/start hooks are optional on Unix; a running executable can be replaced
  atomically. On Windows a stop hook is required because the executable is
  locked while running.

## Commands

```sh
# Consume one queued request:
mix2api update run \
  --request-dir /run/workbuddy2api-hub-updater \
  --root /srv/mix2api \
  --stop-command /usr/bin/systemctl --stop-arg stop --stop-arg mix2api \
  --start-command /usr/bin/systemctl --start-arg start --start-arg mix2api

# Remove a journal left by a crash, restoring the previous files:
mix2api update recover --root /srv/mix2api --request-dir /run/workbuddy2api-hub-updater

# Offline install from a downloaded, checksummed release archive:
mix2api update install --root /srv/mix2api --version 2.0.0 \
  --archive ./mix2api-2.0.0-linux-amd64.tar.gz \
  --checksums ./SHA256SUMS

# Restore a retained backup directly:
mix2api update rollback --root /srv/mix2api --version 1.6.5
```

`--stop-command`/`--start-command` are executable paths plus fixed
`--stop-arg`/`--start-arg` values. No shell is involved. Point them at reviewed
wrapper scripts when shell syntax is needed.

## Safety properties

- Only `https://github.com/ayeaaaa/mix2api-v2/releases/download/vX.Y.Z/` URLs are
  constructed; redirects must stay on HTTPS GitHub hosts.
- `SHA256SUMS` must contain the exact asset name and match before extraction.
- The archive must have one top-level directory and a
  `release-manifest.json` matching the requested version, stable tag, fixed
  repository, kind `standard`, and this executor's GOOS/GOARCH/GOARM.
- Every listed file is checksum-verified; unlisted files, symlinks, hardlinks,
  devices, setuid/setgid/sticky bits, traversal, and oversized archives are
  rejected.
- A complete backup is written and published before the service stops. A
  `backup.json` is marked complete only after every copied file is hashed.
- Replacement uses same-directory temp files and renames. A journal is written
  before the first replacement; failures and crashes restore every file that
  was moved. Recovery also understands the original journal schema that omitted
  `old_path`/`existed` by using the `Target+".mix2api-old"` sibling, persists
  progress after each verified entry, and deletes the journal only when every
  entry verifies. Rollback verifies all backup hashes again before changing
  files.
- The executor removes `request.json` only after the request validates.
- Status and backup inventory files stay mode `0600`; status messages never
  contain credentials or absolute paths.

## Backups

Backups live under `<root>/.mix2api-backups` by default, one directory per
retained version, mode `0700`. The newest three **complete** backups are kept;
incomplete backups remain on disk for diagnosis. The panel inventory
is `<request-dir>/backups.json`; it contains versions, times, file counts,
sizes, and completeness, never paths.

Rollback is refused unless a complete backup with a stable `X.Y.Z` version
different from the running version exists. Incomplete backups remain on disk
for diagnosis but are never offered.

## Windows

Cross-build and run the same command:

```powershell
mix2api.exe update run --request-dir C:\ProgramData\mix2api\updates `
  --root C:\ProgramData\mix2api\app `
  --stop-command C:\ProgramData\mix2api\stop-service.cmd `
  --start-command C:\ProgramData\mix2api\start-service.cmd
```

Use `.exe` hooks directly, or `.cmd`/`.bat` wrappers. Windows cannot execute a
batch file through `CreateProcess`, so the executor runs `.cmd`/`.bat` through
`cmd.exe /d /s /c ""<fixed path>" <fixed args>"`; `.exe` hooks run directly.
The batch line is handed to CreateProcess verbatim, so a hook path containing
spaces stays one quoted token instead of being split by Go's argv quoting.
Arguments are fixed operator values, never request fields. Ensure hooks report
failure with a non-zero exit status so the executor can restore the previous
version.
