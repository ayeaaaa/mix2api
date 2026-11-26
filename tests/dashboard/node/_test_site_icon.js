/* 默认品牌图标：空 site_icon 时侧栏、标签页和设置预览都使用内置 C 图标；
 * 自定义图标仍然覆盖默认值，恢复默认后回到内置图标。
 *
 * Run with Node: node tests/dashboard/node/_test_site_icon.js
 */
'use strict';
const assert = require('assert');
const {dashboardHtml, dashboardScript} = require('./_dashboard_source.js');
const dom = require('./_dom_stub.js');

const html = dashboardHtml();
assert.ok(html.includes('<link id="siteFavicon" rel="icon" type="image/svg+xml" href="/favicon.svg">'),
  'first-paint head must point at the default SVG favicon');
assert.ok(html.includes('<div class="next-brand-mark"><img src="/favicon.svg" alt="" decoding="async"></div>'),
  'first-paint sidebar must show the default SVG icon');

const script = dashboardScript();
const mark = dom.makeElement('div');
dom.installDom({
  querySelector: {'#nextSidebar .next-brand-mark': mark},
  fetch: () => Promise.resolve({status: 200, ok: true,
    json: () => Promise.resolve({}), text: () => Promise.resolve('{}')}),
});

const api = new Function(script + `
  return { applyBranding: applyBranding, iconValue: () => SITE_ICON_VALUE };
`)();
const lastIcon = () => mark.children[mark.children.length - 1];

api.applyBranding({site_title: '', site_icon: ''});
assert.equal(dom.byId('siteFavicon').href, '/favicon.svg',
  'empty site_icon should use the default favicon');
assert.equal(lastIcon().src, '/favicon.svg',
  'empty site_icon should use the default sidebar mark');
assert.equal(dom.byId('setSiteIconPreview').src, '/favicon.svg',
  'settings preview should show the default icon');
assert.equal(dom.byId('setSiteIconState').textContent, '默认图标',
  'settings should name the built-in icon');
assert.equal(api.iconValue(), '',
  'built-in icon must not be persisted as a custom site_icon');

const custom = 'data:image/png;base64,iVBORw0KGgo=';
api.applyBranding({site_title: 'X', site_icon: custom});
assert.equal(dom.byId('siteFavicon').href, custom,
  'custom icon should replace the favicon');
assert.equal(lastIcon().src, custom,
  'custom icon should replace the sidebar mark');
assert.equal(dom.byId('setSiteIconPreview').src, custom,
  'settings preview should show the custom icon');
assert.equal(dom.byId('setSiteIconState').textContent, '已设置图标',
  'settings should identify a custom icon');

api.applyBranding({site_title: '', site_icon: ''});
assert.equal(dom.byId('siteFavicon').href, '/favicon.svg',
  'resetting branding should restore the default favicon');
assert.equal(lastIcon().src, '/favicon.svg',
  'resetting branding should restore the default sidebar mark');

console.log('site icon assertions passed');