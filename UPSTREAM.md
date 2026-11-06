# 上游快照与维护状态

本仓库只有一个运行中的服务：`app/`（Python）。`reference/codearts2api/` 是 Go 版 codearts2api 的源码快照，仅用于协议移植对照，**不参与构建、不运行**。

| 目录 | 上游 | 基线 commit | 基线版本 |
|---|---|---|---|
| `app/` | https://github.com/ardeyouxipianyi/workbuddy2api-hub | `d234396` | v2.0.0（最终版） |
| `reference/codearts2api/` | https://github.com/HITZY2002/codearts2api | `8d1e43d` | — |

## 维护状态

WorkBuddy2API-Hub 已于 **2026-11-06** 发布 v2.0.0 后停止维护：不再更新代码、不跟进上游变化、不再处理新的 issue / PR。

**mix2api 自 v1.5.0 起接管维护。** 后续功能、修复与发布都在本仓库进行，不再有“同步上游”流程。上游 v2.0.0 的用量聚合缓存补写（#805）与设置页分组语言切换修复（#804）已并入本仓库；更早的 v1.9.3–v1.10.11 变更记录已补进 `app/docs/CHANGELOG.md`。

## 平台支持

- **Docker**：`ghcr.io/ayeaaaa/mix2api` 已回填 v1.0.0–v1.5.0 全部历史 tag，linux/amd64 + linux/arm64。
- **便携包**：v1.2.0 起提供带 Windows 精简运行时的 `deploy.zip`；v1.0.x–v1.1.x 的 `deploy.zip` 是源码包。每个 Release 另有 `source.zip` 与 `SHA256SUMS`。
- **OpenWrt**：从 v1.5.0 起随 Release 提供 `.ipk`（opkg，24.10 及更早）与 `.apk`（apk-tools，25.x+）。包名、服务名和 UCI 配置名仍沿用 `workbuddy2api`（保升级兼容），`PKG_VERSION` 与 `server_version` 对齐。注意：从上游 1.9.2 的包升级到 1.5.0 属于版本号回退，需先卸载旧包或手动允许降级。

## 本仓库相对上游的改动

### app/

- 新增 `wb_codearts_crypto.py`：纯标准库 P-256 ECDSA、DPoP、PKCE、华为云 SDK-HMAC-SHA256。
- 重写 `wb_codearts.py`：单进程 CodeArts OAuth、token 续期、账号池、模型发现、聊天 SSE、签到。
- `wb_proxy.py`：`codearts` 出口、Key 绑定、模型目录、三类入口路由、用量归因、`/codearts/*` 管理端点、`/oauth/callback`。
- `dashboard.html`：第三张出口卡片、三出口视图、CodeArts 账号表、OAuth 弹窗、设置区块、i18n。
- `tests/`：密码学、账号池、模型、路由、管理端点、看板测试。

### reference/codearts2api/

- 仅保留源码与测试作为协议参考；本仓库的 Python 实现按它的 `signer.go`、`dpop.go`、`client.go`、`sse.go`、`models.go`、`checkin.go`、`pool.go`、`oauth.go` 移植。
- 不再构建、不再部署；根目录 compose 只有 `hub` 一个服务。

## 已同步的上游版本（历史）

本仓库已整体同步到上游最终版 `d234396` / v2.0.0，保留 mix2api 自己的品牌、面板、发布体系和本地默认配置。上游的媒体生成模块（`wb_media.py` / `wb_media_client.py` / `wb_media_tasks.py`）、CodeArts 新签到活动、设置并发保护、目录/价格快照、账号分组与分页、登录出口代理槽、413 响应、用量聚合缓存补写以及 v1.10.5–v2.0.0 的修复均已进入本仓库。

以下为 v1.10.0 – v1.10.4 的 CodeArts 修复记录（保留作历史说明）：

- `x-ot-session-id` / `user-session-id` / `x-ot-trace-id`：按账号稳定会话，修掉 CodeArts 会话名额被历史请求占满后的排队与 `TM.00001041`。
- CodeArts 的 Key 账号白名单与模型限制：`open_chat()` / `CodeArtsPool.pick()` 接受 `allowed_accounts`，禁止模型在进入上游前直接返回 400。
- CodeArts SSE 首个事件保留：`CodeArtsStream.__iter__` 不再在 `_done` 后丢掉 `_pending` 的首帧。
- OAuth 回调校验：`redirect` 必须属于 CodeArts 门户，拒绝带凭据的跳转；回调 HTML 做转义。
- 工具名校验：`normalize_tools` 丢弃上游不接受的 `function.name`（空、非法字符、超长、重名），修掉 400 / code 11133。
- 面板单请求体上限：设置页可调 1–256 MB，立即生效；413 提示同步更新。
- 面板细节：Key 选择器显示账号备注、活动状态区分「无本地记录」、删除重复的「上游响应头超时」卡片、活动流跨天补日期。
- 429 绝对恢复时刻：`X-RateLimit-Reset` / `X-RateLimit-Reset-At` 与正文 `reset_at` 已覆盖模型 429、登录限流和 CodeArts 透传出口（OpenAI / Anthropic 两种信封）。
- v1.10.2–v1.10.4：CodeArts OAuth 回调认领 `state` 作为 `ticket_id`；非流式聚合在 `finally` 关闭上游，修掉背靠背请求提前释放账号；`pricing/pricing.json` 快照与 `wb_catalog.py` / `wb_tasks.py` 对齐上游 v1.10.4；成长中心新增任务、Buddy 开盲盒与小程序链式任务已随 `wb_tasks.py` 进入本仓库。

`codearts2api` 的 `8d1e43d` 基线已包含每日签到与新手活动积分，本仓库现有实现已对齐；该上游没有新的后续 commit。

本仓库的 `wb_scheduler.py` 仍保留 CodeArts 独立保活/签到巡检；`wb_codearts.py` 保留单进程 CodeArts 实现与本地视觉支持，只回移上游的协议修复，不做整文件替换。

## 维护约定（接管后）

1. 新功能、修复直接在本仓库 `app/` 进行；不再从上游同步。
2. 改动必须带回归测试；提交前跑 `cd app && python3 tests/run_all.py`。
3. 版本号写在 `app/wb_proxy.py` 的两处（`runtime_settings_view()` 的 `version` 与 `Handler.server_version`）以及 `app/wrt/openwrt/workbuddy2api/Makefile` 的 `PKG_VERSION`。打 `v<version>` tag 时，release workflow 会断言 tag、源码与 OpenWrt 包版本三处一致；Docker 镜像由 `.github/workflows/docker.yml` 发布，便携包与 `.ipk`/`.apk` 由 `.github/workflows/release.yml` 构建。
4. `app/dashboard.html` 是单文件，修改尽量小步；改完跑 JS 套件。
5. 发布说明写在 `docs/releases/`。

历史同步流程（已停用）：先跑上游测试，再看 `git diff <基线>..<新版本>`，把改动重放到 `app/`；冲突时两边都留，不要用「取一侧」的方式解冲突。
