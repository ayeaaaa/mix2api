/* Raccoon 每日活跃的刷新登录契约。
 *
 * 上游在登录会话刷新后异步记 daily_grant；面板不再发“hi”对话，也不消耗
 * 积分。这里钉住按钮/文案和每日活跃单元格的三种状态。
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');

const html = dashboardHtml();
const script = dashboardScript();
const page = loadDashboard();

assert.ok(html.includes('每日活跃（刷新登录）'), '设置页应说明刷新登录');
assert.ok(html.includes('刷新登录</button>'), '账号行应提供刷新登录按钮');
assert.ok(!script.includes('发一次最小对话触发当日活跃'),
  '每日活跃仍保留发对话的旧文案');
assert.ok(script.includes("one.granted ? 'ok'"), '手动刷新结果应按 granted 判断颜色');

const today = (() => {
  const d = new Date();
  return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' +
    String(d.getDate()).padStart(2, '0');
})();

const granted = page.raccoonDailyCell({
  daily_active: {date: today, ok: true, granted: true, points: 300, refreshed: true, attempts: 1},
});
assert.ok(granted.includes('已到账 300'), granted);

const refreshed = page.raccoonDailyCell({
  daily_active: {date: today, ok: true, granted: false, points: 0, refreshed: true, attempts: 1},
});
assert.ok(refreshed.includes('已刷新 · 未到账') && refreshed.includes('已刷新 1 次'), refreshed);

const idle = page.raccoonDailyCell({daily_active: {}});
assert.ok(idle.includes('今日未刷新'), idle);

console.log('raccoon daily refresh assertions passed');
