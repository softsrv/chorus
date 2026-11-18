// Unit tests for the Quests tab's take-quest and approval flows (CLM-1..12).
// Run: node --test web/jstest/
//
// app.js and api.js run as shipped against a minimal DOM; only the server
// (fetch) is a stub, answering with the JSON shapes internal/http/handlers
// emits (questJSON, pendingQuestJSON, familyMemberJSON, the error envelope).

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import {
  installBrowser, installFetch, loadApp, settle, walk, buttons, buttonStarting,
  click, submit, section, sectionNode,
} from './harness.mjs';
import {
  MEMBERS, SAM, AVA, LEO, RANGE_QUESTS, FEED_CAT, MOW_LAWN, READ_CHAPTER, WATER_PLANTS, TIDY_ROOM, RAKE_LEAVES,
  BIRTHDAYS, MIN_ABOVE_MAX, RANGE_422, ageQuest,
} from './fixtures/age_range.mjs';
import { readFileSync } from 'node:fs';

const H = 'h1';
const HP = `/households/${H}`;
const REFRESH_KEY = 'chorus.refresh_token';

let env;
let srv; // mutable server state per test
let calls;
let signedOut;

const quest = (over) => ({
  id: 'q1', household_id: H, library_id: null, name: 'Feed the cat', points: 5, repeat_days: 1,
  notes: 'Half a scoop, then fresh water.', available: true, claimed_by: null, claimed_at: null,
  awaiting_approval: false, completed_at: null, available_at: null,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
  shareable: false, teamwork_bonus: 0, shared: false, participants: [],
  min_age: null, max_age: null, ineligible_member_ids: [], ...over,
});

const err = (status, code, message = code) => [status, { error: { code, message } }];

function routes() {
  const r = {
    'POST /auth/refresh': () => [200, {
      access_token: 'at', refresh_token: 'rt',
      user: { id: 'u1', email: 'p@example.com', display_name: 'Parent', email_verified: true },
    }],
    'GET /households': () => [200, { items: [{ id: H, name: 'Home', timezone: 'UTC', role: 'admin' }] }],
    [`GET ${HP}/admin-pin`]: () => [200, { set: srv.adminPinSet }],
    [`POST ${HP}/admin-pin/verify`]: (r) => (r.body.pin === srv.adminPin ? [204, null] : err(403, 'wrong_pin', 'Wrong PIN.')),
    [`GET ${HP}/quests`]: () => [200, { items: srv.quests, next_offset: null }],
    [`GET ${HP}/combos`]: () => [200, { items: [] }],
    'GET /quest-library': () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
    [`GET ${HP}/family-members/birthdays`]: () => [200, { items: [] }],
    [`POST ${HP}/family-members/m-a/verify`]: () => [204, null],
    [`POST ${HP}/family-members/m-b/verify`]: (r) => (r.body.pin === '1234' ? [204, null] : err(403, 'wrong_pin')),
    [`GET ${HP}/quests/pending`]: (r) => srv.pending(r),
    ...srv.extra,
  };
  return new Proxy(r, {
    get(target, prop) {
      if (prop in target) return target[prop];
      if (typeof prop === 'string' && prop.startsWith(`GET ${HP}/family-members/`) && prop.endsWith('/points-history')) {
        return (req) => (srv.pointsHistory ? srv.pointsHistory(req) : [200, { items: srv.history || [] }]);
      }
      return undefined;
    },
  });
}

beforeEach(() => {
  env = installBrowser();
  sessionStorage.setItem(REFRESH_KEY, 'rt');
  signedOut = 0;
  window.addEventListener('chorus:signed-out', () => { signedOut += 1; });
  srv = {
    adminPinSet: false,
    adminPin: '9999',
    quests: [quest()],
    members: [
      { id: 'm-a', household_id: H, name: 'Ann', has_pin: false, points: 0 },
      { id: 'm-b', household_id: H, name: 'Ben', has_pin: true, points: 30 },
    ],
    history: [],
    pending: () => [200, { items: [] }],
    extra: {},
  };
  calls = installFetch(new Proxy({}, { get: (_, k) => routes()[k] }));
});

const text = () => env.$app.textContent;
const actionCalls = () => calls.filter((c) => /\/(quests\/[^/]+\/(accept|abandon|done)|quest-completions\/[^/]+\/(approve|reject))$/.test(c.path));

async function becomeAnn() {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ann'));
}

async function becomeBen() {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ben'));
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = '1234';
  await submit($pin.parentNode.parentNode);
}

async function backToAdmin() {
  await click(buttons(env.$app, 'Administrator view')[0]);
}

test('CLM-8: no active member shows the prompt and no quest action controls', async () => {
  await loadApp();
  assert.match(text(), /Choose a family member to take quests\./);
  for (const label of ['Accept', 'Abandon', 'Mark done']) assert.equal(buttons(env.$app, label).length, 0, label);
  assert.equal(actionCalls().length, 0);
});

test('CLM-1: quest detail shows notes; Accept sends the active member and moves it to Mine, claimed for others', async () => {
  srv.extra[`POST ${HP}/quests/q1/accept`] = (r) => {
    srv.quests = [quest({ available: false, claimed_by: r.body.member_id, claimed_at: '2025-01-02T00:00:00Z' })];
    return [200, srv.quests[0]];
  };
  await loadApp();
  await becomeAnn();
  assert.match(section(env.$app, 'Available'), /Feed the cat/);

  await click(buttons(env.$app, 'View')[0]);
  assert.match(text(), /Half a scoop, then fresh water\./);
  assert.match(text(), /5 points · resets at midnight after 1 day\(s\)/);
  assert.equal(buttons(env.$app, 'Decline').length, 1);

  await click(buttons(env.$app, 'Accept')[0]);
  const [accept] = actionCalls();
  assert.equal(accept.path, `${HP}/quests/q1/accept`);
  assert.deepEqual(accept.body, { member_id: 'm-a', pin: '' });
  assert.match(section(env.$app, 'Mine'), /Feed the cat/);
  assert.doesNotMatch(section(env.$app, 'Available'), /Feed the cat/);

  await backToAdmin();
  await becomeBen();
  assert.match(text(), /Quests for Ben/);
  assert.doesNotMatch(section(env.$app, 'Available'), /Feed the cat/);
  assert.match(section(env.$app, 'Claimed'), /Feed the cat.*Taken by Ann/);
});

test('CLM-2: Decline sends nothing; Abandon posts the member and returns the quest to Available', async () => {
  await loadApp();
  await becomeAnn();
  await click(buttons(env.$app, 'View')[0]);
  const before = calls.length;
  await click(buttons(env.$app, 'Decline')[0]);
  assert.equal(calls.length, before, 'Decline sent a request');
  assert.equal(buttons(env.$app, 'Accept').length, 0, 'detail still open');
  assert.match(section(env.$app, 'Available'), /Feed the cat/);

  srv.quests = [quest({ available: false, claimed_by: 'm-a' })];
  srv.extra[`POST ${HP}/quests/q1/abandon`] = () => { srv.quests = [quest()]; return [200, srv.quests[0]]; };
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests')); // re-read the list
  assert.match(section(env.$app, 'Mine'), /Feed the cat/);
  await click(buttons(env.$app, 'Abandon')[0]);
  const [abandon] = actionCalls();
  assert.equal(abandon.path, `${HP}/quests/q1/abandon`);
  assert.deepEqual(abandon.body, { member_id: 'm-a', pin: '' });
  assert.match(section(env.$app, 'Available'), /Feed the cat/);
  assert.doesNotMatch(section(env.$app, 'Mine'), /Feed the cat/);
});

test('CLM-3: a 409 quest_taken shows the already-taken message, reloads, keeps the member and session', async () => {
  srv.extra[`POST ${HP}/quests/q1/accept`] = () => {
    srv.quests = [quest({ available: false, claimed_by: 'm-b' })];
    return err(409, 'quest_taken', 'This quest has already been taken.');
  };
  await loadApp();
  await becomeAnn();
  await click(buttons(env.$app, 'View')[0]);
  await click(buttons(env.$app, 'Accept')[0]);
  assert.match(text(), /This quest was already taken\./);
  const acceptAt = calls.findIndex((c) => c.path.endsWith('/accept'));
  assert.ok(calls.slice(acceptAt + 1).some((c) => c.method === 'GET' && c.path === `${HP}/quests`), 'list not reloaded');
  assert.match(section(env.$app, 'Claimed'), /Feed the cat.*Taken by Ben/);
  assert.match(text(), /Quests for Ann/);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
  assert.equal(signedOut, 0);
});

test('CLM-4: Abandon and Mark done only on own quests; Mine row has no badge', async () => {
  srv.quests = [
    quest({ id: 'q1', name: 'Own quest', available: false, claimed_by: 'm-a' }),
    quest({ id: 'q2', name: 'Bens quest', available: false, claimed_by: 'm-b' }),
  ];
  await loadApp();
  await becomeAnn();
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.equal(buttons(rowOf('Own quest'), 'Mark done').length, 1);
  assert.equal(buttons(rowOf('Own quest'), 'Abandon').length, 1);
  assert.equal(buttons(rowOf('Bens quest'), 'Mark done').length + buttons(rowOf('Bens quest'), 'Abandon').length, 0);
  assert.match(rowOf('Bens quest').textContent, /Taken by Ben/);
  assert.doesNotMatch(rowOf('Own quest').textContent, /Waiting for approval/);
});

test('abandon CLM-2: a 404 on Abandon shows exactly Quest not found.; a wrong PIN shows the PIN error; neither uses another give-up word', async () => {
  const otherWords = /cancel|give up|drop|quit|withdraw|release|put back/i;
  const alerts = (root) => walk(root).filter((n) => n.getAttribute?.('role') === 'alert' || /\balert\b/.test(n.className || '')).map((n) => n.textContent);

  srv.quests = [quest({ available: false, claimed_by: 'm-a' })];
  srv.extra[`POST ${HP}/quests/q1/abandon`] = () => { srv.quests = [quest({ available: false, claimed_by: 'm-b' })]; return err(404, 'not_found', 'Not found.'); };
  await loadApp();
  await becomeAnn();
  await click(buttons(sectionNode(env.$app, 'Mine'), 'Abandon')[0]);
  assert.equal(actionCalls().at(-1).path, `${HP}/quests/q1/abandon`);
  const notFound = alerts(env.$app);
  assert.deepEqual(notFound, ['Quest not found.']);
  assert.doesNotMatch(notFound[0], otherWords);
  assert.match(text(), /Quests for Ann/);

  srv.quests = [quest({ available: false, claimed_by: 'm-a' })];
  srv.extra[`POST ${HP}/quests/q1/abandon`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests')); // re-read the list
  await click(buttons(sectionNode(env.$app, 'Mine'), 'Abandon')[0]);
  const pinError = alerts(document.body).find((t) => /PIN/.test(t));
  assert.equal(pinError, 'That PIN is no longer correct. Pick the member again.');
  assert.doesNotMatch(pinError, otherWords);
  assert.match(text(), /Choose a family member to take quests\./);
});

test('CLM-8: a re-taken chore whose earlier completion still waits shows Waiting for approval in Mine (with Mark done) and in Claimed', async () => {
  srv.quests = [
    quest({ id: 'q1', name: 'Own retaken', available: false, claimed_by: 'm-a', awaiting_approval: true }),
    quest({ id: 'q2', name: 'Bens retaken', available: false, claimed_by: 'm-b', awaiting_approval: true }),
  ];
  await loadApp();
  await becomeAnn();
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.match(section(env.$app, 'Mine'), /Own retaken/);
  assert.match(rowOf('Own retaken').textContent, /Waiting for approval/);
  assert.equal(buttons(rowOf('Own retaken'), 'Mark done').length, 1);
  assert.equal(buttons(rowOf('Own retaken'), 'Abandon').length, 1);
  assert.match(section(env.$app, 'Claimed'), /Bens retaken/);
  assert.match(rowOf('Bens retaken').textContent, /Taken by Ben/);
  assert.match(rowOf('Bens retaken').textContent, /Waiting for approval/);
});

test('CLM-5: Mark done posts the member, moves to Coming back later with Back date and Waiting for approval badge', async () => {
  const future = '2030-01-02T00:00:00Z';
  srv.quests = [quest({ available: false, claimed_by: 'm-a' })];
  srv.extra[`POST ${HP}/quests/q1/done`] = () => {
    srv.quests = [quest({ available: false, claimed_by: null, available_at: future, awaiting_approval: true })];
    return [200, srv.quests[0]];
  };
  await loadApp();
  await becomeAnn();
  assert.match(text(), /Viewing as: Ann · 0 points/);
  await click(buttons(env.$app, 'Mark done')[0]);
  const [done] = actionCalls();
  assert.equal(done.path, `${HP}/quests/q1/done`);
  assert.deepEqual(done.body, { member_id: 'm-a', pin: '' });
  assert.match(section(env.$app, 'Mine'), /You have no quests yet\./);
  assert.equal(buttons(sectionNode(env.$app, 'Mine'), 'Abandon').length, 0);
  assert.doesNotMatch(section(env.$app, 'Mine'), /Feed the cat/);
  const comingBack = section(env.$app, 'Coming back later');
  assert.match(comingBack, /Feed the cat/);
  assert.match(comingBack, /Back/);
  assert.match(comingBack, /Waiting for approval/);
  assert.equal(buttons(env.$app, 'Mark done').length, 0);
  assert.match(text(), /Viewing as: Ann · 0 points/);
});

test('CLM-6: the Available list follows the server available flag only', async () => {
  const future = '2999-01-01T00:00:00Z';
  srv.quests = [quest({ available: false, completed_at: '2025-01-03T00:00:00Z', available_at: future })];
  await loadApp();
  await becomeAnn();
  assert.doesNotMatch(section(env.$app, 'Available'), /Feed the cat/);
  assert.match(section(env.$app, 'Coming back later'), /Feed the cat/);

  // The server says available while available_at is still in the future: the
  // client believes the flag, not its own clock.
  srv.quests = [quest({ available: true, completed_at: '2025-01-03T00:00:00Z', available_at: future })];
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));
  assert.match(section(env.$app, 'Available'), /Feed the cat/);
  assert.equal(section(env.$app, 'Coming back later'), null);
});

test('CLM-11: resting quest in Coming back later renders Back followed by fmtDate(available_at)', async () => {
  const future = '2030-01-02T06:00:00Z';
  srv.quests = [quest({ available: false, completed_at: '2025-01-03T00:00:00Z', available_at: future })];
  await loadApp();
  await becomeAnn();
  const comingBack = section(env.$app, 'Coming back later');
  assert.ok(comingBack, 'Coming back later section should be present');
  const expectedText = `Back ${new Date(future).toLocaleString()}`;
  assert.ok(comingBack.includes(expectedText), `expected ${comingBack} to include ${expectedText}`);
});

test('CLM-8: resting quest with awaiting_approval shows Back date and badge; same quest available shows in Available with badge', async () => {
  const future = '2030-01-02T00:00:00Z';
  srv.quests = [
    quest({ id: 'q1', name: 'Resting chore', available: false, claimed_by: null, available_at: future, awaiting_approval: true }),
    quest({ id: 'q2', name: 'Available chore', available: true, claimed_by: null, available_at: future, awaiting_approval: true }),
  ];
  await loadApp();
  await becomeAnn();
  const comingBack = section(env.$app, 'Coming back later');
  assert.match(comingBack, /Resting chore/);
  assert.match(comingBack, /Back/);
  assert.match(comingBack, /Waiting for approval/);

  const avail = section(env.$app, 'Available');
  assert.match(avail, /Available chore/);
  assert.match(avail, /Waiting for approval/);
});

test('CLM-7: points show in the picker, header and Family tab, and refresh after an approval', async () => {
  srv.pending = () => [200, { items: [{ completion_id: 'c1', quest_id: 'q1', name: 'Feed the cat', points: 5, member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-03T00:00:00Z' }] }];
  srv.quests = [quest({ available: false, claimed_by: null, awaiting_approval: true, available_at: '2999-01-01T00:00:00Z' })];
  srv.extra[`POST ${HP}/quest-completions/c1/approve`] = () => {
    srv.members = srv.members.map((m) => (m.id === 'm-a' ? { ...m, points: 5 } : m));
    srv.quests = [quest({ available: false, completed_at: '2025-01-04T00:00:00Z', available_at: '2999-01-01T00:00:00Z' })];
    srv.pending = () => [200, { items: [] }];
    return [200, { completion: { id: 'c1', quest_id: 'q1', member_id: 'm-a', done_at: '2025-01-03T00:00:00Z', approved_at: '2025-01-04T00:00:00Z', status: 'approved' }, member: srv.members[0], combo_bonuses: [] }];
  };
  await loadApp();
  assert.match(text(), /Ann: 0 points/);
  assert.match(text(), /Ben: 30 points/);

  await click(buttons(env.$app, 'Switch member')[0]);
  assert.ok(buttonStarting(env.$app, 'Ann · 0 points'));
  assert.ok(buttonStarting(env.$app, 'Ben · 30 points'));

  await click(buttons(env.$app, 'Approve')[0]);
  assert.match(text(), /Ann: 5 points/);

  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Family'));
  assert.match(text(), /Ann5 points/);
  assert.match(text(), /Ben30 points/);

  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));
  await becomeAnn();
  assert.match(text(), /Viewing as: Ann · 5 points/);
});

test('CLM-8: a 403 wrong_pin on a member action clears the active member and keeps the session', async () => {
  srv.extra[`POST ${HP}/quests/q1/accept`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await loadApp();
  await becomeAnn();
  await click(buttons(env.$app, 'View')[0]);
  await click(buttons(env.$app, 'Accept')[0]);
  assert.match(text(), /Viewing as: Administrator/);
  assert.match(text(), /Choose a family member to take quests\./);
  assert.match(document.body.textContent, /That PIN is no longer correct\. Pick the member again\./);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
  assert.equal(signedOut, 0);
});

test('CLM-9: a 404 on accept or approve shows Quest not found. and reloads', async () => {
  srv.extra[`POST ${HP}/quests/q1/accept`] = () => { srv.quests = []; return err(404, 'not_found', 'Not found.'); };
  await loadApp();
  await becomeAnn();
  await click(buttons(env.$app, 'View')[0]);
  await click(buttons(env.$app, 'Accept')[0]);
  assert.match(text(), /Quest not found\./);
  assert.doesNotMatch(section(env.$app, 'Available'), /Feed the cat/);

  let pendingItems = [{ completion_id: 'c9', quest_id: 'q9', name: 'Feed the cat', points: 5, member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-03T00:00:00Z' }];
  srv.pending = () => [200, { items: pendingItems }];
  srv.extra[`POST ${HP}/quest-completions/c9/approve`] = () => { pendingItems = []; return err(404, 'not_found', 'Not found.'); };
  await backToAdmin();
  const pendingBefore = calls.filter((c) => c.path.endsWith('/pending')).length;
  await click(buttons(env.$app, 'Approve')[0]);
  assert.match(text(), /Quest not found\./);
  assert.ok(calls.filter((c) => c.path.endsWith('/pending')).length > pendingBefore, 'approvals not reloaded');
  assert.match(text(), /Nothing waiting for approval\./);
});

async function unlockAdmin(pin) {
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = pin;
  await submit($pin.parentNode.parentNode);
}

test('CLM-10: Approvals lists name, member and points with both buttons, sends X-Admin-PIN, and has an empty state', async () => {
  srv.adminPinSet = true;
  let items = [
    { completion_id: 'c1', quest_id: 'q1', name: 'Feed the cat', points: 5, member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-03T00:00:00Z' },
    { completion_id: 'c2', quest_id: 'q2', name: 'Take out trash', points: 10, member_id: 'm-b', member_name: 'Ben', done_at: '2025-01-04T00:00:00Z' },
  ];
  srv.pending = (r) => (r.headers['X-Admin-PIN'] === '9999' ? [200, { items }] : err(403, 'admin_pin_required'));
  await loadApp();
  assert.match(text(), /Enter the admin PIN to review approvals\./);
  await unlockAdmin('9999');

  const req = calls.filter((c) => c.path === `${HP}/quests/pending`).at(-1);
  assert.equal(req.headers['X-Admin-PIN'], '9999');
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.match(rowOf('Feed the cat').textContent, /Ann · 5 points/);
  assert.match(rowOf('Feed the cat').textContent, new RegExp(new Date('2025-01-03T00:00:00Z').toLocaleString()));
  assert.match(rowOf('Take out trash').textContent, /Ben · 10 points/);
  assert.match(rowOf('Take out trash').textContent, new RegExp(new Date('2025-01-04T00:00:00Z').toLocaleString()));
  for (const name of ['Feed the cat', 'Take out trash']) {
    assert.equal(buttons(rowOf(name), 'Approve').length, 1);
    assert.equal(buttons(rowOf(name), 'Send back').length, 1);
  }

  items = [];
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));
  assert.match(text(), /Nothing waiting for approval\./);
});

test('CLM-11: Approve and Send back post an empty body to completion endpoints with X-Admin-PIN', async () => {
  srv.adminPinSet = true;
  srv.quests = [
    quest({ id: 'q1', available: false, claimed_by: null, awaiting_approval: true }),
    quest({ id: 'q2', name: 'Water plants', available: false, claimed_by: null, awaiting_approval: true }),
  ];
  let items = [
    { completion_id: 'c1', quest_id: 'q1', name: 'Feed the cat', points: 5, member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-03T00:00:00Z' },
    { completion_id: 'c2', quest_id: 'q2', name: 'Water plants', points: 5, member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-03T00:00:00Z' },
  ];
  srv.pending = () => [200, { items }];
  srv.extra[`POST ${HP}/quest-completions/c1/approve`] = () => {
    srv.members = srv.members.map((m) => (m.id === 'm-a' ? { ...m, points: 5 } : m));
    items = items.filter((i) => i.completion_id !== 'c1');
    return [200, { completion: { id: 'c1', quest_id: 'q1', member_id: 'm-a', done_at: '2025-01-03T00:00:00Z', approved_at: '2025-01-04T00:00:00Z', status: 'approved' }, member: srv.members[0], combo_bonuses: [] }];
  };
  srv.extra[`POST ${HP}/quest-completions/c2/reject`] = () => {
    items = items.filter((i) => i.completion_id !== 'c2');
    return [200, { completion: { id: 'c2', quest_id: 'q2', member_id: 'm-a', done_at: '2025-01-03T00:00:00Z', status: 'rejected' } }];
  };
  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  const rowOf = (name) => walk(approvals()).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  await click(buttons(rowOf('Feed the cat'), 'Approve')[0]);
  const approve = actionCalls().at(-1);
  assert.equal(approve.method, 'POST');
  assert.equal(approve.path, `${HP}/quest-completions/c1/approve`);
  assert.deepEqual(approve.body, {});
  assert.equal(approve.headers['X-Admin-PIN'], '9999');
  assert.match(text(), /Ann: 5 points/);
  assert.equal(rowOf('Feed the cat'), undefined);

  await click(buttons(rowOf('Water plants'), 'Send back')[0]);
  const reject = actionCalls().at(-1);
  assert.equal(reject.path, `${HP}/quest-completions/c2/reject`);
  assert.deepEqual(reject.body, {});
  assert.equal(reject.headers['X-Admin-PIN'], '9999');
  assert.match(text(), /Nothing waiting for approval\./);
  assert.match(text(), /Ann: 5 points/);
});

test('CLM-10: takeCard fetches points-history with X-Member-PIN and renders table in order', async () => {
  srv.history = [
    { id: 'a3', points: 5, reason: 'streak: Feed the cat 4', earned_at: '2025-01-03T12:00:00Z' },
    { id: 'a2', points: 3, reason: 'Feed the cat', earned_at: '2025-01-02T12:00:00Z' },
    { id: 'a1', points: 10, reason: 'Clean sweep', earned_at: '2025-01-01T12:00:00Z' },
  ];
  await loadApp();
  await becomeBen();

  const histCall = calls.find((c) => c.path === `${HP}/family-members/m-b/points-history`);
  assert.ok(histCall, 'points-history was fetched');
  assert.equal(histCall.headers['X-Member-PIN'], '1234');

  const historyNode = sectionNode(env.$app, 'Points history');
  assert.ok(historyNode, 'Points history section exists');

  const ths = walk(historyNode).filter((n) => n.tagName === 'TH').map((n) => n.textContent);
  assert.deepEqual(ths, ['Date', 'Points', 'Reason']);

  const rows = walk(historyNode).filter((n) => n.tagName === 'TBODY');
  assert.equal(rows.length, 1);
  const trs = walk(rows[0]).filter((n) => n.tagName === 'TR');
  assert.equal(trs.length, 3);

  const rowData = trs.map((tr) => walk(tr).filter((n) => n.tagName === 'TD').map((td) => td.textContent));
  assert.equal(rowData[0][1], '+5');
  assert.equal(rowData[0][2], 'streak: Feed the cat 4');
  assert.equal(rowData[1][1], '+3');
  assert.equal(rowData[1][2], 'Feed the cat');
  assert.equal(rowData[2][1], '+10');
  assert.equal(rowData[2][2], 'Clean sweep');
});

test('a failing points-history read keeps the quest lists and actions; the error shows in the history section', async () => {
  srv.quests = [quest(), quest({ id: 'q2', name: 'Fold laundry', available: false, claimed_by: 'm-a' })];
  srv.pointsHistory = () => err(429, 'rate_limited', 'Too many requests.');
  await loadApp();
  await becomeAnn();

  assert.match(section(env.$app, 'Available'), /Feed the cat/);
  assert.match(section(env.$app, 'Mine'), /Fold laundry/);
  assert.equal(buttons(env.$app, 'Mark done').length, 1);
  assert.equal(buttons(env.$app, 'Abandon').length, 1);
  const historyNode = sectionNode(env.$app, 'Points history');
  assert.ok(historyNode, 'Points history section exists');
  assert.match(historyNode.textContent, /Could not load points history/);
  assert.equal(walk(historyNode).filter((n) => n.tagName === 'TABLE').length, 0);
});

test('a 403 wrong_pin on points-history returns to the member picker, not a blank tab', async () => {
  await loadApp();
  srv.pointsHistory = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await becomeBen();

  assert.match(text(), /Viewing as: Administrator/);
  assert.match(text(), /Choose a family member to take quests\./);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt', 'session kept');
  assert.equal(signedOut, 0);
});

test('the points history is read once per member selection, not again on the rerender after accept or mark done', async () => {
  srv.extra[`POST ${HP}/quests/q1/accept`] = (r) => {
    srv.quests = [quest({ available: false, claimed_by: r.body.member_id, claimed_at: '2025-01-02T00:00:00Z' })];
    return [200, srv.quests[0]];
  };
  srv.extra[`POST ${HP}/quests/q1/done`] = () => {
    srv.quests = [quest({ available: false, awaiting_approval: true })];
    return [200, srv.quests[0]];
  };
  srv.history = [{ id: 'a1', points: 10, reason: 'Clean sweep', earned_at: '2025-01-01T12:00:00Z' }];
  const historyReads = () => calls.filter((c) => c.method === 'GET' && c.path.endsWith('/points-history'));
  await loadApp();
  await becomeAnn();
  assert.equal(historyReads().length, 1, 'one read on selecting the member');

  await click(buttons(env.$app, 'View')[0]);
  await click(buttons(env.$app, 'Accept')[0]);
  assert.match(section(env.$app, 'Mine'), /Feed the cat/);
  await click(buttons(env.$app, 'Mark done')[0]);
  assert.equal(historyReads().length, 1, 'actions rerender without reading the history again');
  assert.match(sectionNode(env.$app, 'Points history').textContent, /Clean sweep/, 'the history still shows');

  await becomeAnn();
  assert.equal(historyReads().length, 2, 'picking the member again reads it afresh');
});

test('switching members while the first member\'s history read is pending never shows or acts on the first member\'s view under the second', async () => {
  srv.quests = [
    quest({ id: 'qa', name: 'Ann chore', available: false, claimed_by: 'm-a' }),
    quest({ id: 'qb', name: 'Ben chore', available: false, claimed_by: 'm-b' }),
  ];
  srv.pointsHistory = (r) => [200, { items: r.path.includes('/m-a/')
    ? [{ id: 'a1', points: 7, reason: 'Ann award', earned_at: '2025-01-01T12:00:00Z' }]
    : [{ id: 'b1', points: 9, reason: 'Ben award', earned_at: '2025-01-02T12:00:00Z' }] }];
  const posted = [];
  srv.extra[`POST ${HP}/quests/qa/done`] = (r) => { posted.push(['qa', r.body]); return [200, srv.quests[0]]; };
  srv.extra[`POST ${HP}/quests/qb/done`] = (r) => { posted.push(['qb', r.body]); return [200, srv.quests[1]]; };
  await loadApp();

  // Hold Ann's points-history response until Ben's view has rendered.
  const realFetch = globalThis.fetch;
  let releaseAnn;
  const annHeld = new Promise((r) => { releaseAnn = r; });
  globalThis.fetch = async (url, init) => {
    const res = realFetch(url, init);
    if (url.includes('/family-members/m-a/points-history')) await annHeld;
    return res;
  };

  await becomeAnn();
  assert.doesNotMatch(text(), /Quests for/, 'Ann\'s view waits for her history');
  await becomeBen();
  assert.match(text(), /Quests for Ben/);
  releaseAnn();
  await settle();

  // Ben's view stands: his title, his list, his history; nothing of Ann's.
  assert.match(text(), /Quests for Ben/);
  assert.doesNotMatch(text(), /Quests for Ann/);
  assert.match(section(env.$app, 'Mine'), /Ben chore/);
  assert.doesNotMatch(section(env.$app, 'Mine'), /Ann chore/);
  const historyNode = sectionNode(env.$app, 'Points history');
  assert.match(historyNode.textContent, /Ben award/);
  assert.doesNotMatch(historyNode.textContent, /Ann award/);

  // Mark done from the view on screen posts Ben's quest with Ben's identity.
  assert.equal(buttons(env.$app, 'Mark done').length, 1);
  await click(buttons(env.$app, 'Mark done')[0]);
  assert.deepEqual(posted, [['qb', { member_id: 'm-b', pin: '1234' }]]);
  // Ann's late answer never displaced Ben's history: the rerender after the
  // action reads nothing again, and Ben's history still shows.
  const benReads = calls.filter((c) => c.method === 'GET' && c.path === `${HP}/family-members/m-b/points-history`);
  assert.equal(benReads.length, 1, 'one history read for Ben, none duplicated by the stale answer');
  assert.match(sectionNode(env.$app, 'Points history').textContent, /Ben award/);
  globalThis.fetch = realFetch;
});

test('a wrong_pin answering a superseded member\'s history read does not throw out the member picked since', async () => {
  srv.pointsHistory = (r) => (r.path.includes('/m-a/') ? err(403, 'wrong_pin', 'Wrong PIN.') : [200, { items: [] }]);
  await loadApp();
  const realFetch = globalThis.fetch;
  let releaseAnn;
  const annHeld = new Promise((r) => { releaseAnn = r; });
  globalThis.fetch = async (url, init) => {
    const res = realFetch(url, init);
    if (url.includes('/family-members/m-a/points-history')) await annHeld;
    return res;
  };
  await becomeAnn();
  await becomeBen();
  releaseAnn();
  await settle();
  assert.match(text(), /Viewing as: Ben/);
  assert.match(text(), /Quests for Ben/);
  globalThis.fetch = realFetch;
});

test('CLM-11: empty points-history renders empty message and no table', async () => {
  srv.history = [];
  await loadApp();
  await becomeAnn();

  const historyNode = sectionNode(env.$app, 'Points history');
  assert.ok(historyNode, 'Points history section exists');
  assert.match(historyNode.textContent, /No points yet/);
  assert.match(historyNode.textContent, /Points you earn will show up here\./);

  const tables = walk(historyNode).filter((n) => n.tagName === 'TABLE');
  assert.equal(tables.length, 0, 'no table should be rendered');
  const rows = walk(historyNode).filter((n) => n.tagName === 'TR');
  assert.equal(rows.length, 0, 'no rows should be rendered');
});

test('CLM-1: points history inserts API data as textContent with no markup or child elements', async () => {
  const evilReason = '<b>x</b><img src=x onerror=alert(1)>';
  srv.history = [
    { id: 'a1', points: 5, reason: evilReason, earned_at: '2025-01-01T12:00:00Z' },
  ];
  await loadApp();
  await becomeAnn();

  const historyNode = sectionNode(env.$app, 'Points history');
  const cells = walk(historyNode).filter((n) => n.tagName === 'TD');
  const reasonCell = cells.find((td) => td.textContent === evilReason);
  assert.ok(reasonCell, 'reason cell contains the exact literal string');
  assert.equal(reasonCell.children.length, 0, 'cell has no child elements created');
  assert.equal(reasonCell.childNodes.length, 1);
  assert.equal(reasonCell.childNodes[0].data, evilReason);
});

test('CLM-12: a 403 admin-PIN error says so, keeps the list and the session', async () => {
  srv.adminPinSet = true;
  const items = [{ completion_id: 'c1', quest_id: 'q1', name: 'Feed the cat', points: 5, member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-03T00:00:00Z' }];
  srv.pending = () => [200, { items }];
  srv.extra[`POST ${HP}/quest-completions/c1/approve`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await loadApp();
  await unlockAdmin('9999');
  await click(buttons(env.$app, 'Approve')[0]);
  assert.match(text(), /Wrong administrator PIN\./);
  assert.equal(buttons(env.$app, 'Approve').length, 1, 'row removed');
  assert.match(text(), /Feed the cat/);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
  assert.equal(signedOut, 0);

  // The pending list itself answering admin_pin_required says the same.
  srv.pending = () => err(403, 'admin_pin_required', 'Admin PIN is required.');
  await unlockAdmin('9999');
  assert.match(text(), /Wrong administrator PIN\./);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
  assert.equal(signedOut, 0);
});

test('CLM-7: approvals card renders completions in order, with name, member points, date text, and completion POST paths', async () => {
  srv.adminPinSet = true;
  const d1 = '2025-01-01T10:00:00Z';
  const d2 = '2025-01-02T10:00:00Z';
  let items = [
    { completion_id: 'c-old', quest_id: 'q1', name: 'First chore', points: 3, member_id: 'm-a', member_name: 'Ann', done_at: d1, teamwork_bonus: 0, members: [{ member_id: 'm-a', member_name: 'Ann', points: 3 }] },
    { completion_id: 'c-new', quest_id: 'q2', name: 'Second chore', points: 7, member_id: 'm-b', member_name: 'Ben', done_at: d2, teamwork_bonus: 0, members: [{ member_id: 'm-b', member_name: 'Ben', points: 7 }] },
  ];
  srv.pending = () => [200, { items }];
  srv.extra[`POST ${HP}/quest-completions/c-old/approve`] = () => {
    items = items.filter((i) => i.completion_id !== 'c-old');
    return [200, { completion: { id: 'c-old', status: 'approved' }, member: srv.members[0], combo_bonuses: [] }];
  };
  srv.extra[`POST ${HP}/quest-completions/c-new/reject`] = () => {
    items = items.filter((i) => i.completion_id !== 'c-new');
    return [200, { completion: { id: 'c-new', status: 'rejected' } }];
  };
  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  const lis = walk(approvals()).filter((n) => n.tagName === 'LI');
  assert.equal(lis.length, 2);
  assert.ok(lis[0].textContent.includes('First chore'));
  assert.ok(lis[0].textContent.includes('Ann · 3 points'));
  assert.ok(lis[0].textContent.includes(new Date(d1).toLocaleString()));
  assert.ok(lis[1].textContent.includes('Second chore'));
  assert.ok(lis[1].textContent.includes('Ben · 7 points'));
  assert.ok(lis[1].textContent.includes(new Date(d2).toLocaleString()));

  await click(buttons(lis[0], 'Approve')[0]);
  const approveCall = actionCalls().at(-1);
  assert.equal(approveCall.path, `${HP}/quest-completions/c-old/approve`);

  const remainingLis = walk(approvals()).filter((n) => n.tagName === 'LI');
  assert.equal(remainingLis.length, 1);
  await click(buttons(remainingLis[0], 'Send back')[0]);
  const rejectCall = actionCalls().at(-1);
  assert.equal(rejectCall.path, `${HP}/quest-completions/c-new/reject`);

  assert.match(text(), /Nothing waiting for approval\./);
});

test('CLM-12: approvalsCard act() posts with completion_id only and never falls back to quest id', async () => {
  srv.adminPinSet = true;
  let items = [
    { id: 'quest-x1', completion_id: 'comp-y1', quest_id: 'quest-x1', name: 'First chore', points: 3, member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-01T10:00:00Z' },
    { id: 'quest-x2', completion_id: 'comp-y2', quest_id: 'quest-x2', name: 'Second chore', points: 7, member_id: 'm-b', member_name: 'Ben', done_at: '2025-01-02T10:00:00Z' },
  ];
  srv.pending = () => [200, { items }];
  srv.extra[`POST ${HP}/quest-completions/comp-y1/approve`] = () => {
    items = items.filter((i) => i.completion_id !== 'comp-y1');
    return [200, { completion: { id: 'comp-y1', status: 'approved' }, member: srv.members[0], combo_bonuses: [] }];
  };
  srv.extra[`POST ${HP}/quest-completions/comp-y2/reject`] = () => {
    items = items.filter((i) => i.completion_id !== 'comp-y2');
    return [200, { completion: { id: 'comp-y2', status: 'rejected' } }];
  };
  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  const lis = () => walk(approvals()).filter((n) => n.tagName === 'LI');
  assert.equal(lis().length, 2);

  await click(buttons(lis()[0], 'Approve')[0]);
  const approveCall = actionCalls().at(-1);
  assert.equal(approveCall.path, `${HP}/quest-completions/comp-y1/approve`);
  assert.ok(!approveCall.path.includes('quest-x1'), 'approve path must not contain quest id');

  assert.equal(lis().length, 1);
  await click(buttons(lis()[0], 'Send back')[0]);
  const rejectCall = actionCalls().at(-1);
  assert.equal(rejectCall.path, `${HP}/quest-completions/comp-y2/reject`);
  assert.ok(!rejectCall.path.includes('quest-x2'), 'reject path must not contain quest id');

  for (const c of calls) {
    assert.ok(!c.path.includes('quest-x1'), 'no call path may contain quest-x1');
    assert.ok(!c.path.includes('quest-x2'), 'no call path may contain quest-x2');
  }
  assert.match(text(), /Nothing waiting for approval\./);
});

test('CLM-12 guard: an unrefreshable 401 still signs out', async () => {
  await loadApp();
  srv.pending = () => err(401, 'unauthorized', 'Unauthorized.');
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));
  await settle();
  assert.equal(sessionStorage.getItem(REFRESH_KEY), null);
  assert.ok(signedOut >= 1);
});

test('a blank points or days field is sent as typed, not 0, and the server\'s 422 shows on the field', async () => {
  srv.extra[`POST ${HP}/quests`] = () => [422, { error: {
    code: 'validation_failed', message: 'Check the highlighted fields.',
    fields: { points: 'is required', repeat_days: 'must be at least 1' },
  } }];
  await loadApp();
  const $form = buttons(env.$app, 'Add quest')[0].parentNode;
  const input = (name) => walk($form).find((n) => (n.tagName === 'INPUT' || n.tagName === 'TEXTAREA') && n.name === name);
  input('name').value = 'Water plants';
  input('points').value = '';
  input('repeat_days').value = '';
  await submit($form);

  const post = calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
  assert.equal(post.body.points, '', 'blank points is not coerced to 0');
  assert.equal(post.body.repeat_days, '', 'blank days is not coerced to 0');
  assert.equal(input('points').parentNode.childNodes.at(-1).textContent, 'is required');

  input('points').value = '7';
  input('repeat_days').value = '2';
  await submit($form);
  const ok = calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
  assert.equal(ok.body.points, 7);
  assert.equal(ok.body.repeat_days, 2);
});

// ── CLM-3: empty states ───────────────────────────────────────────────────────

// noInlineStyle asserts the strict-CSP rule on what was rendered: no element
// carries a style attribute or an inline style property.
const noInlineStyle = () => {
  for (const n of walk(env.$app)) {
    assert.equal(n.getAttribute('style'), null, `${n.tagName} has a style attribute`);
    assert.deepEqual(Object.keys(n.style), [], `${n.tagName} has an inline style`);
  }
};

const openTab = async (label) => {
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === label));
  await settle();
};

test('CLM-3: empty quests tab shows the add/browse prompt with the library browser and add form', async () => {
  srv.quests = [];
  srv.extra['GET /quest-library'] = () => [200, { items: [
    { id: 'ql1', name: 'Make your bed', points: 5, repeat_days: 1, notes: '' },
  ] }];
  await loadApp();
  assert.match(text(), /No quests yet\. Add one below or browse the library\./);
  assert.equal(buttons(env.$app, 'Add quest').length, 1, 'add form is offered');
  const lib = section(env.$app, 'Quest Library');
  assert.ok(lib, 'library browser is shown');
  assert.match(lib, /Make your bed/);
  assert.equal(buttons(sectionNode(env.$app, 'Quest Library'), 'Add').length, 1, 'library item can be added');
  noInlineStyle();
});

test('CLM-3: empty rewards tab shows the add/browse prompt with the library browser and add form', async () => {
  srv.extra[`GET ${HP}/rewards`] = () => [200, { items: [] }];
  srv.extra['GET /reward-library'] = () => [200, { items: [
    { id: 'rl1', description: 'Pick the movie', cost: 20 },
  ] }];
  await loadApp();
  await openTab('Rewards');
  assert.match(text(), /No rewards yet\. Add one below or browse the library for ideas\./);
  assert.equal(buttons(env.$app, 'Add reward').length, 1, 'add form is offered');
  const lib = section(env.$app, 'Reward Library');
  assert.ok(lib, 'library browser is shown');
  assert.match(lib, /Pick the movie/);
  assert.equal(buttons(sectionNode(env.$app, 'Reward Library'), 'Add').length, 1, 'library item can be adopted');
  noInlineStyle();
});

test('CLM-3: empty family tab shows the add-member prompt', async () => {
  srv.members = [];
  await loadApp();
  await openTab('Family');
  assert.match(text(), /No family members yet\. Add one to get started\./);
  assert.equal(buttons(env.$app, 'Add member').length, 1, 'add-member form is offered');
  noInlineStyle();
});

test('CLM-3: home view with no households shows the New household form as the primary view', async () => {
  srv.extra['GET /households'] = () => [200, { items: [] }];
  await loadApp();
  assert.match(text(), /New household/);
  assert.doesNotMatch(text(), /invite/i);
  assert.equal(buttons(env.$app, 'Create').length, 1, 'create form is the main view');
  assert.equal(walk(env.$app).filter((n) => n.tagName === 'A' && n.textContent === 'Quests').length, 0, 'no household panel');
  noInlineStyle();
});

// combos CLM-5/CLM-6: the approve response's combo_bonuses becomes a plain-text
// notice naming the chore and every combo paid; none paid, no notice.
function approvalWithBonuses(bonuses, streak = {}) {
  srv.adminPinSet = true;
  let items = [{ completion_id: 'c1', quest_id: 'q1', name: 'Wipe counters', points: 10, member_id: 'm-a', member_name: 'Sam', done_at: '2025-01-03T00:00:00Z', teamwork_bonus: 0, members: [{ member_id: 'm-a', member_name: 'Sam', points: 10 }] }];
  srv.pending = () => [200, { items }];
  srv.extra[`POST ${HP}/quest-completions/c1/approve`] = () => {
    items = [];
    return [200, { completion: { id: 'c1', quest_id: 'q1', member_id: 'm-a', done_at: '2025-01-03T00:00:00Z', approved_at: '2025-01-04T00:00:00Z', status: 'approved' }, member: srv.members[0], combo_bonuses: bonuses, streak_bonus: 0, streak_count: 1, ...streak }];
  };
}

const alerts = () => walk(env.$app).filter((n) => n.getAttribute('role') === 'alert');

test('combos CLM-5: an approval that paid two combos shows each name and bonus, and the list reloads', async () => {
  approvalWithBonuses([
    { combo_id: 'c1', name: 'Kitchen Crew', bonus_points: 20 },
    { combo_id: 'c2', name: 'Clean Sweep', bonus_points: 15 },
  ]);
  await loadApp();
  await unlockAdmin('9999');
  const pendingBefore = calls.filter((c) => c.path.endsWith('/pending')).length;
  await click(buttons(env.$app, 'Approve')[0]);
  const notice = alerts().map((n) => n.textContent).join('|');
  assert.match(notice, /Approved 'Wipe counters' for Sam: \+10 points, combo bonus 'Kitchen Crew' \+20, combo bonus 'Clean Sweep' \+15/);
  assert.ok(calls.filter((c) => c.path.endsWith('/pending')).length > pendingBefore, 'approvals not reloaded');
  assert.match(text(), /Nothing waiting for approval\./);
});

test('combos CLM-5: an approval with combo_bonuses [] shows no bonus text and no notice', async () => {
  approvalWithBonuses([]);
  await loadApp();
  await unlockAdmin('9999');
  await click(buttons(env.$app, 'Approve')[0]);
  assert.doesNotMatch(text(), /combo bonus/);
  assert.equal(alerts().length, 0);
  assert.match(text(), /Nothing waiting for approval\./);
});

test('combos CLM-6: a combo name with markup stays literal text in the bonus notice', async () => {
  const evil = '<img src=x onerror=1>';
  approvalWithBonuses([{ combo_id: 'c1', name: evil, bonus_points: 5 }]);
  await loadApp();
  await unlockAdmin('9999');
  await click(buttons(env.$app, 'Approve')[0]);
  const [notice] = alerts();
  const span = walk(notice).find((n) => n.tagName === 'SPAN');
  assert.equal(span.childNodes.length, 1);
  assert.ok(span.childNodes[0] instanceof Text);
  assert.ok(span.childNodes[0].data.includes(`combo bonus '${evil}' +5`));
  assert.equal(walk(env.$app).filter((n) => n.tagName === 'IMG').length, 0);
});

// ── streaks CLM-13: the approve response's streak bonus is told to the admin ──

test('streaks CLM-13: an approval that paid a streak bonus shows Streak bonus +N (streak n) as text', async () => {
  approvalWithBonuses([], { streak_bonus: 5, streak_count: 4 });
  await loadApp();
  await unlockAdmin('9999');
  await click(buttons(env.$app, 'Approve')[0]);
  const [notice] = alerts();
  assert.ok(notice, 'a notice is shown');
  const span = walk(notice).find((n) => n.tagName === 'SPAN');
  assert.equal(span.childNodes.length, 1);
  assert.ok(span.childNodes[0] instanceof Text, 'notice is a text node');
  assert.equal(span.childNodes[0].data, "Approved 'Wipe counters' for Sam: +10 points, Streak bonus +5 (streak 4)");
  assert.match(text(), /Nothing waiting for approval\./);
});

test('streaks CLM-13: streak and combo bonuses paid together are both named in one notice', async () => {
  approvalWithBonuses([{ combo_id: 'c1', name: 'Kitchen Crew', bonus_points: 20 }], { streak_bonus: 10, streak_count: 3 });
  await loadApp();
  await unlockAdmin('9999');
  await click(buttons(env.$app, 'Approve')[0]);
  const notice = alerts().map((n) => n.textContent).join('|');
  assert.match(notice, /\+10 points, combo bonus 'Kitchen Crew' \+20, Streak bonus \+10 \(streak 3\)/);
});

test('streaks CLM-13: streak_bonus 0 shows no notice, even at a streak count of 3 or more', async () => {
  approvalWithBonuses([], { streak_bonus: 0, streak_count: 5 });
  await loadApp();
  await unlockAdmin('9999');
  await click(buttons(env.$app, 'Approve')[0]);
  assert.doesNotMatch(text(), /Streak bonus \+/);
  assert.equal(alerts().length, 0);
});

// ── streaks CLM-11: the quest form's streak bonus, and the rows that show it ──

test('streaks CLM-11: the quest form sends streak_bonus as a number; quest, edit and library rows show the bonus', async () => {
  srv.quests = [quest({ streak_bonus: 7 })];
  srv.extra['GET /quest-library'] = () => [200, { items: [
    { id: 'ql1', name: 'Make your bed', points: 5, repeat_days: 1, notes: '', streak_bonus: 5 },
    { id: 'ql2', name: 'Clean the garage', points: 25, repeat_days: 30, notes: '', streak_bonus: 10 },
  ] }];
  srv.extra[`POST ${HP}/quests`] = (r) => [201, quest({ id: 'q2', ...r.body })];
  srv.extra[`PUT ${HP}/quests/q1`] = (r) => [200, quest(r.body)];
  await loadApp();

  const manage = walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Manage quests').parentNode;
  assert.match(manage.textContent, /5 points · 1 day\(s\) · streak bonus 7/);
  const lib = section(env.$app, 'Quest Library');
  assert.match(lib, /Make your bed5 points · 1 day\(s\) · streak bonus 5/);
  assert.match(lib, /Clean the garage25 points · 30 day\(s\) · streak bonus 10/);

  const $add = buttons(env.$app, 'Add quest')[0].parentNode;
  const input = ($f, name) => walk($f).find((n) => (n.tagName === 'INPUT' || n.tagName === 'TEXTAREA') && n.name === name);
  const label = input($add, 'streak_bonus').parentNode.childNodes[0].textContent;
  assert.equal(label, 'Streak bonus');
  assert.equal(input($add, 'streak_bonus').type, 'number');
  input($add, 'name').value = 'Water plants';
  input($add, 'points').value = '3';
  input($add, 'repeat_days').value = '1';
  input($add, 'streak_bonus').value = '5';
  await submit($add);
  const post = calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
  assert.equal(post.body.streak_bonus, 5, "'5' is sent as the number 5");

  await click(buttons(walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Manage quests').parentNode, 'Edit')[0]);
  const $edit = buttons(env.$app, 'Save')[0].parentNode;
  assert.equal(String(input($edit, 'streak_bonus').value), '7', 'edit form starts from the quest\'s bonus');
  input($edit, 'streak_bonus').value = '-1';
  await submit($edit);
  const put = calls.filter((c) => c.method === 'PUT' && c.path === `${HP}/quests/q1`).at(-1);
  assert.equal(put.body.streak_bonus, -1, 'the server, not the client, judges the value');
  noInlineStyle();
});

test('streaks: the add form starts the streak bonus at 0, and a blank streak bonus is left out so the server saves 0', async () => {
  srv.extra[`POST ${HP}/quests`] = (r) => [201, quest({ id: 'q2', ...r.body, streak_bonus: r.body.streak_bonus ?? 0 })];
  await loadApp();
  const $add = buttons(env.$app, 'Add quest')[0].parentNode;
  const input = ($f, name) => walk($f).find((n) => (n.tagName === 'INPUT' || n.tagName === 'TEXTAREA') && n.name === name);
  assert.equal(String(input($add, 'streak_bonus').value), '0', 'a new quest starts at 0');

  input($add, 'name').value = 'Water plants';
  input($add, 'points').value = '3';
  input($add, 'repeat_days').value = '1';
  input($add, 'streak_bonus').value = '';
  await submit($add);
  const post = calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
  assert.ok(post, 'the quest was posted');
  assert.equal('streak_bonus' in post.body, false, 'a blank streak bonus is not sent as an empty string');
  assert.equal(post.body.points, 3);
});

test('streaks: blanking the streak bonus on the edit form never sends 0; it is left out so the server keeps the stored bonus', async () => {
  srv.quests = [quest({ streak_bonus: 7 })];
  srv.extra[`PUT ${HP}/quests/q1`] = (r) => [200, quest({ ...r.body, streak_bonus: r.body.streak_bonus ?? 7 })];
  await loadApp();
  const input = ($f, name) => walk($f).find((n) => (n.tagName === 'INPUT' || n.tagName === 'TEXTAREA') && n.name === name);
  await click(buttons(walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Manage quests').parentNode, 'Edit')[0]);
  const $edit = buttons(env.$app, 'Save')[0].parentNode;
  assert.equal(String(input($edit, 'streak_bonus').value), '7');
  input($edit, 'streak_bonus').value = '';
  await submit($edit);
  const put = calls.filter((c) => c.method === 'PUT' && c.path === `${HP}/quests/q1`).at(-1);
  assert.ok(put, 'the quest was saved');
  assert.equal('streak_bonus' in put.body, false, 'a blank streak bonus is omitted, never sent as 0');
  assert.notEqual(put.body.streak_bonus, 0);
});

// ── fix CLM-1: the combos read is the combos card's alone ───────────────────

const comboReads = () => calls.filter((c) => c.method === 'GET' && c.path === `${HP}/combos`);

test('fix CLM-1: a 500 from GET /combos shows the error in the combos card and the rest of the tab renders', async () => {
  srv.extra[`GET ${HP}/combos`] = () => err(500, 'internal', 'Internal server error.');
  await loadApp();
  assert.equal(comboReads().length, 1);
  assert.match(text(), /Manage quests/);
  assert.match(text(), /Approvals/);
  const combosCard = walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Manage combos').parentNode;
  assert.match(combosCard.textContent, /Could not load combos\./);
  assert.equal(buttons(combosCard, 'Create combo').length, 0);
});

test('fix CLM-1: the member view sends no combos request', async () => {
  srv.adminPinSet = true; // with no admin PIN, management stays visible to members
  await loadApp();
  const before = comboReads().length;
  await becomeAnn();
  assert.match(section(env.$app, 'Available'), /Feed the cat/);
  assert.ok(calls.filter((c) => c.method === 'GET' && c.path === `${HP}/quests`).length > 1, 'tab re-read for Ann');
  assert.equal(comboReads().length, before, 'combos requested in member view');
  assert.doesNotMatch(text(), /Manage combos/);
});

// ── quest sharing (CLM-2, CLM-5, CLM-6, CLM-10) ──────────────────────────────

const field = (root, name) => walk(root).find((n) => (n.tagName === 'INPUT' || n.tagName === 'TEXTAREA') && n.name === name);

test('sharing CLM-2: the admin form shows Teamwork bonus only while Shareable is ticked, sends both, and every list row shows Shareable and Teamwork bonus', async () => {
  srv.quests = [
    quest({ id: 'q1', name: 'Clean the living room', points: 100, shareable: true, teamwork_bonus: 20 }),
    quest({ id: 'q2', name: 'Take out trash', points: 10 }),
  ];
  srv.extra[`POST ${HP}/quests`] = (r) => [201, quest({ id: 'q3', ...r.body })];
  await loadApp();
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.match(rowOf('Clean the living room').textContent, /Shareable Yes · Teamwork bonus 20/);
  assert.match(rowOf('Take out trash').textContent, /Shareable No · Teamwork bonus -/);

  const $form = buttons(env.$app, 'Add quest')[0].parentNode;
  assert.ok(field($form, 'shareable'), 'Shareable checkbox present');
  assert.equal(field($form, 'teamwork_bonus'), undefined, 'bonus hidden while Shareable is unticked');
  field($form, 'name').value = 'Clean the living room';
  field($form, 'points').value = '100';
  field($form, 'repeat_days').value = '7';
  await click(field($form, 'shareable'));
  assert.ok(field($form, 'teamwork_bonus'), 'bonus shown once Shareable is ticked');
  field($form, 'teamwork_bonus').value = '20';
  await submit($form);
  const post = calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
  assert.equal(post.body.shareable, true);
  assert.equal(post.body.teamwork_bonus, 20);

  // Unticked again: the field goes and no bonus is sent as live.
  const $form2 = buttons(env.$app, 'Add quest')[0].parentNode;
  field($form2, 'name').value = 'Dust';
  await click(field($form2, 'shareable'));
  field($form2, 'teamwork_bonus').value = '15';
  await click(field($form2, 'shareable'));
  assert.equal(field($form2, 'teamwork_bonus'), undefined);
  await submit($form2);
  const post2 = calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
  assert.equal(post2.body.shareable, false);
  assert.equal(post2.body.teamwork_bonus, 0);
  noInlineStyle();
});

test('sharing CLM-2: editing a shareable quest starts with Shareable ticked and its bonus, and PUT sends both', async () => {
  srv.quests = [quest({ id: 'q1', name: 'Clean the living room', points: 100, shareable: true, teamwork_bonus: 20 })];
  srv.extra[`PUT ${HP}/quests/q1`] = (r) => [200, quest({ id: 'q1', ...r.body })];
  await loadApp();
  await click(buttons(env.$app, 'Edit')[0]);
  const $form = buttons(env.$app, 'Save')[0].parentNode;
  assert.equal(field($form, 'shareable').checked, true);
  assert.equal(String(field($form, 'teamwork_bonus').value), '20');
  field($form, 'teamwork_bonus').value = '30';
  await submit($form);
  const put = calls.filter((c) => c.method === 'PUT' && c.path === `${HP}/quests/q1`).at(-1);
  assert.equal(put.body.shareable, true);
  assert.equal(put.body.teamwork_bonus, 30);
});

const sharedQuest = () => quest({
  id: 'q1', name: 'Clean the living room', points: 100, available: false, claimed_by: 'm-a',
  shareable: true, teamwork_bonus: 20, shared: true, participants: ['m-a', 'm-b'],
});

async function becomeCat() {
  srv.extra[`POST ${HP}/family-members/m-c/verify`] = () => [204, null];
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Cat'));
}

test('sharing CLM-5: a shared quest is under Mine with the Shared badge and the other members for each participant, not for anyone else', async () => {
  srv.members.push({ id: 'm-c', household_id: H, name: 'Cat', has_pin: false, points: 0 });
  srv.quests = [sharedQuest()];
  const rowOf = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Clean the living room'));
  const badge = () => walk(rowOf()).filter((n) => n.tagName === 'SPAN' && n.className === 'badge badge-sm' && n.textContent === 'Shared');
  await loadApp();

  await becomeAnn();
  assert.match(section(env.$app, 'Mine'), /Clean the living room/);
  assert.equal(badge().length, 1);
  assert.match(rowOf().textContent, /Shared with Ben/);
  assert.doesNotMatch(rowOf().textContent, /Shared with .*Ann/);

  await backToAdmin();
  await becomeBen();
  assert.match(section(env.$app, 'Mine'), /Clean the living room/);
  assert.equal(badge().length, 1);
  assert.match(rowOf().textContent, /Shared with Ann/);
  assert.equal(buttons(rowOf(), 'Mark done').length, 1, 'any participant can mark it done');
  assert.equal(buttons(rowOf(), 'Share').length, 0, 'only the holder shares');

  await backToAdmin();
  await becomeCat();
  assert.doesNotMatch(section(env.$app, 'Mine'), /Clean the living room/);
  assert.match(section(env.$app, 'Claimed'), /Clean the living room/);
  noInlineStyle();
});

test('sharing CLM-6: Share on the holder\'s shareable quest picks a member not on it, asks that member\'s PIN, and posts both members and PINs', async () => {
  srv.members.push({ id: 'm-c', household_id: H, name: 'Cat', has_pin: false, points: 0 });
  srv.quests = [quest({ id: 'q1', name: 'Clean the living room', available: false, claimed_by: 'm-a', shareable: true, teamwork_bonus: 20 })];
  srv.extra[`POST ${HP}/quests/q1/share`] = (r) => {
    srv.quests = [quest({ ...srv.quests[0], shared: true, participants: ['m-a', r.body.target_member_id] })];
    return [200, srv.quests[0]];
  };
  const shareCalls = () => calls.filter((c) => c.path === `${HP}/quests/q1/share`);
  await loadApp();
  await becomeAnn();
  const rowOf = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Clean the living room'));
  assert.equal(buttons(rowOf(), 'Share').length, 1);
  await click(buttons(rowOf(), 'Share')[0]);
  const picker = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Clean the living room with').parentNode;
  const offered = walk(picker).filter((n) => n.tagName === 'BUTTON' && n.textContent !== 'Cancel').map((n) => n.textContent);
  assert.deepEqual(offered, ['Ben', 'Cat'], 'self is not offered');
  assert.equal(shareCalls().length, 0, 'nothing sent before a member is picked');

  await click(buttons(picker, 'Ben')[0]);
  assert.match(text(), /Ben enters their PIN to accept/);
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = '5678';
  await submit($pin.parentNode.parentNode);
  assert.equal(shareCalls().length, 1);
  assert.deepEqual(shareCalls()[0].body, { member_id: 'm-a', pin: '', target_member_id: 'm-b', target_pin: '5678' });
  assert.match(rowOf().textContent, /Shared with Ben/);

  // Ben is on it now: only Cat is left to offer.
  await click(buttons(rowOf(), 'Share')[0]);
  const picker2 = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Clean the living room with').parentNode;
  assert.deepEqual(walk(picker2).filter((n) => n.tagName === 'BUTTON' && n.textContent !== 'Cancel').map((n) => n.textContent), ['Cat']);
  await click(buttons(picker2, 'Cat')[0]);
  assert.equal(shareCalls().length, 2, 'a member without a PIN is shared with at once');
  assert.deepEqual(shareCalls()[1].body, { member_id: 'm-a', pin: '', target_member_id: 'm-c', target_pin: '' });
  noInlineStyle();
});

test('sharing CLM-6 / 1.1/5: a wrong PIN for the sharer (no target_pin field) clears the active member as on accept, even after the target typed their PIN', async () => {
  srv.quests = [quest({ id: 'q1', name: 'Clean the living room', available: false, claimed_by: 'm-a', shareable: true, teamwork_bonus: 20 })];
  srv.extra[`POST ${HP}/quests/q1/share`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await loadApp();
  await becomeAnn();
  const rowOf = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Clean the living room'));
  await click(buttons(rowOf(), 'Share')[0]);
  const picker = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Clean the living room with').parentNode;
  await click(buttons(picker, 'Ben')[0]);
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = '5678';
  await submit($pin.parentNode.parentNode);
  assert.match(text(), /Choose a family member to take quests\./);
  assert.match(document.body.textContent, /That PIN is no longer correct\. Pick the member again\./);
  assert.doesNotMatch(text(), /Wrong PIN\. Try again\./);
  assert.equal(signedOut, 0);
});

test('sharing CLM-6 / 1.1/5: a wrong target PIN (fields.target_pin) stays on the target\'s PIN form and keeps the active member', async () => {
  srv.quests = [quest({ id: 'q1', name: 'Clean the living room', available: false, claimed_by: 'm-a', shareable: true, teamwork_bonus: 20 })];
  srv.extra[`POST ${HP}/quests/q1/share`] = () => [403, { error: { code: 'wrong_pin', message: 'Wrong PIN.', fields: { target_pin: 'is wrong' } } }];
  await loadApp();
  await becomeAnn();
  const rowOf = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Clean the living room'));
  await click(buttons(rowOf(), 'Share')[0]);
  const picker = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Clean the living room with').parentNode;
  await click(buttons(picker, 'Ben')[0]);
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = '5678';
  await submit($pin.parentNode.parentNode);
  assert.match(text(), /Wrong PIN\. Try again\./);
  assert.match(text(), /Ben enters their PIN to accept/);
  assert.doesNotMatch(text(), /Choose a family member to take quests\./);
});

test('sharing CLM-6: no Share on a quest that is not shareable, one held by someone else, or when nobody is left to share with', async () => {
  srv.quests = [
    quest({ id: 'q1', name: 'Take out trash', available: false, claimed_by: 'm-a' }),
    quest({ id: 'q2', name: 'Bens shareable', available: false, claimed_by: 'm-b', shareable: true, teamwork_bonus: 5 }),
    quest({ id: 'q3', name: 'Full house', available: false, claimed_by: 'm-a', shareable: true, teamwork_bonus: 5, shared: true, participants: ['m-a', 'm-b'] }),
  ];
  await loadApp();
  await becomeAnn();
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.equal(buttons(rowOf('Take out trash'), 'Share').length, 0);
  assert.equal(buttons(rowOf('Bens shareable'), 'Share').length, 0);
  assert.equal(buttons(rowOf('Full house'), 'Share').length, 0);
  assert.equal(buttons(env.$app, 'Share').length, 0);
  assert.equal(calls.filter((c) => c.path.endsWith('/share')).length, 0);
});

test('sharing CLM-8: share picker lists every candidate including out-of-range; shareButton absent when every candidate is out of range', async () => {
  srv.members.push({ id: 'm-c', household_id: H, name: 'Cat', has_pin: false, points: 0 });
  // Ann holds a shareable quest where Cat is ineligible.
  srv.quests = [quest({
    id: 'q1', name: 'Clean living room', available: false, claimed_by: 'm-a', shareable: true,
    min_age: 10, max_age: 14, ineligible_member_ids: ['m-c'],
  })];
  await loadApp();
  await becomeAnn();
  const rowOf = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Clean living room'));
  assert.equal(buttons(rowOf(), 'Share').length, 1, 'Share offered because Ben is eligible');
  await click(buttons(rowOf(), 'Share')[0]);

  const picker = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Clean living room with').parentNode;
  const offered = walk(picker).filter((n) => n.tagName === 'BUTTON' && n.textContent !== 'Cancel').map((n) => n.textContent);
  assert.deepEqual(offered, ['Ben', 'Cat'], 'lists every non-participant including ineligible member Cat');

  // If every candidate is ineligible, shareButton is absent.
  srv.quests = [quest({
    id: 'q1', name: 'Clean living room', available: false, claimed_by: 'm-a', shareable: true,
    min_age: 10, max_age: 14, ineligible_member_ids: ['m-b', 'm-c'],
  })];
  await become('Ann');
  assert.equal(buttons(rowOf(), 'Share').length, 0, 'shareButton absent when every candidate is ineligible');

  // If nobody is ineligible, Share button shows.
  srv.quests = [quest({
    id: 'q1', name: 'Clean living room', available: false, claimed_by: 'm-a', shareable: true,
    min_age: null, max_age: null, ineligible_member_ids: [],
  })];
  await become('Ann');
  assert.equal(buttons(rowOf(), 'Share').length, 1, 'Share offered when all candidates eligible');
});

test('sharing CLM-9 / CLM-10: ineligible candidates rendered disabled with visible reason text; in-range members stay enabled buttons', async () => {
  ageHousehold();
  srv.extra[`POST ${HP}/quests/q-mow/share`] = (r) => {
    srv.quests = [quest({ ...srv.quests[0], shared: true, participants: [SAM.id, r.body.target_member_id] })];
    return [200, srv.quests[0]];
  };
  const shareCalls = () => calls.filter((c) => c.path === `${HP}/quests/q-mow/share`);

  // Sam holds Mow the lawn (10–14). Ava is in ineligible_member_ids. Leo is eligible.
  srv.quests = [quest({
    ...MOW_LAWN,
    available: false, claimed_by: SAM.id, shareable: true,
  })];

  await loadApp();
  await become('Sam');
  const rowOf = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Mow the lawn'));
  await click(buttons(rowOf(), 'Share')[0]);

  const picker = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Mow the lawn with').parentNode;
  const avaBtn = buttons(picker, 'Ava')[0];
  const leoBtn = buttons(picker, 'Leo')[0];
  assert.ok(avaBtn, 'Ava button rendered');
  assert.equal(avaBtn.disabled, true, 'Ava button is disabled');
  assert.ok(leoBtn, 'Leo button rendered');
  assert.equal(leoBtn.disabled, false, 'Leo button is enabled');

  // Reason text beside Ava
  const avaSpan = avaBtn.parentNode;
  assert.equal(avaSpan.tagName, 'SPAN');
  const small = walk(avaSpan).find((n) => n.tagName === 'SMALL');
  assert.ok(small, 'small tag for reason');
  assert.equal(small.textContent, 'Ages 10–14 only');

  // Clicking disabled entry makes no request
  await click(avaBtn);
  assert.equal(shareCalls().length, 0, 'clicking disabled Ava sends no request');
  assert.doesNotMatch(text(), /Ava enters their PIN/);

  // Clicking Leo starts the share flow
  await click(leoBtn);
  assert.equal(shareCalls().length, 1, 'Leo without PIN shared with at once');
  assert.equal(shareCalls()[0].body.target_member_id, LEO.id);
  noInlineStyle();

  // One-sided variants: min only ('Ages 10+ only') and max only ('Up to 14 only')
  srv.quests = [quest({
    ...READ_CHAPTER, // min_age: 6, ineligible: [LEO.id]
    available: false, claimed_by: SAM.id, shareable: true, min_age: 10, max_age: null, ineligible_member_ids: [LEO.id],
  })];
  await become('Sam');
  const readRow = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Read a chapter'));
  await click(buttons(readRow(), 'Share')[0]);
  const pickerMin = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Read a chapter with').parentNode;
  const leoBtnMin = buttons(pickerMin, 'Leo')[0];
  assert.equal(leoBtnMin.disabled, true);
  assert.equal(walk(leoBtnMin.parentNode).find((n) => n.tagName === 'SMALL').textContent, 'Ages 10+ only');
  noInlineStyle();

  srv.quests = [quest({
    ...WATER_PLANTS, // max_age: 12, ineligible: [AVA.id]
    available: false, claimed_by: SAM.id, shareable: true, min_age: null, max_age: 14, ineligible_member_ids: [AVA.id],
  })];
  await become('Sam');
  const waterRow = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Water plants'));
  await click(buttons(waterRow(), 'Share')[0]);
  const pickerMax = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Water plants with').parentNode;
  const avaBtnMax = buttons(pickerMax, 'Ava')[0];
  assert.equal(avaBtnMax.disabled, true);
  assert.equal(walk(avaBtnMax.parentNode).find((n) => n.tagName === 'SMALL').textContent, 'Up to 14 only');
  noInlineStyle();
});

test('sharing CLM-12: share picker decides eligibility only from ineligible_member_ids, ignores birthdays, and only includes household members', async () => {
  // Sam holds quest. Leo has birthday in fixture that would make him age 42, but is NOT in ineligible_member_ids.
  // Ava has no birthday.
  // Ineligible_member_ids has an outsider id not in household members.
  srv.members = [
    { id: 'm-sam', household_id: H, name: 'Sam', has_pin: false, points: 0, birthday: '2015-04-02' },
    { id: 'm-ava', household_id: H, name: 'Ava', has_pin: false, points: 0, birthday: null },
    { id: 'm-leo', household_id: H, name: 'Leo', has_pin: false, points: 0, birthday: '1980-01-01' },
  ];
  srv.quests = [quest({
    id: 'q-share', name: 'Board game', available: false, claimed_by: 'm-sam', shareable: true,
    min_age: 10, max_age: 14, ineligible_member_ids: ['m-ava', 'outsider-id-123'],
  })];
  await loadApp();
  await become('Sam');
  const rowOf = () => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Board game'));
  await click(buttons(rowOf(), 'Share')[0]);

  const picker = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Board game with').parentNode;
  const buttonsInPicker = walk(picker).filter((n) => n.tagName === 'BUTTON' && n.textContent !== 'Cancel');
  assert.deepEqual(buttonsInPicker.map((b) => b.textContent), ['Ava', 'Leo'], 'only household candidates listed, outsider ignored');

  const avaBtn = buttons(picker, 'Ava')[0];
  const leoBtn = buttons(picker, 'Leo')[0];
  assert.equal(avaBtn.disabled, true, 'Ava is disabled because she is in ineligible_member_ids (even though birthday is null)');
  assert.equal(leoBtn.disabled, false, 'Leo is enabled because he is not in ineligible_member_ids (even though birthday would be out of range)');
  noInlineStyle();
});

test('sharing CLM-10: a shared completion is one approval item naming every member and what each gets, with Approve and Send back', async () => {
  srv.adminPinSet = true;
  let items = [{
    completion_id: 'c1', quest_id: 'q1', name: 'Clean the living room', points: 100, teamwork_bonus: 20,
    member_id: 'm-a', member_name: 'Alex', done_at: '2025-01-03T00:00:00Z',
    members: [{ member_id: 'm-a', member_name: 'Alex', points: 50 }, { member_id: 'm-b', member_name: 'Sam', points: 50 }],
  }];
  srv.pending = () => [200, { items }];
  srv.extra[`POST ${HP}/quest-completions/c1/approve`] = () => {
    items = [];
    return [200, { completion: { id: 'c1', status: 'approved' }, member: srv.members[0], combo_bonuses: [{ combo_id: 'k1', name: 'Tidy', bonus_points: 5 }] }];
  };
  await loadApp();
  await unlockAdmin('9999');
  const approvals = walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  const lis = walk(approvals).filter((n) => n.tagName === 'LI');
  assert.equal(lis.length, 1, 'one item for the shared completion');
  const lines = walk(lis[0]).filter((n) => n.tagName === 'DIV').map((n) => n.textContent);
  assert.ok(lines.includes('shared by Alex and Sam'), lines.join('|'));
  assert.ok(lines.includes('Alex gets 50 + 20 bonus = 70'));
  assert.ok(lines.includes('Sam gets 50 + 20 bonus = 70'));
  assert.equal(buttons(lis[0], 'Approve').length, 1);
  assert.equal(buttons(lis[0], 'Send back').length, 1);
  noInlineStyle();

  await click(buttons(lis[0], 'Approve')[0]);
  const notice = alerts().map((n) => n.textContent).join('|');
  assert.match(notice, /Approved 'Clean the living room' for Alex and Sam: \+50 points and \+20 teamwork bonus each, combo bonus 'Tidy' \+5/);
});

test('sharing CLM-10: three members are named with commas and and, each with their split', async () => {
  srv.adminPinSet = true;
  srv.pending = () => [200, { items: [{
    completion_id: 'c1', quest_id: 'q1', name: 'Big clean', points: 100, teamwork_bonus: 20,
    member_id: 'm-a', member_name: 'Ann', done_at: '2025-01-03T00:00:00Z',
    members: [
      { member_id: 'm-a', member_name: 'Ann', points: 34 },
      { member_id: 'm-b', member_name: 'Ben', points: 34 },
      { member_id: 'm-c', member_name: 'Cat', points: 34 },
    ],
  }] }];
  await loadApp();
  await unlockAdmin('9999');
  assert.match(text(), /shared by Ann, Ben and Cat/);
  assert.match(text(), /Cat gets 34 \+ 20 bonus = 54/);
});

// ── age ranges (CLM-19..29) ─────────────────────────────────────────────────

const manageCard = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Manage quests').parentNode;
const manageRow = (name) => walk(manageCard()).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
const radio = (root, value) => walk(root).find((n) => n.tagName === 'INPUT' && n.type === 'radio' && n.value === value);
const fieldsetOf = (root) => walk(root).find((n) => n.tagName === 'FIELDSET');
const lastPost = () => calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
const birthdayCalls = () => calls.filter((c) => /birthday/.test(c.path));

function ageHousehold() {
  srv.members = MEMBERS.map((m) => ({ ...m }));
  for (const m of MEMBERS) srv.extra[`POST ${HP}/family-members/${m.id}/verify`] = () => [204, null];
}

async function become(name) {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, name));
}

const leftOutNotes = () => walk(env.$app).filter((n) => n.tagName === 'P' && /is not listed for/.test(n.textContent)).map((n) => n.textContent);

test('CLM-19: the Ages fieldset offers All ages / Age range; the age fields and helper show only under Age range; the body sends null for All ages and for a blank side', async () => {
  srv.extra[`POST ${HP}/quests`] = (r) => [201, quest({ id: 'q9', ...r.body })];
  await loadApp();
  const addForm = () => buttons(env.$app, 'Add quest')[0].parentNode;
  const $ages = fieldsetOf(addForm());
  assert.ok($ages, 'an Ages fieldset');
  assert.equal(walk($ages).find((n) => n.tagName === 'LEGEND').textContent, 'Ages');
  const labels = walk($ages).filter((n) => n.tagName === 'LABEL').map((n) => n.textContent);
  assert.deepEqual(labels, ['All ages', 'Age range']);
  assert.equal(radio(addForm(), 'all').checked, true, 'a new quest starts at All ages');
  assert.equal(field(addForm(), 'min_age'), undefined);
  assert.equal(field(addForm(), 'max_age'), undefined);
  assert.doesNotMatch(addForm().textContent, /Leave one empty/);

  const fill = async (min, max) => {
    const $f = addForm();
    field($f, 'name').value = 'Mow the lawn';
    field($f, 'points').value = '20';
    field($f, 'repeat_days').value = '7';
    if (min === undefined) { await submit($f); return; }
    await click(radio($f, 'range'));
    assert.equal(field($f, 'min_age').parentNode.childNodes[0].textContent, 'Minimum age');
    assert.equal(field($f, 'max_age').parentNode.childNodes[0].textContent, 'Maximum age');
    assert.equal(field($f, 'min_age').type, 'number');
    assert.match(fieldsetOf($f).textContent, /Leave one empty for no limit on that side\./);
    field($f, 'min_age').value = min;
    field($f, 'max_age').value = max;
    await submit($f);
  };

  await fill();
  assert.equal(lastPost().body.min_age, null);
  assert.equal(lastPost().body.max_age, null);
  await fill('10', '14');
  assert.deepEqual([lastPost().body.min_age, lastPost().body.max_age], [10, 14]);
  await fill('6', '');
  assert.deepEqual([lastPost().body.min_age, lastPost().body.max_age], [6, null]);
  await fill('', '12');
  assert.deepEqual([lastPost().body.min_age, lastPost().body.max_age], [null, 12]);

  // Values typed under Age range, then All ages chosen again: hidden and sent as null.
  const $f = addForm();
  field($f, 'name').value = 'Dust';
  await click(radio($f, 'range'));
  field($f, 'min_age').value = '8';
  await click(radio($f, 'all'));
  assert.equal(field($f, 'min_age'), undefined, 'All ages hides the fields');
  assert.doesNotMatch($f.textContent, /Leave one empty/);
  await submit($f);
  assert.deepEqual([lastPost().body.min_age, lastPost().body.max_age], [null, null]);
  noInlineStyle();
});

test('CLM-20 / CLM-29: every quest is listed whatever its range, each row shows the range in bold from questRangeText, and editing opens with its range', async () => {
  srv.quests = RANGE_QUESTS.map((q) => quest(q));
  srv.extra[`PUT ${HP}/quests/${MOW_LAWN.id}`] = (r) => [200, quest({ ...MOW_LAWN, ...r.body })];
  await loadApp();
  assert.equal(walk(manageCard()).filter((n) => n.tagName === 'LI' && buttons(n, 'Edit').length === 1).length, 4, 'four quest rows');
  const bold = (name) => walk(manageRow(name)).filter((n) => n.tagName === 'STRONG').map((n) => n.textContent);
  assert.deepEqual(bold('Feed the cat'), ['All ages']);
  assert.deepEqual(bold('Mow the lawn'), ['Ages 10\u201314']);
  assert.deepEqual(bold('Read a chapter'), ['Ages 6+']);
  assert.deepEqual(bold('Water plants'), ['Up to 12']);

  await click(buttons(manageRow('Mow the lawn'), 'Edit')[0]);
  const $edit = buttons(env.$app, 'Save')[0].parentNode;
  assert.equal(radio($edit, 'range').checked, true);
  assert.equal(radio($edit, 'all').checked, false);
  assert.equal(String(field($edit, 'min_age').value), '10');
  assert.equal(String(field($edit, 'max_age').value), '14');
  await submit($edit);
  const put = calls.filter((c) => c.method === 'PUT' && c.path === `${HP}/quests/${MOW_LAWN.id}`).at(-1);
  assert.deepEqual([put.body.min_age, put.body.max_age], [10, 14]);

  await click(buttons(manageRow('Feed the cat'), 'Edit')[0]);
  const $edit2 = buttons(env.$app, 'Save')[0].parentNode;
  assert.equal(radio($edit2, 'all').checked, true, 'a quest with neither bound opens at All ages');
  assert.equal(field($edit2, 'min_age'), undefined);
  noInlineStyle();
});

test('CLM-20: questRangeText is the one place a range is worded', () => {
  const src = readFileSync(new URL('../static/js/app.js', import.meta.url), 'utf8');
  const start = src.indexOf('function questRangeText(');
  assert.ok(start >= 0, 'questRangeText is defined');
  const end = src.indexOf('\n}\n', start);
  const outside = src.slice(0, start) + src.slice(end);
  assert.doesNotMatch(outside, /`Ages \$\{|`Up to \$\{|Ages \$\{[^}]*\}\+/, 'a range worded outside questRangeText');
  assert.equal(src.match(/function questRangeText\(/g).length, 1);
});

test('CLM-21: a 422 on max_age shows the message inside the Ages fieldset and keeps Age range, 14 and 10; a min_age error shows there too', async () => {
  srv.extra[`POST ${HP}/quests`] = () => RANGE_422;
  await loadApp();
  const $f = buttons(env.$app, 'Add quest')[0].parentNode;
  field($f, 'name').value = 'Mow the lawn';
  field($f, 'points').value = '20';
  field($f, 'repeat_days').value = '7';
  await click(radio($f, 'range'));
  field($f, 'min_age').value = '14';
  field($f, 'max_age').value = '10';
  await submit($f);
  assert.equal(lastPost().body.min_age, 14);
  const $set = fieldsetOf($f);
  const hint = field($f, 'max_age').parentNode.childNodes.at(-1);
  assert.equal(hint.textContent, MIN_ABOVE_MAX);
  assert.ok(walk($set).includes(hint), 'the error is inside the Ages fieldset');
  assert.equal(hint.className, field($f, 'name').parentNode.childNodes.at(-1).className, 'same style as the other field errors');
  assert.ok(hint.childNodes.every((c) => c instanceof Text), 'text only');
  assert.equal(radio($f, 'range').checked, true);
  assert.equal(String(field($f, 'min_age').value), '14');
  assert.equal(String(field($f, 'max_age').value), '10');
  assert.equal(field($f, 'name').value, 'Mow the lawn', 'nothing is cleared');

  srv.extra[`POST ${HP}/quests`] = () => [422, { error: { code: 'validation_failed', message: 'Check the highlighted fields.', fields: { min_age: 'must be a whole number of 0 or more' } } }];
  field($f, 'min_age').value = '-1';
  await submit($f);
  const minHint = field($f, 'min_age').parentNode.childNodes.at(-1);
  assert.equal(minHint.textContent, 'must be a whole number of 0 or more');
  assert.ok(walk(fieldsetOf($f)).includes(minHint));
  assert.equal(field($f, 'max_age').parentNode.childNodes.at(-1).textContent, '', 'the earlier max_age error is cleared');
  noInlineStyle();
});

async function openFamilyUnlocked() {
  srv.adminPinSet = true;
  await loadApp();
  await openTab('Family');
  await unlockAdmin('9999');
}

const familyRow = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
const dateInput = (name) => walk(familyRow(name)).find((n) => n.tagName === 'INPUT' && n.name === 'birthday');

test('CLM-22: the Family tab reads birthdays with the admin PIN and shows each date, the server\'s age, the Not-set line, Save birthday, Clear and the footer', async () => {
  ageHousehold();
  srv.extra[`GET ${HP}/family-members/birthdays`] = (r) => (r.headers['X-Admin-PIN'] === '9999' ? [200, BIRTHDAYS] : err(403, 'admin_pin_required'));
  await openFamilyUnlocked();
  const reads = calls.filter((c) => c.method === 'GET' && c.path === `${HP}/family-members/birthdays`);
  assert.equal(reads.length, 1);
  assert.equal(reads[0].headers['X-Admin-PIN'], '9999');

  assert.equal(dateInput('Sam').type, 'date');
  assert.equal(dateInput('Sam').value, '2015-04-02');
  assert.equal(dateInput('Ava').value, '');
  assert.match(familyRow('Sam').textContent, /Birthday.*\(age 11\)/);
  assert.match(familyRow('Leo').textContent, /\(age 42\)/, 'the age is shown as the server sent it');
  assert.match(familyRow('Ava').textContent, /Not set: Ava can do every quest/);
  assert.doesNotMatch(familyRow('Ava').textContent, /\(age/);
  assert.doesNotMatch(familyRow('Sam').textContent, /Not set/);
  for (const n of ['Sam', 'Ava', 'Leo']) assert.equal(buttons(familyRow(n), 'Save birthday').length, 1, n);
  assert.equal(buttons(familyRow('Sam'), 'Clear').length, 1);
  assert.equal(buttons(familyRow('Ava'), 'Clear').length, 0);
  assert.equal(buttons(familyRow('Sam'), 'Clear PIN').length, 0, 'Clear PIN is still only for a member with a PIN');
  assert.match(text(), /Birthdays are only shown here, behind your admin PIN\./);
  noInlineStyle();
});

test('CLM-22: a failing birthdays read keeps the family list and its controls; the error shows in the birthday rows', async () => {
  ageHousehold();
  srv.extra[`GET ${HP}/family-members/birthdays`] = () => err(500, 'internal', 'Internal server error.');
  await openFamilyUnlocked();
  assert.ok(familyRow('Sam'), 'the family list renders');
  assert.equal(buttons(familyRow('Sam'), 'Rename').length, 1);
  assert.match(familyRow('Sam').textContent, /Could not load birthdays\./);
  assert.equal(buttons(env.$app, 'Add member').length, 1);
});

test('CLM-23: Save birthday PUTs the date with the admin PIN and re-renders from the reply; Clear PUTs null; an empty date sends nothing; refusals show on the row and keep the old value', async () => {
  ageHousehold();
  srv.extra[`GET ${HP}/family-members/birthdays`] = () => [200, BIRTHDAYS];
  let reply = null;
  srv.extra[`PUT ${HP}/family-members/${AVA.id}/birthday`] = (r) => reply(r);
  srv.extra[`PUT ${HP}/family-members/${SAM.id}/birthday`] = (r) => reply(r);
  const puts = () => calls.filter((c) => c.method === 'PUT' && c.path.endsWith('/birthday'));
  await openFamilyUnlocked();

  await click(buttons(familyRow('Ava'), 'Save birthday')[0]);
  assert.equal(puts().length, 0, 'an empty date sends no request');

  reply = () => [422, { error: { code: 'validation_failed', message: 'Check the highlighted fields.', fields: { birthday: "can't be in the future" } } }];
  dateInput('Ava').value = '2999-01-01';
  await click(buttons(familyRow('Ava'), 'Save birthday')[0]);
  assert.equal(puts().length, 1);
  assert.deepEqual(puts()[0].body, { birthday: '2999-01-01' });
  assert.equal(puts()[0].headers['X-Admin-PIN'], '9999');
  assert.match(familyRow('Ava').textContent, /can't be in the future/);
  assert.match(familyRow('Ava').textContent, /Not set: Ava can do every quest/, 'the shown birthday is kept');
  assert.equal(dateInput('Ava').value, '');

  reply = (r) => [200, { member_id: AVA.id, birthday: r.body.birthday, age: 11 }];
  dateInput('Ava').value = '2015-04-02';
  await click(buttons(familyRow('Ava'), 'Save birthday')[0]);
  assert.deepEqual(puts().at(-1).body, { birthday: '2015-04-02' });
  assert.equal(puts().at(-1).path, `${HP}/family-members/${AVA.id}/birthday`);
  assert.match(familyRow('Ava').textContent, /\(age 11\)/);
  assert.doesNotMatch(familyRow('Ava').textContent, /Not set|can't be in the future/);
  assert.equal(buttons(familyRow('Ava'), 'Clear').length, 1);

  reply = () => [200, { member_id: SAM.id, birthday: null, age: null }];
  const $clear = buttons(familyRow('Sam'), 'Clear')[0];
  await click($clear);
  assert.equal(puts().filter((c) => c.path.includes(SAM.id)).length, 0, 'Clear asks first');
  await click($clear);
  assert.deepEqual(puts().at(-1).body, { birthday: null });
  assert.equal(puts().at(-1).headers['X-Admin-PIN'], '9999');
  assert.match(familyRow('Sam').textContent, /Not set: Sam can do every quest/);
  assert.equal(buttons(familyRow('Sam'), 'Clear').length, 0);

  reply = () => err(403, 'wrong_pin', 'Wrong PIN.');
  dateInput('Ava').value = '2014-01-01';
  await click(buttons(familyRow('Ava'), 'Save birthday')[0]);
  assert.match(familyRow('Ava').textContent, /Wrong administrator PIN\./);
  assert.match(familyRow('Ava').textContent, /\(age 11\)/);
  assert.equal(dateInput('Ava').value, '2015-04-02', 'the shown birthday is kept');

  reply = () => err(403, 'admin_pin_required', 'Admin PIN required.');
  await click(buttons(familyRow('Ava'), 'Save birthday')[0]);
  assert.match(familyRow('Ava').textContent, /Admin PIN required\./);
  assert.match(familyRow('Ava').textContent, /\(age 11\)/);
  assert.equal(signedOut, 0);
  noInlineStyle();
});

test('CLM-24: the member quest view and the share picker never request a birthday and show no birthday or age', async () => {
  ageHousehold();
  srv.adminPinSet = true;
  srv.quests = [...RANGE_QUESTS, TIDY_ROOM].map((q) => quest(q));
  srv.extra[`GET ${HP}/family-members/birthdays`] = () => [200, BIRTHDAYS];
  await loadApp();
  await become('Ava');
  assert.match(text(), /Quests for Ava/);
  await click(buttons(walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Tidy room')), 'Share')[0]);
  assert.ok(walk(env.$app).find((n) => n.textContent === 'Share Tidy room with'), 'share picker open');
  assert.deepEqual(birthdayCalls(), [], 'no birthday request');
  assert.doesNotMatch(text(), /2015-04-02|2020-01-01|\(age \d+\)|Birthday/);

  // Only familyTab names the birthday routes.
  const src = readFileSync(new URL('../static/js/app.js', import.meta.url), 'utf8');
  const start = src.indexOf('async function familyTab(');
  const end = src.indexOf('function adminPinCard(', start);
  assert.ok(start > 0 && end > start);
  assert.match(src.slice(start, end), /\/birthdays`/);
  assert.doesNotMatch(src.slice(0, start) + src.slice(end), /\/birthday/);
});

test('CLM-25 / CLM-28: Available leaves out a quest whose ineligible_member_ids lists the member; Mine keeps a held one; the empty state is unchanged', async () => {
  ageHousehold();
  srv.adminPinSet = true;
  srv.quests = [...RANGE_QUESTS, TIDY_ROOM].map((q) => quest(q));
  await loadApp();
  await become('Ava');
  const availNames = () => walk(sectionNode(env.$app, 'Available')).filter((n) => n.tagName === 'LI').map((n) => n.childNodes[0].childNodes[0].textContent);
  assert.deepEqual(availNames(), ['Feed the cat', 'Read a chapter']);
  for (const li of walk(sectionNode(env.$app, 'Available')).filter((n) => n.tagName === 'LI')) assert.equal(buttons(li, 'View').length, 1);
  await click(buttons(sectionNode(env.$app, 'Available'), 'View')[1]);
  assert.equal(buttons(env.$app, 'Accept').length, 1, 'an eligible quest can be accepted');
  assert.match(section(env.$app, 'Mine'), /Tidy room/, 'a held quest stays in Mine although Ava is out of its range');

  // Sam is in no list (no birthday, or in range): every available quest is his.
  await become('Sam');
  assert.deepEqual(availNames(), ['Feed the cat', 'Mow the lawn', 'Read a chapter', 'Water plants']);
  assert.deepEqual(leftOutNotes(), []);

  // Nothing left for the member: the empty text is unchanged.
  srv.quests = [quest({ ...MOW_LAWN }), quest({ ...WATER_PLANTS })];
  await become('Ava');
  assert.match(section(env.$app, 'Available'), /No quests available right now\./);
});

test('CLM-26: a note under Available names each quest left out for the member with its range; none for a held or resting quest, none when nothing is left out', async () => {
  ageHousehold();
  srv.adminPinSet = true;
  srv.quests = [...RANGE_QUESTS, TIDY_ROOM, RAKE_LEAVES,
    ageQuest({ id: 'q-rest', name: 'Resting one', available: false, available_at: '2999-01-01T00:00:00Z', min_age: 10, max_age: 14, ineligible_member_ids: [AVA.id] }),
  ].map((q) => quest(q));
  await loadApp();
  await become('Ava');
  assert.deepEqual(leftOutNotes(), [
    'Mow the lawn (ages 10\u201314) is not listed for Ava.',
    'Water plants (up to 12) is not listed for Ava.',
  ]);
  for (const n of walk(env.$app).filter((x) => x.tagName === 'P' && /is not listed/.test(x.textContent))) {
    assert.ok(n.childNodes.every((c) => c instanceof Text), 'the note is text');
  }

  await become('Leo');
  assert.deepEqual(leftOutNotes(), ['Read a chapter (ages 6+) is not listed for Leo.']);

  srv.quests = [quest({ ...FEED_CAT })];
  await become('Ava');
  assert.deepEqual(leftOutNotes(), []);
  noInlineStyle();
});

test('CLM-27: a 409 age_restricted on accept reloads the list, then shows the age-range message; quest_taken and already_participant keep theirs', async () => {
  ageHousehold();
  srv.quests = [quest({ ...FEED_CAT })];
  srv.extra[`POST ${HP}/quests/${FEED_CAT.id}/accept`] = () => {
    srv.quests = [quest({ ...FEED_CAT, ineligible_member_ids: [LEO.id] })];
    return err(409, 'age_restricted', "member is outside this quest's age range");
  };
  await loadApp();
  await become('Leo');
  await click(buttons(env.$app, 'View')[0]);
  await click(buttons(env.$app, 'Accept')[0]);
  const acceptAt = calls.findIndex((c) => c.path.endsWith('/accept'));
  assert.ok(calls.slice(acceptAt + 1).some((c) => c.method === 'GET' && c.path === `${HP}/quests`), 'list reloaded');
  assert.match(text(), /This quest is outside that member's age range\./);
  assert.match(leftOutNotes().join('|'), /Feed the cat \(all ages\) is not listed for Leo\./);
  assert.match(text(), /Quests for Leo/);

  // The same code on share gives the same message.
  srv.quests = [quest({ ...TIDY_ROOM })];
  srv.extra[`POST ${HP}/quests/${TIDY_ROOM.id}/share`] = () => err(409, 'age_restricted', 'x');
  await backToAdmin();
  await become('Ava');
  await click(buttons(walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Tidy room')), 'Share')[0]);
  await click(buttons(env.$app, 'Sam')[0]);
  assert.match(text(), /This quest is outside that member's age range\./);
  assert.doesNotMatch(text(), /This quest was already taken\./);
});
