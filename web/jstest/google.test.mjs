// Unit tests for the auth view's Google Identity Services button (CLM-1, 3, 5).
// Run: node --test web/jstest/google.test.mjs
//
// app.js and api.js run as shipped against a minimal DOM. Only the server
// (fetch) and Google's own library (window.google.accounts.id, loaded from
// accounts.google.com in the browser) are stand-ins.

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
