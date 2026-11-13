/* 「临期/到期积分」统计。
 *
 * 后端把每个账号所有有到期日的套餐包汇总成 a.expiring：
 *   credits / packages / within_window / within_packages / window_days
 * 账号行与积分明细都要显示“全部有到期日”的总额，同时保留窗口内和最近包。
 *
 * Run with Node: node tests/_test_expiring_credits.js
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

/* ---- 1. 结构：两张区域卡片和明细卡片都有落点 -------------------------- */
check('国际版卡片应有到期积分行', /id="tabIntlExpiringLine"/.test(html));
check('国内版卡片应有到期积分行', /id="tabCnExpiringLine"/.test(html));
check('积分明细卡片标题应为到期积分', html.includes('title="全部有到期日的剩余积分；下方列出窗口内与最近到期包">到期积分</div>'));

const jsonResponse = payload => Promise.resolve({
  ok: true, status: 200,
  json: () => Promise.resolve(payload),
  text: () => Promise.resolve(JSON.stringify(payload)),
});

const page = loadDashboard({
  fetch: () => jsonResponse({accounts: [], slots: [], data: [], results: [], rows: [], models: [], total: 0}),
});
page.USAGE_BY_ACCOUNT = {};

/* ---- 2. 账号行：显示全部到期总额，tooltip 带窗口和最近包 -------------- */
const account = {
  uid: 'u-expiry', nickname: '临期测试', realm: 'cn', enabled: true,
  credits: {
    remain: 5128.42, size: 6000,
    earliest_expiring: {name: 'Bonus Pack', remain: 36.42, days_left: 2.5, cycle_end_time: '2026-11-20 10:00:00'},
  },
  expiring: {credits: 4628.42, packages: 71, within_window: 968.42, within_packages: 21, window_days: 7},
};
const row = page.accountRow(account);
check('账号行显示全部到期积分', row.includes('到期积分 4,628'), row);
check('账号行 tooltip 说明全部有到期日', row.includes('全部有到期日：4,628.42 分'), row);
check('账号行 tooltip 说明 7 天内金额', row.includes('其中 7 天内：968.42 分 / 21 个包'), row);
check('账号行 tooltip 保留最近包', row.includes('最近包：36.42 分 / 2.5 天'), row);
check('窗口内占比不高时用警告色，而不是把全部余额标红', row.includes('badge mini warn'), row);

/* ---- 3. 积分明细：主数字是全部到期总额，副行是窗口内 + 最近包 -------- */
const credits = {
  remain: 5128.42, used: 871.58, size: 6000,
  used_percent: '14.5%', remain_percent: '85.5%', is_paid_user: true,
  earliest_expiring: {name: 'Bonus Pack', remain: 36.42, days_left: 2.5, cycle_end_time: '2026-11-20 10:00:00'},
  packages: [],
};
page.renderCreditsDetail(credits, account, account.expiring);
const value = document.getElementById('cmExpiryValue');
const sub = document.getElementById('cmExpirySub');
check('明细主数字是全部到期积分', value.textContent === '4,628.42', value.textContent);
check('明细副行说明全部有到期日', sub.textContent.includes('全部有到期日 · 71 个包'), sub.textContent);
check('明细副行说明窗口内金额', sub.textContent.includes('窗口 7 天内 968.42 分 / 21 个包'), sub.textContent);
check('明细副行保留最近包', sub.textContent.includes('最近 36.42 分 / 2.5 天'), sub.textContent);

/* ---- 4. 刷新返回空统计时必须清掉旧值 -------------------------------- */
check('积分明细刷新用空值覆盖旧 expiring',
  /acct\.expiring = res\.expiring \|\| null;/.test(html));
check('积分明细渲染不再回退到旧 expiring',
  /renderCreditsDetail\(res\.credits, acct, res\.expiring \|\| null\);/.test(html));

console.log('expiring credits suite passed: ' + checks + ' checks');