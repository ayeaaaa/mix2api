/* 「用量」页的区域口径。
 *
 * 页头显示当前版本（国内版 / 国际版 / CodeArts / 小浣熊），但历史实现把
 * analyticsQuery() 写死成 realm=all，于是 KPI/图表拿全部区域、最近请求拿
 * 默认区域，同一个页面出现两套口径。现在页面必须：
 *
 *   1. 默认跟随当前版本（VIEW_REALM）；
 *   2. 提供「全部区域」选项，KPI、时序、模型表和最近请求一起切换；
 *   3. 所有带 realm 的用量请求都显式带参数，不再依赖服务端默认区域。
 *
 * Run with Node: node tests/_test_analytics_realm_scope.js
 */
'use strict';
const assert = require('assert');
const vm = require('node:vm');
const {dashboardHtml} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');

const html = dashboardHtml();

let checks = 0;
function check(label, ok, detail){
  assert.ok(ok, label + (detail === undefined ? '' : '  -> ' + detail));
  checks += 1;
}

/* ---- 1. 结构：区域切换控件在用量页页头 -------------------------------- */
check('用量页应有区域切换控件', /id="analyticsScopeSeg"/.test(html));
check('区域控件应有跟随当前版本选项', /id="analyticsScopeSeg"[\s\S]{0,200}?data-scope="view"/.test(html));
check('区域控件应有全部区域选项', /data-scope="all"/.test(html));

/* ---- 2. 行为：查询串跟随当前版本，全部选项覆盖 ------------------------ */
const dom = require('./_dom_stub.js');
// The scope assertions are synchronous. Keep the panel's background loaders
// pending so they cannot throw on the deliberately empty fixture after the
// assertions have already run.
const page = loadDashboard({
  fetch: () => new Promise(() => {}),
});

page.VIEW_REALM = 'cn';
check('默认跟随国内版', page.analyticsQuery().includes('realm=cn'), page.analyticsQuery());

page.VIEW_REALM = 'intl';
check('跟随国际版', page.analyticsQuery().includes('realm=intl'), page.analyticsQuery());

page.VIEW_REALM = 'codearts';
check('跟随 CodeArts', page.analyticsQuery().includes('realm=codearts'), page.analyticsQuery());

page.VIEW_REALM = 'cn';
page.setAnalyticsScope('all');
check('全部区域选项覆盖当前版本', page.analyticsQuery().includes('realm=all'), page.analyticsQuery());

page.setAnalyticsScope('view');
check('切回当前版本恢复 CN', page.analyticsQuery().includes('realm=cn'), page.analyticsQuery());

/* 页面的点击处理必须能真正切换，而不是只测试 state 函数。 */
const seg = document.getElementById('analyticsScopeSeg');
const allBtn = dom.makeElement('button');
allBtn.setAttribute('data-scope', 'all');
allBtn.closest = () => allBtn;
seg.appendChild(allBtn);
seg.dispatchEvent({type: 'click', target: allBtn});
check('点击全部区域后查询串切到 all', page.analyticsQuery().includes('realm=all'), page.analyticsQuery());

page.setAnalyticsScope('view');

/* ---- 3. 最近请求也必须带显式区域，不能吃服务端默认值 ----------------- */
check('最近请求查询串显式带 CN',
  page.analyticsRecentQuery().includes('realm=cn'), page.analyticsRecentQuery());

page.setAnalyticsScope('all');
check('最近请求在全区域模式下带 all',
  page.analyticsRecentQuery().includes('realm=all'), page.analyticsRecentQuery());

/* ---- 4. 新版用量页的并发与区间一致性 -------------------------------- */
check('区域按钮维护 aria-pressed',
  /setAttribute\('aria-pressed', on \? 'true' : 'false'\)/.test(html));
check('perf 折叠区使用新版 state.range 取数',
  /function perfQuery\(\)\{[\s\S]{0,200}?rangeParams\(state\.range\)/.test(html));
check('区域/区间切换会给静态区、热力图、最近请求各自加代次保护',
  /WBA_STATIC_GEN/.test(html) && /WBA_HEAT_GEN/.test(html) && /WBA_RECENT_GEN/.test(html));
check('切区域会清热力图缓存并刷新静态区',
  /heatFetchedAt = 0;[\s\S]{0,700}?refreshAnalyticsScopeData\(\)/.test(html));
check('轮询去重键包含区域，避免 CN/INTL 互相挡请求',
  /const pendingKey = range \+ '\\|' \+ analyticsRealmValue\(\)/.test(html));

/* ---- 5. 模型矩阵白名单：网关目录的 canonical id ---------------------- */
/* 用量数据仍是全区域口径，但白名单只回答「哪些 id 算已知模型」。网关目录的
 * m.id 带出口前缀（cn/deepseek），与用量聚合的分组键一致；旧的 realm=all
 * 目录只有裸 id，会把已限定的模型全部滤掉。这里把 loadAnalyticsMatrix 的
 * 源码取出来单独跑一遍，确认它真的请求网关目录并使用 canonical id。 */
const matrixFn = (html.match(/async function loadAnalyticsMatrix\(\)\{[\s\S]*?\n\}/) || [''])[0];
check('矩阵白名单来自网关目录', matrixFn.includes("getJSON('/v1/models?realm=gateway')"), matrixFn);
check('矩阵白名单不再请求 realm=all 目录', !matrixFn.includes('/v1/models?realm=all'), matrixFn);
check('矩阵白名单把 m.id 当 canonical 键',
  /MATRIX_MODELS = new Set\(models\.data\.map\(m => m\.id\)\)/.test(matrixFn), matrixFn);

function runMatrixWhitelist(){
  const calls = [];
  const context = {
    console: console,
    POLL_READ: {},
    MATRIX_MODELS: new Set(),
    matrixModelsAt: 0,
    MATRIX_MODELS_TTL_MS: 0,
    cachedMatrixData: null,
    analyticsQuery: () => '?realm=all',
    getJSON: (url) => {
      calls.push(url);
      if(url === '/v1/models?realm=gateway'){
        return Promise.resolve({object: 'list', realm: 'gateway', data: [
          {id: 'cn/deepseek', realm: 'cn'},
          {id: 'codearts/deepseek', realm: 'codearts'},
          {id: 'deepseek', realm: 'intl'},
        ]});
      }
      return Promise.resolve({
        by_model: {}, by_model_realm: {}, by_model_acct: {}, accounts_map: {},
      });
    },
    fillMatrixFilters: () => {},
    renderPerfMatrix: () => {},
    isAuthError: () => false,
  };
  vm.createContext(context);
  return vm.runInContext(matrixFn + '\nloadAnalyticsMatrix()', context)
    .then(() => ({calls: calls, models: Array.from(context.MATRIX_MODELS)}));
}

runMatrixWhitelist().then(result => {
  check('矩阵白名单实际请求网关目录',
    result.calls.includes('/v1/models?realm=gateway'), JSON.stringify(result.calls));
  check('矩阵白名单保留 canonical id',
    result.models.join(',') === 'cn/deepseek,codearts/deepseek,deepseek',
    JSON.stringify(result.models));
  console.log('analytics realm scope suite passed: ' + checks + ' checks');
}).catch(e => { console.error(e); process.exit(1); });