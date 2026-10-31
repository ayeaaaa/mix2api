/* CodeArts OAuth 弹窗与设置区块。
 *
 * Run with Node: node tests/_test_codearts_oauth.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

assert.ok(html.includes('id="codeartsOauthModal"'), '缺少 OAuth 弹窗');
assert.ok(html.includes('id="codeartsOauthUrl"'), '缺少授权链接展示');
assert.ok(html.includes('id="codeartsOauthCallback"'), '缺少远端回调输入');
assert.ok(html.includes('id="codeartsOauthStatus"'), '缺少授权状态行');
assert.ok(html.includes('/codearts/accounts/oauth/start'), '缺少 OAuth start 调用');
assert.ok(html.includes('/codearts/accounts/oauth/poll'), '缺少 OAuth poll 调用');
assert.ok(html.includes('/codearts/accounts/oauth/callback'), '缺少 OAuth callback 调用');
assert.ok(/r\.next_url/.test(script), 'OAuth 中转必须处理 next_url');
assert.ok(/pollCodeartsOAuth\(true\)/.test(script), 'continue 后应继续轮询');

assert.ok(html.includes('id="setCodeartsEnabled"'), '缺少启用开关');
assert.ok(html.includes('id="setCodeartsBaseUrl"'), '缺少 Base URL');
assert.ok(html.includes('id="setCodeartsApiKey"'), '缺少 API Key 输入');
assert.ok(html.includes('id="setCodeartsTimeout"'), '缺少管理超时');
assert.ok(html.includes('id="setCodeartsStreamTimeout"'), '缺少流式超时');
assert.ok(html.includes('id="setCodeartsModelsTtl"'), '缺少模型缓存 TTL');
assert.ok(html.includes('id="setCodeartsHint"'), '缺少 Key 状态提示');
assert.ok(/api_key_set/.test(script), '设置页应显示 Key 是否已设置，而不是明文');
assert.ok(/saveCodeartsSettings/.test(script), '缺少保存函数');
assert.ok(/testCodeartsConnection/.test(script), '缺少测试连接函数');

dom.installDom({confirm: () => true});
const realLog = console.log;
console.log = () => {};
let api;
try {
  api = new Function(script + `
    return {renderCodeartsSettings: renderCodeartsSettings};
  `)();
} catch (e) {
  console.log = realLog;
  console.error('脚本求值失败:', e.message);
  process.exit(1);
}
console.log = realLog;

api.renderCodeartsSettings({
  enabled: true, base_url: 'http://codearts:7866', api_key_set: true,
  configured: true, timeout_seconds: 30, stream_timeout_seconds: 600,
  models_ttl_seconds: 60,
});
assert.equal(document.getElementById('setCodeartsEnabled').checked, true,
  '启用开关应回填');
assert.equal(document.getElementById('setCodeartsBaseUrl').value,
  'http://codearts:7866', 'Base URL 应回填');
assert.equal(document.getElementById('setCodeartsApiKey').value, '',
  'API Key 输入框不应回填明文');
assert.ok(document.getElementById('setCodeartsHint').textContent.includes('已设置'),
  '应提示 Key 已设置');
assert.equal(document.getElementById('setCodeartsState').textContent, '(已启用)',
  '应显示已启用');

console.log('codearts oauth/settings assertions passed');
