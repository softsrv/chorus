// Unit tests for the Rewards tab (rewards CLM-1..7).
// Run: node --test web/jstest/rewards.test.mjs
//
// app.js and api.js run as shipped against a minimal DOM; only the server
// (fetch) is a stub, answering with the JSON shapes internal/http/handlers
// emits (rewardJSON, libraryRewardJSON, the error envelope).

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  installBrowser, installFetch, loadApp, walk, buttons, buttonStarting, click, submit, sectionNode,
} from './harness.mjs';

const H = 'h1';
const HP = `/households/${H}`;
const REFRESH_KEY = 'chorus.refresh_token';

let env;
let srv;
let calls;
let signedOut;

const reward = (over) => ({
  id: 'r1', household_id: H, library_id: null, description: '$10', cost: 1000,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z', ...over,
});

const LIBRARY = [
  { id: 'l1', description: 'Pick the movie', cost: 50 },
  { id: 'l2', description: 'Stay up 30 minutes late', cost: 100 },
  { id: 'l3', description: 'Choose dinner', cost: 150 },
  { id: 'l4', description: 'Extra screen time', cost: 200 },
  { id: 'l5', description: 'Trip for ice cream', cost: 300 },
  { id: 'l6', description: '$5 allowance', cost: 500 },
];

const err = (status, code, message = code, fields) => [status, { error: { code, message, ...(fields ? { fields } : {}) } }];

function routes() {
  const r = {
    'POST /auth/refresh': () => [200, {
      access_token: 'at', refresh_token: 'rt',
      user: { id: 'u1', email: 'p@example.com', display_name: 'Parent', email_verified: true },
    }],
    'GET /households': () => [200, { items: [{ id: H, name: 'Home', timezone: 'UTC', role: 'admin' }] }],
    [`GET ${HP}/admin-pin`]: () => [200, { set: srv.adminPinSet }],
    [`POST ${HP}/admin-pin/verify`]: (r) => (r.body.pin === srv.adminPin ? [204, null] : err(403, 'wrong_pin', 'Wrong PIN.')),
    [`GET ${HP}/quests`]: () => [200, { items: [], next_offset: null }],
    'GET /quest-library': () => [200, { items: [] }],
    [`GET ${HP}/family-members`]: () => [200, { items: srv.members }],
    [`POST ${HP}/family-members/m-a/verify`]: () => [204, null],
    [`POST ${HP}/family-members/m-sam/verify`]: (r) => (r.body.pin === '1234' ? [204, null] : err(403, 'wrong_pin', 'Wrong PIN.')),
    [`GET ${HP}/quests/pending`]: () => [200, { items: [] }],
    [`GET ${HP}/reward-redemptions/pending`]: (r) => (srv.pendingRewards ? srv.pendingRewards(r) : [200, { items: [] }]),
    [`GET ${HP}/rewards`]: () => [200, { items: srv.rewards }],
    'GET /reward-library': () => [200, { items: LIBRARY }],
    [`POST ${HP}/rewards`]: (r) => {
      const created = reward({ id: `r${srv.rewards.length + 1}`, ...r.body });
      srv.rewards = [...srv.rewards, created];
      return [201, created];
    },
    [`PUT ${HP}/rewards/r1`]: (r) => {
      srv.rewards = srv.rewards.map((x) => (x.id === 'r1' ? { ...x, ...r.body } : x));
      return [200, srv.rewards[0]];
    },
    [`DELETE ${HP}/rewards/r1`]: () => { srv.rewards = srv.rewards.filter((x) => x.id !== 'r1'); return [204, null]; },
    [`DELETE ${HP}/rewards/r2`]: () => { srv.rewards = srv.rewards.filter((x) => x.id !== 'r2'); return [204, null]; },
    [`POST ${HP}/rewards/from-library`]: (r) => {
      const lib = LIBRARY.find((l) => l.id === r.body.library_id);
      const copy = reward({ id: 'r9', library_id: lib.id, description: lib.description, cost: lib.cost });
      srv.rewards = [...srv.rewards, copy];
      return [201, copy];
    },
    ...srv.extra,
  };
  return new Proxy(r, {
    get(target, prop) {
      if (prop in target) return target[prop];
      if (typeof prop === 'string') {
        if (prop.startsWith(`GET ${HP}/family-members/`) && prop.endsWith('/reward-redemptions/pending')) {
          return (req) => (srv.memberPendingRewards ? srv.memberPendingRewards(req) : [200, { items: srv.pendingRedemptions || [] }]);
        }
        if (prop.startsWith(`POST ${HP}/rewards/`) && prop.endsWith('/redeem')) {
          return (req) => (srv.redeem ? srv.redeem(req) : [201, { id: 'red-1', reward_id: prop.split('/')[3], status: 'pending' }]);
        }
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
    rewards: [reward()],
    members: [{ id: 'm-a', household_id: H, name: 'Ann', has_pin: false, points: 0 }],
    pendingRedemptions: [],
    extra: {},
  };
  calls = installFetch(new Proxy({}, { get: (_, k) => routes()[k] }));
});

const text = () => env.$app.textContent;
const tabLink = (label) => walk(env.$app).find((n) => n.tagName === 'A' && n.textContent === label);
const openRewards = () => click(tabLink('Rewards'));
const rewardWrites = () => calls.filter((c) => c.method !== 'GET' && /\/rewards/.test(c.path));
const formWithSubmit = (label) => buttons(env.$app, label)[0].parentNode;
const inputIn = (root, name) => walk(root).find((n) => (n.tagName === 'INPUT' || n.tagName === 'TEXTAREA') && n.name === name);
const hintOf = ($input) => $input.parentNode.childNodes.at(-1);
const rowOf = (desc) => walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith(desc));
const ROW_CONTROLS = ['Edit', 'Delete', 'Add', 'Add reward'];

async function fill(formNode, values) {
  for (const [k, v] of Object.entries(values)) inputIn(formNode, k).value = v;
  await submit(formNode);
}

async function unlockAdminInRewards(pin) {
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  $pin.value = pin;
  await submit($pin.parentNode.parentNode);
}

async function becomeSam(pin = '1234') {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Sam'));
  const $pin = walk(env.$app).find((n) => n.tagName === 'INPUT' && n.name === 'pin');
  if ($pin) {
    $pin.value = pin;
    await submit($pin.parentNode.parentNode);
  }
}

async function becomeAnn() {
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ann'));
}

test('CLM-1: rewards list description and cost; add POSTs {description, cost}; Edit PUTs; Delete on the second click', async () => {
  await loadApp();
  await openRewards();
  assert.match(rowOf('$10').textContent, /1000 points/);

  await fill(formWithSubmit('Add reward'), { description: 'Pizza night', cost: '250' });
  const post = rewardWrites().at(-1);
  assert.equal(post.method, 'POST');
  assert.equal(post.path, `${HP}/rewards`);
  assert.deepEqual(post.body, { description: 'Pizza night', cost: 250 });
  assert.match(rowOf('Pizza night').textContent, /250 points/);

  await click(buttons(rowOf('$10'), 'Edit')[0]);
  const $edit = formWithSubmit('Save');
  assert.equal(inputIn($edit, 'description').value, '$10');
  assert.equal(String(inputIn($edit, 'cost').value), '1000');
  await fill($edit, { description: '$20', cost: '2000' });
  const put = rewardWrites().at(-1);
  assert.equal(put.method, 'PUT');
  assert.equal(put.path, `${HP}/rewards/r1`);
  assert.deepEqual(put.body, { description: '$20', cost: 2000 });
  assert.match(rowOf('$20').textContent, /2000 points/);

  const del = buttons(rowOf('$20'), 'Delete')[0];
  await click(del);
  assert.equal(rewardWrites().filter((c) => c.method === 'DELETE').length, 0, 'first click only arms');
  await click(del);
  const last = rewardWrites().at(-1);
  assert.equal(last.method, 'DELETE');
  assert.equal(last.path, `${HP}/rewards/r1`);
  assert.equal(rowOf('$20'), undefined);
});

test('CLM-2: the library lists description and cost with only Add; Add posts from-library and refreshes', async () => {
  srv.rewards = [];
  await loadApp();
  await openRewards();
  const lib = sectionNode(env.$app, 'Reward Library');
  for (const item of LIBRARY) {
    const $li = walk(lib).find((n) => n.tagName === 'LI' && n.textContent.startsWith(item.description));
    assert.match($li.textContent, new RegExp(`${item.cost} points`));
    const labels = walk($li).filter((n) => n.tagName === 'BUTTON').map((b) => b.textContent);
    assert.deepEqual(labels, ['Add'], item.description);
  }

  const $first = walk(lib).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Pick the movie'));
  await click(buttons($first, 'Add')[0]);
  const post = rewardWrites().at(-1);
  assert.equal(post.method, 'POST');
  assert.equal(post.path, `${HP}/rewards/from-library`);
  assert.deepEqual(post.body, { library_id: 'l1' });
  assert.ok(calls.filter((c) => c.path === '/reward-library').every((c) => c.method === 'GET'));
  // The household list now shows the adopted copy, editable like any reward.
  const $copy = walk(env.$app).find((n) => n.tagName === 'LI' && n.textContent.startsWith('Pick the movie') && buttons(n, 'Edit').length);
  assert.ok($copy, 'adopted reward is in the household list');
  await click(buttons($copy, 'Edit')[0]);
  await fill(formWithSubmit('Save'), { description: 'Pick two movies', cost: '80' });
  assert.equal(rewardWrites().at(-1).path, `${HP}/rewards/r9`);
});

test('CLM-3: a 422 puts each field message under its input, keeps the values and the list, and sends cost as typed', async () => {
  srv.extra[`POST ${HP}/rewards`] = () => err(422, 'validation_failed', 'Check the highlighted fields.',
    { description: 'required', cost: 'must be a non-negative number' });
  await loadApp();
  await openRewards();
  const $form = formWithSubmit('Add reward');
  await fill($form, { description: ' ', cost: 'abc' });

  assert.deepEqual(rewardWrites().at(-1).body, { description: ' ', cost: 'abc' });
  assert.equal(hintOf(inputIn($form, 'description')).textContent, 'required');
  assert.equal(hintOf(inputIn($form, 'cost')).textContent, 'must be a non-negative number');
  assert.equal(inputIn($form, 'description').value, ' ');
  assert.equal(inputIn($form, 'cost').value, 'abc');
  assert.equal(walk(env.$app).filter((n) => n.tagName === 'LI' && buttons(n, 'Edit').length).length, 1, 'no new row');

  await fill($form, { description: 'x', cost: '-5' });
  assert.deepEqual(rewardWrites().at(-1).body, { description: 'x', cost: -5 }, 'a negative cost is the server\'s to reject');
});

test('CLM-4: an empty list shows the empty state with the add form and library; a list with rewards does not', async () => {
  const EMPTY = 'No rewards yet. Add one below or browse the library for ideas.';
  srv.rewards = [];
  await loadApp();
  await openRewards();
  assert.ok(walk(env.$app).some((n) => n.textContent === EMPTY && n.tagName === 'P'));
  assert.equal(buttons(env.$app, 'Add reward').length, 1);
  assert.ok(sectionNode(env.$app, 'Reward Library'));

  srv.rewards = [reward()];
  await openRewards();
  assert.ok(!text().includes(EMPTY));
});

test('CLM-6: member view is read-only; the admin view unlocks with the PIN and every write sends X-Admin-PIN only in the header', async () => {
  srv.adminPinSet = true;
  await loadApp();
  await openRewards();
  // Administrator's view, PIN not entered: read-only with an unlock form.
  for (const label of ROW_CONTROLS) assert.equal(buttons(env.$app, label).length, 0, `locked: ${label}`);
  assert.match(text(), /Enter the admin PIN to manage rewards\./);

  // A member's view: read-only list, no unlock and no library.
  await click(buttons(env.$app, 'Switch member')[0]);
  await click(buttonStarting(env.$app, 'Ann'));
  await openRewards();
  assert.match(rowOf('$10').textContent, /1000 points/);
  for (const label of [...ROW_CONTROLS, 'Unlock']) assert.equal(buttons(env.$app, label).length, 0, `member: ${label}`);
  assert.equal(sectionNode(env.$app, 'Reward Library'), null);

  // Back to the administrator's view with the admin PIN.
  await click(buttons(env.$app, 'Administrator view')[0]);
  await unlockAdminInRewards('9999');
  await openRewards();
  await fill(formWithSubmit('Add reward'), { description: 'Pizza night', cost: '250' });
  await click(buttons(rowOf('$10'), 'Edit')[0]);
  await fill(formWithSubmit('Save'), { description: '$10', cost: '900' });
  const lib = sectionNode(env.$app, 'Reward Library');
  await click(buttons(lib, 'Add')[0]);
  const del = buttons(rowOf('Pizza night'), 'Delete')[0];
  await click(del);
  await click(del);

  const writes = rewardWrites();
  assert.deepEqual(writes.map((c) => c.method).sort(), ['DELETE', 'POST', 'POST', 'PUT']);
  for (const w of writes) {
    assert.equal(w.headers['X-Admin-PIN'], '9999', `${w.method} ${w.path}`);
    assert.ok(!w.url.includes('9999'), 'PIN in the query');
    assert.ok(!JSON.stringify(w.body ?? {}).includes('9999'), 'PIN in the body');
  }
  for (const s of [localStorage, sessionStorage]) {
    for (const v of s.writes.values()) assert.ok(!String(v).includes('9999'), 'PIN in storage');
  }

  // A 403 wrong_pin shows its error and keeps the session.
  srv.extra[`PUT ${HP}/rewards/r1`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await click(buttons(rowOf('$10'), 'Edit')[0]);
  const $edit = formWithSubmit('Save');
  await fill($edit, { description: '$10', cost: '1' });
  assert.match($edit.textContent, /Wrong PIN\./);
  assert.equal(signedOut, 0);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'rt');
});

test('CLM-13: in member view, Redeem button shown per reward row, enabled iff points >= cost, else disabled with Need N more points', async () => {
  const fixture = JSON.parse(readFileSync(new URL('./fixtures/rewards-member-enough.json', import.meta.url), 'utf8'));
  srv.members = fixture.members;
  srv.rewards = fixture.rewards;
  srv.pendingRedemptions = fixture.pendingRedemptions;

  await loadApp();
  await becomeSam();
  await openRewards();

  const redeemButtons = buttons(env.$app, 'Redeem');
  assert.equal(redeemButtons.length, 3, 'exactly three Redeem buttons');

  const movieRow = rowOf('Movie night');
  const movieBtn = buttons(movieRow, 'Redeem')[0];
  assert.equal(movieBtn.disabled, false);

  const screenRow = rowOf('Extra screen time');
  const screenBtn = buttons(screenRow, 'Redeem')[0];
  assert.equal(screenBtn.disabled, false);

  const pizzaRow = rowOf('Pizza night');
  const pizzaBtn = buttons(pizzaRow, 'Redeem')[0];
  assert.equal(pizzaBtn.disabled, true);
  assert.match(pizzaRow.textContent, /Need 30 more points/);
  assert.match(pizzaRow.textContent, /150 points/);
});

test('CLM-14: admin view renders no Redeem button and makes no redeem or member pending requests', async () => {
  const fixture = JSON.parse(readFileSync(new URL('./fixtures/rewards-member-enough.json', import.meta.url), 'utf8'));
  srv.rewards = fixture.rewards;

  await loadApp();
  const before = calls.length;
  await openRewards();

  assert.equal(buttons(env.$app, 'Redeem').length, 0);
  assert.equal(calls.slice(before).filter((c) => c.path.includes('/redeem') || c.path.includes('/reward-redemptions')).length, 0);
});

test('CLM-15: clicking Redeem POSTs {member_id, pin}, renders Pending approval badge, notice above list, points unchanged; pre-existing pending renders badge', async () => {
  const fixture = JSON.parse(readFileSync(new URL('./fixtures/rewards-member-enough.json', import.meta.url), 'utf8'));
  srv.members = fixture.members;
  srv.rewards = fixture.rewards;
  srv.pendingRedemptions = fixture.pendingRedemptions;

  await loadApp();
  await becomeSam();
  await openRewards();

  const movieRow = rowOf('Movie night');
  const movieBtn = buttons(movieRow, 'Redeem')[0];
  await click(movieBtn);

  const post = calls.find((c) => c.method === 'POST' && c.path === `${HP}/rewards/r-movie/redeem`);
  assert.ok(post, 'redeem POST made');
  assert.deepEqual(post.body, { member_id: 'm-sam', pin: '1234' });

  assert.equal(buttons(rowOf('Movie night'), 'Redeem').length, 0, 'Redeem button replaced');
  assert.match(rowOf('Movie night').textContent, /Pending approval/);
  assert.match(text(), /Movie night requested — waiting for approval\. Your points stay at 120 until then\./);
  assert.match(text(), /120 points/);

  // (b) Loads with pending-route stub returning that reward renders badge
  const pendingFixture = JSON.parse(readFileSync(new URL('./fixtures/rewards-member-pending.json', import.meta.url), 'utf8'));
  srv.pendingRedemptions = pendingFixture.pendingRedemptions;
  await loadApp();
  await becomeSam();
  await openRewards();

  assert.equal(buttons(rowOf('Movie night'), 'Redeem').length, 0);
  assert.match(rowOf('Movie night').textContent, /Pending approval/);
});

test('CLM-16: a 422 answer shows inline message and keeps Redeem button; wrong_pin invokes memberActionFailed', async () => {
  const fixture = JSON.parse(readFileSync(new URL('./fixtures/rewards-member-enough.json', import.meta.url), 'utf8'));
  srv.members = fixture.members;
  srv.rewards = fixture.rewards;

  srv.extra[`POST ${HP}/rewards/r-movie/redeem`] = () => [422, { error: { code: 'insufficient_points', message: 'Not enough points' } }];
  await loadApp();
  await becomeSam();
  await openRewards();

  await click(buttons(rowOf('Movie night'), 'Redeem')[0]);
  assert.match(rowOf('Movie night').textContent, /Not enough points/);
  assert.equal(buttons(rowOf('Movie night'), 'Redeem').length, 1);
  assert.doesNotMatch(rowOf('Movie night').textContent, /Pending approval/);

  // wrong_pin answers
  srv.extra[`POST ${HP}/rewards/r-movie/redeem`] = () => err(403, 'wrong_pin', 'Wrong PIN.');
  await click(buttons(rowOf('Movie night'), 'Redeem')[0]);
  assert.equal(signedOut, 0);
  assert.ok(buttonStarting(env.$app, 'Switch member') || buttonStarting(env.$app, 'Administrator view'));
});

test('CLM-21: empty list in member view shows empty state text and zero Redeem buttons', async () => {
  const EMPTY = 'No rewards yet. Add one below or browse the library for ideas.';
  srv.rewards = [];
  await loadApp();
  await becomeAnn();
  await openRewards();
  assert.ok(text().includes(EMPTY));
  assert.equal(buttons(env.$app, 'Redeem').length, 0);
});

// The rewardsTab source, from its definition to the next section divider.
const APP_SRC = readFileSync(new URL('../static/js/app.js', import.meta.url), 'utf8');
function rewardsSource(src) {
  const start = src.indexOf('async function rewardsTab(');
  const end = src.indexOf('// ── Family', start);
  assert.ok(start >= 0 && end > start, 'rewardsTab section not found');
  return src.slice(start, end);
}

// The rewards section's write surface. Redeem is the one member write; every
// other word for spending is still refused, and any request path under a
// single reward other than edit/delete, from-library and redeem is a finding.
const SPEND = /spend|\bbuy\b|claim|purchase/i;
const REWARD_PATHS = /`\$\{base\}\/[^`]*`/g;
const ALLOWED_PATHS = new Set(['`${base}/${r.id}`', '`${base}/from-library`', '`${base}/${r.id}/redeem`']);

function spendFindings(section) {
  const out = [];
  if (SPEND.test(section)) out.push('spend wording');
  for (const m of section.match(REWARD_PATHS) || []) if (!ALLOWED_PATHS.has(m)) out.push(m);
  return out;
}

test('CLM-5: the rewards UI writes only create, edit, delete, from-library and the member-view redeem', () => {
  const section = rewardsSource(APP_SRC);
  assert.deepEqual(spendFindings(section), []);
  // The admin writes are exactly reward management as it was.
  const writes = section.match(/adminRequest\('(POST|PUT|DELETE)', [^,)]+/g) || [];
  assert.deepEqual(writes.sort(), [
    "adminRequest('DELETE', `${base}/${r.id}`",
    "adminRequest('POST', `${base}/from-library`",
    "adminRequest('POST', base",
    "adminRequest('PUT', `${base}/${r.id}`",
  ]);
  // The plain api() writes are the admin-PIN verify and the member's redeem.
  assert.deepEqual((section.match(/\bapi\('(POST|PUT|DELETE)', [^,)]+/g) || []).sort(), [
    "api('POST', `${base}/${r.id}/redeem`",
    "api('POST', `/households/${h.id}/admin-pin/verify`",
  ]);
  // Redeem is posted only from renderActionArea, which runs only in member view.
  const areaStart = section.indexOf('const renderActionArea = ');
  const areaEnd = section.indexOf('\n    };', areaStart);
  assert.ok(areaStart >= 0 && areaEnd > areaStart, 'renderActionArea not found');
  const redeemAt = section.indexOf("api('POST', `${base}/${r.id}/redeem`");
  assert.ok(redeemAt > areaStart && redeemAt < areaEnd, 'redeem POST lives in renderActionArea');
  const outside = section.slice(0, areaStart) + section.slice(areaEnd);
  assert.deepEqual(outside.match(/renderActionArea\(\)/g), ['renderActionArea()'], 'called once outside itself');
  assert.match(outside, /if \(isMember\) \{\s*renderActionArea\(\);/, 'that one call is gated on member view');
  // The check fails on a planted spend word, route or admin write.
  assert.notDeepEqual(spendFindings(section.replace("text: 'Edit'", "text: 'Buy'")), []);
  assert.notDeepEqual(spendFindings(`${section}adminRequest('POST', \`\${base}/\${r.id}/use\`)`), []);
  const planted = `${section}adminRequest('POST', \`\${base}/\${r.id}/redeem\`)`;
  assert.notDeepEqual((planted.match(/adminRequest\('(POST|PUT|DELETE)', [^,)]+/g) || []).length, writes.length);
});

const HTML_SINKS = /innerHTML|insertAdjacentHTML|outerHTML|style:|style=|<script/i;

test('CLM-7: the rewards UI inserts text only — no innerHTML, inline style or script', () => {
  const section = rewardsSource(APP_SRC);
  assert.equal(HTML_SINKS.exec(section), null);
  assert.ok(HTML_SINKS.test(section.replace("text: r.description", 'innerHTML: r.description')), 'check fails on a planted sink');
  assert.ok(HTML_SINKS.test(`${section}el('div', { style: 'color:red' })`), 'check fails on a planted style');
});

function rewardHistorySource(src) {
  const fnStart = src.indexOf('function rewardHistoryTable(');
  assert.ok(fnStart >= 0, 'rewardHistoryTable not found in app.js');
  // Read until next function or section divider
  const fnEnd = src.indexOf('\nfunction ', fnStart + 1);
  const end = fnEnd >= 0 ? fnEnd : src.length;
  return src.slice(fnStart, end);
}

test('CLM-11: reward history inserts text only — no innerHTML, inline style or script', () => {
  const section = rewardHistorySource(APP_SRC);
  assert.match(section, /No rewards redeemed yet/, 'read the real rewardHistoryTable body');
  assert.throws(() => rewardHistorySource('function other() {}'), /rewardHistoryTable not found/, 'a missing function fails the check');
  assert.equal(HTML_SINKS.exec(section), null);
  assert.ok(HTML_SINKS.test(`${section}el('div', { innerHTML: 'danger' })`), 'check fails on a planted sink');
});

