// Unit tests for quest age ranges and the Family tab's birthdays.
// Run: node --test web/jstest/

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, walk, buttons, click, submit, section, sectionNode } from './harness.mjs';
import {
  SAM, AVA, LEO, RANGE_QUESTS, FEED_CAT, MOW_LAWN, WATER_PLANTS, TIDY_ROOM, RAKE_LEAVES, BIRTHDAYS,
  MIN_ABOVE_MAX, RANGE_422, ageQuest,
} from './fixtures/age_range.mjs';
import { readFileSync } from 'node:fs';
import {
  setup, env, srv, calls, signedOut, H, HP, REFRESH_KEY, quest, err, routes, text, backToAdmin,
  ageHousehold, become, field, noInlineStyle, openTab, unlockAdmin,
} from './quests-fixture.mjs';

beforeEach(setup);

// ── age ranges (CLM-19..29) ─────────────────────────────────────────────────

const manageCard = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Manage quests').parentNode;
const manageRow = (name) => walk(manageCard()).find((n) => n.tagName === 'LI' && n.textContent.startsWith(name));
const radio = (root, value) => walk(root).find((n) => n.tagName === 'INPUT' && n.type === 'radio' && n.value === value);
const fieldsetOf = (root) => walk(root).find((n) => n.tagName === 'FIELDSET');
const lastPost = () => calls.filter((c) => c.method === 'POST' && c.path === `${HP}/quests`).at(-1);
const birthdayCalls = () => calls.filter((c) => /birthday/.test(c.path));

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
  const src = readFileSync(new URL('../static/js/quests.js', import.meta.url), 'utf8');
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
  const src = readFileSync(new URL('../static/js/family.js', import.meta.url), 'utf8');
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

test('CLM-17: approvals card lists pending reward requests with member name, description, signed points, and Approve/Reject buttons', async () => {
  srv.adminPinSet = true;
  let completions = [
    { completion_id: 'c1', quest_id: 'q1', name: 'Empty the dishwasher', points: 10, member_id: 'm-a', member_name: 'Alex', done_at: '2025-01-03T00:00:00Z' },
  ];
  let redemptions = [
    { id: 'red-1', household_id: H, member_id: 'm-sam', member_name: 'Sam', reward_id: 'r-movie', description: 'Movie night', cost: 100, status: 'pending', requested_at: '2025-01-03T12:00:00Z' },
  ];
  srv.pending = () => [200, { items: completions }];
  srv.pendingRewards = () => [200, { items: redemptions }];

  srv.extra[`POST ${HP}/reward-redemptions/red-1/approve`] = () => {
    redemptions = redemptions.filter((r) => r.id !== 'red-1');
    return [200, { redemption: { id: 'red-1', status: 'approved' } }];
  };
  srv.extra[`POST ${HP}/reward-redemptions/red-1/reject`] = () => {
    redemptions = redemptions.filter((r) => r.id !== 'red-1');
    return [200, { redemption: { id: 'red-1', status: 'rejected' } }];
  };

  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  assert.ok(walk(approvals()).some((n) => n.tagName === 'H4' && n.textContent === 'Reward requests'), 'Reward requests heading present');

  const lis = () => walk(approvals()).filter((n) => n.tagName === 'LI');
  assert.equal(lis().length, 2, 'one quest row and one redemption row');

  const questRow = lis()[0];
  assert.match(questRow.textContent, /Empty the dishwasher/);
  assert.equal(buttons(questRow, 'Approve').length, 1);
  assert.equal(buttons(questRow, 'Send back').length, 1);

  const rewardRow = lis()[1];
  assert.match(rewardRow.textContent, /Sam/);
  assert.match(rewardRow.textContent, /Reward: Movie night/);
  assert.match(rewardRow.textContent, /-100/);
  assert.equal(buttons(rewardRow, 'Approve').length, 1);
  assert.equal(buttons(rewardRow, 'Reject').length, 1);

  // Click Approve on the redemption row
  await click(buttons(rewardRow, 'Approve')[0]);
  const approveCall = calls.filter((c) => c.method === 'POST' && c.path.includes('/reward-redemptions/red-1/approve')).at(-1);
  assert.ok(approveCall, 'approve POST called');
  assert.deepEqual(approveCall.body, {});
  assert.equal(approveCall.headers['X-Admin-PIN'], '9999');

  // Verify row leaves the list after reload
  assert.equal(lis().length, 1);
  assert.doesNotMatch(approvals().textContent, /Reward: Movie night/);
  assert.ok(!walk(approvals()).some((n) => n.tagName === 'H4' && n.textContent === 'Reward requests'));

  // Test reject path with a fresh redemption
  redemptions = [
    { id: 'red-1', household_id: H, member_id: 'm-sam', member_name: 'Sam', reward_id: 'r-movie', description: 'Movie night', cost: 100, status: 'pending', requested_at: '2025-01-03T12:00:00Z' },
  ];
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));
  const rewardRow2 = lis()[1];
  assert.ok(rewardRow2);
  await click(buttons(rewardRow2, 'Reject')[0]);
  const rejectCall = calls.filter((c) => c.method === 'POST' && c.path.includes('/reward-redemptions/red-1/reject')).at(-1);
  assert.ok(rejectCall, 'reject POST called');
  assert.deepEqual(rejectCall.body, {});
  assert.equal(rejectCall.headers['X-Admin-PIN'], '9999');
  assert.equal(lis().length, 1);
});

test('CLM-18: a 422 answer to reward approve shows message inline and keeps the row and both buttons', async () => {
  srv.adminPinSet = true;
  srv.pending = () => [200, { items: [] }];
  srv.pendingRewards = () => [200, {
    items: [
      { id: 'red-pizza', household_id: H, member_id: 'm-alex', member_name: 'Alex', reward_id: 'r-pizza', description: 'Pizza night', cost: 150, status: 'pending', requested_at: '2025-01-03T12:00:00Z' },
    ],
  }];
  const msg = "Can't approve Pizza night for Alex: Alex has 90 points and it costs 150. Nothing was taken.";
  srv.extra[`POST ${HP}/reward-redemptions/red-pizza/approve`] = () => [422, { error: { code: 'insufficient_points', message: msg, fields: { points: msg } } }];

  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  const lis = () => walk(approvals()).filter((n) => n.tagName === 'LI');
  assert.equal(lis().length, 1);

  await click(buttons(lis()[0], 'Approve')[0]);

  assert.match(approvals().textContent, new RegExp(msg));
  assert.match(approvals().textContent, /Nothing was taken\./);
  assert.equal(lis().length, 1);
  assert.equal(buttons(lis()[0], 'Approve').length, 1);
  assert.equal(buttons(lis()[0], 'Reject').length, 1);
});

test('CLM-18: a 422 on reward approve shows the reason from fields.points, not a generic message', async () => {
  srv.adminPinSet = true;
  srv.pending = () => [200, { items: [] }];
  srv.pendingRewards = () => [200, {
    items: [
      { id: 'red-pizza', household_id: H, member_id: 'm-alex', member_name: 'Alex', reward_id: 'r-pizza', description: 'Pizza night', cost: 150, status: 'pending', requested_at: '2025-01-03T12:00:00Z' },
    ],
  }];
  const msg = "Can't approve Pizza night for Alex: Alex has 90 points and it costs 150. Nothing was taken.";
  srv.extra[`POST ${HP}/reward-redemptions/red-pizza/approve`] = () => [422, { error: { code: 'validation_failed', message: 'One or more fields are invalid.', fields: { points: msg } } }];

  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  await click(buttons(walk(approvals()).filter((n) => n.tagName === 'LI')[0], 'Approve')[0]);

  assert.match(approvals().textContent, /Nothing was taken\./);
  assert.doesNotMatch(approvals().textContent, /One or more fields are invalid/);
});

test('CLM-19: a 403 wrong_pin on reward approve/reject invokes wrongAdminPin and keeps the row', async () => {
  srv.adminPinSet = true;
  srv.pending = () => [200, { items: [] }];
  srv.pendingRewards = () => [200, {
    items: [
      { id: 'red-1', household_id: H, member_id: 'm-sam', member_name: 'Sam', reward_id: 'r-movie', description: 'Movie night', cost: 100, status: 'pending', requested_at: '2025-01-03T12:00:00Z' },
    ],
  }];
  srv.extra[`POST ${HP}/reward-redemptions/red-1/approve`] = () => err(403, 'wrong_pin', 'Wrong PIN.');

  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  const lis = () => walk(approvals()).filter((n) => n.tagName === 'LI');
  assert.equal(lis().length, 1);

  await click(buttons(lis()[0], 'Approve')[0]);

  assert.match(text(), /Wrong administrator PIN\./);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
  assert.equal(signedOut, 0);
});

test('CLM-20: approvals card empty state accounts for both quest and redemption lists', async () => {
  srv.adminPinSet = true;
  srv.pending = () => [200, { items: [] }];
  srv.pendingRewards = () => [200, { items: [] }];

  await loadApp();
  await unlockAdmin('9999');

  const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  assert.match(approvals().textContent, /Nothing waiting for approval\./);
  assert.doesNotMatch(approvals().textContent, /Reward requests/);
  assert.equal(walk(approvals()).filter((n) => n.tagName === 'TABLE' || n.tagName === 'UL').length, 0);

  // With only a redemption pending, the empty text is absent
  srv.pendingRewards = () => [200, {
    items: [
      { id: 'red-1', household_id: H, member_id: 'm-sam', member_name: 'Sam', reward_id: 'r-movie', description: 'Movie night', cost: 100, status: 'pending', requested_at: '2025-01-03T12:00:00Z' },
    ],
  }];
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === 'Quests'));

  const approvals2 = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  assert.doesNotMatch(approvals2().textContent, /Nothing waiting for approval\./);
  assert.match(approvals2().textContent, /Reward requests/);
});

test('1.1/4: a failing reward-requests read shows an inline error and the quest completions still render with their actions', async () => {
  for (const [status, code] of [[500, 'internal'], [429, 'rate_limited']]) {
    srv.adminPinSet = true;
    srv.pending = () => [200, { items: [
      { completion_id: 'c1', quest_id: 'q1', name: 'Empty the dishwasher', points: 10, member_id: 'm-a', member_name: 'Alex', done_at: '2025-01-03T00:00:00Z' },
    ] }];
    srv.pendingRewards = () => [status, { error: { code, message: 'Server trouble.' } }];

    await loadApp();
    await unlockAdmin('9999');

    const approvals = walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
    const row = walk(approvals).find((n) => n.tagName === 'LI' && n.textContent.includes('Empty the dishwasher'));
    assert.ok(row, `quest row renders when the reward read answers ${status}`);
    assert.equal(buttons(row, 'Approve').length, 1);
    assert.equal(buttons(row, 'Send back').length, 1);
    assert.match(approvals.textContent, /Could not load reward requests/);
    assert.doesNotMatch(approvals.textContent, /Nothing waiting for approval\./);
  }
});

test('1.1/4: with no quest completions, a failing reward-requests read never claims nothing is waiting', async () => {
  srv.adminPinSet = true;
  srv.pending = () => [200, { items: [] }];
  srv.pendingRewards = () => [500, { error: { code: 'internal', message: 'Server trouble.' } }];
  await loadApp();
  await unlockAdmin('9999');
  const approvals = walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
  assert.match(approvals.textContent, /Could not load reward requests/);
  assert.doesNotMatch(approvals.textContent, /Nothing waiting for approval\./);
});

test('1.1/8: a 404 on reward approve or reject (already decided or cancelled) reloads the card with a notice and the stale row leaves', async () => {
  for (const verb of ['approve', 'reject']) {
    srv.adminPinSet = true;
    srv.pending = () => [200, { items: [] }];
    let redemptions = [
      { id: 'red-1', household_id: H, member_id: 'm-sam', member_name: 'Sam', reward_id: 'r-movie', description: 'Movie night', cost: 100, status: 'pending', requested_at: '2025-01-03T12:00:00Z' },
    ];
    srv.pendingRewards = () => [200, { items: redemptions }];
    srv.extra[`POST ${HP}/reward-redemptions/red-1/${verb}`] = () => {
      // Decided in another session (or cancelled when the member left).
      redemptions = [];
      return err(404, 'not_found', 'Not found.');
    };

    await loadApp();
    await unlockAdmin('9999');

    const approvals = () => walk(env.$app).find((n) => n.tagName === 'H3' && n.textContent === 'Approvals').parentNode;
    const lis = () => walk(approvals()).filter((n) => n.tagName === 'LI');
    assert.equal(lis().length, 1, `${verb}: the request is listed`);

    await click(buttons(lis()[0], verb === 'approve' ? 'Approve' : 'Reject')[0]);

    assert.match(text(), /That reward request is no longer waiting for approval\./, `${verb}: notice shown`);
    assert.equal(lis().length, 0, `${verb}: stale row gone after the reload`);
    assert.match(approvals().textContent, /Nothing waiting for approval\./);
    assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
    delete srv.extra[`POST ${HP}/reward-redemptions/red-1/${verb}`];
  }
});
