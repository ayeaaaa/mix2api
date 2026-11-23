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
    '认证用户名不能为空':'Auth username cannot be empty',
    '支持 HTTP / HTTPS / SOCKS5。Docker 部署时 127.0.0.1 指容器自身，请填宿主机地址、容器名或 host.docker.internal；添加后记得点「保存槽位」，再在账号表绑定出口。':'Supports HTTP / HTTPS / SOCKS5. Inside Docker, 127.0.0.1 is the container itself; use the host address, a container name, or host.docker.internal. After adding, click Save slots, then bind the exit in the account table.'
  };
  for(const [src,want] of Object.entries(en)) assert.equal(page.WB_I18N.translate(src),want,src);
  const hant = {
    '添加代理槽':'新增代理槽',
    '+ 添加空槽位':'+ 新增空槽位',
    '代理服务器，如 127.0.0.1':'代理伺服器，如 127.0.0.1',
    '认证 user:pass（可选）':'認證 user:pass（可選）',
    '代理端口必须是 1–65535':'代理埠必須是 1–65535',
    '高级：添加一行后直接填写完整代理 URL':'進階：新增一行後直接填寫完整代理 URL',
    '端口':'埠',
    '该代理地址已在槽位中':'此代理位址已在槽位中',
    '已加入代理槽，记得点「保存槽位」':'已加入代理槽，記得點「儲存槽位」',
    '添加失败: ':'新增失敗: ',
    '不支持的代理协议（请使用 HTTP / HTTPS / SOCKS5）':'不支援的代理協定（請使用 HTTP / HTTPS / SOCKS5）',
    '请填写代理服务器':'請填寫代理伺服器',
    '代理服务器只填主机名或 IP，不要带协议、端口或路径':'代理伺服器只填主機名稱或 IP，不要帶協定、埠或路徑',
    '代理服务器格式无效':'代理伺服器格式無效',
    '认证用户名不能为空':'認證使用者名稱不能為空',
    '支持 HTTP / HTTPS / SOCKS5。Docker 部署时 127.0.0.1 指容器自身，请填宿主机地址、容器名或 host.docker.internal；添加后记得点「保存槽位」，再在账号表绑定出口。':'支援 HTTP / HTTPS / SOCKS5。Docker 部署時 127.0.0.1 指容器自身，請填主機位址、容器名稱或 host.docker.internal；新增後記得點「儲存槽位」，再在帳號表綁定出口。'
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
