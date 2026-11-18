// Unit tests for the Family tab's admin points history (admin points CLM-1..3, CLM-5).
// Run: make test-js
//
// app.js and api.js run as shipped against a minimal DOM; only the server
// (fetch) is a stub, answering from the canned fixtures in ./fixtures.

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import {
  installBrowser, installFetch, loadApp, settle, walk, buttons, click, submit, sectionNode,
} from './harness.mjs';
import * as withHistory from './fixtures/family-member-history.fixture.mjs';
import * as noHistory from './fixtures/family-member-no-history.fixture.mjs';

const H = 'h1';
const HP = `/households/${H}`;

let env;
let srv;
let calls;

const err = (status, code, message = code) => [status, { error: { code, message } }];

function routes() {
  const r = {
    'POST /auth/refresh': () => [200, {
      access_token: 'at', refresh_token: 'rt',
      user: { id: 'u1', email: 'p@example.com', display_name: 'Parent', email_verified: true },
    }],
    'GET /households': () => [200, { items: [{ id: H, name: 'Home', timezone: 'UTC', role: 'admin' }] }],
    [`GET ${HP}/admin-pin`]: () => [200, { set: srv.adminPinSet }],
    [`POST ${HP}/admin-pin/verify`]: (q) => (q.body.pin === srv.adminPin ? [204, null] : err(403, 'wrong_pin', 'Wrong PIN.')),
    [`GET ${HP}/quests`]: () => [200, { items: [], next_offset: null }],
    [`GET ${HP}/combos`]: () => [200, { items: [] }],
    'GET /quest-library': () => [200, { items: [] }],
    [`GET ${HP}/quests/pending`]: () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
  };
  for (const m of srv.members) {
    r[`GET ${HP}/family-members/${m.id}/points-history`] = (q) => srv.history(q, m);
  }
  return r;
}

beforeEach(() => {
  env = installBrowser();
  sessionStorage.setItem('chorus.refresh_token', 'rt');
  srv = {
    adminPinSet: false,
    adminPin: '9999',
    members: withHistory.members,
    history: (_, m) => [200, { items: srv.fixture.pointsHistory[m.id] }],
    fixture: withHistory,
  };
  calls = installFetch(new Proxy({}, { get: (_, k) => routes()[k] }));
});

const use = (fixture) => { srv.fixture = fixture; srv.members = fixture.members; };
const text = () => env.$app.textContent;
const openFamily = async () => {
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Family'));
  await settle();
};
const unlock = async (pin) => {
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = pin;
  await submit($pin.parentNode.parentNode);
};
const historyCalls = (id) => calls.filter((c) => c.method === 'GET' && c.path === `${HP}/family-members/${id}/points-history`);
const h3s = () => walk(env.$app).filter((n) => n.tagName === 'H3' && !['New household', 'Admin PIN'].includes(n.textContent)).map((n) => n.textContent);

test('admin points history CLM-1: History opens the member view with heading, Back, and the history table in API order', async () => {
  await loadApp();
  await openFamily();
  assert.equal(buttons(env.$app, 'History').length, 1, 'one History control per member row');

  await click(buttons(env.$app, 'History')[0]);
  assert.deepEqual(h3s(), ['Maya — 42 points']);
  assert.equal(buttons(env.$app, 'History').length, 0, 'family list replaced');
  assert.equal(buttons(env.$app, 'Rename').length, 0, 'family list replaced');
  assert.equal(buttons(env.$app, 'Add member').length, 0, 'family list replaced');
  const back = buttons(env.$app, 'Back to family');
  assert.equal(back.length, 1);

  const historyNode = sectionNode(env.$app, 'Points history');
  assert.ok(historyNode, 'Points history section exists');
  // Heading, then Back, then the history, in that order.
  const order = walk(env.$app).filter((n) => (n.tagName === 'H3' && n.textContent.includes('points')) || n.tagName === 'H4' || n === back[0]);
  assert.deepEqual(order.map((n) => n.tagName), ['H3', 'BUTTON', 'H4']);

  const ths = walk(historyNode).filter((n) => n.tagName === 'TH').map((n) => n.textContent);
  assert.deepEqual(ths, ['Date', 'Points', 'Reason']);
  const trs = walk(historyNode).filter((n) => n.tagName === 'TBODY').flatMap((b) => walk(b).filter((n) => n.tagName === 'TR'));
  const rowData = trs.map((tr) => walk(tr).filter((n) => n.tagName === 'TD').map((td) => td.textContent));
  assert.deepEqual(rowData.map((r) => [r[1], r[2]]), [['+5', 'Quest: Feed the cat'], ['+3', 'Combo bonus']]);
  for (const tr of trs) {
    for (const td of walk(tr).filter((n) => n.tagName === 'TD')) assert.equal(td.children.length, 0, 'cells hold text only');
  }

  await click(back[0]);
  assert.deepEqual(h3s(), ['Family']);
  assert.match(text(), /Maya42 points/);
  assert.equal(buttons(env.$app, 'History').length, 1, 'member list is back');
  assert.equal(sectionNode(env.$app, 'Points history'), null);
});

test('admin points history CLM-1: a history answer that arrives after Back is not rendered over the family list', async () => {
  await loadApp();
  await openFamily();
  const realFetch = globalThis.fetch;
  let release;
  globalThis.fetch = async (url, init) => {
    if (url.endsWith('/points-history')) await new Promise((r) => { release = r; });
    return realFetch(url, init);
  };
  const opening = click(buttons(env.$app, 'History')[0]);
  await settle();
  assert.match(sectionNode(env.$app, 'Points history').textContent, /Loading points history/);
  await click(buttons(env.$app, 'Back to family')[0]);
  release();
  await opening;
  await settle();
  globalThis.fetch = realFetch;
  assert.deepEqual(h3s(), ['Family']);
  assert.equal(sectionNode(env.$app, 'Points history'), null, 'stale history not rendered');
});

test('admin points history CLM-1: a failing history read shows an inline error inside the member view', async () => {
  srv.history = () => err(429, 'rate_limited', 'Too many requests.');
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  assert.deepEqual(h3s(), ['Maya — 42 points']);
  assert.match(sectionNode(env.$app, 'Points history').textContent, /Could not load points history/);
  assert.equal(buttons(env.$app, 'Back to family').length, 1);
});

test('admin points history CLM-2: an empty history shows the empty text and no table', async () => {
  use(noHistory);
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  assert.deepEqual(h3s(), ['Leo — 0 points']);
  const historyNode = sectionNode(env.$app, 'Points history');
  assert.ok(historyNode);
  const ps = walk(historyNode).filter((n) => n.tagName === 'P').map((n) => n.textContent);
  assert.deepEqual(ps, ['No points history yet.']);
  assert.equal(walk(env.$app).filter((n) => n.tagName === 'TABLE').length, 0, 'no table');
});

test('admin points history CLM-3: with no admin PIN set, the history read sends no PIN header', async () => {
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  const [call] = historyCalls('m-maya');
  assert.ok(call, 'points-history was read');
  assert.equal(call.headers['X-Admin-PIN'], undefined);
  assert.equal(call.headers['X-Member-PIN'], undefined);
});

test('admin points history CLM-3: with an admin PIN set, the locked tab has no History control; unlocked, the read sends X-Admin-PIN', async () => {
  srv.adminPinSet = true;
  srv.history = (q, m) => (q.headers['X-Admin-PIN'] === '9999' ? [200, { items: withHistory.pointsHistory[m.id] }] : err(403, 'wrong_pin'));
  await loadApp();
  await openFamily();
  assert.match(text(), /Enter the admin PIN to manage the family\./);
  assert.equal(buttons(env.$app, 'History').length, 0, 'locked tab renders no History control');
  assert.equal(historyCalls('m-maya').length, 0);

  await unlock('9999');
  await click(buttons(env.$app, 'History')[0]);
  const [call] = historyCalls('m-maya');
  assert.equal(call.headers['X-Admin-PIN'], '9999');
  assert.equal(call.headers['X-Member-PIN'], undefined);
  assert.deepEqual(h3s(), ['Maya — 42 points']);
  assert.equal(walk(sectionNode(env.$app, 'Points history')).filter((n) => n.tagName === 'TR').length, 3, 'header row + 2 entries');
});

test('admin points history CLM-3: a wrong_pin on the history read locks the tab again', async () => {
  srv.adminPinSet = true;
  await loadApp();
  await openFamily();
  await unlock('9999');
  srv.history = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await click(buttons(env.$app, 'History')[0]);
  assert.match(text(), /Enter the admin PIN to manage the family\./);
  assert.equal(buttons(env.$app, 'History').length, 0);
});

test('admin points history CLM-5: with no members, the empty text shows with the Add member form and no History or points controls', async () => {
  srv.members = [];
  await loadApp();
  await openFamily();
  assert.match(text(), /No family members yet\. Add one to get started\./);
  assert.equal(buttons(env.$app, 'Add member').length, 1);
  assert.equal(buttons(env.$app, 'History').length, 0);
  assert.equal(sectionNode(env.$app, 'Points history'), null);
  assert.equal(walk(env.$app).filter((n) => n.tagName === 'INPUT' && n.name === 'points').length, 0, 'no points form');
});
