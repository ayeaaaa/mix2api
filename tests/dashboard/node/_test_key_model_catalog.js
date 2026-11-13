/* Key 模型选择器的目录来源与区域过滤。
 *
 * Key 一旦绑定某个出口，它只能看到那个出口的模型。网关目录
 * (`/v1/models?realm=gateway`) 给出的 id 带出口前缀（`cn/m1`）并附带
 * `realm` 字段，正是 Key 白名单该存的形式；旧的无范围 / intl / cn 列表只有
 * 裸 id，且可能只覆盖一个区域。所以：
 *
 *   1. 先读 `/v1/models?realm=gateway`，成功就不再打旧接口；
 *   2. 网关目录失败或为空时，保留原来的 `/v1/models` → intl / cn 回退；
 *   3. 选择器按 `row.realm` 过滤；空 realm（跟随面板切换）显示全部；
 *      旧条目没有 realm，不能被过滤掉；
 *   4. 编辑中切换出口时，旧出口的 canonical 选择会失效（`cn/m1` 在 intl 下
 *      永远匹配不上），必须在切换与保存两处清掉并提示；裸模式仍然有效。
 *
 * Run with Node:
 *   node tests/dashboard/node/_test_key_model_catalog.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml} = require('./_dashboard_source.js');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');

// installDom() 会把 global.setTimeout 换成 no-op；等异步落定要自己先抓住平台定时器。
const PLATFORM_SET_TIMEOUT = setTimeout;

const html = dashboardHtml();

let checks = 0;
function check(label, ok, detail){
  assert.ok(ok, label + (detail === undefined ? '' : '  -> ' + detail));
  checks += 1;
}

function jsonResponse(payload){
  return Promise.resolve({
    status: 200, ok: true,
    json: () => Promise.resolve(payload),
    text: () => Promise.resolve(JSON.stringify(payload)),
  });
}

const GATEWAY_MODELS = [
  {id: 'cn/m1', realm: 'cn'},
  {id: 'codearts/m1', realm: 'codearts'},
  {id: 'intl/m1', realm: 'intl'},
];
const LEGACY_MODELS = [
  {id: 'm-legacy'},
  {id: 'cn/m1', realm: 'cn'},
];

const SETTINGS = {
  panel_password_is_default: false,
  api_keys: [
    {id: 'k1', name: '固定国内版', realm: 'cn', models: ['cn/m1', 'm-legacy'], enabled: true,
     masked: 'wb-aaaa****1111', created_at: '2026-09-01'},
    {id: 'k2', name: '跟随面板切换', realm: '', models: [], enabled: true,
     masked: 'wb-bbbb****2222', created_at: '2026-09-02'},
    {id: 'k3', name: '固定 CodeArts', realm: 'codearts', models: ['codearts/m1'], enabled: true,
     masked: 'wb-cccc****3333', created_at: '2026-09-03'},
  ],
  deleted_api_keys: [],
  limits: {},
  version: 'v-test',
  accounts_dir: '/data/accounts',
};

let gatewayMode = 'ok';
let CALLS = [];
let POSTS = [];

function payloadFor(url){
  if(url === '/settings') return SETTINGS;
  if(url === '/v1/models') return {object: 'list', data: LEGACY_MODELS};
  if(url.indexOf('/v1/models?realm=') === 0){
    // The gateway URL is handled before this point (it has its own failure mode).
    return {object: 'list', data: []};
  }
  if(url === '/accounts'){
    return {accounts: [], slots: [], data: [], results: [], byAccount: [], rows: [], total: 0};
  }
  return {current: 'intl', accounts: [], slots: [], data: [], results: [], byAccount: [], rows: [], total: 0};
}

const page = loadDashboard({
  fetch: (url, opts) => {
    const u = String(url);
    CALLS.push(u);
    if(opts && opts.method === 'POST'){
      POSTS.push({url: u, body: JSON.parse(String(opts.body || '{}'))});
    }
    if(u === '/v1/models?realm=gateway'){
      if(gatewayMode === 'fail'){
        return Promise.resolve({
          status: 503, ok: false,
          json: () => Promise.resolve({error: {message: 'offline'}}),
          text: () => Promise.resolve('{"error":{"message":"offline"}}'),
        });
      }
      return jsonResponse({object: 'list', realm: 'gateway', data: GATEWAY_MODELS});
    }
    return jsonResponse(payloadFor(u));
  },
});

const settle = () => new Promise(resolve => PLATFORM_SET_TIMEOUT(resolve, 0));

(async () => {
  /* ---- 1. 结构：renderKeyPickers 按编辑行的 realm 过滤 --------------- */
  check('renderKeyPickers 应调用 filterKeyModelsForRealm(models, keyEditRealm(row))',
    /filterKeyModelsForRealm\(models, keyEditRealm\(row\)\)/.test(html));

  /* ---- 1b. 全部提供商 Key ------------------------------------------ */
  check('Key 出口选项应包含全部提供商',
    /\[['"]gateway['"],\s*['"]全部提供商/.test(html));
  check('全部提供商 Key 的模型过滤返回全部',
    page.filterKeyModelsForRealm(GATEWAY_MODELS, 'gateway').length === GATEWAY_MODELS.length,
    JSON.stringify(page.filterKeyModelsForRealm(GATEWAY_MODELS, 'gateway')));
  const gatewayReconciled = page.reconcileKeyModelsForRealm(['cn/m1', 'codearts/m1'], 'gateway');
  check('切到全部提供商时保留所有 canonical 选择',
    JSON.stringify(gatewayReconciled.models) === JSON.stringify(['cn/m1', 'codearts/m1']) &&
      gatewayReconciled.dropped.length === 0,
    JSON.stringify(gatewayReconciled));
  check('Key 列表显示全部提供商',
    page.keyRealmCell({realm: 'gateway', source: 'panel'}).includes('全部提供商'),
    page.keyRealmCell({realm: 'gateway', source: 'panel'}));
  check('分析表显示 CodeArts 出口',
    page.keyRealmCell({realm: 'codearts', source: 'panel'}).includes('CodeArts'),
    page.keyRealmCell({realm: 'codearts', source: 'panel'}));

  /* ---- 2. 网关目录优先 ---------------------------------------------- */
  CALLS = [];
  const models = await page.loadKeyModelCatalog();
  check('第一条请求必须是网关目录',
    CALLS[0] === '/v1/models?realm=gateway', JSON.stringify(CALLS));
  check('网关目录成功时不再打旧接口',
    CALLS.length === 1, JSON.stringify(CALLS));
  check('返回网关目录的三个 id',
    JSON.stringify(models.map(m => m.id)) === JSON.stringify(['cn/m1', 'codearts/m1', 'intl/m1']),
    JSON.stringify(models));

  /* ---- 3. 按 realm 过滤，空 realm 与旧条目放行 ----------------------- */
  check('realm=cn 只留 cn/m1',
    JSON.stringify(page.filterKeyModelsForRealm(models, 'cn').map(m => m.id))
      === JSON.stringify(['cn/m1']),
    JSON.stringify(page.filterKeyModelsForRealm(models, 'cn')));
  check('realm=codearts 只留 codearts/m1',
    JSON.stringify(page.filterKeyModelsForRealm(models, 'codearts').map(m => m.id))
      === JSON.stringify(['codearts/m1']),
    JSON.stringify(page.filterKeyModelsForRealm(models, 'codearts')));
  check('空 realm（跟随面板切换）返回全部',
    page.filterKeyModelsForRealm(models, '').length === models.length,
    JSON.stringify(page.filterKeyModelsForRealm(models, '')));
  check('没有 realm 的旧条目不会被过滤掉',
    JSON.stringify(page.filterKeyModelsForRealm(LEGACY_MODELS, 'cn').map(m => m.id))
      === JSON.stringify(['m-legacy', 'cn/m1']),
    JSON.stringify(page.filterKeyModelsForRealm(LEGACY_MODELS, 'cn')));

  /* ---- 4. 网关失败回退到无范围列表，选择器不能空 -------------------- */
  // Let any initial background catalog load settle, then force a fresh read so
  // the assertion does not depend on cache timing.
  await settle();
  delete page.KEY_MODEL_CATALOG;
  gatewayMode = 'fail';
  CALLS = [];
  const fallback = await page.loadKeyModelCatalog(true);
  check('网关失败后仍然先尝试网关目录',
    CALLS[0] === '/v1/models?realm=gateway', JSON.stringify(CALLS));
  check('网关失败后回退到 /v1/models',
    CALLS.includes('/v1/models'), JSON.stringify(CALLS));
  check('回退结果不为空',
    fallback.length > 0 && fallback.some(m => m.id === 'm-legacy'),
    JSON.stringify(fallback));

  /* ---- 5. 选择器渲染：同一份目录按两行各自的 realm 过滤 ------------- */
  gatewayMode = 'ok';
  delete page.KEY_MODEL_CATALOG;
  await page.loadSettings(true);
  await settle();
  // A background settings/catalog load may have populated the fallback list
  // from step 4. Force the gateway catalog before rendering so this assertion
  // is independent of that timing.
  await page.loadKeyModelCatalog(true);
  await settle();

  await page.renderKeyPickers(0);
  const cnList = document.getElementById('editKeyModelList_0').innerHTML;
  check('固定国内版的 Key 只渲染国内版模型（旧条目照常保留）',
    cnList.includes('value="cn/m1"') && !cnList.includes('codearts/m1')
      && !cnList.includes('intl/m1'),
    cnList);
  check('目录里没有的已保存精确模型作为勾选的 legacy 行出现',
    /value="m-legacy"[^>]*checked/.test(cnList) && cnList.includes('legacy'),
    cnList);

  // The DOM stub does not materialize the rows written through innerHTML, so
  // build the checkboxes the panel would have rendered for the select-all test.
  const checkboxValues = (inner) => {
    const values = [];
    const re = /<input type="checkbox" value="([^"]+)"/g;
    let match;
    while((match = re.exec(inner))) values.push(match[1]);
    return values;
  };
  const modelList0 = document.getElementById('editKeyModelList_0');
  const boxes0 = checkboxValues(cnList).map(value => {
    const el = dom.makeElement('input');
    el.value = value;
    return el;
  });
  modelList0.querySelectorAll = () => boxes0;
  page.openEditKey(0);
  await page.renderKeyPickers(0);
  page.setKeyModelsAll(0, true);
  check('全选从已有精确集合出发，不会丢掉目录里没有的 legacy 模型',
    JSON.stringify(page.API_KEY_ROWS[0]._draftModels)
      === JSON.stringify(['cn/m1', 'm-legacy']),
    JSON.stringify(page.API_KEY_ROWS[0]._draftModels));

  // An empty filtered list must not be read as "select nothing", which the
  // server interprets as an unrestricted allow-list.
  page.openEditKey(1);
  page.API_KEY_ROWS[1]._draftRealm = 'raccoon';
  page.API_KEY_ROWS[1]._draftModels = ['raccoon/hidden'];
  const emptyList1 = document.getElementById('editKeyModelList_1');
  emptyList1.innerHTML = '';
  emptyList1.querySelectorAll = () => [];
  const warnLog = console.log;
  const warnings = [];
  console.log = (...args) => warnings.push(args.join(' '));
  try {
    page.setKeyModelsAll(1, true);
  } finally {
    console.log = warnLog;
  }
  check('空筛选列表全选保留原限制，不会放宽成「不限制」',
    JSON.stringify(page.API_KEY_ROWS[1]._draftModels) === JSON.stringify(['raccoon/hidden']),
    JSON.stringify(page.API_KEY_ROWS[1]._draftModels));
  page.cancelEditKey(1);

  await page.renderKeyPickers(1);
  const allList = document.getElementById('editKeyModelList_1').innerHTML;
  check('跟随面板切换的 Key 渲染全部模型',
    allList.includes('value="cn/m1"') && allList.includes('value="codearts/m1"')
      && allList.includes('value="intl/m1"'),
    allList);

  /* ---- 6. 编辑中切换出口：只改草稿，已保存的行不动 ------------------ */
  check('出口下拉框切换时应重算模型清单', /onchange="changeKeyRealm\('/.test(html));

  page.openEditKey(0);
  const realmSelect = document.getElementById('editKeyRealm_0');
  realmSelect.value = 'intl';
  const logs = [];
  const realLog = console.log;
  console.log = (...args) => logs.push(args.join(' '));
  try {
    await page.changeKeyRealm(0, realmSelect.value);
  } finally {
    console.log = realLog;
  }

  const editingRow = page.API_KEY_ROWS[0];
  check('切换出口只更新草稿，不改动已保存的 realm',
    editingRow.realm === 'cn', editingRow.realm);
  check('切换出口只更新草稿，不改动已保存的模型',
    JSON.stringify(editingRow.models) === JSON.stringify(['cn/m1', 'm-legacy']),
    JSON.stringify(editingRow.models));
  check('草稿按新出口清理了旧 canonical 选择',
    editingRow._draftRealm === 'intl'
      && JSON.stringify(editingRow._draftModels) === JSON.stringify(['m-legacy']),
    JSON.stringify([editingRow._draftRealm, editingRow._draftModels]));
  check('切换后给出移除提示', logs.some(line => line.includes('已移除')), JSON.stringify(logs));
  const switchedList = document.getElementById('editKeyModelList_0').innerHTML;
  check('切换后选择器只列目标出口的模型',
    switchedList.includes('value="intl/m1"') && !switchedList.includes('cn/m1'),
    switchedList);

  /* ---- 7. 取消：草稿丢弃，之后整表保存不得提交被取消的出口 ---------- */
  page.cancelEditKey(0);
  check('取消后已保存的 realm 仍是 cn', editingRow.realm === 'cn', editingRow.realm);
  check('取消后已保存的模型仍是原列表',
    JSON.stringify(editingRow.models) === JSON.stringify(['cn/m1', 'm-legacy']),
    JSON.stringify(editingRow.models));
  check('取消后草稿被丢弃',
    editingRow._draftRealm === undefined && editingRow._draftModels === undefined,
    JSON.stringify([editingRow._draftRealm, editingRow._draftModels]));
  check('取消后编辑状态结束', editingRow._editing === false, editingRow._editing);
  const canceledCard = document.getElementById('keyList').innerHTML;
  check('取消后卡片回到原出口', canceledCard.includes('固定国内版出口'), canceledCard);
  check('取消后卡片回到原模型', canceledCard.includes('限 cn/m1, m-legacy'), canceledCard);

  POSTS = [];
  // 取消后再触发一次整表保存（启用/禁用另一把 Key 走的就是 saveApiKeys）。
  await page.toggleKeyRow(1);
  const cancelPost = POSTS.find(p => p.url === '/settings/save');
  const cancelSaved = cancelPost && cancelPost.body && cancelPost.body.api_keys
    && cancelPost.body.api_keys[0];
  check('取消后的整表保存仍提交 cn', !!cancelSaved && cancelSaved.realm === 'cn',
    JSON.stringify(cancelSaved));
  check('取消后的整表保存仍提交原模型列表',
    !!cancelSaved && JSON.stringify(cancelSaved.models) === JSON.stringify(['cn/m1', 'm-legacy']),
    JSON.stringify(cancelSaved && cancelSaved.models));

  /* ---- 8. 保存：提交草稿里的新出口并清掉旧 canonical 选择 ----------- */
  await page.loadSettings(true);
  await settle();
  const keyListHtml = document.getElementById('keyList').innerHTML;
  check('Key 卡片显示固定 CodeArts 出口',
    keyListHtml.includes('固定 CodeArts 出口'),
    keyListHtml.slice(0, 300));
  page.openEditKey(0);
  document.getElementById('editKeyRealm_0').value = 'intl';
  await page.changeKeyRealm(0, 'intl');
  // Simulate a stale canonical entry surviving in the draft; save must still
  // reconcile it away instead of posting an id from the old exit.
  page.API_KEY_ROWS[0]._draftModels = ['cn/m1', 'm-legacy'];
  document.getElementById('editKeyName_0').value = '固定国际版';
  document.getElementById('editKeyValue_0').value = '';
  POSTS = [];
  await page.saveSingleKey(0);
  const savePost = POSTS.find(p => p.url === '/settings/save');
  const savedKey = savePost && savePost.body && savePost.body.api_keys
    && savePost.body.api_keys[0];
  check('保存时提交新的出口', !!savedKey && savedKey.realm === 'intl',
    JSON.stringify(savedKey));
  check('保存时不会提交旧出口的 canonical id',
    !!savedKey && JSON.stringify(savedKey.models) === JSON.stringify(['m-legacy']),
    JSON.stringify(savedKey && savedKey.models));

  /* ---- 9. 切到没有模型的出口：不能静默放宽成「不限制」 --------------- */
  await page.loadSettings(true);
  await settle();
  page.openEditKey(0);
  page.API_KEY_ROWS[0]._draftModels = ['cn/m1'];
  document.getElementById('editKeyRealm_0').value = 'raccoon';
  await page.changeKeyRealm(0, 'raccoon');
  POSTS = [];
  await page.saveSingleKey(0);
  check('目标出口没有模型时不提交空限制',
    !POSTS.some(p => p.url === '/settings/save'), JSON.stringify(POSTS));
  check('目标出口没有模型时已保存的模型列表保持原样',
    JSON.stringify(page.API_KEY_ROWS[0].models) === JSON.stringify(['cn/m1', 'm-legacy']),
    JSON.stringify(page.API_KEY_ROWS[0].models));

  console.log('key model catalog assertions passed (' + checks + ' checks)');
})().catch(e => { console.error(e); process.exit(1); });
