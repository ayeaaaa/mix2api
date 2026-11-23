'use strict';
const assert = require('node:assert');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');
const page = loadDashboard();
page.toast=()=>{};
const byId = id => page.document.getElementById(id);
(async ()=>{
  const en = {
    '添加代理槽':'Add proxy slot',
    '+ 添加空槽位':'+ Add empty slot',
    '高级：添加一行后直接填写完整代理 URL':'Advanced: add a row and enter the full proxy URL',
    '代理服务器，如 127.0.0.1':'Proxy server, e.g. 127.0.0.1',
    '端口':'Port',
    '认证 user:pass（可选）':'Auth user:pass (optional)',
    '该代理地址已在槽位中':'This proxy URL is already in a slot',
    '已加入代理槽，记得点「保存槽位」':'Added to proxy slots; remember to click Save slots',
    '添加失败: ':'Add failed: ',
    '不支持的代理协议（请使用 HTTP / HTTPS / SOCKS5）':'Unsupported proxy protocol (use HTTP / HTTPS / SOCKS5)',
    '请填写代理服务器':'Enter a proxy server',
    '代理服务器只填主机名或 IP，不要带协议、端口或路径':'Enter only a hostname or IP; do not include a scheme, port, or path',
    '代理服务器格式无效':'Invalid proxy server',
    '代理端口必须是 1–65535':'Proxy port must be 1–65535',
    '认证用户名不能为空':'Auth username cannot be empty'
  };
  for(const [src,want] of Object.entries(en)) assert.equal(page.WB_I18N.translate(src),want,src);
  const hant = {
    '添加代理槽':'新增代理槽',
    '+ 添加空槽位':'+ 新增空槽位',
    '代理服务器，如 127.0.0.1':'代理伺服器，如 127.0.0.1',
    '认证 user:pass（可选）':'認證 user:pass（可選）',
    '代理端口必须是 1–65535':'代理埠必須是 1–65535'
  };
  for(const [src,want] of Object.entries(hant)) assert.equal(page.WB_I18N.translateHant(src),want,src);
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
