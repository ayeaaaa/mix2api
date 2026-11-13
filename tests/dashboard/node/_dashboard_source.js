/* The embedded dashboard page, and the source of the inline scripts it ships.
 *
 * The Go harness writes the byte-for-byte embedded asset to a temporary file
 * and points DASHBOARD_HTML at it. Keeping the suites on the same extraction
 * rule means they exercise exactly the bytes the binary serves; no Node
 * runtime or build step is involved in producing the asset.
 *
 *   const {dashboardScript} = require('./_dashboard_source.js');
 */
'use strict';

const fs = require('fs');
const path = require('path');

const HTML_PATH = process.env.DASHBOARD_HTML
  || path.join(__dirname, '..', '..', '..', 'internal', 'web', 'dashboard.html');

// The shipped page, so a suite always runs the production asset.
function dashboardHtml() {
  return fs.readFileSync(HTML_PATH, 'utf8');
}

// Its inline scripts, joined in document order - what a suite evaluates.
function dashboardScript() {
  return [...dashboardHtml().matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)]
    .map(match => match[1]).join('\n');
}

module.exports = {dashboardHtml, dashboardScript, HTML_PATH};
