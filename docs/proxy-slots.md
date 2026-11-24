# Proxy slots

The panel's **Settings → 账号代理槽** section stores proxy exits in
`accounts/settings.json` and lets each provider account pin one slot. The
transport supports `http://`, `https://`, `socks5://`, and `socks5h://` URLs.
`NormalizeProxyURL` adds `http://` when the scheme is omitted and converts the
legacy `host:port:user:pass` form.

## Adding/editing a slot

Use the **添加代理槽** form above the table, or edit the structured columns
directly in a row:

- Protocol: HTTP / HTTPS / SOCKS5
- Proxy server: hostname or IP only (no scheme, port, or path)
- Port: 1–65535 (blank keeps the URL's default/omitted port)
- Authentication: separate username and password fields; the table masks the
  password with a password input
- Exit / enabled / bound accounts / test / delete remain per row

The panel stores the same normalized URL as before; the structured fields are
an editor over that URL, not a new backend schema. Click **高级** to view or edit
the raw URL for paths, queries, legacy forms, or special credentials. Raw URL
editing can show credentials in plain text. A URL with a path/query or an
unsupported format automatically opens the raw editor and locks the structured
fields.

A new slot is not persisted until **保存槽位** is clicked; clicking **测试** on
an unsaved row saves the whole dirty slot list first so it has an id (other dirty
rows are persisted too). Duplicate normalized URLs are rejected before saving.
After saving, bind accounts to it in the gateway account table or use
**一键分配代理**.

Do not add browser-style per-target fields such as 网址协议, direct-connect,
Proxy DNS, or SOCKS4: the slot is one gateway egress used for both HTTP and
HTTPS upstreams, SOCKS5 already resolves hostnames at the proxy, and the
transport does not implement SOCKS4.

If the gateway runs in Docker, `127.0.0.1` inside a slot refers to the gateway
container itself, not the host's proxy. Use `host.docker.internal`, a shared
compose service name such as `cli-proxy-mihomo`, or the host's LAN address.

## Test and discovery

**测试** performs a real request through the proxy to an exit-IP endpoint. The
defaults match the v1 gateway:

| Variable | Default | Purpose |
|---|---|---|
| `WB_PROXY_PROBE_URL` | `https://api.ipify.org` | Exit-IP response read through the proxy |
| `WB_PROXY_GEO_URL` | `http://ip-api.com/json` | Direct geo/ISP lookup for the exit IP |
| `WB_PROXY_DISCOVER_HOST` | `cli-proxy-mihomo` | Host used by **自动发现 mihomo 端口** |
| `WB_PROXY_DISCOVER_PORTS` | `17901-17910` | Port range or comma/space list, max 64 |

The probe result stores the exit IP and timestamp. When the geo lookup succeeds
it also stores country, country code, residential/datacenter kind, ISP and ASN;
the panel uses country + kind to auto-name unnamed slots. A geo lookup failure
does not fail the proxy probe and does not erase the last known country data.
The proxy probe only reports reachability; it does not guarantee model access,
cache hits, or upstream availability.

Discovery tests `http://<host>:<port>` for each configured mihomo port in
parallel and reports reachable and unreachable candidates; importing a candidate
still requires **保存槽位**.

## Failure semantics

- Unreachable proxy: HTTP 200 with `ok=false` and a sanitized error; the slot
  remains saved and can still be bound.
- Invalid/unsupported proxy URL: reported as a probe failure, not a save
  failure.
- Probe/discover unavailable (for example in an embedding that did not inject
  the network seams): `proxy probe unavailable` / an empty candidate list.

Proxy credentials are not logged. Probe errors strip URL userinfo before they
reach the panel.
