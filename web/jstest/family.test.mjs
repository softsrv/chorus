// Unit tests for the Family tab's admin points history and admin points
// adjustments (task 1 CLM-1..3, CLM-5; task 2 CLM-4..6, CLM-8).
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
import * as adminAdjustment from './fixtures/family-member-admin-adjustment.fixture.mjs';
import * as overRemoval from './fixtures/family-member-over-removal.fixture.mjs';

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
    [`GET ${HP}/reward-redemptions/pending`]: () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
  };
  for (const m of srv.members) {
    r[`GET ${HP}/family-members/${m.id}/points-history`] = (q) => srv.history(q, m);
    r[`POST ${HP}/family-members/${m.id}/points-adjustments`] = (q) => srv.adjust(q, m);
    r[`POST ${HP}/family-members/${m.id}/verify`] = (q) => ((q.body.pin || '') === '' && !m.has_pin ? [204, null] : err(403, 'wrong_pin'));
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
    adjust: () => err(500, 'unexpected', 'no adjust stub'),
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

// ── Admin points adjustments ────────────────────────────────────────────────

const adjustForm = () => walk(env.$app).find((n) => n.tagName === 'FORM'
  && walk(n).some((c) => c.tagName === 'LEGEND' && c.textContent === 'Adjust points'));
const field = (name, value) => walk(adjustForm()).find((n) => n.tagName === 'INPUT' && n.name === name && (value === undefined || n.value === value));
const fill = ({ points, dir = 'add', reason = '' }) => {
  field('points').value = String(points);
  // The harness DOM does not toggle radio groups on click: set them as a browser would.
  field('direction', 'add').checked = dir === 'add';
  field('direction', 'remove').checked = dir === 'remove';
  field('reason').value = reason;
};
const historyRows = () => {
  const node = sectionNode(env.$app, 'Points history');
  return walk(node).filter((n) => n.tagName === 'TBODY').flatMap((b) => walk(b).filter((n) => n.tagName === 'TR'))
    .map((tr) => walk(tr).filter((n) => n.tagName === 'TD').map((td) => td.textContent));
};
const adjustCalls = (id) => calls.filter((c) => c.method === 'POST' && c.path === `${HP}/family-members/${id}/points-adjustments`);

// A stateful stand-in for the adjust endpoint: it applies the signed amount to
// the member and puts an admin row on top of the history, as the server does.
const applyingServer = (fixture) => {
  const members = fixture.members.map((m) => ({ ...m }));
  const history = Object.fromEntries(Object.entries(fixture.pointsHistory).map(([k, v]) => [k, [...v]]));
  srv.members = members;
  srv.fixture = { members, pointsHistory: history };
  srv.adjust = (q, m) => {
    const live = members.find((x) => x.id === m.id);
    live.points += q.body.points;
    history[m.id].unshift({
      id: `pa-new-${history[m.id].length}`, kind: 'admin', points: q.body.points,
      reason: q.body.reason || 'Adjusted by admin', earned_at: '2025-02-01T12:00:00Z', created_at: '2025-02-01T12:00:00Z',
    });
    return [200, { ...live }];
  };
};

test('admin points CLM-4: the member view has the heading, Back above the adjust form, and Points history; Back returns to the list', async () => {
  use(adminAdjustment);
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  assert.deepEqual(h3s(), ['Maya — 42 points']);
  const back = buttons(env.$app, 'Back to family')[0];
  const order = walk(env.$app).filter((n) => n === back || n.tagName === 'LEGEND' || n.tagName === 'H4');
  assert.deepEqual(order.map((n) => n.textContent), ['Back to family', 'Adjust points', 'Points history']);
  await click(back);
  assert.deepEqual(h3s(), ['Family']);
  assert.equal(adjustForm(), undefined, 'no adjust form on the family list');
});

test('admin points CLM-6: history rows keep API order with signed amounts; only admin rows carry (admin adjustment)', async () => {
  use(adminAdjustment);
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  assert.deepEqual(historyRows().map((r) => [r[1], r[2]]), [
    ['+10', 'helped grandma (admin adjustment)'],
    ['-5', 'left toys out (admin adjustment)'],
    ['+5', 'Quest: Feed the cat'],
    ['+3', 'Combo bonus'],
  ]);
  assert.ok(!sectionNode(env.$app, 'Points history').textContent.includes('(from admin)'), 'admin view uses its own label');
});

test('admin points CLM-5: the adjust form has Points, Add (default) / Remove, an optional Reason (max 200) and Save', async () => {
  use(adminAdjustment);
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  const $form = adjustForm();
  assert.ok($form, 'Adjust points fieldset');
  assert.equal(walk($form).filter((n) => n.tagName === 'FIELDSET').length, 1);
  assert.equal(field('points').type, 'number');
  assert.equal(field('direction', 'add').checked, true, 'Add is the default');
  assert.equal(field('direction', 'remove').checked, false);
  assert.equal(field('reason').getAttribute('maxlength'), '200');
  assert.equal(field('reason').required, false, 'reason is optional');
  const labels = walk($form).filter((n) => n.tagName === 'SPAN').map((n) => n.textContent);
  assert.deepEqual(labels, ['Points', 'Add', 'Remove', 'Reason']);
  assert.equal(buttons($form, 'Save').length, 1);
  assert.equal(buttons($form, 'Save')[0].className, 'btn btn-primary btn-sm');
});

test('admin points CLM-5/CLM-4/CLM-6: Add 10 POSTs +10 with the reason and X-Admin-PIN, then the heading and history show the change', async () => {
  srv.adminPinSet = true;
  applyingServer(withHistory);
  await loadApp();
  await openFamily();
  await unlock('9999');
  await click(buttons(env.$app, 'History')[0]);
  fill({ points: 10, dir: 'add', reason: 'helped grandma' });
  await submit(adjustForm());
  const [call] = adjustCalls('m-maya');
  assert.deepEqual(call.body, { points: 10, reason: 'helped grandma' });
  assert.equal(call.headers['X-Admin-PIN'], '9999');
  assert.deepEqual(h3s(), ['Maya — 52 points'], 'heading total refreshed');
  assert.deepEqual(historyRows()[0].slice(1), ['+10', 'helped grandma (admin adjustment)'], 'new entry on top');
});

test('admin points CLM-5: Remove 5 POSTs -5 and the history shows -5; a blank reason is sent as blank', async () => {
  applyingServer(withHistory);
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  fill({ points: 5, dir: 'remove' });
  await submit(adjustForm());
  const [call] = adjustCalls('m-maya');
  assert.deepEqual(call.body, { points: -5, reason: '' });
  assert.equal(call.headers['X-Admin-PIN'], undefined, 'no admin PIN set, none sent');
  assert.deepEqual(h3s(), ['Maya — 37 points']);
  assert.deepEqual(historyRows()[0].slice(1), ['-5', 'Adjusted by admin (admin adjustment)']);
});

test('admin points CLM-5: a zero or non-whole amount is refused on the form and nothing is sent', async () => {
  use(adminAdjustment);
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  for (const points of ['0', '2.5', '']) {
    fill({ points });
    await submit(adjustForm());
    const alert = walk(adjustForm()).find((n) => n.getAttribute('role') === 'alert');
    assert.ok(alert, `alert for ${JSON.stringify(points)}`);
  }
  assert.equal(adjustCalls('m-maya').length, 0);
});

test('admin points CLM-8: a 422 on points shows inline between Remove and Reason, keeps the values, and changes nothing', async () => {
  use(overRemoval);
  srv.adjust = () => [422, overRemoval.overRemovalError];
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  const before = historyRows();
  fill({ points: 5, dir: 'remove' });
  await submit(adjustForm());
  assert.equal(adjustCalls('m-leo').length, 1);
  const $form = adjustForm();
  const alert = walk($form).find((n) => n.getAttribute('role') === 'alert');
  assert.ok(alert, 'inline alert');
  assert.equal(alert.textContent, "Leo only has 3 points — you can't remove 5.");
  const fieldset = walk($form).find((n) => n.tagName === 'FIELDSET');
  assert.ok(walk(fieldset).includes(alert), 'alert is inside the fieldset');
  const order = walk(fieldset).filter((n) => n === alert || n === field('direction', 'remove') || n === field('reason'));
  assert.deepEqual(order, [field('direction', 'remove'), alert, field('reason')]);
  assert.equal(field('points').value, '5', 'entered amount kept');
  assert.equal(field('direction', 'remove').checked, true, 'direction kept');
  assert.deepEqual(h3s(), ['Leo — 3 points']);
  assert.deepEqual(historyRows(), before);
});

test('admin points CLM-8: a 422 on reason shows its error inline the same way', async () => {
  use(adminAdjustment);
  srv.adjust = () => [422, { error: { code: 'validation_error', message: 'Check the highlighted field.', fields: { reason: 'must be at most 200 characters' } } }];
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  fill({ points: 1, reason: 'x' });
  await submit(adjustForm());
  const alert = walk(adjustForm()).find((n) => n.getAttribute('role') === 'alert');
  assert.equal(alert.textContent, 'must be at most 200 characters');
  assert.equal(field('reason').value, 'x');
  assert.deepEqual(h3s(), ['Maya — 42 points']);
});

test('admin points: a wrong_pin on save locks the tab again', async () => {
  srv.adminPinSet = true;
  use(adminAdjustment);
  srv.adjust = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await loadApp();
  await openFamily();
  await unlock('9999');
  await click(buttons(env.$app, 'History')[0]);
  fill({ points: 1 });
  await submit(adjustForm());
  assert.match(text(), /Enter the admin PIN to manage the family\./);
});

test('admin points 2.1/7: after an adjustment to the active member, their quest card history, header and picker show the new total', async () => {
  applyingServer(noHistory);
  await loadApp();
  const startingWith = (prefix) => walk(env.$app).find((n) => n.tagName === 'BUTTON' && n.textContent.startsWith(prefix));
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(startingWith('Leo'));
  await settle();
  assert.match(text(), /Viewing as: Leo · 0 points/);
  assert.match(sectionNode(env.$app, 'Points history').textContent, /No points yet/);
  assert.equal(historyCalls('m-leo').length, 1, 'the card read the history once');

  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  fill({ points: 7, dir: 'add', reason: 'helped grandma' });
  await submit(adjustForm());
  await settle();
  assert.deepEqual(h3s(), ['Leo — 7 points']);
  assert.match(text(), /Viewing as: Leo · 7 points/, 'member bar total re-read after the save');

  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));
  await settle();
  assert.deepEqual(historyRows().map((r) => [r[1], r[2]]), [['+7', 'helped grandma (from admin)']]);
  assert.match(text(), /Viewing as: Leo · 7 points/);
  await click(buttons(env.$app, 'Switch member')[0]);
  assert.ok(startingWith('Leo · 7 points'), 'picker shows the new total');
});
