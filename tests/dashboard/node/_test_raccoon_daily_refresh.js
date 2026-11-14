/* Raccoon 每日活跃的 Web 触发契约。
 *
 * 上游在 Web 端设置请求后异步记 daily_grant；面板不再发“hi”对话，也不消耗
 * 积分。这里钉住按钮/文案和每日活跃单元格的四种状态。
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');

const html = dashboardHtml();
const script = dashboardScript();
const page = loadDashboard();

assert.ok(html.includes('每日活跃（Web 触发）'), '设置页应说明 Web 触发');
assert.ok(html.includes('触发每日活跃</button>'), '账号行应提供触发每日活跃按钮');
assert.ok(!script.includes('发一次最小对话触发当日活跃'),
  '每日活跃仍保留发对话的旧文案');
assert.ok(script.includes('触发每日活跃失败: '), '手动触发失败应有独立文案');
assert.ok(script.includes("one.granted ? 'ok'"), '手动触发结果应按 granted 判断颜色');

const today = (() => {
  const d = new Date();
  return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' +
    String(d.getDate()).padStart(2, '0');
})();

const granted = page.raccoonDailyCell({
  daily_active: {date: today, ok: true, granted: true, points: 300, refreshed: false, attempts: 1},
});
assert.ok(granted.includes('已到账 300'), granted);

const triggered = page.raccoonDailyCell({
  daily_active: {date: today, ok: true, granted: false, points: 0, refreshed: false, attempts: 1},
});
assert.ok(triggered.includes('已触发 · 未到账') && triggered.includes('已触发 1 次'), triggered);

const failed = page.raccoonDailyCell({
  daily_active: {date: today, ok: false, granted: false, points: 0, refreshed: false, attempts: 1},
});
assert.ok(failed.includes('触发失败') && failed.includes('已尝试 1 次'), failed);

const idle = page.raccoonDailyCell({daily_active: {}});
assert.ok(idle.includes('今日未触发'), idle);

console.log('raccoon daily web-trigger assertions passed');
