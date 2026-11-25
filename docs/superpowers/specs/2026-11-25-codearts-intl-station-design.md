# CodeArts 国内/国际双站 Provider 设计

- 日期：2026-11-25
- 仓库：`mix2api-v2`
- 状态：待批准
- 目标：在现有 `codearts` provider 中增加国际站，共用已验证的代码，但不假设国际站与国内站行为相同。

## 1. Intent

把国际站 CodeArts Agent 做成 mix2api 的第二个 CodeArts realm，与国内站并列：

```text
codearts         -> 国内站（cn-north-4）
codearts-intl    -> 国际站（ap-southeast-1）
```

用户得到的体验应与现有 `codearts`、`raccoon`、`doubao` 一致：添加账号、模型列表、`/v1/chat/completions`、SSE、账号池、健康检查、额度展示、面板管理。

设计核心不是“复制一份国际版”，而是：

- **一个 CodeArts 实现**
- **两个 Station profile**
- **两个 Provider instance / realm**
- **两套 settings 和 accounts 目录**
- **行为差异全部显式建模，不靠猜测**

## 2. Goals

1. 保持国内站行为不变，现有测试全绿。
2. 新增 `codearts-intl` realm，支持国际站模型目录、聊天、SSE、额度和健康检查。
3. 国际站账号支持两种进入方式：
   - 第一阶段：导入 CodeArts Agent 的 session JSON（含 `refresh_token`、PKCE verifier、DPoP 私钥）。
   - 第二阶段：在 mix2api 面板直接发起国际站 OAuth 登录（`site=intl`）。
4. 国际站 Free 套餐的 5M token 额度可见、可限制、可展示。
5. 国际站不启用国内站独有的 benefit/opengw、每日签到、积分批次等逻辑。
6. 国际站和国内站的模型 ID、host、locale、`stop` 行为、额度类型可以独立配置。
7. 代码可测试：签名、OAuth URL、refresh、模型目录、额度解析均有单元测试。

## 3. Non-goals

1. 不在第一阶段做 Windows credential sidecar。先证明 session JSON 导入 + provider 内刷新可以工作；sidecar 只作为后续可选 fallback。
2. 不把国际站伪装成国内站：不共用同一套账号目录、settings key 或 realm。
3. 不新增 Anthropic Messages 接口。现有 `codearts` provider 只做 OpenAI-compatible chat；国际站沿用同一范围。
4. 不自动领取国际站签到/福利。国际站没有国内站的每日签到和 benefit 通道。
5. 不在第一阶段做多区域自动 fallback（国际站失败自动切国内站）。两个 realm 独立。
6. 不复制 `magpie`/`CodeArts2API` 的整份代码；只借鉴协议与实现模式，必要时按许可证署名。

## 4. Constraints and Findings

### 4.1 现有代码的硬编码点

当前 `internal/providers/codearts` 有多处国内站硬编码，需要参数化：

- `provider.go`：`ProviderRealm = "codearts"`，`New(store)` 固定。
- `config.go`：settings key 固定 `"codearts"`。
- `pool.go`：账号目录固定 `dir/codearts`，state 固定 `dir/codearts/state.json`。
- `oauth.go`：`oauthDefaultPortal`、`portalHost()`、`oauthDefaultSTS`、locale、client id 固定国内站。
- `chat.go`：`snapHost()`、`X-Language: zh-cn`、`x-auth-token`、benefit 头固定。
- `catalog.go`：`snapHost()`、opengw benefit host、国内模型种子固定。
- `models.go`：`owned_by: codearts`、国内 credit 表、国内 fallback limits。
- `credits.go`/`checkin.go`：国内积分、签到逻辑。
- `routes.go`：`/codearts/...` 路由前缀固定；两个 realm 需要各自的路由命名空间。

### 4.2 mix2api 已有双 realm 先例

`providers/base.go` 明确允许同一 implementation 注册多个 realm instance。`workbuddy` 已采用：

- `ProviderID` 共用 `workbuddy`
- `ProviderIDIntl = "workbuddy-intl"`、`ProviderIDCN = "workbuddy-cn"`
- `ProviderRealmIntl = "intl"`、`ProviderRealmCN = "cn"`
- 两个实例共用 implementation，但各自配置和账号池过滤

CodeArts 应沿用同样模式，但使用独立账号目录，避免两个 Pool 同时写同一个 `state.json`。

### 4.3 国际站与国内站已确认差异

参考：

- `magpie-community/plugins` `packages/codearts/index.mjs`（MIT）
- magpie issue #1090（国际站登录实测）
- `2951461586/CodeArts2API`（Apache-2.0，Go 凭证/签名/刷新）
- `HITZY2002/codearts2api`（MIT，国内站代理）

| 项目 | 国内站 | 国际站 | 影响 |
|---|---|---|---|
| API host | `snap-access.cn-north-4.myhuaweicloud.com` | `snap-access.ap-southeast-1.myhuaweicloud.com` | 所有 snap 请求/签名 |
| Portal | `codearts.huaweicloud.com/portal` | `codearts.ap-southeast-1.huaweicloud.com/portal` | OAuth authorize |
| 登录入口 | portal authorize | 先走 `auth.huaweicloud.com/authui/login?site=intl&locale=en-us&service=<authorize>` | 不能只换 host |
| locale | `zh-cn` | `en-us` | 请求头/登录页 |
| STS | `sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens` | 同一个 STS，但凭据必须记录 `station` | refresh 不能丢站 |
| 免费通道 | opengw/benefit 可用 | 无 opengw，相关接口 404 | 不请求 benefit |
| 额度 | 积分/credit | Free `is_token_package`，5,000,000 tokens | 额度解析不同 |
| 签到 | 有 | 无 | 不注册签到 job |
| `stop` | 非空直接 400 | 只接受数组 | 请求体归一化不同 |
| 模型 | GLM/DeepSeek/Pangu/Kimi 等 | GLM-5.3/5.2/5.1、Qwen3-VL-235B | 模型种子/能力不同 |
| refresh_token | 一次性、轮转 | 一次性、轮转，且绑定 station | 只能一方刷新 |

> 注：表中的 `stop` 行是 magpie 对上游的实测口径；mix2api 当前国内实现仍把字符串转数组后转发，现有测试也锁定了这个行为。本设计**不改国内行为**，只给国际站加数组化。

国际站模型实测：

| 显示名 | API model ID | 上下文 | 最大输出 | 备注 |
|---|---|---:|---:|---|
| GLM-5.3 | `glm-5.3` | 202,752 | 65,535 | 默认 |
| GLM-5.2 | `GLM-5.2` | 202,752 | 131,072 | |
| GLM-5.1 | `GLM-5.1` | 202,752 | 131,072 | 日志提示 2026-11-30 可能下线 |
| Qwen3-VL-235B | `Qwen3-VL-235B` | 1,048,576 | 131,072 | `display_enabled:false`，API 可调用 |

实测额度：

```json
{
  "package": {"spec_code":"codearts.agent.trial","package_name_en":"Free","is_token_package":true},
  "metrics": [{"name":"usageTokenChatMessages","usage_token_num":41170,"package_token_amount":5000000}],
  "start_date":"2026-11-25",
  "end_date":"2026-12-26"
}
```

### 4.4 凭证事实

- 国际站 `state.vscdb` 的 `huaweicloud.codearts-snap` -> `userInfoKey.originData` 包含：
  - `id` / `account.id` / `account.label`
  - `accessKey`, `secretKey`, `securityToken`, `expiresAt`, `stationType`
- 加密的 `HuaweiCloudSession` 包含：
  - `account.id` / `account.label`
  - `refresh_token`
  - `loginContext.pkcePair.codeVerifier`
  - `loginContext.dpopKeyPair.privateKeyJwk`
- `refresh_token` 是一次性轮转的；IDE 和网关不能同时刷新。
- `auth.huaweicloud.com/authui/login?site=intl` 是国际站身份体系入口；直接访问 portal authorize 会落到国内登录页。
- 国际站与国内站共用 STS 端点，但凭据必须带 `station` 并持久化。
- **账号 UID**：优先 `account.id`，其次 `originData.id`，最后从 refresh token JWT 的 `user_profile`/`sub` 解析。UID 决定账号文件名、去重和面板身份，必须显式写入 Auth。
- **Security Token 与 x-auth-token 分离**：国内站把 `CloudDragonToken` 同时用作 `X-Security-Token` 和 `x-auth-token`；国际站常见 `accessToken` 为空。Auth 需要独立的 `SecurityToken` 字段，避免把空 `x-auth-token` 当成空签名 token。

## 5. Approaches

### A. Station profile + 两个 instance（选定）

- 一个 `codearts` package
- `Station` 结构体描述站点差异
- `New(store, station)` 构造，注册 `codearts` 和 `codearts-intl` 两个 instance
- 配置/账号目录按 station 分开

优点：共用最多代码；差异显式；新增站点只需 profile；符合 WorkBuddy 先例。
缺点：需要一次较大的参数化重构；现有测试要跟着改构造方式。

### B. 复制 `codearts` 为 `codeartsintl`

- 新 package，复制国内实现后改 host/模型/额度

优点：短期快。
缺点：两份代码漂移；签名/流式/错误处理修一次要改两处。拒绝。

### C. 单 Provider 内部多 realm

- 一个 instance 同时服务国内/国际，按账号 station 路由

优点：少一个 instance。
缺点：Registry 以 realm 为键；API key 绑定、路由、面板、健康检查都依赖 realm；破坏现有契约。拒绝。

### 凭证子方案

- **A1（第一阶段，选定）**：导入 session JSON + provider 内 DPoP refresh。
  - 最接近国内版，无 Windows 运行时依赖。
  - 使用 `CodeArts2API -export-session` 或等价小工具从 Windows 导出一次。
- **A2（第二阶段）**：面板直接发起国际站 OAuth。
  - 复用现有 `codearts` OAuth loopback + callback URL paste。
  - 登录入口加 `site=intl`，portal/locale 从 station 取。
- **A3（可选 fallback）**：Windows sidecar 只读 `state.vscdb`，提供短期 AK/SK。
  - 不作为第一阶段依赖；只在无法导入 refresh token 时启用。

## 6. Design

### 6.1 Station profile

新增 `internal/providers/codearts/station.go`：

```go
type Station struct {
    ID            string // "mainland" / "international"
    Realm         string // "codearts" / "codearts-intl"
    ProviderID    string // "codearts" / "codearts-intl"（ID() 返回值）
    SettingsKey   string // "codearts" / "codearts-intl"
    AccountsDir   string // "codearts" / "codearts-intl"（相对 accounts 根目录）
    SnapHost      string
    OpenGWHost    string // 国际站为空
    Portal        string
    LoginURL      string // 国际站 auth.huaweicloud.com/authui/login?site=intl
    Locale        string
    SupportsBenefit bool
    SupportsCheckin bool
    QuotaKind     string // "credit" / "token"
    StopMode      string // "reject" / "array-only"
    SignOptions   SignOptions
    ModelSeeds    []string
    ModelOwner    string
}
```

预置：

```go
var Mainland = Station{ ... }
var International = Station{ ... }
```

`chat.go`/`catalog.go`/`oauth.go`/`config.go`/`pool.go` 从 `p.station` 取配置。

### 6.2 Provider instances

`codearts.New(store)` 保持为国内站兼容入口；新增：

```go
func New(store *config.Store, station Station) (*Provider, error)
func NewMainland(store *config.Store) (*Provider, error)
func NewInternational(store *config.Store) (*Provider, error)
```

`internal/app/providers.go` 注册两个：

```go
codeartsCN, _ := codearts.NewMainland(store)
codeartsIntl, _ := codearts.NewInternational(store)
registry.Register(codeartsCN)
registry.Register(codeartsIntl)
```

两个 Pool 使用不同目录：

```text
<accounts>/codearts/...
<accounts>/codearts-intl/...
```

这样不会有两个 Pool 同时写同一个 `state.json`。

### 6.3 Settings

- 国内：`settings["codearts"]`（保持现有结构）
- 国际：`settings["codearts-intl"]`
- 共用字段：`enabled`、`timeout_seconds`、`stream_timeout_seconds`、`models_ttl_seconds`、`max_concurrent`
- 国内专属：`benefit_auto_claim`、`credits_auto_claim`、`credits_expiring_priority`、`checkin_time`、`checkin_retry_minutes`
- 国际不读取/不保存国内专属字段；面板可隐藏

`LoadConfig(store, station)` 与 `SaveConfig(store, station, patch)` 参数化。

### 6.4 Accounts / Auth

`Auth` 增加：

```go
Station       string `json:"station,omitempty"`        // ""/mainland 兼容旧账号
SecurityToken string `json:"security_token,omitempty"` // 国际站 X-Security-Token；国内站留空时回退 CloudDragonToken
```

导入规则：

- 旧国内账号没有 `station` -> 视为 `mainland`。
- 国际账号必须 `station: "international"`。
- 国内路由导入国际 session、或国际路由导入国内 session，必须在写盘/刷新前拒绝；否则会用错误的 host 签名并消耗一次性 refresh token。
- 国际 session JSON 适配：
  - `account.id`（或 `userInfoKey.id` / JWT claim）-> `UserID`
  - `account.label` -> `UserName`
  - `accessKey` -> `AccessKeyID`
  - `secretKey` -> `SecretAccessKey`
  - `securityToken` -> `SecurityToken`
  - **不**把 `securityToken` 写进 `CloudDragonToken`；国际站 `x-auth-token` 可以为空
  - `refresh_token` -> `RefreshToken`
  - `loginContext.pkcePair.codeVerifier` -> `CodeVerifier`
  - `loginContext.dpopKeyPair.privateKeyJwk` -> `DPopPrivateKey`
  - `expiresAt` -> `Expiration`
  - `domainId` -> `DomainID`
  - `stationType` / `station` -> `station: "international"`
- Import validation:
  - `UserID` 必填；缺失时尝试 JWT，仍缺失则 400。
  - 国际站要求 `AccessKeyID`、`SecretAccessKey`、`SecurityToken` 至少可用于签名；`SecurityToken` 为空时仍允许 IAM 永久 AK/SK 模式，但要显式标记。
  - `Page.Add` 的 `CloudDragonToken` 非空校验改为 station-aware：国内要求 `CloudDragonToken`，国际要求 `SecurityToken` 或显式永久 AK/SK。
- 新增 `POST /codearts-intl/accounts/import-session` 或在现有 import 中接受 `session_json`。第一阶段优先做 JSON 导入，不要求用户手工拆字段。

### 6.4.1 模型选择与 realm 冲突

国内站目录也已经包含 `GLM-5.3`、`GLM-5.2`、`Qwen3-VL-235B`，国际站使用相同上游 ID。裸模型名会产生歧义，必须显式选择：

- 优先：`codearts-intl/glm-5.3`（现有 `parseModelRef` 的 `exit/model` 机制）
- 或：`X-Realm: codearts-intl` / `?realm=codearts-intl`
- 或：API key 绑定 `codearts-intl`

`/v1/models` 仍可分别按 realm 查询；面板应显示两个 realm 的模型，并提示同名模型使用 `realm/model`。
不要依赖“当前 active realm”静默决定同名模型，否则可能扣错 5M/积分额度。

### 6.4.2 Station mismatch 防御

- 账号文件里的 `station` 必须与 Provider 的 station 一致；不一致的账号在加载时跳过并记录原因，不参与调度。
- OAuth 回调、import、refresh 三条路径都要检查 station。
- refresh 后保留原 `station`；如果 STS 返回的刷新结果没有 station 字段，沿用旧值。

### 6.5 Token refresh

- 复用现有 `TokenManager` 与 DPoP/STS 逻辑。
- STS endpoint 两边相同；refresh 时保留 `Station`。
- `refresh_token` 一次性：
  - provider 内 single-flight（现有 TokenManager 已有）。
  - 不与 IDE 同时刷新；导入后由 provider 持有，IDE 里的旧 token 可能失效。
  - 若用户仍要 IDE 同时使用，需要 sidecar 只读模式或人工重新登录。
- 刷新失败分类：
  - `STS5.1806` / `invalid_grant` / `refresh token has been used` -> 标记需重新登录，不重试。
  - 网络/DPoP 临时错误 -> 保留原凭据，稍后重试。

### 6.6 Chat / signing

- `newChatRequest` 使用 `p.station.SnapHost`。
- `X-Language` 用 `p.station.Locale`。
- `x-auth-token` 仅当 `CloudDragonToken` 非空时发送（国际站可能为空）。
- `app-id: CodeAgent3.0` 保留。
- `maas_type` 仅在 `station.SupportsBenefit` 且账号是 benefit 模型时发送；国际站不发送。
- `stop` 归一化：
  - `mainland`：**保持现有 mix2api 行为**（当前代码把字符串转成单元素数组并转发；现有测试期待 `[END]`）。本阶段不根据 magpie 的国内站观察去改国内行为；如果要改，单独任务、单独实测。
  - `international`：字符串转单元素数组；数组保留；空值删除。国际站实测只接受数组。
- 签名：
  - 复用现有 `SignRequest`，先按国内站行为验证国际站是否接受 `Content-Type` 参与签名。
  - 若不接受，`SignOptions` 增加 `IncludeContentType bool`，国际站设为 false。
  - `X-Sdk-Content-Sha256` 模式可配置：body hash / UNSIGNED-PAYLOAD。
- 会话头：
  - `Chat-Id` / `Session-Id` / `x-ot-session-id` 继续按账号稳定派生，避免每次新会话撞并发上限。
  - 国际站并发上限尚未确认；先沿用现有 `MaxConcurrent` 与错误分类，实测后调整。

### 6.7 Catalog

- `snapHost` 从 station 取。
- 国内：builtin + agent detail + opengw benefit。
- 国际：builtin + agent detail；**不请求** opengw/benefit。
- 国际模型种子：只作为**冷启动/发现失败**的 fallback，不能像国内 `officialCoreModels` 一样每次 `Models()` 都无条件合并。
  - 冷启动种子：`glm-5.3`、`GLM-5.2`、`Qwen3-VL-235B`。
  - `GLM-5.1` **不放入永久种子**；它日志提示 2026-11-30 下线，必须靠目录发现，发现不到就消失。
  - 如果账户已有成功目录，`Models()` 只使用目录结果 + 明确的上游发布模型，不把种子强行加回。
- 保留上游原 ID 大小写；聊天请求 body 和 header 必须一致。
- `owned_by`：国内 `codearts`，国际 `codearts-intl`（wire `provider` 字段也随 realm）。
- 模型能力：
  - GLM 文本；Qwen3-VL 支持图片。
  - `Qwen3-VL` 上游 `display_enabled:false`，面板可标记“国际站实验/隐藏模型”，默认仍可调用。
- 模型下线：`GLM-5.1` 日志提示 2026-11-30 下线，目录刷新后自然消失；不硬编码为永久可用。

### 6.8 Usage / quota

- 国内：现有 credit 逻辑不变。
- 国际：解析 `/snap-manager/v1/statistics/plugin`：
  - `package.is_token_package == true`
  - `metrics[].name == "usageTokenChatMessages"`
  - `usage_token_num` / `package_token_amount`
  - `start_date` / `end_date`
- 额度窗口：
  - 名称 `Tokens`
  - used / limit
  - `resetsAt = end_date`
  - `Unit = "token"`
- 额度存储与调度：
  - 复用 `AccountCredits` / `CreditTotal` 的 `Amount/Used/Remain` 形状，但 `Unit` 明确为 `"token"`。
  - `creditExhaustedLocked` 改为按“额度耗尽”判断：`Remain <= 0` 时停用该账号，无论单位是 credit 还是 token。
  - `reserveBlockedLocked` **只对 `Unit == "credit"` 生效**；不要把 `reserve_credits` 当成 token 数。若以后需要 token 预留，新增 `reserve_tokens`，不要复用 credit 字段。
  - 面板同时显示 `unit`，避免把 5M tokens 显示成 5M 积分。
- 账号状态/overview 显示 `Free`、5M 额度、已用/剩余。
- 国际站不注册签到/福利 job；`Jobs()` 只保留 token refresh/额度刷新。

### 6.9 Routes / panel

- 现有路由 `/codearts/...` 保持国内站兼容。
- 国际站路由前缀 `/codearts-intl/...`，由 provider 的 `Routes()` 返回；或由 core 按 realm 自动加前缀（需确认现有机制，优先复用现有路由注册方式）。
- 面板：
  - 两个 realm 独立显示，名称“CodeArts（国内）”“CodeArts（国际）”。
  - 国际站隐藏签到/积分批次/福利入口。
  - 账号导入支持“粘贴 session JSON”。
  - 额度显示 token 而不是积分。
- API key 的 realm 绑定沿用现有机制；不改变国内 key 行为。

### 6.10 Compatibility / migration

- 现有 `data/accounts/codearts` 不改名、不迁移。
- 新国际账号写入 `data/accounts/codearts-intl`。
- 现有 settings `"codearts"` 不改 schema。
- 新增 `"codearts-intl"` section，默认 disabled。
- 旧 Auth JSON 无 `station` -> mainland。
- API 响应中的 `provider`/`owned_by` 国内保持 `codearts`，国际为 `codearts-intl`。

## 7. Difference Matrix and Tests

### 7.1 Unit tests

1. `Station` profiles：
   - 国内 host/portal/locale/opengw/checkin
   - 国际 host/portal/login URL/locale/no opengw/no checkin
2. OAuth：
   - 国内 authorize URL 不变
   - 国际 authorize URL 先包 `auth.huaweicloud.com/authui/login?site=intl`
   - 国际 callback result URL 用国际 portal
3. Auth：
   - 无 station 的旧 JSON -> mainland
   - 国际 session JSON -> 字段映射正确
   - `station` 在 refresh 后保留
4. Signing：
   - 国内签名 golden vector 不变
   - 国际签名 golden vector（含/不含 content-type 两个变体）
5. Chat：
   - 国际 host / locale / `x-auth-token` 行为
   - `stop` 国内删除、国际数组化
   - 国际不发送 `maas_type`
6. Catalog：
   - 国际跳过 opengw
   - 国际模型种子/ID 大小写
   - `Qwen3-VL` vision 能力
7. Usage：
   - 国际 5M token metrics -> token window
   - 国内 credit 解析不变
8. Pool：
   - 国内/国际账号目录隔离
   - 旧国内账号仍加载
9. Routes：
   - `/codearts` 与 `/codearts-intl` 路由不冲突
   - 面板 overview 字段区分 realm

### 7.2 Integration / live smoke

1. 用当前 Windows 客户端导出国际站 session JSON。
2. 导入 `codearts-intl` 账号。
3. `/v1/models` 看到 4 个国际模型。
4. 非流式 `glm-5.3` + 流式 `GLM-5.2` + `Qwen3-VL-235B` 成功。
5. `/snap-manager/v1/statistics/plugin` 显示 5M/已用。
6. refresh 测试：
   - 使用一次性 refresh token，确认刷新后 `station` 保留。
   - 记录 IDE 与 provider 不能同时刷新的行为。
7. 国内站回归：现有 `/v1/models`、chat、额度、签到测试全绿。

## 8. Rollout Phases

### Phase 1：Station 抽取（无行为变化）

- 新增 `Station`，国内站用 `Mainland`
- 参数化 config/pool/oauth/chat/catalog/usage 的硬编码
- 现有测试全绿
- 产物：一个 provider，行为不变

### Phase 2：国际站骨架

- 新增 `International` profile
- 注册 `codearts-intl`，默认 disabled
- 独立 accounts 目录/settings key
- 国际模型种子、no opengw/no checkin
- 产物：面板能看到国际站，未启用/无账号

### Phase 3：国际站凭证与聊天

- session JSON 导入
- 国际 DPoP refresh
- chat host/locale/sign/stop
- `/v1/models`、chat、SSE
- 产物：国际站可真实调用

### Phase 4：国际站额度与面板

- token 额度解析
- overview/health
- 面板隐藏国内专属功能
- 产物：可运营的国际站入口

### Phase 5：国际站 OAuth（可选）

- `site=intl` 登录入口
- callback URL paste（现有 OAuth 已支持）
- 产物：无需手工导出 session

### Phase 6：Sidecar（可选 fallback）

- 只在需要“IDE 与网关同时存活”时实现
- 只读 `state.vscdb` / 提供短期 AK/SK
- 不进入核心 provider

## 9. Risks

1. **国际站行为仍可能变化**：内部接口、模型、额度、并发限制都可能调整。Station profile 和测试要允许快速修改。
2. **refresh_token 一次性**：provider 刷新会让 IDE 旧 token 失效；必须明确告知用户，或提供 sidecar 只读模式。
3. **5M 是试用额度**：30 天、50 seats、可能受条款限制；不能当成长期无限池。
4. **Qwen3-VL 隐藏**：上游 `display_enabled:false`，可能随时关闭；面板标记实验性。
5. **GLM-5.1 下线**：目录刷新自然处理，不硬编码。
6. **签名细节差异**：国际站可能不接受 `Content-Type` 参与签名；用 `SignOptions` 和 golden tests 验证。
7. **并发限制未知**：先沿用现有 MaxConcurrent，实测后调整；不要把并发 400 当账号失效。
8. **许可证**：借鉴 `CodeArts2API`（Apache-2.0）或 `magpie`（MIT）代码时保留 NOTICE/版权；只复制必要片段。

## 10. Decisions Made (builder may rely on these)

- 选定 Approach A：一个实现 + Station profile + 两个 instance。
- ProviderID：国内 `codearts`，国际 `codearts-intl`；Realm 同名。
- 账号目录：`accounts/codearts`、`accounts/codearts-intl`。
- Settings：`codearts`、`codearts-intl`。
- 第一阶段凭证：session JSON 导入 + provider 内 refresh；sidecar 不做。
- 第二阶段：国际站 OAuth `site=intl`；复用 callback URL paste。
- 国际站不启用 benefit/opengw、签到、积分批次。
- 国际站额度按 token package 解析，存入 `AccountCredits.Total` 且 `Unit="token"`；`reserve_credits` 不作用于 token。
- `stop`：国内保持现有 mix2api 行为不变；国际字符串转数组。
- 旧 Auth 无 station -> mainland；国际账号必须显式 `station: "international"`。
- 国际导入 UID 优先 `account.id`，其次 `id`，最后 JWT。
- 国际 `security_token` 存独立 `SecurityToken` 字段，不塞进 `CloudDragonToken`；`x-auth-token` 可为空。
- 同名模型必须用 `codearts-intl/<model>`、`X-Realm` 或 API key 绑定明确选择；不静默按 active realm 选。
- 国际站模型种子仅用于冷启动；`GLM-5.1` 不永久种子，目录没有就消失。
- 国内站行为、路由、API key 绑定不改。

## 11. References

- magpie CodeArts plugin: https://github.com/magpie-community/plugins/tree/main/packages/codearts
- magpie #1090: https://github.com/magpie-community/plugins/issues/1090
- CodeArts2API: https://github.com/2951461586/CodeArts2API
- HITZY2002/codearts2api: https://github.com/HITZY2002/codearts2api
- Coding2API: https://github.com/RobbsLuo/Coding2API
- WorkBuddy2API-Hub: https://github.com/niyouqianma/workbuddy2api-hub
- 现有国内 provider: `internal/providers/codearts`
- 现有双 realm 先例: `internal/providers/workbuddy`

## 12. Open Items for Builder

- 现有 `routes.go` 的路由前缀是否由 core 自动按 realm 加；如果不是，需新增 `codearts-intl` 前缀并保持国内兼容。
- 现有 `OAuthSessions` 必须在每个 instance 内独立创建，不能共享同一张 session table；两个 realm 各自隔离。
- 现有 `credits.go` 的接口可以保留国内实现，国际站加一个 token 解析函数；不建议为了统一而重写国内 credit 逻辑。
- 国际站 `Content-Type` 是否参与签名需要真实请求验证；先做 golden test（两种 canonicalization）+ live smoke，确认后固定 `SignOptions`。
- 国际站 `stop` 已验证只接受数组；国内站是否也应改成删除，需要单独实测，不在本设计内改。
- 面板前端是否需要新组件，取决于现有 provider 页面是否按 realm 动态渲染；若现有页面已按 realm 分组，只加标签/隐藏字段。
- 国际站账号可用性：当前 Windows 客户端可用于导出 session；CI 里用 fake STS/snap upstream，不依赖真实账号。