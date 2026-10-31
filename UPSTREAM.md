# 上游快照与同步

本仓库的 `apps/hub` 和 `apps/codearts` 是两个上游项目的派生副本。为了让「三出口」这件事可部署、可回滚，集成改动直接落在副本里；上游的原始版权与许可证保留在各自目录。

| 目录 | 上游 | 基线 commit | 基线版本 |
|---|---|---|---|
| `apps/hub` | https://github.com/ardeyouxipianyi/workbuddy2api-hub | `736d08e` | v1.9.2 |
| `apps/codearts` | https://github.com/HITZY2002/codearts2api | `8d1e43d` | — |

## 本仓库相对上游的改动

### apps/hub

- 新增 `wb_codearts.py`：CodeArts 客户端、模型缓存、请求体、账号适配对象。
- `wb_settings.py` / `wb_proxy.py`：`codearts` 出口、Key 绑定、模型目录、三类入口路由、用量归因、`/codearts/*` 管理代理。
- `dashboard.html`：第三张出口卡片、三出口视图、CodeArts 账号表、OAuth 弹窗、设置区块、i18n。
- `tests/`：CodeArts 客户端、realm、路由、管理代理、看板测试。

### apps/codearts

- `internal/auth`：`Delete(dir, userID)`。
- `internal/pool`：`Pool.Remove(name)`。
- `internal/server`：`POST /admin/api/accounts/delete`。
- 对应测试与 README 管理端点表。

## 同步上游

1. 拉上游最新 tag/commit，先跑上游自己的测试。
2. 用 `git diff <基线>..<上游新版本>` 看上游改了什么。
3. 把上游改动重放到 `apps/hub` / `apps/codearts`，冲突时**两边都留**：上游的新行为不能丢，本仓库的 CodeArts 集成也不能丢。
4. 更新本文件的基线 commit 与版本。
5. 跑 `apps/hub/tests/run_all.py` 和 `apps/codearts` 的 `go test ./...`。

不要用「取一侧」的方式解冲突；`apps/hub` 的看板是单文件，整块覆盖最容易把上游的修复弄丢。
