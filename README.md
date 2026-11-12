# mix2api v2

[中文](README.md) | [English](README.en.md)

mix2api v2 是一个用 Go 重写的多出口 AI 网关。一个进程、一个二进制，同时提供
OpenAI Chat Completions、Responses、Anthropic Messages、SSE 和内嵌看板，把
WorkBuddy 国际版 / 国内版、华为云 CodeArts（遥遥领先）、Raccoon（商汤小浣熊）收进同一套
API Key 和账号池。

当前版本：**2.1.1**。默认分支 `v2`；`main` 保留为冻结的 v1.6.6 历史。

## 由来

mix2api 承接于 WorkBuddy2API-Hub 社区项目，并参考 CodeArts2API 的协议实现。

v1 是 Python 单进程版本，最终冻结在 1.6.6；v2 是从 v1.6.5 / v1.6.6 的行为
和数据格式出发重新设计的 Go 项目，不是逐行翻译。v1 的 Python 实现作为行为
基准保留，后续功能、修复和发布都在 v2 进行。

## v2 的架构

v2 的核心变化是把“所有东西放在一个应用里”改成“通用核心 + 独立 Provider”：

```text
客户端 / SDK
   │  OpenAI Chat / Responses / Anthropic Messages / SSE
   ▼
mix2api v2（单二进制）
   ├─ API / SSE / 内嵌看板
   ├─ core：配置、账号状态、传输、调度、用量、日志
   ├─ providers：WorkBuddy 国际版 / 国内版、CodeArts（遥遥领先）、Raccoon（商汤小浣熊）
   └─ platform：面板、媒体、智能体、定价、更新等服务
```

- **单进程、单二进制**：生产构建 `CGO_ENABLED=0`，不需要 Python、Node 或
  sidecar 运行时。
- **Provider 编译期注册**：每个上游是一个独立 package，新增出口时只增加
  Provider，不修改通用路由、账号和用量逻辑。
- **看板仍是内嵌的静态 HTML/JS**：由 `go:embed` 打进二进制，不引入前端
  构建链。
- **数据兼容 v1.6.6**：账号文件、`state.json`、`settings.json`、usage JSONL
  和日志库保持可读写；未知字段会保留。迁移是 copy-only，v1 和 v2 不能共用
  同一个运行目录。

## 能力概览

- 四个出口：WorkBuddy 国际版、WorkBuddy 国内版、CodeArts（遥遥领先）、Raccoon（商汤小浣熊）；可按
  API Key 绑定出口，也可以跟随看板默认出口。
- 三类协议：`/v1/chat/completions`、`/v1/responses`、Anthropic Messages，
  以及统一的 SSE 流式输出。
- 账号池、并发/冷却、代理槽、模型目录、积分/额度护栏、用量、日志和活动记录
  都由同一个进程管理。
- 看板内置账号管理、任务、设置、用量、日志和更新/回滚入口。

## 部署

### 方式一：Docker 镜像（推荐）

镜像已发布到 GHCR，支持 `linux/amd64` 和 `linux/arm64`：

```sh
docker pull ghcr.io/ayeaaaa/mix2api:v2.1.1
```

先在当前 shell 生成两个独立凭据，不要把真实值写进仓库或提交到 Git：

```sh
export PANEL_PASSWORD="$(openssl rand -base64 24)"
export MIX2API_API_KEY="sk-$(openssl rand -hex 24)"
```

准备数据目录。容器默认以 `10001:10001` 运行：

```sh
mkdir -p data/accounts data/usage data/media
sudo chown -R 10001:10001 data
```

启动：

```sh
docker run -d --name mix2api \
  --restart unless-stopped \
  -p 127.0.0.1:58675:58675 \
  -e HOST=0.0.0.0 \
  -e PORT=58675 \
  -e TZ=Asia/Shanghai \
  -e PANEL_PASSWORD \
  -e MIX2API_API_KEY \
  -v "$PWD/data/accounts:/app/accounts" \
  -v "$PWD/data/usage:/app/usage" \
  -v "$PWD/data/media:/app/media" \
  ghcr.io/ayeaaaa/mix2api:v2.1.1
```

这里只把端口发布到 `127.0.0.1`，适合前置反向代理或用 SSH 隧道访问。需要让
局域网访问时，再按实际网络改成受控的监听地址，不要直接把管理面板暴露到公网。

检查健康状态：

```sh
curl -fsS http://127.0.0.1:58675/health
```

### 方式二：Docker Compose

仓库提供预构建镜像的 compose 文件：

```sh
cd deploy
cp ../.env.example .env
chmod 600 .env
```

编辑 `.env`，至少把 `PANEL_PASSWORD` 和 `MIX2API_API_KEY` 改成真实值；然后：

```sh
mkdir -p ../data/accounts ../data/usage ../data/media
sudo chown -R 10001:10001 ../data
docker compose -f docker-compose.image.yml up -d
curl -fsS http://127.0.0.1:58675/health
```

如果要本地构建镜像，使用 `docker-compose.yml`：

```sh
cd deploy
docker compose up -d --build
```

compose 默认发布 `58675` 到所有网卡。公网部署前请加反向代理和 TLS，并限制
管理面板来源。

### 方式三：直接运行二进制

从 [GitHub Release](https://github.com/ayeaaaa/mix2api/releases) 下载对应平台
的压缩包和 `SHA256SUMS`，先校验再解压：

```sh
sha256sum -c SHA256SUMS
tar -xzf mix2api-2.1.1-linux-amd64.tar.gz
cd mix2api-2.1.1-linux-amd64
```

启动。`HOST` 默认是 `127.0.0.1`；数据目录默认是当前目录下的
`accounts/`、`usage/`、`media/`：

```sh
export PANEL_PASSWORD="$(openssl rand -base64 24)"
export MIX2API_API_KEY="sk-$(openssl rand -hex 24)"
./mix2api
```

需要指定绝对路径时：

```sh
ACCOUNTS_DIR=/srv/mix2api/data/accounts \
USAGE_DIR=/srv/mix2api/data/usage \
WB_MEDIA_DIR=/srv/mix2api/data/media \
./mix2api
```

Windows 和 macOS 的发布包结构相同；OpenWrt 的 init/UCI 说明在
[`packaging/openwrt/README.md`](packaging/openwrt/README.md)。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `HOST` | `127.0.0.1` | 监听地址；非 loopback 必须同时配置两个凭据 |
| `PORT` | `58675` | HTTP 端口 |
| `PANEL_PASSWORD` | 无 | 面板密码；仅用于首次初始化，之后以看板内修改的密码为准 |
| `MIX2API_API_KEY` | 无 | `/v1` API Key；客户端用 `Authorization: Bearer <key>` |
| `MIX2API_API_KEY_REALM` | 空 | 可选，把 Bootstrap Key 固定到一个出口 |
| `MIX2API_TLS_FINGERPRINT` | `python` | WorkBuddy 国际/国内 HTTPS 出站 TLS 指纹；`node` 启用内置 uTLS 桥，`python` / `go` / `off` 关闭；兼容 `WB_TLS_FINGERPRINT` |
| `ACCOUNTS_DIR` | `accounts` | 账号和设置目录 |
| `USAGE_DIR` | `usage` | 用量和日志目录 |
| `WB_MEDIA_DIR` | `media` | 媒体任务目录 |
| `TZ` | 系统时区 | 调度器使用的本地时区，建议显式设置 |

`PANEL_PASSWORD` 和 `MIX2API_API_KEY` 是两套独立凭据：前者只用于打开看板，
后者只用于调用 `/v1`。生产环境请使用随机值，不要把真实值写进 README、
compose 文件、日志或 Git。

`MIX2API_TLS_FINGERPRINT=node` 只影响 WorkBuddy 国际版/国内版的 HTTPS
出站，复刻官方 Node/OpenSSL ClientHello；默认关闭，修改后需要重启。代理槽、
证书校验和 SSE 行为保持不变，细节见 [`docs/utls-bridge.md`](docs/utls-bridge.md)。

### 首次使用

1. 打开 `http://127.0.0.1:58675/`，用面板密码登录；首次登录后立即修改密码。
2. 在「设置」中启用需要的出口，再到对应视图导入账号：
   - WorkBuddy：按看板提示完成 OAuth；
   - CodeArts（遥遥领先）：华为云 OAuth；
   - Raccoon（商汤小浣熊）：导入桌面客户端 `auth.json` 或走本机回环授权。
3. 在「设置」中创建 API Key，按需绑定出口和模型。
4. 用 `/v1/models` 和一次最小聊天请求验证：

```sh
curl -fsS http://127.0.0.1:58675/v1/models \
  -H "Authorization: Bearer $MIX2API_API_KEY"
```

### 反向代理与安全

- 建议让 mix2api 只监听 `127.0.0.1`，由 Caddy/Nginx 等反向代理提供 HTTPS；
  不要让面板和 `/v1` 直接裸奔在公网。
- 管理面板使用独立的 `PANEL_PASSWORD`；API 客户端使用 `MIX2API_API_KEY`。
- 不要把 `accounts/`、`usage/`、`media/`、`.env` 或账号 JSON 提交到仓库。
- 日志和错误信息默认做了脱敏；仍应在反向代理层限制访问来源和请求体大小。
- 本项目的账号风控、限流、封禁风险由使用者自行承担。

### 升级、迁移与回滚

- **v2 小版本升级**：备份数据目录后替换二进制或拉取新镜像；容器重建时保持
  原来的三个数据卷不变。
- **从 v1 迁移**：先停 v1，使用 copy-only 迁移，原目录不会被修改：

  ```sh
  mix2api migrate --from <v1-data-dir> --to <v2-data-dir> --dry-run
  mix2api migrate --from <v1-data-dir> --to <v2-data-dir>
  ```

- **回滚**：用 `mix2api rollback` 生成一份 v1 可读的新目录，不要把正在运行的
  v1 和 v2 指向同一个目录。完整参数见
  [`docs/migration.md`](docs/migration.md)。
- 更新执行器的备份、校验和恢复流程见
  [`docs/update-executor.md`](docs/update-executor.md) 和
  [`docs/runbook.md`](docs/runbook.md)。

## 维护与发布

- `v2` 是默认开发分支；`main` 只保留 v1.6.6 冻结历史，不再接收功能。
- 版本号、构建身份和发布资产由 `internal/core/version`、`make dist` 和
  [`.github/workflows/release.yml`](.github/workflows/release.yml) 管理；镜像由
  根目录 `Dockerfile` 构建。
- 构建后检查二进制身份：

  ```sh
  ./bin/mix2api version --json
  ```

- 本地检查：

  ```sh
  make check   # gofmt + vet + tests
  make race    # race detector
  make build   # 生成 bin/mix2api
  ```

- 发布流程见 [`docs/release.md`](docs/release.md)，发布门槛和已知运行时缺口见
  [`docs/gate-f.md`](docs/gate-f.md)。

## 文档

- [打包与部署资产](docs/packaging.md)
- [迁移与回滚](docs/migration.md)
- [更新执行器](docs/update-executor.md)
- [运维手册](docs/runbook.md)
- [发布流程](docs/release.md)
- [Gate F](docs/gate-f.md)
- [安全策略](SECURITY.md)
- [变更记录](CHANGELOG.md)

## 边界与许可

本项目仅用于协议学习、研究和自用。请优先使用官方客户端，不要使用主账号，
不要共享或转售账号；因非官方客户端导致的风控、限流或封禁由使用者自行承担。

MIT License，见 [`LICENSE`](LICENSE)。