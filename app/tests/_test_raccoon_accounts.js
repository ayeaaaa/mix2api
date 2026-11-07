/* 小浣熊（Raccoon）账号表与启停/测试/备注/任务/出口/删除操作。
 *
 * 表里只出现账号、状态、Token 剩余、出口、并发、模型与操作；没有 WorkBuddy /
 * CodeArts 的积分和签到列，也不渲染任何 access_token / refresh_token。
 *
 * Run with Node: node tests/_test_raccoon_accounts.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

assert.ok(html.includes('id="raccoonAccounts"'), '缺少小浣熊账号表');
assert.ok(/renderRaccoonAccounts/.test(script), '缺少账号表渲染函数');
assert.ok(/raccoonSlotSelectHtml/.test(script), '缺少小浣熊出口选择器');
assert.ok(script.includes('/raccoon/accounts/proxy'), '缺少出口绑定端点');
assert.ok(script.includes("raccoonAccountAction(\\'delete\\'"),
  '缺少删除操作入口');
assert.ok(/confirm\("删除小浣熊账号/.test(script), '删除应有二次确认');
assert.ok(script.includes("raccoonAccountAction(\\'clear-cooldown\\'"),
  '缺少清冷却操作入口');
assert.ok(script.includes("raccoonAccountAction(\\'tasks\\'"),
  '缺少任务开关操作入口');

let POSTS = [];
let GETS = [];
let FAIL_OVERVIEW = false;
dom.installDom({
  confirm: () => true,
  fetch: (url, opts) => {
    const body = opts && opts.body ? JSON.parse(String(opts.body)) : {};
    GETS.push(String(url));
    if (opts && opts.method === 'POST') POSTS.push({url: String(url), body});
    if (FAIL_OVERVIEW && String(url).indexOf('/raccoon/overview') >= 0
        && (!opts || opts.method !== 'POST')) {
      return Promise.resolve({
        status: 500, ok: false,
        json: () => Promise.resolve({}),
        text: () => Promise.resolve(''),
      });
    }
    const payload = {ok: true, message: '已删除 u1', online: true,
                     stats: {total: 2, healthy: 1}, accounts: [], models: [],
                     slots: []};
    return Promise.resolve({
      status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload)),
    });
  },
});

const realLog = console.log;
console.log = () => {};
let api;
try {
  api = new Function(script + `
    window.updateUI = function(){};
    return {renderRaccoonAccounts: renderRaccoonAccounts,
            renderRaccoonCards: renderRaccoonCards,
            loadRaccoonOverview: loadRaccoonOverview,
            raccoonAccountAction: raccoonAccountAction,
            raccoonSetProxy: raccoonSetProxy,
            raccoonEditNote: raccoonEditNote,
            raccoonGrantDesktop: raccoonGrantDesktop,
            raccoonGrantMobile: raccoonGrantMobile,
            setSlots: function(v){ PROXY_SLOTS = v; }};
  `)();
} catch (e) {
  console.log = realLog;
  console.error('脚本求值失败:', e.message);
  process.exit(1);
}
console.log = realLog;

const box = document.getElementById('raccoonAccounts');
api.setSlots([{id: 'slot-1', name: '美国 住宅', url: 'http://p:1',
               enabled: true}]);
api.renderRaccoonAccounts({
  stats: {total: 2, healthy: 1},
  accounts: [
    {uid: 'u1', nickname: '浣熊一号', disabled: false, cooling: false,
     reason: '', token_remaining: '29d0h0m', expires_at: '2026-12-01T12:00:00+08:00',
     active_concurrent: 0, max_concurrent: 3, proxy_slot: 'slot-1', proxy: '',
     note: '备注一', tasks_enabled: true, models: 9,
     credits: {available: 1234, today_used: 3, week_used: 7, unit: 'points'},
     points_tasks: {
       new_user: [
         {key: 'register_pack', status: 'completed'},
         {key: 'desktop_login', status: 'unfinished'},
         {key: 'mobile_login', status: 'unfinished'},
       ],
       invitee: [], invite: {registered_invitee_count: 0}, fetched_at: 1,
     },
     access_token: 'SECRET_ACCESS', refresh_token: 'SECRET_REFRESH'},
    {uid: 'u2', nickname: '浣熊二号', disabled: true, cooling: false,
     reason: 'manual', token_remaining: '1d0h0m', expires_at: '',
     active_concurrent: 0, max_concurrent: 1, proxy_slot: '', proxy: '',
     note: '', tasks_enabled: false, models: 5},
  ],
});
assert.ok(box.innerHTML.includes('浣熊一号'), '应渲染昵称');
assert.ok(box.innerHTML.includes('浣熊二号'), '应渲染第二个账号');
assert.ok(box.innerHTML.includes('u1'), '应渲染 UID');
assert.ok(box.innerHTML.includes('已停用'), '停用账号应显示状态');
assert.ok(box.innerHTML.includes('29d0h0m'), '应显示 Token 剩余时间');
assert.ok(box.innerHTML.includes('2026-12-01T12:00:00+08:00'), '应显示 Token 到期时间');
assert.ok(box.innerHTML.includes('<th>Token</th>'), '账号表应有 Token 列');
assert.ok(box.innerHTML.includes('<th>出口</th>'), '账号表应有出口列');
assert.ok(box.innerHTML.includes("raccoonAccountAction('enable','u2')"),
  '停用账号应显示启用按钮');
assert.ok(box.innerHTML.includes("raccoonAccountAction('disable','u1')"),
  '正常账号应显示停用按钮');
assert.ok(box.innerHTML.includes("raccoonAccountAction('clear-cooldown','u1')"),
  '应显示清冷却按钮');
assert.ok(box.innerHTML.includes("raccoonAccountAction('test','u1')"),
  '应显示单账号测试按钮');
assert.ok(box.innerHTML.includes("raccoonAccountAction('tasks','u1',false)"),
  '应显示任务开关按钮');
assert.ok(box.innerHTML.includes("raccoonEditNote('u1')"),
  '应显示备注按钮');
assert.ok(box.innerHTML.includes("raccoonAccountAction('delete','u1')"),
  '应显示删除按钮');
assert.ok(box.innerHTML.includes('备注一'), '账号表应显示备注');
assert.ok(box.innerHTML.includes('raccoonSetProxy'), '应显示出口选择器');
assert.ok(box.innerHTML.includes('slot-1'), '出口选择器应包含已绑定槽位');
assert.ok(box.innerHTML.includes('美国 住宅'), '出口选择器应显示槽位名称');
assert.ok(/raccoonRefreshCredits/.test(script), '应显示刷新积分入口');
assert.ok(script.includes('/raccoon/credits/refresh'), '缺少积分刷新端点');

// 小浣熊有积分、没有签到；也不能把 token 字段带进表格。
assert.ok(!box.innerHTML.includes('SECRET_ACCESS'), '不能渲染 access_token');
assert.ok(!box.innerHTML.includes('SECRET_REFRESH'), '不能渲染 refresh_token');
assert.ok(box.innerHTML.includes('<th>积分</th>'), '小浣熊账号表应有积分列');
assert.ok(box.innerHTML.includes('1234'), '应显示积分余额');
assert.ok(box.innerHTML.includes('今日 -3'), '应显示今日消耗');
assert.ok(box.innerHTML.includes('7天 -7'), '应显示近 7 天消耗');
assert.ok(!box.innerHTML.includes('任务：'), '任务状态应放在任务与福利页');
assert.ok(!box.innerHTML.includes('领取桌面奖励'), '桌面奖励入口应在任务与福利页');
assert.ok(!box.innerHTML.includes('领取移动端奖励'), '移动奖励入口应在任务与福利页');
assert.ok(!box.innerHTML.includes('签到'), '小浣熊账号表不应有签到列');
assert.ok(!box.innerHTML.includes('签到'), '小浣熊账号表不应有签到列');

// UID 来自服务端/上游，不能直接拼进 inline onclick 的 JS 字符串。HTML 转义
// 不处理单引号，所以这里必须用只允许安全字符的 id 形态。
api.renderRaccoonAccounts({
  stats: {total: 1, healthy: 1},
  accounts: [{uid: "x' onmouseover='alert(1)", nickname: 'evil',
              disabled: false, cooling: false, proxy_slot: '',
              token_remaining: '1h0m0s', expires_at: '',
              active_concurrent: 0, max_concurrent: 1, tasks_enabled: true,
              models: 1}],
});
const evilHtml = box.innerHTML;
assert.ok(!/onclick="[^"]*x' onmouseover/.test(evilHtml),
  'UID 中的单引号不能进入 inline onclick 的 JS 字符串');
assert.ok(evilHtml.includes("raccoonAccountAction('delete','xonmouseoveralert1')"),
  'UID 应按安全字符集收敛后再拼进处理器');

// 昵称、备注与旧代理 URL 同样来自服务端/上游，必须 HTML 转义。
api.setSlots([]);
api.renderRaccoonAccounts({
  stats: {total: 1, healthy: 1},
  accounts: [{uid: 'u3', nickname: '<img src=x onerror=alert(1)>',
              note: '<script>alert("note")</script>', disabled: false,
              cooling: false, proxy_slot: '',
              proxy: 'http://p/?x=<svg onload=alert(3)>',
              token_remaining: '1h0m0s', expires_at: '',
              active_concurrent: 0, max_concurrent: 1, tasks_enabled: true,
              models: 1}],
});
const xssHtml = box.innerHTML;
assert.ok(!xssHtml.includes('<img'), '昵称不能原样输出 HTML');
assert.ok(!xssHtml.includes('<script'), '备注不能原样输出 HTML');
assert.ok(!xssHtml.includes('<svg'), '代理 URL 不能原样输出 HTML');
assert.ok(xssHtml.includes('&lt;img'), '昵称应被 HTML 转义');
assert.ok(xssHtml.includes('&lt;script&gt;'), '备注应被 HTML 转义');
assert.ok(xssHtml.includes('&lt;svg'), '代理 URL 应被 HTML 转义');

// 空状态要告诉用户从哪里开始。
api.renderRaccoonAccounts({stats: {total: 0}, accounts: []});
assert.ok(box.innerHTML.includes('auth.json'), '空状态应提示导入 auth.json');
assert.ok(box.innerHTML.includes('添加账号'), '空状态应提示添加账号');
assert.ok(box.innerHTML.includes('小浣熊'), '空状态应说明是小浣熊账号');

(async () => {
  GETS = [];
  await api.loadRaccoonOverview(true);
  assert.ok(GETS.some(u => u.indexOf('/raccoon/overview?force=1') >= 0),
    'loadRaccoonOverview(true) 应带上 force 查询参数');
  assert.equal(window.RACCOON_OVERVIEW.stats.total, 2,
    'loadRaccoonOverview 应填充 window.RACCOON_OVERVIEW');

  api.renderRaccoonCards({stats: {total: 2, healthy: 1},
                          models: [{id: 'm1'}, {id: 'm2'}]});
  const count = document.getElementById('raccoonAcctCount');
  count.textContent = ' · 2';
  FAIL_OVERVIEW = true;
  await api.loadRaccoonOverview(true);
  assert.equal(window.RACCOON_OVERVIEW, null,
    '读取失败应清空 RACCOON_OVERVIEW，避免继续显示旧数据');
  assert.equal(document.getElementById('raccoonCards').innerHTML, '',
    '读取失败应清空统计卡');
  assert.equal(count.textContent, '', '读取失败应清空账号计数');
  assert.ok(box.innerHTML.includes('导入 auth.json'), '读取失败应显示可读错误提示');
  FAIL_OVERVIEW = false;

  await api.raccoonAccountAction('delete', 'u1');
  const hit = POSTS.find(p => p.url.indexOf('/raccoon/accounts/delete') >= 0);
  assert.ok(hit, '删除应 POST /raccoon/accounts/delete');
  assert.equal(hit.body.uid, 'u1', '删除应带上 uid');

  POSTS = [];
  await api.raccoonSetProxy('u1', 'slot-1', null);
  const bind = POSTS.find(p => p.url.indexOf('/raccoon/accounts/proxy') >= 0);
  assert.ok(bind, '出口绑定应 POST /raccoon/accounts/proxy');
  assert.equal(bind.body.uid, 'u1', '出口绑定应带上 uid');
  assert.equal(bind.body.slot_id, 'slot-1', '出口绑定应带上 slot_id');

  POSTS = [];
  await api.raccoonAccountAction('tasks', 'u1', false);
  const tasks = POSTS.find(p => p.url.indexOf('/raccoon/accounts/tasks') >= 0);
  assert.ok(tasks, '任务开关应 POST /raccoon/accounts/tasks');
  assert.equal(tasks.body.enabled, false, '任务开关应带上 enabled');

  POSTS = [];
  POSTS = [];
  await api.raccoonAccountAction('note', 'u1', 'hello');
  const note = POSTS.find(p => p.url.indexOf('/raccoon/accounts/note') >= 0);
  assert.ok(note, '备注应 POST /raccoon/accounts/note');
  assert.equal(note.body.note, 'hello', '备注应带上 note');

  POSTS = [];
  await api.raccoonAccountAction('test', 'u1');
  const tested = POSTS.find(p => p.url.indexOf('/raccoon/accounts/test') >= 0);
  assert.ok(tested, '测试应 POST /raccoon/accounts/test');
  assert.equal(tested.body.uid, 'u1', '测试应带上 uid');

  console.log('raccoon account assertions passed');
})().catch(error => { console.log = realLog; console.error(error); process.exit(1); });
