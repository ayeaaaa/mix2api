/* 小浣熊任务与福利页：任务状态、积分、桌面/移动奖励入口。
 *
 * 账号表只负责账号管理，任务状态和领取按钮在这里渲染。
 * Run with Node: node tests/_test_raccoon_tasks.js
 */
'use strict';

const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();

assert.ok(html.includes('id="sectionRaccoonTasks"'), '缺少小浣熊任务区块');
assert.ok(html.includes('id="raccoonTaskCards"'), '缺少小浣熊任务统计卡');
assert.ok(html.includes('id="raccoonTasksTable"'), '缺少小浣熊任务表');
assert.ok(html.includes('id="raccoonAutoClaim"'), '缺少自动领取选项');

let POSTS = [];
let GETS = [];
dom.installDom({
  fetch: (url, opts) => {
    const body = opts && opts.body ? JSON.parse(String(opts.body)) : {};
    GETS.push(String(url));
    if (opts && opts.method === 'POST') {
      POSTS.push({url: String(url), body});
    }
    const payload = {
      ok: true, message: '操作成功', online: true,
      stats: {total: 0, healthy: 0}, accounts: [], models: [], slots: [],
    };
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
    return {
      renderRaccoonTasks: renderRaccoonTasks,
      raccoonGrantDesktop: raccoonGrantDesktop,
      raccoonGrantMobile: raccoonGrantMobile,
      raccoonDailyActive: raccoonDailyActive,
      saveRaccoonAutoClaim: saveRaccoonAutoClaim,
      loadRaccoonTasks: loadRaccoonTasks
    };
  `)();
} catch (e) {
  console.log = realLog;
  console.error('脚本求值失败:', e.message);
  process.exit(1);
}
console.log = realLog;

const cards = document.getElementById('raccoonTaskCards');
const table = document.getElementById('raccoonTasksTable');

function render(accounts){
  api.renderRaccoonTasks({accounts});
}

// 已全部完成的账号：显示状态和积分，不显示领取按钮。
render([{
  uid: 'u1', nickname: '已领完', disabled: false,
  credits: {available: 9284, today_used: 7},
  points_tasks: {new_user: [
    {key: 'register_pack', status: 'completed'},
    {key: 'desktop_login', status: 'completed'},
    {key: 'mobile_login', status: 'completed'},
  ], invitee: []},
}]);
assert.ok(cards.innerHTML.includes('小浣熊账号池'), '应渲染账号统计卡');
assert.ok(cards.innerHTML.includes('9284'), '应汇总可用积分');
assert.ok(cards.innerHTML.includes('待领取登录奖励'), '应有待领取奖励卡');
assert.ok(table.innerHTML.includes('已领完'), '三项完成应显示已领完');
assert.ok(table.innerHTML.includes('注册'), '应显示注册礼包列');
assert.ok(table.innerHTML.includes('✓'), '应显示完成标记');
assert.ok(!table.innerHTML.includes('raccoonGrantDesktop'), '不应显示桌面领取按钮');
assert.ok(!table.innerHTML.includes('raccoonGrantMobile'), '不应显示移动领取按钮');

// 移动端未完成：只显示移动领取按钮。
render([{
  uid: 'u2', nickname: '移动待领', disabled: false,
  credits: {available: 100, today_used: 0},
  points_tasks: {new_user: [
    {key: 'register_pack', status: 'completed'},
    {key: 'desktop_login', status: 'completed'},
    {key: 'mobile_login', status: 'unfinished'},
  ], invitee: []},
}]);
assert.ok(table.innerHTML.includes("raccoonGrantMobile('u2'"),
  '移动未完成应显示领取按钮');
assert.ok(!table.innerHTML.includes("raccoonGrantDesktop('u2'"),
  '桌面已完成不应显示桌面按钮');
assert.ok(cards.innerHTML.includes('3000'), '待领取奖励应计入 3000');

// 桌面端未完成：只显示桌面领取按钮。
render([{
  uid: 'u3', nickname: '桌面待领', disabled: false,
  credits: {available: 50, today_used: 1},
  points_tasks: {new_user: [
    {key: 'register_pack', status: 'completed'},
    {key: 'desktop_login', status: 'unfinished'},
    {key: 'mobile_login', status: 'completed'},
  ], invitee: []},
}]);
assert.ok(table.innerHTML.includes("raccoonGrantDesktop('u3'"),
  '桌面未完成应显示领取按钮');
assert.ok(!table.innerHTML.includes("raccoonGrantMobile('u3'"),
  '移动已完成不应显示移动按钮');

// 被邀请账号的专属任务。
render([{
  uid: 'u4', nickname: '被邀请', disabled: false,
  credits: {available: 10}, points_tasks: {new_user: [], invitee: [
    {key: 'active_7d', status: 'in_progress'},
    {key: 'first_payment', status: 'unfinished'},
  ]},
}]);
assert.ok(table.innerHTML.includes('7日 …'), '应显示 7 日活跃任务');
assert.ok(table.innerHTML.includes('首付 —'), '应显示首次付费任务');
assert.ok(!table.innerHTML.includes("raccoonGrantDesktop('u4'"),
  '被邀请账号不应显示桌面按钮');
assert.ok(!table.innerHTML.includes("raccoonGrantMobile('u4'"),
  '被邀请账号不应显示移动按钮');

// 上游昵称和 UID 仍然必须转义，不能进入 inline onclick。
render([{
  uid: "x' onmouseover='alert(1)",
  nickname: '<img src=x onerror=alert(1)>',
  credits: {available: 1},
  points_tasks: {new_user: [{key: 'mobile_login', status: 'unfinished'}]},
}]);
assert.ok(!table.innerHTML.includes('<img'), '昵称不能原样输出 HTML');
assert.ok(table.innerHTML.includes('&lt;img'), '昵称应被 HTML 转义');
assert.ok(!/onclick="[^"]*x' onmouseover/.test(table.innerHTML),
  'UID 中的单引号不能进入 onclick');
assert.ok(table.innerHTML.includes("raccoonGrantMobile('xonmouseoveralert1'"),
  'UID 应按安全字符集收敛');

// 空状态。
render([]);
assert.ok(table.innerHTML.includes('还没有小浣熊账号'), '空状态应提示先添加账号');

// 自动领取开关应跟随概览里的配置。
const autoClaim = document.getElementById('raccoonAutoClaim');
api.renderRaccoonTasks({auto_claim_login: true, accounts: []});
assert.equal(autoClaim.checked, true, 'auto_claim_login=true 应勾选');
api.renderRaccoonTasks({auto_claim_login: false, accounts: []});
assert.equal(autoClaim.checked, false, 'auto_claim_login=false 应取消勾选');

(async () => {
  POSTS = [];
  await api.saveRaccoonAutoClaim(true);
  const autoSave = POSTS.find(p => p.url.indexOf('/settings/save') >= 0);
  assert.ok(autoSave, '自动领取开关应 POST /settings/save');
  assert.equal(autoSave.body.raccoon.auto_claim_login, true,
    '自动领取开关应带上 auto_claim_login');
  assert.ok(GETS.some(u => u.indexOf('/raccoon/overview?force=1') >= 0),
    '保存自动领取设置后应刷新概览');
  window.VIEW_REALM = 'raccoon';
  GETS = [];
  await api.loadRaccoonTasks(true);
  assert.ok(GETS.some(u => u.indexOf('/raccoon/overview?force=1') >= 0),
    'loadRaccoonTasks(true) 应强制刷新概览');

  render([{
    uid: 'u2', nickname: '移动待领', credits: {available: 100},
    points_tasks: {new_user: [{key: 'mobile_login', status: 'unfinished'}]},
  }]);
  POSTS = [];
  await api.raccoonGrantMobile('u2', null);
  assert.ok(POSTS.some(p => p.url.indexOf('/raccoon/accounts/grant-mobile') >= 0),
    '移动按钮应 POST grant-mobile');

  POSTS = [];
  await api.raccoonGrantDesktop('u2', null);
  assert.ok(POSTS.some(p => p.url.indexOf('/raccoon/accounts/grant-desktop') >= 0),
    '桌面按钮应 POST grant-desktop');

  // 每日活跃列：未打卡 / 已到账 / 未到账，以及手动打卡按钮。
  render([{
    uid: 'd1', nickname: '未打卡', credits: {available: 1},
    points_tasks: {new_user: []},
  }]);
  assert.ok(table.innerHTML.includes('今日未打卡'), '无状态应显示今日未打卡');
  assert.ok(table.innerHTML.includes("raccoonDailyActive('d1'"),
    '应提供每日活跃打卡按钮');

  const now = new Date();
  const today = now.getFullYear() + '-'
    + String(now.getMonth() + 1).padStart(2, '0') + '-'
    + String(now.getDate()).padStart(2, '0');
  render([{
    uid: 'd2', nickname: '已到账', credits: {available: 1},
    points_tasks: {new_user: []},
    daily_active: {date: today, ok: true, points: 300, attempts: 1},
  }]);
  assert.ok(table.innerHTML.includes('已到账 300'), '到账应显示积分');
  assert.ok(cards.innerHTML.includes('今日活跃奖励'), '应有今日活跃奖励卡');

  render([{
    uid: 'd3', nickname: '未到账', credits: {available: 1},
    points_tasks: {new_user: []},
    daily_active: {date: today, ok: false, points: 0, attempts: 2},
  }]);
  assert.ok(table.innerHTML.includes('未到账'), '未到账应显示提示');

  POSTS = [];
  await api.raccoonDailyActive('d1', null);
  const dailyPost = POSTS.find(
    p => p.url.indexOf('/raccoon/accounts/daily-active') >= 0);
  assert.ok(dailyPost, '打卡按钮应 POST daily-active');
  assert.equal(dailyPost.body.uid, 'd1', '打卡应带账号 uid');

  console.log('raccoon task page assertions passed');
})().catch(error => {
  console.log = realLog;
  console.error(error);
  process.exit(1);
});