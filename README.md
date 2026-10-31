# mix2api

一个 **Python 单进程** OpenAI 兼容网关：把 **WorkBuddy 国际版**、**WorkBuddy 国内版** 和 **华为云 CodeArts** 三个上游收进一个服务、一个看板、一套 API Key。

```text
客户端 ──► mix2api（单进程）──► WorkBuddy 国际版
                          ├──► WorkBuddy 国内版
                          └──► 华为云 CodeArts（OAuth / 续期 / 模型 / 签到都在进程内）
```

- **一个服务**：不再需要单独运行 codearts2api；CodeArts 的 OAuth、token 续期、账号池、模型发现、聊天 SSE 和签到都由本进程直接完成。
- **三个出口**：看板顶部切换 `国际版 / 国内版 / CodeArts`；也可以给不同 API Key 各绑一个出口，三个同时用。
- **一个看板**：三个出口的账号都在这里增删、启停、签到；CodeArts 账号用华为云 OAuth 添加。
- **三类协议**：`/v1/chat/completions`、`/v1/responses`（Codex）、Anthropic Messages（需显式开启）都能走 CodeArts。

## 项目由来

本项目是 [workbuddy2api-hub](https://github.com/ardeyouxipianyi/workbuddy2api-hub)（MIT）和 [codearts2api](https://github.com/HITZY2002/codearts2api)（MIT）的整合分支。

- **起点**：wb-hub 只有 WorkBuddy 国际版 / 国内版两个出口；codearts2api 是一个独立的 Go 服务，自己管华为云账号池。
- **目标**：让 wb-hub 的出口开关多一个 `CodeArts`，三个上游共用一个地址、一个看板、一套 API Key。
- **过程**：先做过「hub + 独立 codearts2api」的代理版；确认要单进程后，把 CodeArts 的 OAuth、token 续期、模型发现、聊天 SSE、签到和积分查询移植进 Python，最终只运行 `app/` 一个服务。
- **代码关系**：`app/` 派生自 wb-hub；`reference/codearts2api/` 只保留 Go 源码作协议参考，不参与构建和运行。

## 这次做了什么

- **单进程 CodeArts 客户端**：华为云 OAuth2（PKCE + DPoP）、refresh_token 续期、账号池、并发/冷却、模型发现（agent-center / builtin / 限时福利）、聊天 SSE、签到、积分余额。
- **纯标准库密码学**：P-256 ECDSA、DPoP JWT、PKCE、华为云 `SDK-HMAC-SHA256` 签名；ECDSA 与 HMAC 都用 Go 侧向量对拍。
- **看板**：第三张 CodeArts 出口卡片、三出口切换、CodeArts 账号表（OAuth 添加 / 启停 / 清冷却 / 签到 / 删除）、设置页、积分余额与已用/总额、模型描述。
- **账号代理槽**：CodeArts 账号可绑定与 WorkBuddy 相同的「账号代理槽」，账号表直接选出口；聊天、模型发现、token 续期、签到和积分查询都走该槽。出站复用 `wb_upstream_pool` 的连接复用、代理认证、`no_proxy` 和失效连接重试，槽位停用/删除回退直连。
- **协议与用量**：`/v1/chat/completions`、`/v1/responses`、Anthropic Messages 统一路由；CodeArts 用量记 `realm=codearts`，不按 WorkBuddy 价目表计价。
- **部署**：单服务 compose，默认端口 `58675`；`make build / up / test / logs`。
- **测试**：243 个 hub 套件全过；CodeArts 链路用本机假华为云上游覆盖，密码学有 Go 对照向量。

> 本项目仅用于协议学习与个人自用；账号风控、限流、封禁风险由使用者自行承担。请优先使用官方客户端，不要使用主账号，不要共享或转售账号。

## 目录

| 路径 | 内容 |
|---|---|
| `app/` | 唯一的服务：Python 网关 + 看板（三出口） |
| `deploy/` | 单服务 compose |
| `docs/` | 设计、实施计划、部署说明 |
| `reference/codearts2api/` | Go 版 codearts2api 源码，**仅作协议移植参考，不构建、不运行** |

`app/` 派生自 [workbuddy2api-hub](https://github.com/ardeyouxipianyi/workbuddy2api-hub)；`reference/codearts2api` 派生自 [codearts2api](https://github.com/HITZY2002/codearts2api)。两者都是 MIT，版权与同步方式见 [`UPSTREAM.md`](UPSTREAM.md)。

## 快速启动（Docker）

### 方式一：拉预构建镜像（推荐）

镜像已发布到 GHCR，支持 `linux/amd64` 与 `linux/arm64`：

```bash
cd deploy
docker compose -f docker-compose.image.yml up -d
```

或手动：

```bash
docker pull ghcr.io/ayeaaaa/mix2api:v1.0.7
docker run -d --name mix2api -p 58675:58675 \
  -v "$PWD/data/accounts:/app/accounts" \
  -v "$PWD/data/usage:/app/usage" \
  ghcr.io/ayeaaaa/mix2api:v1.0.7
```

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

CodeArts 账号在 `app/accounts/codearts/` 下；WorkBuddy 账号在 `app/accounts/` 下。两者互不干扰。

## CodeArts 说明

- 登录：华为云 CodeArts OAuth2（PKCE + DPoP），refresh_token 与 client_id、DPoP 私钥一起落盘。
- 续期：token 到期前 30 分钟自动刷新；refresh_token 终态失效时账号标记为「需要重新登录」。
- 模型：agent-center / builtin / 限时福利三路合并，福利模型聊天时自动加 `maas_type: benefit`。
- 聊天：`/api/v2/chat/completions`，AK/SK `SDK-HMAC-SHA256` 签名 + `x-auth-token`。
- 签到：`/v1/ops/delivery|claim|confirm`；看板「签到 / 全员签到」可手动触发，「设置 → CodeArts 接入」的「每天自动签到」默认开启，跟随国内版签到时段（默认 09:00 / 21:00），只跑已启用且未冷却的账号，当天成功后不重复。
- 积分：`snap-manager/v1/statistics/plugin` 查询剩余 / 总额 / 已用；单次请求的积分扣减上游不返回，最近请求表积分列显示 `0.00`、估算价格显示 `—`。
- 代理：CodeArts 账号可绑定「账号代理槽」，出站请求与 WorkBuddy 共用 `wb_upstream_pool`（连接复用、代理认证、`no_proxy`、失效连接重试）；槽位停用/删除回退直连，首次 OAuth 换 token 仍直连。
- 图片：`/v1/models` 按 CodeArts 上游的 `supports_images` 宣告视觉能力，并输出 WorkBuddy 同款 `capabilities.vision` / `input_modalities` 等字段。当前 `Qwen3-VL-235B`、`kimi-k2.6-vl` 为 true；`GLM-5.2`、`kimi-k3`、`deepseek-v4.1-flash` 等为 false，不会向客户端误报可读图。视觉模型支持 OpenAI `image_url`、Responses `input_image`、Anthropic `image` 块；data URL 已实测，远程 URL 原样透传（能否抓取取决于上游）。
- 模型命名：CodeArts 对外统一用 `YYLS/<模型名>`（如 `YYLS/GLM-5.2`、`YYLS/deepseek-v4.1-flash`），避免和 WorkBuddy 同名模型冲突。`/v1/models?realm=codearts` 返回带前缀的 ID；请求带 `YYLS/` 时自动路由到 CodeArts，并在发给华为前剥掉前缀。
- 推理档位：`deepseek-v4.1-flash` / `-1017` 支持 `none / low / medium / high / xhigh / max`；`GLM-5.2` / `GLM-5.2-SFT-Harmony` / `GLM-5.3` 支持 `none / minimal / low / medium / high / xhigh / max`；`glm-5.3-flash` 只支持 `low / high / max` 且始终思考。`/v1/models` 会宣告 `reasoning_efforts`；`auto` 或未知值在本地直接 400，并列出允许值。
- 模型元数据：`/v1/models` 输出 `context_length`、`max_input_tokens`、`max_output_tokens`、`max_completion_tokens`、`max_output` 和工具能力字段；上游没给上下文/输出上限时，用同名 WorkBuddy 目录兜底。
- 请求兼容：chat 透传 `stop`、`response_format`、`frequency_penalty`、`presence_penalty`、`seed`（上游已实测接受）。
- 调度：CodeArts 每 30 分钟检查一次 Token 并续期快过期账号；22:00 仍随 WorkBuddy 巡检。自动签到跟随签到时段。CodeArts 账号表新增备注、任务开关（API 停用但继续签到）和单账号测试。
- 错误语义：非视觉模型收到图片返回 400，并提示可用视觉模型；未知参数/图片错误不再统一吞成 502。
- 统一目录：`/v1/models?realm=all` 同时包含 WorkBuddy 和 `YYLS/` CodeArts 模型；CodeArts 签到写入 `/activity/history`，可按区域筛选。
- 限制：CodeArts 账号不参与 WorkBuddy 的积分/额度护栏和跨出口回退；API Key 的「允许账号」白名单只作用于 WorkBuddy 账号；代理槽按 HTTP/HTTPS 代理实现。

## 安全

- 华为云凭据只存在本机 `app/accounts/codearts/codearts-*.json`（权限 600），不返回浏览器、不写日志。
- `/codearts/*` 管理端点只接受面板会话，不接受普通 API Key；删除账号不可恢复。
- 本项目仅用于协议学习与个人自用；账号风控、限流、封禁风险由使用者自行承担。

## 文档

- 设计：`app/docs/superpowers/specs/2026-10-31-codearts-native-design.md`
- 实施计划：`docs/superpowers/plans/2026-10-31-codearts-native.md`
- 部署与回滚：`docs/setup.md`

## License

MIT。`app/` 与 `reference/codearts2api/` 保留各自上游的 MIT 许可证与版权声明。
