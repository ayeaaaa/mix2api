# Security policy

## Supported versions

| Version | Status |
| --- | --- |
| `2.0.0` | current final release; security fixes accepted |
| v1.6.5 | upstream baseline, retained on `main` of this repository |
| older v2 snapshots | not supported |

`2.0.0` is the current final release. Gate F evidence and the remaining real
Windows SCM verification item are recorded in `docs/gate-f.md`.

## Reporting a vulnerability

Open a private security advisory on the repository or contact the maintainer
directly. Do not file a public issue with credentials, tokens, account UIDs, or
a working exploit. Include:

- affected version/commit from `mix2api version --json`;
- reproduction against fake upstream or redacted data;
- impact and any known mitigation.

## Secret handling

- Account files, `settings.json`, and status/backup inventory files are written
  with owner-only permissions where the component owns them.
- The gateway never logs credential values. Panel/API error paths are expected
  to stay redacted; tests in `internal/api` and `internal/core/secrets` guard
  this.
- Environment credentials (`PANEL_PASSWORD`, `MIX2API_API_KEY`) are bootstrap
  or request-time values; never commit `.env`.
- Migration copies data byte-for-byte. It does not encrypt an unencrypted v1
  tree; protect the destination with filesystem permissions.

## Update security model

The gateway process only queues one validated request. The privileged executor:

- accepts only `https://github.com/ayeaaaa/mix2api/releases/download/...`;
- requires `SHA256SUMS` to match the fixed asset name;
- validates `release-manifest.json` (format, version, stable tag, fixed
  repository, platform, per-file sha256);
- rejects traversal, symlinks, hardlinks, devices, setuid/setgid/sticky bits,
  oversized archives, and unlisted files;
- writes a complete backup and a transaction journal before replacing files;
- restores on failure and exposes `mix2api update recover` for crashes;
- never invokes a shell for stop/start hooks.

The request directory and install root must not be writable by untrusted users.
On Windows, replace only while the service is stopped; the executable is locked
while running.

## Deployment hardening

- Bind loopback and use a reverse proxy; a non-loopback bind requires both
  credentials.
- Run the container as UID/GID 10001; do not mount the application directory
  writable unless the update executor owns it.
- Use TLS at the proxy; the gateway speaks plain HTTP by design.
- Rotate panel/API/provider credentials after suspected exposure.
- Keep `accounts/`, `usage/`, and `media/` out of source control and off
  world-readable mounts.
- Verify release assets with `sha256sum -c SHA256SUMS` and the SBOM before
  deployment.

## Known limitations

- Real Windows SCM/service update execution is still unverified; the Windows
  binary and `.cmd` hooks were tested under Wine.
- The update executor trusts its operator-provided stop/start hooks.
- v1 and v2 must not share a live data directory.
- No security audit of the full v1 behavior surface has been performed.
