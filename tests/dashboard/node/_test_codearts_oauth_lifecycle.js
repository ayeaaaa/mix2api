'use strict';
const assert = require('assert');
const {loadDashboard} = require('./_dashboard_sandbox.js');
const dom = require('./_dom_stub.js');

const settle = () => new Promise(resolve => setImmediate(resolve));
const response = (payload, status = 200) => ({status, ok: status === 200,
  json: async () => payload, text: async () => JSON.stringify(payload)});

// Control only network/timer boundaries, leaving the real modal and refresh
// code running. Keep canceled callbacks so we can also test their stale guards.
function harness(){
  const calls = [], pending = [], timeouts = [], intervals = [];
  let delayOverview = false;
  const page = loadDashboard({fetch: (url, opts) => {
    const target = String(url);
    const call = {url: target, body: opts && opts.body ? JSON.parse(opts.body) : null};
    calls.push(call);
    if(/\/accounts\/(oauth\/|import$)/.test(target) || (delayOverview && target.includes('/overview'))){
      return new Promise((resolve, reject) => pending.push(Object.assign(call, {
        resolve: (payload, status) => resolve(response(payload, status)), reject,
      })));
    }
    return Promise.resolve(response(target === '/realm'
      ? {current: 'codearts', options: ['codearts', 'codearts-intl']}
      : {stats: {}, accounts: [], models: [], slots: []}));
  }});
  page.REALM_OPTIONS = ['codearts', 'codearts-intl'];
  page.setTimeout = (fn, ms) => { timeouts.push({fn, ms}); return timeouts.length; };
  page.setInterval = fn => { intervals.push(fn); return intervals.length; };
  page.clearInterval = () => {};
  const buttons = [dom.makeElement('button'), dom.makeElement('button')];
  page.document.getElementById('codeartsOauthModal').querySelectorAll = () => buttons;
  const latest = suffix => pending.filter(r => r.url.endsWith(suffix)).at(-1);
  const open = async (realm, sid = realm) => {
    page.openCodeartsOauthModal(realm);
    latest('/oauth/start').resolve({session_id: sid, auth_url: 'https://example.test/' + sid});
    await settle();
  };
  const snapshot = () => ({
    url: dom.byId('codeartsOauthUrl').textContent,
    status: dom.byId('codeartsOauthStatus').textContent,
    callback: dom.byId('codeartsOauthCallback').value,
    session: dom.byId('codeartsSessionJson').value,
  });
  return {page, calls, pending, timeouts, intervals, buttons, latest, open, snapshot,
    delayOverview: () => { delayOverview = true; }};
}

const cases = [];
const test = (name, run) => cases.push({name, run});

test('late start cannot replace a reopened modal URL or session', async () => {
  const h = harness();
  h.page.openCodeartsOauthModal('codearts-intl');
  const old = h.latest('/oauth/start');
  h.page.closeCodeartsOauthModal();
  await h.open('codearts', 'new-mainland');
  const before = h.snapshot();
  old.resolve({session_id: 'old-intl', auth_url: 'https://example.test/old-intl'});
  await settle();
  assert.deepEqual(h.snapshot(), before, 'stale start overwrote the new modal');
  assert.equal(h.intervals.length, 1, 'stale start created another poll interval');
  const poll = h.page.pollCodeartsOAuth(false);
  const request = h.latest('/oauth/poll');
  assert.equal(request.url, '/codearts/accounts/oauth/poll');
  assert.equal(request.body.session_id, 'new-mainland');
  request.resolve({status: 'pending'});
  await poll;
});

test('late start failure cannot change the reopened same-realm status', async () => {
  const h = harness();
  h.page.openCodeartsOauthModal('codearts-intl');
  const old = h.latest('/oauth/start');
  h.page.closeCodeartsOauthModal();
  await h.open('codearts-intl', 'new-intl');
  const before = h.snapshot();
  old.reject(new Error('old start failure'));
  await settle();
  assert.deepEqual(h.snapshot(), before);
});

for(const result of [{status: 'pending', message: 'old pending'},
                    {status: 'done', ok: true}, {status: 'error', message: 'old failure'}, null]){
  test('late poll ' + (result ? result.status : 'rejection') + ' cannot affect a reopened modal', async () => {
    const h = harness();
    await h.open('codearts-intl');
    const oldInterval = h.intervals[0];
    const poll = h.page.pollCodeartsOAuth(false);
    const old = h.latest('/oauth/poll');
    h.page.closeCodeartsOauthModal();
    await h.open('codearts');
    dom.byId('codeartsSessionJson').value = 'new draft';
    const before = h.snapshot(), callCount = h.calls.length;
    if(result) old.resolve(result); else old.reject(new Error('old poll failure'));
    await poll;
    assert.deepEqual(h.snapshot(), before, 'stale poll changed the new modal');
    assert.equal(h.calls.length, callCount, 'stale poll triggered a refresh');
    for(const timer of h.timeouts) timer.fn();
    assert.ok(dom.byId('codeartsOauthModal').classList.contains('show'), 'stale poll closed the new modal');
    oldInterval();
    await settle();
    assert.equal(h.calls.length, callCount, 'old interval polled the new session');
  });
}

test('late callback cannot clear new input, replace its URL, or re-enable its busy button', async () => {
  const h = harness();
  await h.open('codearts-intl');
  dom.byId('codeartsOauthCallback').value = 'http://127.0.0.1/oauth/callback?code=old';
  const callback = h.page.importCodeartsCallback(h.buttons[0]);
  const old = h.latest('/oauth/callback');
  h.page.closeCodeartsOauthModal();
  await h.open('codearts');
  dom.byId('codeartsOauthCallback').value = 'new callback draft';
  h.buttons[0].disabled = true; // a request in the new modal owns this button
  const before = h.snapshot();
  old.resolve({next_url: 'https://example.test/old-next'});
  await callback;
  assert.deepEqual(h.snapshot(), before);
  assert.equal(h.buttons[0].disabled, true);
  assert.equal(h.timeouts.length, 0, 'stale callback queued another poll');
});

test('callback delayed poll is bound to the old open, not the new session', async () => {
  const h = harness();
  await h.open('codearts-intl');
  dom.byId('codeartsOauthCallback').value = 'http://127.0.0.1/oauth/callback?code=x';
  const callback = h.page.importCodeartsCallback();
  h.latest('/oauth/callback').resolve({message: 'received'});
  await callback;
  const timer = h.timeouts.find(t => t.ms === 500);
  assert.ok(timer, 'callback must schedule a poll');
  h.page.closeCodeartsOauthModal();
  await h.open('codearts');
  const before = h.calls.length;
  timer.fn();
  await settle();
  assert.equal(h.calls.length, before, 'old callback polled a new modal session');
});

for(const ok of [true, false]){
  test('late clipboard ' + (ok ? 'success' : 'failure') + ' cannot update a reopened modal', async () => {
    const h = harness(), toasts = [];
    await h.open('codearts-intl');
    let resolveCopy, rejectCopy;
    h.page.writeClipboard = () => new Promise((resolve, reject) => { resolveCopy = resolve; rejectCopy = reject; });
    h.page.toast = (...args) => toasts.push(args);
    const copying = h.page.copyCodeartsAuthUrl();
    h.page.closeCodeartsOauthModal();
    await h.open('codearts');
    const before = h.snapshot();
    if(ok) resolveCopy(); else rejectCopy(new Error('old clipboard failure'));
    await copying;
    assert.deepEqual(h.snapshot(), before);
    assert.equal(toasts.length, 0, 'old clipboard result produced a toast');
  });
}

for(const ok of [true, false]){
  test('late session import ' + (ok ? 'success' : 'failure') + ' cannot affect a reopened modal', async () => {
    const h = harness();
    await h.open('codearts-intl');
    dom.byId('codeartsSessionJson').value = '{"account":{"id":"old"}}';
    const importing = h.page.importCodeartsSessionJson(h.buttons[1]);
    const old = h.latest('/accounts/import');
    h.page.closeCodeartsOauthModal();
    assert.equal(h.buttons[1].disabled, false, 'close must release the old busy button');
    await h.open('codearts');
    dom.byId('codeartsSessionJson').value = 'new secret draft';
    h.buttons[1].disabled = true;
    const before = h.snapshot(), callCount = h.calls.length;
    old.resolve(ok ? {ok: true, uid: 'old'} : {error: {message: 'old import failure'}}, ok ? 200 : 400);
    await importing;
    assert.deepEqual(h.snapshot(), before);
    assert.equal(h.buttons[1].disabled, true);
    assert.equal(h.calls.length, callCount, 'stale import refreshed realm data');
    assert.equal(h.timeouts.length, 0, 'stale import queued modal close');
  });
}

for(const kind of ['poll', 'import']){
  test(kind + ' completion refresh uses captured realm and cannot close after reopen during refresh', async () => {
    const h = harness();
    await h.open('codearts-intl');
    h.page.VIEW_REALM = 'codearts';
    h.delayOverview();
    let completing;
    if(kind === 'poll'){
      completing = h.page.pollCodeartsOAuth(false);
      h.latest('/oauth/poll').resolve({status: 'done', ok: true});
    }else{
      dom.byId('codeartsSessionJson').value = '{"account":{"id":"old"}}';
      completing = h.page.importCodeartsSessionJson();
      h.latest('/accounts/import').resolve({ok: true, uid: 'old'});
    }
    await settle();
    const overview = h.latest('/codearts-intl/overview?force=1');
    assert.ok(overview, 'completion must refresh its captured international realm');
    assert.ok(!h.calls.some(c => c.url.startsWith('/codearts/overview')), 'must not refresh the mutable view realm');
    h.page.closeCodeartsOauthModal();
    await h.open('codearts');
    overview.resolve({stats: {}, accounts: [], models: []});
    await completing;
    assert.equal(h.timeouts.length, 0, 'completion scheduled a close after its context expired');
  });

  test(kind + ' delayed close cannot close a reopened modal', async () => {
    const h = harness();
    await h.open('codearts-intl');
    let completing;
    if(kind === 'poll'){
      completing = h.page.pollCodeartsOAuth(false);
      h.latest('/oauth/poll').resolve({status: 'done', ok: true});
    }else{
      dom.byId('codeartsSessionJson').value = '{"account":{"id":"old"}}';
      completing = h.page.importCodeartsSessionJson();
      h.latest('/accounts/import').resolve({ok: true, uid: 'old'});
    }
    await completing;
    const timer = h.timeouts.find(t => t.ms === 900);
    assert.ok(timer, 'successful completion must schedule close');
    h.page.closeCodeartsOauthModal();
    await h.open('codearts-intl', 'new-same-realm');
    dom.byId('codeartsSessionJson').value = 'new draft';
    timer.fn();
    assert.ok(dom.byId('codeartsOauthModal').classList.contains('show'));
    assert.equal(dom.byId('codeartsSessionJson').value, 'new draft');
  });
}

(async () => {
  let failures = 0;
  for(const {name, run} of cases){
    try { await run(); }
    catch(error){ failures++; console.error(name + ': ' + error.message); }
  }
  if(failures) throw new Error(failures + '/' + cases.length + ' OAuth lifecycle cases failed');
  console.log('codearts oauth lifecycle assertions passed: ' + cases.length + '/' + cases.length);
})().catch(error => { console.error(error); process.exit(1); });
