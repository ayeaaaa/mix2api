# Changelog

All notable changes to this project are documented here. The format follows
Keep a Changelog and the project uses semantic versioning.

## [Unreleased]

### 中文

#### 界面

- 看板统一为单一的新导航布局，移除「传统布局 / 控制台布局」切换；`/classic`
  仅保留为兼容静态别名，不再切换到旧壳。
- 侧栏导航按「工作台 / 资源 / 系统」分组，入口依次为总览、用量、账号与提供商、
  模型、自动化、媒体、智能体、设置、日志，减少同义页面和重复入口。
- 账号页改成提供商标签视图：WorkBuddy 国际、WorkBuddy 国内、CodeArts、小浣熊
  分开显示各自账号、凭证、积分、出口和操作，不再把不同提供商堆在同一屏。
- 设置页改成左侧分组导航：基础、提供商、模型与定价、路由与限额、自动化、
  稳定性、系统；每次只显示当前模块的设置，提供商接入不再混在通用 API 设置里。
- 用量页恢复控制台版数据看板形态：保留「本页导航」侧栏、区间选择、KPI、
  Token 时序、最近请求、工作回溯和用量明细；移动端收起页内导航，区间选择
  置顶，KPI 保持两列。内容区不再重复显示「数据看板」标题，只保留顶栏标题和
  区间摘要。
- 去掉账号页多余的区块导航和重复标题，统一卡片、表格、工具栏和空状态间距。
- 修复手机端左侧导航遮罩被基础 `display:none` 规则覆盖的问题：菜单打开后
  点击非菜单区域会关闭侧栏。
- 修复左上角菜单图标被按钮默认 padding 压缩成小点的问题：图标按钮改为无
  padding，并使用更清晰的列表/菜单图标。

### English

#### Interface

- The dashboard now uses one navigation layout. The classic/console layout
  switch and its menu entries were removed; `/classic` remains only as a
  byte-compatible alias and no longer switches shells.
- The sidebar is grouped into Workspace, Resources, and System: Overview,
  Usage, Accounts & providers, Models, Automation, Media, Agents, Settings,
  Logs.
- Accounts are provider-scoped tabs: WorkBuddy International, WorkBuddy China,
  CodeArts, and Raccoon show their own accounts, credentials, credits, exits,
  and actions instead of stacking every provider on one screen.
- Settings use a left module rail: Basics, Providers, Models & pricing, Routing
  & limits, Automation, Stability, System. Only the selected module is shown,
  and provider setup is no longer mixed into generic API settings.
- The Usage page restores the console data-dashboard shape: a page-level
  navigation rail, range selector, KPIs, token timeline, recent requests, work
  history, and usage detail; mobile hides the page rail, moves the range
  selector above the KPI grid, and keeps two columns. The content area no longer
  repeats the Data dashboard heading; only the top bar title and range summary
  remain.
- Removed the extra accounts section navigation and duplicate headings, and
  normalized card, table, toolbar, and empty-state spacing.
- Fixed the mobile sidebar backdrop being hidden by the base `display:none`
  rule; tapping outside the open menu now closes it.
- Fixed the top-left menu icon being squeezed into a dot by the button's default
  padding; icon buttons now use zero padding and a clearer list/menu glyph.

## [2.2.0] - 2026-11-12

### 中文

#### 新增

- WorkBuddy 国际版 / 国内版新增可选 uTLS 出站指纹桥：进程内 loopback
  CONNECT bridge 复刻官方 Node/OpenSSL ClientHello（18 个 cipher、10 个
  extension、无 ALPN），JA3 / JA4 与抓包值完全一致。
- 通过 `MIX2API_TLS_FINGERPRINT=node`（兼容 `WB_TLS_FINGERPRINT=node`）启用，
  默认关闭；只影响 WorkBuddy 的 HTTPS 出站，CodeArts / Raccoon 继续使用 Go
  原生 TLS。
- 代理槽支持 HTTP / HTTPS / SOCKS5；bridge 只监听 loopback，使用随机 secret
  认证，并继续校验上游证书。
- 构建工具链升至 Go 1.26，uTLS 升至 1.8.2，并升级 brotli / compress /
  x/net / x/crypto / x/sys；`govulncheck` 复扫为 0 漏洞。

#### 解决的问题

- Go `crypto/tls` 的 ClientHello 与 WorkBuddy 官方客户端不同，HTTP 层即使
  完全一致，仍可能在 TLS 指纹层被区分。
- 原有传输路径没有可切换的 TLS 指纹能力，无法在不改业务代码的情况下验证
  上游对指纹的响应。

#### 结果

- 启用后 WorkBuddy HTTPS 请求使用精确的 Node/OpenSSL ClientHello，HTTP/1.1、
  keep-alive、SSE、代理槽和证书校验行为保持不变。
- 未启用或使用不支持的模式时保持原行为；不支持的代理 scheme 会明确失败，
  不会静默回退。
- 新增 ClientHello JA3/JA4、未认证 CONNECT、直连、HTTP / HTTPS / SOCKS5
  代理、keep-alive、证书失败和真实上游握手测试；全量 `go test ./...` 通过。
- 使用 WorkBuddy 国内版真实账号完成非流式 chat 与 SSE 流式 smoke，均返回
  200；smoke 使用复制数据目录，未修改生产数据。

### English

#### Added

- Add an optional uTLS outbound fingerprint bridge for WorkBuddy International
  and China. The in-process loopback CONNECT bridge reproduces the official
  Node/OpenSSL ClientHello (18 ciphers, 10 extensions, no ALPN), matching the
  captured JA3 / JA4 exactly.
- Enable it with `MIX2API_TLS_FINGERPRINT=node` (`WB_TLS_FINGERPRINT=node` is
  accepted as an alias). It is off by default and only affects WorkBuddy HTTPS
  upstreams; CodeArts and Raccoon keep Go's native TLS.
- Proxy slots support HTTP, HTTPS, and SOCKS5. The bridge listens only on
  loopback, authenticates with a random secret, and still verifies upstream
  certificates.
- The build baseline moves to Go 1.26, uTLS to 1.8.2, and brotli / compress /
  x/net / x/crypto / x/sys to their fixed versions; a fresh `govulncheck` scan
  reports 0 vulnerabilities.

#### Problems fixed

- Go `crypto/tls` presents a different ClientHello from the official WorkBuddy
  client, so matching HTTP headers alone could still be distinguished at the
  TLS fingerprint layer.
- The previous transport had no switchable TLS fingerprint path, making it
  impossible to validate upstream behavior without changing business code.

#### Result

- With the switch enabled, WorkBuddy HTTPS requests use the exact Node/OpenSSL
  ClientHello while HTTP/1.1, keep-alive, SSE, proxy slots, and certificate
  verification stay unchanged.
- The default path is unchanged; unsupported proxy schemes fail explicitly
  instead of silently falling back.
- Added ClientHello JA3/JA4, unauthenticated CONNECT, direct, HTTP / HTTPS /
  SOCKS5 proxy, keep-alive, certificate-failure, and live upstream handshake
  tests; the full `go test ./...` suite passes.
- A real WorkBuddy China account completed both non-streaming chat and SSE
  streaming smoke requests with HTTP 200; the smoke used a copied data
  directory and did not modify production data.

## [2.1.1] - 2026-11-12

### 中文

#### 修复的问题

- 看板切换国际版 / 国内版 / 全部时，usage cache 保存会删除其他 realm 的缓存
  条目，导致每次切换重新全量扫描 `usage.jsonl`；生产/备用端实测切换卡顿约
  6.5–17 秒。
- snapshot / series / analytics 在日志没有新增时仍会每 5 秒重写数 MB 的
  checkpoint，造成持续 CPU 和磁盘 IO。
- 部分模型没有走结构化 `tool_calls`，而是把 DSML / `<run_code>` 工具调用
  打印在正文里；Agent 客户端收不到调用，可能重复追问并让上下文持续变长。
- 带内 SSE `error` 可能被当成正常完成，失败流也可能被恢复成可执行的工具调用。
- CodeArts / Raccoon 的 legacy `functions` 在 allowlist 阶段被丢弃；
  `tool_choice=none` 时工具声明被清空，模型拿不到函数 schema。

#### 修复结果

- cache 按 realm 独立保存，切换视图不再互相清缓存；日志 offset 没有推进时
  不重写 checkpoint。
- Chat Completions 只恢复请求中已声明的打印式工具调用；真实结构化调用优先；
  未闭合、畸形、未声明的调用不恢复。
- 流内错误按失败处理：不补成功 finish，不从失败流中恢复工具调用。
- CodeArts / Raccoon 保留 legacy `functions` 并转换为标准 `tools`，同时保留
  `function_call` / `tool_choice=none` 语义；显式 `tools` 仍优先。

### English

#### Problems fixed

- Saving the usage cache for one realm deleted entries for the other realms, so
  switching between intl / cn / all forced a full `usage.jsonl` rescan
  (observed 6.5-17s stalls on production/backup).
- snapshot / series / analytics rewrote multi-megabyte checkpoints every five
  seconds even when the log had not advanced, causing sustained CPU and disk IO.
- Some models printed DSML / `<run_code>` tool calls in the message body instead
  of emitting structured `tool_calls`; agent clients saw no call and could ask
  again, growing the context.
- An in-band SSE `error` could be treated as a successful completion, and a
  failed stream could still be recovered into executable tool calls.
- CodeArts / Raccoon dropped legacy `functions` during the allowlist stage, and
  `tool_choice=none` cleared the tool declarations so the model received no
  function schema.

#### Result

- Caches are kept per realm, so switching views no longer clears the others;
  unchanged log offsets no longer rewrite checkpoints.
- Chat Completions only recovers printed tool calls declared by the request;
  real structured calls win, and unclosed, malformed or undeclared calls are
  rejected.
- In-band errors fail closed: no successful finish and no recovered calls.
- CodeArts / Raccoon preserve legacy `functions` by converting them to standard
  `tools`, and retain `function_call` / `tool_choice=none` semantics; explicit
  `tools` still take precedence.

## [2.1.0] - 2026-11-11

### 中文

#### 新增

- 设置页新增「品牌外观」：可自定义浏览器标签标题、侧栏品牌名和图标
  （favicon + 侧栏品牌图标），支持 http(s) 图标 URL 或不超过 512KB 的图片
  上传。设置保存在 `settings.json`，只影响看板显示；API、健康检查和更新检查
  里的项目名不变。

### English

#### Added

- Add a Settings → Branding section for the browser tab title, sidebar brand
  name and icon (favicon plus sidebar mark). It accepts an http(s) icon URL or
  an uploaded image up to 512KB. Values live in `settings.json` and affect the
  dashboard only; API, health and update-check project identity stay unchanged.

## [2.0.7] - 2026-11-11

### 中文

#### 性能

- 新增持久化 analytics 聚合表 `usage-rollup.sqlite3`：按天/出口/账号/模型/Key
  聚合请求、错误、Token、积分、成本、TTFT/速度/耗时。现有 analytics 缓存
  失效时优先读表，不再重新解析整份 JSONL；表按日志 offset 增量推进，缺失或
  损坏时安全回退到原扫描。本机 19.5k 行首次建表约 0.64 秒，后续读取约 3 毫秒。

### English

#### Performance

- Add a persistent analytics rollup (`usage-rollup.sqlite3`) keyed by
  day/realm/account/model/key, storing requests, errors, tokens, credits, cost,
  TTFT/speed/elapsed sums and max_at. When the analytics view cache is invalid,
  the rollup answers the query without reparsing the whole JSONL. It advances by
  log offset and safely falls back to the original scan if missing/corrupt. On a
  local 19.5k-row ledger, the first build takes about 0.64s and later reads take
  about 3ms.

## [2.0.6] - 2026-11-11

### 中文

#### 性能

- usage 聚合扫描改用紧凑的 `scanRow` 结构解码，避免每行创建 `json.Decoder`
  和大部分 interface boxing；保留 `Row` map 作为兼容边界。本机 19.5k 行全量
  `/usage` 从约 0.82 秒进一步降到约 0.60 秒。

### English

#### Performance

- Decode aggregation scans through a compact `scanRow` struct instead of a
  per-line `json.Decoder` into `map[string]any`, keeping `Row` as the
  compatibility boundary. A local 19.5k-row full `/usage` scan drops from about
  0.82s to about 0.60s.

## [2.0.5] - 2026-11-11

### 中文

#### 性能

- 修复全量用量扫描的主要 CPU 热点：pricing timeline 不再在每行定价时深拷贝；
  新增单次扫描复用的不可变 `RowPricer`，避免每行 `os.Stat`、锁和重复查表。
  本机 19.5k 行数据上，全量 `/usage` 从约 75.7 秒降到约 0.82 秒。

### English

#### Performance

- Remove the dominant full-scan CPU hotspot: per-row pricing no longer deep-copies
  the entire policy timeline. A per-scan immutable `RowPricer` reuses the
  timeline, policy, markup and override tables instead of statting and locking
  for every row. On a local 19.5k-row ledger, a full `/usage` scan drops from
  about 75.7s to about 0.82s.

## [2.0.4] - 2026-11-11

### 中文

#### 修复

- 修复跨天后用量视图重新全量扫描：analytics 的日对齐窗口在“今天”还没有任何
  用量、日桶尚未生成时，将缺失的当天视为空，不再回退扫描整份日志；迁移后的
  v1 series cache 在开放窗口下允许当前分钟/小时尚未产生桶，同样按空数据处理。

### English

#### Fixed

- Avoid a full usage-log rescan after local midnight: day-aligned analytics
  windows treat the current day's missing bucket as empty, and migrated v1
  series caches serve open windows whose current minute/hour has no rows yet
  instead of falling back to a cold scan.

## [2.0.3] - 2026-11-11

### 中文

#### 修复

- 修复迁移后用量缓存失效：`ValidateCachePosition` 改为按 size/offset/tail
  内容校验，不再依赖原文件 inode/path；v1 全局（`realm: null`）
  analytics/series checkpoint 仅由 all-realm 视图复用，具体 realm 仍走
  对应缓存或按 realm 过滤扫描；日对齐的 analytics 窗口复用 v1 日桶，避免
  重新全量扫描。

### English

#### Fixed

- Usage checkpoints survive a data migration: `ValidateCachePosition` now
  validates the size/offset/tail content instead of the original inode/path.
  v1 global (`realm: null`) analytics/series checkpoints are adopted by the
  all-realm view only; scoped realms still use an exact entry or a
  realm-filtered scan. Day-aligned analytics windows use the v1 per-day buckets
  instead of rescanning the full usage log.

## [2.0.2] - 2026-11-11

### 中文

本次补丁将 v1.6.6 的 Raccoon（商汤小浣熊）每日活跃打卡移植到 v2，并恢复
看板的总额度列。

#### 新增

- Raccoon（商汤小浣熊）每日活跃打卡：先读取当天 `daily_grant` 账单，已到账
  的账号不重复发送对话；需要时发送一次最小对话，最多轮询 6 秒确认 300 积分
  奖励，并按账号持久化状态。每天最多尝试两次、间隔 1 小时；开启
  `daily_active` 后默认在本地 09:00 / 21:00 执行，`daily_active_model`
  可覆盖打卡模型。
- 新增面板接口 `POST /raccoon/accounts/daily-active`。手动 `force` 可绕过
  每日次数/间隔限制，但不会重复打卡已到账账号。
- 看板新增 Raccoon 设置开关、每日活跃任务列、奖励卡和单账号打卡按钮。
- 看板顶栏恢复总额度列（可用 / 总积分 / 总额度）。

### English

Patch release adding the v1.6.6 Raccoon（商汤小浣熊） daily-activity check-in
to v2 and restoring the dashboard's total-quota column.

#### Added

- Raccoon daily-activity check-in: reads today's `daily_grant` bill first,
  sends one minimal chat only when needed, verifies the 300-point reward for up
  to six seconds, persists each account's state, and allows at most two
  attempts per day one hour apart. The opt-in `daily_active` setting runs at
  09:00 / 21:00 local; `daily_active_model` overrides the ping model.
- `POST /raccoon/accounts/daily-active` panel action. Manual `force` bypasses
  the daily attempt/interval limits but never re-pings an account whose reward
  already landed.
- Dashboard Raccoon settings toggle, daily-activity task column, reward card,
  and per-account check-in button.
- Dashboard top bar shows total quota beside total credits for both WorkBuddy
  realms (`可用 / 总积分 / 总额度`); the v2.0.1 asset only exposed total
  credits.

## [2.0.1] - 2026-11-11

### 中文

本次补丁聚焦上游协议对齐、账号安全和请求保真。

#### 新增

- `11140` 持久封禁：账号被上游判定封禁后，API 调度和自动任务立即停止，
  直到管理员手动重新启用；重启、token 刷新、重新导入和成功探测都不会清除
  该状态。
- 国际版 `deepseek-v4.1-flash` 出站别名映射到 `deepseek-v4.1-flash-sg`；
  目录、API Key 校验、响应和 `requested_model` 仍保留逻辑 ID。设置
  `WB_INTL_MODEL_ALIAS=0` 可关闭。
- DSH flow-map 支持 `.credentials.yaml` 的 `refs:`。
- 国内版 Kimi 支持 `$ref` / `$defs` 工具 schema 内联。
- 浏览器身份、Web 请求头和 ACP 请求形态对齐官方网页端（按账号浏览器画像、
  `X-Request-ID`、`session/new`、`set_mode`、`set_config_option`、完整
  prompt `_meta`、human pause）。

#### 修复

- CodeArts（遥遥领先）同账号模型 ID 大小写回退、仅 role 的 preflight 帧、
  SSE 积分耗尽停泊；手动停用在终态凭据错误后仍保留。
- 完成 `11151` 空文本防护，覆盖列表分片和嗅探出的字符串数组。
- 计费头清理只作用于 system/developer 消息；工具描述会被清洗；裸计费键和
  `11128` 正文不再触发旧改写。
- 旧式 reasoning 模型的推理镜像和 Responses 回放保真（相邻 reasoning 合并、
  内联 assistant reasoning/tool calls、工具边界清理）。
- Profile wrapper 兼容、桌面身份 `5.9.4` / CLI `2.165.0`、看板
  `X-Robots-Tag: noindex, nofollow`。

#### 验证

- `make check` 通过；v1 对齐套件和 v2 contract/oracle 套件保持全绿。

### English

Patch release focused on upstream protocol parity, account safety, and request
fidelity.

#### Added

- Persistent `11140` account bans: API dispatch and automatic tasks stop until
  an operator manually enables the account; restart, token refresh, re-import
  and successful probes do not clear it.
- Intl `deepseek-v4.1-flash` outbound alias to `deepseek-v4.1-flash-sg`; the
  logical id remains in the catalog, API-key checks, responses and
  `requested_model`. `WB_INTL_MODEL_ALIAS=0` disables it.
- DSH flow-map `refs:` support for `.credentials.yaml`.
- CN Kimi `$ref` / `$defs` tool-schema inlining.
- Browser identity, web headers and ACP request shape aligned with the official
  web client (per-account browser profiles, `X-Request-ID`, `session/new`,
  `set_mode`, `set_config_option`, full prompt `_meta`, human pause).

#### Fixed

- CodeArts model-ID casing fallback on the same account, role-only preflight
  frames, and SSE credit-exhaustion parking; manual disable survives terminal
  credential errors.
- Complete 11151 empty-text guard for list parts and sniffed string arrays.
- Billing-header cleanup is scoped to system/developer messages; tool
  descriptions are sanitized; bare billing keys and `11128` body text no longer
  get the old rewrites.
- Reasoning mirror for legacy-reasoning models and Responses replay fidelity
  (adjacent reasoning merge, inline assistant reasoning/tool calls, tool-boundary
  clearing).
- Profile-wrapper compatibility, desktop identity `5.9.4` / CLI `2.165.0`,
  dashboard `X-Robots-Tag: noindex, nofollow`.

#### Verification

- `make check` passes; v1 parity suites and the v2 contract/oracle suites remain
  green.

## [2.0.0] - 2026-11-11

### 中文

正式版。源码发布在 `ayeaaaa/mix2api` 的 `v2` 分支；当时 `main` 为
v1.6.5。所有者已批准正式发布，真实 Windows SCM 更新路径仍未验证，详见
`docs/gate-f.md`。

#### 修复

- Docker buildx 的 `linux/arm64` 构建继承自动 `TARGETARCH`，不再被 amd64
  默认值覆盖；arm64 镜像包含 AArch64 二进制，并在 QEMU 下通过健康检查。
- 发布身份统一为 `ayeaaaa/mix2api` 和 `ghcr.io/ayeaaaa/mix2api`；内嵌看板
  重新与 v1.6.5 字节一致，包括两个仓库链接。
- 最终 `2.0.0` 成为默认构建身份；compose 和打包示例使用 `v2.0.0` 标签。

#### 验证

- 真实 systemd 更新安装/回滚、真实归档更新、本地 registry 多架构推送/
  拉取、arm64 容器运行，以及本地 GitHub Actions `verify` dry-run。
- Windows amd64 二进制和 `.cmd` 更新钩子在 Wine 下验证；真实 Windows SCM
  执行仍待验证。
- `govulncheck`：无可达漏洞；一个不可达的 `x/sys` 模块发现记录在
  `docs/security-scan.md`。

### English

Final release. Source is published on branch `v2` of `ayeaaaa/mix2api`; `main`
remains v1.6.5. Owner approved the final release with the real Windows SCM
update path still unverified; see `docs/gate-f.md`.

#### Fixed

- Docker buildx `linux/arm64` builds now inherit the automatic `TARGETARCH`
  instead of shadowing it with an amd64 default; the arm64 image contains an
  AArch64 binary and passes the healthcheck under QEMU.
- Release identity is unified on `ayeaaaa/mix2api` and
  `ghcr.io/ayeaaaa/mix2api`; the embedded dashboard is byte-identical to v1.6.5
  again, including its two repository links.
- Final `2.0.0` is the default build identity; compose and packaging examples
  use the `v2.0.0` tag.

#### Verified

- Real systemd update install/rollback, real-archive update, local registry
  multi-arch push/pull, arm64 container runtime, and a local GitHub Actions
  `verify` dry-run.
- Windows amd64 binary and `.cmd` update hooks under Wine; real Windows SCM
  execution remains pending.
- `govulncheck`: no reachable vulnerabilities; one unreachable `x/sys` module
  finding is recorded in `docs/security-scan.md`.

## [2.0.0-rc.1] - 2026-11-10

Release candidate. **Not a final v2.0.0 release.** Gate F oracle, real-account
smoke, and CI workflow dry-run are still pending; see `docs/gate-f.md`.

### Added

- Go rewrite of the v1.6.5 gateway with a single CGO-free binary and embedded
  dashboard.
- Provider modules for WorkBuddy intl/cn, CodeArts, and Raccoon, including
  account pools, proxy slots, model catalogs, check-in/activity, credits,
  media, agents, probes, usage, logs, pricing, and update discovery.
- `mix2api version [--json|--short]` with linker-injected version, commit, and
  build date.
- Reproducible release tooling (`make dist`) for Linux amd64/arm64/armv7,
  Windows amd64, macOS amd64/arm64, OpenWrt amd64/arm64/armv7, and portable
  Unix/Windows archives, with `SHA256SUMS`, SPDX SBOM, and per-archive release
  manifests.
- Multi-stage Dockerfile (CGO=0, non-root UID 10001, healthcheck), compose
  examples for build/image deployments, `.env.example`, and deployment docs.
- Safe update executor (`mix2api update run/install/rollback/recover`) with
  fixed repository validation, checksum/manifest verification, complete
  backups, atomic replacement, transaction journal recovery, and stop/start
  hooks on Unix and Windows.
- Copy-only migration (`mix2api migrate`) and v1-compatible rollback
  (`mix2api rollback`) with JSON/JSONL/SQLite validation, source snapshots,
  destination locks, checksum receipts, dry-run, and atomic publish.
- Oracle harness for selected v1.6.5 outputs plus a dedicated-account smoke
  checklist and Gate F signoff template.
- 200-concurrent SSE load harness with first-byte percentiles, memory/CPU,
  usage rows, and SQLite log append evidence.
- Manual release workflow with explicit `publish` and `gate_f_signed` guards,
  multi-arch GHCR build, checksum verification, and release asset upload.

### Fixed

- v1.6.5 oracle parity: `/agents` picker semantics, `global_key_set`, and Codex
  path order; `/health` v1 readiness/media/identity fields; `/v1/models`
  `catalog` provenance; non-stream chat `elapsed_ms`/`first_chunk_at`; `/updates`
  null/empty defaults; persisted active-realm usage selection; and nonempty
  request logging for `/logs`.
- Update journal now records original hashes, existence, old paths, and
  completion state before and after each rename; recovery reconstructs the
  `.mix2api-old` sibling, including the exact original schema that omitted
  `old_path`/`existed`, persists verified progress per entry, and keeps the
  journal on mismatch.
- Final `2.0.0` is offered over a running `2.0.0-rc.N`; rollback to a retained
  RC backup is supported.
- ARMv7 GOARM spellings (`7`/`v7`/`armv7`) are compared as one family during
  update extraction, not only in the release helper.
- Migration rejects symlinked destination and receipt parents, treats fresh
  empty/truncated locks as live for a grace period, and keeps dry runs free of
  destination, lock, and receipt writes.
- Migration rejects symlink/real-path overlap, reclaims stale locks, stages the
  receipt before publishing, and leaves no destination when receipt staging
  fails.
- Rollback to a manifest-less version removes the newer manifest and
  synthesizes one when the backup version is known.
- Windows `.cmd`/`.bat` update hooks run through `cmd.exe /d /s /c` with the
  canonical quoted batch command line, so hook paths containing spaces stay
  intact; `.exe` hooks run directly.
- Release identity was unified on `ayeaaaa/mix2api-v2` for this candidate. The
  final `2.0.0` release moved the source, releases, compose examples, and
  updater to `ayeaaaa/mix2api` while keeping the dashboard byte-identical to
  v1.6.5.
- The release workflow rejects `v`-prefixed, leading-zero, build-metadata, or
  otherwise non-strict version inputs before building.
- `SOURCE_DATE_EPOCH` now fixes archive entry timestamps as documented, and an
  invalid value fails the build instead of silently choosing 1980.
- OpenWrt packages carry the target `PKGARCH`, append env vars through procd,
  and install the UCI config as `0600`.
- `SHA256SUMS` now covers the SBOM and release metadata in addition to every
  archive.

### Changed

- v2 defaults to the `2.0.0-rc.1` build identity; release builds override it
  with `-ldflags -X`.
- Packaging uses fixed archive timestamps/owners and sorted entries; fixed
  metadata builds are byte-identical.
- OpenWrt builds clamp the 100 GiB log-size cap to the platform `int` maximum
  so 32-bit armv7 compiles.

### Data compatibility

- Existing settings, account, state, usage, logs, pricing, media, agent, and
  update files keep their v1 shapes and unknown keys.
- `usage.jsonl` appends remain v1 JSONL; `logs.sqlite3` keeps the v1 schema.
- Migration copies data only; it never rewrites the source or converts values.
- Rollback produces a fresh v1-compatible directory; v1 and v2 never share a
  live directory.

### Security

- Release assets are checksum/manifest verified before update.
- Update extraction rejects traversal, symlinks, hardlinks, devices, special
  mode bits, oversized archives, and unlisted files.
- Update requests are create-only and validated; status/backup inventory files
  are bounded and owner-checked.
- Migration refuses symlinks/special files and detects source mutation during
  the copy.
- Added `SECURITY.md` with reporting and deployment hardening guidance.

### Known gaps

- Selected v1.6.5 oracle recordings have not been captured/committed.
- Real-account smoke on dedicated accounts has not been signed.
- Real service update install and Windows update hooks have not been exercised
  outside fake tests.
- v1.6.5 performance baseline comparison has not been recorded.
- The release workflow has not been dry-run in CI.

## [1.6.5] - upstream baseline

The Python v1.6.5 gateway remains the behavioral oracle and is not modified by
this project. See the original repository for its history.
