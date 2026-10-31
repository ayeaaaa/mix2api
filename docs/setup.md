# 部署与运维

## 1. Docker Compose（推荐）

```bash
cd deploy
docker compose up -d --build
```

只有一个容器：

| 服务 | 端口 | 说明 |
|---|---|---|
| `mix2api` | `58675` | 统一入口、看板、三个出口（含 CodeArts 原生客户端） |

数据目录（compose 挂载）：

```text
data/accounts/            # WorkBuddy 账号、面板密码、API Key、CodeArts 配置
data/accounts/codearts/   # CodeArts 账号凭据 codearts-{uid}.json + state.json
data/usage/               # 请求与用量流水
```

### 从两服务版本迁移

如果之前跑过「hub + codearts2api」两服务版本：

```bash
mv data/hub/accounts/* data/accounts/ 2>/dev/null || true
mv data/hub/usage/* data/usage/ 2>/dev/null || true
mkdir -p data/accounts/codearts
cp codearts2api/auths/codearts-*.json data/accounts/codearts/ 2>/dev/null || true
```

CodeArts 账号文件字段与 Go 版兼容；`state.json` 里的停用/冷却状态不会自动迁移，
需要的话在新看板里重新启用/清冷却。

## 2. 首次配置

1. 打开 `http://<host>:58675/`，用默认面板密码 `admin` 登录，立刻在「设置」里改掉。
2. 进「设置 → CodeArts 接入」：
   - 勾选「启用 CodeArts 出口」；
   - 单账号并发默认 5，按需要调整；
   - 保存。不需要填 Base URL 或 API Key——CodeArts 由本进程直接连华为云。
3. 顶部出口卡片出现第三张 `CodeArts`；点它切到 CodeArts 视图。
4. 点「+ 添加账号 (OAuth)」，浏览器打开华为云授权页。授权完成后：
   - 浏览器和网关在同一台机器：回调 `127.0.0.1:<端口>/oauth/callback` 自动完成，弹窗轮询到 `done`；
   - 网关在远端：浏览器会跳到打不开的 `127.0.0.1` 回调。把地址栏完整地址粘贴到弹窗的「远端回调」输入框，点「中转回调」；如果返回 `continue`，弹窗会自动打开下一个华为云页面，继续轮询。
5. 在「设置 → API Key 与出口绑定」里新建 Key：
   - 出口选 `CodeArts`：这个 Key 只走 CodeArts；
   - 出口留空：跟随顶部「网关默认出口」；
   - 模型限制可留空，或填 CodeArts 的模型 ID（支持 `*`）。

## 3. 本地开发

```bash
cd app
python3 wb_proxy.py --port 58675
```

没有第二个进程。CodeArts 账号文件在 `app/accounts/codearts/`。

## 4. 验证

```bash
# 健康
curl -s http://127.0.0.1:58675/health | python -m json.tool

# 模型目录（把 KEY 换成看板里的 Key）
curl -s http://127.0.0.1:58675/v1/models?realm=codearts \
  -H "Authorization: Bearer $KEY" | python -m json.tool

# 一条非流式对话
curl -s http://127.0.0.1:58675/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"GLM-5.2","messages":[{"role":"user","content":"你好"}]}'
```

测试套件：

```bash
cd app && python3 tests/run_all.py
```

当前基线：243 个套件全过（181 Python + 62 JS）。CodeArts 链路的测试用
`tests/_fake_codearts.py` 起本机假华为云上游，不依赖真实账号；真实账号的 OAuth
与对话需要部署后按第 2 节手动走一遍。

## 5. 回滚

CodeArts 出口默认关闭，回滚不会动 WorkBuddy 的数据。

1. 看板「设置 → CodeArts 接入」关闭「启用 CodeArts 出口」。
2. 把所有出口绑定为 `CodeArts` 的 API Key 改回 `国际版` / `国内版` / 留空。
3. 如果要回退到不支持 CodeArts 的旧版本：先完成第 2 步，再替换代码。旧版会把未知的 `codearts` 出口读成「跟随默认出口」，如果没解绑，流量会落到 WorkBuddy。
4. `data/usage/usage.jsonl` 里已经写入的 `realm=codearts` 行会保留；旧看板把它们归入未知出口，不影响 WorkBuddy 统计。

## 6. 排障

| 现象 | 检查 |
|---|---|
| CodeArts 卡片显示离线 | 「设置 → CodeArts 接入」是否启用；`/health` 的 `codearts.ok` |
| 账号列表为空 | 先 OAuth 添加账号；`data/accounts/codearts/` 是否有 `codearts-*.json` |
| OAuth 一直 pending | 浏览器没完成华为云授权；远端回调没粘贴。先试「中转回调」 |
| `refresh_token 已失效` | 账号需要重新登录：停用/删除旧账号，再走一次 OAuth |
| `model ... is not available` | 该账号没注册这个模型；在 CodeArts 视图点「刷新」重新发现模型 |
| 聊天 502 / 连不上华为云 | 检查网关出网、DNS、代理；`/health` 的 `codearts.ok` |
| WorkBuddy 请求变慢 | CodeArts 有独立超时和账号池，不占 WorkBuddy 账号槽位；检查 `/health` 的 `codearts` 子字段 |
