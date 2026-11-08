# mix2api v2

Go rewrite of mix2api v1.6.5. This repository currently contains the foundation
and the Raccoon provider vertical slice; CodeArts, WorkBuddy, dashboard,
packaging, and the full v1 oracle suite are later plans.

## Build and run

```bash
CGO_ENABLED=0 go build -o mix2api ./cmd/mix2api
PANEL_PASSWORD='change-me' MIX2API_API_KEY='change-me-too' ./mix2api
```

- `PORT` defaults to `58675`.
- `HOST` defaults to `127.0.0.1`. A non-loopback `HOST` requires both
  `PANEL_PASSWORD` and `MIX2API_API_KEY`; startup fails otherwise.
- `/raccoon/*` panel routes accept `X-Panel-Token` or
  `Authorization: Bearer <PANEL_PASSWORD>`. They never use cookies.
- `/v1/*` accepts `Authorization: Bearer <MIX2API_API_KEY>`, `X-API-Key`, or
  the legacy query spellings.
- `ACCOUNTS_DIR` defaults to `accounts`, `USAGE_DIR` defaults to `usage`
  (the usage directory is not created until usage storage is implemented).

The command binds its listener before composing the API so provider OAuth
redirects use the real listener port, never the request `Host` header.

## Data compatibility

Account files, `state.json`, and `settings.json` retain unknown fields.
Raccoon `state.json` timestamps are written as numeric Unix seconds, the shape
v1.6.5 reads, and RFC3339/numeric legacy values are accepted on load. Keep v1
and v2 from sharing a live data directory: stop one process before starting the
other.

## Tests

```bash
go test ./...
go test -race ./...
go vet ./...
```

Contract goldens under `tests/contract/testdata` are v2 regression fixtures,
not a captured full v1.6.5 oracle; see `tests/contract/testdata/PROVENANCE.md`
and `tests/oracle/README.md`.

## License

MIT. See `LICENSE`.