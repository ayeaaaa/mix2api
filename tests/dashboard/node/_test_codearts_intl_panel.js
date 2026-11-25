/* CodeArts 国际站（codearts-intl）面板契约。
 *
 * 国际站是并列的顶层区域（独立的网关卡片 + 账号页签），不是国内 CodeArts
 * 视图里的子开关；只有 REALM_OPTIONS 里出现 codearts-intl（后端启用时才
 * 上报）时才露出。配额单位跟着站点走：国际站按 token 计，卡片与汇总都得
 * 写 Token（空态 / 加载中 / 失败也一样），不能沿用「积分」。
 *
 * 行为契约（评审补齐）：
 *   - 切换站点瞬间清掉旧站点的行（缓存有则立即重画，没有则加载态），
 *     不许旧行挂着新站点的端点等人点；
 *   - 行级账号操作把行所属站点绑进 handler，端点不由可变视图状态决定；
 *   - 国内站的概览请求一律不带 ?force=1（保持服务端缓存行为不变）；
 *   - 同一站点并发两次概览请求，只有最新一次能写 / 清缓存；
 *   - 两张卡片的 Key 计数按各自 realm 分开。
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
                 'tabCodeartsIntlHealthy', 'tabCodeartsIntlCredits',
                 'tabCodeartsIntlTotal', 'tabCodeartsIntlModels',
                 'tabCodeartsIntlKeyBadge', 'tabCodeartsIntlDefaultBadge',
                 'tabCodeartsIntlViewBadge', 'tabCodeartsIntlOffline',
                 'tabCodeartsIntlHost']){
  assert.ok(html.includes('id="' + id + '"'), 'dashboard missing #' + id);
}
assert.ok(html.includes("switchViewRealm('codearts-intl')"), 'international tab is not wired');
assert.ok(script.includes('function codeartsEndpoint'), 'missing codeartsEndpoint');
assert.ok(script.includes("/codearts-intl/overview"), 'missing international overview request');
assert.ok(script.includes('CODEARTS_OVERVIEWS'), 'missing per-realm overview map');

const intlOverview = {
  service: 'codearts-intl', region: 'intl', online: true,
  stats: {total: 1, healthy: 1, cooling: 0, disabled: 0, credits: 4958830},
  models: [{id: 'glm-5.3'}],
  accounts: [{uid: 'intl-1', nickname: 'Intl', enabled: true,
              credits: {remain: 4958830, used: 41170, unit: 'token', package: '体验版'},
              active_concurrent: 0, max_concurrent: 1}],
};
const mainlandOverview = {
  service: 'codearts', region: 'cn', online: true,
  stats: {total: 1, healthy: 0, cooling: 1, disabled: 0, credits: 12},
  models: [{id: 'glm-4.6'}],
  accounts: [{uid: 'cn-1', nickname: 'Mainland', enabled: true,
              credits: {remain: 12, used: 3, unit: 'credit'}, tasks_enabled: true}],
};

const calls = [];
const POSTS = [];
let fetchMode = 'resolve';   // 'resolve'：立即应答；'pending'：概览请求挂起待手动放行
const pending = [];
const respond = payload => ({status: 200, ok: true,
  json: () => Promise.resolve(payload),
  text: () => Promise.resolve(JSON.stringify(payload))});

const page = loadDashboard({
  fetch: (url, opts) => {
    const target = String(url);
    calls.push(target);
    if(opts && opts.method === 'POST') POSTS.push(target);
    let payload = {};
    if(target.indexOf('/codearts-intl/overview') === 0) payload = intlOverview;
    else if(target.indexOf('/codearts/overview') === 0) payload = mainlandOverview;
    else if(target.indexOf('/realm') === 0) payload = {current: 'codearts-intl', options: ['intl', 'cn', 'codearts', 'codearts-intl']};
    if(fetchMode === 'pending' && /\/codearts(-intl)?\/overview/.test(target)){
      return new Promise(resolve => pending.push(resolve));
    }
    return Promise.resolve(respond(payload));
  },
});
const flushPending = (i, payload) => pending[i](respond(payload));

page.REALM_OPTIONS = ['intl', 'cn', 'codearts', 'codearts-intl'];
page.VIEW_REALM = 'codearts-intl';
page.CODEARTS_OVERVIEWS = {'codearts-intl': intlOverview};
page.updateUI();
assert.equal(dom.byId('tabCodeartsIntl').style.display, '', 'intl tab hidden');
assert.equal(dom.byId('accountsRealmCodeartsIntl').style.display, '', 'intl account tab hidden');
assert.equal(dom.byId('codeartsAccountsSection').style.display, '', 'shared account section hidden');
assert.ok(/Token/.test(dom.byId('tabCodeartsIntlCredits').textContent || ''));

/* 两张卡片的 Key 计数按 realm 分开。 */
page.API_KEY_ROWS = [
  {id: 'k1', enabled: true, realm: 'codearts'},
  {id: 'k2', enabled: true, realm: 'codearts-intl'},
];
page.updateUI();
assert.equal(dom.byId('tabCodeartsKeyBadge').textContent, '1 个 Key', 'mainland key badge should count only codearts keys');
assert.equal(dom.byId('tabCodeartsIntlKeyBadge').textContent, '1 个 Key', 'intl key badge should count only codearts-intl keys');
page.API_KEY_ROWS = [];

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

  /* 切回国内站：概览请求不带 ?force=1（保持服务端缓存行为），表格恢复
   * 国内站的列与签到操作。 */
  calls.length = 0;
  await page.switchViewRealm('codearts');
  assert.equal(page.VIEW_REALM, 'codearts', 'switch should land on the mainland view');
  assert.ok(calls.includes('/codearts/overview'),
    'mainland switch should load the mainland overview: ' + calls.join(','));
  assert.ok(!calls.includes('/codearts/overview?force=1'),
    'mainland refresh must not send ?force=1 (cache behavior is unchanged)');
  assert.equal((page.CODEARTS_OVERVIEWS['codearts'] || {}).service, 'codearts',
    'mainland overview should be cached per realm');
  const mainlandHtml = dom.byId('codeartsAccounts').innerHTML;
  assert.ok(mainlandHtml.indexOf('Mainland') >= 0, 'mainland account row should render');
  assert.ok(/<th>积分<\/th>/.test(mainlandHtml), 'mainland quota column should stay 积分');
  assert.ok(mainlandHtml.includes('签到'), 'mainland table keeps the check-in column');

  /* 切换瞬间清掉旧站点的行：概览请求挂起时，国内行立刻消失、表格进入
   * 加载态，而不是等国际站概览回来才换。 */
  delete page.CODEARTS_OVERVIEWS['codearts-intl'];
  fetchMode = 'pending';
  pending.length = 0;
  const sw = page.switchViewRealm('codearts-intl');
  const staleHtml = dom.byId('codeartsAccounts').innerHTML;
  assert.ok(!staleHtml.includes('Mainland'),
    'stale mainland rows must be cleared the moment the view switches');
  assert.ok(staleHtml.includes('正在加载'),
    'a loading state must replace the stale rows while the overview is in flight');
  assert.equal(pending.length, 1, 'the intl overview request should be in flight');
  flushPending(0, intlOverview);
  await sw;
  assert.ok(dom.byId('codeartsAccounts').innerHTML.includes('Intl'),
    'intl rows should render once the overview resolves');
  fetchMode = 'resolve';

  /* 行级账号操作按行所属站点选端点，不由当前视图决定。 */
  POSTS.length = 0;
  await page.codeartsAccountAction('disable', 'cn-1', null, 'codearts');
  assert.ok(POSTS.some(u => u === '/codearts/accounts/disable'),
    'a mainland row action must post to the mainland endpoint: ' + POSTS.join(','));
  await page.codeartsAccountAction('disable', 'intl-1', null, 'codearts-intl');
  assert.ok(POSTS.some(u => u === '/codearts-intl/accounts/disable'),
    'an intl row action must post to the intl endpoint: ' + POSTS.join(','));
  const disableTargets = POSTS.filter(u => u.endsWith('/accounts/disable'));
  assert.equal(disableTargets.length, 2,
    'exactly one POST per row action: ' + disableTargets.join(','));

  /* 并发两次同一站点的概览请求（后台预载不带 force、用户切换带 force，
   * 两条不同 URL 各自出网）：新请求先返回并提交，旧响应晚回来不得覆盖。 */
  fetchMode = 'pending';
  pending.length = 0;
  const staleOverview = {service: 'codearts-intl', region: 'intl', online: true,
                         stats: {total: 9, healthy: 9, cooling: 0, disabled: 0, credits: 1},
                         models: [], accounts: []};
  const oldReq = page.loadCodeartsOverviewFor('codearts-intl', false);
  const newReq = page.loadCodeartsOverviewFor('codearts-intl', true);
  assert.equal(pending.length, 2, 'both overview requests should be in flight');
  assert.ok(calls.includes('/codearts-intl/overview'),
    'the background prefetch should be the non-forced URL');
  assert.ok(calls.includes('/codearts-intl/overview?force=1'),
    'the user-triggered switch should be the forced URL');
  flushPending(1, intlOverview);        // 新请求先返回并提交
  await newReq;
  assert.equal((page.CODEARTS_OVERVIEWS['codearts-intl'] || {}).stats.total, 1,
    'the newer response should be committed');
  flushPending(0, staleOverview);       // 旧请求后返回：不得覆盖
  await oldReq;
  assert.equal((page.CODEARTS_OVERVIEWS['codearts-intl'] || {}).stats.total, 1,
    'a stale response must not overwrite the newer cache entry');
  fetchMode = 'resolve';

  /* 空的国际概览（零账号）也按 Token 显示；单位判断不依赖账号存在。 */
  page.CODEARTS_OVERVIEWS['codearts-intl'] =
    {service: 'codearts-intl', region: 'intl', online: true, stats: {}, accounts: []};
  page.updateUI();
  assert.ok(/Token/.test(dom.byId('tabCodeartsIntlCredits').textContent || ''),
    'an empty intl overview must still read Token');
  assert.equal(page.codeartsQuotaLabel({service: 'codearts-intl', accounts: []}), 'Token',
    'codeartsQuotaLabel should infer tokens from the intl service marker');
  assert.equal(page.codeartsQuotaLabel({stats: {}, accounts: []}), '积分',
    'codeartsQuotaLabel should keep 积分 for mainland-shaped overviews');

  console.log('codearts intl panel assertions passed');
})().catch(error => { console.error(error); process.exit(1); });
