# Local registry multi-arch push/pull — 2026-11-11

Scope: local Docker registry (`registry:2`, loopback port 5000), real multi-arch
image push and arm64 pull/run. No GHCR login or push was performed.

## Push

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t localhost:5000/mix2api:2.0.0-rc.1 \
  --build-arg VERSION=2.0.0-rc.1 \
  --build-arg COMMIT=18d811f882edb599407aeee41de702d1d3851f7f \
  --push .
```

Result:

- index digest: `sha256:2e0f81cbb02f2e3bc3e18ce977958f778827e55c0f977aa1e868bac457578848`
- `linux/amd64` manifest: `sha256:1b95e2c0293d42d2f235b5f794bb8e957c080917097c8e1816937aa6f24465fb`
- `linux/arm64` manifest: `sha256:32ab13eb828be8de8ed5a62f42ca459f6d1596f94d9c88ac17e673f6e9371a52`
- `docker buildx imagetools inspect` reported an OCI image index with both
  platforms.

## Pull and run

- `docker pull --platform linux/arm64 localhost:5000/mix2api:2.0.0-rc.1`
  succeeded.
- Container `uname -m`: `aarch64`.
- ELF header: `e_machine=0x00b7` (AArch64).
- `mix2api version`: `go1.22.12 linux/arm64`, commit `18d811f`.
- Container user: `10001:10001`.
- Docker healthcheck: `healthy`.
- `/health`: `ok=true`, version `2.0.0-rc.1`.

The local registry container was removed after the test.

## Remaining

Actual GHCR push, registry authentication, and pulling the published image from
GHCR still require explicit release approval and credentials.
