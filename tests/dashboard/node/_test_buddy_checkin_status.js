/* Buddy 加油站徽章：账号今天已经签到成功时，不能被更早的失败重试盖掉。
 *
 * 现场：09:00/11:43/12:04 的定时重试因为代理未接入失败，13:52/13:58 手动签到
 * 成功并写入了 lastCheckin；活动历史没有补成功行，所以旧逻辑取最新失败事件，
 * 一直显示“Buddy 加油站 · 失败”。
 *
 * Run with Node: node tests/_test_buddy_checkin_status.js
 */
'use strict';
const assert = require('assert');
const {loadDashboard} = require('./_dashboard_sandbox.js');

let checks = 0;
function check(label, ok, detail){
  assert.ok(ok, label + (detail === undefined ? '' : '  -> ' + detail));
  checks += 1;
}

const d = new Date();
const pad = n => String(n).padStart(2, '0');
const TODAY = d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());

const page = loadDashboard({
  fetch: (url) => {
    if(String(url).indexOf('/activity/history') === 0){
      return Promise.resolve({
        ok: true, status: 200,
        json: () => Promise.resolve({rows: [{
          uid: 'u-buddy', nickname: '加油站测试', realm: 'cn', task: 'checkin',
          trigger: 'scheduler', ok: false, ts: TODAY + 'T12:04:03+08:00',
          message: 'workbuddy: task request failed',
        }]}),
        text: () => Promise.resolve('{}'),
      });
    }
    return Promise.resolve({
      ok: true, status: 200,
      json: () => Promise.resolve({accounts: [], slots: [], data: [], results: [], rows: []}),
      text: () => Promise.resolve('{}'),
    });
  },
});

(async () => {
  await page.loadActivityToday();
  const account = {
    uid: 'u-buddy', nickname: '加油站测试', realm: 'cn', enabled: true,
    lastCheckin: TODAY + ' 13:58:06',
  };
  const status = page.activityStatusOf(account);
  check('今天已签到优先于更早的失败事件', status.gas.kind === 'done', JSON.stringify(status.gas));
  check('状态显示已领', status.gas.state === '已领', status.gas.state);
  check('整体任务算完成', status.kind === 'done', status.kind);

  const chip = page.activityStatusChip(account);
  check('徽章显示 Buddy 加油站 · 已领', chip.includes('Buddy 加油站 · 已领'), chip);
  check('徽章不再显示失败', !chip.includes('Buddy 加油站 · 失败'), chip);

  console.log('buddy check-in status suite passed: ' + checks + ' checks');
})().catch(err => {
  console.error(err);
  process.exit(1);
});