/* 小浣熊（Raccoon）设置、Key 绑定与标签。
 *
 * 设置区块只暴露启用、管理超时、流式超时、单账号并发；保存走既有
 * /settings/save 的 {raccoon: {...}} 补丁，测试连接走 /raccoon/test。
 * Key 的出口下拉只在 realm 已启用时增加「固定小浣熊出口」，未启用时不改变
 * 原有选项。用量 / 活动 / 模型目录标签都显示「小浣熊」，英文词典有对应词条。
 *
 * Run with Node: node tests/_test_raccoon_settings.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

/* ---- 1. 结构 ---- */
for(const id of ['raccoonEnabled', 'raccoonTimeout', 'raccoonStreamTimeout',
                 'raccoonMaxConcurrent', 'setRaccoonState', 'setRaccoonHint']){
  assert.ok(html.includes('id="' + id + '"'), '缺少 #' + id);
}
assert.ok(/renderRaccoonSettings/.test(script), '缺少设置回填函数');
assert.ok(/saveRaccoonSettings/.test(script), '缺少设置保存函数');
assert.ok(/testRaccoon/.test(script), '缺少测试连接函数');
assert.ok(/postJSON\(['"]\/settings\/save['"],\s*\{raccoon:/.test(script),
  '保存必须 POST {raccoon: {...}} 到 /settings/save');
assert.ok(script.includes('/raccoon/test'), '测试连接必须调用 /raccoon/test');
assert.ok(/renderRaccoonSettings\(data\.raccoon\)/.test(script),
  '设置页应从 data.raccoon 回填');
assert.ok(/固定小浣熊出口/.test(script), 'Key 出口选项应有「固定小浣熊出口」');
assert.ok(/\(window\.REALM_OPTIONS \|\| \[\]\)\.includes\('raccoon'\)/.test(script),
  'Key 出口选项应读取 REALM_OPTIONS');
assert.ok(/ACTIVITY_REALM_LABEL\s*=\s*\{[^}]*raccoon:\s*'小浣熊'/.test(script),
  '活动记录应把小浣熊映射为「小浣熊」');
assert.ok(/function realmLabel[\s\S]{0,400}raccoon/.test(script),
  'realmLabel 应识别 raccoon');
assert.ok(/meta\.source === 'raccoon'/.test(script), '模型目录横幅应识别 raccoon');
assert.ok(html.includes('#pageAnalytics .realm-raccoon'),
  '用量页应有 raccoon 标签样式');

const sectionStart = html.indexOf('id="raccoonSettingsSection"');
const sectionEnd = html.indexOf('<section', sectionStart + 10);
assert.ok(sectionStart > 0 && sectionEnd > sectionStart, '找不到小浣熊设置区块');
const sectionHtml = html.slice(sectionStart, sectionEnd);
assert.ok(!/CodeArts|重置|reset/i.test(sectionHtml),
  '小浣熊设置区块不应出现 CodeArts 专用重置入口');
assert.ok(!/id="raccoonReset/i.test(html), '小浣熊设置不应有重置按钮');

/* ---- 2. 行为 ---- */
let POSTS = [];
let GETS = [];
let testReply = {ok: true, models: 9, message: '连接正常'};
let testStatus = 200;
const toasts = [];

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
    if(target === '/settings/save'){
      payload = {raccoon: {enabled: true, timeout_seconds: 90,
                           stream_timeout_seconds: 1200, max_concurrent: 4,
                           accounts: 2, healthy: 1}};
    } else if(target === '/raccoon/test'){
      payload = testReply;
    } else if(target === '/realm'){
      payload = {current: 'raccoon', options: ['intl', 'cn', 'codearts', 'raccoon']};
    } else if(target === '/accounts'){
      payload = {accounts: [], data: []};
    }
    return Promise.resolve({
      status: target === '/raccoon/test' ? testStatus : 200,
      ok: (target === '/raccoon/test' ? testStatus : 200) < 400,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload)),
    });
  },
});

const realLog = console.log;
console.log = () => {};
global.__toasts = toasts;
let api;
try {
  api = new Function(script + `
    toast = function(msg, kind){ global.__toasts.push([msg, kind]); };
    window.updateUI = function(){ global.__updateUiCalls++; };
    return {
      renderRaccoonSettings: renderRaccoonSettings,
      saveRaccoonSettings: saveRaccoonSettings,
      testRaccoon: testRaccoon,
      realmLabel: realmLabel,
      keyRealmCell: keyRealmCell,
      activityLabels: function(){ return ACTIVITY_REALM_LABEL; },
      renderKeyRows: renderKeyRows,
      setRealmOptions: function(v){ window.REALM_OPTIONS = v; },
      setKeyRows: function(v){ API_KEY_ROWS = v; },
      i18n: window.WB_I18N,
    };
  `)();
} catch (e) {
  console.log = realLog;
  console.error('脚本求值失败:', e.message);
  process.exit(1);
}
global.__updateUiCalls = 0;
console.log = realLog;

const byId = id => document.getElementById(id);

(async () => {
  // 2a. 回填：四个字段 + 状态 + 账号提示。
  api.renderRaccoonSettings({
    enabled: true, timeout_seconds: 45, stream_timeout_seconds: 600,
    max_concurrent: 2, accounts: 2, healthy: 1,
  });
  assert.equal(byId('raccoonEnabled').checked, true, '启用开关应回填');
  assert.equal(byId('raccoonTimeout').value, 45, '管理超时应回填');
  assert.equal(byId('raccoonStreamTimeout').value, 600, '流式超时应回填');
  assert.equal(byId('raccoonMaxConcurrent').value, 2, '单账号并发应回填');
  assert.equal(byId('setRaccoonState').textContent, '(已启用)', '应显示启用状态');
  assert.ok(byId('setRaccoonHint').textContent.includes('2 个账号'),
    '应提示账号数');

  // 2b. 保存：只提交四个字段，走 /settings/save 的 raccoon 补丁。
  POSTS = [];
  GETS = [];
  global.__updateUiCalls = 0;
  byId('raccoonEnabled').checked = true;
  byId('raccoonTimeout').value = '90';
  byId('raccoonStreamTimeout').value = '1200';
  byId('raccoonMaxConcurrent').value = '4';
  await api.saveRaccoonSettings(null);
  const saved = POSTS.filter(p => p.url === '/settings/save');
  assert.equal(saved.length, 1, '保存应只发一条 /settings/save');
  assert.deepEqual(saved[0].body, {
    raccoon: {enabled: true, timeout_seconds: 90,
              stream_timeout_seconds: 1200, max_concurrent: 4},
  }, '保存载荷必须只有 raccoon 的四个字段');
  assert.ok(GETS.includes('/realm'), '保存后应刷新出口选项');
  assert.ok(global.__updateUiCalls > 0, '保存后应刷新界面');
  assert.ok(toasts.some(t => t[1] === 'ok'), '保存成功应给出成功提示');

  // 2c. 测试连接：POST /raccoon/test，成功 / 失败都要有可读提示。
  POSTS = [];
  testReply = {ok: true, models: 9, message: '连接正常'};
  await api.testRaccoon(null);
  assert.equal(POSTS.filter(p => p.url === '/raccoon/test').length, 1,
    '测试连接应 POST /raccoon/test');
  assert.ok(toasts[toasts.length - 1][0].includes('小浣熊连接正常'),
    '测试成功应提示连接正常');

  POSTS = [];
  testStatus = 502;
  testReply = {error: {message: '没有可用的 Raccoon 账号'}};
  await api.testRaccoon(null);
  assert.ok(toasts[toasts.length - 1][0].includes('小浣熊连接失败'),
    '测试失败应提示连接失败');
  assert.equal(toasts[toasts.length - 1][1], 'bad', '测试失败应用错误样式');
  testStatus = 200;

  // 2d. Key 出口下拉：启用时追加「固定小浣熊出口」，禁用时完全消失。
  api.setRealmOptions(['intl', 'cn', 'codearts', 'raccoon']);
  api.setKeyRows([{_editing: true, realm: 'raccoon', name: '小浣熊固定',
                   concurrency_limit: 0, models: [], accounts: [],
                   key: '', masked: '', enabled: true}]);
  api.renderKeyRows();
  let keyHtml = byId('keyList').innerHTML;
  assert.ok(keyHtml.includes('value="raccoon"'), '启用后应有 raccoon 选项');
  assert.ok(keyHtml.includes('固定小浣熊出口'), 'raccoon 选项应显示中文标签');
  assert.ok(/value="raccoon"\s+selected/.test(keyHtml), '已绑定的 raccoon 应选中');

  api.setRealmOptions(['intl', 'cn']);
  api.setKeyRows([{_editing: true, realm: '', name: '跟随',
                   concurrency_limit: 0, models: [], accounts: [],
                   key: '', masked: '', enabled: true}]);
  api.renderKeyRows();
  keyHtml = byId('keyList').innerHTML;
  assert.ok(!keyHtml.includes('固定小浣熊出口'), '未启用时不应出现 raccoon 选项');
  assert.ok(!keyHtml.includes('value="raccoon"'), '未启用时不应有 raccoon 选项值');
  assert.ok(keyHtml.includes('固定国际版出口') && keyHtml.includes('固定国内版出口'),
    '未启用时原有出口选项必须保持不变');

  // 2e. 标签：活动 / 用量 / realmLabel / Key 用量。
  assert.equal(api.activityLabels().raccoon, '小浣熊', '活动记录标签');
  assert.equal(api.realmLabel('raccoon'), '小浣熊', 'realmLabel');
  assert.ok(api.keyRealmCell({realm: 'raccoon'}).includes('小浣熊'),
    '用量表 Key 出口应显示小浣熊');
  assert.ok(/realm-raccoon/.test(api.keyRealmCell({realm: 'raccoon'})),
    '用量表应使用 realm-raccoon 标签样式');

  // 2f. 英文词典：新卡片 / 弹窗 / 设置 / Key 标签都有词条。
  assert.equal(api.i18n.translate('小浣熊'), 'Raccoon', '小浣熊');
  assert.equal(api.i18n.translate('小浣熊 (Raccoon)'), 'Raccoon', '卡片标题');
  assert.equal(api.i18n.translate('小浣熊账号'), 'Raccoon accounts', '账号区标题');
  assert.equal(api.i18n.translate('添加小浣熊账号'), 'Add a Raccoon account', '弹窗标题');
  assert.equal(api.i18n.translate('固定小浣熊出口'), 'Raccoon exit only', 'Key 出口');
  assert.equal(api.i18n.translate('保存小浣熊设置'), 'Save Raccoon settings', '设置保存');

  // 新页面的说明 / 帮助 / 动态提示也必须能整句翻译，不能只覆盖按钮。
  assert.equal(api.i18n.translate(
    '第四个出口直接连商汤小浣熊：导入 auth.json 或本机回环授权后，模型发现、token 续期、冷却与失败切换都在本进程内完成。小浣熊没有积分或签到接口。'),
    'The fourth exit connects directly to SenseTime Raccoon: after importing auth.json or authorizing through the local loopback, model discovery, token refresh, cooldown and failover all happen in this process. Raccoon exposes no credits or check-in API.',
    '设置说明');
  assert.equal(api.i18n.translate(
    '浏览器和本网关必须共享 loopback（同一台机器）。远程用户请导入 auth.json；v1.6.0 暂不支持扫码或密码登录。'),
    'The browser and this gateway must share loopback (the same machine). Remote users should import auth.json; QR-code and password login are not available in v1.6.0.',
    'loopback 提示');
  assert.equal(api.i18n.translate(
    '粘贴桌面客户端 auth.json 的完整 JSON，或 auth 响应里的 {"data": {...}}。'),
    'Paste the full auth.json from the desktop client, or the {"data": {...}} object from an auth response.',
    '导入帮助');
  assert.equal(api.i18n.translate(
    '点击「开始浏览器授权」生成链接；授权完成后本页会自动轮询结果。'),
    'Click Start browser authorization to create a link; this page polls for the result automatically once authorization finishes.',
    '浏览器授权帮助');
  assert.equal(api.i18n.translate('2 个账号，1 个健康；账号在「账号」页的小浣熊视图管理。'),
    '2 accounts, 1 healthy; manage accounts in the Raccoon view on the Accounts page.',
    '账号提示');
  assert.equal(api.i18n.translate('登录成功'), 'Signed in', '成功提示');
  assert.equal(api.i18n.translate('授权失败'), 'Authorization failed', '失败提示');

  console.log('raccoon settings/label assertions passed');
})().catch(error => {
  console.log = realLog;
  console.error(error && error.stack || error);
  process.exit(1);
});
