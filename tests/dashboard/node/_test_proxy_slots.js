'use strict';
const assert = require('node:assert');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');
const page = loadDashboard();
page.toast=()=>{};
const byId = id => page.document.getElementById(id);
(async ()=>{
  assert.equal(page.WB_I18N.translate('添加代理槽'),'Add proxy slot');
  assert.equal(page.WB_I18N.translate('+ 添加空槽位'),'+ Add empty slot');
  assert.equal(page.WB_I18N.translateHant('添加代理槽'),'新增代理槽');
  assert.equal(page.proxyURLFromParts('socks5','127.0.0.1','10808',''), 'socks5://127.0.0.1:10808');
  assert.equal(page.proxyURLFromParts('http','[::1]','7890','user:p@ss'), 'http://user:p%40ss@[::1]:7890');
  assert.throws(()=>page.proxyURLFromParts('socks4','127.0.0.1','1080',''), /不支持的代理协议/);
  assert.throws(()=>page.proxyURLFromParts('http','127.0.0.1','0',''), /端口/);
  assert.throws(()=>page.proxyURLFromParts('http','http://127.0.0.1','1080',''), /只填主机名/);
  assert.throws(()=>page.proxyURLFromParts('http','example.com?x=1','1080',''), /只填主机名/);
  assert.throws(()=>page.proxyURLFromParts('http','[::1','1080',''), /格式无效/);
  assert.equal(page.proxyCompareKey('socks5h://HOST:1080'),page.proxyCompareKey('socks5://host:1080'));

  byId('slotAddProtocol').value='socks5';
  byId('slotAddHost').value='127.0.0.1';
  byId('slotAddPort').value='10808';
  byId('slotAddAuth').value='user:p@ss';
  page.addProxySlotFromForm(null);
  let payload=page.slotPayload();
  assert.equal(payload.length,1);
  assert.equal(payload[0].url,'socks5://user:p%40ss@127.0.0.1:10808');
  assert.equal(byId('slotAddHost').value,'');

  byId('slotAddHost').value='127.0.0.1';
  byId('slotAddPort').value='10808';
  byId('slotAddAuth').value='user:p@ss';
  page.addProxySlotFromForm(null);
  payload=page.slotPayload();
  assert.equal(payload.length,1,'duplicate proxy URL must not add a second slot');
  console.log('proxy slot add form tests passed');
})().catch(e=>{console.error(e);process.exit(1);});
