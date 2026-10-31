# mix2api

把 **WorkBuddy 国际版**、**WorkBuddy 国内版** 和 **华为云 CodeArts** 三个上游，收进一个 OpenAI 兼容网关、一个看板、一套 API Key。

```text
客户端 ──► mix2api / hub ──► WorkBuddy 国际版
                        ├──► WorkBuddy 国内版
                        └──► codearts2api ──► 华为云 CodeArts
```

- **一个地址**：`http://<host>:8788/v1`
- **三个出口**：看板顶部切换 `国际版 / 国内版 / CodeArts`；也可以给不同 API Key 各绑一个出口，三个同时用。
- **一个看板**：WorkBuddy 和 CodeArts 的账号都在这里增删、启停、签到；CodeArts 的账号池、token 续期仍由 codearts2api 负责。
- **三类协议**：`/v1/chat/completions`、`/v1/responses`（Codex）、Anthropic Messages（需显式开启）都能走 CodeArts。

## 目录

| 路径 | 内容 | 来源 |
|---|---|---|
| `apps/hub` | Python 网关 + 看板（三出口） | [workbuddy2api-hub](https://github.com/ardeyouxipianyi/workbuddy2api-hub) 派生 |
| `apps/codearts` | Go 服务，CodeArts 账号池 / token / 签到 | [codearts2api](https://github.com/HITZY2002/codearts2api) 派生 |
| `deploy/` | 一套 compose 把两个服务拉起来 | 本仓库 |
| `docs/` | 设计、实施计划、部署说明 | 本仓库 |

两个上游都是 MIT，版权与同步方式见 [`UPSTREAM.md`](UPSTREAM.md)。

## 快速启动（Docker）

```bash
cd deploy
cp .env.example .env
# 改 .env 里的 CA2A_API_KEY（codearts2api 的访问密钥，必填）
docker compose up -d --build
```

也可以：

```bash
make build   # 构建两个镜像
make up      # 启动
make test    # 跑 hub + codearts 的全部测试
make logs    # 跟踪日志
```

打开 `http://127.0.0.1:8788/`：

1. 用面板密码登录（默认 `admin`，首次登录后立刻改）。
2. 进「设置 → CodeArts 接入」，填 `http://codearts:7866` 和 `.env` 里的 `CA2A_API_KEY`，点「测试连接」，启用。
3. 切到 CodeArts 视图，点「+ 添加账号 (OAuth)」，用华为云账号完成授权。
4. 在「设置 → API Key」里建一个 Key，出口选 `CodeArts`（或留空跟随顶部默认出口）。

CodeArts 自己的 WebUI 仍在 `http://127.0.0.1:7866/`（compose 只绑本机），账号池的底层状态以它为准。

## 本地开发

```bash
# 终端 1：CodeArts 上游
cd apps/codearts
cp config.example.json config.json
export CA2A_API_KEY=$(openssl rand -hex 24)
go run ./cmd/server -config config.json

# 终端 2：hub
cd apps/hub
python wb_proxy.py --port 8788
```

hub 的 CodeArts 配置指向 `http://127.0.0.1:7866`，Key 与上面的 `CA2A_API_KEY` 一致。

## 安全

- CodeArts 的 API Key 只存在 hub 服务端的 `accounts/settings.json`；看板只显示「已设置」，不返回明文。
- `/codearts/*` 管理端点只接受面板会话，不接受普通 API Key；删除账号是破坏性操作，会二次确认。
- codearts2api 的 admin API 能停用/删除账号，不要直接暴露到公网；远程部署请套 TLS 和访问控制。
- 本项目仅用于协议学习与个人自用；账号风控、限流、封禁风险由使用者自行承担。

## 文档

- 设计：`apps/hub/docs/superpowers/specs/2026-10-31-codearts-realm-design.md`
- 实施计划：`docs/superpowers/plans/2026-10-31-codearts-realm.md`
- 部署与回滚：`docs/setup.md`

## License

MIT。`apps/hub` 与 `apps/codearts` 保留各自上游的 MIT 许可证与版权声明。
