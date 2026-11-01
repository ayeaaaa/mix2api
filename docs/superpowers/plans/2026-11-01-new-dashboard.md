# mix2api 新面板实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不改动后端 API 的前提下，新增 `/next` 可切换新面板，功能对齐后替换旧 `/`。

**Architecture:** 复用 `app/dashboard.html` 的页面 DOM、元素 ID 与业务 JS；新增 `data-ui="next"` 作用域的新壳、导航、CSS 和少量同步 JS。`/` 与 `/next` 返回同一份 HTML，首帧脚本按路径切换壳；旧 UI 保持原样直到验收完成。

**Tech Stack:** 原生 HTML/CSS/JS；Python 标准库 HTTP 服务；现有 Node DOM stub 测试；Playwright/Firefox 移动端检查。

**Spec:** `docs/superpowers/specs/2026-11-01-new-dashboard.md`

## Global Constraints

- 不新增前端构建步骤、不引入框架或 CDN 资源。
- 所有新样式限定在 `html[data-ui="next"]` 下。
- 不改后端 API 形状；不新增账号/密钥文件。
- 默认端口、单进程、单面板密码保持不变。
- 旧 `/` 在新面板验收前必须保持可用。
- 现有 JS 测试与 `app/tests/run_all.py` 必须继续通过。

## Review Focus

- 旧页面的内联样式和旧 CSS 可能压过新样式：重点检查侧栏、顶部栏、卡片、表格在浅色/深色下的实际渲染。
- `switchMainTab()` 的页签清单、`MAIN_TABS`、首帧 `TABS` 三处必须同步，否则刷新后会落回错误页面。
- 账号默认合并模式会把账号区块搬进网关页：新壳必须强制独立，且不能与旧设置互相覆盖。
- 手机底部导航与「更多」抽屉不能遮住内容、不能出现双滚动条。
- 旧测试依赖的元素 ID 和函数名不能删除或重命名（除计划中明确的 `pageGateway` → `pageOverview`）。

---

### Task 1: 路由、首帧标记与新壳骨架

**Files:**
- Modify: `app/wb_proxy.py` (`do_GET` 路由)
- Modify: `app/dashboard.html` (head 首帧脚本、body 壳、body 尾部同步脚本)
- Test: `app/tests/_test_next_shell.js` (新建)

**Interfaces:**
- Produces: `window.__NEXT_UI__: boolean`；`html[data-ui="next"]`；`#nextSidebar`、`#nextTopbar`、`#nextContent`、`#nextBottomNav`、`#nextMoreSheet`。
- Produces: `window.nextSyncChrome(tab: string): void`；`window.nextOpenMore(open?: boolean): void`。

- [ ] **Step 1: 写失败测试**

新建 `_test_next_shell.js`，读取 `dashboard.html`，断言包含 `id="nextSidebar"`、`id="nextBottomNav"`、`data-ui="next"` 判定脚本、`nextSyncChrome` 函数；并断言 `wb_proxy.py` 的 `do_GET` 含 `/next` 路由。

- [ ] **Step 2: 运行测试确认失败**

Run: `node app/tests/_test_next_shell.js`
Expected: FAIL，提示缺少 `nextSidebar`。

- [ ] **Step 3: 加路由**

在 `do_GET` 的 `if path in ("/", "/dashboard", "/ui")` 中加入 `"/next", "/next/"`，仍调用 `self._get_dashboard()`。

- [ ] **Step 4: 加首帧标记**

在 `<head>` 顶部脚本中，根据 `location.pathname` 设置 `document.documentElement.dataset.ui = 'next'`，并设置 `window.__NEXT_UI__`。

- [ ] **Step 5: 加壳骨架**

在 `#panelGate` 之后、`<main>` 之前插入 `#nextSidebar`（品牌、8 项导航、底部系统入口）与 `#nextTopbar`（页面标题、出口 pill、`#meta` 挂载点、主题/语言/头像挂载点）；把现有 `<main>` 包进 `#nextContent`；在 `</main>` 后闭合壳。加 `#nextBottomNav` 与 `#nextMoreSheet`。

- [ ] **Step 6: 加同步脚本**

在 body 尾部新增脚本：把旧 `.header-right` 节点移动到 `#nextTopbarActions`；实现 `nextSyncChrome(tab)` 更新标题、侧栏 active、底部导航 active、更多抽屉项；实现 `nextOpenMore()`；包装 `switchMainTab` 调用同步。旧 header 保持隐藏。

- [ ] **Step 7: 运行测试**

Run: `node app/tests/_test_next_shell.js`
Expected: PASS。

- [ ] **Step 8: 提交**

```bash
git add app/wb_proxy.py app/dashboard.html app/tests/_test_next_shell.js
git commit -m "feat(ui): add switchable next dashboard shell"
```

---

### Task 2: 页面拆分与导航映射

**Files:**
- Modify: `app/dashboard.html` (`pageGateway` → `pageOverview`，新建 `pageGateway`，`switchMainTab`、`MAIN_TABS`、首帧 `TABS`、`applyAccountsSeparateTab`)
- Test: `app/tests/_test_next_nav.js` (新建)

**Interfaces:**
- Consumes: Task 1 的 `window.__NEXT_UI__`、`nextSyncChrome`。
- Produces: `switchMainTab('overview')` 可用；`#pageOverview` 含出口卡/汇总/最近请求；`#pageGateway` 含模型清单；`#pageTasks` 含 `#schedulerBar`。

- [ ] **Step 1: 写失败测试**

断言 `dashboard.html` 中存在 `id="pageOverview"`、`id="pageGateway"`、`MAIN_TABS` 含 `'overview'`、`switchMainTab` 的 tabs 列表含 `['overview'`；断言 `applyAccountsSeparateTab` 使用 `pageOverview`。

- [ ] **Step 2: 运行确认失败**

Run: `node app/tests/_test_next_nav.js`
Expected: FAIL。

- [ ] **Step 3: 改名与拆页**

把原 `id="pageGateway"` 改为 `id="pageOverview"`；把「网关模型清单」`<section>` 剪出，放入新的 `<div id="pageGateway" class="main-page">`，加页头「网关模型」；把 `#schedulerBar` 从 `.gw-summary` 移到 `#pageTasks` 顶部并加「后台调度器」标题。

- [ ] **Step 4: 扩展页签清单**

三处加入 `overview`：首帧 `TABS`、`MAIN_TABS`、`switchMainTab` 的 `tabs` 数组；`startDashboard` 默认在新壳用 `overview`。`applyAccountsSeparateTab` 的合并目标改为 `pageOverview`。

- [ ] **Step 5: 强制账号独立**

新壳启动后调用 `applyAccountsSeparateTab(true)`；在 `loadSettings` 后重新应用一次，避免服务端设置把账号区搬回总览。新壳隐藏设置里的「账号独立成一个页签」行。

- [ ] **Step 6: 运行测试**

Run: `node app/tests/_test_next_nav.js && node app/tests/_test_page_visibility.js && node app/tests/_test_page_nav.js`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add app/dashboard.html app/tests/_test_next_nav.js
git commit -m "feat(ui): split overview and gateway pages"
```

---

### Task 3: 新设计 token 与壳样式

**Files:**
- Modify: `app/dashboard.html` (新增 `<style id="nextStyles">`)
- Test: `app/tests/_test_next_styles.js` (新建)

**Interfaces:**
- Produces: `html[data-ui="next"]` 下的 `--bg/--panel/--line/--fg/--dim/--accent` 等 token；`#nextSidebar`、`#nextTopbar`、`#nextBottomNav`、`#nextMoreSheet` 的响应式规则。

- [ ] **Step 1: 写失败测试**

断言样式中存在 `html[data-ui="next"]`、`#nextSidebar`、`#nextBottomNav`、`@media (max-width: 860px)`，且包含 `--accent:#4f46e5`。

- [ ] **Step 2: 运行确认失败**

Run: `node app/tests/_test_next_styles.js`
Expected: FAIL。

- [ ] **Step 3: 写 token 与桌面壳**

在旧样式后追加 `#nextStyles`：新变量覆盖、隐藏旧 header、240px 侧栏、64px 顶栏、内容区最大宽度、8 项导航、active 态、侧栏底部系统入口。

- [ ] **Step 4: 写移动壳**

860px 以下隐藏侧栏，显示底部导航与「更多」抽屉；内容区 padding-bottom 预留 76px；顶栏只保留标题、出口 pill、主题按钮；更多抽屉使用 `position:fixed` + backdrop。

- [ ] **Step 5: 写深色主题**

`html[data-ui="next"][data-theme="dark"]` 覆盖 token；保持强调色可读；不复制整套组件规则。

- [ ] **Step 6: 运行测试**

Run: `node app/tests/_test_next_styles.js && node app/tests/_test_dashboard_theme_contrast.js`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add app/dashboard.html app/tests/_test_next_styles.js
git commit -m "feat(ui): add next dashboard design tokens and shell"
```

---

### Task 4: 通用组件重绘

**Files:**
- Modify: `app/dashboard.html` (`#nextStyles` 内的组件规则)
- Test: `app/tests/_test_next_components.js` (新建)

**Interfaces:**
- Consumes: Task 3 token。
- Produces: 新壳下的 `.card`、`.panel`、`section`、`table`、`button`、`input`、`select`、`.badge`、`.modal`、`.table-wrap` 视觉规则。

- [ ] **Step 1: 写失败测试**

断言样式中包含 `html[data-ui="next"] .card`、`... table`、`... .modal`、`... input`，且新壳下 `section` 的 `border-radius` 为 `14px`。

- [ ] **Step 2: 运行确认失败**

Run: `node app/tests/_test_next_components.js`
Expected: FAIL。

- [ ] **Step 3: 卡片与面板**

统一 14px 圆角、1px 边框、低阴影；卡片内边距 14–16px；标题 13px/700；数值 24px/750；辅助文字 11–12px。

- [ ] **Step 4: 表格**

表头 10–11px、浅灰底；行高 40–44px；数字列 tabular-nums；移动端 `table.data-cards` 保持现有卡片化行为，只改视觉。

- [ ] **Step 5: 表单与按钮**

输入框 36px 高、8px 圆角、聚焦 3px accent-soft；主按钮靛蓝、次按钮白底、危险按钮红字浅红底；统一 disabled 态。

- [ ] **Step 6: 弹窗与提示**

弹窗 16px 圆角、遮罩加深；toast 保持现有定位，只换字体和圆角。

- [ ] **Step 7: 运行测试**

Run: `node app/tests/_test_next_components.js && node app/tests/_test_custom_select.js`
Expected: PASS。

- [ ] **Step 8: 提交**

```bash
git add app/dashboard.html app/tests/_test_next_components.js
git commit -m "feat(ui): restyle next dashboard components"
```

---

### Task 5: 总览 / 网关 / 账号页布局

**Files:**
- Modify: `app/dashboard.html` (页面内联布局与少量新容器)
- Test: `app/tests/_test_next_pages.js` (新建)

**Interfaces:**
- Consumes: Task 2 的页面拆分、Task 4 组件样式。
- Produces: 总览页 4 张出口卡 + 汇总卡 + 最近请求；网关页模型表；账号页 CodeArts/WorkBuddy/禁用/记录四块。

- [ ] **Step 1: 写失败测试**

断言 `#pageOverview` 内含 `realm-cards-grid`、`#cards`、`#recent`；`#pageGateway` 内含 `#modelsTable`；`#pageAccounts` 内含 `#codeartsAccountsSection`、`#workbuddyAccountsSection`、`#workbuddyActivitySection`。

- [ ] **Step 2: 运行确认失败**

Run: `node app/tests/_test_next_pages.js`
Expected: FAIL。

- [ ] **Step 3: 总览页**

把出口卡改成 4 列自适应；汇总卡与调度器状态条改成两栏；最近请求表头/分页统一新组件；页面标题改为「总览」。

- [ ] **Step 4: 网关页**

模型表保留全部列，增加横向滚动容器；表头吸顶；勾选框与模型 ID 对齐；来源横幅与刷新按钮放同一行。

- [ ] **Step 5: 账号页**

CodeArts 与 WorkBuddy 各用独立面板；账号行在桌面保持表格，手机转卡片；操作按钮收进行内，删除用危险色；活动记录筛选条放面板顶部。

- [ ] **Step 6: 运行测试**

Run: `node app/tests/_test_next_pages.js && node app/tests/_test_accounts_page.js && node app/tests/_test_codearts_accounts.js`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add app/dashboard.html app/tests/_test_next_pages.js
git commit -m "feat(ui): lay out overview gateway and accounts"
```

---

### Task 6: 用量 / 自动化 / 智能体 / 日志页

**Files:**
- Modify: `app/dashboard.html`
- Test: `app/tests/_test_next_pages2.js` (新建)

**Interfaces:**
- Consumes: Task 4 组件样式。
- Produces: 四个页面在新壳下可用，现有 ID 不变。

- [ ] **Step 1: 写失败测试**

断言 `#pageAnalytics` 含 `#kpis`、`#seriesCanvas`、`#dimTable`；`#pageTasks` 含 `#schedulerBar`；`#pageAgents` 含 `#agentDetectState`；`#pageLogs` 含日志筛选控件。

- [ ] **Step 2: 运行确认失败**

Run: `node app/tests/_test_next_pages2.js`
Expected: FAIL。

- [ ] **Step 3: 用量页**

只做外壳适配：KPI 卡、图表、折叠区沿用现有结构；图表容器宽度自适应；移动端 KPI 两列。

- [ ] **Step 4: 自动化页**

调度器状态条置顶；国际版/国内版任务分两块；按钮组在窄屏换行。

- [ ] **Step 5: 智能体 / 日志页**

智能体卡片统一新卡片样式；日志终端保留深色，筛选条与导出按钮放同一行；移动端终端高度自适应。

- [ ] **Step 6: 运行测试**

Run: `node app/tests/_test_next_pages2.js && node app/tests/_test_agent_ui.js && node app/tests/_test_console_tail.js`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add app/dashboard.html app/tests/_test_next_pages2.js
git commit -m "feat(ui): adapt analytics automation agents and logs"
```

---

### Task 7: 设置页分组

**Files:**
- Modify: `app/dashboard.html`
- Test: `app/tests/_test_next_settings.js` (新建)

**Interfaces:**
- Consumes: 现有 `pageSettings` 的 section 结构与 `initPageNav`。
- Produces: 设置页顶部 6 组子页签（基础 / API 与出口 / 模型与定价 / 自动化 / 稳定性 / 日志与系统），一次只显示一组 section；不删除任何 section。

- [ ] **Step 1: 写失败测试**

断言 `#pageSettings` 含 `data-settings-group` 容器，且 `NEXT_SETTINGS_GROUPS` 提供 6 组；断言原有 `#pageSettings` 关键 section 标题仍存在。

- [ ] **Step 2: 运行确认失败**

Run: `node app/tests/_test_next_settings.js`
Expected: FAIL。

- [ ] **Step 3: 加分组条**

在设置页头部加横向分组条；用 JS 按 section 标题前缀归类；点击只显示当前组的 section，并更新标题/说明与 URL `group` 参数。分组条在手机横向滚动。

- [ ] **Step 4: 隐藏冲突项**

新壳隐藏「账号独立成一个页签」「顶部页签显示」中与新导航冲突的说明，但保留服务端字段，避免旧壳读到坏数据。

- [ ] **Step 5: 运行测试**

Run: `node app/tests/_test_next_settings.js && node app/tests/_test_settings_load.js && node app/tests/_test_settings_update_ui.js`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add app/dashboard.html app/tests/_test_next_settings.js
git commit -m "feat(ui): group next settings sections"
```

---

### Task 8: 手机端与全量验收

**Files:**
- Modify: `app/dashboard.html`
- Modify: `app/tests/_mobile_check.py` (新增 `/next` 检查)
- Modify: `app/tests/run_all.py` (确保新 JS 测试被收集，通常无需改)
- Test: `app/tests/_test_next_mobile.js` (新建)

**Interfaces:**
- Consumes: 全部前序任务。
- Produces: `/next` 桌面/手机验收通过；`/` 仍可用。

- [ ] **Step 1: 写失败测试**

断言新壳在 390px 宽度下：侧栏隐藏、底部导航显示、`#nextContent` 无横向溢出、表格容器可滚动。

- [ ] **Step 2: 运行确认失败**

Run: `node app/tests/_test_next_mobile.js`
Expected: FAIL。

- [ ] **Step 3: 手机适配**

修底部导航遮挡、更多抽屉滚动锁定、表格卡片化、顶栏换行、弹窗宽度。

- [ ] **Step 4: 扩展移动检查**

在 `_mobile_check.py` 增加 `next` 检查：访问 `/next`，断言侧栏/底栏/内容宽度，截图到 `/tmp/mobile-shots`。

- [ ] **Step 5: 全量测试**

Run: `python3 app/tests/run_all.py`
Expected: 全部通过（允许既有环境跳过项）。

- [ ] **Step 6: 手工验收**

桌面与手机各看一遍：总览、网关、账号、用量、自动化、智能体、设置、日志；浅色/深色；刷新后停留页正确。

- [ ] **Step 7: 提交**

```bash
git add app/dashboard.html app/tests/_mobile_check.py app/tests/_test_next_mobile.js
git commit -m "feat(ui): finish next dashboard responsive pass"
```

---

### Task 9: 切换 `/` 并移除旧壳

**Files:**
- Modify: `app/wb_proxy.py` (`/` 默认新壳)
- Modify: `app/dashboard.html` (首帧默认 `next`，旧 header/nav 删除或隐藏)
- Modify: `app/tests/_dashboard_source.js` (如脚本拆分则更新)
- Modify: `README.md`, `app/README.md`, `docs/releases/v1.0.10.md`
- Test: 全量测试

**Interfaces:**
- Consumes: Task 8 验收结果。
- Produces: `/` 即新面板；`/next` 可保留为重定向或同一页。

- [ ] **Step 1: 切换默认**

首帧脚本改为：路径 `/` 默认 `data-ui="next"`；`/classic` 可选保留旧壳一个版本，或直接删除旧 header/nav。按用户要求「替换」则默认新壳，旧壳标记删除。

- [ ] **Step 2: 清理旧壳**

删除旧 header 的可见结构、旧 `.main-nav`、旧顶部导航 CSS；保留业务 JS 与页面 DOM。

- [ ] **Step 3: 全量测试**

Run: `python3 app/tests/run_all.py`
Expected: 全部通过。

- [ ] **Step 4: 手工验收**

访问 `/`，确认新面板、登录闸门、刷新、主题、语言、弹窗均正常。

- [ ] **Step 5: 更新文档与版本**

写 `docs/releases/v1.0.10.md`，更新 README 的面板说明与截图描述。

- [ ] **Step 6: 提交**

```bash
git add app/wb_proxy.py app/dashboard.html app/tests README.md app/README.md docs/releases/v1.0.10.md
git commit -m "feat(ui): make next dashboard the default"
```
