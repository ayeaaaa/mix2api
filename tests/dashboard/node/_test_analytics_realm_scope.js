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

console.log('analytics realm scope suite passed: ' + checks + ' checks');