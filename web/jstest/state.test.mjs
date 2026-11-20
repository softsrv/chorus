// Client state lives in one object in state.js, and boot() resets it first.
// loadApp re-imports only app.js; state.js and the section modules stay
// cached, so a second load must still start clean: the administrator's view,
// the Quests tab, no member and no PIN held.
// Run: make test-js

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import {
  installBrowser, installFetch, loadApp, walk, buttons, buttonStarting, click, submit,
} from './harness.mjs';

const H = 'h1';
const HP = `/households/${H}`;

let env;
let calls;

const err = (status, code) => [status, { error: { code, message: code } }];

const routes = {
  'POST /auth/refresh': () => [200, {
    access_token: 'at', refresh_token: 'rt',
    user: { id: 'u1', email: 'p@example.com', display_name: 'Parent', email_verified: true },
  }],
  'GET /households': () => [200, { items: [{ id: H, name: 'Home', timezone: 'UTC', role: 'admin' }] }],
  [`GET ${HP}/admin-pin`]: () => [200, { set: true }],
  [`POST ${HP}/admin-pin/verify`]: (r) => (r.body.pin === '9999' ? [204, null] : err(403, 'wrong_pin')),
  [`GET ${HP}/quests`]: () => [200, { items: [], next_offset: null }],
  [`GET ${HP}/combos`]: () => [200, { items: [] }],
  'GET /quest-library': () => [200, { items: [] }],
  [`GET ${HP}/rewards`]: () => [200, { items: [] }],
  'GET /reward-library': () => [200, { items: [] }],
  [`GET ${HP}/family-members`]: () => [200, { items: [
    { id: 'm-a', household_id: H, name: 'Ann', has_pin: false, points: 0 },
    { id: 'm-b', household_id: H, name: 'Ben', has_pin: true, points: 30 },
  ] }],
  [`POST ${HP}/family-members/m-a/verify`]: () => [204, null],
  [`POST ${HP}/family-members/m-b/verify`]: (r) => (r.body.pin === '1234' ? [204, null] : err(403, 'wrong_pin')),
  [`GET ${HP}/family-members/m-a/points-history`]: () => [200, { items: [] }],
  [`GET ${HP}/family-members/m-a/reward-history`]: () => [200, { items: [] }],
  [`GET ${HP}/family-members/m-b/points-history`]: () => [200, { items: [] }],
  [`GET ${HP}/family-members/m-b/reward-history`]: () => [200, { items: [] }],
  [`GET ${HP}/family-members/m-b/reward-redemptions/pending`]: () => [200, { items: [] }],
  [`GET ${HP}/quests/pending`]: () => [200, { items: [] }],
  [`GET ${HP}/reward-redemptions/pending`]: () => [200, { items: [] }],
};

beforeEach(() => {
  env = installBrowser();
  sessionStorage.setItem('chorus.refresh_token', 'rt');
  calls = installFetch(routes);
});

const text = () => env.$app.textContent;
const activeTab = () => walk(env.$app).find((n) => n.tagName === 'A' && n.attributes.role === 'tab' && n.className.includes('tab-active'))?.textContent;
const enterPin = async (pin) => {
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = pin;
  await submit($pin.parentNode.parentNode);
};

test('CLM-7: a re-loaded app starts clean: no member, no PIN, the Quests tab, though sibling modules stay cached', async () => {
  // First load: unlock admin with the PIN, become Ben with his PIN, open Rewards.
  await loadApp();
  assert.match(text(), /Viewing as: Administrator/);
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ben'));
  await enterPin('1234');
  assert.match(text(), /Viewing as: Ben/);
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Rewards'));
  assert.equal(activeTab(), 'Rewards');

  // Second load of app.js, in a fresh page.
  env = installBrowser();
  sessionStorage.setItem('chorus.refresh_token', 'rt');
  calls = installFetch(routes);
  await loadApp();
  assert.match(text(), /Viewing as: Administrator/, 'no member carried over');
  assert.doesNotMatch(text(), /Ben ·|Quests for Ben/);
  assert.equal(activeTab(), 'Quests', 'the tab starts at Quests');
  // The admin PIN set earlier is not held: management asks for it again and
  // no request carries a member or admin PIN.
  assert.match(text(), /Enter the admin PIN to review approvals\./);
  for (const c of calls) {
    assert.equal(c.headers?.['X-Admin-PIN'], undefined, `${c.method} ${c.path} sent an admin PIN`);
    assert.equal(c.headers?.['X-Member-PIN'], undefined, `${c.method} ${c.path} sent a member PIN`);
  }
});

test('CLM-7: a member picked after a re-load is the only one shown; the previous member\'s selection does not leak', async () => {
  await loadApp();
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ann'));
  assert.match(text(), /Quests for Ann/);

  env = installBrowser();
  sessionStorage.setItem('chorus.refresh_token', 'rt');
  calls = installFetch(routes);
  await loadApp();
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ben'));
  await enterPin('1234');
  assert.match(text(), /Quests for Ben/);
  assert.doesNotMatch(text(), /Quests for Ann/);
  // Ben's history is read for Ben, with Ben's PIN, after the re-load.
  const reads = calls.filter((c) => c.path.endsWith('/points-history'));
  assert.deepEqual(reads.map((c) => [c.path, c.headers['X-Member-PIN']]), [[`${HP}/family-members/m-b/points-history`, '1234']]);
});
