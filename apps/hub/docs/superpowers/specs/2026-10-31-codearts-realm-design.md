# CodeArts 第三出口接入设计

- 日期：2026-10-31
- 状态：已实现（`mix2api` monorepo）
- 目标仓库：`ardeyouxipianyi/workbuddy2api-hub`（主）、`HITZY2002/codearts2api`（配套小改）
- 相关文档：`CONTRIBUTING.md`、`README.md`

## 1. 背景与目标

`workbuddy2api-hub`（下称 wb-hub）目前有两个出口（realm）：`intl`（WorkBuddy 国际版）和 `cn`（WorkBuddy 国内版）。用户希望在现有出口切换里加入第三个出口 `codearts`，由 wb-hub 统一暴露地址、API Key、模型目录和看板，后端接到 `codearts2api`。

目标：

1. 看板出现第三张出口卡片和第三个视图按钮，可在 `国际版 / 国内版 / CodeArts` 之间切换；**网关默认出口**（影响未绑定 Key 的请求）持久化，视图本身只跟当前页面走。
2. 选中 CodeArts 时，`/v1/models` 返回 codearts2api 的模型目录；`/v1/chat/completions`、`/v1/responses`、Anthropic Messages 都路由到 codearts2api。
3. API Key 可以绑定到 `codearts` 出口，和现在绑定 `intl/cn` 一样；三个出口可以同时使用（不同 Key 各绑一个）。
4. 在 wb-hub 看板里可以**添加、删除、启用、停用、清冷却、签到** CodeArts 账号，不必再打开 codearts2api 的 WebUI。
5. CodeArts 不可达、未配置或被关闭时，WorkBuddy 两个出口完全不受影响。

## 2. 非目标与反目标

非目标（本期不做）：

- 不在 wb-hub 里重新实现华为云 OAuth / DPoP / token 续期。账号凭据、刷新、冷却、并发、签到仍然由 codearts2api 持有和执行。
- 不做 CodeArts 模型的名称前缀（如 `ca/`）。模型目录按出口隔离，和现在 intl/cn 的口径一致。
- 不做按 Key 绑定到**某个 CodeArts 账号**。codearts2api 的 chat 接口不支持指定账号；本期只支持按 Key 绑定到出口。
- 不把 WorkBuddy 的积分/额度护栏套到 CodeArts 账号上。CodeArts 的额度以华为云为准，看板只读展示。
- 不在 wb-hub 里实现 CodeArts 的每日签到定时任务。保留手动「签到 / 全员签到」，定时任务仍由 codearts2api 的 watchdog 负责。
- **不做 CodeArts 积分余额查询。** codearts2api 的 `overview.stats.credits` 不是余额（`Pool.Stats()` 里它是健康账号数，`Pool.List()` 每行硬编码 0，`adminCredits` 只做 token 校验）；真实余额要新写华为云计费接口，超出本期范围。看板不显示 CodeArts 积分，显示 `—` 和「以华为云控制台为准」。

反目标（即使做出来也算失败）：

- 为了接 CodeArts 把 WorkBuddy 账号池、设备指纹、积分护栏的行为改坏。
- 两个进程同时写同一份 CodeArts 凭据/状态文件，出现 token 刷新竞态。
- CodeArts 的 API Key 出现在浏览器、日志或看板响应里。
- CodeArts 掉线时 `/health` 变红、WorkBuddy 请求被拖慢或被误路由。

## 3. 现状（实现依据）

### 3.1 wb-hub 的出口模型

- `CURRENT_REALM` 是进程内全局变量，默认 `intl`；持久化在 `accounts/active_realm.json`。
- `load_persisted_realm()` / `save_persisted_realm()` 的白名单写死 `("intl", "cn")`。
- `wb_settings.REALMS = ("", "intl", "cn")`：空串表示 Key 不绑出口。
- Key 绑定优先于 `X-Realm` 头和 `?realm=`，最后才回落到全局开关（`_request_realm()`）。
- `detect_model_realm()` 会用模型名猜出口（`gpt-* → intl`、`deepseek-v4-pro* → cn`），`exclusive_realm()` 用于跨区拒绝。CodeArts 模型名与 WorkBuddy 重叠（如 `GLM-5.2`、`deepseek-v4-pro-0813`），这套猜法必须在 CodeArts 出口下绕过。
- 请求的唯一上游入口是 `open_upstream()`；`/v1/chat/completions`、`/v1/responses`、`/v1/messages` 最终都走它。
- 三个回程处理器只依赖 `account` 的三个属性：`uid`、`chat_base_url()`、`proxy`（见 `_chat_stream_response` / `_responses_*` / `_messages_*`）。这给了一个低成本复用点。
- `record_usage()` / `record_error()` 目前用 `POOL.get(account).realm` 或 `CURRENT_REALM` 决定 `row["realm"]`。
- `/v1/models` 由 `_get_v1_models()` 调 `fetch_models(realm=...)`。
- 看板有两个状态：`window.VIEW_REALM`（当前视图）和 `window.ACTIVE_GATEWAY_REALM`（网关默认出口）。两者都写死 intl/cn 的二元判断。

### 3.2 codearts2api 的接口

- 推理：`POST /v1/chat/completions`、`GET /v1/models`，`Authorization: Bearer <CA2A_API_KEY>`。
- 管理（同一把 Key）：
  - `GET /admin/api/overview` → `{stats:{total,healthy,disabled,cooling,credits}, accounts:[...], models:[...], schedule}`
  - `POST /admin/api/accounts/enable|disable|clear-cooldown` → `{uid}`
  - `POST /admin/api/checkin` → `{uid?}`（空=全员）
  - `POST /admin/api/keepalive`
  - `POST /admin/api/oauth/start` → `{session_id, auth_url, expires_in}`
  - `POST /admin/api/oauth/poll` → `{status: pending|done|error, account?}`
  - `POST /admin/api/oauth/import-callback` → `{session_id, callback_url}`
- **没有删除账号的接口**。`Pool.SyncToDir()` 可以用磁盘上的 `auths/` 全量对齐，但 wb-hub 不应该直接改远端进程的目录（同机才可行，且会和 watchdog/refresh 竞态）。
- 账号对象字段：`uid`、`nickname`/`user_name`、`disabled`、`cooling`、`until`、`reason`、`err_count`、`active_concurrent`、`max_concurrent`、`token_remaining`、`expires_at`、`checkin`。
- OAuth 登录是服务端流程：start 返回华为授权链接，poll 轮询 ticket；浏览器不在 codearts2api 所在机器时，可把 `127.0.0.1/oauth/callback?...` 地址粘贴回 import-callback。

## 4. 总体方案

### 4.1 角色边界

```text
客户端 ──► wb-hub ──► codearts2api ──► 华为云 CodeArts
             │            │
             │            └── 账号池 / token 刷新 / 冷却 / 签到 / 并发
             └── 统一地址、Key、模型目录、看板、用量记录
```

- wb-hub 是**前门 + 控制面代理**：只保存 codearts2api 的 `base_url` 和 `api_key`，不保存任何华为云凭据。
- codearts2api 仍是**账号与凭据的唯一所有者**。看板的增删启停全部通过它的 admin API 完成；删除需要给它加一个端点（见 §6）。
- 两者可以是同机（`http://127.0.0.1:7866`）或远端（HTTPS / 内网地址）。wb-hub 不假设共享文件系统。

### 4.2 请求路径

`realm=codearts` 时：

1. 入口鉴权、Key 的模型限制、并发槽位、日志与现有逻辑一致。
2. **绕过** WorkBuddy 专属逻辑：`detect_model_realm()`、`exclusive_realm()`、跨区拒绝、账号池选择、账号级护栏、realm fallback、服务端 web tools。
3. 用 `wb_codearts.build_chat_body()` 生成标准 OpenAI 请求体（始终 `stream=true` + `stream_options.include_usage=true`），POST 到 `{base_url}/v1/chat/completions`。
4. 把返回的 `urllib` 响应和 `CodeArtsAccount` 适配对象交给现有回程处理器：
   - `/v1/chat/completions`：`_chat_stream_response` / `_chat_nonstream_response`
   - `/v1/responses`：`_responses_stream_response` / `_responses_nonstream_response`（`allow_web_tools=False`）
   - `/v1/messages`：`_messages_stream_response` / `_messages_nonstream_response`
5. 用量行带 `realm="codearts"`、`account="codearts"`；积分/花费标为不可计价。

适配对象只需要：

```python
class CodeArtsAccount:
    uid = "codearts"
    realm = "codearts"
    product = "codearts"
    proxy = None
    nickname = "CodeArts"
    chat_path = "/v1/chat/completions"
    def chat_base_url(self): return base_url
    def model_disabled(self, mid): return False
    def is_cli_identity(self): return False
```

`_chat_*` 里的 `account.chat_base_url() + CHAT_PATH` 只用于出口退避的键，不参与真实请求；把这两处改成 `getattr(account, "chat_path", CHAT_PATH)`，CodeArts 的键就是正确的 `/v1/chat/completions`。

### 4.3 配置与开关

`accounts/settings.json` 新增：

```json
"codearts": {
  "enabled": false,
  "base_url": "http://127.0.0.1:7866",
  "api_key": "",
  "timeout_seconds": 30,
  "stream_timeout_seconds": 600,
  "models_ttl_seconds": 60
}
```

规则：

- `enabled` 默认 `false`，老安装行为不变。
- `codearts_configured()` = `enabled && base_url && api_key`。
- `base_url` 只接受 `http://` / `https://`，去掉尾部 `/`，拒绝带用户名密码的 URL。
- `api_key` 只存在服务端；`GET /settings` 返回 `api_key_set: true` 和空 `api_key`，不返回明文。
- 保存设置时 `api_key` 为空表示「保留原值」，和现有 Key 的语义一致。
- 未配置时：`/realm` 的 options 只有 `intl/cn`，看板不显示第三张卡片；`X-Realm: codearts` 返回 400 可读错误。
- **超时口径**：`timeout_seconds` 只用于管理调用、健康检查、模型拉取；所有 chat 调用（流式与非流式）都用 `stream_timeout_seconds`。原因是 wb-hub 对上游始终发 `stream=true`，非流式客户端也要等完整聚合；codearts2api 在账号池忙时可能先等 180s 并发槽、再按队列重试（默认 30 次 × 10s），30s 的非流式超时会把本来能成功的请求误杀。设置页的说明要写清这一点。

## 5. wb-hub 改动

### 5.1 新模块 `wb_codearts.py`

纯标准库、兼容 Python 3.9。职责：

- 配置读写与校验（`load_config` / `save_config` / `configured`）。
- HTTP 客户端：
  - `_request(method, path, body=None, timeout=None, stream=False)`
  - 默认直连（`ProxyHandler({})`），避免环境变量里的 `HTTP_PROXY` 把 `127.0.0.1` 请求带走；预留 `proxy` 配置位。
  - `Authorization: Bearer <api_key>`，`Content-Type: application/json`。
  - 错误体最多读 64 KiB；不把 Key 写进异常文本。
- 推理：
  - `models()`：GET `/v1/models`，缓存 `models_ttl_seconds`；失败返回上次成功缓存并标记 `stale`。
  - `open_chat(body, timeout)`：POST `/v1/chat/completions`，返回 `urllib` 响应对象；始终请求 SSE。
- 管理：
  - `overview()`、`oauth_start()`、`oauth_poll()`、`oauth_callback()`、`enable()`、`disable()`、`remove()`、`clear_cooldown()`、`checkin()`、`keepalive()`、`health()`。
- 模型映射：
  - `model_entries()` → wb-hub `/v1/models` 条目：`id`、`object:"model"`、`created`、`owned_by:"codearts"`、`provider:"codearts"`；不伪造能力/价格字段。
- 请求体：
  - `build_chat_body(payload, session_key)`：
    - 复制客户端字段；删除 `_` 开头的内部标记、`user`、`metadata`。
    - 复用 `merge_adjacent_tool_calls` / `repack_tool_result_blocks` / `cleanup_orphan_tool_calls` 修复工具配对。
    - **不**注入 WorkBuddy 的 `SYSTEM_PROMPT`，**不**做 DeepSeek 的 `reasoning_content` 回填。
    - `stream=true`；`stream_options={"include_usage": True}`。
    - `conversation_id`：有 `session_key` 时取其 sha256 前 32 位 hex（codearts2api 的 `validChatID` 要求 32 hex）；没有就省略，让它按首条 user 消息自推导。
- 错误映射：
  - 连接失败 / 超时 → `CodeArtsUnavailable`，上层回 502（不是 503，避免和「账号池空」混淆）。
  - 401/403 → 502「CodeArts 拒绝了配置的 API Key」。
  - 400 → 原样 400，带上游 `message`。
  - 429/5xx → 502/503，带 `Retry-After`（如果有）。
  - 流已开始后出错 → 由现有回程处理器发 SSE error 帧。

### 5.2 realm 与 Key 绑定

- `wb_settings.REALMS` 增加 `"codearts"`。
- `load_persisted_realm()` / `save_persisted_realm()` 白名单增加 `"codearts"`；启动时如果持久化的是 `codearts` 但当前未配置，回落 `intl` 并记一条日志。
- **关闭出口时的收敛**：保存 `codearts.enabled=false` 时，如果 `CURRENT_REALM == "codearts"`，服务端强制切回 `intl` 并写盘；保存响应带一条 `warning` 告诉看板「默认出口已切回国际版」。
- **已绑 Key 的行为**：Key 绑定 `codearts` 而出口被停用时，请求返回 400 可读错误（「该 Key 绑定的 CodeArts 出口已停用，请改绑或重新启用」），**不静默改绑**到 WorkBuddy。`_clean_key_entry()` 只在 realm 不在 `REALMS` 时清空；本期 `codearts` 始终在 `REALMS` 里，所以读路径不会把绑定改掉。
- **回滚 runbook**：回退到不支持 `codearts` 的旧版本前，先把所有绑 `codearts` 的 Key 改回 `""`/`intl/cn`，否则旧版 `_clean_key_entry()` 会把未知 realm 读成 `""`，流量会跟着面板默认出口走，可能落到 WorkBuddy。README 的回滚说明要写这一条。
- `_get_realm()` 的 `options` 动态生成：`["intl","cn"]` + 配置好时的 `"codearts"`；如果 `CURRENT_REALM` 不在 options 里（配置被删/关闭），`_get_realm()` 返回前先收敛到 `intl` 并持久化，避免出现「current 不在自己的 options 里」。
- `_handle_settings_save()` 里 Key 的 `realm` 校验从 `("", "intl", "cn")` 改为 `("", "intl", "cn", "codearts")`；绑定 `codearts` 但未配置时返回 400。
- `detect_model_realm()` / `exclusive_realm()` / `_cross_realm_error()` 在 `realm == "codearts"` 时直接放行。
- `open_upstream_with_fallback()` 不参与 CodeArts；CodeArts 永远不 fallback 到 WorkBuddy 出口。
- `LIMIT_REALMS` 保持 `("intl","cn")`：账号级护栏不扩展到 CodeArts。

### 5.3 模型目录

`_get_v1_models()` 增加分支：

```python
if req_realm == "codearts":
    data = wb_codearts.model_entries()
    # 只应用 Key 的模型限制；跳过 pinned / models_only_available / tier 标注
```

- 返回 `catalog` 元信息：`{source:"codearts2api", fetched_at, stale}`。
- codearts 不可达且无缓存 → 502，消息里带 base_url 和「检查 codearts2api 是否在运行」。
- 模型 ID 保持上游原样（区分大小写），不额外造小写别名（codearts2api 自己已经返回别名）。

### 5.4 Chat 路由

`_dispatch_chat_post()` 在完成背景请求拦截、模型别名、Key 并发检查后：

```python
req_realm = explicit_realm or CURRENT_REALM
if req_realm == "codearts":
    return self._handle_codearts_chat(payload, session_key, req_realm)
```

`_handle_codearts_chat()`：

- 校验 `wb_codearts.configured()`，否则 503/400 可读错误。
- `body = wb_codearts.build_chat_body(payload, session_key)`
- `upstream, account, effort = wb_codearts.open_chat(body, timeout=...)`
- `with upstream:` 按 `want_stream` 调现有 `_chat_stream_response` / `_chat_nonstream_response`。
- 不占 `account_slot`、不做 WorkBuddy 的 `PinnedAccount` / `RateLimited` 逻辑；异常统一映射。

### 5.5 Responses / Anthropic

- `_handle_responses()`：在 `chat_req = responses_to_chat(...)` 之后、`open_upstream_with_fallback()` 之前分支：

```python
if req_realm == "codearts":
    upstream, account, effort = wb_codearts.open_chat(chat_req, ...)
    with upstream:
        if compaction: ...
        if want_stream: return self._responses_stream_response(..., allow_web_tools=False)
        return self._responses_nonstream_response(..., allow_web_tools=False)
```

- `_responses_stream_response` / `_responses_nonstream_response` 增加 `allow_web_tools=True` 参数；CodeArts 传 `False`，跳过 `follow_up_with_tool_results()`（那是 WorkBuddy 的 web_search/web_fetch 代跑）。
- `_handle_messages()` 同理；`_messages_*` 本身没有 web tools，直接复用。
- Anthropic Messages 仍受 `WB_ENABLE_ANTHROPIC_MESSAGES` 总开关约束，不为 CodeArts 单独放开。

### 5.6 用量与计费

- `record_usage()` / `record_error()` 增加 `realm=None` 参数；有值时优先于 `POOL.get(account)` 和 `CURRENT_REALM`。
- CodeArts 调用传 `realm="codearts"`、`account="codearts"`。
- 用量行带 `cost_policy="codearts:unpriced"`；价格解析器对该策略返回「无价格」，看板显示 `—`，不拿 WorkBuddy 价目表估算 CodeArts。
- CodeArts 上游返回的 usage 优先使用；没有 usage 时沿用现有文本估算，并保留 `usage_missing` 标记。
- 看板**不显示 CodeArts 积分**：codearts2api 没有真实余额接口，`overview.stats.credits` 实际是健康账号数。CodeArts 视图的统计条只显示账号健康数、模型数、请求数、Token、成功率；积分位显示 `—`，提示「以华为云控制台为准」。

### 5.7 健康检查与错误

- `GET /health` 增加 `codearts: {configured, ok, latency_ms, accounts, models}`；CodeArts 掉线**不**影响整体 `ok`，只作为子字段。
- `GET /codearts/overview` 是看板用的主数据源；内部缓存 5 秒，避免看板轮询打爆 codearts2api。
- 所有 CodeArts 异常都在 `wb_codearts` 边界转成可读中文消息，日志里只记 base_url 的 host，不记 Key。

### 5.8 看板 API（新增，全部要求面板会话）

前缀 `/codearts/`，加入 `MANAGEMENT_PATH_PREFIXES` 和 `_is_panel_route()`，不参与 CORS。

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/codearts/overview` | 状态 + 账号 + 模型 + 签到/调度信息 |
| GET | `/codearts/accounts` | 账号列表（overview 的 accounts 投影） |
| POST | `/codearts/accounts/oauth/start` | 代理 codearts2api 发起授权 |
| POST | `/codearts/accounts/oauth/poll` | 代理轮询 |
| POST | `/codearts/accounts/oauth/callback` | 代理粘贴回调地址 |
| POST | `/codearts/accounts/enable` | `{uid}` |
| POST | `/codearts/accounts/disable` | `{uid, reason?}` |
| POST | `/codearts/accounts/delete` | `{uid}`，代理新端点 |
| POST | `/codearts/accounts/clear-cooldown` | `{uid}` |
| POST | `/codearts/accounts/checkin` | `{uid?}` |
| POST | `/codearts/keepalive` | 全员保活 |
| POST | `/codearts/test` | 测试连接（healthz + overview） |

- 这些端点只接受面板会话（`_panel_ok()`），**不接受**普通 API Key。原因是删除/停用是破坏性操作，API Key 可能被分发给客户端。
- 返回体不透传 `api_key`；OAuth 的 `auth_url` 只回给已登录面板。
- OAuth 代理要原样保留 `status:"continue"` 和 `next_url`，不能只处理 `pending|done|error`。
- 上游 404（老版本 codearts2api 没有 delete）→ 409 + 「请升级 codearts2api 以支持删除账号」。

### 5.9 看板 UI

**网关页**

- 第三张卡片 `tabCodearts`（未配置时 `display:none`）：状态点、`base_url` 的 host、`健康/总数`、`模型数`、当前视图/默认出口徽章。**不显示积分**（没有真实余额）。
- 「网关默认出口」卡片从两态按钮改成三段选择：`国际版 | 国内版 | CodeArts`；CodeArts 未配置时隐藏。点击 POST `/realm`。
- 顶部统计条在 CodeArts 视图下切换为 CodeArts 口径（请求数、Token、成功率、账号健康），隐藏 WorkBuddy 积分卡；积分位显示 `—`。
- **持久化口径**：持久化的是「网关默认出口」（`ACTIVE_GATEWAY_REALM`，服务端写盘），和现在 intl/cn 的行为一致。`VIEW_REALM` 仍只是当前页面的视图，支持 `?view=codearts`，不额外写 localStorage；刷新后视图回到默认出口。这样「点一下视图按钮」不会悄悄改掉所有未绑定 Key 的路由。

**账号页**

- 第三个视图按钮 `CodeArts`。
- `VIEW_REALM === 'codearts'` 时：
  - 隐藏 WorkBuddy 的账号表、导入桌面凭据、产品切换、积分护栏、每日任务等控件。
  - 显示 CodeArts 账号表：`账号(昵称/UID)`、`状态(启用/停用/冷却)`、`Token 剩余/到期`、`并发`、`签到`、`操作`。
  - 操作按钮：`启用`、`停用`、`清冷却`、`删除`（二次确认，提示不可恢复）、`签到`。
  - 顶部按钮：`+ 添加账号 (OAuth)`、`全员签到`、`保活`、`打开 codearts2api 控制台`。
- 添加账号弹窗复用 codearts2api 的 UX：
  1. POST start，展示并打开 `auth_url`；
  2. 每 2.5s POST poll；
  3. `pending` 时显示「等待浏览器授权」，并提供粘贴 `127.0.0.1/oauth/callback?...` 的输入框 + `中转回调` 按钮；
  4. 中转回调可能返回 `status:"continue"` + `next_url`（粘贴的是带 portal secret 的回调地址）：弹窗要打开 `next_url` 并继续轮询，不能停在「中转成功」；
  5. `done` 后刷新账号表并关闭弹窗。
- 删除成功后刷新 overview；如果被删账号在某个 Key 的 `accounts` 限制里，保留该 UID 不主动清理（无害），但在 Key 编辑处提示「该账号已不存在」。

**设置页**

- 新增「CodeArts 接入」区块：
  - 开关 `启用 CodeArts 出口`
  - `Base URL`（占位 `http://127.0.0.1:7866`）
  - `API Key`（密码框；已保存时显示 `已设置`，留空表示不修改）
  - `超时` / `流式超时` / `模型缓存秒数`
  - `测试连接` 按钮 → POST `/codearts/test`
- 保存走现有 `/settings` POST 的 `codearts` 字段；保存后刷新 `/realm` options 和看板卡片可见性。

**其他页面**

- 数据看板的出口筛选增加 `CodeArts`；CodeArts 行的积分/花费列显示 `—`，请求数/Token/成功率正常统计。
- 模型清单页显示 `owned_by=codearts` 徽章。
- 任务页/调度条在 CodeArts 视图下隐藏（签到仍可在账号表手动触发）。

### 5.10 i18n 与文案

- 新增文案同时补简体、繁体、英文词典（`tests/_test_i18n_traditional.js`、`tests/_test_ui_language.py` 会钉）。
- 文案统一用「CodeArts」而不是「码道」，避免和 WorkBuddy 的「国内版」混淆。
- 错误文案示例：
  - 未配置：「CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。」
  - 不可达：「连不上 CodeArts（{host}）：请确认 codearts2api 正在运行，且 wb-hub 能访问该地址。」
  - Key 被拒：「CodeArts 拒绝了当前 API Key，请检查设置页的 Key 是否与 codearts2api 的 CA2A_API_KEY 一致。」

### 5.11 代码落点清单

| 文件 | 改动 |
|---|---|
| `wb_codearts.py` | 新增：配置、HTTP 客户端、模型缓存、请求体、适配对象、错误映射 |
| `wb_settings.py` | `REALMS` 加 `codearts`；`codearts_config` / `set_codearts_config` / `codearts_configured`；Key realm 校验 |
| `wb_proxy.py` | realm 持久化白名单；`_get_realm` options；`_get_v1_models` 分支；`_dispatch_chat_post` / `_handle_responses` / `_handle_messages` 分支；`_handle_codearts_*`；`/codearts/*` 路由；`record_usage` / `record_error` 的 `realm` 参数；`allow_web_tools` 参数；`chat_path` 兼容 |
| `dashboard.html` | 第三张卡片、三段默认出口、第三个视图按钮、CodeArts 账号表与 OAuth 弹窗、设置区块、统计/筛选适配、i18n |
| `tests/` | 新增 3~4 个套件；更新现有 realm/UI/settings 相关断言 |
| `README.md` | 新增「CodeArts 第三出口」章节 + `### Unreleased` 变更记录 |
| `docs/` | 本设计文档 |

## 6. codearts2api 改动

### 6.1 删除账号端点

新增 `POST /admin/api/accounts/delete`（`withAuth`），请求 `{uid}`：

1. `Pool.Remove(uid)`：在 `p.mu` 下从 `p.accounts` 移除；找不到返回 false；`saveState()` 重写 `state.json`（不再包含该账号）。
2. `auth.Delete(dir, uid)`：删除 `auths/codearts-{uid}.json`；文件不存在不算失败。
3. Handler 清理内存映射：
   - `delete(h.chats, uid)`（账号默认 chat）
   - 遍历 `h.convAcct`，删除 value == uid 的条目
   - 可选：清 `availability` 里该账号的探测缓存
4. 返回 `{ok:true, deleted:true, message:"已删除 <uid>"}`；不存在返回 404 `{ok:false, message:"account not found"}`。
5. 正在进行的请求不强制中断；它们持有 `*Account` 指针，跑完后自然结束。
6. **fail closed**：如果 `h.cfg.APIKey == ""`，删除端点直接 403（`admin disabled: no api_key configured`）。当前版本启动时已经强制要求 API Key，这一条是给旧版本/异常配置的兜底，避免一个无鉴权的 listener 能永久删凭据。

配套：

- `internal/auth/auth.go`：`Delete(dir, userID string) error`，复用 `FileName()`。
- `internal/pool/pool.go`：`Remove(name string) bool`。
- `internal/server/handler.go`：注册路由。
- 测试：`auth` 删除、`pool.Remove`、handler 删除路由 + 404 + 内存映射清理。
- README：管理端点表加一行，标注「不可恢复」。

### 6.2 为什么不让 wb-hub 直接删文件

- 只有同机/共享卷才可行；远端部署做不到。
- 直接改 `auths/` 需要再调 `/admin/api/reload`，而 `SyncToDir` 会整体替换账号切片，正在进行的请求和 `state.json` 的时序都不可控。
- 一个显式端点把「删文件 + 移出池 + 清映射」放在同一个进程里，语义清楚，也能返回 404/错误。

## 7. 兼容性与数据

- `enabled=false` 时，`/realm` options、看板卡片、Key 绑定选项都不出现；老安装行为不变。
- 已有 `usage.jsonl` 行没有 `realm=codearts`，聚合逻辑不受影响。
- 新增的 CodeArts 用量行带 `realm="codearts"`；旧版看板回滚后会把它们当成未知出口，建议回滚时先关闭 CodeArts 出口（数据保留，不再新增）。
- `accounts/settings.json` 新增 `codearts` 对象；旧版本读到未知键会忽略。
- 不迁移、不改写任何 WorkBuddy 账号文件。

## 8. 安全

- CodeArts API Key 只存在服务端 `settings.json`；`GET /settings` 只返回 `api_key_set`。
- `/codearts/*` 管理端点要求面板会话，不认普通 API Key；降低「拿到客户端 Key 就能删号」的风险。
- 日志只记 `base_url` 的 host，不记 Key、不记 `auth_url` 的完整 query。
- `base_url` 校验 scheme，拒绝 URL 内嵌凭据；这是管理员配置项，仍建议只在内网/本机暴露 codearts2api 的 admin API。
- 删除账号是破坏性操作：看板二次确认；codearts2api 端点只删凭据和池内条目，不删历史用量。
- 不把 codearts2api 的 WebUI 直接反代到公网；如需远程，套 TLS + 访问控制。

## 9. 测试计划

### 9.1 Python（wb-hub）

- `tests/_test_codearts_client.py`
  - 配置默认值/校验/密钥保留语义
  - 假 codearts 服务：models 缓存、stale 回退、chat 请求体（stream/usage/conversation_id）
  - 错误映射：401/400/429/5xx/连接失败
  - 管理调用：overview/enable/disable/delete/checkin/oauth start+poll
- `tests/_test_codearts_realm.py`
  - realm 持久化白名单、未配置回落
  - `enabled=false` 且当前出口是 codearts → 强制回 intl 并写盘
  - 已绑 codearts 的 Key 在出口停用后 → 400 可读错误，不静默改绑
  - Key 绑定 codearts、`/realm` options
  - `detect_model_realm` / `exclusive_realm` 在 codearts 下不拦截
  - `LIMIT_REALMS` 不包含 codearts
- `tests/_test_codearts_routing.py`
  - 假 codearts 服务 + wb-hub handler：
    - `/v1/models?realm=codearts`
    - `/v1/chat/completions` 流式/非流式
    - `/v1/responses` 流式/非流式
    - `/v1/messages`（在开关打开时）
    - 用量行 `realm=codearts`、`cost_policy=codearts:unpriced`
    - WorkBuddy 池不被调用（用假 POOL 断言零调用）
- `tests/_test_codearts_admin.py`
  - `/codearts/*` 未登录面板 → 401
  - 普通 API Key 不能调用删除
  - Key 不出现在任何响应/日志
  - OAuth 中转返回 `continue` + `next_url` 时原样透传
  - 上游 404 → 409 可读提示
  - codearts2api 未配置 API Key 时删除端点 403（Go 侧测试）

### 9.2 JavaScript（wb-hub 看板）

- `tests/_test_codearts_switch.js`
  - 第三张卡片/第三个视图按钮的显示条件
  - `VIEW_REALM='codearts'` 时隐藏 WorkBuddy 专属区块
  - 默认出口三段选择的 POST 载荷
  - 设置区块保存/回填（Key 留空不覆盖）
- 更新 `_test_accounts_realm_switch.js`、`_test_settings_load.js`、`_test_i18n_traditional.js` 里写死两态的断言。

### 9.3 Go（codearts2api）

- `internal/auth/auth_test.go`：Delete 存在/不存在/路径安全。
- `internal/pool/pool_test.go`：Remove 命中/未命中/state 重写。
- `internal/server/admin_test.go`：delete 路由鉴权、404、清理 `chats`/`convAcct`。

### 9.4 冒烟

1. 起假 codearts2api（stdlib `http.server`）和 wb-hub（端口 8799），配置 base_url/key，启用。
2. 看板：三张卡片、切换、添加账号（假 OAuth start/poll）、停用/启用/删除、设置保存。
3. 端到端：`/v1/models`、chat 流式/非流式、responses、messages 各一条，确认收到真实响应。
4. 真机（有 CodeArts 账号时）：用真实 codearts2api 走一遍 OAuth 添加 + 一条真实对话 + 删除。
5. `python tests/run_all.py` 全绿；`python scripts/check_clean_checkout.py` 干净。

## 10. 验收标准

- 未配置 CodeArts 时，老安装的 `/realm`、看板、Key 绑定、请求行为完全不变。
- 配置并启用后，看板可切换三出口；CodeArts 模型出现在 `/v1/models`，三类入口都能拿到回复。
- 看板可完成 CodeArts 账号的添加（OAuth）、停用、启用、清冷却、签到、删除。
- CodeArts 掉线时 WorkBuddy 两个出口的请求成功率、延迟、`/health` 主状态不受影响。
- 任何响应、日志、看板 DOM 中都不出现 CodeArts API Key。
- 新增/更新测试全绿，README 与 i18n 同步。

## 11. 风险与回滚

| 风险 | 缓解 |
|---|---|
| CodeArts 模型名与 WorkBuddy 重叠，被旧猜法误路由 | codearts 分支在猜法之前；`exclusive_realm` 直接放行；测试钉住 `deepseek-v4-pro-0813` |
| codearts2api 版本旧，没有 delete 端点 | 409 可读提示；README 标注最低版本 |
| 远端 codearts2api 网络抖动拖慢 wb-hub | 独立超时、5s overview 缓存、失败快速返回；不占 WorkBuddy 账号槽位 |
| 看板二元判断遗漏，CodeArts 视图显示 WorkBuddy 数据 | 单独 `renderCodeartsView()`；JS 测试断言隐藏/显示 |
| 删除账号误操作 | 二次确认；只删凭据不删用量；端点返回明确结果 |
| 关闭出口后默认出口仍指向 codearts，或已绑 Key 静默改绑 | 保存 `enabled=false` 时强制收敛到 intl；已绑 Key 返回 400；回滚 runbook 要求先解绑 |
| 回滚后 `realm=codearts` 的旧用量行 | 回滚前先关闭出口；聚合对未知 realm 归入「其他」，不崩溃 |

回滚方式：设置页关闭 `enabled`，或回退代码；不涉及数据迁移。codearts2api 的 delete 端点是增量接口，回退无副作用。

## 12. 我替你做的决定（可在评审时推翻）

1. **账号仍归 codearts2api 管**，wb-hub 只做代理；不在 Python 里重写华为云 OAuth/DPoP。
2. **删除端点加在 codearts2api**，不让 wb-hub 直接改 `auths/` 目录。
3. **模型不加前缀**，按出口隔离；切换出口即切换模型目录。
4. **CodeArts 用量不按 WorkBuddy 价目表计价**，看板显示 `—`；**不显示 CodeArts 积分**，因为 codearts2api 没有真实余额接口，`stats.credits` 实际是健康账号数。积分位提示「以华为云控制台为准」。
5. **CodeArts 管理端点只认面板会话**，不认普通 API Key。
6. **添加账号只做 OAuth**，不做手工粘贴 auth JSON（后续可加）。
7. **不做按 Key 绑定 CodeArts 账号**，只支持绑定出口；codearts2api 没有按请求指定账号的接口。
8. **CodeArts 视图隐藏 WorkBuddy 专属任务/调度/积分护栏**，避免误操作和误读。
9. **持久化的是网关默认出口，不是视图。** `VIEW_REALM` 仍是当前页面的视图，支持 `?view=codearts`，不写 localStorage；避免「点视图按钮」改掉所有未绑定 Key 的路由。
10. **所有 chat 调用都用 `stream_timeout_seconds`**（wb-hub 对上游始终 stream=true），`timeout_seconds` 只用于管理/健康/模型拉取。
11. **关闭 CodeArts 出口时强制把默认出口收敛回 `intl`**；已绑 codearts 的 Key 报可读错误，不静默改绑。
12. **OAuth 中转必须处理 `continue` + `next_url`**，否则远端部署的粘贴回调流程会卡住。

## 13. 需要你确认

- 是否接受在 `codearts2api` 增加 `POST /admin/api/accounts/delete`（约 40 行 + 测试）？这是「看板里能删账号」的前提。
- CodeArts 账号添加是否只做 OAuth 就够了？还是也要支持粘贴 `auths/codearts-*.json`？

## 14. 实现落点（2026-10-31）

- `apps/hub/wb_codearts.py`：配置、直连 HTTP、模型缓存、请求体、`CodeArtsAccount`、管理 API、健康检查。
- `apps/hub/wb_proxy.py`：`codearts` 出口持久化与收敛、`/v1/models` 分支、三类入口分支、`/codearts/*` 管理代理、用量 `realm` 归因、`/health` 子字段。
- `apps/hub/wb_settings.py`：`REALMS` 加 `codearts`，配置读写薄包装。
- `apps/hub/dashboard.html`：第三张卡片、三出口视图、CodeArts 账号表、OAuth 弹窗、设置区块、i18n。
- `apps/codearts`：`auth.Delete` / `auth.Exists`、`Pool.Remove`、`POST /admin/api/accounts/delete`、测试与 README。
- 与设计的差异：
  - 默认出口卡片仍是「循环到下一个出口」的按钮（`intl → cn → codearts`），没有改成三段并排控件；行为与设计一致，改动面更小。
  - `CodeArtsAccount` 适配对象直接复用现有回程处理器，没有为 CodeArts 单独写一套 SSE/JSON 转换。
  - 模型刷新走新增的 `POST /codearts/models/refresh`，不复用 WorkBuddy 的 `/catalog/refresh`。
  - CodeArts 视图的统计条显示健康/总数、模型数、冷却、停用；不显示积分（见 §2）。

