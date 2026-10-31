# CodeArts 单进程原生接入实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 把 codearts2api 的 CodeArts 协议移植进 Python 网关，最终只运行一个 `app/` 进程。

**Architecture:** `app/wb_codearts_crypto.py` 提供纯标准库密码学；`app/wb_codearts.py` 提供 OAuth、账号池、模型发现、聊天流、签到；`app/wb_proxy.py` 负责 realm 路由和 `/codearts/*` 管理端点；`reference/codearts2api/` 只作协议参考，不构建。

**Tech Stack:** Python 3.9+ 标准库、原生 HTML/JS、Docker Compose（单服务）。

**Spec:** `app/docs/superpowers/specs/2026-10-31-codearts-native-design.md`

**Status:** 已实现（2026-10-31）。

## Global Constraints

- Python 只用标准库，兼容 3.9。
- 华为云凭据只落本机 `accounts/codearts/`，不进日志、不回浏览器。
- CodeArts 默认关闭；未启用时行为与旧版一致。
- 看板文案覆盖简中 / 繁中 / 英文。
- 测试：`cd app && python3 tests/run_all.py`（243 套件）。

## Tasks

- [x] Task 1：`wb_codearts_crypto.py`（P-256 ECDSA、DPoP、PKCE、SDK-HMAC-SHA256），用 Go 向量对拍。
- [x] Task 2：账号文件读写、`state.json`、账号池（pick/并发/冷却/停用）。
- [x] Task 3：OAuth start/poll/callback + 浏览器 `/oauth/callback`，token 交换与落盘。
- [x] Task 4：token 续期（到期前 30 分钟、refresh_token 终态失效停用）。
- [x] Task 5：模型发现（agent-center / builtin / 福利），福利标记与 `maas_type`。
- [x] Task 6：聊天 SSE 适配（标准 chunk、text 快照、output 终态、usage、内嵌错误）。
- [x] Task 7：签到（delivery/claim/confirm）。
- [x] Task 8：`wb_proxy` 的 `/codearts/*` 改为本地池，`/v1/models`、chat/responses/messages 走原生客户端。
- [x] Task 9：看板：第三卡片、CodeArts 账号表、OAuth 弹窗、设置（启用/并发/超时/缓存）。
- [x] Task 10：单服务 compose、Makefile、README/UPSTREAM/setup 更新。
- [x] Task 11：测试：crypto/native/models/routing/admin/JS + 全量回归。
- [x] Task 12：Playwright 真浏览器冒烟（账号表、停用、OAuth 弹窗、设置、0 JS 报错）。

## 验证

```bash
cd app && python3 tests/run_all.py   # 243 passed
cd deploy && docker compose config    # 单服务
```

真实华为云账号的 OAuth 与对话需要部署后手动走一遍（见 `docs/setup.md` 第 2 节）。
