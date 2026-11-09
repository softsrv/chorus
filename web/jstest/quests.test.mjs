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
  done_at: null, awaiting_approval: false, completed_at: null, available_at: null,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z', ...over,
});

const err = (status, code, message = code) => [status, { error: { code, message } }];

function routes() {
  return {
    'POST /auth/refresh': () => [200, {
      access_token: 'at', refresh_token: 'rt',
      user: { id: 'u1', email: 'p@example.com', display_name: 'Parent', email_verified: true },
    }],
    'GET /households': () => [200, { items: [{ id: H, name: 'Home', timezone: 'UTC', role: 'admin' }] }],
    [`GET ${HP}/admin-pin`]: () => [200, { set: srv.adminPinSet }],
    [`POST ${HP}/admin-pin/verify`]: (r) => (r.body.pin === srv.adminPin ? [204, null] : err(403, 'wrong_pin', 'Wrong PIN.')),
    [`GET ${HP}/quests`]: () => [200, { items: srv.quests, next_offset: null }],
    'GET /quest-library': () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
    [`POST ${HP}/family-members/m-a/verify`]: () => [204, null],
    [`POST ${HP}/family-members/m-b/verify`]: (r) => (r.body.pin === '1234' ? [204, null] : err(403, 'wrong_pin')),
    [`GET ${HP}/quests/pending`]: (r) => srv.pending(r),
    ...srv.extra,
  };
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
    pending: () => [200, { items: [] }],
    extra: {},
  };
  calls = installFetch(new Proxy({}, { get: (_, k) => routes()[k] }));
});

const text = () => env.$app.textContent;
const actionCalls = () => calls.filter((c) => /\/quests\/[^/]+\/(accept|abandon|done|approve|reject)$/.test(c.path));

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
  assert.match(text(), /5 points · 1 day\(s\) until available again/);
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

test('CLM-4: Abandon and Mark done only on own quests; awaiting approval shows the note and no controls', async () => {
  srv.quests = [
    quest({ id: 'q1', name: 'Own quest', available: false, claimed_by: 'm-a' }),
    quest({ id: 'q2', name: 'Bens quest', available: false, claimed_by: 'm-b' }),
    quest({ id: 'q3', name: 'Own waiting', available: false, claimed_by: 'm-a', done_at: '2025-01-03T00:00:00Z', awaiting_approval: true }),
  ];
  await loadApp();
  await becomeAnn();
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.equal(buttons(rowOf('Own quest'), 'Mark done').length, 1);
  assert.equal(buttons(rowOf('Own quest'), 'Abandon').length, 1);
  assert.equal(buttons(rowOf('Bens quest'), 'Mark done').length + buttons(rowOf('Bens quest'), 'Abandon').length, 0);
  assert.match(rowOf('Bens quest').textContent, /Taken by Ben/);
  assert.match(rowOf('Own waiting').textContent, /Waiting for approval/);
  assert.equal(walk(rowOf('Own waiting')).filter((n) => n.tagName === 'BUTTON').length, 0);
});

test('CLM-5: Mark done posts the member, shows Waiting for approval, and points stay as the server has them', async () => {
  srv.quests = [quest({ available: false, claimed_by: 'm-a' })];
  srv.extra[`POST ${HP}/quests/q1/done`] = () => {
    srv.quests = [quest({ available: false, claimed_by: 'm-a', done_at: '2025-01-03T00:00:00Z', awaiting_approval: true })];
    return [200, srv.quests[0]];
  };
  await loadApp();
  await becomeAnn();
  assert.match(text(), /Viewing as: Ann · 0 points/);
  await click(buttons(env.$app, 'Mark done')[0]);
  const [done] = actionCalls();
  assert.equal(done.path, `${HP}/quests/q1/done`);
  assert.deepEqual(done.body, { member_id: 'm-a', pin: '' });
  assert.match(section(env.$app, 'Mine'), /Feed the cat.*Waiting for approval/);
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

test('CLM-7: points show in the picker, header and Family tab, and refresh after an approval', async () => {
  srv.pending = () => [200, { items: srv.quests.filter((q) => q.awaiting_approval).map((q) => ({ ...q, member_name: 'Ann' })) }];
  srv.quests = [quest({ available: false, claimed_by: 'm-a', awaiting_approval: true, done_at: '2025-01-03T00:00:00Z' })];
  srv.extra[`POST ${HP}/quests/q1/approve`] = () => {
    srv.members = srv.members.map((m) => (m.id === 'm-a' ? { ...m, points: 5 } : m));
    srv.quests = [quest({ available: false, completed_at: '2025-01-04T00:00:00Z', available_at: '2999-01-01T00:00:00Z' })];
    return [200, srv.quests[0]];
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

  let pendingItems = [{ ...quest({ id: 'q9', awaiting_approval: true, claimed_by: 'm-a' }), member_name: 'Ann' }];
  srv.pending = () => [200, { items: pendingItems }];
  srv.extra[`POST ${HP}/quests/q9/approve`] = () => { pendingItems = []; return err(404, 'not_found', 'Not found.'); };
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
    { ...quest({ id: 'q1', name: 'Feed the cat', points: 5, awaiting_approval: true, claimed_by: 'm-a' }), member_name: 'Ann' },
    { ...quest({ id: 'q2', name: 'Take out trash', points: 10, awaiting_approval: true, claimed_by: 'm-b' }), member_name: 'Ben' },
  ];
  srv.pending = (r) => (r.headers['X-Admin-PIN'] === '9999' ? [200, { items }] : err(403, 'admin_pin_required'));
  await loadApp();
  assert.match(text(), /Enter the admin PIN to review approvals\./);
  await unlockAdmin('9999');

  const req = calls.filter((c) => c.path === `${HP}/quests/pending`).at(-1);
  assert.equal(req.headers['X-Admin-PIN'], '9999');
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.match(rowOf('Feed the cat').textContent, /Ann · 5 points/);
  assert.match(rowOf('Take out trash').textContent, /Ben · 10 points/);
  for (const name of ['Feed the cat', 'Take out trash']) {
    assert.equal(buttons(rowOf(name), 'Approve').length, 1);
    assert.equal(buttons(rowOf(name), 'Send back').length, 1);
  }

  items = [];
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));
  assert.match(text(), /Nothing waiting for approval\./);
});

test('CLM-11: Approve and Send back post an empty body with X-Admin-PIN; a sent-back quest is the member\'s to mark done again', async () => {
  srv.adminPinSet = true;
  srv.quests = [
    quest({ id: 'q1', available: false, claimed_by: 'm-a', awaiting_approval: true, done_at: '2025-01-03T00:00:00Z' }),
    quest({ id: 'q2', name: 'Water plants', available: false, claimed_by: 'm-a', awaiting_approval: true, done_at: '2025-01-03T00:00:00Z' }),
  ];
  srv.pending = () => [200, { items: srv.quests.filter((q) => q.awaiting_approval).map((q) => ({ ...q, member_name: 'Ann' })) }];
  srv.extra[`POST ${HP}/quests/q1/approve`] = () => {
    srv.members = srv.members.map((m) => (m.id === 'm-a' ? { ...m, points: 5 } : m));
    srv.quests = srv.quests.map((q) => (q.id === 'q1' ? quest({ available: false, completed_at: '2025-01-04T00:00:00Z', available_at: '2999-01-01T00:00:00Z' }) : q));
    return [200, {}];
  };
  srv.extra[`POST ${HP}/quests/q2/reject`] = () => {
    srv.quests = srv.quests.map((q) => (q.id === 'q2' ? { ...q, done_at: null, awaiting_approval: false } : q));
    return [200, {}];
  };
  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  const rowOf = (name) => walk(approvals()).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  await click(buttons(rowOf('Feed the cat'), 'Approve')[0]);
  const approve = actionCalls().at(-1);
  assert.equal(approve.method, 'POST');
  assert.equal(approve.path, `${HP}/quests/q1/approve`);
  assert.deepEqual(approve.body, {});
  assert.equal(approve.headers['X-Admin-PIN'], '9999');
  assert.match(text(), /Ann: 5 points/);
  assert.equal(rowOf('Feed the cat'), undefined);

  await click(buttons(rowOf('Water plants'), 'Send back')[0]);
  const reject = actionCalls().at(-1);
  assert.equal(reject.path, `${HP}/quests/q2/reject`);
  assert.deepEqual(reject.body, {});
  assert.equal(reject.headers['X-Admin-PIN'], '9999');
  assert.match(text(), /Nothing waiting for approval\./);
  assert.match(text(), /Ann: 5 points/);

  await becomeAnn();
  const mine = sectionNode(env.$app, 'Mine');
  assert.match(mine.textContent, /Water plants/);
  assert.equal(buttons(mine, 'Mark done').length, 1);
  assert.match(text(), /Viewing as: Ann · 5 points/);
});

test('CLM-12: a 403 admin-PIN error says so, keeps the list and the session', async () => {
  srv.adminPinSet = true;
  const items = [{ ...quest({ id: 'q1', awaiting_approval: true, claimed_by: 'm-a' }), member_name: 'Ann' }];
  srv.pending = () => [200, { items }];
  srv.extra[`POST ${HP}/quests/q1/approve`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
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
