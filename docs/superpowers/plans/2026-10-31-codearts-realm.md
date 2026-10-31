# CodeArts 第三出口（mix2api）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在独立的 `mix2api` 仓库里，把 `codearts2api` 接成 wb-hub 的第三个出口，并在 wb-hub 看板里完成 CodeArts 账号的增删启停。

**Architecture:** monorepo：`apps/hub` 是 wb-hub 的派生副本（Python），`apps/codearts` 是 codearts2api 的派生副本（Go）。两个服务仍各自独立运行，由根目录的 compose 一起拉起；wb-hub 通过 codearts2api 的 OpenAI 兼容接口和 admin API 做前门与账号控制面，不重新实现华为云 OAuth。

**Tech Stack:** Python 3.9+（纯标准库）、Go 1.22+、原生 HTML/JS 看板、Docker Compose。

**Spec:** `apps/hub/docs/superpowers/specs/2026-10-31-codearts-realm-design.md`

## Global Constraints

- Python 只用标准库，兼容 3.9；不要引入 pip 依赖。
- Go 代码通过 `go test ./...`；不要引入新模块。
- CodeArts 默认关闭；未配置时 `/realm`、看板、Key 绑定、请求行为与老版本一致。
- 任何日志、HTTP 响应、看板 DOM 都不得出现 CodeArts API Key。
- 看板文案同时覆盖简体、繁体、英文；新增文本必须进 i18n 词典。
- 提交信息格式 `<type>(<scope>): 中文摘要`，正文中英双语。
- 不直接改 `apps/hub` / `apps/codearts` 里与本次集成无关的行为；所有上游差异记进 `UPSTREAM.md`。
- 测试命令：`cd apps/hub && python tests/run_all.py`；`cd apps/codearts && go test ./...`；`node apps/hub/tests/_test_*.js` 由 run_all 覆盖。

## Review Focus

- CodeArts 地址错 / Key 错 / 服务没起：WorkBuddy 两个出口的请求、`/health` 主状态、看板加载都不能被拖慢或变红。
- 模型名冲突（`deepseek-v4-pro-0813`、`GLM-5.2`）：选中 CodeArts 时必须走 CodeArts，不能被 `detect_model_realm()` 改判成 `cn`。
- 关闭 CodeArts 出口时默认出口仍是 `codearts`：必须收敛回 `intl`；已绑 codearts 的 Key 要报可读错误，不能静默改绑到 WorkBuddy。
- 远端部署的 OAuth 粘贴回调返回 `status:"continue"` + `next_url`：弹窗必须继续打开并轮询，不能卡在「中转成功」。
- 删除一个仍有会话亲和 / 仍在 Key 的 `accounts` 白名单里的账号：请求不能崩，账号不能「复活」，白名单残留无害。

---

## Task 1: 仓库骨架、上游快照与部署编排

**Files:**
- Create: `README.md`
- Create: `LICENSE`
- Create: `UPSTREAM.md`
- Create: `.gitignore`
- Create: `deploy/docker-compose.yml`
- Create: `deploy/.env.example`
- Create: `deploy/config/codearts.json`
- Create: `docs/setup.md`
- Existing: `apps/hub/`（已从 `ardeyouxipianyi/workbuddy2api-hub@736d08e` 复制）
- Existing: `apps/codearts/`（已从 `HITZY2002/codearts2api@8d1e43d` 复制）

**Interfaces:**
- Produces: 目录结构 `apps/hub`、`apps/codearts`、`deploy/`、`docs/`；根 README 说明两个上游的版权和同步方式。

- [ ] **Step 1: 写 `UPSTREAM.md`，记录来源、commit、同步命令**

```markdown
| 目录 | 上游 | Commit | 同步方式 |
|---|---|---|---|
| apps/hub | https://github.com/ardeyouxipianyi/workbuddy2api-hub | 736d08e (v1.9.2) | git diff 上游 tag，手工重放集成补丁 |
| apps/codearts | https://github.com/HITZY2002/codearts2api | 8d1e43d | 同上 |
```

- [ ] **Step 2: 写根 `README.md`**

内容至少包含：项目定位（三出口聚合）、架构图、Docker 一键启动、本地开发启动、CodeArts 配置步骤、账号管理入口、安全提示、上游版权重声明。

- [ ] **Step 3: 写 `deploy/docker-compose.yml`**

```yaml
services:
  hub:
    build: ../apps/hub
    ports: ["8788:8788"]
    environment: [HOST=0.0.0.0, PORT=8788, TZ=Asia/Shanghai]
    volumes:
      - ../data/hub/accounts:/app/accounts
      - ../data/hub/usage:/app/usage
    depends_on: [codearts]
  codearts:
    build: ../apps/codearts
    ports: ["127.0.0.1:7866:7866"]
    environment:
      CA2A_API_KEY: ${CA2A_API_KEY:?set CA2A_API_KEY in deploy/.env}
      TZ: Asia/Shanghai
    volumes:
      - ../data/codearts/auths:/app/auths
      - ../data/codearts/data:/app/data
      - ./config/codearts.json:/app/config.json:ro
```

- [ ] **Step 4: 写 `deploy/.env.example` 与 `deploy/config/codearts.json`**

`.env.example` 至少含 `CA2A_API_KEY=changeme`；`codearts.json` 从 `apps/codearts/config.example.json` 复制，`api_key` 留空（由 env 覆盖）。

- [ ] **Step 5: 写 `.gitignore`**

忽略 `data/`、`deploy/.env`、`__pycache__/`、`*.pyc`、`apps/codearts/bin/`、`apps/codearts/auths/`、`apps/codearts/data/`。

- [ ] **Step 6: 验证**

Run:
```bash
cd /root/opencode/mix2api
docker compose -f deploy/docker-compose.yml --env-file deploy/.env.example config
cd apps/codearts && go build ./...
cd apps/hub && python -c "import wb_proxy, wb_settings; print('import ok')"
```
Expected: compose 配置可解析；Go 编译通过；Python 导入无异常。

- [ ] **Step 7: 提交**

```bash
git add README.md LICENSE UPSTREAM.md .gitignore deploy docs
git commit -m "chore(repo): scaffold mix2api monorepo" -m "Vendor both upstreams, add compose and setup docs. 搭建 mix2api monorepo，收纳两个上游并补部署编排。"
```

---

## Task 2: `wb_codearts.py` 配置、HTTP 客户端与模型缓存

**Files:**
- Create: `apps/hub/wb_codearts.py`
- Create: `apps/hub/tests/_fake_codearts.py`
- Test: `apps/hub/tests/_test_codearts_client.py`

**Interfaces:**
- Produces:
  - `DEFAULTS: dict`
  - `load_config(accounts_dir: str) -> dict`
  - `save_config(accounts_dir: str, patch: dict) -> dict`
  - `configured(accounts_dir: str) -> bool`
  - `masked_config(accounts_dir: str) -> dict`（`api_key` 为空，带 `api_key_set`）
  - `models(config: dict, force: bool = False) -> list[dict]`
  - `model_entries(config: dict) -> list[dict]`
  - `invalidate_models() -> None`
  - 异常：`CodeArtsError`、`CodeArtsUnavailable`、`CodeArtsAuthError`、`CodeArtsBadRequest`

- [ ] **Step 1: 写失败测试（配置）**

`tests/_test_codearts_client.py` 先覆盖：

```python
cfg = wb_codearts.load_config(tmp)
assert cfg["enabled"] is False
assert cfg["base_url"] == "http://127.0.0.1:7866"
saved = wb_codearts.save_config(tmp, {"enabled": True, "base_url": "http://x:7866/", "api_key": "k1"})
assert saved["base_url"] == "http://x:7866"
assert wb_codearts.save_config(tmp, {"api_key": ""})["api_key"] == "k1"
for bad in ("ftp://x", "http://u:p@x", ""):
    with pytest.raises(ValueError): wb_codearts.save_config(tmp, {"base_url": bad})
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && python tests/_test_codearts_client.py`
Expected: `ModuleNotFoundError: wb_codearts` 或 `AttributeError`。

- [ ] **Step 3: 实现配置部分**

`load_config` 读 `accounts/settings.json` 的 `codearts` 对象，和 `DEFAULTS` 合并并做类型收敛；`save_config` 校验 `base_url`、`timeout_seconds > 0`、`models_ttl_seconds > 0`，`api_key` 空串保留旧值。`masked_config` 不返回明文。

- [ ] **Step 4: 写失败测试（HTTP + 模型缓存）**

用 `_fake_codearts.start_server()` 起假服务：

```python
srv, base, state = start_server(models=["GLM-5.2", "glm-5.2"])
cfg = wb_codearts.load_config(tmp) | {"base_url": base, "api_key": "test-key", "enabled": True}
first = wb_codearts.models(cfg)
assert [m["id"] for m in first] == ["GLM-5.2", "glm-5.2"]
second = wb_codearts.models(cfg)
assert state["models_hits"] == 1  # 命中缓存
state["fail_models"] = True
assert [m["id"] for m in wb_codearts.models(cfg)] == ["GLM-5.2", "glm-5.2"]  # stale
```

- [ ] **Step 5: 实现 `_request` / `models` / `model_entries`**

`_request` 用 `urllib.request.build_opener(ProxyHandler({}))` 直连；`Authorization: Bearer`；错误体最多读 64 KiB；`HTTPError` 按 401/403、400、429、5xx 映射到异常；`URLError`/`socket.timeout` → `CodeArtsUnavailable`。缓存键 `(base_url, api_key 的 sha256 前 8 位)`，TTL `models_ttl_seconds`。

- [ ] **Step 6: 跑测试确认通过**

Run: `cd apps/hub && python tests/_test_codearts_client.py`
Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add apps/hub/wb_codearts.py apps/hub/tests/_fake_codearts.py apps/hub/tests/_test_codearts_client.py
git commit -m "feat(codearts): add client config and model cache" -m "Add the CodeArts client config, direct HTTP client and cached model catalog. 新增 CodeArts 客户端配置、直连 HTTP 与模型目录缓存。"
```

---

## Task 3: CodeArts 请求体、流式打开与账号适配对象

**Files:**
- Modify: `apps/hub/wb_codearts.py`
- Modify: `apps/hub/tests/_fake_codearts.py`
- Test: `apps/hub/tests/_test_codearts_chat.py`

**Interfaces:**
- Produces:
  - `class CodeArtsAccount`：`uid="codearts"`、`realm="codearts"`、`product="codearts"`、`proxy=None`、`chat_path="/v1/chat/completions"`、`chat_base_url() -> str`、`model_disabled(mid) -> False`、`is_cli_identity() -> False`
  - `build_chat_body(payload: dict, session_key: str | None = None) -> dict`
  - `open_chat(config: dict, body: dict, timeout: float | None = None) -> response`

- [ ] **Step 1: 写失败测试（请求体）**

```python
body = wb_codearts.build_chat_body({"model": "GLM-5.2", "messages": [{"role": "user", "content": "hi"}], "stream": False, "_x": 1, "user": "u"}, "sess-1")
assert body["stream"] is True
assert body["stream_options"] == {"include_usage": True}
assert "_x" not in body and "user" not in body
assert body["conversation_id"] == hashlib.sha256(b"sess-1").hexdigest()[:32]
assert all(m["role"] != "system" for m in body["messages"])  # 不注入 WorkBuddy 系统提示
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && python tests/_test_codearts_chat.py`
Expected: `AttributeError: build_chat_body`。

- [ ] **Step 3: 实现 `build_chat_body`**

复制客户端字段；删除 `_` 前缀键、`user`、`metadata`、`store`、`previous_response_id`；用本地导入的 `merge_adjacent_tool_calls` / `repack_tool_result_blocks` / `cleanup_orphan_tool_calls` 修复工具配对；`messages` 为空时抛 `CodeArtsBadRequest`；`conversation_id` 合法 32hex 就保留，否则由 `session_key` 派生。

- [ ] **Step 4: 写失败测试（流式与错误）**

假服务返回标准 OpenAI SSE：`role` chunk、文本 chunk、`finish_reason` chunk、`usage` chunk、`[DONE]`。断言：

```python
resp = wb_codearts.open_chat(cfg, body)
lines = [l for l in resp if l.startswith(b"data:")]
assert any(b'"usage"' in l for l in lines)
state["last_auth"] == "Bearer test-key"
state["last_body"]["stream"] is True
```

再覆盖 401、400、500、连接拒绝四种错误映射，并断言异常文本里没有 `test-key`。

- [ ] **Step 5: 实现 `open_chat` 与 `CodeArtsAccount`**

`open_chat` 用 `stream_timeout_seconds`（或传入 timeout），POST `{base_url}/v1/chat/completions`；设置 `resp._wb_billed_model = body["model"]`（失败则忽略）。`CodeArtsAccount` 是普通类，构造参数 `config`。

- [ ] **Step 6: 跑测试确认通过**

Run: `cd apps/hub && python tests/_test_codearts_chat.py`
Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add apps/hub/wb_codearts.py apps/hub/tests/_fake_codearts.py apps/hub/tests/_test_codearts_chat.py
git commit -m "feat(codearts): add chat body builder and stream opener" -m "Build standard OpenAI bodies, open the CodeArts SSE stream and expose an account adapter. 生成标准 OpenAI 请求体、打开 CodeArts SSE 流并提供账号适配对象。"
```

---

## Task 4: realm 持久化、Key 绑定与停用收敛

**Files:**
- Modify: `apps/hub/wb_settings.py`
- Modify: `apps/hub/wb_proxy.py`
- Test: `apps/hub/tests/_test_codearts_realm.py`

**Interfaces:**
- Consumes: `wb_codearts.configured(accounts_dir)`
- Produces:
  - `wb_settings.REALMS == ("", "intl", "cn", "codearts")`
  - `wb_settings.codearts_config(accounts_dir) -> dict`（委托 `wb_codearts.load_config`）
  - `load_persisted_realm()` / `save_persisted_realm(realm)` 接受 `codearts`
  - `_get_realm()` 的 `options` 动态含 `codearts`
  - `_request_realm()` 可返回 `codearts`

- [ ] **Step 1: 写失败测试（持久化与 Key 绑定）**

```python
wb_settings.set_codearts_config(tmp, {"enabled": True, "base_url": "http://x", "api_key": "k"})
assert "codearts" in wb_settings.REALMS
wb_proxy.save_persisted_realm("codearts"); assert wb_proxy.load_persisted_realm() == "codearts"
# 未配置时回落
wb_settings.set_codearts_config(tmp, {"enabled": False})
wb_proxy.CURRENT_REALM = "codearts"; wb_proxy.load_persisted_realm(); assert wb_proxy.CURRENT_REALM == "intl"
```

再断言 Key 保存：`realm="codearts"` 可保存；未配置时返回 400；`LIMIT_REALMS == ("intl","cn")`。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && python tests/_test_codearts_realm.py`
Expected: `AssertionError` / `KeyError`。

- [ ] **Step 3: 改 `wb_settings.py`**

`REALMS` 加 `codearts`；新增 `codearts_config` / `set_codearts_config` / `codearts_configured`（委托 `wb_codearts`）；`_handle_settings_save` 的 realm 校验改为接受 codearts，但未配置时 400。

- [ ] **Step 4: 改 `wb_proxy.py` 的 realm 生命周期**

- `load_persisted_realm` / `save_persisted_realm` 白名单加 codearts；启动时未配置则回落 intl。
- `_get_realm` 动态 options，并在 `CURRENT_REALM` 不在 options 时收敛到 intl 并写盘。
- `_cross_realm_error` 在 `realm == "codearts"` 时直接返回 `""`。
- 保存 `enabled=false` 时如果 `CURRENT_REALM == "codearts"`，强制 `save_persisted_realm("intl")` 并在响应里带 `warning`。
- `open_upstream_with_fallback` 不处理 codearts（由调用方分支）。

- [ ] **Step 5: 跑测试确认通过**

Run: `cd apps/hub && python tests/_test_codearts_realm.py`
Expected: 全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add apps/hub/wb_settings.py apps/hub/wb_proxy.py apps/hub/tests/_test_codearts_realm.py
git commit -m "feat(realm): add codearts realm persistence and key binding" -m "Add the third realm to settings, persistence, key binding and disable-time reconciliation. 将 CodeArts 纳入出口持久化、Key 绑定与停用收敛。"
```

---

## Task 5: `/v1/models` 的 CodeArts 分支

**Files:**
- Modify: `apps/hub/wb_proxy.py`
- Test: `apps/hub/tests/_test_codearts_models.py`

**Interfaces:**
- Consumes: `wb_codearts.model_entries(config) -> list[dict]`
- Produces: `GET /v1/models?realm=codearts` 返回 `{object:"list", data:[...], realm:"codearts", catalog:{source:"codearts2api", fetched_at, stale}}`

- [ ] **Step 1: 写失败测试**

用假服务起 handler（`wb_proxy.GatewayServer` 绑定 127.0.0.1:0），发 `GET /v1/models?realm=codearts`：

```python
assert payload["realm"] == "codearts"
assert [m["id"] for m in payload["data"]] == ["GLM-5.2", "glm-5.2"]
assert payload["data"][0]["owned_by"] == "codearts"
assert payload["catalog"]["source"] == "codearts2api"
```

再断言：Key 限制生效时只返回允许的模型；上游不可达且无缓存 → 502；有缓存 → 200 + `stale=True`。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && python tests/_test_codearts_models.py`
Expected: 404 / 502 / 断言失败。

- [ ] **Step 3: 实现分支**

`_get_v1_models()` 在 `req_realm == "codearts"` 时走 `wb_codearts.model_entries`，只应用 `filter_models_by_key`，跳过 pinned / `models_only_available` / tier 标注。异常映射：`CodeArtsUnavailable` → 502，`CodeArtsAuthError` → 502。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd apps/hub && python tests/_test_codearts_models.py`
Expected: 全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add apps/hub/wb_proxy.py apps/hub/tests/_test_codearts_models.py
git commit -m "feat(models): serve the codearts catalog per realm" -m "Return the CodeArts model list under realm=codearts with cache metadata. 在 codearts 出口下返回 CodeArts 模型目录与缓存元信息。"
```

---

## Task 6: Chat / Responses / Messages 路由与用量归因

**Files:**
- Modify: `apps/hub/wb_proxy.py`
- Test: `apps/hub/tests/_test_codearts_routing.py`

**Interfaces:**
- Consumes: `wb_codearts.build_chat_body`、`wb_codearts.open_chat`、`wb_codearts.CodeArtsAccount`
- Produces:
  - `Handler._handle_codearts_chat(payload, session_key, realm)`
  - `_dispatch_chat_post` / `_handle_responses` / `_handle_messages` 的 codearts 分支
  - `record_usage(..., realm=None)`、`record_error(..., realm=None)`
  - `_chat_stream_response(..., realm=None)`、`_chat_nonstream_response(..., realm=None)`
  - `_responses_*` / `_messages_*` 的 `allow_web_tools` 参数（仅 responses 需要）

- [ ] **Step 1: 写失败测试（chat 流式/非流式）**

假服务返回标准 SSE。断言：

```python
# /v1/chat/completions stream=true
events = read_sse(resp)
assert any(e["choices"][0]["delta"].get("content") for e in events if e.get("choices"))
assert last_usage_row()["realm"] == "codearts"
assert last_usage_row()["cost_policy"] == "codearts:unpriced"
# 非流式
assert resp.json()["choices"][0]["message"]["content"]
```

再断言 WorkBuddy 池零调用（用假 POOL 的 `pick_for_session` 计数）。

- [ ] **Step 2: 写失败测试（responses / messages）**

`POST /v1/responses`（流式/非流式）与 `POST /v1/messages`（先打开 `enable_anthropic_messages`）各一条，断言返回形状正确、`realm=codearts`、没有走 `follow_up_with_tool_results`。

- [ ] **Step 3: 跑测试确认失败**

Run: `cd apps/hub && python tests/_test_codearts_routing.py`
Expected: 503「no usable account」或断言失败。

- [ ] **Step 4: 实现分支与 `realm` 归因**

- `record_usage` / `record_error` 增加 `realm=None`，优先使用显式值。
- `_chat_*` 增加 `realm=None` 并传给 `record_upstream_usage`。
- `_responses_*` / `_messages_*` 已有 `realm` 参数，直接传 `"codearts"`；responses 增加 `allow_web_tools=True`，codearts 传 False。
- `_handle_codearts_chat`：校验配置 → `build_chat_body` → `open_chat` → 复用现有回程处理器。
- `_dispatch_chat_post` / `_handle_responses` / `_handle_messages` 在 `req_realm == "codearts"` 时走新分支；异常统一映射，流已开始时由回程处理器发 SSE error。

- [ ] **Step 5: 跑测试确认通过**

Run: `cd apps/hub && python tests/_test_codearts_routing.py`
Expected: 全部 PASS。

- [ ] **Step 6: 跑现有路由回归**

Run: `cd apps/hub && python tests/_test_request_contract.py && python tests/_test_anthropic_messages.py`
Expected: PASS，WorkBuddy 路径未变。

- [ ] **Step 7: 提交**

```bash
git add apps/hub/wb_proxy.py apps/hub/tests/_test_codearts_routing.py
git commit -m "feat(routing): route chat, responses and messages to codearts" -m "Add the codearts branch to all three inbound protocols and attribute usage to the new realm. 三类入口接入 CodeArts 分支，并把用量归到新出口。"
```

---

## Task 7: `/codearts/*` 管理代理与设置视图

**Files:**
- Modify: `apps/hub/wb_codearts.py`
- Modify: `apps/hub/wb_proxy.py`
- Test: `apps/hub/tests/_test_codearts_admin.py`

**Interfaces:**
- Produces:
  - `wb_codearts.overview(config) -> dict`（5 秒缓存）
  - `oauth_start` / `oauth_poll` / `oauth_callback` / `enable` / `disable` / `remove` / `clear_cooldown` / `checkin` / `keepalive` / `health`
  - Handler `_handle_codearts(path, payload)`，路径见设计 §5.8
  - `runtime_settings_view()` 增加 `codearts` 字段（`api_key_set`，无明文）

- [ ] **Step 1: 写失败测试（鉴权与代理）**

```python
# 无面板会话
assert post("/codearts/accounts/delete", {"uid": "u1"}).status == 401
# 只有 API Key、没有面板会话
assert post("/codearts/accounts/delete", {"uid": "u1"}, api_key=KEY).status == 401
# 面板会话
assert post("/codearts/accounts/delete", {"uid": "u1"}, panel=TOKEN).json()["deleted"] is True
```

再覆盖 overview、enable、disable、clear-cooldown、checkin、keepalive、oauth start/poll/continue、上游 404 → 409。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && python tests/_test_codearts_admin.py`
Expected: 404 / 401 断言失败。

- [ ] **Step 3: 实现客户端管理方法与路由**

`_handle_codearts` 只接受 `_panel_ok()`；GET overview/accounts，POST 其余。`MANAGEMENT_PATH_PREFIXES` 与 `_is_panel_route()` 加 `/codearts`。`runtime_settings_view()` 返回 `codearts: {enabled, base_url, api_key_set, timeout_seconds, stream_timeout_seconds, models_ttl_seconds}`。

- [ ] **Step 4: 实现设置保存**

`_handle_settings_save` 接受 `codearts` 对象；`api_key` 空串保留旧值；保存后如果 `enabled=false` 且当前出口是 codearts，执行 Task 4 的收敛并返回 warning。

- [ ] **Step 5: 跑测试确认通过**

Run: `cd apps/hub && python tests/_test_codearts_admin.py`
Expected: 全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add apps/hub/wb_codearts.py apps/hub/wb_proxy.py apps/hub/tests/_test_codearts_admin.py
git commit -m "feat(admin): proxy codearts account management" -m "Expose panel-only CodeArts account CRUD, OAuth and settings endpoints. 增加面板专属的 CodeArts 账号管理、OAuth 与设置端点。"
```

---

## Task 8: codearts2api 删除账号端点

**Files:**
- Modify: `apps/codearts/internal/auth/auth.go`
- Modify: `apps/codearts/internal/pool/pool.go`
- Modify: `apps/codearts/internal/server/handler.go`
- Modify: `apps/codearts/internal/server/admin.go`
- Test: `apps/codearts/internal/auth/auth_test.go`
- Test: `apps/codearts/internal/pool/pool_test.go`
- Test: `apps/codearts/internal/server/admin_test.go`（新建）
- Modify: `apps/codearts/README.md`

**Interfaces:**
- Produces:
  - `auth.Delete(dir, userID string) error`
  - `Pool.Remove(name string) bool`
  - `POST /admin/api/accounts/delete` → `{ok, deleted, message}`；404 / 403

- [ ] **Step 1: 写失败测试（auth.Delete）**

```go
func TestDeleteRemovesCredentialFile(t *testing.T) {
    dir := t.TempDir()
    a := &Auth{UserID: "u1", CloudDragonTok: "t"}
    if err := SaveNew(dir, a); err != nil { t.Fatal(err) }
    if err := Delete(dir, "u1"); err != nil { t.Fatal(err) }
    if _, err := os.Stat(filepath.Join(dir, a.FileName())); !os.IsNotExist(err) { t.Fatal("file still exists") }
}
```

- [ ] **Step 2: 写失败测试（Pool.Remove）**

构造 `New([]*auth.Auth{{UserID:"u1"}}, Config{}, stateFile)`，`Remove("u1")` 返回 true，`List()` 为空，`state.json` 不含 u1；再 `Remove("nope")` 返回 false。

- [ ] **Step 3: 跑测试确认失败**

Run: `cd apps/codearts && go test ./internal/auth ./internal/pool`
Expected: `undefined: Delete` / `undefined: Remove`。

- [ ] **Step 4: 实现 `auth.Delete` 与 `Pool.Remove`**

`Delete` 用 `(&Auth{UserID: userID}).FileName()` 拼路径，`os.Remove`，`IsNotExist` 不算错。`Remove` 在 `p.mu` 下移除并 `saveState()`。

- [ ] **Step 5: 写失败测试（HTTP 路由）**

```go
func TestAdminDeleteAccount(t *testing.T) {
    // 构造 handler + pool，POST /admin/api/accounts/delete {"uid":"u1"}
    // 断言 200、pool 为空、auth 文件不存在、chats/convAcct 已清理
}
func TestAdminDeleteRequiresAPIKey(t *testing.T) { /* cfg.APIKey=="" -> 403 */ }
func TestAdminDeleteUnknown(t *testing.T) { /* 404 */ }
```

- [ ] **Step 6: 实现路由与内存清理**

注册 `POST /admin/api/accounts/delete`；`APIKey==""` → 403；`Pool.Remove` 失败 → 404；删除 `h.chats[uid]` 和 `h.convAcct` 中 value==uid 的条目；返回 `{ok:true, deleted:true, message:"已删除 <uid>"}`。

- [ ] **Step 7: 跑测试确认通过**

Run: `cd apps/codearts && go test ./...`
Expected: 全部 PASS。

- [ ] **Step 8: 更新 README 管理端点表**

加一行 `POST /admin/api/accounts/delete`，标注「不可恢复，仅删凭据与池内条目，不删历史用量」。

- [ ] **Step 9: 提交**

```bash
git add apps/codearts/internal/auth apps/codearts/internal/pool apps/codearts/internal/server apps/codearts/README.md
git commit -m "feat(admin): add account delete endpoint" -m "Allow deleting a CodeArts credential and its pool entry in one authenticated call. 增加删除 CodeArts 凭据与池内条目的管理端点。"
```

---

## Task 9: 看板第三张卡片、视图切换与默认出口

**Files:**
- Modify: `apps/hub/dashboard.html`
- Test: `apps/hub/tests/_test_codearts_switch.js`
- Modify: `apps/hub/tests/_test_accounts_realm_switch.js`

**Interfaces:**
- Consumes: `GET /realm` 的 `options`；`GET /codearts/overview`
- Produces:
  - `window.VIEW_REALM` 支持 `"codearts"`
  - 元素 `tabCodearts`、`accountsRealmCodearts`、`codeartsDefaultBtn`
  - 函数 `switchViewRealm('codearts')`、`loadCodeartsOverview()`、`applyRealmView()`

- [ ] **Step 1: 写失败测试**

```javascript
const html = dashboardHtml();
assert(html.includes('id="tabCodearts"'));
assert(html.includes("switchViewRealm('codearts')"));
assert(html.includes('id="accountsRealmCodearts"'));
// 未配置时卡片隐藏：模拟 /realm options 只有 intl/cn
assert(renderWithOptions(["intl","cn"]).tabCodearts.hidden === true);
assert(renderWithOptions(["intl","cn","codearts"]).tabCodearts.hidden === false);
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && node tests/_test_codearts_switch.js`
Expected: `AssertionError`。

- [ ] **Step 3: 加第三张卡片与视图按钮**

网关页 `realm-cards-grid` 加 `tabCodearts`（未配置 `display:none`），显示状态点、host、健康/总数、模型数、当前视图/默认出口徽章；账号页 `accountsRealmSwitch` 加 `accountsRealmCodearts`。`switchViewRealm('codearts')` 更新 `VIEW_REALM`、按钮 active 状态、卡片徽章。

- [ ] **Step 4: 把默认出口改成三段选择**

`cardGatewayCtrl` 的单个 toggle 按钮改成三个按钮（`intl` / `cn` / `codearts`），点击 POST `/realm`；`codearts` 未配置时隐藏。`toggleActiveGatewayRealm()` 保留为兼容包装，内部按三态循环。

- [ ] **Step 5: 写失败测试（默认出口与视图持久化口径）**

断言点击 `codearts` 默认出口按钮 POST `{realm:"codearts"}`；`VIEW_REALM` 不写 localStorage；`?view=codearts` 启动时视图为 codearts。

- [ ] **Step 6: 跑测试确认通过**

Run: `cd apps/hub && node tests/_test_codearts_switch.js && node tests/_test_accounts_realm_switch.js`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add apps/hub/dashboard.html apps/hub/tests/_test_codearts_switch.js apps/hub/tests/_test_accounts_realm_switch.js
git commit -m "feat(ui): add the codearts realm card and selector" -m "Add the third realm card, view button and three-way default exit selector. 增加第三张出口卡片、视图按钮与三段默认出口选择。"
```

---

## Task 10: CodeArts 账号表与启停/删除/签到操作

**Files:**
- Modify: `apps/hub/dashboard.html`
- Test: `apps/hub/tests/_test_codearts_accounts.js`

**Interfaces:**
- Consumes: `GET /codearts/overview`、`POST /codearts/accounts/{enable,disable,delete,clear-cooldown,checkin}`
- Produces: `renderCodeartsAccounts(overview)`、`codeartsAccountAction(action, uid)`

- [ ] **Step 1: 写失败测试**

```javascript
const html = dashboardHtml();
assert(html.includes('id="codeartsAccounts"'));
assert(html.includes("codeartsAccountAction('delete'"));
// 视图为 codearts 时 WorkBuddy 账号表隐藏
assert(viewModel("codearts").workbuddyAccountsHidden === true);
// 删除有二次确认
assert(html.includes("confirm("));
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && node tests/_test_codearts_accounts.js`
Expected: `AssertionError`。

- [ ] **Step 3: 实现账号表**

`VIEW_REALM === 'codearts'` 时隐藏 WorkBuddy 账号表、导入桌面凭据、产品切换、积分护栏、每日任务；显示 `codeartsAccounts` 表：账号（昵称/UID）、状态（启用/停用/冷却）、Token 剩余/到期、并发、签到、操作。操作按钮：启用、停用、清冷却、删除（二次确认，提示不可恢复）、签到。

- [ ] **Step 4: 实现数据加载与刷新**

`loadCodeartsOverview()` 拉 `/codearts/overview`，渲染统计条和账号表；操作成功后重新拉取；失败时显示可读错误，不清空旧表。

- [ ] **Step 5: 跑测试确认通过**

Run: `cd apps/hub && node tests/_test_codearts_accounts.js`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add apps/hub/dashboard.html apps/hub/tests/_test_codearts_accounts.js
git commit -m "feat(ui): add codearts account table and actions" -m "Render CodeArts accounts and wire enable, disable, cooldown, check-in and delete actions. 渲染 CodeArts 账号表并接上启用、停用、清冷却、签到与删除。"
```

---

## Task 11: OAuth 添加账号弹窗与 CodeArts 设置区块

**Files:**
- Modify: `apps/hub/dashboard.html`
- Test: `apps/hub/tests/_test_codearts_oauth.js`

**Interfaces:**
- Consumes: `POST /codearts/accounts/oauth/{start,poll,callback}`、`GET/POST /settings` 的 `codearts` 字段、`POST /codearts/test`
- Produces: `startCodeartsOAuth()`、`pollCodeartsOAuth()`、`importCodeartsCallback()`、`saveCodeartsSettings()`、`testCodeartsConnection()`

- [ ] **Step 1: 写失败测试**

```javascript
assert(html.includes('id="codeartsOauthModal"'));
assert(html.includes("oauth/start"));
assert(html.includes("next_url"));       // continue 分支
assert(html.includes('id="codeartsSettings"'));
assert(html.includes("api_key_set"));    // 不显示明文
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd apps/hub && node tests/_test_codearts_oauth.js`
Expected: `AssertionError`。

- [ ] **Step 3: 实现 OAuth 弹窗**

复用现有 modal 样式：发起授权 → 打开 `auth_url` → 每 2.5s 轮询 → `pending` 显示粘贴回调输入框 → `continue` 打开 `next_url` 并继续轮询 → `done` 刷新账号表并关闭。

- [ ] **Step 4: 实现设置区块**

设置页新增「CodeArts 接入」：启用开关、Base URL、API Key（已设置时显示占位，不显示明文）、超时/流式超时/缓存秒数、测试连接。保存走 `/settings` 的 `codearts` 字段；保存后刷新 `/realm` options 与卡片可见性。

- [ ] **Step 5: 跑测试确认通过**

Run: `cd apps/hub && node tests/_test_codearts_oauth.js`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add apps/hub/dashboard.html apps/hub/tests/_test_codearts_oauth.js
git commit -m "feat(ui): add codearts oauth and settings" -m "Add the OAuth add-account modal and the CodeArts settings section. 增加 OAuth 添加账号弹窗与 CodeArts 设置区块。"
```

---

## Task 12: i18n、数据看板口径、文档与全量验证

**Files:**
- Modify: `apps/hub/dashboard.html`
- Modify: `apps/hub/tests/_test_i18n_traditional.js`
- Modify: `apps/hub/tests/_test_settings_load.js`
- Modify: `apps/hub/README.md`
- Modify: `apps/hub/docs/superpowers/specs/2026-10-31-codearts-realm-design.md`（状态改为「已实现」）
- Modify: `docs/setup.md`
- Test: 全量

**Interfaces:**
- Consumes: 前面所有任务
- Produces: 完整的 zh / zh-Hant / en 文案；CodeArts 出口的用量筛选；可复现的部署文档

- [ ] **Step 1: 补 i18n 词典**

新增文案同时补简体、繁体、英文；跑 `node tests/_test_i18n_traditional.js` 确认没有回落中文。

- [ ] **Step 2: 数据看板筛选**

出口筛选增加 CodeArts；CodeArts 行的积分/花费列显示 `—`，请求数/Token/成功率正常统计。

- [ ] **Step 3: 更新文档**

`apps/hub/README.md` 新增「CodeArts 第三出口」章节 + `### Unreleased` 变更记录；`docs/setup.md` 写清 compose、首次配置、OAuth 添加、回滚步骤（先解绑 codearts Key）。

- [ ] **Step 4: 跑全量测试**

Run:
```bash
cd apps/hub && python tests/run_all.py
cd apps/codearts && go test ./...
```
Expected: 全部 PASS。

- [ ] **Step 5: 冒烟**

起假 codearts + hub（端口 8799），走一遍：配置 → 切出口 → 列模型 → chat 流式/非流式 → responses → messages → 添加/停用/启用/删除账号。记录命令与结果到 `docs/setup.md` 的「验证」段。

- [ ] **Step 6: 提交**

```bash
git add apps/hub/dashboard.html apps/hub/tests apps/hub/README.md apps/hub/docs docs
git commit -m "docs(codearts): finish i18n, analytics and rollout docs" -m "Complete the third-realm translations, analytics filter and deployment/rollback docs. 补齐第三出口的翻译、统计筛选与部署回滚文档。"
```

---

## Self-Review

**Spec coverage:**

- 目标 1（第三卡片/视图/持久化）→ Task 9
- 目标 2（模型目录 + 三类入口）→ Task 5、Task 6
- 目标 3（Key 绑定 + 三出口并用）→ Task 4
- 目标 4（看板账号 CRUD）→ Task 7、Task 8、Task 10、Task 11
- 目标 5（故障隔离）→ Task 3、Task 5、Task 6、Task 7
- 非目标（不重写 OAuth、不加前缀、不做积分）→ 全局约束 + Task 2/3/12
- 配置/开关/超时口径 → Task 2、Task 4
- 安全（Key 不落浏览器、面板会话）→ Task 7、Task 11
- 测试/验收/回滚 → Task 12

**Review Focus 对应测试：**

- CodeArts 不可达/Key 错 → Task 2（错误映射）、Task 5（模型 502）、Task 6（chat 502）
- 模型名冲突 → Task 4（`_cross_realm_error` 放行）、Task 6（路由）
- 关闭出口收敛 → Task 4
- OAuth continue → Task 7（代理透传）、Task 11（弹窗）
- 删除残留 → Task 8（chats/convAcct 清理）、Task 10（刷新）

**类型一致性：**

- `load_config` / `save_config` / `configured` / `masked_config` / `models` / `model_entries` / `build_chat_body` / `open_chat` / `CodeArtsAccount` 在 Task 2/3 定义，Task 4/5/6/7 消费。
- `record_usage` / `record_error` 的 `realm` 参数在 Task 6 定义并用于 Task 5/7 的调用。
- 看板元素 ID 与函数名在 Task 9/10/11 保持一致。

**比例：** 计划约 500 行，规格约 470 行；每个任务只写接口、断言和命令，不抄实现。
