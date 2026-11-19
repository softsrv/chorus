// Shared setup for the Quests tab tests: the stub server's routes and state,
// and the helpers quests.test.mjs and the quest-*.test.mjs files share. Each
// test file installs it with beforeEach(setup).

import assert from 'node:assert/strict';
import {
  installBrowser, installFetch, settle, walk, buttons, buttonStarting, click, submit,
} from './harness.mjs';
import { MEMBERS } from './fixtures/age_range.mjs';

export const H = 'h1';
export const HP = `/households/${H}`;
export const REFRESH_KEY = 'chorus.refresh_token';

// The per-test state. Only setup() reassigns these; the test files read them
// through ES live bindings, so each sees the values of the current test.
export let env;
export let srv; // mutable server state per test
export let calls;
export let signedOut;

export const quest = (over) => ({
  id: 'q1', household_id: H, library_id: null, name: 'Feed the cat', points: 5, repeat_days: 1,
  notes: 'Half a scoop, then fresh water.', available: true, claimed_by: null, claimed_at: null,
  awaiting_approval: false, completed_at: null, available_at: null,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
  shareable: false, teamwork_bonus: 0, shared: false, participants: [],
  min_age: null, max_age: null, ineligible_member_ids: [], ...over,
});

export const err = (status, code, message = code) => [status, { error: { code, message } }];

export function routes() {
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
    [`GET ${HP}/reward-redemptions/pending`]: (r) => (srv.pendingRewards ? srv.pendingRewards(r) : [200, { items: [] }]),
    ...srv.extra,
  };
  return new Proxy(r, {
    get(target, prop) {
      if (prop in target) return target[prop];
      if (typeof prop === 'string' && prop.startsWith(`GET ${HP}/family-members/`) && prop.endsWith('/points-history')) {
        return (req) => (srv.pointsHistory ? srv.pointsHistory(req) : [200, { items: srv.history || [] }]);
      }
      if (typeof prop === 'string' && prop.startsWith(`GET ${HP}/family-members/`) && prop.endsWith('/reward-history')) {
        return (req) => (srv.rewardHistory ? srv.rewardHistory(req) : [200, { items: [] }]);
      }
      if (typeof prop === 'string' && prop.startsWith(`GET ${HP}/family-members/`) && prop.endsWith('/upcoming-quests')) {
        return (req) => (srv.upcoming ? srv.upcoming(req) : [200, { items: [] }]);
      }
      return undefined;
    },
  });
}

export function setup() {
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
}

export const text = () => env.$app.textContent;
export const actionCalls = () => calls.filter((c) => /\/(quests\/[^/]+\/(accept|abandon|done)|quest-completions\/[^/]+\/(approve|reject))$/.test(c.path));

export async function becomeAnn() {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ann'));
}

export async function becomeBen() {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ben'));
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = '1234';
  await submit($pin.parentNode.parentNode);
}

export async function backToAdmin() {
  await click(buttons(env.$app, 'Administrator view')[0]);
}

export async function unlockAdmin(pin) {
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = pin;
  await submit($pin.parentNode.parentNode);
}

// noInlineStyle asserts the strict-CSP rule on what was rendered: no element
// carries a style attribute or an inline style property.
export const noInlineStyle = () => {
  for (const n of walk(env.$app)) {
    assert.equal(n.getAttribute('style'), null, `${n.tagName} has a style attribute`);
    assert.deepEqual(Object.keys(n.style), [], `${n.tagName} has an inline style`);
  }
};

export const openTab = async (label) => {
  await click(walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === label));
  await settle();
};

export const alerts = () => walk(env.$app).filter((n) => n.getAttribute('role') === 'alert');

export const field = (root, name) => walk(root).find((n) => (n.tagName === 'INPUT' || n.tagName === 'TEXTAREA') && n.name === name);

export function ageHousehold() {
  srv.members = MEMBERS.map((m) => ({ ...m }));
  for (const m of MEMBERS) srv.extra[`POST ${HP}/family-members/${m.id}/verify`] = () => [204, null];
}

export async function become(name) {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, name));
}
