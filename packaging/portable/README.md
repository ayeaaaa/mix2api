# Portable mix2api

The portable archive contains one static binary with the dashboard embedded.
No Python, Node, or system SQLite is required.

## Unix

```sh
tar -xzf mix2api-2.0.0-portable-linux-amd64.tar.gz
cd mix2api-2.0.0-portable-linux-amd64
PANEL_PASSWORD='change-me' MIX2API_API_KEY='change-me-too' ./start-mix2api.sh
```

`start-mix2api.sh` daemonizes by default and writes `mix2api.pid` and
`mix2api.log` beside the binary. Set `MIX2API_FOREGROUND=1` to run under a
supervisor. `stop-mix2api.sh` only stops the PID from that file.

## Windows

Extract the zip and double-click `start-mix2api.cmd`, or run it from a shell
after setting `PANEL_PASSWORD` and `MIX2API_API_KEY`. `stop-mix2api.cmd` stops
`mix2api.exe`.

Data defaults to `accounts/`, `usage/`, and `media/` under the extracted
directory. Override `ACCOUNTS_DIR`, `USAGE_DIR`, and `WB_MEDIA_DIR`.

A non-loopback `HOST` requires both `PANEL_PASSWORD` and `MIX2API_API_KEY`.
Use the v1 migration command before first start:

```sh
./mix2api migrate --from /path/to/v1/data --to ./data --dry-run
./mix2api migrate --from /path/to/v1/data --to ./data
```
