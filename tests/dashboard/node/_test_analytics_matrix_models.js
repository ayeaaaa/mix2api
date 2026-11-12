/* 统计矩阵的白名单必须同时认识规范 ID 和裸 ID。
 *
 * 网关目录的每条记录同时带 `id`（`intl/deepseek`）和 `upstream_id`
 * （`deepseek`）。出口限定请求按 `requested_model` 聚合成前者；裸请求和
 * 升级前的历史行仍按 `model` 聚合成后者。如果白名单只收 `id`，后者会被
 * 静默过滤掉，而合计行还在统计它，表现为「有请求但表格里看不到」。
 *
 * 这里把 loadAnalyticsMatrix 和 renderPerfMatrix 的源码取出来，用最小
 * 运行时跑一遍真实数据，断言两行都留在表格里。
 *
 * Run with Node:
 *   node tests/dashboard/node/_test_analytics_matrix_models.js
 */
'use strict';
const assert = require('assert');
const vm = require('node:vm');
const {dashboardHtml} = require('./_dashboard_source.js');

const html = dashboardHtml();

let checks = 0;
function check(label, ok, detail){
  assert.ok(ok, label + (detail === undefined ? '' : '  -> ' + detail));
  checks += 1;
}

function extract(pattern){
  return (html.match(pattern) || [''])[0];
}
const matrixFn = extract(/async function loadAnalyticsMatrix\(\)\{[\s\S]*?\n\}/);
const renderFn = extract(/function renderPerfMatrix\(usage, perf\)\{[\s\S]*?\n\}/);

const GATEWAY = {
  object: 'list', realm: 'gateway',
  data: [{id: 'intl/deepseek', upstream_id: 'deepseek', realm: 'intl'}],
};
const USAGE = {
  requests: 2, errors: 0, total_tokens: 20, prompt_tokens: 10,
  completion_tokens: 10, reasoning_tokens: 0, cost_cny: 0,
  by_model: {
    'intl/deepseek': {requests: 1, total_tokens: 10, prompt_tokens: 5,
      completion_tokens: 5, reasoning_tokens: 0, cached_tokens: 0, accounts: {}},
    'deepseek': {requests: 1, total_tokens: 10, prompt_tokens: 5,
      completion_tokens: 5, reasoning_tokens: 0, cached_tokens: 0, accounts: {}},
  },
  by_model_realm: {}, by_model_acct: {}, accounts_map: {},
};
const PERF = {
  errors: 0,
  by_model: {
    'intl/deepseek': {requests: 1},
    'deepseek': {requests: 1},
  },
  by_model_realm: {}, by_model_acct: {},
};

async function renderMatrix(){
  const elements = {
    perfMatrix: {innerHTML: ''},
    matrixModelCount: {textContent: ''},
  };
  const context = {
    console: console,
    POLL_READ: {},
    MATRIX_MODELS: new Set(),
    matrixModelsAt: 0,
    MATRIX_MODELS_TTL_MS: 0,
    cachedMatrixData: null,
    matrixModelFilter: '',
    matrixAcctFilter: '',
    PRICING_ON: false,
    analyticsQuery: () => '?realm=all',
    getJSON: (url) => {
      if(url === '/v1/models?realm=gateway') return Promise.resolve(GATEWAY);
      if(url.indexOf('/usage/perf') === 0) return Promise.resolve(PERF);
      return Promise.resolve(USAGE);
    },
    document: {
      getElementById: (id) => elements[id] || null,
    },
    fillMatrixFilters: () => {},
    isAuthError: () => false,
    esc: (value) => String(value),
    fmt: (value) => String(value),
    tripleTokens: (a, b, c) => [a, b, c].join(' / '),
    ms: (value) => String(value) + ' ms',
    pct: (value) => String(value) + '%',
    fmtCost: (value) => String(value),
    applyTableSort: () => {},
  };
  vm.createContext(context);
  vm.runInContext(matrixFn + '\n' + renderFn + '\n' + `
    var matrixDone = loadAnalyticsMatrix().then(function(){
      return {
        models: Array.from(MATRIX_MODELS),
        html: document.getElementById('perfMatrix').innerHTML,
      };
    });
  `, context);
  return context.matrixDone;
}

(async () => {
  const result = await renderMatrix();
  const models = result.models;

  check('白名单收录规范 ID', models.indexOf('intl/deepseek') >= 0, JSON.stringify(models));
  check('白名单收录裸 upstream ID', models.indexOf('deepseek') >= 0, JSON.stringify(models));
  check('规范 ID 的用量行仍渲染',
    /data-label="模型"><b>intl\/deepseek<\/b>/.test(result.html), result.html.slice(0, 400));
  check('裸 ID 的用量行仍渲染',
    /data-label="模型"><b>deepseek<\/b>/.test(result.html), result.html.slice(0, 400));

  console.log('analytics matrix model suite passed: ' + checks + ' checks');
})().catch(e => { console.error(e); process.exit(1); });
