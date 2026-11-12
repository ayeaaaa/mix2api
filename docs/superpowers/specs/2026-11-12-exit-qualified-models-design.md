# 出口限定模型 ID 设计

日期：2026-11-12
状态：设计待评审
基线：`origin/v2` v2.3.0
相关：PR #14（活动出口优先）、PR #15（Buddy 签到状态）；两者与本设计独立，合并后再 rebase。

## 1. 意图

网关现在有四个出口：

| 出口 | Provider ID | Realm | 说明 |
|---|---|---|---|
| WorkBuddy 国际版 | `workbuddy` | `intl` | 与国内版共用 provider |
| WorkBuddy 国内版 | `workbuddy` | `cn` | 与国际化版共用 provider |
| CodeArts | `codearts` | `codearts` | 华为 |
| Raccoon | `raccoon` | `raccoon` | 小浣熊 |

同一个上游模型可能同时出现在多个出口。现在客户端只能靠 `realm`、API Key 绑定或活动出口隐式选择，模型 ID 本身无法表达“我要 CodeArts 的 deepseek，而不是国内 WorkBuddy 的 deepseek”。

本次改动要让所有出口同时可用，并让客户端用**出口限定模型 ID**显式选择：

```text
intl/deepseek-v4.1-flash
cn/deepseek-v4.1-flash
codearts/deepseek-v4.1-flash
raccoon/raccoon-090dd3
```

这不是“同一个请求并行打到多个出口”。请求仍然只走一个出口；变化的是模型 ID 可以携带出口，路由从隐式变成显式。

## 2. 目标与非目标

### 目标

- 增加出口限定的规范模型 ID：`<exit>/<upstream-model>`。
- 增加全出口模型目录 `GET /v1/models?realm=gateway`，同一上游模型在不同出口有不同 ID。
- Chat、Responses、Messages 都接受出口限定 ID，并在发给上游前去掉前缀。
- 不带前缀的旧模型 ID 保持现有解析顺序和兼容行为。
- API Key 的 realm 绑定仍是硬策略，出口前缀不能越权。
- API Key 的模型白名单同时支持旧裸 ID 和新出口限定 ID。
- 用量行保留上游原始 `model` 用于计价，同时用 `requested_model` 记录客户端请求的出口限定 ID。
- 看板、Key 模型选择、统计矩阵可以逐步采用全出口目录，不要求一次性重写。
- 不修改账号池、积分、调度、签到、定价数据。

### 非目标

- 不做单请求多出口 fan-out、竞速、镜像或自动挑最便宜出口。
- 不做跨 provider 自动回退。WorkBuddy 内部的 intl/cn 回退只在没有显式出口时保留。
- 不改变 `realm=all` 在 `/v1/models` 和 Chat 中的“仅 WorkBuddy intl/cn”语义。
- 不把四个 provider 的积分、模型倍率、模型能力强行统一。
- 不重写现有用量行，不要求历史数据迁移。
- 不在本设计里新增 provider；只让现有出口可被显式选择。

## 3. 当前行为与约束

### 3.1 模型目录

`internal/api/models.go` 当前规则：

- 多 provider 时，`/v1/models` 不带 `realm` 返回 400。
- `realm=all` 只合并 WorkBuddy 的 `intl`、`cn`；按裸模型 ID 小写去重，先 `intl` 后 `cn`。
- CodeArts、Raccoon 不参与 `realm=all`。
- 具体 realm 直接返回该 provider 的模型目录，`id` 是上游裸 ID。
- 测试明确锁定 `realm=all` 不泄漏非 WorkBuddy 模型（`TestModelsRealmAllUnion`、`TestModelsRealmAllLockedWithoutWorkBuddy`）。

### 3.2 Chat 路由

`internal/api/chat.go` 的 `resolveChatProvider` 当前顺序：

1. API Key 绑定 realm（硬策略，压过所有请求级 realm）。
2. 显式 realm：`X-Realm` → `?realm` → body `realm`。
3. `realm=all`：只在 WorkBuddy `intl`/`cn` 里按模型匹配。
4. 单 provider：直接使用。
5. 多 provider 且无 realm：按注册顺序做模型匹配；PR #14 合并后先尝试活动出口，再回退到其余 provider。
6. 匹配不到返回 404。

`Provider.OpenChat` 收到的 `ChatRequest.Model` 和 `Raw["model"]` 都是客户端原始模型 ID；provider 不做前缀解析。

### 3.3 API Key

- `Entry.Realm` 是绑定出口，`keys.BoundRealm` 归一化；`all` 和未知 realm 被拒绝。
- `Entry.Models` 是 `fnmatch` 风格的白名单，大小写不敏感；空表示不限制。
- 当前白名单匹配的是请求里的完整模型字符串。`deepseek/*` 不会匹配 `cn/deepseek-v4.1-flash`。

### 3.4 用量与看板

- `usage.RecordInput.Model` 来自 provider 的 `UsageSnapshot.Model`（上游原始 ID）。
- 当请求模型和计费模型不同时，`requested_model` 记录请求侧 ID。
- 分析表当前按 `model` 聚合；`realm` 只做过滤/账号归属。
- 看板统计矩阵用 `/v1/models?realm=all` 作为 WorkBuddy 模型白名单；Key 模型选择器先请求不带 realm 的 `/v1/models`，失败后再补 `intl`/`cn`。
- `realm=all` 在用量接口里表示“全部 realm”，与模型/Chat 的 WorkBuddy-only 语义不同；这是现有重载，本设计不改变它。

## 4. 命名与解析规则

### 4.1 规范 ID

```text
<exit>/<upstream-model>
```

- `exit`：已注册 provider 的 `Realm()`，大小写不敏感，规范化为小写。
- `upstream-model`：provider 自己声明的模型 ID，保留原始大小写。
- 只按第一个 `/` 切分；上游模型 ID 可以继续包含 `/`。
- 例：`cn/deepseek-ai/DeepSeek-V3` → exit `cn`，upstream `deepseek-ai/DeepSeek-V3`。

### 4.2 为什么不用 `provider/model`

WorkBuddy 的 `intl` 和 `cn` 共用 provider ID `workbuddy`。`workbuddy/deepseek...` 无法区分国际版和国内版，会丢掉本设计最重要的出口差异。因此规范前缀使用**出口/realm**，不是 provider 名。

如果未来需要更长的可读别名，可以额外接受 `workbuddy-intl/...`、`workbuddy-cn/...`，但规范 ID 仍是 `intl/...`、`cn/...`，避免一个模型两个规范名。

### 4.3 裸 ID 兼容

- 不带 `/` 的模型 ID 继续按当前逻辑解析。
- 只有**已注册 provider realm** 的第一段才被解释为出口；第一个 `/` 前不是已注册 realm 时，整个字符串按裸模型 ID 处理。
- 因此不存在“未知出口 404”：`openai/gpt-4o`、`meta-llama/Llama-3` 这类带 `/` 的上游裸模型可以继续透传。拼错的出口（例如 `codeart/foo`）会作为裸模型发给当前解析出的 provider，由上游返回它自己的错误。
- 第一个 `/` 前是已注册 realm，但后面为空 → 400。
- 第一个 `/` 前是已注册 realm → 出口限定 ID，不再尝试当裸 ID。
- 出口前缀是保留命名空间。若某个 provider 的裸模型 ID 本身以 `cn/` 开头，必须用该出口的限定形式访问（例如 `intl/cn/foo`），不能再用裸 `cn/foo`。

### 4.4 解析接口

新增 `internal/api/modelref.go`：

```go
type modelRef struct {
    Raw       string
    Exit      string // 小写；裸 ID 时为空
    Upstream  string // 发给 provider 的原始 ID
    Canonical string // 有出口时 exit + "/" + upstream；否则 Raw
    Qualified bool
}

func parseModelRef(raw string, isExit func(string) bool) (modelRef, error)
```

`isExit` 由 `Server.providerForRealm` 提供，只在已注册 provider 中判断，避免把任意 `foo/bar` 误判为出口。`parseModelRef` 不做 catalog 查询，因此不会因为目录缓存过期而拒绝请求。

## 5. API 契约

### 5.1 `GET /v1/models?realm=gateway`

`gateway` 不是 provider realm，而是模型发现/路由作用域；`providerForRealm("gateway")` 必须继续返回 nil，不能把它当 provider。

新增全出口目录，返回所有已注册 provider 的模型：

```json
{
  "object": "list",
  "realm": "gateway",
  "data": [
    {
      "id": "cn/deepseek-v4.1-flash",
      "qualified_id": "cn/deepseek-v4.1-flash",
      "upstream_id": "deepseek-v4.1-flash",
      "provider": "workbuddy",
      "realm": "cn",
      "object": "model",
      "...": "保留 provider 原有元数据"
    },
    {
      "id": "codearts/deepseek-v4.1-flash-1017",
      "qualified_id": "codearts/deepseek-v4.1-flash-1017",
      "upstream_id": "deepseek-v4.1-flash-1017",
      "provider": "codearts",
      "realm": "codearts",
      "object": "model"
    }
  ],
  "catalogs": {
    "intl": {"source": "remote", "fetched_at": 1700000000},
    "cn": {"source": "remote", "fetched_at": 1700000000},
    "codearts": {"source": "codearts"},
    "raccoon": {"source": "raccoon"}
  }
}
```

规则：

- `data[].id` 在 gateway 作用域使用规范 ID。
- `data[].upstream_id` 是 provider 原始 ID。
- `data[].qualified_id` 始终等于规范 ID，方便从具体 realm 的响应迁移。
- `data[].provider` 取 `provider.ID()`；`data[].realm` 取 `provider.Realm()`。
- 按 `lower(qualified_id)` 去重；同一出口同一模型只保留一条。
- 单个 provider 目录失败不遮住其他 provider；只有全部失败才返回错误。
- `force` 继续透传给每个 provider。
- `catalogs` 是新增字段，按 realm 汇总 catalog provenance；gateway 响应不再使用单数 `catalog`。
- 没有 provider 时返回 404；不是 400，因为这是“无出口可用”，不是参数错误。

### 5.2 `GET /v1/models?realm=<具体出口>`

保持向后兼容：

- `id` 仍是上游裸 ID。
- 新增 `qualified_id`、`upstream_id`、`provider`、`realm` 字段。
- `catalog` 字段保持原样。
- 这样旧的模型选择器、账号禁用模型、看板模型页不需要立即改。

### 5.3 `GET /v1/models?realm=all`

- 语义保持不变：只合并 WorkBuddy `intl`/`cn`，按裸 ID 去重，`id` 保持裸 ID。
- 可以补充 `qualified_id`、`upstream_id`、`provider`、`realm` 字段，但不改变 `id` 和顺序。
- 不把 CodeArts/Raccoon 加进 `realm=all`。

### 5.4 无 realm

- 单 provider：保持默认该 provider。
- 多 provider：保持 400，但错误信息补充“可用 `realm=gateway` 获取全出口目录”。

### 5.5 API Key 绑定

- Key 绑定 realm 为具体出口时，`realm=gateway` 只返回该出口的模型，仍使用规范 ID；不得泄漏其他出口。
- Key 绑定 realm 为 `gateway` 或 `all` 继续非法（400）。
- 绑定 Key 的模型选择器应只显示该 Key 可用的出口模型。

## 6. Chat / Responses / Messages 路由

### 6.1 请求字段

三个协议都要读取客户端模型字符串，解析成 `modelRef`：

- `requestedModel`：客户端看到的模型（有出口时是规范 ID，否则是裸 ID）。
- `upstreamModel`：发给 provider 的裸 ID。
- `exit`：有出口时是出口名；否则为空。
- `payload["model"]` / 翻译后的 chat map 必须改成 `upstreamModel`，不能把前缀发给上游。
- `ChatRequest.Model` 使用 `upstreamModel`。
- 响应里的 `model`、错误信息、用量里的 `requested_model` 使用 `requestedModel`。

### 6.2 解析优先级

有出口限定 ID 时：

1. Key 绑定 realm 必须等于该出口，否则 403。
2. 显式具体 realm（`X-Realm`/`?realm`/body）必须等于该出口，否则 400。
3. `realm=all` 或 `realm=gateway` 与出口限定 ID 兼容；前缀更具体，按前缀走。
4. 出口在解析阶段已经确认是已注册 realm；选中该 provider，禁用 provider 内部跨 realm 回退（`AllowRealmFallback=false`）。
5. 不做 catalog 预检查；显式出口的未知模型交给 provider 分类，避免目录缓存过期误杀。

没有出口限定 ID 时，保持现有顺序：

1. Key 绑定 realm。
2. 显式 `realm=all`：WorkBuddy intl/cn 模型匹配。
3. 显式 `realm=gateway`：在所有 provider 中匹配：
   - 任何 provider 的目录读取失败 → 503，要求客户端改用 `exit/model` 或具体 realm。不能因为某个 provider 暂时不可读，就把一个实际上多出口存在的模型误判为唯一。
   - 全部目录读取成功，恰好一个 provider 有该模型 → 使用它。
   - 多个 provider 都有 → 400，消息列出可用规范 ID，要求客户端显式选出口。
   - 没有 → 404。
   - `realm=gateway` 的模型列表仍允许部分 provider 失败；只有路由时的裸模型唯一性判断要求完整目录。
4. 显式具体 realm：直接使用该 provider。
5. 无 realm：单 provider 直接用；多 provider 先按活动出口偏好，再做唯一匹配（PR #14 合并后的行为）。
6. 匹配不到：404。

### 6.3 错误语义

| 情况 | 状态 | 说明 |
|---|---|---|
| `cn/` 空上游 | 400 | `model is required after exit prefix` |
| 前缀出口与 Key 绑定冲突 | 403 | Key 策略拒绝，不能靠前缀越权 |
| 前缀出口与显式 realm 冲突 | 400 | 两个显式意图冲突 |
| `realm=gateway` + 裸模型目录不完整 | 503 | 要求改用 `exit/model` 或具体 realm |
| `realm=gateway` + 裸模型多匹配 | 400 | 列出候选规范 ID |
| `realm=gateway` + 裸模型无匹配 | 404 | `model not found in gateway exits` |
| 显式出口但 provider 无此模型 | 由 provider 决定 | 不做目录预检查 |

未注册的第一段不是错误：它按裸模型处理，见 4.3。因此没有“未知出口 404”这一行。

`realm=all` + 出口限定 ID：前缀优先，允许 `intl/...`、`cn/...`；如果前缀是 CodeArts/Raccoon，也允许，因为客户端已经显式指定了出口。`realm=all` 只是没有前缀时的 WorkBuddy 回退范围。

### 6.4 协议适配点

- `internal/api/chat.go`：`chatHTTPRequest` 增加 `requestedModel`、`upstreamModel`、`exit`；`HandleChat` 解析、校验、改写 payload；`resolveChatProvider` 接受 `modelRef`。
- `internal/api/responses.go`：在翻译前解析；`translated.Chat["model"]` 改为 upstream；输出和观测用 requested。
- `internal/api/messages.go`：在 `chatMapToRaw` 前改写 `chat["model"]`；输出和观测用 requested。
- `internal/api/chat_webtools.go`：本地网页工具回合若复用模型字段，必须使用 upstream；最终响应仍用 requested。
- `internal/api/chat.go` 的 `HandleChat(provider, model)` 显式 provider 入口也要解析前缀；前缀与显式 provider 的 realm 不一致时 400。

### 6.5 客户端可见的 `model` 回显

出口限定请求的客户端可见结构化 `model` 字段**一律使用规范 ID**，不采用上游回报的模型别名：

- Chat 非流式：响应体的 `model`。
- Chat 流式：每个 chunk 的 `model` 和最终聚合的 `model`。
- Responses：`response.created` / `response.completed` 等事件和最终对象的 `model`。
- Messages：`message_start.message.model`、`message_delta` 和聚合对象的 `model`。

裸请求保持现有回显行为，不强制改写。provider 的自由文本错误消息不重写，只保证 JSON 结构化字段里的 `model` 是规范 ID。实现时给 stream normalizer 增加“强制模型”开关，避免上游 chunk 覆盖；Responses/Messages 在输出层统一覆盖。

## 7. API Key 模型白名单

新增一个共享判断，避免三个协议各写一套：

```go
func allowsRequestedModel(entry keys.Entry, ref modelRef) bool {
    if keys.AllowsModel(entry, ref.Canonical) {
        return true
    }
    if ref.Qualified && keys.AllowsModel(entry, ref.Upstream) {
        return true
    }
    return false
}
```

语义：

- 旧 Key 的裸模式继续匹配裸请求，例如 `deepseek/*`。
- 旧 Key 的裸模式也能匹配出口限定请求的上游部分，因此升级后旧 Key 不会突然拒绝 `cn/deepseek...`。
- 新 Key 可以写 `cn/*`、`codearts/*`、`intl/deepseek-*`。
- 裸请求 + 只写 `cn/*` 的 Key：当前阶段仍拒绝；客户端应显式发 `cn/model`。这样避免在解析 provider 之前猜测出口。
- 空白名单仍表示不限制。
- 匹配继续使用现有 `fnmatch` 和大小写归一化。

错误信息里的“请求模型”和“允许列表”使用客户端原始模型，方便用户对照。

## 8. 用量、分析与看板

### 8.1 用量记录（第一阶段）

- `model`：上游原始 ID，保证现有定价、OpenRouter 匹配、历史聚合不变。
- `requested_model`：出口限定请求时写规范 ID；裸请求时与 `model` 相同或留空，沿用现有 recorder 的去重逻辑。
- `realm`：provider 实际出口，现有逻辑不变。
- 失败行：优先记录客户端请求的 `requestedModel`；如果能确定出口，`realm` 写出口。

这样一行 `cn/deepseek...` 请求会得到：

```json
{
  "model": "deepseek-v4.1-flash",
  "requested_model": "cn/deepseek-v4.1-flash",
  "realm": "cn"
}
```

定价仍按 `model`，出口区分已经可用 `requested_model` 做后续聚合。

### 8.2 分析聚合（第二阶段）

第一阶段不改分析表，避免一次改动过大。第二阶段再决定：

- 如果行有 `requested_model`，模型聚合键使用 `requested_model`。
- 没有 `requested_model` 的旧行继续使用 `model`，保持历史可比。
- 看板模型表显示规范 ID，并保留 `realm` 列。
- `usage/perf`、`usage` 的 `by_model` 采用同一键函数，避免两套口径。

不要求回填历史行；旧行继续按裸模型聚合。需要严格区分历史出口时，使用 `realm` 过滤/分组。

### 8.3 看板（第二阶段）

第一阶段不改 embedded dashboard，保持后端先行。此时 Key 模型选择器仍然只显示旧的 `intl`/`cn` 目录；不限模型的 Key 不受影响，绑定 Key 如果需要新出口模型，可以先通过设置文件/API 写入规范模式，或暂时使用裸模式。

第二阶段再做：

- Key 模型选择器改为优先请求 `/v1/models?realm=gateway`；失败再回退裸 `/v1/models`、`intl`、`cn`。
- 面板请求没有 Key 绑定，服务端不会替它按 Key 过滤；前端必须根据当前编辑行的 `row.realm` 在客户端过滤 gateway 目录。`row.realm` 为空时显示全部出口。
- Key 行保存的模型值可以是规范 ID；旧裸模式仍能匹配。
- 模型页、账号禁用模型继续使用具体 realm，不受影响。
- 统计矩阵的 WorkBuddy 白名单保持 `realm=all`；等模型聚合键切换后，再改用 `gateway` 目录，避免 CodeArts/Raccoon 模型被误过滤。

### 8.4 Agent 模型选择

`internal/platform/agents` 当前按活动 WorkBuddy + CodeArts 官方 ID 生成选择清单，且不含 Raccoon。全出口模型应该作为单独阶段处理：

- 选择清单改用 `realm=gateway` 的规范 ID。
- 写入客户端配置的模型 ID 带出口前缀。
- 保留旧选择项的兼容读取；裸 ID 继续按现有规则解析。
- 这会影响 Claude Code/Codex/OpenCode/DSH/Crush/ZCode 等生成配置，必须单独测试，不塞进第一阶段。

## 9. 兼容性与迁移

- 旧的 `/v1/models?realm=cn` 响应：`id` 不变，新增字段；旧客户端不受影响。
- 旧的 `/v1/models?realm=all`：语义、顺序、去重规则不变。
- 旧 Chat 裸 ID：解析顺序不变。
- 旧 API Key 模型模式：裸请求不变；出口限定请求会额外尝试上游裸模式，属于放宽兼容。
- 旧用量行：不迁移；新行增加 `requested_model`。
- 旧看板：第一阶段不改；第二阶段 Key 选择器改动带失败回退。
- `gateway` 是保留 scope；Key 不能绑定 `gateway`。
- 出口前缀是保留命名空间；见 4.3 的边界说明。
- 不需要数据库 schema 迁移，不需要重写 `usage.jsonl` 或 rollup。

## 10. 安全

- API Key 绑定 realm 是硬策略；出口前缀不能绕过。
- `realm=gateway` 在绑定 Key 下只返回绑定出口，不能用来枚举其他出口。
- 出口限定请求不做跨 provider 回退，避免“指定 CodeArts 却落到 WorkBuddy”。
- 模型白名单同时检查规范 ID 和上游裸 ID，避免升级后旧 Key 失效；但不会因为检查裸 ID 而绕过 `realm` 绑定。
- 只接受已注册 provider 的 realm 作为前缀；未知前缀按裸模型处理，不会动态创建 provider。
- 错误信息不泄漏 provider 内部路径/凭据；沿用现有 failure 映射。

## 11. 测试计划

### 11.1 解析单测

`internal/api/modelref_test.go`：

- 裸 ID、出口 ID、大小写出口、空上游、多斜杠、`cn` 无斜杠、`gateway/foo` 当裸 ID。
- `openai/gpt-4o`、`meta-llama/Llama-3` 在没有同名 provider 时按裸 ID。
- 已注册 `alpha` stub 的 `alpha/foo` 解析为出口。
- 前缀不被改写上游大小写。

### 11.2 模型目录

`internal/api/models_test.go`：

- `realm=gateway` 合并 intl/cn/codearts/raccoon，同一裸模型出现多条规范 ID。
- 去重按 qualified ID，不按裸 ID。
- 每个条目有 `qualified_id`、`upstream_id`、`provider`、`realm`。
- 单 provider 失败不遮住其他 provider；全失败返回错误。
- 绑定 Key 的 gateway 只含绑定出口。
- `realm=all` 仍不泄漏 CodeArts/Raccoon，且 `id` 仍为裸 ID。
- 具体 realm 的 `id` 仍为裸 ID，新增 `qualified_id`。
- `force` 透传。

### 11.3 Chat 路由

`internal/api/chat_test.go`：

- `cn/m1` 路由到 cn，provider 收到 `m1`，`Raw["model"]` 也是 `m1`。
- `codearts/m1`、`raccoon/m1`、`intl/m1` 分别路由。
- 同裸模型存在于多个出口时，规范 ID 仍各自路由。
- Key 绑定 `cn` + `codearts/m1` → 403。
- `?realm=codearts` + `cn/m1` → 400。
- `?realm=all` + `cn/m1` → 允许；`?realm=all` + 裸模型仍只走 WorkBuddy。
- `?realm=gateway` + 唯一裸模型 → 路由；多匹配 → 400 且列出候选；任一 provider 目录失败 → 503。
- 未知出口前缀按裸模型处理；已注册出口空上游 → 400。
- 显式出口时 `AllowRealmFallback` 为 false。
- 非流式响应 `model` 回显规范 ID；流式每个 chunk 和聚合 `model` 都回显规范 ID。
- 裸请求的 `model` 回显保持现状。
- 失败观测记录 requested model 和出口。

### 11.4 Responses / Messages

- `responses_test.go`、`messages_test.go` 各覆盖：规范 ID 路由、上游 payload 去前缀、响应 model 回显（含流式事件）、Key 绑定冲突、`realm=gateway` 行为。
- Anthropic 错误体保持现有形状。

### 11.5 Key 白名单

`internal/api/auth_platform_test.go` 或新测试：

- Key `deepseek/*` 允许 `cn/deepseek...`。
- Key `cn/*` 只允许 cn 规范 ID。
- Key 裸模式仍允许裸请求。
- Key `codearts/*` 拒绝 `cn/...`。
- 空列表不限制。
- 大小写、通配符沿用现有行为。

### 11.6 用量

- 规范 ID 请求写入 `model=upstream`、`requested_model=exit/upstream`、`realm=exit`。
- 裸请求保持原有字段。
- 失败行记录 requested model。
- 现有定价/分析测试不回归。

### 11.7 看板（第二阶段）

- Key 选择器优先使用 gateway 列表；gateway 失败时回退旧路径。
- Key 行绑定 `cn` 时，客户端过滤只显示 `realm=cn` 的条目；绑定为空时显示全部。
- 现有 Key 选择器、模型页、统计矩阵 JS 测试通过。
- 补规范 ID 聚合和 Agent 配置测试。

## 12. 实施阶段

### 阶段 1：后端路由与全出口目录

- 新增 `modelref.go` 及单测。
- 扩展 `providers.Model` 元数据（`qualified_id`、`upstream_id`、`provider`、`realm`）。
- `handleModels` 增加 `gateway`，保持 `all`/具体 realm 兼容。
- Chat/Responses/Messages 接入解析、改写 payload、路由、错误。
- Key 白名单双匹配。
- 用量记录 requested model。
- 更新 API 文档和测试。

### 阶段 2：看板与统计

- Key 模型选择器改用 gateway，并按当前编辑 Key 的 `row.realm` 在客户端过滤。
- 分析表模型聚合键优先 `requested_model`。
- 统计矩阵/模型页展示规范 ID 与出口。
- Agent 模型选择改用 gateway 并写规范 ID。
- JS/契约测试。

### 阶段 3：可选别名与定价展示

- 如需 `workbuddy-intl/...` 可读别名，做解析层别名，不改变规范 ID。
- 如各出口定价不同，先按 `realm` 分组展示；计价仍按上游 `model` + 现有价格表，不擅自改计费。
- 评估是否给 `realm=all` 增加弃用提示，但不改变其 WorkBuddy-only 语义。

## 13. 成功标准

- `/v1/models?realm=gateway` 能同时列出四个出口；同一裸模型在不同出口有不同规范 ID。
- 客户端发 `codearts/<model>` 时只走 CodeArts；发 `cn/<model>` 时只走国内 WorkBuddy；上游收到的模型字符串不含前缀。
- 旧裸 ID、旧 `realm=all`、旧 Key 白名单、旧用量行保持可用。
- 绑定 Key 无法通过前缀访问其他出口。
- 用量行能区分 `realm` + 上游模型；规范请求带 `requested_model`。
- 现有全部 Go/JS 测试通过；新增测试覆盖上表。

## 14. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 客户端不接受带 `/` 的模型 ID | 旧裸 ID 永远保留；gateway 是新增 scope |
| 上游模型 ID 本身含 `/` | 只按第一个 `/` 切分；未知前缀按裸 ID |
| 旧 Key 白名单失效 | 规范 ID 和上游裸 ID 双匹配 |
| `realm=all` 语义被误改 | 明确保持 WorkBuddy-only；全出口用 `gateway` |
| 统计同名模型混淆 | 第一阶段写 `requested_model`；第二阶段切聚合键 |
| 前缀泄漏给上游 | 解析后同时改写 `ChatRequest.Model` 和 payload/chat map |
| 显式出口被内部回退覆盖 | 设置 `AllowRealmFallback=false` |
| 绑定 Key 枚举其他出口 | gateway 目录按绑定 realm 过滤；前缀冲突 403 |
| 目录缓存过期误杀模型 | 显式出口不做 catalog 预检查，交给 provider |

## 15. 已定决策

- 规范前缀使用出口名：`intl/`、`cn/`、`codearts/`、`raccoon/`，不用 `provider/model`。
- 只有已注册 realm 的第一段才当出口；未知前缀按裸模型 ID 处理，不返回“未知出口”。
- 全出口目录使用新 scope `realm=gateway`；`realm=all` 保持 WorkBuddy-only。
- 具体 realm 的模型 `id` 保持裸 ID；gateway 的 `id` 使用规范 ID；两者都带 `qualified_id`。
- `realm=gateway` + 裸模型多匹配时返回 400，要求显式出口；目录不完整时返回 503，失败关闭。
- 出口限定请求的客户端可见 `model` 字段强制回显规范 ID；裸请求保持现状；provider 自由文本错误不重写。
- Key 绑定冲突返回 403；显式 realm 冲突返回 400。
- 第一阶段后端 only；Key 模型选择器、分析聚合、统计矩阵、Agent 配置放第二阶段。
- 用量第一阶段只增加 `requested_model`，不迁移历史行。
- 看板/Agent 改造放第二阶段。
