# mix2api on OpenWrt

Use the `mix2api-<version>-openwrt-<arch>.tar.gz` asset for your router's CPU
(`amd64`, `arm64`, or `armv7`). The archive contains the static binary, the
procd init script, and a UCI configuration sample.

## Portable install

```sh
tar -xzf mix2api-2.0.0-openwrt-arm64.tar.gz
cd mix2api-2.0.0-openwrt-arm64
install -m 0755 mix2api /usr/bin/mix2api
install -m 0755 etc/init.d/mix2api /etc/init.d/mix2api
install -m 0600 etc/config/mix2api /etc/config/mix2api
/etc/init.d/mix2api enable
/etc/init.d/mix2api start
```

The service runs as root because procd and the `/srv/mix2api` data directories
are root-owned. `/etc/config/mix2api` is installed mode `0600` because it may
contain the panel password. The default listener is loopback
`127.0.0.1:58675`; expose it through your reverse proxy rather than binding the
router directly to the WAN. A non-loopback `HOST` requires both
`PANEL_PASSWORD` and `MIX2API_API_KEY`.

## SDK package

`packaging/openwrt/Makefile` builds a procd package around the prebuilt binary:

```sh
make package/mix2api/compile \
  MIX2API_BINARY=/path/to/mix2api-2.0.0-openwrt-arm64/mix2api V=s
```

Data directories are created under `/srv/mix2api`. Keep the v1.6.5 gateway
stopped before migrating data, and never point v1 and v2 at the same live
directory.
