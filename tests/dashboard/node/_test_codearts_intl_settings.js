/* CodeArts 国际站（codearts-intl）设置与 Key 标签。
 *
 * 国际站有自己的一节设置：只暴露两个站共用的字段（启用、并发、超时、流式
 * 超时、模型缓存），不能出现国内的签到/福利开关；保存只写
 * settings["codearts-intl"]，不能顺手动到国内站。
 *
 * 设置区块必须始终可见——站点还没启用时，用户正是要在这一节里把它打开。
 * Key/模型/用量视图也要把两个站分清楚：CodeArts 与 CodeArts 国际。
 *
 * Run with Node: node tests/dashboard/node/_test_codearts_intl_settings.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
const script = dashboardScript();
for(const id of ['codeartsIntlSettingsSection', 'setCodeartsIntlEnabled',
                 'setCodeartsIntlMaxConcurrent', 'setCodeartsIntlState']){
  assert.ok(html.includes('id="' + id + '"'), 'dashboard missing #' + id);
}
assert.ok(script.includes('function saveCodeartsIntlSettings'), 'missing international settings save');
assert.ok(script.includes('function renderCodeartsIntlSettings'), 'missing international settings render');
assert.ok(script.includes('codearts-intl'), 'missing international realm labels');

/* 国内站设置里的签到/福利字段不能出现在国际站这一节。 */
const section = html.slice(html.indexOf('id="codeartsIntlSettingsSection"'));
const sectionEnd = section.indexOf('</section>');
const sectionHtml = section.slice(0, sectionEnd < 0 ? section.length : sectionEnd);
for(const id of ['setCodeartsAutoCheckin', 'setCodeartsCheckinTime',
                 'setCodeartsCheckinRetry', 'setCodeartsExpiringPriority']){
  assert.ok(!sectionHtml.includes('id="' + id + '"'),
    'international settings must not expose mainland field #' + id);
}

const posts = [];
const page = loadDashboard({
  fetch: (url, opts) => {
    const body = opts && opts.body ? JSON.parse(String(opts.body)) : null;
    if(opts && opts.method === 'POST') posts.push({url: String(url), body: body});
    let payload = {current: 'intl', options: ['intl', 'cn', 'codearts-intl']};
    if(String(url).indexOf('/settings/save') === 0){
      payload = {'codearts-intl': Object.assign({enabled: true, accounts: 1, healthy: 1}, body && body['codearts-intl'])};
    }
    return Promise.resolve({status: 200, ok: true,
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload))});
  },
});
(async () => {
  page.renderCodeartsIntlSettings({
    enabled: true, max_concurrent: 1, timeout_seconds: 30,
    stream_timeout_seconds: 600, models_ttl_seconds: 300,
    accounts: 1, healthy: 1,
  });
  assert.equal(dom.byId('setCodeartsIntlEnabled').checked, true);
  assert.equal(dom.byId('setCodeartsIntlState').textContent, '(已启用)');
  dom.byId('setCodeartsIntlMaxConcurrent').value = '2';
  await page.saveCodeartsIntlSettings();
  const saved = posts.filter(p => p.url === '/settings/save').pop();
  assert.ok(saved && saved.body['codearts-intl'], 'international settings not saved under codearts-intl');
  assert.equal(saved.body['codearts-intl'].max_concurrent, 2);
  assert.ok(!('codearts' in saved.body), 'international save must not touch the mainland section');

  /* Key/用量标签：两个 CodeArts 站要能分辨，国际站不能落回「跟随」。 */
  assert.ok(page.keyRealmCell({realm: 'codearts-intl', source: 'panel'}).includes('CodeArts 国际'),
    'international key should be labelled CodeArts 国际');
  const mainlandKey = page.keyRealmCell({realm: 'codearts', source: 'panel'});
  assert.ok(mainlandKey.includes('CodeArts') && !mainlandKey.includes('国际'),
    'mainland key label must stay distinct: ' + mainlandKey);
  const reconciled = page.reconcileKeyModelsForRealm(['codearts-intl/glm-5.3'], 'codearts');
  assert.equal(JSON.stringify(reconciled.models), '[]',
    'codearts-intl canonical ids must be scoped to their realm');
  assert.equal(JSON.stringify(reconciled.dropped), JSON.stringify(['codearts-intl/glm-5.3']));

  /* 未启用时区块照样渲染，用户才能在这里打开它。 */
  page.renderCodeartsIntlSettings({
    enabled: false, max_concurrent: 1, timeout_seconds: 30,
    stream_timeout_seconds: 600, models_ttl_seconds: 300,
    accounts: 0, healthy: 0,
  });
  assert.equal(dom.byId('setCodeartsIntlEnabled').checked, false);
  assert.equal(dom.byId('setCodeartsIntlState').textContent, '(未启用)');
  const intlSectionTag = (html.match(/<section[^>]*id="codeartsIntlSettingsSection"[^>]*>/) || [''])[0];
  assert.ok(intlSectionTag && !/display\s*:\s*none/.test(intlSectionTag),
    'international settings section must stay visible while disabled');

  console.log('codearts intl settings assertions passed');
})().catch(err => {
  console.error(err.stack || err);
  process.exit(1);
});
