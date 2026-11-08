# mix2api v2.0 — Go 模块化复刻设计

日期：2026-11-08
基线：`mix2api` v1.6.5（原仓库 `e40c4e8`）
目标：`mix2api-v2.0.0`
状态：设计待评审

## 1. 意图

把现有 Python 单进程网关复刻成一个可长期维护的 Go 项目。主程序只负责通用网关能力，每个上游渠道是一个编译期 Provider 模块。新增渠道时只新增一个 Provider package、在 `cmd` 中注册并重新构建，不修改核心路由、账号、用量或面板逻辑。

原仓库 `mix2api` 保持不动，继续作为 v1.6.5 的行为基准。新项目独立开发、独立测试、独立发布候选；完整 parity 通过后再切换主线。

## 2. 目标 / 非目标

### 目标

- 单进程、单二进制；Go 编译期模块；不做 Go `plugin` 热插拔。
- 行为复刻 v1.6.5 的全部能力：
  - 四个出口：WorkBuddy 国际版、WorkBuddy 国内版、CodeArts、Raccoon
  - OpenAI Chat Completions、Responses、Anthropic Messages、SSE
  - 账号池、代理槽、API Key/realm、模型目录、用量、日志、活动、定价
  - 签到、每日活跃、成长任务、Buddy、猫猫旅行、连登、CodeArts 签到/保活
  - Raccoon 积分、任务、桌面/移动登录奖励、自动领取
  - 媒体生成、智能体一键配置、更新/回滚、Docker/Windows/OpenWrt 发布
  - 原有面板页面与交互
- 保持旧数据的读取/写入兼容：settings、账号文件、state、usage、logs、pricing 缓存、更新状态。
- 保持旧 HTTP API、端口、Header、状态码、错误体语义和 SSE 帧语义。
- 提供完整的 Go 测试、契约测试、并行对比与打包测试。
- v2.0 一次性交付；内部阶段只作为质量门，不发布半成品。

### 非目标

- 不做运行时动态加载或第三方插件市场。
- 不把 dashboard 用 Go 重写；前端仍是静态 HTML/JS。
- 不在 v2.0 中改变产品语义或新增未在 v1.6.5 出现的功能。
- 不把四个 Provider 的积分、模型、签到、任务强行统一。
- 不与旧网关共用同一账号池/写入目录。

## 3. 约束

- 原仓库不修改，不提交 v2 代码。
- 新仓库：`mix2api-v2`。
- Go：实现开始时最新稳定版，`go.mod` 固定版本；生产构建 `CGO_ENABLED=0`（若 SQLite 方案需要 CGO，则必须在设计评审中明确例外）。
- 默认零外部运行时依赖；允许必要的纯 Go 库（例如 SQLite 驱动）。
- 单进程、单二进制；dashboard 通过 `go:embed` 打包。
- 支持 Windows x64、Linux amd64/arm64、OpenWrt 常用架构（至少 amd64/arm64/armv7；具体矩阵按旧发布资产确定）。
- 保留 MIT 许可证与原项目 attribution；dashboard 和测试 fixture 可复用 MIT 资产。

## 4. 总体架构

```text
cmd/mix2api/
  main.go                 # flags/env、启动、注册 providers
  providers.go            # 显式 import + registry.Register

internal/core/
  config/                 # paths、settings.json、环境变量、原子写
  secrets/                # redaction、mask、0600 权限
  storage/                # 账号文件、state、JSONL append、SQLite logstore
  transport/              # HTTP client、连接复用、代理槽、TLS、超时
  usage/                  # usage 记录、缓存、聚合
  pricing/                # 价目、估算、缓存
  activity/               # 活动历史
  logs/                   # 日志、导出、脱敏
  errors/                 # ProviderError、分类
  accountstate/           # 通用账号运行状态
  scheduler/              # job 注册、时区、手动触发、后台线程
  identity/               # 通用身份版本存储；UA/指纹行为由 provider 决定
  catalog/                # 通用模型元数据、缓存
  probes/                 # 通用探测框架；窗口/额度策略由 provider 决定
  update/                 # 更新请求、安装、回滚
  atrest/                 # 静态凭据保护（按旧行为）
  webtools/               # 通用本地网页工具框架
  ipintel/                # 出口 IP / 地区信息

internal/providers/
  base.go                 # Provider、Pool、Account、ChatStream 接口
  registry.go             # realm -> provider
  workbuddy/              # intl + cn 共用一个 provider 实现
  codearts/
  raccoon/

internal/api/
  server.go               # net/http、keepalive、超时、panic recovery
  auth.go                 # API Key / panel / loopback
  routes/
    health.go
    models.go
    chat.go
    responses.go
    messages.go
    admin.go
    panel.go
    media.go
    agents.go
    update.go
  sse/                    # 统一 SSE 解析/聚合/终止帧

internal/web/
  embed.go                # go:embed dashboard
  assets/                 # 旧 dashboard HTML/CSS/JS 兼容资产

tests/
  contract/               # 新旧网关黑盒对比
  provider/               # fake upstream
  integration/            # 真实 HTTP 行为
  packaging/              # Docker/portable/OpenWrt
```

### 依赖规则

- `core` 不 import `providers`、`api`、`web`。
- `providers` 可以 import `core`，不得互相 import。
- `api` 只通过 `providers/registry` 和 `core` 工作。
- `web` 不 import Go 后端。
- `cmd` 是唯一可以 import 所有 provider 的地方。
- 增加 `tests/boundaries`，用 `go list`/AST 检查禁止的 import。

## 5. Provider 模块

### 5.1 接口

```go
type Provider interface {
    ID() string
    Realm() string

    Overview(ctx context.Context, force bool) (any, error)
    Health(ctx context.Context, force bool) (Health, error)
    Models(ctx context.Context, force bool) ([]Model, error)

    OpenChat(ctx context.Context, req ChatRequest) (ChatStream, error)

    Routes() []Route       // provider-specific admin/OAuth/checkin/media routes
    Jobs() []scheduler.Job
}

type ChatRequest struct {
    Raw        map[string]json.RawMessage
    Headers    http.Header
    Model      string
    Messages   []json.RawMessage
    SessionKey string
}

type ChatStream interface {
    Next() ([]byte, error)
    Close() error
}

type Health = map[string]any
type Model = map[string]any

type AuthClass int
const (
    AuthNone AuthClass = iota
    AuthAPIKey
    AuthPanel
)
type Route struct {
    Method  string
    Path    string
    Auth    AuthClass
    Handler http.HandlerFunc
}
```

Provider 内部自己持有配置和账号池，不把这些暴露给核心 API。Provider 的管理/设置路由可以自己读写配置。

- **一个 provider 实例对应一个 realm**。WorkBuddy 注册两个实例（`intl`、`cn`），共享同一套 WorkBuddy 代码。这样 `Overview`/`Health`/`Models` 不需要额外传 realm，registry 直接按 realm 选实例。
- Provider 自己注册 `/codearts/*`、`/raccoon/*` 和 WorkBuddy 的账号/签到/媒体等管理路由。核心只提供通用 `/health`、`/v1/*`、面板登录、API Key、代理槽、用量等路由。
- 可选能力用窄接口，不塞进 `Provider`：
  - `AdminProvider`：账号导入、OAuth、签到、积分刷新
  - `MediaProvider`：图片/视频生成与任务
  - `AgentProvider`：渠道特有的一键配置
  - `UpdateProvider`：渠道特有的更新行为（如有）
- `ChatRequest.Raw` 保留原始 JSON，`Headers` 保留原始请求头；Provider 自己处理 WorkBuddy 的 conversation id、压缩体、身份头，或 CodeArts 的 reasoning 映射。核心只负责通用校验和边界。
- `Overview`/`Health` 返回的现有 wire 字段必须保持 v1 形状；内部可以用 `extra`，但不能改变旧字段语义。

### 5.2 注册

```go
// cmd/providers.go
func registerProviders(r *registry.Registry) {
    r.Register(workbuddy.New("intl"))
    r.Register(workbuddy.New("cn"))
    r.Register(codearts.New())
    r.Register(raccoon.New())
}
```

新增渠道需要：

1. 新建 `internal/providers/newprovider`
2. 实现 `Provider`
3. 在 `cmd/providers.go` 加一行注册
4. 重新编译并发布

不修改 `internal/api`、`core` 或其它 provider。

### 5.3 账号状态

通用状态用组合，不做“大一统 Account 基类”：

```go
type AccountState struct {
    UID              string
    Disabled         bool
    ManualDisabled   bool
    Reason           string
    CoolUntil        time.Time
    LastUsed         time.Time
    Note             string
    TasksEnabled     bool
    ActiveConcurrent int
    MaxConcurrent    int
}
```

Provider 账号对象包含 `State`，另放自己的：

- WorkBuddy：credits/packages、checkin、daily activity、identity
- CodeArts：auth、info、checkin、zero-credit、manual disable
- Raccoon：auth、credits、points tasks、login rewards

### 5.4 调度与错误

Provider 提供：

- `Ready(account)`：停用、冷却、积分、模型能力等
- `PickScore(account)`：并发、last_used、临期积分等
- `ClassifyError(error)`：请求错误 / 鉴权 / 限流 / 账号积分耗尽 / 临时故障
- `OnSuccess` / `OnError`：恢复、冷却、停用、换号

通用循环负责重试/换号；Provider 决定哪些错误可以换号。

## 6. 数据兼容

必须保持原路径和格式：

- `accounts/settings.json`
- `accounts/<workbuddy>.json`
- `accounts/codearts/*.json`、`state.json`
- `accounts/raccoon/*.json`、`state.json`
- `usage/usage.jsonl`、`limit-events.jsonl`、cache 文件
- `usage/logs.sqlite3`
- pricing、catalog、model、update 缓存
- 旧环境变量、默认端口 `58675`、realm 名称、API Key 字段

Go 结构体必须保留未知 JSON 字段：配置写回时不能丢未来字段。建议使用保留原始 `map[string]json.RawMessage` 的 merge 写入。

SQLite 日志使用兼容旧 schema 的纯 Go 驱动；如果实现时确认必须 CGO，需单独评审并说明打包影响。

## 7. HTTP 与协议

- 使用 `net/http`，保持旧路径和别名。
- SSE：逐行读取、flush、取消传播、终止帧、keepalive。
- Chat/Responses/Messages 共用内部 `ChatRequest`/`ChatStream`，Provider 返回规范化事件；API 层负责 OpenAI/Responses/Anthropic 形状。
- 保留旧错误体、状态码、`Server` header（v2.0.0）。
- 支持 `/u/<ref>`、`?realm=`、Key 绑定、账号/模型白名单。
- `realm=all` 仍只含 WorkBuddy，不合并其它 provider。
- 不做跨 provider 回退；WorkBuddy 国际版→国内版回退仍是 WorkBuddy 内部策略。

## 8. 面板

v2.0 先复用 v1.6.5 的 dashboard 资产作为兼容客户端（MIT，保留 attribution），由 Go `embed` 提供。Provider 通过现有 `/codearts/*`、`/raccoon/*`、WorkBuddy 管理端点提供数据；所有旧页面和操作保持可用。

前端模块化不阻塞 Provider 模块化。后续可把 dashboard 拆成 `web/src` 的 shell/shared/provider 模块，但不作为 v2.0 的前置条件；若拆分，必须保持同一 HTML/JS 行为和现有 JS 测试。

## 9. 完整功能清单

### 通用

- API Key、realm、默认出口、Key 账号/模型限制
- 账号代理槽、连接复用、代理认证、no_proxy、TLS/CA
- usage 记录/聚合/最近请求/看板/导出
- pricing、估算、OpenRouter 价、模型倍率
- 日志、活动历史、限流事件、用量窗口
- 健康/概览/模型目录/刷新
- 智能体一键配置、客户端配置文件、DSH/Codex/OpenCode 等
- 媒体生成、图片/视频任务、并发/排队
- 本地网页工具、提示词、窗口探测、额度扫描、出口 IP
- 更新检查、安装、回滚、便携包/外部执行器/systemd
- 面板登录、密码、设置、隐藏页签、语言、主题、移动端
- 调度器手动/定时触发

### WorkBuddy 国际版 / 国内版

- OAuth/导入/账号管理、token 续期
- 模型目录、免费模型、Pro/档位、身份 UA/版本/指纹
- 签到、每日活跃、网页活跃、成长任务、Buddy、猫猫旅行、连登、抽奖
- credits/packages、积分刷新、积分获取历史
- 国际版→国内版回退、跨出口策略、窗口/额度探测
- 媒体与智能体相关行为

### CodeArts

- OAuth PKCE + DPoP、token 续期、账号文件
- 模型发现、benefit、视觉/推理档位
- Chat/Responses/Messages/SSE
- 签到、30 分钟保活、积分查询/刷新
- 手动停用持久化、零积分不调度、积分耗尽换号、临期积分优先
- 代理槽、错误语义、账号操作

### Raccoon

- auth.json/loopback OAuth、token 续期
- 模型目录、Chat/Responses/Messages/SSE
- 积分余额/账单/今日/7 天
- 积分任务、桌面/移动登录奖励、自动领取
- 账号池、代理槽、护栏、账号操作
- 无签到/成长任务

## 10. 交付与验收

### 契约

- 旧网关 v1.6.5 作为 oracle：同一请求、同一假上游，比较规范化后的 JSON/SSE/usage。
- Oracle 使用 **测试专用副本**（`tests/oracle/v1.6.5`）：复制 v1.6.5 源码到测试树，只允许在测试进程内 patch 上游 base URL/identity endpoint；不修改原仓库。已经支持 env override 的端点优先使用 env。
- 规范化规则：
  - JSON：忽略 key 顺序、`Date`、`Server`、动态 UUID/会话 ID/请求 ID、elapsed/timestamp；保留字段存在性、类型、数值和错误码。
  - SSE：比较事件类型序列、role、finish_reason、文本拼接结果、tool call 结构和 usage 数值；不比较 chunk 边界、keepalive 间隔和动态 ID。
  - usage：比较 realm、账号、模型、状态、credit、token 汇总；忽略时间戳和行顺序中的非语义差异。
- 覆盖 health、models、chat、responses、messages、admin、media、agents、update。
- 保留旧 dashboard JS 测试。

### 兼容范围

- 目标：v1.6.5 **shipped dashboard 实际调用的所有路由** + 文档公开 API + 仍被 UI 使用的 legacy alias。
- 公共 API 的旧字段、状态码、错误体、SSE 语义保持原样；不重命名、不删除。只在旧客户端可忽略的位置增加可选字段。
- 未文档化、已无调用方的实验路由不要求复刻，但要在 parity 清单里显式列为 excluded。
- 行为以 bug-for-bug 为目标；安全修复可以例外，但必须记录变更并证明不改变正常请求的成功语义。

### 性能

- 同一机器、同一上游/fake upstream 对比 v1.6.5：
  - 200 并发 SSE 下 p95 首字节延迟不高于 v1.6.5 的 1.2 倍
  - 吞吐不低于 v1.6.5 的 90%
  - 常驻内存不高于 v1.6.5 的 1.5 倍
  - 无数据竞争（`go test -race`）、无 goroutine 泄漏
- 未达到时先定位数据面/锁/序列化问题，不直接接受回退。

### 真实账号与数据

- 自动化测试只使用 fake upstream 和脱敏 fixture。
- 真实账号 smoke test 为**手动验收**，使用 owner 单独提供的测试账号；自动化不得触发签到、领奖、抽奖、媒体生成等可变操作，除非 owner 明确提供专用测试账号。
- 迁移测试使用 v1.6.5 数据目录的**脱敏副本**；不能直接对生产数据目录运行新网关。

### 数据

- 新网关读取旧数据副本并启动。
- 不修改旧数据目录。
- 读/写账号、settings、usage 后旧网关仍能读取。
- 回滚到 v1.6.5 可用。

### Provider

- 三个 provider 的 fake upstream 测试 + 真实账号 smoke test。
- 手动停用、零积分、签到、积分/任务、模型发现、换号、并发、亲和性。

### 打包

- Docker 多架构构建与运行。
- Windows 单 exe + 更新/回滚。
- OpenWrt ipk/apk 按架构构建。
- Release 资产与 SHA256SUMS。
- 旧升级执行器兼容或明确迁移路径。

### 性能

- 与 v1.6.5 对比：吞吐、p95 延迟、并发 SSE、内存、CPU。
- 不允许大幅回退。

### 合并

- 新项目达到 v2.0.0-rc 后，原仓库创建 v2 分支并 snapshot import；不是两个仓库 git merge。
- 原 v1.6.5 保留可回退。
- v2.0.0 发布前完成全量验收。

## 11. 发布与迁移默认策略

- v2 的 GitHub Release / GHCR 先由 **新仓库** 发布；原仓库的 Release 继续停留在 v1.6.5，避免 v1 的更新器把 v2 Go 包当成兼容升级。
- 旧 v1.6.5 安装迁移到 v2 为**手动迁移**：停止旧进程，备份数据目录，启动 v2，使用同一数据副本；不要求旧 Python 更新器自动升级到 Go 包。
- v2 parity 通过后，把新仓库代码快照到原仓库的 `v2` 分支；不在原仓库直接创建 `v2.0.0` tag，直到明确切换发布渠道。
- 回滚：v2 必须保持 v1 数据格式可读；需要回退时停 v2、恢复 v2 启动前备份、启动 v1.6.5。
- 以上是默认策略；如果 owner 要求 v2 直接复用原仓库 Release 渠道，需要额外设计 v1 更新器主版本门控（当前 v1 只比较版本号，会看到 v2）。

## 12. 交付节奏

- 用户已明确不设固定期限，目标一次完整交付 v2.0.0；不使用 MVP 发布。
- 内部按里程碑验收，每个里程碑要求可运行、可对比、可回滚。
- 真实账号 smoke test 放在完整 parity 后手动执行；自动化先用 fake upstream + 脱敏 fixture。
- 团队按单实现流 + 可并行子任务组织；共享状态（账号池、调度、数据目录）不并行写。

## 13. 风险

- Provider 行为差异大，不能强行统一。
- 旧测试大量依赖 Python 内部，不能 1:1 迁移；需要黑盒契约替代。
- SQLite 跨平台/纯 Go 选择可能增加二进制大小。
- OpenWrt 从 all-arch Python 包变为按架构 Go 包，发布矩阵变大。
- dashboard 单文件较大；v2.0 先兼容复用，避免阻塞后端。
- 新旧并行时不能共用账号目录，避免 token/签到竞争。

## 12. 对实现者的默认决定

- Go 使用标准库优先；SQLite 用纯 Go 方案。
- Provider 注册使用显式 `cmd` 注册，不用 `init()` 黑魔法。
- 通用错误/调度/账号状态放 `core`；业务规则留在 provider。
- dashboard v2.0 复用旧资产，不进行框架迁移。
- 不引入 Node 运行时；只有测试可使用 Node（沿用旧 JS 测试）。
- 测试以行为/契约为主，不以复制旧 Python 单元测试为目标。