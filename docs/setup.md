# 部署与运维

## 1. Docker Compose（推荐）

```bash
cd deploy
cp .env.example .env
# 编辑 .env：CA2A_API_KEY=$(openssl rand -hex 24)
docker compose up -d --build
```

两个容器：

| 服务 | 端口 | 说明 |
|---|---|---|
| `mix2api-hub` | `8788` | 统一入口与看板 |
| `mix2api-codearts` | `127.0.0.1:7866` | CodeArts 账号池 / token / 签到；admin API 只绑本机 |

数据目录：

```text
data/hub/accounts/     # hub 的面板密码、API Key、CodeArts 接入配置
data/hub/usage/        # hub 的请求与用量流水
data/codearts/auths/   # CodeArts 账号凭据（由 codearts2api 写）
data/codearts/data/    # codearts2api 的 state.json / 会话状态
```

## 2. 首次配置

1. 打开 `http://<host>:8788/`，用默认面板密码 `admin` 登录，立刻在「设置」里改掉。
2. 进「设置 → CodeArts 接入」：
   - `Base URL`：compose 内填 `http://codearts:7866`；本机直跑填 `http://127.0.0.1:7866`。
   - `API Key`：与 `deploy/.env` 的 `CA2A_API_KEY` 一致。
   - 点「测试连接」，看到 `ok` 后打开「启用 CodeArts 出口」并保存。
3. 顶部出口卡片会出现第三张 `CodeArts`；点它切到 CodeArts 视图。
4. 点「+ 添加账号 (OAuth)」，浏览器打开华为云授权页。授权完成后：
   - 如果浏览器和 codearts2api 在同一台机器，弹窗会自动轮询到 `done`；
   - 如果在远端，浏览器会跳到 `127.0.0.1/oauth/callback?...` 打不开。把地址栏完整地址粘贴到弹窗的输入框，点「中转回调」；如果返回 `continue`，弹窗会自动打开下一个华为云页面，继续轮询即可。
5. 在「设置 → API Key」里新建 Key：
   - 出口选 `CodeArts`：这个 Key 只走 CodeArts；
   - 出口留空：跟随顶部「网关默认出口」；
   - 模型限制可留空，或填 CodeArts 的模型 ID（支持 `*`）。

## 3. 本地开发

```bash
# CodeArts 上游
cd apps/codearts
cp config.example.json config.json
export CA2A_API_KEY=$(openssl rand -hex 24)
go run ./cmd/server -config config.json

# hub
cd apps/hub
python wb_proxy.py --port 8788
```

hub 的 CodeArts 配置填 `http://127.0.0.1:7866`，Key 用上面的值。

## 4. 验证

```bash
# hub 健康
curl -s http://127.0.0.1:8788/health | python -m json.tool

# 模型目录（把 KEY 换成看板里的 Key）
curl -s http://127.0.0.1:8788/v1/models?realm=codearts \
  -H "Authorization: Bearer $KEY" | python -m json.tool

# 一条非流式对话
curl -s http://127.0.0.1:8788/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"GLM-5.2","messages":[{"role":"user","content":"你好"}]}'

# codearts2api 自己的健康
curl -s http://127.0.0.1:7866/healthz
```

测试套件：

```bash
cd apps/hub && python tests/run_all.py
cd apps/codearts && go test ./...
```

## 5. 回滚

CodeArts 出口默认关闭，回滚不会动 WorkBuddy 的数据。

1. 看板「设置 → CodeArts 接入」关闭「启用 CodeArts 出口」。
2. 把所有出口绑定为 `CodeArts` 的 API Key 改回 `国际版` / `国内版` / 留空。
3. 如果要回退到不支持 CodeArts 的旧版本：先完成第 2 步，再替换代码。旧版会把未知的 `codearts` 出口读成「跟随默认出口」，如果没解绑，流量会落到 WorkBuddy。
4. `data/hub/usage/usage.jsonl` 里已经写入的 `realm=codearts` 行会保留；旧看板把它们归入未知出口，不影响 WorkBuddy 统计。

## 6. 排障

| 现象 | 检查 |
|---|---|
| 看板 CodeArts 卡片显示离线 | `docker compose ps`；`curl http://127.0.0.1:7866/healthz`；hub 的 Base URL 是否用容器名 `codearts` |
| `CodeArts 拒绝了当前 API Key` | hub 设置里的 Key 与 `deploy/.env` 的 `CA2A_API_KEY` 不一致；改完重启 codearts 容器 |
| `model ... is not available` | 该账号没注册这个模型；在 codearts2api WebUI 或 `/v1/models` 里核对 |
| 删除账号返回 409 | codearts2api 版本旧，没有 `/admin/api/accounts/delete`；升级 `apps/codearts` 后重建容器 |
| OAuth 一直 pending | 浏览器没完成华为云授权；或远端回调没粘贴。先试「中转回调」 |
| 添加账号后列表不刷新 | 看板每 2.5s 轮询；确认 codearts2api 的 `auths/` 目录可写 |
| WorkBuddy 请求变慢 | 检查 `/health` 的 `codearts` 子字段；CodeArts 调用有独立超时，不应占用 WorkBuddy 账号槽位 |
