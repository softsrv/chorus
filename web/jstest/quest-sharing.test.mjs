// Unit tests for quest sharing in the Quests tab.
// Run: node --test web/jstest/

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, walk, buttons, buttonStarting, click, submit, section } from './harness.mjs';
import { SAM, AVA, LEO, MOW_LAWN, READ_CHAPTER, WATER_PLANTS } from './fixtures/age_range.mjs';
import {
  setup, env, srv, calls, signedOut, H, HP, quest, err, text, becomeAnn, becomeBen, backToAdmin,
  ageHousehold, alerts, become, field, noInlineStyle, unlockAdmin,
} from './quests-fixture.mjs';

beforeEach(setup);

// ── quest sharing (CLM-2, CLM-5, CLM-6, CLM-10) ──────────────────────────────

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

test('sharing CLM-8 / 1.1/3: share picker lists every candidate including out-of-range; Share still offered when every candidate is out of range, and the picker shows each disabled with its reason', async () => {
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

  // Every candidate ineligible: Share is still offered, and the picker shows
  // each member disabled with the reason; nothing is sent.
  srv.quests = [quest({
    id: 'q1', name: 'Clean living room', available: false, claimed_by: 'm-a', shareable: true,
    min_age: 10, max_age: 14, ineligible_member_ids: ['m-b', 'm-c'],
  })];
  await become('Ann');
  assert.equal(buttons(rowOf(), 'Share').length, 1, 'Share offered even when every candidate is ineligible');
  await click(buttons(rowOf(), 'Share')[0]);
  const allOut = walk(env.$app).find((n) => n.className === 'font-semibold' && n.textContent === 'Share Clean living room with').parentNode;
  const allOutButtons = walk(allOut).filter((n) => n.tagName === 'BUTTON' && n.textContent !== 'Cancel');
  assert.deepEqual(allOutButtons.map((n) => n.textContent), ['Ben', 'Cat'], 'every out-of-range member is listed');
  assert.ok(allOutButtons.every((n) => n.disabled), 'every out-of-range member is disabled');
  const reasons = walk(allOut).filter((n) => n.tagName === 'SMALL').map((n) => n.textContent);
  assert.deepEqual(reasons, ['Ages 10\u201314 only', 'Ages 10\u201314 only']);
  const before = calls.filter((c) => c.path === `${HP}/quests/q1/share`).length;
  for (const b of allOutButtons) await click(b);
  assert.equal(calls.filter((c) => c.path === `${HP}/quests/q1/share`).length, before, 'a disabled member sends nothing');

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

test('sharing CLM-11: a 409 age_restricted from share reloads the list, shows the age-range notice, and leaves the quest unshared', async () => {
  ageHousehold();
  srv.extra[`POST ${HP}/quests/q-mow/share`] = () => err(409, 'age_restricted', "member is outside this quest's age range");

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
  const leoBtn = buttons(picker, 'Leo')[0];
  assert.ok(leoBtn, 'Leo button rendered');
  assert.equal(leoBtn.disabled, false, 'Leo is eligible');

  await click(leoBtn);

  const shareAt = calls.findIndex((c) => c.path === `${HP}/quests/q-mow/share`);
  assert.ok(shareAt !== -1, 'share request sent');
  assert.ok(calls.slice(shareAt + 1).some((c) => c.method === 'GET' && c.path === `${HP}/quests`), 'list reloaded after 409');

  assert.match(text(), /This quest is outside that member's age range\./);
  const updatedRow = rowOf();
  assert.ok(updatedRow, 'quest still present');
  assert.doesNotMatch(updatedRow.textContent, /Shared with/);
  noInlineStyle();
});

test('sharing CLM-12: share picker decides eligibility only from ineligible_member_ids, ignores birthdays, and only includes household members', async () => {
  // Sam holds quest. Leo has birthday in fixture that would make him age 42, but is NOT in ineligible_member_ids.
  // Ava has no birthday.
  // Ineligible_member_ids has an outsider id not in household members.
  ageHousehold();
  srv.members = [
    { ...SAM, birthday: '2015-04-02' },
    { ...AVA, birthday: null },
    { ...LEO, birthday: '1980-01-01' },
  ];
  srv.quests = [quest({
    id: 'q-share', name: 'Board game', available: false, claimed_by: SAM.id, shareable: true,
    min_age: 10, max_age: 14, ineligible_member_ids: [AVA.id, 'outsider-id-123'],
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
