/* CodeArts 添加账号弹窗的授权链接契约。
 *
 * 需求：打开弹窗只生成并展示授权链接，不自动打开浏览器窗口；用户自己复制
 * 或点击「打开授权页」。中转回调后的 next_url 也只能展示，不能自动弹窗。
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');

const html = dashboardHtml();
const script = dashboardScript();

assert.ok(html.includes('onclick="copyCodeartsAuthUrl(this)"'),
  'CodeArts 授权弹窗缺少「复制授权链接」按钮');
assert.ok(html.includes('id="codeartsOauthModal"'),
  'CodeArts 授权弹窗不存在');
assert.ok(script.includes('function copyCodeartsAuthUrl'),
  '缺少 copyCodeartsAuthUrl');
assert.ok(script.includes('writeClipboard(url)'),
  '复制授权链接没有使用统一剪贴板实现');
assert.ok(!script.includes('window.open(r.auth_url'),
  'startCodeartsOAuth 仍然自动打开授权页');
assert.ok(!script.includes('window.open(r.next_url'),
  'importCodeartsCallback 仍然自动打开中转地址');
assert.ok(script.includes('function openCodeartsAuthUrl'),
  '用户主动打开授权页的按钮不应被删除');

console.log('codearts oauth UI assertions passed');
