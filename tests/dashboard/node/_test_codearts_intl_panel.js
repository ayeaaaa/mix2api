/* CodeArts 国际站（codearts-intl）面板契约。
 *
 * 国际站是并列的顶层区域（独立的网关卡片 + 账号页签），不是国内 CodeArts
 * 视图里的子开关；只有 REALM_OPTIONS 里出现 codearts-intl（后端启用时才
 * 上报）时才露出。配额单位跟着站点走：国际站按 token 计，卡片与汇总都得
 * 写 Token，不能沿用「积分」。
 *
 * Run with Node: node tests/dashboard/node/_test_codearts_intl_panel.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

for(const id of ['tabCodeartsIntl', 'accountsRealmCodeartsIntl',
                 'tabCodeartsIntlHealthy', 'tabCodeartsIntlCredits']){
  assert.ok(html.includes('id="' + id + '"'), 'dashboard missing #' + id);
}
assert.ok(html.includes("switchViewRealm('codearts-intl')"), 'international tab is not wired');
assert.ok(script.includes('function codeartsEndpoint'), 'missing codeartsEndpoint');
assert.ok(script.includes("/codearts-intl/overview"), 'missing international overview request');
assert.ok(script.includes('CODEARTS_OVERVIEWS'), 'missing per-realm overview map');

const calls = [];
const intlOverview = {
  service: 'codearts-intl', region: 'intl', online: true,
  stats: {total: 1, healthy: 1, cooling: 0, disabled: 0, credits: 4958830},
  models: [{id: 'glm-5.3'}],
  accounts: [{uid: 'intl-1', nickname: 'Intl', enabled: true,
              credits: {remain: 4958830, used: 41170, unit: 'token', package: '体验版'},
              active_concurrent: 0, max_concurrent: 1}],
};
const page = loadDashboard({
  fetch: (url, opts) => {
    const target = String(url);
    calls.push(target);
    let payload = {};
    if(target.indexOf('/codearts-intl/overview') === 0) payload = intlOverview;
    if(target.indexOf('/realm') === 0) payload = {current: 'codearts-intl', options: ['intl', 'cn', 'codearts', 'codearts-intl']};
    return Promise.resolve({status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload))});
  },
});
page.REALM_OPTIONS = ['intl', 'cn', 'codearts', 'codearts-intl'];
page.VIEW_REALM = 'codearts-intl';
page.CODEARTS_OVERVIEWS = {'codearts-intl': intlOverview};
page.updateUI();
assert.equal(dom.byId('tabCodeartsIntl').style.display, '', 'intl tab hidden');
assert.equal(dom.byId('accountsRealmCodeartsIntl').style.display, '', 'intl account tab hidden');
assert.equal(dom.byId('codeartsAccountsSection').style.display, '', 'shared account section hidden');
assert.ok(/Token/.test(dom.byId('tabCodeartsIntlCredits').textContent || ''));

/* 行为：切到国际站只打 /codearts-intl 的端点，共用的账号表按站点裁剪
 * （Token 列、无签到列与签到/任务操作），工具栏的签到入口收起来。 */
(async () => {
  calls.length = 0;
  // 已经在国际视图时 switchViewRealm 是幂等 no-op，先退回国际版再切过去。
  page.VIEW_REALM = 'intl';
  await page.switchViewRealm('codearts-intl');
  assert.equal(page.VIEW_REALM, 'codearts-intl', 'switch should land on the intl view');
  assert.ok(calls.some(u => String(u).indexOf('/codearts-intl/overview?force=1') === 0),
    'intl switch must force-load the intl overview: ' + calls.join(','));
  assert.ok(!calls.some(u => String(u).indexOf('/codearts/overview') === 0),
    'intl switch must not touch the mainland overview endpoint');
  assert.equal((page.CODEARTS_OVERVIEWS['codearts-intl'] || {}).service, 'codearts-intl',
    'intl overview should be cached per realm');
  const accountsHtml = dom.byId('codeartsAccounts').innerHTML;
  assert.ok(accountsHtml.indexOf('Intl') >= 0, 'intl account row should render');
  assert.ok(/<th>Token<\/th>/.test(accountsHtml), 'intl quota column should read Token');
  assert.ok(!accountsHtml.includes('签到'), 'intl table must not show check-in cells');
  assert.ok(!accountsHtml.includes('任务'), 'intl table must not show task toggles');
  assert.ok(accountsHtml.includes('重新登录') && accountsHtml.includes('删除'),
    'intl table keeps relogin and delete actions');
  assert.equal(dom.byId('codeartsCheckinAllBtn').style.display, 'none',
    'check-in-all toolbar button must be hidden for intl');
  assert.ok(/Token/.test(dom.byId('codeartsRefreshCreditsBtn').textContent || ''),
    'credits refresh button should be relabelled for tokens');
  assert.ok(/Token/.test(dom.byId('tabCodeartsIntlCredits').textContent || ''),
    'intl card quota should still read Token after reload');

  console.log('codearts intl panel assertions passed');
})().catch(error => { console.error(error); process.exit(1); });
