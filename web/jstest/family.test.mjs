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
    [`GET ${HP}/quests`]: () => [200, { items: srv.quests, next_offset: null }],
    [`GET ${HP}/combos`]: () => [200, { items: [] }],
    'GET /quest-library': () => [200, { items: [] }],
    [`GET ${HP}/quests/pending`]: () => [200, { items: [] }],
    [`GET ${HP}/reward-redemptions/pending`]: () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
    [`GET ${HP}/quest-schedules`]: (q) => srv.listSchedules(q),
    [`POST ${HP}/quest-schedules`]: (q) => srv.createSchedule(q),
  };
  for (const s of srv.schedules) {
    r[`DELETE ${HP}/quest-schedules/${s.id}`] = (q) => srv.deleteSchedule(q, s);
  }
  for (const m of srv.members) {
    r[`GET ${HP}/family-members/${m.id}/points-history`] = (q) => srv.history(q, m);
    r[`GET ${HP}/family-members/${m.id}/reward-history`] = (q) => srv.rewardHistory(q, m);
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
    rewardHistory: () => [200, { items: [] }],
    adjust: () => err(500, 'unexpected', 'no adjust stub'),
    fixture: withHistory,
    // Scheduled quests: the household's quests for the Quest select, and the
    // household's schedules (every member's; the view filters to its member).
    quests: [],
    schedules: [],
    listSchedules: () => [200, { items: srv.schedules }],
    createSchedule: () => err(500, 'unexpected', 'no schedule stub'),
    deleteSchedule: (_, s) => { srv.schedules = srv.schedules.filter((x) => x !== s); return [204, null]; },
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
  // Heading, then Back, then scheduling, then the points history, then the reward history.
  const order = walk(env.$app).filter((n) => (n.tagName === 'H3' && n.textContent.includes('points')) || n.tagName === 'H4' || n === back[0]);
  assert.deepEqual(order.map((n) => n.tagName), ['H3', 'BUTTON', 'H4', 'H4', 'H4', 'H4']);
  assert.deepEqual(order.slice(2).map((n) => n.textContent), ['Schedule a quest', 'Scheduled', 'Points history', 'Reward history']);

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

// ── Reward history in the member view ───────────────────────────────────────

const rewardHistoryCalls = (id) => calls.filter((c) => c.method === 'GET' && c.path === `${HP}/family-members/${id}/reward-history`);
const movieNight = { id: 'rr1', member_id: 'm-maya', reward_id: 'r-movie', description: 'Movie night', cost: 100, status: 'approved', requested_at: '2025-11-17T12:00:00Z', decided_at: '2025-11-18T12:00:00Z' };

test('reward history CLM-8/CLM-9: History reads reward-history with X-Admin-PIN and shows Date/Reward/Points beside the points history', async () => {
  srv.adminPinSet = true;
  srv.rewardHistory = (q) => (q.headers['X-Admin-PIN'] === '9999' ? [200, { items: [movieNight] }] : err(403, 'wrong_pin'));
  await loadApp();
  await openFamily();
  await unlock('9999');
  await click(buttons(env.$app, 'History')[0]);
  const reads = rewardHistoryCalls('m-maya');
  assert.equal(reads.length, 1, 'reward-history read once');
  assert.equal(reads[0].headers['X-Admin-PIN'], '9999');
  assert.equal(reads[0].headers['X-Member-PIN'], undefined);

  assert.ok(sectionNode(env.$app, 'Points history'), 'points history still shown');
  const node = sectionNode(env.$app, 'Reward history');
  assert.ok(node, 'Reward history section exists');
  assert.deepEqual(walk(node).filter((n) => n.tagName === 'TH').map((n) => n.textContent), ['Date', 'Reward', 'Points']);
  const tds = walk(node).filter((n) => n.tagName === 'TD');
  assert.deepEqual(tds.slice(1).map((td) => td.textContent), ['Movie night', '100']);
  assert.ok(tds[0].textContent.length > 0, 'date shown');
  for (const td of tds) assert.equal(td.children.length, 0, 'cells hold text only');
});

test('reward history CLM-10: no redeemed rewards shows the empty text and no table in the section', async () => {
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  const node = sectionNode(env.$app, 'Reward history');
  assert.deepEqual(walk(node).filter((n) => n.tagName === 'P').map((n) => n.textContent), ['No rewards redeemed yet']);
  assert.equal(walk(node).filter((n) => n.tagName === 'TABLE').length, 0, 'no table');
});

test('reward history CLM-8: an answer that arrives after Back is not rendered over the family list', async () => {
  srv.rewardHistory = () => [200, { items: [movieNight] }];
  await loadApp();
  await openFamily();
  const realFetch = globalThis.fetch;
  let release;
  globalThis.fetch = async (url, init) => {
    if (url.endsWith('/reward-history')) await new Promise((r) => { release = r; });
    return realFetch(url, init);
  };
  const opening = click(buttons(env.$app, 'History')[0]);
  await settle();
  assert.match(sectionNode(env.$app, 'Reward history').textContent, /Loading reward history/);
  await click(buttons(env.$app, 'Back to family')[0]);
  release();
  await opening;
  await settle();
  globalThis.fetch = realFetch;
  assert.deepEqual(h3s(), ['Family']);
  assert.equal(sectionNode(env.$app, 'Reward history'), null, 'stale reward history not rendered');
  assert.doesNotMatch(text(), /Movie night/);
});

test('reward history CLM-8: a wrong_pin on the reward-history read locks the tab again', async () => {
  srv.adminPinSet = true;
  await loadApp();
  await openFamily();
  await unlock('9999');
  srv.rewardHistory = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await click(buttons(env.$app, 'History')[0]);
  assert.match(text(), /Enter the admin PIN to manage the family\./);
  assert.equal(buttons(env.$app, 'History').length, 0);
  assert.equal(sectionNode(env.$app, 'Reward history'), null);
});

test('reward history CLM-8: a failing reward-history read shows an inline error and keeps the points history', async () => {
  srv.rewardHistory = () => err(429, 'rate_limited', 'Too many requests.');
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  assert.deepEqual(h3s(), ['Maya — 42 points']);
  assert.match(sectionNode(env.$app, 'Reward history').textContent, /Could not load reward history/);
  assert.equal(walk(sectionNode(env.$app, 'Points history')).filter((n) => n.tagName === 'TABLE').length, 1);
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
  assert.deepEqual(order.map((n) => n.textContent), ['Back to family', 'Adjust points', 'Schedule a quest', 'Scheduled', 'Points history', 'Reward history']);
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

// ── Scheduled quests (CLM-7) ─────────────────────────────────────────────────

const QUESTS = [{ id: 'q-cat', name: 'Feed the cat' }, { id: 'q-room', name: 'Clean your room' }];
const schedule = (id, questId, questName, memberId, date) => ({
  id, household_id: H, quest_id: questId, quest_name: questName, member_id: memberId, member_name: 'x', start_date: date,
});
const scheduleForm = () => sectionNode(env.$app, 'Schedule a quest').children.find((n) => n.tagName === 'FORM');
const scheduledText = () => sectionNode(env.$app, 'Scheduled').textContent;
const openMayaWithPin = async () => {
  srv.adminPinSet = true;
  await loadApp();
  await openFamily();
  await unlock('9999');
  await click(buttons(env.$app, 'History')[0]);
};

test('scheduled quests CLM-7: Schedule POSTs {quest_id, member_id, date} with X-Admin-PIN and the view shows the new schedule', async () => {
  srv.quests = QUESTS;
  srv.createSchedule = (q) => {
    const s = schedule('s-new', q.body.quest_id, 'Clean your room', q.body.member_id, q.body.date);
    srv.schedules = [...srv.schedules, s];
    return [201, s];
  };
  await openMayaWithPin();
  const form = scheduleForm();
  const $select = walk(form).find((n) => n.tagName === 'SELECT');
  assert.deepEqual(walk($select).filter((n) => n.tagName === 'OPTION').map((o) => o.textContent), ['Feed the cat', 'Clean your room']);
  $select.value = 'q-room';
  walk(form).find((n) => n.tagName === 'INPUT' && n.type === 'date').value = '2026-11-21';
  assert.equal(buttons(form, 'Schedule').length, 1);
  assert.equal(buttons(form, 'Schedule')[0].className, 'btn btn-primary btn-sm');
  await submit(form);
  const post = calls.find((c) => c.method === 'POST' && c.path === `${HP}/quest-schedules`);
  assert.deepEqual(post.body, { quest_id: 'q-room', member_id: 'm-maya', date: '2026-11-21' });
  assert.equal(post.headers['X-Admin-PIN'], '9999');
  assert.match(scheduledText(), /Clean your room — from 2026-11-21/);
});

test('scheduled quests CLM-7: a refused date shows the server message on the form and keeps the view', async () => {
  srv.quests = QUESTS;
  srv.createSchedule = () => [422, { error: { code: 'validation_failed', message: 'One or more fields are invalid.', fields: { date: 'Date must be today or later.' } } }];
  await openMayaWithPin();
  await submit(scheduleForm());
  const alert = walk(scheduleForm()).find((n) => n.getAttribute('role') === 'alert');
  assert.equal(alert.textContent, 'Date must be today or later.');
  assert.deepEqual(h3s(), ['Maya — 42 points']);
});

test('scheduled quests CLM-7: the list shows only this member\'s schedules; Remove confirms, sends DELETE and the empty line follows', async () => {
  srv.schedules = [
    schedule('s1', 'q-cat', 'Feed the cat', 'm-maya', '2026-11-22'),
    schedule('s2', 'q-room', 'Clean your room', 'm-other', '2026-11-23'),
  ];
  await openMayaWithPin();
  assert.match(scheduledText(), /Feed the cat — from 2026-11-22/);
  assert.doesNotMatch(scheduledText(), /Clean your room/, 'another member\'s schedule is not listed');
  const remove = buttons(sectionNode(env.$app, 'Scheduled'), 'Remove');
  assert.equal(remove.length, 1);
  assert.equal(remove[0].className, 'btn btn-ghost btn-xs');
  await click(remove[0]);
  assert.equal(remove[0].textContent, 'Sure?');
  assert.equal(calls.filter((c) => c.method === 'DELETE').length, 0, 'first click only arms');
  await click(remove[0]);
  const del = calls.filter((c) => c.method === 'DELETE');
  assert.deepEqual(del.map((c) => c.path), [`${HP}/quest-schedules/s1`]);
  assert.equal(del[0].headers['X-Admin-PIN'], '9999');
  assert.match(scheduledText(), /No quests are scheduled\./);
});

test('scheduled quests CLM-7: with no schedules the Scheduled section says so', async () => {
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  const empty = walk(sectionNode(env.$app, 'Scheduled')).find((n) => n.tagName === 'P');
  assert.equal(empty.textContent, 'No quests are scheduled.');
  assert.equal(empty.className, 'text-xs opacity-60');
});

test('scheduled quests CLM-7: a failed schedules read shows an inline error, not an empty list', async () => {
  srv.listSchedules = () => err(500, 'internal', 'boom');
  await loadApp();
  await openFamily();
  await click(buttons(env.$app, 'History')[0]);
  assert.match(text(), /Could not load scheduled quests\. Reload to try again\./);
  assert.doesNotMatch(text(), /No quests are scheduled\./);
});

test('scheduled quests CLM-7: a wrong_pin on Schedule locks the tab again', async () => {
  srv.quests = QUESTS;
  srv.createSchedule = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await openMayaWithPin();
  await submit(scheduleForm());
  assert.match(text(), /Enter the admin PIN to manage the family\./);
});

test('scheduled quests CLM-7: a wrong_pin on Remove locks the tab again', async () => {
  srv.schedules = [schedule('s1', 'q-cat', 'Feed the cat', 'm-maya', '2026-11-22')];
  srv.deleteSchedule = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await openMayaWithPin();
  const remove = buttons(sectionNode(env.$app, 'Scheduled'), 'Remove')[0];
  await click(remove);
  await click(remove);
  assert.match(text(), /Enter the admin PIN to manage the family\./);
});
