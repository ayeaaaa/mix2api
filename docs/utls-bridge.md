# WorkBuddy uTLS 出站指纹桥

## 背景

HTTP 头和 JSON 字段一致，仍然可能被上游在 TLS 层识别：Go `crypto/tls` 的
ClientHello 与 WorkBuddy 官方客户端不同。官方 chat 走的是 Node 主进程，不是
渲染器 Chromium，抓包得到的指纹是：

```text
JA3: 771,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-13-51-45-43,29-23-24,0
JA3 MD5: 71dc8c533dd919ae9f4963224a4ba8fd
JA4: t13d120a00_5d04281c6031_78e6aca7449b
```

特征：18 个 cipher、10 个 extension、**不带 ALPN**、groups 为
`X25519 / P-256 / P-384`、9 个 signature algorithm。国内 `www.workbuddy.cn`
和国际 `www.workbuddy.ai` 抓包结果相同。

## 实现

v2 使用一个**进程内 loopback CONNECT bridge**，不需要额外二进制或 sidecar：

```text
WorkBuddy provider
  → transport.Client
  → 127.0.0.1:<随机端口> CONNECT（随机 secret）
  → bridge 直连 / HTTP(S) / SOCKS5 代理
  → uTLS 精确 ClientHello
  → 明文 HTTP/1.1 双向转发
```

- `internal/core/transport/utlsbridge` 负责监听、认证、代理拨号和 uTLS 握手。
- 使用 uTLS 1.8.2 和 Go 1.26 构建基线；`govulncheck` 复扫为 0 漏洞。
- `transport.Client` 的 HTTPS 请求通过 `DialTLSContext` 进入 bridge；HTTP 请求
  保持原来的直连/代理路径。
- bridge 只监听 loopback，启动时生成 32 字节随机 secret；CONNECT 必须携带
  `X-Mix2API-Bridge-Auth`，使用常量时间比较。
- 上游证书仍由 Go 的系统信任库校验；没有跳过校验。
- bridge 返回的连接已经完成 TLS 握手，Go transport 只在其上发送 HTTP/1.1，
  因此 keep-alive、SSE 和现有连接池逻辑不变。

## 开关

默认关闭，行为与之前完全一致。修改环境变量后需要重启进程：

```sh
# 启用 WorkBuddy 国际版 / 国内版的 uTLS 指纹
MIX2API_TLS_FINGERPRINT=node

# 兼容 wb-hub 的旧变量名；两者同时设置时 MIX2API 优先
WB_TLS_FINGERPRINT=node
```

关闭：

```sh
MIX2API_TLS_FINGERPRINT=python
# 或 go / native / stdlib / 0 / off / false / no
```

只影响 WorkBuddy 国际版和国内版。CodeArts 和 Raccoon 继续使用 Go 原生 TLS；
它们不是同一个官方客户端，不能直接套用这份指纹。

## 代理槽

bridge 支持：

- 直连
- `http://` 代理（CONNECT）
- `https://` 代理（先 TLS 到代理，再 CONNECT）
- `socks5://` / `socks5h://` 代理

HTTPS 目标走 bridge 时，代理 URL 通过 loopback CONNECT 的
`X-Mix2API-Upstream-Proxy` 传给 bridge；代理凭据不会写入日志。HTTP 目标仍走
原有代理路径。

不支持的代理 scheme 会直接失败，不会静默回退到普通 TLS。

## 限制

- 固定无 ALPN，因此只使用 HTTP/1.1；这与官方 Node 客户端和现有 SSE 路径一致。
- 使用系统信任库；暂不支持 `WB_CA_BUNDLE` 这类自定义 CA。
- 开关是进程级环境变量，不支持看板热切换。
- 复刻抓包指纹只能减少 TLS 层差异，不构成免封保证。
- 不做订阅桥（subbridge）；该功能已明确拒绝。

## 验证

```sh
go test ./internal/core/transport/utlsbridge
```

测试会真实启动 bridge 并抓 ClientHello，断言：

- JA3 / JA3 MD5 / JA4 与抓包值完全一致
- TLS record 长度为 255 字节、session id 长度为 32、无 ALPN
- 未认证 CONNECT 返回 403
- 直连和 HTTP 代理下的 HTTPS 请求都能完成，且协议为 HTTP/1.1
- 不支持的代理 scheme 会失败

可选的真实上游握手验证：

```sh
MIX2API_UTLS_LIVE=1 go test ./internal/core/transport/utlsbridge \
  -run TestLiveWorkBuddyHandshake -count=1 -v
```

该测试只完成 TLS 握手和证书校验，不发送业务请求。

## 参考

- wb-hub 原实现：`tools/wb-tls-bridge/main.go`、`wb_upstream_pool.py`
- 指纹参考：`misakano7545/workbuddy2api-panel` 的
  `internal/upstream/tlsfingerprint.go`
- 本项目实现：`internal/core/transport/utlsbridge`

## English summary

The bridge is opt-in and WorkBuddy-only. `MIX2API_TLS_FINGERPRINT=node` (or the
`WB_TLS_FINGERPRINT` alias) starts an in-process loopback CONNECT bridge that
reproduces the captured Node/OpenSSL ClientHello (18 ciphers, 10 extensions, no
ALPN). HTTPS requests use the bridge; HTTP requests keep the existing proxy
path. HTTP, HTTPS, and SOCKS5 proxy slots are supported. The bridge listens only
on loopback, authenticates each CONNECT with a random secret, and still verifies
upstream certificates with the system trust store. The default is off; changing
the mode requires a restart. It reduces the TLS-layer difference but is not a
guarantee against account enforcement.
