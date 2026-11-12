// Unit tests for the Combos card in Quests tab (combos CLM-1..7).
// Run: node --test web/jstest/combos.test.mjs

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import {
  installBrowser, installFetch, loadApp, walk, click, submit, buttons, buttonStarting,
} from './harness.mjs';

const H = 'h1';
const HP = `/households/${H}`;
const REFRESH_KEY = 'chorus.refresh_token';

let env;
let srv;
let calls;
let signedOut;

const quest = (over) => ({
  id: 'q1', household_id: H, library_id: null, name: 'Feed the cat', points: 5, repeat_days: 1,
  notes: '', available: true, claimed_by: null, claimed_at: null,
  done_at: null, awaiting_approval: false, completed_at: null, available_at: null,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z', ...over,
});

const combo = (over) => ({
  id: 'c1', name: 'Kitchen Crew', bonus_points: 20, quest_ids: ['q1', 'q2'], ...over,
});

const err = (status, code, message = code, fields) => [status, { error: { code, message, ...(fields ? { fields } : {}) } }];

function routes() {
  return {
    'POST /auth/refresh': () => [200, {
      access_token: 'at', refresh_token: 'rt',
      user: { id: 'u1', email: 'p@example.com', display_name: 'Parent', email_verified: true },
    }],
    'GET /households': () => [200, { items: [{ id: H, name: 'Home', timezone: 'UTC', role: 'admin' }] }],
    [`GET ${HP}/admin-pin`]: () => [200, { set: srv.adminPinSet }],
    [`POST ${HP}/admin-pin/verify`]: (r) => (r.body.pin === '9999' ? [204, null] : err(403, 'wrong_pin', 'Wrong PIN.')),
    [`GET ${HP}/quests`]: () => [200, { items: srv.quests, next_offset: null }],
    [`GET ${HP}/combos`]: () => [200, { items: srv.combos }],
    'GET /quest-library': () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
    [`POST ${HP}/family-members/m-a/verify`]: () => [204, null],
    [`GET ${HP}/quests/pending`]: () => [200, { items: [] }],
    ...srv.extra,
  };
}

beforeEach(() => {
  env = installBrowser();
  sessionStorage.setItem(REFRESH_KEY, 'rt');
  srv = {
    adminPinSet: false,
    quests: [
      quest({ id: 'q1', name: 'Load dishwasher', points: 10 }),
      quest({ id: 'q2', name: 'Wipe counters', points: 10 }),
      quest({ id: 'q3', name: 'Fold laundry', points: 15 }),
      quest({ id: 'q4', name: 'Vacuum living room', points: 20 }),
    ],
    combos: [
      combo({ id: 'c1', name: 'Kitchen Crew', bonus_points: 20, quest_ids: ['q1', 'q2'] }),
      combo({ id: 'c2', name: 'Laundry Day', bonus_points: 15, quest_ids: ['q3', 'q4'] }),
    ],
    members: [{ id: 'm-a', household_id: H, name: 'Ann', has_pin: false, points: 0 }],
    extra: {},
  };
  signedOut = 0;
  window.addEventListener('chorus:signed-out', () => { signedOut += 1; });
  calls = installFetch(new Proxy({}, { get: (_, k) => routes()[k] }));
});

const text = () => env.$app.textContent;

test('CLM-1: while management is visible, Quests tab renders combos card showing name, chore names, and bonus', async () => {
  await loadApp();

  const fullText = text();
  assert.match(fullText, /Manage combos/);

  // Check combo 1
  assert.match(fullText, /Kitchen Crew/);
  assert.match(fullText, /Load dishwasher, Wipe counters/);
  assert.match(fullText, /\+20/);

  // Check combo 2
  assert.match(fullText, /Laundry Day/);
  assert.match(fullText, /Fold laundry, Vacuum living room/);
  assert.match(fullText, /\+15/);

  // Verify GET /households/{id}/combos was called
  const comboCalls = calls.filter((c) => c.method === 'GET' && c.path === `${HP}/combos`);
  assert.equal(comboCalls.length, 1);
});

async function becomeAnn() {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ann'));
}

test('CLM-7: member view renders no Manage combos card', async () => {
  srv.adminPinSet = true;
  await loadApp();
  await becomeAnn();

  assert.doesNotMatch(text(), /Manage combos/);
});

test('CLM-7: management visible but locked renders no Add/Edit/Delete controls for combos', async () => {
  srv.adminPinSet = true;
  await loadApp();

  assert.match(text(), /Manage combos/);
  assert.equal(buttons(env.$app, 'Edit').length, 0);
  assert.equal(buttons(env.$app, 'Delete').length, 0);
  assert.equal(buttons(env.$app, 'Add combo').length, 0);
  assert.equal(buttons(env.$app, 'Create combo').length, 0);
  const forms = walk(env.$app).filter((n) => n.tagName === 'FORM');
  for (const f of forms) {
    assert.doesNotMatch(f.textContent, /Create combo|Add combo|Bonus points/);
  }
});


// ── CLM-2..6: add, edit, delete, refused saves, empty state, text only ──────

const comboWrites = () => calls.filter((c) => c.method !== 'GET' && /\/combos/.test(c.path));
const formWithSubmit = (label) => buttons(env.$app, label)[0].parentNode;
const inputIn = (root, name) => walk(root).find((n) => ['INPUT', 'SELECT'].includes(n.tagName) && n.name === name);
const hintOf = ($input) => $input.parentNode.childNodes.at(-1);
const comboRow = (name) => walk(env.$app).find((n) => n.tagName === 'TR' && n.textContent.startsWith(name));
const comboTable = () => walk(env.$app).find((n) => n.tagName === 'TABLE');

async function unlockAdmin(pin) {
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = pin;
  await submit($pin.parentNode.parentNode);
}

// fillCombo types a name and bonus and selects the given quest ids.
async function fillCombo(formNode, { name, bonus, questIds }) {
  inputIn(formNode, 'name').value = name;
  inputIn(formNode, 'bonus_points').value = bonus;
  for (const o of inputIn(formNode, 'quest_ids').children) o.selected = questIds.includes(o.value);
  await submit(formNode);
}

function assertPinOnlyInHeader(c) {
  assert.equal(c.headers['X-Admin-PIN'], '9999');
  assert.doesNotMatch(c.url, /9999/);
  assert.doesNotMatch(JSON.stringify(c.body ?? null), /9999/);
}

function crudServer() {
  srv.extra[`POST ${HP}/combos`] = (r) => {
    const created = combo({ id: 'c3', ...r.body });
    srv.combos = [...srv.combos, created];
    return [201, created];
  };
  srv.extra[`PUT ${HP}/combos/c1`] = (r) => {
    srv.combos = srv.combos.map((c) => (c.id === 'c1' ? { ...c, ...r.body } : c));
    return [200, srv.combos[0]];
  };
  srv.extra[`DELETE ${HP}/combos/c2`] = () => { srv.combos = srv.combos.filter((c) => c.id !== 'c2'); return [204, null]; };
}

test('CLM-2: Create POSTs name, bonus and chosen chores; Edit PUTs; Delete on the confirm click; PIN only in the header', async () => {
  srv.adminPinSet = true;
  crudServer();
  await loadApp();
  await unlockAdmin('9999');

  const picker = inputIn(formWithSubmit('Create combo'), 'quest_ids');
  assert.deepEqual(Array.from(picker.children).map((o) => o.textContent),
    ['Load dishwasher', 'Wipe counters', 'Fold laundry', 'Vacuum living room']);

  await fillCombo(formWithSubmit('Create combo'), { name: 'Bathroom Blitz', bonus: '25', questIds: ['q1', 'q4'] });
  const post = comboWrites().at(-1);
  assert.equal(post.method, 'POST');
  assert.equal(post.path, `${HP}/combos`);
  assert.deepEqual(post.body, { name: 'Bathroom Blitz', bonus_points: 25, quest_ids: ['q1', 'q4'] });
  assertPinOnlyInHeader(post);
  assert.match(comboRow('Bathroom Blitz').textContent, /Load dishwasher, Vacuum living room\+25/);

  await click(buttons(comboRow('Kitchen Crew'), 'Edit')[0]);
  const editForm = formWithSubmit('Save');
  assert.equal(inputIn(editForm, 'name').value, 'Kitchen Crew');
  assert.deepEqual(Array.from(inputIn(editForm, 'quest_ids').children).filter((o) => o.selected).map((o) => o.value), ['q1', 'q2']);
  await fillCombo(editForm, { name: 'Kitchen Heroes', bonus: '30', questIds: ['q1', 'q2', 'q3'] });
  const put = comboWrites().at(-1);
  assert.equal(put.method, 'PUT');
  assert.equal(put.path, `${HP}/combos/c1`);
  assert.deepEqual(put.body, { name: 'Kitchen Heroes', bonus_points: 30, quest_ids: ['q1', 'q2', 'q3'] });
  assertPinOnlyInHeader(put);
  assert.match(comboRow('Kitchen Heroes').textContent, /Load dishwasher, Wipe counters, Fold laundry\+30/);

  const before = comboWrites().length;
  const del = buttons(comboRow('Laundry Day'), 'Delete')[0];
  await click(del);
  assert.equal(comboWrites().length, before, 'first click only arms');
  assert.equal(del.textContent, 'Sure?');
  await click(del);
  const d = comboWrites().at(-1);
  assert.equal(d.method, 'DELETE');
  assert.equal(d.path, `${HP}/combos/c2`);
  assertPinOnlyInHeader(d);
  assert.equal(comboRow('Laundry Day'), undefined);
});

test('CLM-2: a blank bonus is sent as typed, not as 0', async () => {
  crudServer();
  srv.extra[`POST ${HP}/combos`] = () => err(422, 'validation_failed', 'Check the highlighted fields.', { bonus_points: 'required' });
  await loadApp();
  await fillCombo(formWithSubmit('Create combo'), { name: 'X', bonus: '', questIds: ['q1', 'q2'] });
  assert.equal(comboWrites().at(-1).body.bonus_points, '');
});

test('CLM-3: a 422 shows every field error, keeps what was typed, and leaves the list unchanged', async () => {
  srv.extra[`POST ${HP}/combos`] = () => err(422, 'validation_failed', 'Check the highlighted fields.', {
    name: 'is required', bonus_points: 'must be 0 or more', quest_ids: 'pick at least two chores',
  });
  await loadApp();
  const f = formWithSubmit('Create combo');
  await fillCombo(f, { name: '  ', bonus: '-5', questIds: ['q3'] });
  assert.equal(hintOf(inputIn(f, 'name')).textContent, 'is required');
  assert.equal(hintOf(inputIn(f, 'bonus_points')).textContent, 'must be 0 or more');
  assert.equal(hintOf(inputIn(f, 'quest_ids')).textContent, 'pick at least two chores');
  assert.equal(inputIn(f, 'name').value, '  ');
  assert.equal(inputIn(f, 'bonus_points').value, '-5');
  assert.deepEqual(Array.from(inputIn(f, 'quest_ids').children).filter((o) => o.selected).map((o) => o.value), ['q3']);
  assert.equal(walk(comboTable()).filter((n) => n.tagName === 'TR').length, 3, 'header + the two combos');
  assert.ok(comboRow('Kitchen Crew') && comboRow('Laundry Day'));
});

test('CLM-3: a 403 wrong_pin on a combo save is reported as a PIN error and keeps the session', async () => {
  srv.adminPinSet = true;
  srv.extra[`PUT ${HP}/combos/c1`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await loadApp();
  await unlockAdmin('9999');
  await click(buttons(comboRow('Kitchen Crew'), 'Edit')[0]);
  await fillCombo(formWithSubmit('Save'), { name: 'Renamed', bonus: '5', questIds: ['q1', 'q2'] });
  assert.match(formWithSubmit('Save').textContent, /Wrong PIN\./);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
  assert.equal(signedOut, 0);
  assert.deepEqual(srv.combos.map((c) => c.name), ['Kitchen Crew', 'Laundry Day']);
});

test('CLM-4: with no combos the card says so, renders no table, and still offers the create form', async () => {
  srv.combos = [];
  await loadApp();
  assert.match(text(), /No combos yet/);
  assert.match(text(), /Create one to reward chores done together on the same day\./);
  assert.equal(comboTable(), undefined);
  assert.equal(buttons(env.$app, 'Create combo').length, 1);
});

test('CLM-6: combo and chore names with markup render as literal text in the list and the picker', async () => {
  const evil = '<img src=x onerror=1>';
  srv.quests = [quest({ id: 'q1', name: evil }), quest({ id: 'q2', name: 'Wipe counters' })];
  srv.combos = [combo({ id: 'c1', name: evil, quest_ids: ['q1', 'q2'] })];
  await loadApp();
  const row = comboRow(evil);
  assert.ok(row, 'combo name shown literally');
  const [nameCell, choresCell] = row.children;
  assert.ok(nameCell.childNodes[0] instanceof Text);
  assert.equal(nameCell.childNodes[0].data, evil);
  assert.equal(choresCell.textContent, `${evil}, Wipe counters`);
  const opt = Array.from(inputIn(formWithSubmit('Create combo'), 'quest_ids').children)[0];
  assert.ok(opt.childNodes[0] instanceof Text);
  assert.equal(opt.childNodes[0].data, evil);
  assert.equal(walk(env.$app).filter((n) => n.tagName === 'IMG').length, 0);
});
