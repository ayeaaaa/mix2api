'use strict';
const assert = require('node:assert');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');
const page = loadDashboard();
const posts=[];
page.postJSON=async (url,body)=>{posts.push({url,body:JSON.parse(JSON.stringify(body))});return {ok:true};};
page.loadSettings=async ()=>{};
page.toast=()=>{};
(async ()=>{
  page.renderAffinitySettings({
    affinity_by_prefix:true,
    affinity_max_messages:400,
    affinity_page_size:3,
    affinity_long_strategy:'cohort',
    affinity_provider_strategies:{},
    affinity_provider_page_sizes:{},
    affinity_model_pool_sizes:{'intl/deepseek-v4.1-flash-sg':3},
    remaining_priority_supported:false,
    remaining_priority_enabled:true
  });
  assert.equal(dom.byId('setAffinityPrefix').checked,true);
  assert.equal(dom.byId('setAffinityPageSize').value,'3');
  assert.equal(dom.byId('setAffinityLongStrategy').value,'cohort');
  assert.equal(dom.byId('setAffinityPageSizeLabel').textContent,'固定账号数');
  assert.equal(dom.byId('setAffinityStrategyWorkbuddy').value,'');
  assert.equal(dom.byId('setAffinityStrategyCodearts').value,'');
  assert.equal(dom.byId('setAffinityStrategyRaccoon').value,'');
  assert.equal(dom.byId('setAffinityPageWorkbuddy').value,'');
  assert.equal(dom.byId('setAffinityPageLabelWorkbuddy').textContent,'数量（个账号）');
  assert.equal(dom.byId('setAffinityPageWorkbuddy').placeholder,'跟随全局：3 个账号');
  assert.equal(dom.byId('setAffinityPageLabelCodearts').textContent,'数量（个账号）');
  assert.equal(dom.byId('setAffinityPageCodearts').placeholder,'跟随全局：3 个账号');
  assert.equal(dom.byId('setAffinityPageLabelRaccoon').textContent,'数量（个账号）');
  assert.equal(dom.byId('setAffinityPageRaccoon').placeholder,'跟随全局：3 个账号');
  assert.equal(dom.byId('setRemainingPriority').disabled,true);
  assert.equal(dom.byId('setRemainingPrioritySave').disabled,true);
  await page.saveRemainingPriority({disabled:false});
  assert.equal(posts.length,0,'unsupported control must not POST');
  dom.byId('setAffinityModelPools').value='{}';
  await page.saveAffinityRouting({disabled:false});
  assert.equal(posts.length,1);
  assert.equal(posts[0].body.affinity_model_pool_sizes['intl/deepseek-v4.1-flash-sg'],0,'clearing uses explicit off, not an empty deep-merge');
  assert.equal(posts[0].body.affinity_long_strategy,'cohort');
  assert.deepEqual(posts[0].body.affinity_provider_strategies,{workbuddy:'',codearts:'',raccoon:''});
  assert.deepEqual(posts[0].body.affinity_provider_page_sizes,{workbuddy:-1,codearts:-1,raccoon:-1});

  page.renderAffinitySettings({
    affinity_by_prefix:true,
    affinity_max_messages:401,
    affinity_page_size:4,
    affinity_long_strategy:'partition',
    affinity_provider_strategies:{workbuddy:'cohort',codearts:'partition',raccoon:''},
    affinity_provider_page_sizes:{workbuddy:5,codearts:-1,raccoon:0},
    affinity_model_pool_sizes:{},
    remaining_priority_supported:false
  });
  assert.equal(dom.byId('setAffinityLongStrategy').value,'partition');
  assert.equal(dom.byId('setAffinityPageSizeLabel').textContent,'分区数量');
  assert.equal(dom.byId('setAffinityStrategyWorkbuddy').value,'cohort');
  assert.equal(dom.byId('setAffinityStrategyCodearts').value,'partition');
  assert.equal(dom.byId('setAffinityStrategyRaccoon').value,'');
  assert.equal(dom.byId('setAffinityPageWorkbuddy').value,'5');
  assert.equal(dom.byId('setAffinityPageCodearts').value,'');
  assert.equal(dom.byId('setAffinityPageRaccoon').value,'0');
  assert.equal(dom.byId('setAffinityPageLabelWorkbuddy').textContent,'数量（个账号）');
  assert.equal(dom.byId('setAffinityPageWorkbuddy').placeholder,'本提供商：5 个账号');
  assert.equal(dom.byId('setAffinityPageLabelCodearts').textContent,'数量（个分区）');
  assert.equal(dom.byId('setAffinityPageCodearts').placeholder,'跟随全局：4 个分区');
  assert.equal(dom.byId('setAffinityPageLabelRaccoon').textContent,'数量（个分区）');
  assert.equal(dom.byId('setAffinityPageRaccoon').placeholder,'本提供商：0 个分区');
  await page.saveAffinityRouting({disabled:false});
  assert.equal(posts.length,2);
  assert.equal(posts[1].body.affinity_long_strategy,'partition');
  assert.equal(posts[1].body.affinity_page_size,4);
  assert.deepEqual(posts[1].body.affinity_provider_strategies,{workbuddy:'cohort',codearts:'partition',raccoon:''});
  assert.deepEqual(posts[1].body.affinity_provider_page_sizes,{workbuddy:5,codearts:-1,raccoon:0});

  dom.byId('setAffinityLongStrategy').value='cohort';
  dom.byId('setAffinityLongStrategy').onchange();
  assert.equal(dom.byId('setAffinityPageSizeLabel').textContent,'固定账号数');
  assert.equal(dom.byId('setAffinityPageLabelRaccoon').textContent,'数量（个账号）');
  assert.equal(dom.byId('setAffinityPageRaccoon').placeholder,'本提供商：0 个账号');
  assert.equal(dom.byId('setAffinityPageLabelCodearts').textContent,'数量（个分区）');
  dom.byId('setAffinityStrategyWorkbuddy').value='partition';
  dom.byId('setAffinityStrategyWorkbuddy').onchange();
  assert.equal(dom.byId('setAffinityPageLabelWorkbuddy').textContent,'数量（个分区）');
  assert.equal(dom.byId('setAffinityPageWorkbuddy').placeholder,'本提供商：5 个分区');
  dom.byId('setAffinityPageWorkbuddy').value='';
  dom.byId('setAffinityPageWorkbuddy').oninput();
  assert.equal(dom.byId('setAffinityPageWorkbuddy').placeholder,'跟随全局：4 个分区');
  dom.byId('setAffinityPageSize').value='6';
  dom.byId('setAffinityPageSize').oninput();
  assert.equal(dom.byId('setAffinityPageWorkbuddy').placeholder,'跟随全局：6 个分区');

  dom.byId('setAffinityPageWorkbuddy').value='1.5';
  await page.saveAffinityRouting({disabled:false});
  assert.equal(posts.length,2,'invalid fractional provider page must not POST');
  dom.byId('setAffinityPageWorkbuddy').value='5';
  dom.byId('setAffinityPageSize').value='1.5';
  await page.saveAffinityRouting({disabled:false});
  assert.equal(posts.length,2,'invalid fractional global page must not POST');
  console.log('affinity settings tests passed');
})().catch(e=>{console.error(e);process.exit(1);});
