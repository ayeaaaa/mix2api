# 上游快照与同步

本仓库只有一个运行中的服务：`app/`（Python）。`reference/codearts2api/` 是 Go 版 codearts2api 的源码快照，仅用于协议移植对照，**不参与构建、不运行**。

| 目录 | 上游 | 基线 commit | 基线版本 |
|---|---|---|---|
| `app/` | https://github.com/ardeyouxipianyi/workbuddy2api-hub | `736d08e` | v1.9.2 |
| `reference/codearts2api/` | https://github.com/HITZY2002/codearts2api | `8d1e43d` | — |

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

## 同步上游

1. 拉上游最新 tag/commit，先跑上游自己的测试。
2. 用 `git diff <基线>..<上游新版本>` 看上游改了什么。
3. 把上游改动重放到 `app/`；冲突时**两边都留**：上游的新行为不能丢，本仓库的 CodeArts 集成也不能丢。
4. 更新本文件的基线 commit 与版本。
5. 跑 `cd app && python3 tests/run_all.py`。

不要用「取一侧」的方式解冲突；`app/dashboard.html` 是单文件，整块覆盖最容易把上游的修复弄丢。
