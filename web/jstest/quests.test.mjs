// Unit tests for the Quests tab's take-quest and approval flows (CLM-1..12).
// Run: node --test web/jstest/quests.test.mjs (whole suite: make test-js)
//
// app.js and api.js run as shipped against a minimal DOM; only the server
// (fetch) is a stub, answering with the JSON shapes internal/http/handlers
// emits (questJSON, pendingQuestJSON, familyMemberJSON, the error envelope).

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import {
  loadApp, settle, walk, buttons, buttonStarting, click, submit, section, sectionNode,
} from './harness.mjs';
import * as memberCardAdminRows from './fixtures/member-card-admin-rows.fixture.mjs';
import {
  setup, env, srv, calls, signedOut, HP, REFRESH_KEY, quest, err, text, actionCalls, becomeAnn,
  becomeBen, backToAdmin, alerts, field, unlockAdmin,
} from './quests-fixture.mjs';

beforeEach(setup);

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

test('CLM-14: a held quest shows its claim_expires_at as due-by text; an expired (available) quest is under Available, not Mine', async () => {
  const due = '2030-06-15T05:00:00Z';
  srv.quests = [
    quest({ id: 'q1', name: 'Own quest', available: false, claimed_by: 'm-a', claim_expires_at: due }),
    quest({ id: 'q2', name: 'Expired quest', available: true, claimed_by: null, claim_expires_at: null }),
    quest({ id: 'q3', name: 'Bens quest', available: false, claimed_by: 'm-b', claim_expires_at: due }),
  ];
  await loadApp();
  await becomeAnn();
  const rowOf = (name) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
  assert.ok(rowOf('Own quest').textContent.includes(`Due by ${new Date(due).toLocaleString()}`));
  assert.match(section(env.$app, 'Available'), /Expired quest/);
  assert.doesNotMatch(section(env.$app, 'Mine'), /Expired quest/);
  assert.doesNotMatch(rowOf('Expired quest').textContent, /Due by/);
  assert.doesNotMatch(rowOf('Bens quest').textContent, /Due by/);
});

test('CLM-14: due-by text is a text node, never interpreted as HTML', async () => {
  srv.quests = [quest({ id: 'q1', name: 'Own quest', available: false, claimed_by: 'm-a', claim_expires_at: '<img src=x onerror=1>' })];
  await loadApp();
  await becomeAnn();
  const own = walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Own quest'));
  assert.equal(walk(own).filter((n) => n.tagName === 'IMG').length, 0);
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

test('admin points CLM-3/CLM-12: the member card shows admin rows signed with (from admin), read-only, and the new total in header and picker', async () => {
  srv.members = memberCardAdminRows.members;
  srv.history = memberCardAdminRows.pointsHistory['m-maya'];
  srv.extra[`POST ${HP}/family-members/m-maya/verify`] = (r) => (r.body.pin === '4321' ? [204, null] : err(403, 'wrong_pin'));
  await loadApp();
  await click(buttons(env.$app, 'Switch member')[0]);
  assert.ok(buttonStarting(env.$app, 'Maya · 42 points'), 'picker shows the adjusted total');
  await click(buttonStarting(env.$app, 'Maya'));
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = '4321';
  await submit($pin.parentNode.parentNode);

  const histCall = calls.find((c) => c.path === `${HP}/family-members/m-maya/points-history`);
  assert.equal(histCall.headers['X-Member-PIN'], '4321');
  assert.match(text(), /Viewing as: Maya · 42 points/);

  const historyNode = sectionNode(env.$app, 'Points history');
  const trs = walk(historyNode).filter((n) => n.tagName === 'TBODY').flatMap((b) => walk(b).filter((n) => n.tagName === 'TR'));
  const rowData = trs.map((tr) => walk(tr).filter((n) => n.tagName === 'TD').map((td) => td.textContent));
  assert.deepEqual(rowData.map((r) => [r[1], r[2]]), [
    ['+10', 'helped grandma (from admin)'],
    ['-5', 'left toys out (from admin)'],
    ['+5', 'Quest: Feed the cat'],
  ]);
  assert.ok(!historyNode.textContent.includes('(admin adjustment)'), 'member card uses its own label');
  assert.equal(walk(historyNode).filter((n) => ['FORM', 'INPUT', 'BUTTON'].includes(n.tagName)).length, 0, 'history is read-only');
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

// ── Reward history on the member card ────────────────────────────────────────

const rewardHistoryReads = () => calls.filter((c) => c.method === 'GET' && c.path.endsWith('/reward-history'));

test('reward history CLM-7/CLM-9: picking a member reads reward-history once with X-Member-PIN and shows it under Points history', async () => {
  srv.history = [{ id: 'p1', points: -100, reason: 'Redeemed: Movie night', earned_at: '2025-11-18T12:00:00Z' }];
  srv.rewardHistory = () => [200, { items: [
    { id: 'rr1', member_id: 'm-b', reward_id: 'r-movie', description: 'Movie night', cost: 100, status: 'approved', requested_at: '2025-11-17T12:00:00Z', decided_at: '2025-11-18T12:00:00Z' },
  ] }];
  srv.extra[`POST ${HP}/quests/q1/accept`] = (r) => {
    srv.quests = [quest({ available: false, claimed_by: r.body.member_id, claimed_at: '2025-01-02T00:00:00Z' })];
    return [200, srv.quests[0]];
  };
  await loadApp();
  await becomeBen();

  const reads = rewardHistoryReads();
  assert.equal(reads.length, 1, 'one reward-history read per selection');
  assert.equal(reads[0].path, `${HP}/family-members/m-b/reward-history`);
  assert.equal(reads[0].headers['X-Member-PIN'], '1234');

  const h4s = walk(env.$app).filter((n) => n.tagName === 'H4').map((n) => n.textContent);
  assert.ok(h4s.indexOf('Points history') >= 0 && h4s.indexOf('Reward history') === h4s.indexOf('Points history') + 1,
    'Reward history follows Points history');
  const node = sectionNode(env.$app, 'Reward history');
  assert.deepEqual(walk(node).filter((n) => n.tagName === 'TH').map((n) => n.textContent), ['Date', 'Reward', 'Points']);
  const tds = walk(node).filter((n) => n.tagName === 'TD');
  assert.deepEqual(tds.slice(1).map((td) => td.textContent), ['Movie night', '100']);
  for (const td of tds) assert.equal(td.children.length, 0, 'cells hold text only');
  // The points history keeps its own signed redemption row.
  assert.match(sectionNode(env.$app, 'Points history').textContent, /-100Redeemed: Movie night/);

  // An action rerenders without reading it again.
  await click(buttons(env.$app, 'View')[0]);
  await click(buttons(env.$app, 'Accept')[0]);
  assert.equal(rewardHistoryReads().length, 1, 'no re-read on an action');
  assert.match(sectionNode(env.$app, 'Reward history').textContent, /Movie night/);
});

test('reward history CLM-10: no redeemed rewards shows the empty text and no table on the member card', async () => {
  await loadApp();
  await becomeAnn();
  const node = sectionNode(env.$app, 'Reward history');
  assert.ok(node, 'Reward history section exists');
  assert.deepEqual(walk(node).filter((n) => n.tagName === 'P').map((n) => n.textContent), ['No rewards redeemed yet']);
  assert.equal(walk(node).filter((n) => n.tagName === 'TABLE').length, 0, 'no table');
});

test('reward history CLM-7: a late answer for a member picked earlier is not rendered under the member picked since', async () => {
  srv.rewardHistory = (r) => [200, { items: [{ id: r.path.includes('/m-a/') ? 'ra' : 'rb', description: r.path.includes('/m-a/') ? 'Ann treat' : 'Ben treat', cost: 5, status: 'approved', decided_at: '2025-01-02T12:00:00Z' }] }];
  await loadApp();
  const realFetch = globalThis.fetch;
  let releaseAnn;
  const annHeld = new Promise((r) => { releaseAnn = r; });
  globalThis.fetch = async (url, init) => {
    const res = realFetch(url, init);
    if (url.includes('/family-members/m-a/reward-history')) await annHeld;
    return res;
  };
  await becomeAnn();
  await becomeBen();
  releaseAnn();
  await settle();
  globalThis.fetch = realFetch;
  assert.match(text(), /Quests for Ben/);
  const node = sectionNode(env.$app, 'Reward history');
  assert.match(node.textContent, /Ben treat/);
  assert.doesNotMatch(text(), /Ann treat/);
});

test('reward history CLM-7: a failing reward-history read stays in its section; wrong_pin returns to the picker', async () => {
  srv.rewardHistory = () => err(429, 'rate_limited', 'Too many requests.');
  await loadApp();
  await becomeAnn();
  assert.match(sectionNode(env.$app, 'Reward history').textContent, /Could not load reward history/);
  assert.equal(walk(sectionNode(env.$app, 'Points history')).filter((n) => n.tagName === 'P').length, 1, 'points history renders');
  assert.match(section(env.$app, 'Available'), /Feed the cat/);

  srv.rewardHistory = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await becomeBen();
  assert.doesNotMatch(text(), /Viewing as: Ben/);
  assert.equal(sectionNode(env.$app, 'Reward history'), null);
  assert.equal(signedOut, 0);
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

// ── Upcoming (upcoming CLM-1..4) ──────────────────────────────────────────────

const upcomingCalls = (id) => calls.filter((c) => c.method === 'GET' && c.path === `${HP}/family-members/${id}/upcoming-quests`);

test('upcoming CLM-1: with no upcoming quests the Upcoming section still renders exactly You have no upcoming quests.', async () => {
  await loadApp();
  await becomeAnn();
  const node = sectionNode(env.$app, 'Upcoming');
  assert.ok(node, 'Upcoming section exists');
  assert.deepEqual(walk(node).filter((n) => n.tagName === 'P').map((n) => n.textContent), ['You have no upcoming quests.']);
  assert.equal(walk(node).filter((n) => n.tagName === 'LI').length, 0);
});

test('upcoming CLM-3: takeCard reads the member\'s upcoming-quests and lists them in API order between Available and Claimed, marking repeats', async () => {
  srv.upcoming = () => [200, {
    items: [
      { schedule_id: 's1', quest_id: 'q1', quest_name: 'Feed the cat', next_date: '2026-11-22', repeat: true },
      { schedule_id: 's2', quest_id: 'q2', quest_name: 'Walk dog', next_date: '2026-11-24', repeat: false },
    ],
  }];
  await loadApp();
  await becomeAnn();
  assert.equal(upcomingCalls('m-a').length, 1, 'read once for the render');
  const node = sectionNode(env.$app, 'Upcoming');
  const rows = walk(node).filter((n) => n.tagName === 'LI');
  assert.deepEqual(rows.map((li) => li.children[0].textContent), ['Sun 22 Nov — Feed the cat', 'Tue 24 Nov — Walk dog']);
  assert.equal((node.textContent.match(/repeats/g) || []).length, 1, 'one repeats mark');
  assert.match(rows[0].textContent, /repeats$/);
  const h4s = walk(env.$app).filter((n) => n.tagName === 'H4').map((n) => n.textContent);
  assert.ok(h4s.indexOf('Available') < h4s.indexOf('Upcoming'), 'after Available');
  assert.ok(h4s.indexOf('Upcoming') < h4s.indexOf('Claimed'), 'before Claimed');
});

test('upcoming CLM-3: a date is shown as the calendar day sent, with no timezone shift', async () => {
  const realTZ = process.env.TZ;
  process.env.TZ = 'America/Los_Angeles';
  try {
    srv.upcoming = () => [200, { items: [{ schedule_id: 's1', quest_id: 'q1', quest_name: 'Feed the cat', next_date: '2026-01-01', repeat: false }] }];
    await loadApp();
    await becomeAnn();
    assert.equal(walk(sectionNode(env.$app, 'Upcoming')).find((n) => n.tagName === 'LI').children[0].textContent, 'Thu 1 Jan — Feed the cat');
  } finally {
    if (realTZ === undefined) delete process.env.TZ; else process.env.TZ = realTZ;
  }
});

test('upcoming CLM-2: a quest name with markup shows as literal text in Upcoming and creates no element', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  srv.upcoming = () => [200, { items: [{ schedule_id: 's1', quest_id: 'q1', quest_name: evil, next_date: '2026-11-22', repeat: false }] }];
  await loadApp();
  await becomeAnn();
  const node = sectionNode(env.$app, 'Upcoming');
  assert.equal(walk(node).find((n) => n.tagName === 'LI').textContent, `Sun 22 Nov — ${evil}`);
  assert.equal(walk(env.$app).filter((n) => n.tagName === 'IMG').length, 0);
  for (const n of walk(node)) assert.equal(n.getAttribute?.('style') ?? null, null, 'no inline style');
});

test('upcoming CLM-4: a failed upcoming read shows an inline error in Upcoming only; Available, Mine and Claimed render; no sign-out or member change', async () => {
  srv.quests = [
    quest({ id: 'q1', name: 'Free one' }),
    quest({ id: 'q2', name: 'Anns one', available: false, claimed_by: 'm-a' }),
    quest({ id: 'q3', name: 'Bens one', available: false, claimed_by: 'm-b' }),
  ];
  srv.upcoming = () => err(500, 'internal', 'boom');
  await loadApp();
  await becomeAnn();
  assert.match(section(env.$app, 'Upcoming'), /Could not load upcoming quests\. Reload to try again\./);
  assert.match(section(env.$app, 'Available'), /Free one/);
  assert.match(section(env.$app, 'Mine'), /Anns one/);
  assert.match(section(env.$app, 'Claimed'), /Bens one/);
  for (const s of ['Available', 'Mine', 'Claimed']) assert.doesNotMatch(section(env.$app, s), /Could not load/);
  assert.match(text(), /Quests for Ann/);
  assert.equal(signedOut, 0);
});
