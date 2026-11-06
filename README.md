# mix2api

一个 **Python 单进程** OpenAI 兼容网关：把 **WorkBuddy 国际版**、**WorkBuddy 国内版**、**华为云 CodeArts** 和 **商汤小浣熊（Raccoon）** 四个上游收进一个服务、一个看板、一套 API Key。

```text
客户端 ──► mix2api（单进程）──► WorkBuddy 国际版
                          ├──► WorkBuddy 国内版
                          ├──► 华为云 CodeArts（OAuth / 续期 / 模型 / 签到都在进程内）
                          └──► 商汤小浣熊 Raccoon（auth.json / 回环 OAuth / 模型 / SSE 都在进程内）
```

- **一个服务**：不再需要单独运行 codearts2api 或 Raccoon 的 Node 反代；CodeArts 的 OAuth、token 续期、账号池、模型发现、聊天 SSE 和签到，以及 Raccoon 的 auth.json / 回环 OAuth、续期、账号池、模型发现和聊天 SSE，都由本进程直接完成。
- **四个出口**：看板顶部切换 `国际版 / 国内版 / CodeArts / 小浣熊`；也可以给不同 API Key 各绑一个出口，四个同时用。
- **一个看板**：四个出口的账号都在这里增删、启停；CodeArts 账号用华为云 OAuth 添加，Raccoon 账号导入 `auth.json` 或走本机回环授权。
- **三类协议**：`/v1/chat/completions`、`/v1/responses`（Codex）、Anthropic Messages（需显式开启）都能走 CodeArts 和小浣熊。

## 项目由来

mix2api 是一个独立的单进程网关项目：把 WorkBuddy 国际版、WorkBuddy 国内版、华为云 CodeArts 和商汤小浣熊（Raccoon）四个上游收进一个服务、一个看板、一套 API Key。

上游 WorkBuddy2API-Hub 已于 2026-11-06 发布 v2.0.0 后停止维护；mix2api 自 v1.5.0 起接管代码与后续维护。

- **四个出口**：国际版、国内版、CodeArts、小浣熊共用同一个地址和看板，也可以按 API Key 绑定不同出口。
- **单进程**：CodeArts 的 OAuth、token 续期、模型发现、聊天 SSE、签到和积分查询都在 `app/` 进程内完成，不再需要独立的 Go 服务；Raccoon 的 auth.json / 回环 OAuth、续期、模型发现和聊天 SSE 也全部在 `app/` 进程内完成，不需要 Node sidecar。
- **代码关系**：`reference/codearts2api/` 只保留 Go 源码作协议参考，不参与构建和运行；Raccoon 为原生 Python 实现，参考桌面客户端协议与本地参考代理的实测行为，不复制第三方项目代码；上游基线与许可说明见 [`UPSTREAM.md`](UPSTREAM.md)。

## 这次做了什么

- **单进程 CodeArts 客户端**：华为云 OAuth2（PKCE + DPoP）、refresh_token 续期、账号池、并发/冷却、模型发现（agent-center / builtin / 限时福利）、聊天 SSE、签到、积分余额。
- **小浣熊第四出口（原生 Python）**：导入桌面客户端 `auth.json` 或本机回环 OAuth；多账号池、并发/冷却/失败切换、账号级代理槽、备注/任务开关/测试/启停/清冷却/删除、token 自动续期与模型目录刷新。不依赖 Node sidecar，也没有第三方依赖。
- **纯标准库密码学**：P-256 ECDSA、DPoP JWT、PKCE、华为云 `SDK-HMAC-SHA256` 签名；ECDSA 与 HMAC 都用 Go 侧向量对拍。
- **看板**：第三张 CodeArts、第四张小浣熊出口卡片、四出口切换、CodeArts / Raccoon 账号表、设置页、CodeArts 积分余额与已用/总额、模型描述。
- **账号代理槽**：CodeArts 与 Raccoon 账号可绑定与 WorkBuddy 相同的「账号代理槽」，账号表直接选出口；CodeArts 的聊天、模型发现、token 续期、签到和积分查询，以及 Raccoon 的聊天、模型发现和 token 续期都走该槽。出站复用 `wb_upstream_pool` 的连接复用、代理认证、`no_proxy` 和失效连接重试，槽位停用/删除回退直连。
- **协议与用量**：`/v1/chat/completions`、`/v1/responses`、Anthropic Messages 统一路由；CodeArts 用量记 `realm=codearts`、Raccoon 记 `realm=raccoon` 且 `cost_policy=raccoon:unpriced`，都不按 WorkBuddy 价目表计价。
- **部署**：单服务 compose，默认端口 `58675`；`make build / up / test / logs`。
- **测试**：284 个套件：212 个 Python + 72 个 JS 全过；CodeArts 链路用本机假华为云上游覆盖，Raccoon 链路用本机假小浣熊上游覆盖，密码学有 Go 对照向量。

> 本项目仅用于协议学习与个人自用；账号风控、限流、封禁风险由使用者自行承担。请优先使用官方客户端，不要使用主账号，不要共享或转售账号。

## 目录

| 路径 | 内容 |
|---|---|
| `app/` | 唯一的服务：Python 网关 + 看板（四出口） |
| `deploy/` | 单服务 compose |
| `docs/` | 设计、实施计划、部署说明 |
| `reference/codearts2api/` | Go 版 codearts2api 源码，**仅作协议移植参考，不构建、不运行** |

`app/` 与 `reference/codearts2api` 的上游基线、MIT 版权与维护状态见 [`UPSTREAM.md`](UPSTREAM.md)。

## 快速启动（Docker）

### 方式一：拉预构建镜像（推荐）

镜像已发布到 GHCR，支持 `linux/amd64` 与 `linux/arm64`：

```bash
cd deploy
docker compose -f docker-compose.image.yml up -d
```

或手动：

```bash
docker pull ghcr.io/ayeaaaa/mix2api:v1.6.0
docker run -d --name mix2api -p 58675:58675 \
  -v "$PWD/data/accounts:/app/accounts" \
  -v "$PWD/data/usage:/app/usage" \
  ghcr.io/ayeaaaa/mix2api:v1.6.0
```

### 发布产物

每个 [GitHub Release](https://github.com/ayeaaaa/mix2api/releases) 附带：

- `mix2api-<版本>-deploy.zip`：v1.2.0 起是带 Windows 精简运行时的便携/一键更新包；v1.0.x–v1.1.x 是源码包。
- `mix2api-<版本>-source.zip`：完整源码包，用于手动部署。
- `workbuddy2api_<版本>-<发布号>_all.ipk`：OpenWrt opkg 系安装包（24.10 及更早）。
- `workbuddy2api-<版本>-r<发布号>.apk`：OpenWrt 25.x+ 的 apk-tools 安装包。
- `SHA256SUMS`：上述文件的校验和。

OpenWrt 包名仍是 `workbuddy2api`，这样已有安装可以直接升级；版本从 v1.5.0 起跟 mix2api 对齐。

### 方式二：本地构建

```bash
cd deploy
docker compose up -d --build
```

打开 `http://127.0.0.1:58675/`：

1. 用面板密码登录（默认 `admin`，首次登录后立刻改）。
2. 进「设置 → CodeArts 接入」，勾选「启用 CodeArts 出口」并保存。不需要填 Base URL 或 API Key。
3. 切到 CodeArts 视图，点「+ 添加账号 (OAuth)」，用华为云账号完成授权。
   - 浏览器和网关在同一台机器：授权后回调 `127.0.0.1:<端口>/oauth/callback` 会自动完成。
   - 网关在远端：浏览器打不开 127.0.0.1 回调，把地址栏完整地址粘贴到弹窗的「远端回调」输入框，点「中转回调」。
4. 在「设置 → API Key 与出口绑定」里建一个 Key，出口选 `CodeArts`，或留空跟随顶部默认出口。
5. 要用小浣熊出口：进「设置 → 小浣熊接入」启用，切到小浣熊视图点「添加账号」，导入桌面客户端 `auth.json` 或完成本机回环授权（详见下文「Raccoon 说明」）。Key 的出口选 `小浣熊`，或留空跟随默认出口。

也可以：

```bash
make build   # 构建镜像
make up      # 启动
make test    # 跑全部测试
make logs    # 跟踪日志
```

## 本地开发

```bash
cd app
python3 wb_proxy.py --port 58675
```

CodeArts 账号在 `app/accounts/codearts/` 下，Raccoon 账号在 `app/accounts/raccoon/` 下，WorkBuddy 账号在 `app/accounts/` 下。三者互不干扰。

## CodeArts 说明

- 登录：华为云 CodeArts OAuth2（PKCE + DPoP），refresh_token 与 client_id、DPoP 私钥一起落盘。
- 续期：token 到期前 30 分钟自动刷新；refresh_token 终态失效时账号标记为「需要重新登录」。
- 模型：agent-center / builtin / 限时福利三路合并，福利模型聊天时自动加 `maas_type: benefit`。
- 聊天：`/api/v2/chat/completions`，AK/SK `SDK-HMAC-SHA256` 签名 + `x-auth-token`。
- 签到：`/v1/ops/delivery|claim|confirm`；看板「签到 / 全员签到」可手动触发，「设置 → CodeArts 接入」的「每天自动签到」默认开启，跟随国内版签到时段（默认 09:00 / 21:00），只跑已启用且未冷却的账号，当天成功后不重复。
- 积分：`snap-manager/v1/statistics/plugin` 查询剩余 / 总额 / 已用；单次请求的积分扣减上游不返回，最近请求表积分列显示 `0.00`、估算价格显示 `—`。
- 代理：CodeArts 账号可绑定「账号代理槽」，出站请求与 WorkBuddy 共用 `wb_upstream_pool`（连接复用、代理认证、`no_proxy`、失效连接重试）；槽位停用/删除回退直连，首次 OAuth 换 token 仍直连。
- 图片：`/v1/models` 按 CodeArts 上游的 `supports_images` 宣告视觉能力，并输出 WorkBuddy 同款 `capabilities.vision` / `input_modalities` 等字段。当前 `Qwen3-VL-235B`、`kimi-k2.6-vl` 为 true；`GLM-5.2`、`kimi-k3`、`deepseek-v4.1-flash` 等为 false，不会向客户端误报可读图。视觉模型支持 OpenAI `image_url`、Responses `input_image`、Anthropic `image` 块；data URL 已实测，远程 URL 原样透传（能否抓取取决于上游），`image_url.detail` 和 tool_result 里的图片分片会保留。
- 模型命名：CodeArts 对外使用原版模型名（如 `GLM-5.2`、`deepseek-v4.1-flash`）。当前按 Key/面板出口选择，单一出口下不需要提供商前缀；以后同时支持多出口时再加 `provider/` 前缀区分。
- 推理档位：`deepseek-v4.1-flash` / `-1017` 支持 `none / low / medium / high / xhigh / max`；`GLM-5.2` / `GLM-5.2-SFT-Harmony` / `GLM-5.3` 支持 `none / minimal / low / medium / high / xhigh / max`；`glm-5.3-flash` 只支持 `low / high / max` 且始终思考。`/v1/models` 会宣告 `reasoning_efforts`；`auto` 或未知值在本地直接 400，并列出允许值。
- 模型元数据：`/v1/models` 输出 `context_length`、`max_input_tokens`、`max_output_tokens`、`max_completion_tokens`、`max_output` 和工具能力字段；上游没给上下文/输出上限时，用同名 WorkBuddy 目录兜底。
- 请求兼容：chat 透传 `stop`、`response_format`、`frequency_penalty`、`presence_penalty`、`seed`、`top_k`、`parallel_tool_calls`（上游已实测接受）；`thinking` / `enable_thinking` / camelCase `reasoningEffort` 会归一化成 `reasoning_effort`。
- 调度：CodeArts 每 30 分钟检查一次 Token 并续期快过期账号；22:00 仍随 WorkBuddy 巡检。自动签到跟随签到时段。CodeArts 账号表新增备注、任务开关（API 停用但继续签到）、单账号测试、重新登录和刷新积分。
- 错误语义：非视觉模型收到图片返回 400，并提示可用视觉模型；未知参数/图片错误不再统一吞成 502。
- 活动历史：CodeArts 签到写入 `/activity/history`，可按区域筛选。
- 限制：CodeArts 账号不参与 WorkBuddy 的积分/额度护栏和跨出口回退；CodeArts 账号由本进程的 CodeArts 池轮转，不能按 Key 指定；代理槽按 HTTP/HTTPS 代理实现。

## Raccoon（小浣熊）说明

Raccoon 是第四个出口，完全在本进程内运行，不需要单独的 Node 反代：

- 登录：导入桌面客户端 `auth.json`（也接受 auth 响应里的 `{"data": {...}}`），或使用回环 OAuth。浏览器必须和网关在同一台机器、共享 `127.0.0.1` 回环；远程部署请导入 `auth.json`。
- 模型：9 个原始裸 ID —— `raccoon-090dd3`、`raccoon-19b265`、`raccoon-405a1c`、`sn-sensenova-6-8-flash`、`sn-sensenova-6-8-flash-lite`、`sn-glm-5-3`、`sn-kimi-k3`、`sn-glm-5-3-flash`、`sn-deepseek-v4-1-flash-1017`。目录以官方 `model_catalog` 为准，取不到时回落内置种子目录；`/v1/models?realm=raccoon` 只返回这些裸 ID，不加任何提供商前缀。
- 协议：chat 非流式与 SSE、工具调用、视觉模型的图片输入（OpenAI `image_url`、Responses `input_image`、Anthropic `image`）；`/v1/responses` 与 Anthropic Messages（需显式开启）复用现有适配层。
- 账号：多账号池、并发/会话粘性、429/5xx 冷却后自动换号、401/403 先强制续期一次再重试；账号表支持备注、任务开关（为后续自动化预留；v1.6.0 没有签到/成长任务）、单账号测试、启停、清冷却、删除和账号级代理槽。代理槽与 WorkBuddy / CodeArts 共用同一套连接复用、代理认证和 `no_proxy`。
- 用量：每行记 `realm=raccoon`、上游账号 UID 和 `cost_policy=raccoon:unpriced`；token 数按上游返回记录。Raccoon 账号卡片/账号表没有积分列；通用用量表沿用字段，积分显示 `0.00`、估算价格显示 `—`。小浣熊没有积分/签到接口。
- 限制：v1.6.0 不支持扫码登录、手机号/密码登录（后续版本再补）；`/v1/models?realm=all` 不合并 Raccoon，仍只含 WorkBuddy 模型；出口由 API Key 或面板默认出口选择，单一出口下不需要 `provider/` 前缀。
- 首次配置：
  1. 打开「设置 → 小浣熊接入」，勾选「启用小浣熊出口」并保存（首次成功导入账号会自动启用）。
  2. 切到小浣熊视图，点「添加账号」，导入 `auth.json` 或完成浏览器授权。
  3. 在「设置 → API Key 与出口绑定」里新建 Key，出口选 `小浣熊`，或留空跟随默认出口。

## 安全

- 华为云凭据只存在本机 `app/accounts/codearts/codearts-*.json`（权限 600），不返回浏览器、不写日志。
- 小浣熊 token 只存在本机 `app/accounts/raccoon/raccoon-*.json`（权限 600），管理接口和看板都不回显 access/refresh token。
- `/codearts/*` 与 `/raccoon/*` 管理端点只接受面板会话，不接受普通 API Key；删除账号不可恢复。
- 本项目仅用于协议学习与个人自用；账号风控、限流、封禁风险由使用者自行承担。

## 文档

- 上游基线与维护状态：[`UPSTREAM.md`](UPSTREAM.md)
- 各版本发布说明：[`docs/releases/`](docs/releases/)
- 完整变更记录（含上游 v1.4.5–v2.0.0）：[`app/docs/CHANGELOG.md`](app/docs/CHANGELOG.md)
- CodeArts 设计：[`app/docs/superpowers/specs/2026-10-31-codearts-native-design.md`](app/docs/superpowers/specs/2026-10-31-codearts-native-design.md)
- 新面板实施计划：[`docs/superpowers/plans/2026-11-01-new-dashboard.md`](docs/superpowers/plans/2026-11-01-new-dashboard.md)
- 部署与回滚：[`docs/setup.md`](docs/setup.md)
- 媒体实现：[`app/docs/MEDIA-IMPL.md`](app/docs/MEDIA-IMPL.md)
- 11-128 排障：[`app/docs/TROUBLESHOOTING-11-128.md`](app/docs/TROUBLESHOOTING-11-128.md)

## License

MIT。`app/` 与 `reference/codearts2api/` 保留各自上游的 MIT 许可证与版权声明。Raccoon provider 为本仓库新增的原生 Python 实现，不依赖 Node sidecar。
