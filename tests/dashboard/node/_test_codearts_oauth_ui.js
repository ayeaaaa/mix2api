/* CodeArts 添加账号弹窗的授权链接契约。
 *
 * 需求：打开弹窗只生成并展示授权链接，不自动打开浏览器窗口；用户自己复制
 * 或点击「打开授权页」。中转回调后的 next_url 也只能展示，不能自动弹窗。
 *
 * 国际站：弹窗打开时把 realm 捕获成不可变状态（CODEARTS_OAUTH_REALM），
 * 之后的 start/poll/callback 与会话 JSON 导入都只认这个捕获值，不读会随
 * 视图切换而变的 codeartsRealm()。行级「重新登录」传入行所属 realm 时，
 * 弹窗必须按该 realm 发起。
 */
'use strict';
const assert = require('assert');
const vm = require('node:vm');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');

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

/* 会话 JSON 导入的静态契约：字段、处理函数、动态标题/帮助，以及 realm
 * 捕获。具体端点选择由下面的行为断言验证，不再用拼接后的字面量。 */
assert.ok(html.includes('id="codeartsSessionJson"'), 'missing session JSON import field');
assert.ok(html.includes('id="codeartsOauthTitle"'), 'missing dynamic OAuth title');
assert.ok(html.includes('id="codeartsOauthHelp"'), 'missing dynamic OAuth help');
assert.ok(html.includes('导入 session JSON'), 'missing session JSON import button');
assert.ok(script.includes('function importCodeartsSessionJson'), 'missing session JSON import handler');
assert.ok(script.includes('CODEARTS_OAUTH_REALM'), 'OAuth modal does not capture its realm');
assert.ok(script.includes('function openCodeartsOauthModal(realm)'),
  'OAuth modal cannot be opened for an explicit realm');

const settle = () => new Promise(resolve => setImmediate(resolve));

async function runBehavior() {
  const posted = [];
  const bodies = [];
  const logs = [];
  const toasts = [];
  const originalConsole = {};
  for(const method of Object.keys(console)){
    if(typeof console[method] !== 'function') continue;
    originalConsole[method] = console[method];
    console[method] = (...args) => { logs.push(args.map(String).join(' ')); };
  }

  try {
    const page = loadDashboard({
      fetch: (url, opts) => {
        const target = String(url);
        if (opts && opts.method === 'POST') {
          posted.push(target);
          bodies.push({url: target, body: opts.body});
        }
        let payload = {};
        if (target.endsWith('/accounts/oauth/start')) {
          payload = {session_id: 'sid:' + target, auth_url: 'https://example.test/auth'};
        } else if (target.endsWith('/accounts/oauth/poll')) {
          payload = {status: 'pending'};
        } else if (target.endsWith('/accounts/oauth/callback')) {
          payload = {message: '回调已接收'};
        } else if (target.endsWith('/accounts/import')) {
          payload = {ok: true, uid: 'intl-uid-1'};
        } else if (target.indexOf('/realm') === 0) {
          payload = {current: 'codearts', options: ['codearts', 'codearts-intl']};
        } else if (target.indexOf('/codearts-intl/overview') === 0) {
          payload = {service: 'codearts-intl', region: 'intl', stats: {}, accounts: [], models: []};
        } else if (target.indexOf('/codearts/overview') === 0) {
          payload = {service: 'codearts', region: 'cn', stats: {}, accounts: [], models: []};
        }
        return Promise.resolve({
          status: 200, ok: true,
          json: () => Promise.resolve(payload),
          text: () => Promise.resolve(JSON.stringify(payload)),
        });
      },
    });
    page.toast = (...args) => toasts.push(args.map(String).join(' '));

    const capturedRealm = () => vm.runInContext('CODEARTS_OAUTH_REALM', page);

    /* 国内视图打开：捕获 codearts，标题与帮助走国内文案。 */
    page.REALM_OPTIONS = ['codearts', 'codearts-intl'];
    page.VIEW_REALM = 'codearts';
    page.openCodeartsOauthModal();
    await settle();
    assert.equal(capturedRealm(), 'codearts', 'mainland modal must capture codearts');
    assert.equal(dom.byId('codeartsOauthTitle').textContent, '添加 CodeArts 账号');
    const mainlandHelp = dom.byId('codeartsOauthHelp').textContent;
    assert.ok(mainlandHelp.includes('华为云'), 'mainland help should describe the Huawei Cloud login');
    assert.ok(posted.includes('/codearts/accounts/oauth/start'),
      'mainland modal must start on the mainland endpoint: ' + posted.join(','));

    /* 弹窗还开着时切到国际视图：poll 与 callback 仍必须留在国内端点。 */
    page.VIEW_REALM = 'codearts-intl';
    await page.pollCodeartsOAuth(false);
    assert.ok(posted.includes('/codearts/accounts/oauth/poll'),
      'poll must stay on the captured mainland endpoint: ' + posted.join(','));
    dom.byId('codeartsOauthCallback').value = 'http://127.0.0.1/oauth/callback?code=x';
    await page.importCodeartsCallback();
    assert.ok(posted.includes('/codearts/accounts/oauth/callback'),
      'callback must stay on the captured mainland endpoint: ' + posted.join(','));
    assert.ok(!posted.some(u => u.indexOf('/codearts-intl/accounts/oauth/') === 0),
      'an open mainland modal must never touch international OAuth endpoints: ' + posted.join(','));

    page.closeCodeartsOauthModal();

    /* 国际视图打开：捕获 codearts-intl，标题与帮助走国际文案。 */
    page.VIEW_REALM = 'codearts-intl';
    page.openCodeartsOauthModal();
    await settle();
    assert.equal(capturedRealm(), 'codearts-intl', 'intl modal must capture codearts-intl');
    assert.equal(dom.byId('codeartsOauthTitle').textContent, '添加 CodeArts 国际账号');
    const intlHelp = dom.byId('codeartsOauthHelp').textContent;
    assert.ok(intlHelp.includes('国际'), 'intl help should identify the international station');
    assert.notEqual(intlHelp, mainlandHelp, 'help copy must change with the realm');
    assert.ok(posted.includes('/codearts-intl/accounts/oauth/start'),
      'intl modal must start on the international endpoint: ' + posted.join(','));

    /* 打开后再切回国内视图：导入仍按捕获的 codearts-intl 走。 */
    page.VIEW_REALM = 'codearts';
    const session = {
      account: {id: 'intl-9', label: 'Intl Nine'},
      accessKey: 'AK-SECRET-MARKER',
      secretKey: 'SK-SECRET-MARKER',
      securityToken: 'ST-SECRET-MARKER',
      refresh_token: 'RT-SECRET-MARKER',
    };
    // A numeric UID above Number.MAX_SAFE_INTEGER must survive validation and
    // serialization; parsing into a JS object before posting rounds this value.
    const rawSession = JSON.stringify(session).replace('"intl-9"', '9007199254740993');
    dom.byId('codeartsSessionJson').value = rawSession;
    await page.importCodeartsSessionJson(dom.makeElement('button'));
    const importCalls = bodies.filter(c => c.url.endsWith('/accounts/import'));
    assert.equal(importCalls.length, 1, 'exactly one session import request: ' + posted.join(','));
    assert.equal(importCalls[0].url, '/codearts-intl/accounts/import',
      'session import must use the captured international endpoint');
    const sent = JSON.parse(importCalls[0].body);
    assert.equal(sent.session_json, rawSession,
      'session_json must preserve the original JSON, including the exact numeric UID');
    assert.ok(sent.session_json.includes('9007199254740993'), 'large numeric UID was rounded');
    assert.equal(dom.byId('codeartsSessionJson').value, '',
      'the textarea must be cleared after a successful import');
    const status = dom.byId('codeartsOauthStatus').textContent;
    assert.ok(status.includes('intl-uid-1'), 'import status should include the returned UID: ' + status);
    assert.ok(status.indexOf('AK-SECRET-MARKER') === -1
      && status.indexOf('SK-SECRET-MARKER') === -1
      && status.indexOf('RT-SECRET-MARKER') === -1,
      'raw session credentials must never be echoed into the status');
    assert.ok(logs.join('\n').indexOf('SECRET-MARKER') === -1,
      'raw session credentials must never be logged');

    /* 解析失败：只提示，不发请求，也不回显原始 JSON。 */
    const beforeBad = posted.length;
    for(const malformed of ['SK-SECRET-MARKER', '{"secretKey":"SK-SECRET-MARKER",oops}', '{"account":']){
      dom.byId('codeartsSessionJson').value = malformed;
      await page.importCodeartsSessionJson(dom.makeElement('button'));
      assert.equal(posted.length, beforeBad, 'a malformed session JSON must not be posted');
      const parseStatus = dom.byId('codeartsOauthStatus').textContent;
      assert.ok(/解析失败|JSON/.test(parseStatus), 'malformed JSON must report a validation error');
      assert.ok(!parseStatus.includes('SECRET-MARKER'), 'parser errors must not echo credentials');
      assert.ok(!toasts.join('\n').includes('SECRET-MARKER'), 'parser errors must not toast credentials');
      assert.ok(!logs.join('\n').includes('SECRET-MARKER'), 'parser errors must not log credentials');
    }

    /* 行级「重新登录」显式传入 realm 时，弹窗按该 realm 发起。 */
    page.VIEW_REALM = 'codearts';
    const beforeRelogin = posted.length;
    page.codeartsRelogin('intl-9', 'codearts-intl');
    await settle();
    assert.equal(capturedRealm(), 'codearts-intl',
      'relogin must honour the row realm even when the view is mainland');
    assert.ok(posted.slice(beforeRelogin).includes('/codearts-intl/accounts/oauth/start'),
      'relogin must start on the row realm endpoint: ' + posted.slice(beforeRelogin).join(','));

    page.closeCodeartsOauthModal();
  } finally {
    Object.assign(console, originalConsole);
  }
}

runBehavior().then(() => {
  console.log('codearts oauth UI assertions passed');
}).catch(error => {
  console.error(error);
  process.exit(1);
});
