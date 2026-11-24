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
    '代理地址重复':'Duplicate proxy address',
    '用户名（可选）':'Username (optional)',
    '密码（可选）':'Password (optional)',
    '协议':'Protocol',
    '服务器':'Server',
    '认证':'Authentication',
    '用户名':'Username',
    '密码':'Password',
    '高级':'Advanced',
    '收起高级':'Hide advanced',
    '原始 URL':'Raw URL',
    '包含路径/查询或特殊格式，结构化字段只读；原始 URL 可能明文显示凭据':'Contains a path/query or special format; structured fields are read-only. The raw URL may show credentials in plain text.',
    '可填写带认证或特殊格式的完整地址；原始 URL 可能明文显示凭据':'Use this for a full address with auth or a special format. The raw URL may show credentials in plain text.',
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
    '代理地址重复':'代理位址重複',
    '包含路径/查询或特殊格式，请编辑原始 URL':'包含路徑/查詢或特殊格式，請編輯原始 URL',
    '用户名（可选）':'使用者名稱（可選）',
    '密码（可选）':'密碼（可選）',
    '协议':'協定',
    '服务器':'伺服器',
    '认证':'認證',
    '用户名':'使用者名稱',
    '密码':'密碼',
    '高级':'進階',
    '收起高级':'收起進階',
    '原始 URL':'原始 URL',
    '支持 HTTP / HTTPS / SOCKS5。Docker 部署时 127.0.0.1 指容器自身，请填宿主机地址、容器名或 host.docker.internal；添加后记得点「保存槽位」，再在账号表绑定出口。':'支援 HTTP / HTTPS / SOCKS5。Docker 部署時 127.0.0.1 指容器自身，請填主機位址、容器名稱或 host.docker.internal；新增後記得點「儲存槽位」，再在帳號表綁定出口。'
  };
  for(const [src,want] of Object.entries(hant)) assert.equal(page.WB_I18N.translateHant(src),want,src);

  const parsed = page.parseProxySlotURL('socks5://user:p%40ss@127.0.0.1:10808');
  assert.equal(parsed.protocol,'socks5');
  assert.equal(parsed.host,'127.0.0.1');
  assert.equal(parsed.port,'10808');
  assert.equal(parsed.username,'user');
  assert.equal(parsed.password,'p@ss');
  assert.equal(parsed.advanced,false);
  assert.equal(page.composeProxyURL(parsed),'socks5://user:p%40ss@127.0.0.1:10808');
  const v6 = page.parseProxySlotURL('http://[::1]:7890');
  assert.equal(v6.host,'::1');
  assert.equal(page.composeProxyURL(v6),'http://[::1]:7890');
  const raw = page.parseProxySlotURL('127.0.0.1:10808');
  assert.equal(raw.protocol,'http');
  assert.equal(page.composeProxyURL(raw),'http://127.0.0.1:10808');
  const complex = page.parseProxySlotURL('http://host:8080/path?x=1');
  assert.equal(complex.advanced,true);
  assert.equal(page.composeProxyURL(complex),'http://host:8080/path?x=1');
  assert.equal(page.validateProxyParts({protocol:'socks5',host:'',port:'1080',username:'',password:''}),'请填写代理服务器');
  assert.equal(page.validateProxyParts({protocol:'socks5',host:'127.0.0.1',port:'70000',username:'',password:''}),'代理端口必须是 1–65535');
  assert.equal(page.validateProxyParts({protocol:'http',host:'127.0.0.1',port:'1080',username:'',password:'p'}),null);
  assert.equal(page.composeProxyURL({protocol:'http',host:'127.0.0.1',port:'1080',username:'',password:'p'}),'http://:p@127.0.0.1:1080');
  assert.equal(page.composeProxyURL({protocol:'http',host:'127.0.0.1',port:'080',username:'',password:''}),'http://127.0.0.1:80');
  assert.equal(page.validateProxyParts({protocol:'http',host:'127.0.0.1',port:'1e3',username:'',password:''}),'代理端口必须是 1–65535');
  const legacy = page.parseProxySlotURL('127.0.0.1:1080:user:pass');
  assert.equal(legacy.protocol,'http');
  assert.equal(legacy.host,'127.0.0.1');
  assert.equal(legacy.username,'user');
  assert.equal(legacy.password,'pass');
  assert.equal(page.composeProxyURL(legacy),'http://user:pass@127.0.0.1:1080');
  assert.equal(page.proxyCompareKey('127.0.0.1:1080:user:pass'),page.proxyCompareKey('http://user:pass@127.0.0.1:1080'));
  const legacyScheme=page.parseProxySlotURL('socks5://127.0.0.1:1080:user:pass');
  assert.equal(legacyScheme.protocol,'socks5');
  assert.equal(page.composeProxyURL(legacyScheme),'socks5://user:pass@127.0.0.1:1080');
  assert.equal(page.validateProxyParts({protocol:'http',host:'::ffff:127.0.0.1',port:'1080',username:'',password:''}),null);
  assert.equal(page.validateProxyParts({protocol:'http',host:'127.0.0.1',port:'',username:'',password:''}),null);
  assert.equal(page.parseProxySlotURL('ftp://host:1080').supported,false);
  assert.equal(page.proxySlotValidationError({url:'ftp://host:1080'}),'代理地址格式无效');
  assert.equal(page.proxySlotValidationError({url:'http://host:8080/path?x=1'}),null);
  assert.equal(page.proxySlotValidationError({url:'http://host:1080',_parts:{protocol:'http',host:'host',port:'1080',username:'',password:'p',advanced:false,supported:true}}),null);

  assert.equal(page.proxyURLFromParts('socks5','127.0.0.1','10808',''), 'socks5://127.0.0.1:10808');
  assert.equal(page.proxyURLFromParts('http','[::1]','7890','user:p@ss'), 'http://user:p%40ss@[::1]:7890');
  assert.throws(()=>page.proxyURLFromParts('socks4','127.0.0.1','1080',''), /不支持的代理协议/);
  assert.throws(()=>page.proxyURLFromParts('http','127.0.0.1','0',''), /端口/);
  assert.throws(()=>page.proxyURLFromParts('http','http://127.0.0.1','1080',''), /只填主机名/);
  assert.throws(()=>page.proxyURLFromParts('http','example.com?x=1','1080',''), /只填主机名/);
  assert.throws(()=>page.proxyURLFromParts('http','[::1','1080',''), /格式无效/);
  assert.equal(page.proxyCompareKey('socks5h://HOST:1080'),page.proxyCompareKey('socks5://host:1080'));
  assert.notEqual(page.proxyCompareKey('http://host/path1'),page.proxyCompareKey('http://host/path2'));
  assert.equal(page.proxySlotValidationError({url:'127.0.0.1:1080:user:pass'}),null);
  const onlyPass=page.parseProxySlotURL('http://:p@host:1080');
  assert.equal(onlyPass.password,'p');
  assert.equal(page.composeProxyURL(onlyPass),'http://:p@host:1080');

  byId('slotAddProtocol').value='socks5';
  byId('slotAddHost').value='127.0.0.1';
  byId('slotAddPort').value='10808';
  byId('slotAddUser').value='user';
  byId('slotAddPassword').value='p@ss';
  page.addProxySlotFromForm(null);
  let payload=page.slotPayload();
  assert.equal(payload.length,1);
  assert.equal(payload[0].url,'socks5://user:p%40ss@127.0.0.1:10808');
  const tableHtml=dom.byId('slotList').innerHTML;
  assert.ok(tableHtml.includes('data-label="协议"'),'table renders protocol column');
  assert.ok(tableHtml.includes('type="password"'),'auth password is masked');
  assert.ok(!tableHtml.includes('slotRaw0'),'collapsed advanced row keeps raw URL out of the DOM');
  page.toggleProxyAdvanced(0);
  assert.ok(dom.byId('slotList').innerHTML.includes('slotRaw0'),'advanced raw URL editor opens on demand');
  assert.equal(byId('slotAddHost').value,'');
  page.updateProxyStructured(0,'port','10809');
  assert.equal(page.slotPayload()[0].url,'socks5://user:p%40ss@127.0.0.1:10809');
  page.updateProxyStructured(0,'port','10808');

  byId('slotAddHost').value='127.0.0.1';
  byId('slotAddPort').value='10808';
  byId('slotAddUser').value='user';
  byId('slotAddPassword').value='p@ss';
  page.addProxySlotFromForm(null);
  payload=page.slotPayload();
  assert.equal(payload.length,1,'duplicate proxy URL must not add a second slot');
  page.updateProxyRaw(0,'http://host:9999/path?x=1');
  page.refreshProxySlotRow(0);
  assert.equal(page.slotPayload()[0].url,'http://host:9999/path?x=1');
  assert.ok(dom.byId('slotList').innerHTML.includes('结构化字段只读'),'complex URL locks structured fields');

  page.updateProxyRaw(0,'socks5://user:p%40ss@127.0.0.1:10808');
  page.refreshProxySlotRow(0);
  const calls=[];
  const toasts=[];
  page.toast=(msg)=>{toasts.push(String(msg));};
  page.loadAccounts=async ()=>{};
  page.postJSON=async (url,body)=>{
    calls.push({url,body:JSON.parse(JSON.stringify(body))});
    if(url==='/proxy/slots/save'){
      return {slots:(body.slots||[]).map(s=>Object.assign({},s,{id:'slot-1'}))};
    }
    if(url==='/proxy/slots/test'){
      return {ok:true,id:'slot-1',exit_ip:'203.0.113.20',latency_ms:12,country:'美国',ip_type:'residential',slot:{id:'slot-1'}};
    }
    throw new Error('unexpected '+url);
  };
  page.updateProxyStructured(0,'port','70000');
  await page.saveProxySlots(null);
  assert.equal(calls.length,0,'invalid port must not save');
  assert.equal(toasts.pop(),'代理端口必须是 1–65535');
  page.updateProxyStructured(0,'port','10808');
  await page.saveProxySlots(null);
  assert.equal(calls.length,1,'valid save posts');
  assert.equal(calls[0].url,'/proxy/slots/save');
  assert.equal(calls[0].body.slots[0].url,'socks5://user:p%40ss@127.0.0.1:10808');
  page.updateProxyStructured(0,'port','10809');
  await page.testProxySlot(0,null);
  assert.deepEqual(calls.slice(1).map(c=>c.url),['/proxy/slots/save','/proxy/slots/test'],'dirty test saves current URL before probing');
  assert.equal(calls[1].body.slots[0].url,'socks5://user:p%40ss@127.0.0.1:10809');
  assert.equal(calls[2].body.id,'slot-1');
  assert.ok(toasts.some(t=>t.includes('203.0.113.20')),'success toast uses probe result');
  console.log('proxy slot add form tests passed');
})().catch(e=>{console.error(e);process.exit(1);});
