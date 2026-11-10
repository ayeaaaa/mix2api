#!/usr/bin/env node
/* Run the selected dashboard JS suites against the embedded HTML.
 *
 * The Go harness invokes this file after writing the embedded asset to a
 * temporary path (DASHBOARD_HTML). Each suite runs in its own process because
 * several of them install a global DOM stub and mutate module-level state.
 *
 * Usage: DASHBOARD_HTML=/tmp/dashboard.html node tests/dashboard/node/run.js
 */
'use strict';

const {spawnSync} = require('node:child_process');
const path = require('node:path');

const SUITES = [
  '_test_page_nav.js',
  '_test_panel_gate_first_paint.js',
  '_test_accounts_realm_switch.js',
  '_test_settings_load.js',
  '_test_daily_chat_pending.js',
  '_test_settings_update_ui.js',
  '_test_update_rollback_ui.js',
];

if (!process.env.DASHBOARD_HTML) {
  console.error('DASHBOARD_HTML is required');
  process.exit(2);
}

let failed = 0;
for (const suite of SUITES) {
  const result = spawnSync(process.execPath, [path.join(__dirname, suite)], {
    stdio: 'inherit',
    env: process.env,
  });
  if (result.error) {
    console.error(suite + ': ' + result.error.message);
    failed += 1;
  } else if (result.status !== 0) {
    failed += 1;
  }
}

if (failed) {
  console.error('dashboard JS suites failed: ' + failed + '/' + SUITES.length);
  process.exit(1);
}
console.log('dashboard JS suites passed: ' + SUITES.length + '/' + SUITES.length);
