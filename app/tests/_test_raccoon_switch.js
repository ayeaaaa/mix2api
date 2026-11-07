/* 小浣熊（Raccoon）第四出口：卡片、视图按钮、默认出口循环与区块显隐。
 *
 * 结构断言钉住入口和 id；行为断言把整段脚本放进假 DOM 跑一次 updateUI，
 * 确认 raccoon 视图下 WorkBuddy / CodeArts 区块隐藏、Raccoon 区块出现，
 * 未配置时卡片隐藏。
 *
 * Run with Node: node tests/_test_raccoon_switch.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

/* ---- 1. 结构 ---- */
assert.ok(/id="tabRaccoon"[^>]*onclick="switchViewRealm\('raccoon'\)"/.test(html),
  '网关页缺少小浣熊出口卡片');
assert.ok(/id="accountsRealmRaccoon"[^>]*onclick="switchViewRealm\('raccoon'\)"/.test(html),
  '账号页缺少小浣熊视图按钮');
assert.ok(html.includes('id="tabRaccoonViewBadge"'), '缺少小浣熊当前视图徽章');
assert.ok(html.includes('id="tabRaccoonDefaultBadge"'), '缺少小浣熊默认出口徽章');
assert.ok(html.includes('id="tabRaccoonKeyBadge"'), '缺少小浣熊 Key 计数');
assert.ok(html.includes('id="tabRaccoonHealthy"'), '缺少小浣熊健康账号数');
assert.ok(html.includes('id="tabRaccoonTotal"'), '缺少小浣熊账号总数');
assert.ok(html.includes('id="tabRaccoonModels"'), '缺少小浣熊模型数');
assert.ok(html.includes('id="raccoonSummary"'), '缺少小浣熊统计条');
assert.ok(html.includes('id="raccoonCards"'), '缺少小浣熊统计卡片容器');
assert.ok(html.includes('id="raccoonAccountsSection"'), '缺少小浣熊账号区');
assert.ok(html.includes('id="raccoonAccounts"'), '缺少小浣熊账号表容器');
assert.ok(html.includes('id="raccoonAcctCount"'), '缺少小浣熊账号计数');
assert.ok(html.includes('realm-badge-raccoon'), '缺少小浣熊徽章样式');
assert.ok(/openRaccoonAuth\(\)/.test(html), '小浣熊账号区缺少添加账号入口');
assert.ok(/id="nextGatewayRealmRaccoon"[^>]*onclick="switchViewRealm\('raccoon'\)"/.test(html),
  '网关页出口切换缺少小浣熊按钮');

assert.ok(/if\(r === "raccoon"\)\{[\s\S]{0,300}?loadRaccoonOverview\(true\)/.test(script),
  'switchViewRealm 应有 raccoon 分支并加载概览');
assert.ok(/const isRaccoonView = \(window\.VIEW_REALM === "raccoon"\)/.test(script),
  'updateUI 应识别 raccoon 视图');
assert.ok(/raccoonEnabled = \(window\.REALM_OPTIONS \|\| \[\]\)\.includes\("raccoon"\)/.test(script),
  'updateUI 应读取服务端 REALM_OPTIONS');
assert.ok(/viewFromUrl === 'raccoon' && \(window\.REALM_OPTIONS \|\| \[\]\)\.includes\('raccoon'\)/.test(script),
  '?view=raccoon 只在已启用时被接受');
assert.ok(script.includes('/raccoon/models/refresh'), '刷新模型应调用 /raccoon/models/refresh');
assert.ok(/meta\.source === 'raccoon'/.test(script), '模型表来源横幅应识别 raccoon');
assert.ok(/function realmLabel[\s\S]{0,400}raccoon/.test(script),
  'realmLabel 应识别 raccoon');

/* ---- 2. 行为 ---- */
dom.installDom({
  confirm: () => true,
  fetch: (url, opts) => {
    const payload = {current: 'intl', options: ['intl', 'cn', 'codearts', 'raccoon'],
                     accounts: [], data: [], models: [], rows: [], total: 0, slots: []};
    return Promise.resolve({
      status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload)),
    });
  },
});

const realLog = console.log;
console.log = () => {};
let api;
try {
  api = new Function(script + `
    window.RACCOON_OVERVIEW = {online: true, stats: {total: 2, healthy: 1},
                                models: [{id: 'raccoon-model'}], accounts: [],
                                base_url: 'https://xiaohuanxiong.com'};
    window.REALM_OPTIONS = ['intl', 'cn', 'codearts', 'raccoon'];
    return {updateUI: updateUI, setViewRealm: setViewRealm,
            switchViewRealm: switchViewRealm, realmLabel: realmLabel,
            renderConsoleAccountHealth: renderConsoleAccountHealth};
  `)();
} catch (e) {
  console.log = realLog;
  console.error('脚本求值失败:', e.message);
  process.exit(1);
}
console.log = realLog;

const byId = id => document.getElementById(id);

api.setViewRealm('raccoon');
window.API_KEY_ROWS = [{enabled: true, realm: 'raccoon'}];
api.updateUI();
assert.notEqual(byId('tabRaccoon').style.display, 'none',
  '已配置时小浣熊卡片应显示');
assert.equal(byId('tabRaccoonViewBadge').style.display, 'inline-block',
  '小浣熊视图应显示当前视图徽章');
assert.equal(byId('raccoonAccountsSection').style.display, '',
  '小浣熊视图应显示小浣熊账号区');
assert.equal(byId('raccoonSummary').style.display, '',
  '小浣熊视图应显示小浣熊统计条');
assert.equal(byId('workbuddyAccountsSection').style.display, 'none',
  '小浣熊视图应隐藏 WorkBuddy 账号区');
assert.equal(byId('workbuddyDisabledSection').style.display, 'none',
  '小浣熊视图应隐藏 WorkBuddy 禁用区');
assert.equal(byId('workbuddyActivitySection').style.display, 'none',
  '小浣熊视图应隐藏 WorkBuddy 活动区');
assert.equal(byId('codeartsAccountsSection').style.display, 'none',
  '小浣熊视图应隐藏 CodeArts 账号区');
assert.equal(byId('codeartsSummary').style.display, 'none',
  '小浣熊视图应隐藏 CodeArts 统计条');
assert.equal(byId('cards').style.display, 'none',
  '小浣熊视图应隐藏 WorkBuddy 统计卡');
assert.equal(byId('schedulerBar').style.display, 'none',
  '小浣熊视图应隐藏 WorkBuddy 调度器');
assert.equal(byId('tabRaccoonHealthy').textContent, 1,
  '小浣熊卡片应显示健康账号数');
assert.equal(byId('tabRaccoonTotal').textContent, 2,
  '小浣熊卡片应显示账号总数');
assert.equal(byId('tabRaccoonModels').textContent, 1,
  '小浣熊卡片应显示模型数');
assert.equal(byId('tabRaccoonKeyBadge').textContent, '1 个 Key',
  '小浣熊卡片应显示绑定的 Key 数');
assert.equal(byId('nextGatewayRealmRaccoon').hidden, false,
  '已配置时网关页出口切换应显示小浣熊按钮');
assert.ok(byId('nextGatewayRealmRaccoon').classList.contains('active'),
  '小浣熊视图应把网关页切换按钮标为 active');

// 概览健康度不能把 WorkBuddy 的国际版/国内版分组带进小浣熊视图。
api.renderConsoleAccountHealth([
  {realm: 'intl', enabled: true, credits: {remain: 1}},
  {realm: 'cn', enabled: false},
]);
const healthHtml = byId('consoleAccountHealth').innerHTML;
assert.ok(healthHtml.includes('小浣熊'), '小浣熊视图健康度应显示小浣熊分组');
assert.ok(/1 \/ 2/.test(healthHtml), '小浣熊健康度应使用 RACCOON_OVERVIEW.stats');
assert.ok(!healthHtml.includes('国际版'), '小浣熊视图健康度不应出现国际版分组');
assert.ok(!healthHtml.includes('国内版'), '小浣熊视图健康度不应出现国内版分组');
assert.equal(byId('consoleHealthSummary').textContent, '1 / 2 可用',
  '小浣熊视图健康度汇总应只统计小浣熊账号');

api.setViewRealm('intl');
api.updateUI();
assert.equal(byId('raccoonAccountsSection').style.display, 'none',
  'WorkBuddy 视图应隐藏小浣熊账号区');
assert.equal(byId('raccoonSummary').style.display, 'none',
  'WorkBuddy 视图应隐藏小浣熊统计条');
assert.equal(byId('workbuddyAccountsSection').style.display, '',
  'WorkBuddy 视图应显示 WorkBuddy 账号区');

// 默认出口显示与循环：codearts -> raccoon -> intl。
window.ACTIVE_GATEWAY_REALM = 'codearts';
window.REALM_OPTIONS = ['intl', 'cn', 'codearts', 'raccoon'];
api.updateUI();
assert.ok(byId('btnToggleRealm').textContent.includes('小浣熊'),
  'CodeArts 默认出口的下一个应是 raccoon');
window.ACTIVE_GATEWAY_REALM = 'raccoon';
api.updateUI();
assert.equal(byId('activeGatewayName').textContent, '小浣熊 (Raccoon)',
  '默认出口为小浣熊时应显示网关名称');
assert.equal(byId('activeEndpoint').textContent, 'https://xiaohuanxiong.com',
  '默认出口为小浣熊时应显示 Base URL');
assert.notEqual(byId('tabRaccoonDefaultBadge').style.display, 'none',
  '默认出口为小浣熊时应显示默认出口徽章');
assert.ok(byId('btnToggleRealm').textContent.includes('国际版'),
  'raccoon 默认出口的下一个应是 intl');

assert.ok(!byId('nextGatewayRealmRaccoon').classList.contains('active'),
  '非小浣熊视图不应把网关页切换按钮标为 active');

window.REALM_OPTIONS = ['intl', 'cn'];
window.VIEW_REALM = 'intl';
api.updateUI();
assert.equal(byId('tabRaccoon').style.display, 'none',
  '未配置时小浣熊卡片应隐藏（需先在设置中启用）');
assert.equal(byId('accountsRealmRaccoon').style.display, 'none',
  '未配置时小浣熊视图按钮应隐藏');
assert.equal(byId('nextGatewayRealmRaccoon').hidden, true,
  '未配置时网关页出口切换应隐藏小浣熊按钮');
assert.ok(/启用后网关页才会出现小浣熊卡片/.test(html),
  '设置页应提示启用后才会出现小浣熊入口');

// 未启用时即便 URL 强塞 view=raccoon，也不能露出账号区/统计条；视图应先
// 回落到当前默认出口，WorkBuddy 区块随之恢复（不能只剩空壳）。
window.VIEW_REALM = 'raccoon';
api.updateUI();
assert.equal(window.VIEW_REALM, 'intl',
  '小浣熊停用后无效视图应回落到默认出口');
assert.equal(byId('raccoonAccountsSection').style.display, 'none',
  '未启用时即使视图是 raccoon 也应隐藏账号区');
assert.equal(byId('raccoonSummary').style.display, 'none',
  '未启用时即使视图是 raccoon 也应隐藏统计条');
assert.equal(byId('tabRaccoon').style.display, 'none',
  '未启用时即使视图是 raccoon 也应隐藏卡片');
assert.equal(byId('workbuddyAccountsSection').style.display, '',
  '小浣熊停用后 WorkBuddy 账号区应恢复');
assert.equal(byId('workbuddyDisabledSection').style.display, '',
  '小浣熊停用后 WorkBuddy 禁用区应恢复');
assert.equal(byId('workbuddyActivitySection').style.display, '',
  '小浣熊停用后 WorkBuddy 活动区应恢复');
assert.equal(byId('cards').style.display, '',
  '小浣熊停用后 WorkBuddy 统计卡应恢复');
assert.equal(byId('schedulerBar').style.display, '',
  '小浣熊停用后 WorkBuddy 调度器应恢复');

assert.ok(api.realmLabel('raccoon').includes('小浣熊'), '小浣熊出口名称');
console.log('raccoon switch assertions passed');
