/* 豆包 Work 第五出口的看板契约。
 *
 * 这里钉住三件事：出口启用后 tab/账号区/设置区都能出现；账号页的视图切换
 * 与网关卡片共用 VIEW_REALM；保存设置走的是 doubao 嵌套段，而不是把字段
 * 摊平到根设置里。
 *
 * Run with Node: node tests/dashboard/node/_test_doubao_provider.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

for(const id of ['tabDoubao', 'accountsRealmDoubao', 'doubaoAccountsSection',
                 'doubaoSummary', 'setDoubaoEnabled', 'setDoubaoBaseUrl',
                 'setDoubaoState', 'doubaoCookiesJson']){
  assert.ok(html.includes('id="' + id + '"'), 'dashboard 缺少 #' + id);
}
assert.ok(html.includes("switchViewRealm('doubao')"), '豆包 tab 没有接到 switchViewRealm');
assert.ok(html.includes('/doubao/overview'), '账号区没有请求 /doubao/overview');
assert.ok(script.includes('const doubaoEnabled'), 'updateUI 没有读取 doubao realm option');
assert.ok(script.includes('function renderDoubaoAccounts'), '缺少豆包账号渲染函数');
assert.ok(script.includes('function saveDoubaoSettings'), '缺少豆包设置保存函数');

const posts = [];
const overview = {
  provider: 'doubao', online: true, base_url: 'https://www.doubao.com',
  stats: {total: 1, healthy: 1, disabled: 0, cooling: 0},
  models: [{id: 'doubao-fast'}, {id: 'deepseek-v4.1-flash'}],
  accounts: [{uid: 'db_1', nickname: 'smoke', enabled: true,
              active_concurrent: 0, max_concurrent: 1}],
};
dom.installDom({
  fetch: (url, opts) => {
    const body = opts && opts.body ? JSON.parse(String(opts.body)) : null;
    if(opts && opts.method === 'POST') posts.push({url: String(url), body: body});
    let payload = {current: 'doubao', options: ['intl', 'cn', 'doubao']};
    if(String(url).indexOf('/doubao/overview') === 0) payload = overview;
    if(String(url).indexOf('/settings/save') === 0){
      payload = {doubao: Object.assign({enabled: true, accounts: 1, healthy: 1}, body && body.doubao)};
    }
    return Promise.resolve({status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload))});
  },
});

const realLog = console.log;
console.log = () => {};
let api;
try {
  api = new Function(script + `
    toast = function(){};
    window.updateUI = updateUI;
    return {
      updateUI: updateUI,
      renderDoubaoSettings: renderDoubaoSettings,
      saveDoubaoSettings: saveDoubaoSettings,
      switchViewRealm: switchViewRealm,
    };`)();
} catch(e) {
  console.log = realLog;
  console.error('脚本求值失败:', e.message);
  process.exit(1);
}
console.log = realLog;

const byId = id => dom.byId(id);
const active = el => el.classList.contains('active');

(async () => {
  window.REALM_OPTIONS = ['intl', 'cn', 'doubao'];
  window.VIEW_REALM = 'intl';
  window.DOUBAO_OVERVIEW = overview;
  api.updateUI();
  assert.equal(byId('tabDoubao').style.display, '', '启用豆包后 tab 应显示');
  assert.equal(byId('accountsRealmDoubao').style.display, '', '账号页应出现豆包按钮');

  window.VIEW_REALM = 'doubao';
  api.updateUI();
  assert.ok(active(byId('tabDoubao')), '豆包 tab 应点亮');
  assert.ok(active(byId('accountsRealmDoubao')), '账号页豆包按钮应点亮');
  assert.equal(byId('doubaoAccountsSection').style.display, '', '豆包账号区应显示');
  assert.equal(byId('doubaoSummary').style.display, '', '豆包统计区应显示');
  assert.equal(byId('workbuddyAccountsSection').style.display, 'none', '豆包视图应隐藏 WorkBuddy 账号');

  api.renderDoubaoSettings({
    enabled: true, base_url: 'https://www.doubao.com', aid: '1044603',
    max_concurrent: 2, timeout_seconds: 45, stream_timeout_seconds: 900,
    models_ttl_seconds: 600, accounts: 1, healthy: 1,
  });
  assert.equal(byId('setDoubaoEnabled').checked, true);
  assert.equal(byId('setDoubaoBaseUrl').value, 'https://www.doubao.com');
  assert.equal(byId('setDoubaoMaxConcurrent').value, 2);
  assert.equal(byId('setDoubaoTimeout').value, 45);
  assert.equal(byId('setDoubaoState').textContent, '(已启用)');

  byId('setDoubaoMaxConcurrent').value = '3';
  byId('setDoubaoTimeout').value = '50';
  await api.saveDoubaoSettings();
  const saved = posts.filter(p => p.url === '/settings/save').pop();
  assert.ok(saved, '保存豆包设置没有 POST /settings/save');
  assert.equal(saved.body.doubao.max_concurrent, 3);
  assert.equal(saved.body.doubao.timeout_seconds, 50);
  assert.equal(saved.body.doubao.enabled, true);
})().catch(err => {
  console.log = realLog;
  console.error(err.stack || err);
  process.exit(1);
});
