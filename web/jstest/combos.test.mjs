// Unit tests for the Combos card in Quests tab (combos CLM-1).
// Run: node --test web/jstest/combos.test.mjs

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import {
  installBrowser, installFetch, loadApp, walk, click, buttons, buttonStarting,
} from './harness.mjs';

const H = 'h1';
const HP = `/households/${H}`;
const REFRESH_KEY = 'chorus.refresh_token';

let env;
let srv;
let calls;

const quest = (over) => ({
  id: 'q1', household_id: H, library_id: null, name: 'Feed the cat', points: 5, repeat_days: 1,
  notes: '', available: true, claimed_by: null, claimed_at: null,
  done_at: null, awaiting_approval: false, completed_at: null, available_at: null,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z', ...over,
});

const combo = (over) => ({
  id: 'c1', name: 'Kitchen Crew', bonus_points: 20, quest_ids: ['q1', 'q2'], ...over,
});

function routes() {
  return {
    'POST /auth/refresh': () => [200, {
      access_token: 'at', refresh_token: 'rt',
      user: { id: 'u1', email: 'p@example.com', display_name: 'Parent', email_verified: true },
    }],
    'GET /households': () => [200, { items: [{ id: H, name: 'Home', timezone: 'UTC', role: 'admin' }] }],
    [`GET ${HP}/admin-pin`]: () => [200, { set: srv.adminPinSet }],
    [`GET ${HP}/quests`]: () => [200, { items: srv.quests, next_offset: null }],
    [`GET ${HP}/combos`]: () => [200, { items: srv.combos }],
    'GET /quest-library': () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
    [`POST ${HP}/family-members/m-a/verify`]: () => [204, null],
    [`GET ${HP}/quests/pending`]: () => [200, { items: [] }],
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
  };
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

