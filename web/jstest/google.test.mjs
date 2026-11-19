// Unit tests for the Google Identity Services wiring: the auth view button
// (CLM-1, 2, 3, 5) and the Account page calendar connect popup (CLM-4, 6).
// Run: node --test web/jstest/google.test.mjs
//
// app.js and api.js run as shipped against a minimal DOM. Only the server
// (fetch) and Google's own library (window.google.accounts.id and .oauth2,
// loaded from accounts.google.com in the browser) are stand-ins.

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { installBrowser, installFetch, loadApp, settle, walk, click } from './harness.mjs';

const REFRESH_KEY = 'chorus.refresh_token';
const CLIENT_ID = 'fake-web-client.apps.googleusercontent.example';
const SENTINEL = 'SENTINEL-server-said-token-audience-mismatch';

let env;
let calls;
let gis; // recorded calls into the stubbed GIS library
let srv;

function installGIS() {
  gis = { initialize: [], renderButton: [], prompt: [], callback: null };
  window.google = {
    accounts: {
      id: {
        initialize: (opts) => { gis.initialize.push(opts); gis.callback = opts.callback; },
        renderButton: (node, opts) => { gis.renderButton.push({ node, opts }); },
        prompt: (listener) => { gis.prompt.push(listener); },
      },
    },
  };
}

beforeEach(() => {
  env = installBrowser();
  installGIS();
  srv = { config: { google_sign_in: true, google_calendar: false, google_client_id: CLIENT_ID } };
  calls = installFetch({
    'GET /auth/config': () => [200, srv.config],
    'POST /auth/google': () => [401, { error: { code: 'invalid_google_token', message: SENTINEL } }],
  });
});

const text = () => env.$app.textContent;
const buttonSlots = () => walk(env.$app).filter((n) => n.getAttribute('data-google-button') !== null);
const tab = (label) => walk(env.$app).find((n) => n.tagName === 'A' && n.getAttribute('role') === 'tab' && n.textContent === label);
const serverCallsBesidesConfig = () => calls.filter((c) => !(c.method === 'GET' && c.path === '/auth/config'));

test('CLM-1: with google_sign_in on, login and register each render the GIS button with the configured client id', async () => {
  await loadApp();
  assert.equal(buttonSlots().length, 1, 'login tab has a button container');
  assert.equal(gis.initialize.length, 1);
  assert.equal(gis.initialize[0].client_id, CLIENT_ID);
  assert.equal(typeof gis.initialize[0].callback, 'function');
  assert.equal(gis.renderButton.length, 1);
  assert.equal(gis.renderButton[0].node, buttonSlots()[0], 'button rendered into the login container');

  await click(tab('Register'));
  assert.equal(buttonSlots().length, 1, 'register tab has a button container');
  assert.equal(gis.renderButton.length, 2, 'renderButton ran once per tab');
  assert.equal(gis.renderButton[1].node, buttonSlots()[0], 'button rendered into the register container');
  assert.ok(gis.initialize.every((o) => o.client_id === CLIENT_ID));
});

test('CLM-1: with google_sign_in off, neither tab has a button container and GIS is never initialised', async () => {
  srv.config = { google_sign_in: false, google_calendar: false, google_client_id: '' };
  await loadApp();
  assert.equal(buttonSlots().length, 0, 'login tab');
  await click(tab('Register'));
  assert.equal(buttonSlots().length, 0, 'register tab');
  assert.equal(gis.initialize.length, 0);
  assert.equal(gis.renderButton.length, 0);
  assert.equal(gis.prompt.length, 0);
});

test('CLM-3: dismissing Google\'s prompt shows the cancelled message and sends nothing', async () => {
  await loadApp();
  assert.equal(gis.prompt.length, 1);
  gis.prompt[0]({ isSkippedMoment: () => true, getSkippedReason: () => 'user_cancel', isDismissedMoment: () => false });
  await settle();
  assert.match(text(), /Google sign-in was cancelled/);
  assert.deepEqual(serverCallsBesidesConfig(), []);
});

// The rendered button opens Google's own window, which reports nothing when
// it is closed. The fake GIS exposes the button's click_listener, and the
// person returning to the page (window focus) with no credential is a cancel.
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const clickGoogleButton = () => gis.renderButton[gis.renderButton.length - 1].opts.click_listener();
const returnToPage = () => window.dispatchEvent(new Event('focus'));

test('CLM-3: closing the chooser opened by the rendered button shows the cancelled message and sends nothing', async () => {
  await loadApp();
  assert.equal(typeof gis.renderButton[0].opts.click_listener, 'function', 'the button reports its click');
  clickGoogleButton();
  returnToPage();
  await wait(450);
  await settle();
  assert.match(text(), /Google sign-in was cancelled/);
  assert.deepEqual(serverCallsBesidesConfig(), []);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), null);
});

test('CLM-3: returning to the page without having used the button is not a cancel', async () => {
  await loadApp();
  returnToPage();
  await wait(450);
  assert.doesNotMatch(text(), /cancelled/);
});

test('CLM-3: a credential that follows the button click is not reported as a cancel', async () => {
  await loadApp();
  clickGoogleButton();
  returnToPage();
  await gis.callback({ credential: 'tok' });
  await wait(450);
  await settle();
  assert.doesNotMatch(env.document.body.textContent, /cancelled/);
  assert.match(text(), /Google sign-in failed/);
  assert.equal(calls.filter((c) => c.method === 'POST' && c.path === '/auth/google').length, 1);
});

// A credential slower than the grace period: the focus timer has already
// guessed "cancelled". The sign-in that does complete must withdraw that
// message (view and toast), so a completed sign-in is never left reported
// as cancelled.
test('CLM-3: a credential arriving after the cancel timer fired withdraws the cancelled message', async () => {
  await loadApp();
  clickGoogleButton();
  returnToPage();
  await wait(450);
  await settle();
  assert.match(env.document.body.textContent, /Google sign-in was cancelled/, 'the timer guessed a cancel');
  await gis.callback({ credential: 'late-tok' });
  await settle();
  assert.doesNotMatch(env.document.body.textContent, /cancelled/, 'cancel withdrawn in view and toast');
  assert.match(text(), /Google sign-in failed/, 'the late credential was sent and its outcome shown');
  const posted = calls.filter((c) => c.method === 'POST' && c.path === '/auth/google');
  assert.equal(posted.length, 1);
  assert.equal(posted[0].body.id_token, 'late-tok');
});

test('CLM-3: a One Tap cancel notification followed by a credential withdraws the cancelled message', async () => {
  await loadApp();
  gis.prompt[0]({ isSkippedMoment: () => true, getSkippedReason: () => 'user_cancel', isDismissedMoment: () => false });
  await settle();
  assert.match(env.document.body.textContent, /Google sign-in was cancelled/);
  await gis.callback({ credential: 'tok-after-onetap' });
  await settle();
  assert.doesNotMatch(env.document.body.textContent, /cancelled/);
  assert.equal(calls.filter((c) => c.method === 'POST' && c.path === '/auth/google').length, 1);
});

test('CLM-3: an empty credential shows the cancelled message and sends nothing', async () => {
  await loadApp();
  await gis.callback({ credential: '' });
  await settle();
  assert.match(text(), /Google sign-in was cancelled/);
  await gis.callback({});
  await settle();
  assert.deepEqual(serverCallsBesidesConfig(), []);
  assert.equal(sessionStorage.getItem(REFRESH_KEY), null);
});

test('CLM-3: a prompt Google could not show (no Google session) is not reported as a cancel', async () => {
  await loadApp();
  gis.prompt[0]({ isSkippedMoment: () => false, isNotDisplayed: () => true, isDismissedMoment: () => false });
  await settle();
  assert.doesNotMatch(text(), /cancelled/);
});

test('CLM-5: a refused credential shows the generic failure, never the server text, and stores no tokens', async () => {
  await loadApp();
  await gis.callback({ credential: 'fake.google.id-token' });
  await settle();
  const posted = calls.filter((c) => c.method === 'POST' && c.path === '/auth/google');
  assert.equal(posted.length, 1);
  assert.equal(posted[0].body.id_token, 'fake.google.id-token');
  assert.match(text(), /Google sign-in failed/);
  assert.ok(!text().includes(SENTINEL), 'server error text is not shown in the view');
  assert.ok(!env.document.body.textContent.includes(SENTINEL), 'server error text is not toasted');
  assert.equal(sessionStorage.getItem(REFRESH_KEY), null, 'no refresh token stored');
  assert.equal(buttonSlots().length, 1, 'still on the sign-in view');
});

// ── CLM-2: credential → session, in page ─────────────────────────────────────

const USER = { id: 'u-1', email: 'person@example.test', display_name: 'Pat', email_verified: true, has_password: false };
const TOKENS = { access_token: 'fake-access', refresh_token: 'fake-refresh', user: USER };

test('CLM-2: a GIS credential is exchanged at POST /auth/google, tokens are stored and goHome renders the app without a page navigation', async () => {
  const replaced = [];
  history.replaceState = (_s, _t, url) => replaced.push(url);
  calls = installFetch({
    'GET /auth/config': () => [200, srv.config],
    'POST /auth/google': () => [200, TOKENS],
    'GET /households': () => [200, { items: [] }],
  });
  await loadApp();
  await gis.callback({ credential: 'tok' });
  await settle();

  const posted = calls.filter((c) => c.method === 'POST' && c.path === '/auth/google');
  assert.equal(posted.length, 1, 'one sign-in request');
  assert.equal(posted[0].body.id_token, 'tok');
  assert.equal(posted[0].body.device_name, 'Web test client');
  assert.equal(sessionStorage.getItem(REFRESH_KEY), 'fake-refresh', 'setTokens stored the pair');
  assert.deepEqual(replaced, ['/'], 'goHome ran (in-page route change to /)');
  assert.ok(calls.some((c) => c.method === 'GET' && c.path === '/households'), 'boot rendered the signed-in home');
  assert.equal(buttonSlots().length, 0, 'left the sign-in view');
  assert.equal(window.location, undefined, 'window.location never assigned');
  assert.equal(location.pathname, '/', 'no full-page navigation');
});

// ── CLM-6 / CLM-4: calendar connect through Google's code-client popup ──────

const SCOPES = ['https://www.googleapis.com/auth/calendar.calendarlist.readonly', 'https://www.googleapis.com/auth/calendar.events'];
const CAL_OK = 'Google Calendar connected.';
const CAL_FAIL = 'Could not connect Google Calendar. Please try again.';
const CAL_SENTINEL = 'SENTINEL-google-said-invalid_grant';

let codeClient; // recorded calls into the stubbed GIS oauth2 library

async function openAccount(connectHandler) {
  codeClient = { configs: [], requested: 0 };
  window.google.accounts.oauth2 = {
    initCodeClient: (cfg) => {
      codeClient.configs.push(cfg);
      return { requestCode: () => { codeClient.requested += 1; } };
    },
  };
  srv.config = { google_sign_in: true, google_calendar: true, google_client_id: CLIENT_ID };
  sessionStorage.setItem(REFRESH_KEY, 'fake-refresh');
  location.pathname = '/account';
  calls = installFetch({
    'GET /auth/config': () => [200, srv.config],
    'POST /auth/refresh': () => [200, TOKENS],
    'GET /me/sessions': () => [200, { items: [] }],
    'GET /me/google-calendar': () => [200, { status: 'not_connected' }],
    'GET /me/google-calendar/calendars': () => [200, { items: [] }],
    'GET /households': () => [200, { items: [] }],
    'POST /me/google-calendar/connect': connectHandler,
  });
  await loadApp();
}

const calCard = () => walk(env.$app).find((n) => n.tagName === 'H2' && n.textContent === 'Google Calendar')?.parentNode;
const connectButton = () => walk(env.$app).find((n) => n.tagName === 'BUTTON' && n.textContent === 'Connect Google Calendar');
const connectPosts = () => calls.filter((c) => c.method === 'POST' && c.path === '/me/google-calendar/connect');

test('CLM-6: Connect opens the popup code client with exactly the two calendar scopes and posts the code; the card shows the returned status', async () => {
  await openAccount(() => [200, { status: 'connected', access: 'read_write' }]);
  assert.ok(connectButton(), 'not-connected card offers Connect');
  assert.equal(codeClient.configs.length, 0, 'nothing requested before the click');

  await click(connectButton());
  assert.equal(codeClient.configs.length, 1);
  const cfg = codeClient.configs[0];
  assert.equal(cfg.ux_mode, 'popup');
  assert.equal(cfg.client_id, CLIENT_ID);
  assert.deepEqual(cfg.scope.split(' ').sort(), [...SCOPES].sort());
  assert.equal(codeClient.requested, 1, 'requestCode called on click');
  assert.equal(connectPosts().length, 0, 'nothing sent before Google returns a code');

  await cfg.callback({ code: 'c1' });
  await settle();
  const posted = connectPosts();
  assert.equal(posted.length, 1);
  assert.deepEqual(posted[0].body, { code: 'c1', platform: 'web' });
  assert.equal(posted[0].headers.Authorization, 'Bearer fake-access', 'bearer auth');
  const card = calCard().textContent;
  assert.match(card, /Connected/);
  assert.match(card, /Read and write access\./);
  assert.match(card, new RegExp(CAL_OK.replace('.', '\\.')));
  assert.equal(connectButton(), undefined, 'Connect replaced by the connected card');
  assert.equal(window.location, undefined, 'window.location never assigned');
  assert.equal(location.pathname, '/account', 'stayed on the Account page');
});

test('CLM-6: a read-only grant renders the read-only access line', async () => {
  await openAccount(() => [200, { status: 'connected', access: 'read_only' }]);
  await click(connectButton());
  await codeClient.configs[0].callback({ code: 'c2' });
  await settle();
  assert.match(calCard().textContent, /Read-only access/);
});

test('CLM-4: the server refusing the code shows the fixed message, no server text, and the card stays not-connected', async () => {
  await openAccount(() => [400, { error: { code: 'google_calendar_failed', message: CAL_SENTINEL } }]);
  await click(connectButton());
  await codeClient.configs[0].callback({ code: 'used-code' });
  await settle();
  assert.equal(connectPosts().length, 1);
  assert.match(calCard().textContent, new RegExp(CAL_FAIL.replace(/\./g, '\\.')));
  assert.ok(connectButton(), 'card unchanged: Connect still offered');
  assert.doesNotMatch(calCard().textContent, /Connected/);
  assert.ok(!env.$app.textContent.includes(CAL_SENTINEL), 'server text not in the view');
  assert.ok(!env.document.body.textContent.includes(CAL_SENTINEL), 'server text not toasted');
  assert.equal(window.location, undefined);
});

test('CLM-4: a popup error or a response without a code shows the fixed message and sends nothing', async () => {
  await openAccount(() => [200, { status: 'connected', access: 'read_write' }]);
  await click(connectButton());
  codeClient.configs[0].error_callback({ type: 'popup_closed', message: CAL_SENTINEL });
  await settle();
  assert.match(calCard().textContent, new RegExp(CAL_FAIL.replace(/\./g, '\\.')));
  assert.ok(!env.$app.textContent.includes(CAL_SENTINEL));
  assert.ok(!env.document.body.textContent.includes(CAL_SENTINEL));
  assert.ok(connectButton(), 'card unchanged');

  await click(connectButton());
  await codeClient.configs[1].callback({ error: 'access_denied', error_description: CAL_SENTINEL });
  await settle();
  assert.match(calCard().textContent, new RegExp(CAL_FAIL.replace(/\./g, '\\.')));
  assert.ok(!env.$app.textContent.includes(CAL_SENTINEL));
  assert.equal(connectPosts().length, 0, 'no connect request without a code');
  assert.ok(connectButton(), 'card unchanged');
});

test('CLM-4: a lost connect response, after the server stored the connection, still shows the connected card', async () => {
  let statusReads = 0;
  await openAccount(() => [200, { status: 'connected', access: 'read_write' }]);
  // The connect request never returns (network error); the status read does.
  const ok = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    if (init.method === 'POST' && url.endsWith('/me/google-calendar/connect')) throw new Error('network down');
    if (init.method === 'GET' && url.endsWith('/me/google-calendar')) {
      statusReads += 1;
      return { ok: true, status: 200, json: async () => ({ status: 'connected', access: 'read_only' }) };
    }
    return ok(url, init);
  };
  await click(connectButton());
  await codeClient.configs[0].callback({ code: 'c-lost' });
  await settle();
  assert.equal(statusReads, 1, 'the status was asked for after the lost response');
  const card = calCard().textContent;
  assert.match(card, /Connected/);
  assert.match(card, /Read-only access/);
  assert.match(card, new RegExp(CAL_OK.replace('.', '\\.')));
  assert.doesNotMatch(card, new RegExp(CAL_FAIL.replace(/\./g, '\\.')));
});

test('CLM-4: a lost connect response when the server has no connection shows the failure and keeps the card', async () => {
  await openAccount(() => [200, { status: 'connected', access: 'read_write' }]);
  const ok = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    if (init.method === 'POST' && url.endsWith('/me/google-calendar/connect')) throw new Error('network down');
    return ok(url, init); // GET /me/google-calendar answers not_connected
  };
  await click(connectButton());
  await codeClient.configs[0].callback({ code: 'c-lost-2' });
  await settle();
  assert.match(calCard().textContent, new RegExp(CAL_FAIL.replace(/\./g, '\\.')));
  assert.ok(connectButton(), 'card unchanged');
});
