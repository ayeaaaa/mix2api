# CodeArts 单进程原生接入设计

- 日期：2026-10-31
- 状态：已实现
- 目标：一个 Python 进程、一个看板、一套 API Key，同时服务 WorkBuddy 国际版 / 国内版 / 华为云 CodeArts
- 取代：`2026-10-31-codearts-realm-design.md`（那版是「hub + 独立 codearts2api 两个服务」的代理方案，已废弃）

## 1. 目标与非目标

目标：

1. CodeArts 的 OAuth 登录、token 续期、账号池、模型发现、聊天 SSE、签到全部在 `app/` 进程内完成。
2. 不再部署、不再调用独立的 codearts2api 服务；根目录 compose 只有一个 `hub`。
3. 看板仍提供三出口切换和 CodeArts 账号的增删启停、签到、OAuth 添加。
4. 华为云凭据只存在本机，不出浏览器、不进日志。

非目标：

- 不把 WorkBuddy 的 Python 网关改写成 Go。
- 不保留 codearts2api 的 WebUI / CLI / admin HTTP 服务；`reference/codearts2api` 只作协议参考。
- 不做 CodeArts 积分余额查询（上游没有余额接口，`stats.credits` 实际是健康账号数）。
- 不做按 Key 指定某个 CodeArts 账号；只支持按 Key 绑定出口。

## 2. 模块

```text
app/wb_codearts_crypto.py   纯标准库 P-256 ECDSA / DPoP / PKCE / 华为云 HMAC 签名
app/wb_codearts.py          OAuth、token 续期、账号池、模型发现、聊天流、签到
app/wb_proxy.py             realm 路由、三类入口、用量归因、/codearts/* 管理端点、/oauth/callback
app/dashboard.html          第三张出口卡片、CodeArts 账号表、OAuth 弹窗、设置区块
```

### 2.1 密码学

- P-256 ECDSA：纯 Python 点运算 + `secrets` 随机 k，JOSE raw r||s、低 S 归一化。
- DPoP：`{alg:ES256, typ:dpop+jwt, jwk:{...}}` + `{htm,htu,iat,jti}`。
- PKCE：64 字节随机 → base64url verifier；SHA-256 → challenge。
- 华为云签名：`SDK-HMAC-SHA256`，signed headers = 全部头（小写排序），
  CanonicalURI 每段 percent-encode 且末尾补 `/`，payload hash 取
  `X-Sdk-Content-Sha256`。
- 已用 Go 侧 `signRequest` / `crypto/ecdsa` 生成的向量对拍（`tests/_test_codearts_crypto.py`）。

### 2.2 账号文件

`accounts/codearts/codearts-{uid}.json`，字段与 Go 版一致，便于迁移旧 `auths/`：

```json
{
  "user_id": "...", "user_name": "...", "domain_id": "...",
  "cloud_dragon_token": "<STS security_token>",
  "access_key_id": "...", "secret_access_key": "...",
  "expiration": "2026-...Z",
  "refresh_token": "...", "code_verifier": "...",
  "client_id": "codearts-agent",
  "dpop_private_key": {"kty":"EC","crv":"P-256","x":"...","y":"...","d":"..."},
  "updated_at": 0
}
```

`accounts/codearts/state.json` 存 `err_count / cool_until / disabled / reason / last_error`。
写入走临时文件 + `os.replace`，权限 600。

### 2.3 账号池

- `pick(model, session_key)`：健康（未停用、未冷却、token 未过期）、并发未满、
  优先能服务该模型的账号；会话粘性优先回到上次账号。
- `acquire/release`：每账号 `max_concurrent`（默认 5）。
- `note_success`：清错误计数与冷却。
- `note_error`：普通错误累计 3 次冷却 10 分钟；429/传输错误软冷却 60 秒；
  refresh_token 终态失效直接停用并要求重新登录。
- token 到期前 30 分钟自动刷新；刷新串行化，失败不把健康账号误停用（除非终态）。

### 2.4 OAuth

- `start`：生成 ticket_id / secret / PKCE / DPoP，返回华为云 authorize URL；
  会话存在内存（TTL 10 分钟）。
- `poll`：现代流程等待浏览器回调；旧 ticket 通道（回调只带 secret）走
  `snap-manager/v1/login/ticket` 轮询。
- `callback`：校验 `http://127.0.0.1:<port>/oauth/callback`，`code` 换 token；
  `secret + redirect` 进入 ticket 回退并返回 `continue + next_url`。
- 浏览器 GET `/oauth/callback` 由 `wb_proxy` 处理，不需要面板会话（回调来自华为）。
- 登录成功写账号文件、加入池、标记会话 done。

### 2.5 模型

三路发现，单路失败不影响其他路：

1. agent-center：`useragents` 找 CodeAgent/智能体 → `detail` 的 `gpts.models`。
2. builtin：`/v1/model/builtin`。
3. 限时福利：`opengw.developer` 的 `/api/v1/gateway/config`；默认先幂等领取
   `/api/v1/benefit/claim`。

合并后按账号缓存 1 小时；福利模型记 `benefit=true`，聊天时加
`maas_type: benefit`。全失败时保留旧目录；没有旧目录用种子。

### 2.6 聊天

- 请求体：客户端 OpenAI messages → 标准 `/api/v2/chat/completions` body，
  `chat_id` / `prompt_cache_key` 32hex，`stream=true`，`tool_stream=true`。
- 鉴权：`x-auth-token` + AK/SK 签名；福利模型额外 `maas_type`（签名前设置）。
- 回程：`CodeArtsStream` 把标准 chunk、顶层 `delta`、`text` 全文快照、
  `output` 终态统一成 OpenAI chunk；usage 在末尾单独一帧；`[DONE]` 结束。
- 复用 wb-hub 现有 `_chat_stream_response` / `_responses_*` / `_messages_*`，
  三类入口都走同一条流。
- 上游 HTTP 200 内嵌 `error_code != 0` 在 `preflight` 阶段抛出，未开始流时可直接换号；
  已开始流时由现有处理器发 SSE error 帧。

### 2.7 签到

`/v1/ops/delivery?channel=IDE` 取活动，只处理 `campaignId` 1（每日签到）和
4（新用户），`ELIGIBLE` 领取、`CLAIMED` 补确认、`CONFIRMED/CONSUMED` 跳过。
`Agent-Type: PromptCenter`，AK/SK 签名。签到结果只影响账号的 `checkin` 字段，
不影响推理健康。

## 3. 看板

- 第三张出口卡片：华为云 host、健康/总数、模型数、离线标记。
- 默认出口循环：`国际版 → 国内版 → CodeArts`，只走服务端返回的 options。
- 账号页 CodeArts 视图：账号、状态、Token 剩余/到期、并发、签到、操作
  （启用/停用/清冷却/签到/删除）。
- OAuth 弹窗：发起授权、轮询、远端回调粘贴、`continue + next_url`。
- 设置：启用开关、单账号并发、管理/流式超时、模型缓存；不再有 Base URL/API Key。

## 4. 失败与回滚

- CodeArts 未启用：`/realm` options 不含 codearts，看板卡片隐藏，请求不会路由过去。
- 关闭出口：默认出口强制回 `intl`；已绑 codearts 的 Key 返回可读错误，不静默改绑。
- 账号全部不可用：返回 502/503 可读错误，不影响 WorkBuddy 两个出口。
- 回滚：关闭出口、解绑 Key，再回退代码；`usage.jsonl` 里的 `realm=codearts` 行保留。
- 华为云凭据只在 `accounts/codearts/`；删除账号即删除凭据文件。

## 5. 验证

- `tests/_test_codearts_crypto.py`：P-256/DPoP/PKCE/HMAC，Go 向量对拍。
- `tests/_test_codearts_native.py`：配置、账号文件、池、模型、续期、签到、OAuth。
- `tests/_test_codearts_models.py`：`/v1/models` 并集、Key 过滤、种子兜底。
- `tests/_test_codearts_routing.py`：chat/responses/messages、福利头、用量归因、错误映射。
- `tests/_test_codearts_admin.py`：面板鉴权、账号 CRUD、OAuth、设置。
- JS：第三出口、账号表、OAuth/设置。
- 全量：`cd app && python3 tests/run_all.py`，243 个套件。
