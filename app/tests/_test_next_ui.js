/* 新面板 /next 的结构与导航契约。
 *
 * 这里不驱动完整浏览器，只钉住「新壳、页面拆分、导航映射、旧壳回退」四件事；
 * 真实布局由 _mobile_check.py 的 next 检查负责。
 *
 * Run with Node: node tests/_test_next_ui.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const {dashboardHtml} = require('./_dashboard_source.js');

const html = dashboardHtml();
const proxy = fs.readFileSync(path.join(__dirname, '..', 'wb_proxy.py'), 'utf8');

/* ---- 1. 路由与首帧标记 ---- */
assert.ok(/path in \("\/", "\/dashboard", "\/ui", "\/next", "\/next\/"\)/.test(proxy),
  'wb_proxy.py 的 do_GET 应把 /next 指到看板');
assert.ok(/p === '\/next'/.test(html) && /data-ui/.test(html),
  '首帧脚本应按 /next 路径给 <html> 打 data-ui 标记');
assert.ok(/window\.__NEXT_UI__ = next/.test(html), '首帧脚本应暴露 window.__NEXT_UI__');

/* ---- 2. 新壳骨架 ---- */
for(const id of ['nextSidebar', 'nextTopbar', 'nextContent', 'nextBottomNav',
                 'nextMoreSheet', 'nextSettingsGroups', 'nextSidebarBackdrop']){
  assert.ok(html.includes('id="' + id + '"'), '新壳缺少 #' + id);
}
for(const fn of ['nextGo', 'nextSyncChrome', 'nextOpenMore', 'nextOpenSidebar',
                 'nextSettingsGroup']){
  assert.ok(html.includes('function ' + fn) || html.includes('window.' + fn + ' ='),
    '新壳缺少函数 ' + fn);
}
const navLabels = ['总览', '网关', '账号', '用量', '自动化', '智能体', '设置', '日志'];
for(const label of navLabels){
  assert.ok(new RegExp('data-next-tab="[^"]+"[^>]*>[\\s\\S]{0,2200}?<span>' + label + '</span>').test(html),
    '侧栏缺少导航项：' + label);
}
assert.strictEqual((html.match(/id="nextBottomNav"/g) || []).length, 1,
  '底部导航只能有一份');

/* ---- 3. 页面拆分 ---- */
assert.ok(/<div id="pageOverview" class="main-page">/.test(html), '缺少 #pageOverview');
assert.ok(/<div id="pageGateway" class="main-page">/.test(html), '缺少新的 #pageGateway');
const overviewAt = html.indexOf('<div id="pageOverview"');
const gatewayAt = html.indexOf('<div id="pageGateway"');
const accountsAt = html.indexOf('<div id="pageAccounts"');
assert.ok(overviewAt > 0 && gatewayAt > overviewAt && accountsAt > gatewayAt,
  '页面顺序应为 overview → gateway → accounts');
const overviewHtml = html.slice(overviewAt, gatewayAt);
const gatewayHtml = html.slice(gatewayAt, accountsAt);
assert.ok(overviewHtml.includes('id="recent"'), '总览页应保留最近请求');
assert.ok(!overviewHtml.includes('id="modelsTable"'), '模型清单不应再留在总览页');
assert.ok(gatewayHtml.includes('id="modelsTable"'), '网关页应包含模型清单');
assert.ok(html.slice(accountsAt, html.indexOf('<div id="pageTasks"')).includes('id="codeartsAccountsSection"'),
  '账号页应包含 CodeArts 账号区块');

/* ---- 4. 导航清单三处同步 ---- */
const headTabs = html.match(/var TABS = \[([^\]]*)\];/);
const mainTabs = html.match(/const MAIN_TABS = \[([^\]]*)\];/);
assert.ok(headTabs && mainTabs, '找不到 TABS / MAIN_TABS');
const names = s => s.split(',').map(x => x.trim().replace(/['"]/g, '')).filter(Boolean);
assert.deepStrictEqual(names(headTabs[1]), names(mainTabs[1]),
  '首帧 TABS 与主脚本 MAIN_TABS 必须一致');
assert.ok(names(mainTabs[1]).includes('overview'), 'MAIN_TABS 应包含 overview');
assert.ok(/\[\s*'overview'\s*,\s*btnOverview\s*,\s*pageOverview\s*\]/.test(html),
  'switchMainTab 的 tabs 清单应包含 overview');
assert.ok(/if\(!page\) return;/.test(html),
  'switchMainTab 即使没有旧导航按钮也必须切换页面');

/* ---- 5. 旧壳回退 ---- */
assert.ok(/if\(!NEXT\)\{[\s\S]{0,500}?oldModels\.parentNode\.removeChild\(oldModels\)/.test(html),
  '旧壳应把模型区块搬回 overview 并移除空的 pageGateway');
assert.ok(/if\(!window\.__NEXT_UI__ && tab === 'gateway'[\s\S]{0,220}?tab = 'overview';/.test(html),
  '旧壳的 gateway 页签应映射到 overview');
assert.ok(/oldGatewayBtn\.classList\.toggle\('active', tab === 'overview'\)/.test(html),
  '旧壳的网关按钮应在 overview 页保持 active');

/* ---- 6. 调度器与设置分组 ---- */
assert.ok(/schedSection\.appendChild\(sched\)/.test(html),
  '新壳应把调度器状态条移进自动化页');
assert.ok(/next-settings-group/.test(html) && /data-settings-group="basic"/.test(html),
  '设置页应有分组条');
for(const key of ['basic', 'api', 'model', 'automation', 'system']){
  assert.ok(html.includes('data-settings-group="' + key + '"'), '设置分组缺少 ' + key);
}

/* ---- 7. 样式边界 ---- */
assert.ok(/html\[data-ui="next"\]/.test(html), '新样式必须限定在 data-ui=next 下');
assert.ok(/#nextSidebar/.test(html) && /#nextBottomNav/.test(html),
  '样式应覆盖新壳侧栏与底部导航');
assert.ok(/@media \(max-width: 860px\)/.test(html), '缺少手机断点');
assert.ok(/--accent:#4f46e5/.test(html), '新壳强调色应为靛蓝');
assert.ok(/body > header\{display:none!important\}/.test(html),
  '新壳应隐藏旧顶栏');

console.log('next dashboard shell assertions passed');
