# 开发说明：Provider、模型路由与功能扩展

基线：`v2.4.0`（已包含 PR #14 / #15 / #17）
适用：新增 Provider、出口、模型，或修改 Chat/Responses/Messages、Key、Usage、Dashboard。

这份文档是开发约定，不是发布说明。具体设计细节见
[`docs/superpowers/specs/2026-11-12-exit-qualified-models-design.md`](superpowers/specs/2026-11-12-exit-qualified-models-design.md)，
任务拆分见
[`docs/superpowers/plans/2026-11-12-exit-qualified-models.md`](superpowers/plans/2026-11-12-exit-qualified-models.md)。

## 1. 基本原则

- **Provider 是编译期模块**。新增渠道 = 新增 `internal/providers/<name>` 包 + 在 `internal/app/providers.go` 注册，不做运行时热插拔。
- **一个 Provider 实例只服务一个 realm/出口**。WorkBuddy 的国际版和国内版是两个实例，共享同一套实现。
- **API 层负责跨渠道语义**：模型引用解析、realm/Key 策略、前缀剥离、usage 归因、客户端可见的 `model` 回显。
- **Provider 只处理自己的上游协议**：它收到的是上游模型 ID，不需要也不应该解析 `<exit>/` 前缀。
- **兼容优先**：裸模型 ID、旧 `realm` 参数、`realm=all`、旧 Key 白名单、历史 usage 行都必须继续工作。
- **不做跨 Provider 自动回退或并行 fan-out**。显式出口失败就是失败，不要把请求偷偷送到另一个出口。
- **WorkBuddy 内部的 intl/cn 回退**只在没有显式出口时保留；显式出口会关闭它。

## 2. 代码地图

| 层 | 位置 | 职责 |
|---|---|---|
| Provider 接口 | `internal/providers/base.go` | `Provider`、`ChatRequest`、`ChatStream`、错误/usage 接口 |
| 可选能力 | `internal/providers/hooks.go` | settings、limits、account inventory、realm、media、agent picker 等 |
| 注册 | `internal/providers/registry.go`、`internal/app/providers.go` | 按 realm 注册，保留注册顺序 |
| 模型引用 | `internal/api/modelref.go` | `<exit>/<upstream>` 解析、规范 ID |
| Chat 路由 | `internal/api/chat.go` | Key/realm/exit 优先级、provider 选择、前缀剥离 |
| 模型目录 | `internal/api/models.go` | gateway 全出口目录、具体 realm、`realm=all` |
| Key 策略 | `internal/api/auth.go`、`internal/platform/keys/binding.go` | realm 绑定、模型/账号白名单 |
| 用量 | `internal/api/usage_hook.go`、`internal/platform/usage` | 记录、聚合、rollup、定价 |
| 面板 | `internal/web/dashboard.html` | 模型/账号/Key/统计页面 |
| 契约测试 | `tests/contract`、`tests/dashboard`、`tests/integration` | 线上格式、面板行为、端到端 |

## 3. Provider 契约

### 3.1 必选接口

`providers.Provider`：

```go
type Provider interface {
    ID() string
    Realm() string
    Overview(ctx context.Context, force bool) (any, error)
    Health(ctx context.Context, force bool) (Health, error)
    Models(ctx context.Context, force bool) ([]Model, error)
    OpenChat(ctx context.Context, req ChatRequest) (ChatStream, error)
    Routes() []Route
    Jobs() []scheduler.Job
}
```

- `ID()` 是稳定实现/实例标识。WorkBuddy 返回 `workbuddy-intl` / `workbuddy-cn`；CodeArts 返回 `codearts`；Raccoon 返回 `raccoon`。
- `Realm()` 是路由键。必须唯一、非空、大小写稳定；`all` 和 `gateway` 是保留作用域，不能作为 realm。
- `Models()` 返回 provider 自己的目录，`id` 是**上游裸 ID**。API 层会补 `qualified_id`、`upstream_id`、`provider`、`realm`；gateway 列表把 `id` 改成规范 ID。Provider 不要依赖 API 层修改后的 map。
- `OpenChat()` 只使用 `req.Model`（上游裸 ID）。`req.Raw["model"]` 也已经由 API 层改写成裸 ID；不要再拼回出口前缀。
- `Health()` / `Overview()` 必须脱敏，不能返回 token、密码、完整 cookie。
- `Routes()` 返回 provider 专用面板/管理端点；`Route.Auth` 选择鉴权级别。
- `Jobs()` 可返回 `nil`；定时任务要遵守 scheduler 的时区和幂等约定。

### 3.2 常用可选能力

| 接口 | 用途 |
|---|---|
| `SettingsContributor` | 向面板 settings 提供/保存 provider 配置段 |
| `LimitApplier` | 接收共享的每日限额（积分/token/预留/临期窗口） |
| `UsageSnapshotReporter` | 流结束前报告 realm/account/model/tokens/credit |
| `AccountInventory` | 向 `/health`、用量账号列表提供脱敏账号摘要 |
| `RealmSelector` | 可切换默认出口（目前 WorkBuddy） |
| `RealmAvailableProvider` | 非 selector provider 是否出现在 `/realm` |
| `ProxySlotBinder` | 账号绑定代理槽 |
| `CapabilityProvider` | 声明 media、agent、probe、usage limits 等能力 |
| `AgentModelSpecProvider` / `AgentPickerModelProvider` / `AgentOfficialModelProvider` | Agent 配置器的模型元数据/选择清单 |
| `CatalogProvenanceProvider` | `/v1/models` 的 catalog 来源信息 |
| `MediaProvider` | 图片/视频生成 |
| `HTTPError` / `GatewayHint` | 安全状态码、错误类型、可选网关提示 |

新增可选能力时，先加到 `hooks.go` 的接口，再由平台通过类型断言调用；不要让平台 import 具体 provider。

## 4. 模型 ID 与路由

### 4.1 规范 ID

```text
<realm>/<upstream-model>
```

- `realm` 必须是**已注册**的 provider realm。
- 只按第一个 `/` 切分；上游 ID 可以继续包含 `/`。
- 第一段不是已注册 realm 时，整个字符串按裸模型 ID 处理；不要报“未知出口”。
- 已注册 realm + 空上游 → 400。
- 大小写：realm 规范化小写，上游部分保持原样。
- 保留前缀：`gateway/foo` 不是出口，按裸 ID 处理；`all`/`gateway` 不能被注册为 realm。

### 4.2 路由优先级

`resolveChatProvider` 的完整顺序：

1. **API Key 绑定 realm**：硬策略，压过所有请求级 hint。
2. **规范 ID 的出口**：精确路由；与 Key 绑定冲突 → 403；与显式具体 realm 冲突 → 400；`realm=all`/`gateway` 与规范 ID 兼容，前缀优先。
3. **显式 realm**：`X-Realm` > `?realm` > body `realm`。
   - `realm=gateway` + 裸模型：读取所有 provider 目录，必须唯一匹配；目录不完整 → 503；多匹配 → 400 并列出候选；无匹配 → 404。
   - `realm=all` + 裸模型：只查 WorkBuddy `intl`/`cn`。
   - 具体 realm：直接用该 provider，不做目录预检查。
4. **无 realm**：单 provider 直接用；多 provider 先尝试活动出口，再按注册顺序回退；不做唯一性检查。
5. 仍无匹配 → 404。

规范 ID 请求必须设置 `AllowRealmFallback=false`。裸请求保持 provider 内部默认回退策略。

### 4.3 目录作用域

| 请求 | 返回 |
|---|---|
| `/v1/models?realm=gateway` | 所有已注册、目录可读的出口；`id` 为规范 ID；带 `upstream_id`/`qualified_id`/`provider`/`realm` |
| `/v1/models?realm=<具体>` | 该出口；`id` 保持裸 ID，额外带 `qualified_id` 等 |
| `/v1/models?realm=all` | 仅 WorkBuddy intl/cn；保持裸 ID 和旧去重规则 |
| `/v1/models` 多 provider | 400，提示使用 `realm=gateway` |

- gateway 目录允许单个 provider 失败（部分返回）；`resolveGatewayAmong` 的裸模型路由要求所有目录可读，失败关闭。
- 绑定 Key 的 `realm=gateway` 只返回绑定出口。
- `catalog` 是旧的单目录 provenance；gateway 使用 `catalogs` 按 realm 汇总。

## 5. API 协议约定

### 5.1 Chat / Responses / Messages

三个协议共用同一套模型引用规则：

- 解析后：`requestedModel` = 客户端可见 ID（有出口时规范 ID，否则裸 ID）；`model` = 上游裸 ID。
- 发给 provider 前必须同时改写：
  - `ChatRequest.Model`
  - `Raw["model"]`
  - Responses 的 translated chat map / Messages 的 chat map
- 不允许前缀进入上游 URL、body、日志中的上游模型字段。
- 规范请求的客户端可见 `model` 字段回显规范 ID；裸请求保持原行为。
- 观测/usage：`Model` 上游，`RequestedModel` requested，`Realm` 实际出口。
- 失败观测在 provider 打开后也不能把规范 ID 塞进 `Model`。

### 5.2 新增协议功能的最低要求

- 先复用 `modelref.go`，不要在协议层复制解析。
- 使用 `keyModelFailureForRef` 或等价的 canonical+upstream 检查。
- 显式出口/Key 绑定时关闭 provider 内部 realm fallback。
- 增加 Chat/Responses/Messages 三边的回归测试；至少覆盖：
  - 规范 ID 精确路由
  - 前缀剥离
  - Key 绑定冲突
  - 响应 `model` 回显
  - usage 的 `model`/`requested_model`/`realm`

## 6. API Key 策略

- `Entry.Realm` 是绑定出口；`all`/`gateway` 不能作为绑定值。
- 绑定优先于 `X-Realm`、query、body、`realm=all`、`realm=gateway` 和规范 ID 前缀。
- 规范 ID 与绑定冲突 → 403；显式具体 realm 与规范 ID 冲突 → 400。
- 模型白名单：
  - 先匹配规范 ID，再匹配上游裸 ID。
  - `fnmatch` 大小写不敏感。
  - `deepseek/*` 只匹配带斜杠的 `deepseek/...`，不匹配 `deepseek-v4.1-flash`；后者用 `deepseek-*` 或 `deepseek*`。
  - 不要把 `/` 当成“任意层”通配。
- 账号白名单仍按 uid 精确匹配；与出口绑定独立。
- Key 并发限制、禁用、删除状态由 `internal/platform/keys` 统一处理，provider 不要自己解析 Key。

## 7. Usage / Pricing / Analytics

### 7.1 记录字段

| 字段 | 含义 |
|---|---|
| `model` | 上游/计费模型 ID，用于定价和 OpenRouter 匹配 |
| `requested_model` | 客户端请求的规范 ID（仅当与 `model` 不同才写） |
| `realm` | 实际服务出口 |
| `account` | 实际账号 uid |
| `credit` | 该请求扣减积分（未知时省略/null） |

- Provider 的 `UsageSnapshotReporter` 必须给出真实上游 model；API 层负责把 requested/canonical 分开。
- `applySnapshot` 只能在 `RequestedModel` 为空时回填，不能覆盖已有的规范 ID。
- 不要在 provider 层把 `model` 改成规范 ID；定价会失效。

### 7.2 聚合

- 分析/快照/perf/rollup 使用 `rowModelKey(row)`：有 `requested_model` 用它，否则用 `model`。
- 历史行不迁移；没有 `requested_model` 的旧行继续按裸 `model` 聚合。
- `usage/perf`、`by_model`、`by_model_realm`、`by_model_acct`、rollup 必须使用同一键函数，避免两套口径。
- `cost_missing` 仍按计费的 `model` 记录，不要用规范键。
- 用量页的 `realm=all` 表示“全部 realm”；模型/Chat 的 `realm=all` 只表示 WorkBuddy，两者不要混。

### 7.3 数据重写

- 禁止无备份重写 `usage.jsonl`。
- 重写/替换必须保留文件 owner/group 为容器运行用户（本地为 `10001:10001`），否则容器会 `permission denied`，表现为“请求正常但统计不涨”。
- 不伪造逐条 usage 行；需要补账时先与用户确认汇总/备注方式。

## 8. Dashboard / 面板

- 模型来源：
  - Key 选择器优先 `/v1/models?realm=gateway`，失败回退 `/v1/models` → intl/cn；按 Key 的 `realm` 在客户端过滤。
  - 具体账号的禁用模型仍用 `/v1/models?realm=<账号 realm>`。
  - 统计矩阵白名单使用 gateway 的 `id` + `upstream_id`，否则裸历史行会被过滤掉。
- `realm=all` 的 WorkBuddy-only 语义不能在面板里悄悄改成全出口。
- Agent 模型选择器输出规范 ID；旧裸 `agent_models` 选择仍可路由，但不会自动获得新元数据。
- 面板改动后必须更新：
  - `internal/web/assets.go` 的 `DashboardSHA256`
  - `tests/oracle/dashboard.md` 的 v2 hash/size
  - `tests/dashboard/node/run.js` 的套件列表
  - `tests/dashboard/js_harness_test.go` 的套件计数
  - `tests/dashboard/contract_test.go` 的 contract artifact 列表（新增套件时）

## 9. 新增 Provider：检查清单

1. **建包**：`internal/providers/<name>/`，实现 `Provider`。
2. **定 ID/Realm**：`ID()` 稳定；`Realm()` 唯一；多出口 provider 用多个实例（参考 WorkBuddy）。
3. **目录**：`Models()` 返回裸 `id` 和必要元数据；不要返回规范 ID。
4. **Chat**：`OpenChat` 使用 `req.Model`；不要解析前缀；流必须可取消/关闭。
5. **Usage**：实现 `UsageSnapshotReporter`（若上游有 usage/credit）；给出真实 model/account/token/credit。
6. **错误**：实现 `HTTPError` 安全消息；不要把上游 token、cookie、内部路径返回给客户端。
7. **可选能力**：按需实现 settings、limits、account inventory、proxy slots、media、agent picker、catalog provenance。
8. **注册**：在 `internal/app/providers.go` 创建并 `registry.Register`；确保 realm 不与保留作用域/其他 provider 冲突。
9. **路由**：provider 管理端点加到 `Routes()`，选择正确 `AuthClass`；不要绕过 panel auth。
10. **测试**：provider 单测、API stub/contract、gateway 目录、Key 绑定、usage、dashboard（若面板有入口）。
11. **文档**：更新 README 的出口/部署说明；如改变路由/Key 语义，更新本文件和设计文档。

## 10. 新增出口或模型

### 新出口

- 唯一 realm，不能是 `all`/`gateway`。
- 加入 gateway 目录测试、具体 realm 目录测试、Key 绑定/前缀冲突测试。
- 面板的 `/realm`、账号 tab、模型页、统计 scope 需要有对应显示。
- 更新 README 的出口列表和本文件的术语表。

### 新模型

- Provider 目录返回裸 `id`；同一模型在多个出口可以同名，gateway 会生成不同规范 ID。
- 检查 Key 选择器、统计矩阵、定价/usage 是否能识别；需要时补 golden/contract fixture。
- 不要在 API 层硬编码某个 provider 的模型名；模型能力属于 provider/catalog。

## 11. 新增功能：跨层清单

任何“用户可见”的功能至少检查这些层：

| 层 | 必须确认 |
|---|---|
| API 路由 | 是否复用 modelref/Key/realm 优先级 |
| Provider | 是否只处理上游 ID，是否正确关闭/回退 |
| Key | realm 绑定、模型/账号白名单是否仍然有效 |
| Usage | `model`/`requested_model`/`realm`/credit 是否正确 |
| Dashboard | gateway/具体 realm/`realm=all` 的语义是否一致 |
| Tests | API、provider、usage、dashboard、contract 是否覆盖 |
| Docs | README/本文件/设计文档是否需要更新 |
| Compatibility | 裸 ID、旧 realm、旧 Key、历史 usage 是否仍可用 |

特别注意：

- 不要让新功能在 API 层之外重新解析模型前缀。
- 不要给规范请求偷偷加跨 provider 回退。
- 不要把 `realm=gateway` 当成 provider realm。
- 不要在 dashboard 把 `realm=all` 扩成 CodeArts/Raccoon。
- 不要忘记更新 dashboard hash/oracle 和 JS suite 计数。

## 12. 测试与验证

最低要求：

```bash
make check                 # fmt + vet + go test ./...
go test ./... -count=1     # 最终验证，避免缓存
```

涉及并发/流/池的改动：

```bash
make race
```

面板改动：

```bash
go test ./tests/dashboard -run TestDashboardJSHarness -v
go test ./tests/dashboard ./tests/oracle ./internal/web ./tests/contract -count=1
```

其他：

- 协议/线上格式改动：更新并跑 `tests/contract` 的 golden。
- dashboard 改动：更新 hash/oracle/suite 计数后跑 `tests/dashboard`、`tests/oracle`、`internal/web`。
- Provider 改动：包内单测 + `tests/integration` + 至少一个真实/沙箱 smoke（如果可用）。
- 修 bug 先写失败测试；不要只做手工验证。
- 最终 PR 前跑 `git diff --check`，确认没有未跟踪的临时文件被误提交。

## 13. 常见坑

1. **前缀泄漏**：只改 `ChatRequest.Model`，忘了 `Raw["model"]` 或 translated chat map。
2. **gateway 失败开放**：裸模型路由时某个目录失败仍按部分目录路由，导致多出口歧义被误判为唯一。
3. **`realm=all` 扩散**：把 CodeArts/Raccoon 加进 `realm=all`，破坏旧契约。
4. **Key 白名单斜杠**：把 `deepseek/*` 当成“所有 deepseek”，实际只匹配带斜杠的 ID。
5. **usage 口径混用**：把规范 ID 写进 `model`，导致定价/OpenRouter 找不到；或把上游 ID 写进 `requested_model`。
6. **snapshot 覆盖**：provider 回报的别名覆盖掉 canonical `RequestedModel`。
7. **面板 hash 忘更新**：dashboard 改了但 `assets.go`/oracle 没改，contract test 必挂。
8. **保留 realm**：新 provider 使用 `all`/`gateway`，导致 `providerForRealm` 返回非预期对象。
9. **数据文件 owner**：重写 `usage.jsonl` 后变成 root，容器 10001 无法追加。
10. **裸 ID 行为变化**：无 realm 时现在先尝试活动出口，再按注册顺序回退；改路由时不要丢掉这个兼容路径。

## 14. PR 检查清单

- [ ] 新 provider/出口/模型的目录元数据正确（裸 `id`）
- [ ] 规范 ID 可精确路由，前缀不进入上游
- [ ] Key 绑定和模型白名单回归通过
- [ ] usage `model`/`requested_model`/`realm` 正确
- [ ] `realm=all` 仍仅 WorkBuddy
- [ ] gateway 目录/失败关闭/绑定过滤有测试
- [ ] dashboard hash/oracle/suite 计数已更新（如有面板改动）
- [ ] `make check`、`go test ./... -count=1` 通过
- [ ] README/本文件/设计文档已同步
- [ ] 没有把 `HANDOFF-*`、临时脚本或本地数据误提交
