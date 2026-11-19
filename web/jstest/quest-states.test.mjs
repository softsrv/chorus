// Unit tests for the Quests tab's empty states, streak bonus and combos read.
// Run: node --test web/jstest/

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, walk, buttons, click, submit, section, sectionNode } from './harness.mjs';
import {
  setup, env, srv, calls, HP, quest, err, text, becomeAnn, alerts, noInlineStyle, openTab,
  unlockAdmin,
} from './quests-fixture.mjs';

beforeEach(setup);

// ── CLM-3: empty states ───────────────────────────────────────────────────────

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
