/* 小浣熊（Raccoon）添加账号弹窗：auth.json 导入 + 浏览器回环授权。
 *
 * 两种入口都必须在弹窗里完成：粘贴 auth.json 原文导入；或者走本机 loopback
 * 授权（开始 / 轮询 / 手工粘贴完整回调地址）。弹窗里不能出现 token 输入框，
 * 也不能把 token 回显到页面上。
 *
 * Run with Node: node tests/_test_raccoon_oauth.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

/* ---- 1. 结构 ---- */
for(const id of ['raccoonAuthModal', 'raccoonAuthInput', 'raccoonAuthUrl',
                 'raccoonAuthStatus', 'raccoonAuthCallback']){
  assert.ok(html.includes('id="' + id + '"'), '缺少 #' + id);
}
for(const endpoint of ['/raccoon/accounts/import',
                       '/raccoon/accounts/oauth/start',
                       '/raccoon/accounts/oauth/poll',
                       '/raccoon/accounts/oauth/callback']){
  assert.ok(script.includes(endpoint), '缺少 ' + endpoint + ' 调用');
}
assert.ok(/pollRaccoonOauth\(true\)/.test(script), '轮询应调用 pollRaccoonOauth(true)');
assert.ok(/setInterval\(function\(\)\{ pollRaccoonOauth\(true\); \}, 1500\)/.test(script),
  '浏览器授权应每 1500ms 轮询一次');
assert.ok(/(300000|300\s*\*\s*1000)/.test(script), '轮询应有 300 秒上限');

const modalStart = html.indexOf('id="raccoonAuthModal"');
const modalEnd = html.indexOf('id="loginModal"', modalStart);
assert.ok(modalStart > 0 && modalEnd > modalStart, '找不到小浣熊弹窗边界');
const modalHtml = html.slice(modalStart, modalEnd);
assert.ok(modalHtml.includes('loopback') || modalHtml.includes('同一台机器'),
  '弹窗必须说明浏览器与网关共享 loopback');
assert.ok(modalHtml.includes('auth.json'), '弹窗必须说明远程用户导入 auth.json');
assert.ok(modalHtml.includes('扫码') && modalHtml.includes('密码登录')
  && modalHtml.includes('v1.6.0'),
  '弹窗必须说明 v1.6.0 不支持扫码 / 密码登录');
assert.ok(!/access_token|refresh_token/.test(modalHtml),
  '弹窗不能出现 token 字段或 token 文案');
assert.ok(!/type="password"/.test(modalHtml), '弹窗不能出现密码输入框');

/* ---- 2. 行为 ---- */
let POSTS = [];
let GETS = [];
let oauthReply = {status: 'pending', ok: false, message: '等待浏览器完成授权…'};
let intervalId = 100;
const intervals = [];
const cleared = [];
const AUTH_URL = 'https://xiaohuanxiong.com/authorize?state=abc';

dom.installDom({
  fetch: (url, opts) => {
    const target = String(url);
    const method = (opts && opts.method) || 'GET';
    let body = null;
    if(opts && opts.body){
      try { body = JSON.parse(String(opts.body)); } catch(e) { body = null; }
    }
    if(method === 'POST') POSTS.push({url: target, body});
    else GETS.push(target);
    let payload = {};
    if(target === '/raccoon/accounts/import'){
      payload = {ok: true, uid: 'u1', enabled: true, message: '账号已导入'};
    } else if(target === '/raccoon/accounts/oauth/start'){
      payload = {ok: true, session_id: 'sess-1', auth_url: AUTH_URL,
                 redirect: 'http://127.0.0.1:8787/raccoon/oauth/callback',
                 expires_in: 300};
    } else if(target === '/raccoon/accounts/oauth/poll'){
      payload = oauthReply;
    } else if(target === '/raccoon/accounts/oauth/callback'){
      payload = {ok: true, message: '回调已接收'};
    } else if(target.indexOf('/raccoon/overview') === 0){
      payload = {online: true, stats: {total: 1, healthy: 1},
                 accounts: [], models: [{id: 'm1'}]};
    } else if(target === '/accounts'){
      payload = {accounts: [], data: []};
    } else if(target === '/realm'){
      payload = {current: 'raccoon', options: ['intl', 'cn', 'codearts', 'raccoon']};
    }
    return Promise.resolve({
      status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload)),
    });
  },
});

global.setInterval = (fn, ms) => {
  const id = intervalId++;
  intervals.push({id, fn, ms});
  return id;
};
window.setInterval = global.setInterval;
global.clearInterval = id => { cleared.push(id); };
window.clearInterval = global.clearInterval;

const realLog = console.log;
console.log = () => {};
const toasts = [];
global.__toasts = toasts;
let api;
try {
  api = new Function(script + `
    toast = function(msg, kind){ global.__toasts.push([msg, kind]); };
    window.updateUI = function(){};
    return {
      openRaccoonAuth: openRaccoonAuth,
      closeRaccoonAuth: closeRaccoonAuth,
      importRaccoonAuth: importRaccoonAuth,
      startRaccoonOauth: startRaccoonOauth,
      pollRaccoonOauth: pollRaccoonOauth,
      submitRaccoonCallback: submitRaccoonCallback,
      getSession: function(){ return RACCOON_OAUTH_SESSION; },
      setDeadline: function(v){ RACCOON_OAUTH_DEADLINE = v; },
    };
  `)();
} catch (e) {
  console.log = realLog;
  console.error('脚本求值失败:', e.message);
  process.exit(1);
}
console.log = realLog;

const byId = id => document.getElementById(id);
const isOpen = () => byId('raccoonAuthModal').classList.contains('show');

(async () => {
  // 2a. 打开 / 关闭：关闭必须清掉轮询定时器与会话。
  api.openRaccoonAuth();
  assert.ok(isOpen(), '点击添加账号应打开小浣熊弹窗');
  assert.equal(byId('raccoonAuthUrl').textContent, '', '打开时应清空旧授权链接');
  api.closeRaccoonAuth();
  assert.ok(!isOpen(), '关闭按钮应隐藏弹窗');

  // 2b. 导入校验：空内容 / 缺 access_token 都不能发请求。
  POSTS = [];
  byId('raccoonAuthInput').value = '';
  await api.importRaccoonAuth(null);
  assert.equal(POSTS.filter(p => p.url.indexOf('/raccoon/accounts/import') >= 0).length,
    0, '空 auth.json 不应发导入请求');
  assert.ok(byId('raccoonAuthStatus').textContent.includes('auth.json'),
    '空内容应提示粘贴 auth.json');

  POSTS = [];
  byId('raccoonAuthInput').value = '{"refresh_token":"only-refresh"}';
  await api.importRaccoonAuth(null);
  assert.equal(POSTS.filter(p => p.url.indexOf('/raccoon/accounts/import') >= 0).length,
    0, '缺 access_token 不应发导入请求');
  assert.ok(byId('raccoonAuthStatus').textContent.includes('access_token'),
    '缺 access_token 应给出可读提示');

  // 2c. 合法 auth.json：POST {auth: {...}}，回读概览 / 账号并关闭。
  POSTS = [];
  GETS = [];
  const auth = {access_token: 'tok-import-1', refresh_token: 'ref-import-1',
                office_org_name: 'Raccoon Test'};
  byId('raccoonAuthInput').value = JSON.stringify(auth);
  await api.importRaccoonAuth(null);
  const imported = POSTS.find(p => p.url.indexOf('/raccoon/accounts/import') >= 0);
  assert.ok(imported, '合法 auth.json 应 POST /raccoon/accounts/import');
  assert.deepEqual(imported.body, {auth: auth}, '导入载荷必须包在 {auth: {...}} 里');
  assert.ok(GETS.some(u => u.indexOf('/raccoon/overview?force=1') === 0),
    '导入后应刷新小浣熊概览');
  assert.ok(GETS.some(u => u.indexOf('/accounts') === 0), '导入后应刷新账号列表');
  assert.ok(!isOpen(), '导入成功后应关闭弹窗');
  assert.ok(toasts.some(t => t[1] === 'ok'), '导入成功应给出成功提示');

  // 2d. 浏览器授权：start 渲染链接并启动 1500ms 轮询。
  api.openRaccoonAuth();
  POSTS = [];
  GETS = [];
  intervals.length = 0;
  cleared.length = 0;
  await api.startRaccoonOauth(null);
  assert.equal(POSTS[0].url, '/raccoon/accounts/oauth/start',
    '开始授权应 POST /raccoon/accounts/oauth/start');
  assert.equal(byId('raccoonAuthUrl').textContent, AUTH_URL,
    '应显示服务端返回的授权链接');
  assert.equal(intervals.length, 1, '应启动一个轮询定时器');
  assert.equal(intervals[0].ms, 1500, '轮询间隔应为 1500ms');
  assert.equal(api.getSession(), 'sess-1', '应保存 OAuth session_id');
  const firstTimer = intervals[0].id;

  // 定时器本身必须真的触发轮询。
  POSTS = [];
  intervals[0].fn();
  await new Promise(resolve => setImmediate(resolve));
  assert.ok(POSTS.some(p => p.url === '/raccoon/accounts/oauth/poll'),
    '1500ms 定时器应触发 /raccoon/accounts/oauth/poll');

  // 2e. pending 继续等待，不关闭也不清会话。
  await api.pollRaccoonOauth(true);
  const pendingPoll = POSTS.filter(p => p.url === '/raccoon/accounts/oauth/poll').pop();
  assert.ok(pendingPoll, '轮询应 POST /raccoon/accounts/oauth/poll');
  assert.deepEqual(pendingPoll.body, {session_id: 'sess-1'}, '轮询应带 session_id');
  assert.ok(isOpen(), 'pending 时弹窗应保持打开');

  // 2f. done：停止轮询、回读概览 / 账号、关闭弹窗。
  oauthReply = {status: 'done', ok: true, message: '登录成功'};
  cleared.length = 0;
  GETS = [];
  await api.pollRaccoonOauth(true);
  assert.ok(cleared.includes(firstTimer), 'done 后必须停止轮询');
  assert.ok(GETS.some(u => u.indexOf('/raccoon/overview?force=1') === 0),
    '授权成功后应刷新小浣熊概览');
  assert.ok(GETS.some(u => u.indexOf('/accounts') === 0), '授权成功后应刷新账号列表');
  assert.ok(!isOpen(), '授权成功后应关闭弹窗');
  assert.ok(byId('raccoonAuthStatus').textContent.includes('登录成功'),
    '授权成功后状态行应显示成功');

  // 2g. error：停止轮询、保留弹窗显示原因。
  oauthReply = {status: 'error', ok: false, message: '授权失败：state 已过期'};
  api.openRaccoonAuth();
  await api.startRaccoonOauth(null);
  const errorTimer = intervals[intervals.length - 1].id;
  cleared.length = 0;
  await api.pollRaccoonOauth(true);
  assert.ok(cleared.includes(errorTimer), 'error 后必须停止轮询');
  assert.ok(isOpen(), 'error 时弹窗应保留');
  assert.ok(byId('raccoonAuthStatus').textContent.includes('state 已过期'),
    'error 时应显示服务端原因');

  // 2h. 300 秒上限：超时后不再请求，并停止轮询。
  await api.startRaccoonOauth(null);
  const timeoutTimer = intervals[intervals.length - 1].id;
  POSTS = [];
  cleared.length = 0;
  api.setDeadline(Date.now() - 1);
  await api.pollRaccoonOauth(true);
  assert.equal(POSTS.filter(p => p.url === '/raccoon/accounts/oauth/poll').length, 0,
    '超时后不应继续轮询');
  assert.ok(cleared.includes(timeoutTimer), '超时后必须停止轮询');
  assert.ok(byId('raccoonAuthStatus').textContent.includes('超时'),
    '超时后应显示可读提示');

  // 2i. 手工回调：提交完整回调地址，并继续轮询到 done。
  oauthReply = {status: 'done', ok: true, message: '登录成功'};
  await api.startRaccoonOauth(null);
  const callbackUrl = 'http://127.0.0.1:8787/raccoon/oauth/callback?code=C1&state=S1';
  byId('raccoonAuthCallback').value = callbackUrl;
  POSTS = [];
  await api.submitRaccoonCallback(null);
  const callback = POSTS.find(p => p.url === '/raccoon/accounts/oauth/callback');
  assert.ok(callback, '手工回调应 POST /raccoon/accounts/oauth/callback');
  assert.deepEqual(callback.body, {session_id: 'sess-1', callback_url: callbackUrl},
    '手工回调应提交 session_id 与完整 callback_url');
  assert.ok(POSTS.some(p => p.url === '/raccoon/accounts/oauth/poll'),
    '手工回调后应继续轮询');
  assert.ok(!isOpen(), '手工回调成功后应关闭弹窗');

  // 2j. 关闭清定时器：start 之后直接关闭，不再保留轮询。
  api.openRaccoonAuth();
  await api.startRaccoonOauth(null);
  const closeTimer = intervals[intervals.length - 1].id;
  cleared.length = 0;
  api.closeRaccoonAuth();
  assert.ok(cleared.includes(closeTimer), '关闭弹窗必须清掉轮询定时器');
  assert.equal(api.getSession(), '', '关闭弹窗应清掉 OAuth 会话');

  console.log('raccoon oauth/settings assertions passed');
})().catch(error => {
  console.log = realLog;
  console.error(error && error.stack || error);
  process.exit(1);
});
