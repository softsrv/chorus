// Chorus web test client. Plain DOM, no framework. All user-supplied text is
// inserted with textContent (never innerHTML), so API data can't inject markup.

import { api, publicApi, refresh, setTokens, clearTokens, setLogger, session, ApiError, DEVICE_NAME, authConfig, googleRedeem } from './api.js';

let authConfigPromise = null;

function getAuthConfig() {
  if (!authConfigPromise) {
    authConfigPromise = authConfig().catch(() => ({ google_sign_in: false, google_calendar: false }));
  }
  return authConfigPromise;
}

const $app = document.getElementById('app');
const $nav = document.getElementById('nav-actions');
const $log = document.getElementById('log');

// ── DOM helpers ───────────────────────────────────────────────────────────────

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (k in node && typeof v !== 'string') node[k] = v;
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function card(title, ...body) {
  return el('div', { class: 'card bg-base-100 shadow' },
    el('div', { class: 'card-body gap-3' }, title && el('h2', { class: 'card-title', text: title }), ...body));
}

function alertBox(kind, message) {
  return el('div', { role: 'alert', class: `alert alert-${kind} text-sm` }, el('span', { text: message }));
}

// form builds a form from field specs and calls onSubmit(values). Errors from
// the API are shown above the form, with per-field messages under each input.
function form({ fields, submit, onSubmit, inline = false }) {
  const $error = el('div');
  const inputs = {};
  const hints = {};
  const rows = fields.map((f) => {
    let input;
    if (f.type === 'select') {
      input = el('select', { name: f.name, class: 'select select-bordered w-full' },
        f.options.map(([value, label]) => el('option', { value, text: label, selected: value === (f.value ?? '') })));
    } else if (f.type === 'textarea') {
      input = el('textarea', { name: f.name, class: 'textarea textarea-bordered w-full', rows: 2, text: f.value ?? '' });
    } else if (f.type === 'checkbox') {
      input = el('input', { type: 'checkbox', name: f.name, class: 'checkbox', checked: !!f.value });
    } else {
      input = el('input', {
        type: f.type || 'text', name: f.name, class: 'input input-bordered w-full',
        required: f.required, value: f.value ?? '', placeholder: f.placeholder, autocomplete: f.autocomplete,
      });
    }
    inputs[f.name] = input;
    hints[f.name] = el('p', { class: 'text-error text-xs mt-1' });
    if (f.type === 'checkbox') {
      return el('label', { class: 'label cursor-pointer justify-start gap-2' }, input, el('span', { text: f.label }));
    }
    return el('label', { class: inline ? 'flex-1 min-w-32' : 'block' },
      el('span', { class: 'text-sm', text: f.label }), input, hints[f.name]);
  });

  const $submit = el('button', { type: 'submit', class: 'btn btn-primary', text: submit });
  const $form = el('form', {
    class: inline ? 'flex flex-wrap items-end gap-2' : 'space-y-3',
    onsubmit: async (e) => {
      e.preventDefault();
      $error.replaceChildren();
      Object.values(hints).forEach((h) => { h.textContent = ''; });
      const values = {};
      for (const [name, input] of Object.entries(inputs)) {
        values[name] = input.type === 'checkbox' ? input.checked : input.value;
      }
      $submit.disabled = true;
      try {
        await onSubmit(values, $form);
      } catch (err) {
        showError($error, err, hints);
      } finally {
        $submit.disabled = false;
      }
    },
  }, $error, rows, $submit);
  return $form;
}

function showError($target, err, hints = {}) {
  if (!(err instanceof ApiError)) {
    console.error(err);
    $target.replaceChildren(alertBox('error', String(err)));
    return;
  }
  for (const [field, msg] of Object.entries(err.fields)) {
    if (hints[field]) hints[field].textContent = msg;
  }
  const unplaced = Object.entries(err.fields).filter(([f]) => !hints[f]).map(([f, m]) => `${f} ${m}`);
  $target.replaceChildren(alertBox('error', [err.message, ...unplaced].join(' ')));
}

// action wraps a click handler so API errors surface instead of vanishing.
function action(fn, $errorTarget) {
  return async (e) => {
    e.preventDefault();
    try {
      await fn(e);
    } catch (err) {
      if ($errorTarget) showError($errorTarget, err);
      else toast(err.message || String(err), 'error');
    }
  };
}

function toast(message, kind = 'info') {
  const t = el('div', { class: 'toast toast-top toast-center z-50' }, alertBox(kind, message));
  document.body.append(t);
  setTimeout(() => t.remove(), 4000);
}

const fmtDate = (iso) => new Date(iso).toLocaleString();

// ── API log panel ─────────────────────────────────────────────────────────────

const SECRET_KEYS = /password|token|^code$/i;
const redact = (v) => JSON.stringify(v, (k, val) => (k && SECRET_KEYS.test(k) && typeof val === 'string' ? '•••' : val), 2);

setLogger(({ method, path, status, ms, request, response }) => {
  const ok = status >= 200 && status < 300;
  const entry = el('li', { class: 'border-b border-base-300 pb-2' },
    el('details', {},
      el('summary', { class: 'cursor-pointer font-mono' },
        el('span', { class: `badge badge-sm mr-1 ${ok ? 'badge-success' : 'badge-error'}`, text: status || 'ERR' }),
        `${method} ${path} `, el('span', { class: 'opacity-50', text: `${ms}ms` })),
      request !== undefined && el('pre', { class: 'bg-base-200 p-2 mt-1 overflow-x-auto', text: '→ ' + redact(request) }),
      response !== null && response !== undefined && el('pre', { class: 'bg-base-200 p-2 mt-1 overflow-x-auto', text: '← ' + redact(response) })));
  $log.prepend(entry);
  while ($log.children.length > 50) $log.lastChild.remove();
});
document.getElementById('log-clear').addEventListener('click', () => $log.replaceChildren());

// ── Routing ───────────────────────────────────────────────────────────────────

async function boot() {
  const params = new URLSearchParams(location.search);
  if (location.pathname === '/verify-email' && params.get('token')) return verifyEmailView(params.get('token'));
  if (location.pathname === '/reset-password' && params.get('token')) return resetPasswordView(params.get('token'));
  if (location.pathname === '/forgot-password') return authView('forgot');

  if (location.pathname === '/google-sign-in') {
    const errorParam = params.get('error');
    const codeParam = params.get('code');
    if (errorParam) {
      history.replaceState(null, '', '/');
      const msg = errorParam === 'cancelled' ? 'Google sign-in was cancelled' : 'Google sign-in failed';
      toast(msg, 'error');
      return authView('login', msg);
    }
    if (codeParam) {
      $app.replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Signing in with Google…')));
      try {
        await googleRedeem(codeParam);
        history.replaceState(null, '', '/');
        return homeView();
      } catch (err) {
        history.replaceState(null, '', '/');
        toast('Google sign-in failed', 'error');
        return authView('login', 'Google sign-in failed');
      }
    }
    history.replaceState(null, '', '/');
    return authView('login');
  }

  if (!session.user) await refresh();
  if (session.user) {
    if (location.pathname === '/account') return accountView();
    return homeView();
  }
  return authView('login');
}

window.addEventListener('chorus:signed-out', () => {
  toast('Your session ended. Please sign in again.', 'warning');
  goHome();
});

function goHome() {
  history.replaceState(null, '', '/');
  boot();
}

function renderNav() {
  $nav.replaceChildren();
  if (!session.user) return;
  $nav.append(
    el('span', { class: 'text-sm hidden sm:inline', text: session.user.display_name }),
    el('button', { class: 'btn btn-ghost btn-sm', text: 'Account', onclick: () => accountView() }),
    el('button', {
      class: 'btn btn-ghost btn-sm', text: 'Sign out',
      onclick: action(async () => {
        try { await api('POST', '/auth/logout'); } finally { clearTokens(); goHome(); }
      }),
    }));
}

// ── Auth views ────────────────────────────────────────────────────────────────

async function authView(tab, alertMsg = null) {
  renderNav();
  const cfg = await getAuthConfig();
  const tabs = [['login', 'Sign in'], ['register', 'Register'], ['forgot', 'Forgot password']];
  const $body = el('div', { class: 'space-y-3' });
  if (alertMsg) {
    $body.append(alertBox('error', alertMsg));
  }
  const tabBar = el('div', { role: 'tablist', class: 'tabs tabs-box' },
    tabs.map(([id, label]) => el('a', {
      role: 'tab', class: `tab ${id === tab ? 'tab-active' : ''}`, text: label, href: '#',
      onclick: (e) => { e.preventDefault(); authView(id); },
    })));

  if (tab === 'login') {
    $body.append(form({
      fields: [
        { name: 'email', label: 'Email', type: 'email', required: true, autocomplete: 'email' },
        { name: 'password', label: 'Password', type: 'password', required: true, autocomplete: 'current-password' },
      ],
      submit: 'Sign in',
      onSubmit: async (v) => {
        setTokens(await publicApi('POST', '/auth/login', { ...v, device_name: DEVICE_NAME }));
        goHome();
      },
    }));
    if (cfg?.google_sign_in === true) {
      $body.append(
        el('div', { class: 'divider', text: 'or' }),
        el('a', { class: 'btn btn-outline w-full', href: '/api/v1/auth/google/start', text: 'Sign in with Google' })
      );
    }
  } else if (tab === 'register') {
    $body.append(form({
      fields: [
        { name: 'display_name', label: 'Display name', required: true },
        { name: 'email', label: 'Email', type: 'email', required: true, autocomplete: 'email' },
        { name: 'password', label: 'Password (8+ characters)', type: 'password', required: true, autocomplete: 'new-password' },
      ],
      submit: 'Create account',
      onSubmit: async (v, $form) => {
        const res = await publicApi('POST', '/auth/register', v);
        $form.replaceWith(alertBox('success', `${res.message} (Local dev: open smtp4dev at http://localhost:5080.)`));
      },
    }));
    if (cfg?.google_sign_in === true) {
      $body.append(
        el('div', { class: 'divider', text: 'or' }),
        el('a', { class: 'btn btn-outline w-full', href: '/api/v1/auth/google/start', text: 'Sign in with Google' })
      );
    }
  } else {
    $body.append(form({
      fields: [{ name: 'email', label: 'Email', type: 'email', required: true, autocomplete: 'email' }],
      submit: 'Send reset link',
      onSubmit: async (v, $form) => {
        const res = await publicApi('POST', '/auth/forgot-password', v);
        $form.replaceWith(alertBox('success', res.message));
      },
    }));
  }
  $app.replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Welcome to Chorus', tabBar, $body)));
}

async function verifyEmailView(token) {
  $app.replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Verifying email…')));
  try {
    setTokens(await publicApi('POST', '/auth/verify-email', { token, device_name: DEVICE_NAME }));
    toast('Email verified!', 'success');
    goHome();
  } catch (err) {
    $app.replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Verification failed',
      alertBox('error', err.message),
      el('a', { class: 'btn', href: '/', text: 'Back to sign in' }))));
  }
}

function resetPasswordView(token) {
  renderNav();
  $app.replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Choose a new password', form({
    fields: [{ name: 'password', label: 'New password', type: 'password', required: true, autocomplete: 'new-password' }],
    submit: 'Reset password',
    onSubmit: async (v) => {
      await publicApi('POST', '/auth/reset-password', { token, password: v.password });
      toast('Password reset. Sign in with your new password.', 'success');
      clearTokens();
      goHome();
    },
  }))));
}

// ── Home: households ──────────────────────────────────────────────────────────

const SELECTED_KEY = 'chorus.household';
let households = [];
let selected = null;
let tab = 'quests';

// Active member and admin unlock. Module memory only: never written to
// localStorage, sessionStorage, cookies or the URL, so a reload starts in the
// administrator's view and asks for any PIN again.
let activeMember = null; // { id, name, pin } after a successful verify
let adminPin = null; // the admin PIN once entered, sent as X-Admin-PIN
let adminPinState = { householdId: null, set: false }; // GET .../admin-pin

// clearMemberState returns to the administrator's view with nothing unlocked.
function clearMemberState() {
  activeMember = null;
  adminPin = null;
  adminPinState = { householdId: null, set: false };
}

// activeMemberAction is the one accessor member actions (accept, abandon, mark
// done) take their body from: { member_id, pin }, or null with no member active.
function activeMemberAction() {
  return activeMember ? { member_id: activeMember.id, pin: activeMember.pin } : null;
}

// adminHeaders is the 4th api() argument for admin-gated requests.
function adminHeaders() {
  return adminPin ? { headers: { 'X-Admin-PIN': adminPin } } : {};
}

// adminRequest sends an admin-gated request. A 403 wrong_pin or
// admin_pin_required forgets the held admin PIN (it was changed or removed
// elsewhere) so management locks again; tokens are untouched.
async function adminRequest(method, path, body) {
  try {
    return await api(method, path, body, adminHeaders());
  } catch (err) {
    if (err instanceof ApiError && (err.code === 'wrong_pin' || err.code === 'admin_pin_required')) {
      adminPin = null;
      if (err.code === 'admin_pin_required') adminPinState.set = true;
    }
    throw err;
  }
}

// memberActionFailed handles an error from a member action. A 403 wrong_pin
// clears the active member back to the administrator's view and says so; the
// session tokens are kept. Returns true when it handled the error.
function memberActionFailed(err) {
  if (!(err instanceof ApiError) || err.code !== 'wrong_pin') return false;
  activeMember = null;
  toast('That PIN is no longer correct. Pick the member again.', 'error');
  renderHome();
  return true;
}

// managementVisible: the Family tab and management controls are shown in the
// administrator's view, or to anyone while no admin PIN is set.
const managementVisible = () => !activeMember || !adminPinState.set;
// managementUnlocked: admin-gated writes can be sent.
const managementUnlocked = () => !adminPinState.set || adminPin !== null;

const PIN_RE = /^\d{4}$/;

// checkPin rejects a PIN that is not exactly 4 digits before anything is sent,
// with the error on the field.
function checkPin(pin, field = 'pin') {
  if (!PIN_RE.test(pin)) {
    throw new ApiError(422, { message: 'Check the highlighted field.', fields: { [field]: 'must be exactly 4 digits' } });
  }
}

// pinFailure rewrites PIN errors into clear messages: wrong PIN and rate limit.
function pinFailure(err) {
  if (err instanceof ApiError && err.code === 'wrong_pin') return new ApiError(403, { code: 'wrong_pin', message: 'Wrong PIN. Try again.' });
  if (err instanceof ApiError && err.status === 429) return new ApiError(429, { message: 'Too many attempts, try again later.' });
  return err;
}

// pinForm is a one-field PIN form: validated client side, errors under it.
function pinForm({ label = 'PIN', submit, onPin }) {
  return form({
    fields: [{ name: 'pin', label, type: 'password', required: true, autocomplete: 'off', placeholder: '4 digits' }],
    submit, inline: true,
    onSubmit: async (v) => {
      checkPin(v.pin);
      try { await onPin(v.pin); } catch (err) { throw pinFailure(err); }
    },
  });
}

async function homeView() {
  renderNav();
  if (!session.user.email_verified) return unverifiedView();

  households = (await api('GET', '/households')).items;
  let remembered = null;
  try { remembered = localStorage.getItem(SELECTED_KEY); } catch { /* storage disabled */ }
  const next = households.find((h) => h.id === remembered) || households[0] || null;
  if (next?.id !== selected?.id) clearMemberState();
  selected = next;
  renderHome();
}

function selectHousehold(h) {
  clearMemberState();
  selected = h;
  try { localStorage.setItem(SELECTED_KEY, h ? h.id : ''); } catch { /* storage disabled */ }
  renderHome();
}

function unverifiedView() {
  const $msg = el('div');
  $app.replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Verify your email',
    el('p', { text: `We sent a verification link to ${session.user.email}. (Local dev: open smtp4dev at http://localhost:5080.)` }),
    $msg,
    el('div', { class: 'flex gap-2' },
      el('button', {
        class: 'btn btn-primary', text: 'Resend email',
        onclick: action(async () => {
          const res = await api('POST', '/me/resend-verification');
          $msg.replaceChildren(alertBox('success', res.message));
        }, $msg),
      }),
      el('button', {
        class: 'btn', text: "I've verified — reload",
        onclick: action(async () => { session.user = await api('GET', '/me'); homeView(); }, $msg),
      })))));
}

function newHouseholdForm() {
  return form({
    fields: [
      { name: 'name', label: 'Name', required: true },
      { name: 'timezone', label: 'Time zone', value: Intl.DateTimeFormat().resolvedOptions().timeZone },
    ],
    submit: 'Create',
    onSubmit: async (v) => {
      const h = await api('POST', '/households', v);
      households.push(h);
      selectHousehold(h);
    },
  });
}

function renderHome() {
  if (households.length === 0 || !selected) {
    // No household (normally one is created at first sign-in): the create form
    // is the main view.
    $app.replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('New household', newHouseholdForm())));
    return;
  }

  const list = el('ul', { class: 'menu bg-base-100 rounded-box w-full' },
    households.map((h) => el('li', {},
      el('a', {
        class: selected?.id === h.id ? 'menu-active' : '', href: '#',
        onclick: (e) => { e.preventDefault(); selectHousehold(h); },
      }, el('span', { class: 'flex-1', text: h.name }), el('span', { class: 'badge badge-sm', text: h.role })))));

  const sidebar = el('div', { class: 'space-y-4' },
    list,
    managementVisible() && card(null, el('h3', { class: 'font-semibold', text: 'New household' }), newHouseholdForm()));

  $app.replaceChildren(el('div', { class: 'grid gap-6 md:grid-cols-[16rem_1fr]' }, sidebar, householdPanel(selected)));
}

function householdPanel(h) {
  const $content = el('div');
  if (adminPinState.householdId !== h.id) {
    api('GET', `/households/${h.id}/admin-pin`)
      .then((r) => {
        if (selected?.id !== h.id) return;
        adminPinState = { householdId: h.id, set: !!r.set };
        renderHome();
      })
      .catch((err) => showError($content, err));
    return el('div', { class: 'space-y-4 min-w-0' },
      el('h2', { class: 'text-2xl font-bold', text: h.name }),
      $content);
  }

  const manage = managementVisible();
  const tabs = [['quests', 'Quests'], manage && ['family', 'Family'], ['events', 'Calendar']].filter(Boolean);
  if (!tabs.some(([id]) => id === tab)) tab = 'quests';

  const header = el('div', { class: 'flex flex-wrap items-center gap-2' },
    el('h2', { class: 'text-2xl font-bold flex-1', text: h.name }),
    el('span', { class: 'text-sm opacity-60', text: h.timezone }),
    manage && h.role === 'admin' && el('button', {
      class: 'btn btn-error btn-outline btn-sm', text: 'Delete household',
      onclick: confirmClick('Delete household?', async () => {
        await api('DELETE', `/households/${h.id}`);
        households = households.filter((x) => x.id !== h.id);
        selectHousehold(households[0] || null);
      }),
    }));
  const tabBar = el('div', { role: 'tablist', class: 'tabs tabs-border' },
    tabs.map(([id, label]) => el('a', {
      role: 'tab', class: `tab ${id === tab ? 'tab-active' : ''}`, text: label, href: '#',
      onclick: (e) => { e.preventDefault(); tab = id; renderHome(); },
    })));

  const loaders = { quests: questsTab, family: familyTab, events: eventsTab };
  (loaders[tab] || questsTab)(h, $content).catch((err) => showError($content, err));
  return el('div', { class: 'space-y-4 min-w-0' }, header, memberBar(h), tabBar, $content);
}

// memberBar shows whose view this is and switches it: pick a member (with
// their PIN when they have one), or go back to the administrator's view (with
// the admin PIN when one is set).
function memberBar(h) {
  const $panel = el('div');
  const $who = el('span', { class: 'text-sm flex-1', text: `Viewing as: ${activeMember ? activeMember.name : 'Administrator'}` });

  const become = (m, pin) => {
    activeMember = { id: m.id, name: m.name, pin };
    adminPin = null; // management locks again while a member is active
    renderHome();
  };

  const openPicker = action(async () => {
    const members = (await api('GET', `/households/${h.id}/family-members`)).items;
    const $pinArea = el('div');
    const pick = (m) => action(async () => {
      if (!m.has_pin) {
        await api('POST', `/households/${h.id}/family-members/${m.id}/verify`, { pin: '' });
        become(m, '');
        return;
      }
      $pinArea.replaceChildren(
        el('p', { class: 'text-sm', text: `PIN for ${m.name}` }),
        pinForm({
          submit: 'Switch',
          onPin: async (pin) => {
            await api('POST', `/households/${h.id}/family-members/${m.id}/verify`, { pin });
            become(m, pin);
          },
        }));
    }, $pinArea);
    $panel.replaceChildren(el('div', { class: 'space-y-2' },
      members.length
        ? el('div', { class: 'flex flex-wrap gap-2' },
          members.map((m) => el('button', { class: 'btn btn-sm', text: m.name, onclick: pick(m) })))
        : el('p', { class: 'text-sm opacity-70', text: 'No family members yet.' }),
      $pinArea));
  }, $panel);

  const backToAdmin = () => {
    if (!adminPinState.set) { activeMember = null; renderHome(); return; }
    $panel.replaceChildren(
      el('p', { class: 'text-sm', text: 'Enter the admin PIN to return to the administrator view.' }),
      pinForm({
        label: 'Admin PIN',
        submit: 'Unlock',
        onPin: async (pin) => {
          await api('POST', `/households/${h.id}/admin-pin/verify`, { pin });
          adminPin = pin;
          activeMember = null;
          renderHome();
        },
      }));
  };

  return el('div', { class: 'space-y-2' },
    el('div', { class: 'flex flex-wrap items-center gap-2' },
      $who,
      el('button', { class: 'btn btn-sm', text: 'Switch member', onclick: openPicker }),
      activeMember && el('button', { class: 'btn btn-sm btn-ghost', text: 'Administrator view', onclick: backToAdmin })),
    $panel);
}

// confirmClick turns a button into a two-step confirm (no blocking dialogs).
function confirmClick(prompt, fn) {
  return action(async (e) => {
    const btn = e.currentTarget;
    if (btn.dataset.armed) return fn();
    btn.dataset.armed = '1';
    const original = btn.textContent;
    btn.textContent = prompt;
    setTimeout(() => { delete btn.dataset.armed; btn.textContent = original; }, 3000);
  });
}

// ── Quests ────────────────────────────────────────────────────────────────────

async function questsTab(h, $root) {
  $root.replaceChildren(card(null,
    el('h3', { class: 'font-semibold', text: 'Quests' }),
    el('p', { class: 'text-sm opacity-70', text: 'Quests will appear here.' })));
}

// ── Family ────────────────────────────────────────────────────────────────────

async function familyTab(h, $root) {
  const rerender = () => familyTab(h, $root).catch((err) => showError($root, err));

  if (!managementUnlocked()) {
    $root.replaceChildren(card(null,
      el('h3', { class: 'font-semibold', text: 'Family' }),
      el('p', { class: 'text-sm opacity-70', text: 'Enter the admin PIN to manage the family.' }),
      pinForm({
        label: 'Admin PIN',
        submit: 'Unlock',
        onPin: async (pin) => {
          await api('POST', `/households/${h.id}/admin-pin/verify`, { pin });
          adminPin = pin;
          rerender();
        },
      })));
    return;
  }

  const base = `/households/${h.id}/family-members`;
  const members = (await api('GET', base)).items;

  const rows = members.map((m) => {
    const $row = el('li', { class: 'flex flex-wrap items-center gap-2 py-2 border-b border-base-300' },
      el('div', { class: 'flex-1 min-w-0' },
        el('div', { text: m.name }),
        el('div', { class: 'text-xs opacity-60', text: `${m.points} points` })),
      el('span', { class: 'badge badge-sm', text: m.has_pin ? 'PIN' : 'No PIN' }),
      el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Rename',
        onclick: () => $row.replaceWith(el('li', { class: 'py-2' }, form({
          fields: [{ name: 'name', label: 'Name', required: true, value: m.name }],
          submit: 'Save', inline: true,
          onSubmit: async (v) => { await adminRequest('PUT', `${base}/${m.id}`, { name: v.name }); rerender(); },
        }))),
      }),
      el('button', {
        class: 'btn btn-ghost btn-xs', text: m.has_pin ? 'Change PIN' : 'Set PIN',
        onclick: () => $row.replaceWith(el('li', { class: 'py-2' },
          el('p', { class: 'text-sm', text: `New PIN for ${m.name}` }),
          pinForm({
            submit: 'Save PIN',
            onPin: async (pin) => { await adminRequest('PUT', `${base}/${m.id}/pin`, { pin }); rerender(); },
          }))),
      }),
      m.has_pin && el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Clear PIN',
        onclick: confirmClick('Sure?', async () => { await adminRequest('DELETE', `${base}/${m.id}/pin`); rerender(); }),
      }),
      el('button', {
        class: 'btn btn-ghost btn-xs text-error', text: 'Remove',
        onclick: confirmClick('Sure?', async () => { await adminRequest('DELETE', `${base}/${m.id}`); rerender(); }),
      }));
    return $row;
  });

  const addForm = form({
    fields: [
      { name: 'name', label: 'Name', required: true },
      { name: 'pin', label: 'PIN (optional, 4 digits)', type: 'password', autocomplete: 'off' },
    ],
    submit: 'Add member', inline: true,
    onSubmit: async (v) => {
      const body = { name: v.name };
      if (v.pin !== '') { checkPin(v.pin); body.pin = v.pin; }
      await adminRequest('POST', base, body);
      rerender();
    },
  });

  $root.replaceChildren(
    card(null,
      el('h3', { class: 'font-semibold', text: 'Family' }),
      rows.length
        ? el('ul', {}, rows)
        : el('p', { class: 'opacity-70', text: 'No family members yet. Add one to get started.' }),
      el('div', { class: 'divider my-0' }),
      addForm),
    el('div', { class: 'h-4' }),
    adminPinCard(h, rerender));
}

// adminPinCard sets, changes or removes the admin PIN. The PIN itself is never
// shown back: only whether one is set.
function adminPinCard(h, rerender) {
  const $err = el('div');
  return card(null,
    el('h3', { class: 'font-semibold', text: 'Admin PIN' }),
    el('p', { class: 'text-sm', text: adminPinState.set ? 'Admin PIN set' : 'No admin PIN' }),
    $err,
    pinForm({
      label: adminPinState.set ? 'New admin PIN' : 'Admin PIN',
      submit: adminPinState.set ? 'Change admin PIN' : 'Set admin PIN',
      onPin: async (pin) => {
        await adminRequest('PUT', `/households/${h.id}/admin-pin`, { pin });
        adminPinState.set = true;
        adminPin = pin;
        rerender();
      },
    }),
    adminPinState.set && el('button', {
      class: 'btn btn-ghost btn-sm text-error', text: 'Remove admin PIN',
      onclick: confirmClick('Sure?', async () => {
        try {
          await adminRequest('DELETE', `/households/${h.id}/admin-pin`);
        } catch (err) {
          showError($err, pinFailure(err));
          return;
        }
        adminPinState.set = false;
        adminPin = null;
        rerender();
      }),
    }));
}

// ── Events ────────────────────────────────────────────────────────────────────

let monthOffset = 0;

async function eventsTab(h, $root) {
  const now = new Date();
  const from = new Date(now.getFullYear(), now.getMonth() + monthOffset, 1);
  const to = new Date(now.getFullYear(), now.getMonth() + monthOffset + 1, 1);
  const qs = new URLSearchParams({ from: from.toISOString(), to: to.toISOString() });
  const { status, items, members = [] } = await api('GET', `/households/${h.id}/events?${qs}`);
  const rerender = () => { $root.replaceChildren(); eventsTab(h, $root).catch((err) => showError($root, err)); };
  const toAccount = (e) => { e.preventDefault(); accountView(); };

  if (status === 'not_configured') {
    $root.replaceChildren(card(null,
      el('h3', { class: 'font-semibold', text: 'Calendar' }),
      el('p', { class: 'text-sm opacity-70', text: 'No family calendar is connected for this household yet. A member can connect Google Calendar and pick one on their Account page.' }),
      el('a', { class: 'link link-primary text-sm', href: '/account', text: 'Go to Account', onclick: toAccount })));
    return;
  }
  if (status === 'broken') {
    $root.replaceChildren(card(null,
      el('h3', { class: 'font-semibold', text: 'Calendar' }),
      alertBox('error', 'The Google Calendar connection behind this family calendar is broken. The member who connected it needs to Reconnect on their Account page.'),
      el('a', { class: 'link link-primary text-sm', href: '/account', text: 'Go to Account', onclick: toAccount })));
    return;
  }
  if (status !== 'ok') {
    $root.replaceChildren(card(null,
      el('h3', { class: 'font-semibold', text: 'Calendar' }),
      el('p', { class: 'text-sm opacity-70', text: 'Google Calendar is not available on this server.' })));
    return;
  }

  const nav = el('div', { class: 'join' },
    el('button', { class: 'btn btn-sm join-item', text: '‹', onclick: () => { monthOffset--; rerender(); } }),
    el('button', { class: 'btn btn-sm join-item', text: from.toLocaleDateString(undefined, { month: 'long', year: 'numeric' }), onclick: () => { monthOffset = 0; rerender(); } }),
    el('button', { class: 'btn btn-sm join-item', text: '›', onclick: () => { monthOffset++; rerender(); } }));

  // All-day entries carry a YYYY-MM-DD date; parse it as a local date.
  const localDate = (d) => { const [y, m, day] = d.split('-').map(Number); return new Date(y, m - 1, day); };
  // Entries come from Google and are read-only here: no add, edit or delete.
  const rows = items.map((e) => {
    let currentTitle = e.title;
    let currentTags = e.tags || [];

    const when = e.all_day ? localDate(e.start).toLocaleDateString() + ' (all day)' : `${fmtDate(e.start)} – ${fmtDate(e.end)}`;
    const $title = el('div', { text: currentTitle || '(no title)' });
    const $meta = el('div', { class: 'text-xs opacity-60', text: when + (e.location ? ` · ${e.location}` : '') });
    const $error = el('div');
    const $picker = el('div', { class: 'mt-2 p-2 bg-base-200 rounded text-sm space-y-2' });
    $picker.style.display = 'none';

    let $tagBtn = null;
    if (e.can_tag) {
      let isOpen = false;
      const tagCheckboxes = [];

      const renderPicker = () => {
        $picker.replaceChildren();
        tagCheckboxes.length = 0;

        const tagSet = new Set(currentTags);
        const memberRows = members.map((m) => {
          const isTaggable = !!m.taggable;
          const isChecked = tagSet.has(m.member_id);
          const cb = el('input', {
            type: 'checkbox',
            class: 'checkbox checkbox-xs',
            checked: isChecked,
            disabled: !isTaggable,
          });
          if (isTaggable) {
            tagCheckboxes.push({ id: m.member_id, input: cb });
          }

          let labelText = m.name;
          if (!isTaggable) {
            let reasonText = m.reason;
            if (m.reason === 'duplicate_name') reasonText = 'duplicate name';
            else if (m.reason === 'reserved_character') reasonText = 'name contains , [ or ]';
            else if (m.reason === 'empty_name') reasonText = 'empty name';
            labelText += ` (${reasonText})`;
          }

          return el('label', { class: `label cursor-pointer justify-start gap-2 py-0.5 ${!isTaggable ? 'opacity-50' : ''}` },
            cb,
            el('span', { class: 'text-xs', text: labelText }));
        });

        const $saveBtn = el('button', {
          class: 'btn btn-primary btn-xs',
          text: 'Save',
          onclick: async (evt) => {
            evt.preventDefault();
            $error.replaceChildren();
            const selected = tagCheckboxes.filter((item) => item.input.checked).map((item) => item.id);
            $saveBtn.disabled = true;
            try {
              const res = await api('PUT', `/households/${h.id}/calendar/entries/${encodeURIComponent(e.id)}/tags`, { tags: selected });
              currentTitle = res.title;
              currentTags = res.tags || [];
              $title.textContent = currentTitle || '(no title)';
              isOpen = false;
              $picker.style.display = 'none';
            } catch (err) {
              showError($error, err);
            } finally {
              $saveBtn.disabled = false;
            }
          },
        });

        const $cancelBtn = el('button', {
          class: 'btn btn-ghost btn-xs',
          text: 'Cancel',
          onclick: (evt) => {
            evt.preventDefault();
            $error.replaceChildren();
            isOpen = false;
            $picker.style.display = 'none';
          },
        });

        $picker.append(
          el('div', { class: 'font-semibold text-xs', text: 'Tag family members' }),
          el('div', { class: 'space-y-0.5' }, memberRows),
          el('div', { class: 'flex gap-2 mt-2' }, $saveBtn, $cancelBtn));
      };

      $tagBtn = el('button', {
        class: 'btn btn-ghost btn-xs',
        text: 'Tag',
        onclick: (evt) => {
          evt.preventDefault();
          $error.replaceChildren();
          isOpen = !isOpen;
          if (isOpen) {
            renderPicker();
            $picker.style.display = 'block';
          } else {
            $picker.style.display = 'none';
          }
        },
      });
    }

    const rowContent = el('div', { class: 'flex-1 min-w-0' },
      $title,
      $meta,
      $error,
      e.can_tag ? $picker : null);

    return el('li', { class: 'flex items-start gap-3 py-2 border-b border-base-300' },
      rowContent,
      $tagBtn);
  });

  $root.replaceChildren(card(null,
    el('div', { class: 'flex items-center justify-between' }, el('h3', { class: 'font-semibold', text: 'Calendar' }), nav),
    el('ul', {}, rows.length ? rows : el('li', { class: 'opacity-60 py-2', text: 'No events this month.' }))));
}

// ── Account ───────────────────────────────────────────────────────────────────

async function accountView() {
  renderNav();
  const calOutcome = new URLSearchParams(location.search).get('google_calendar');
  let $outcomeAlert = null;
  if (calOutcome) {
    history.replaceState(null, '', location.pathname);
    if (calOutcome === 'connected') {
      const msg = 'Google Calendar connected.';
      $outcomeAlert = alertBox('success', msg);
      toast(msg, 'success');
    } else {
      const msg = 'Could not connect Google Calendar. Please try again.';
      $outcomeAlert = alertBox('error', msg);
      toast(msg, 'error');
    }
  }

  const { items: sessions } = await api('GET', '/me/sessions');
  const $sessionsErr = el('div');

  const sessionRows = sessions.map((s) => el('li', { class: 'flex items-center gap-3 py-2 border-b border-base-300' },
    el('div', { class: 'flex-1 min-w-0' },
      el('div', { text: (s.device_name || 'Unknown device') + (s.current ? ' (this device)' : '') }),
      el('div', { class: 'text-xs opacity-60', text: `${s.ip_address || '?'} · last used ${fmtDate(s.last_used_at)}` })),
    el('button', {
      class: 'btn btn-ghost btn-xs text-error', text: 'Revoke',
      onclick: action(async () => {
        await api('DELETE', `/me/sessions/${s.id}`);
        if (s.current) { clearTokens(); return goHome(); }
        accountView();
      }, $sessionsErr),
    })));

  const hasPassword = session.user?.has_password !== false;

  const $calErr = el('div');
  const $calCard = el('div', { class: 'card bg-base-100 shadow' });

  const onConnect = action(async () => {
    const resp = await api('POST', '/me/google-calendar/connect');
    if (resp?.url) {
      window.location = resp.url;
    }
  }, $calErr);

  const onDisconnect = action(async () => {
    await api('DELETE', '/me/google-calendar');
    toast('Google Calendar disconnected.', 'info');
    renderCalendarCard({ status: 'not_connected' });
  }, $calErr);

  async function loadFamilyCalendars($root) {
    const $sectionErr = el('div');
    $root.replaceChildren(
      el('h3', { class: 'text-sm font-semibold', text: 'Family calendars' }),
      $sectionErr,
      el('p', { class: 'text-xs opacity-70', text: 'Loading calendars…' }),
    );

    let calResp;
    let householdsList;
    try {
      [calResp, householdsList] = await Promise.all([
        api('GET', '/me/google-calendar/calendars'),
        api('GET', '/households').then((r) => r.items || []),
      ]);
    } catch (err) {
      $root.replaceChildren(
        el('h3', { class: 'text-sm font-semibold', text: 'Family calendars' }),
        $sectionErr,
        el('p', { class: 'text-xs opacity-70', text: 'Could not load your Google calendars.' }),
      );
      showError($sectionErr, err);
      return;
    }

    const calendars = calResp?.items || [];
    if (calendars.length === 0) {
      $root.replaceChildren(
        el('h3', { class: 'text-sm font-semibold', text: 'Family calendars' }),
        $sectionErr,
        el('p', { class: 'text-xs opacity-70', text: 'No Google calendars found.' }),
      );
      return;
    }

    if (householdsList.length === 0) {
      $root.replaceChildren(
        el('h3', { class: 'text-sm font-semibold', text: 'Family calendars' }),
        $sectionErr,
        el('p', { class: 'text-xs opacity-70', text: 'You do not belong to any households yet.' }),
      );
      return;
    }

    const householdStatuses = await Promise.all(
      householdsList.map(async (h) => {
        try {
          const status = await api('GET', `/households/${h.id}/calendar`);
          return { h, status };
        } catch (err) {
          return { h, status: null, err };
        }
      })
    );

    const rows = householdStatuses.map(({ h, status }) => {
      const $rowErr = el('div');
      const setBy = status?.set_by;
      const isOtherSetter = setBy && setBy.id && setBy.id !== session.user?.id;

      if (isOtherSetter) {
        const calName = status.calendar_name || status.calendar_id || 'Calendar';
        const setterName = setBy.display_name || 'another member';
        return el('div', { class: 'space-y-1' },
          el('div', { class: 'text-sm font-medium', text: h.name }),
          el('p', { class: 'text-xs opacity-70', text: `${h.name}: ${calName} (set by ${setterName})` }),
        );
      }

      const $select = el('select', { class: 'select select-bordered select-sm w-full max-w-xs' },
        calendars.map((c) => el('option', {
          value: c.id,
          text: c.name + (c.primary ? ' (primary)' : ''),
          selected: status?.calendar_id ? c.id === status.calendar_id : !!c.primary,
        }))
      );

      const $saveBtn = el('button', {
        class: 'btn btn-primary btn-sm',
        text: 'Set as family calendar',
        onclick: action(async () => {
          await api('PUT', `/households/${h.id}/calendar`, { calendar_id: $select.value });
          toast(`Updated family calendar for ${h.name}.`, 'success');
          loadFamilyCalendars($root);
        }, $rowErr),
      });

      const buttons = [$saveBtn];
      if (setBy && setBy.id === session.user?.id) {
        buttons.push(el('button', {
          class: 'btn btn-ghost btn-sm text-error',
          text: 'Clear',
          onclick: action(async () => {
            await api('DELETE', `/households/${h.id}/calendar`);
            toast(`Cleared family calendar for ${h.name}.`, 'info');
            loadFamilyCalendars($root);
          }, $rowErr),
        }));
      }

      return el('div', { class: 'space-y-1' },
        el('div', { class: 'text-sm font-medium', text: h.name }),
        $rowErr,
        el('div', { class: 'flex flex-wrap items-center gap-2' },
          $select,
          ...buttons,
        ),
      );
    });

    $root.replaceChildren(
      el('h3', { class: 'text-sm font-semibold', text: 'Family calendars' }),
      $sectionErr,
      el('div', { class: 'space-y-3' }, ...rows),
    );
  }

  function renderCalendarCard(cal) {
    $calErr.replaceChildren();
    let content;
    switch (cal.status) {
      case 'not_connected':
        content = [
          el('p', { class: 'text-sm opacity-70', text: 'Connect your Google Calendar to sync family events.' }),
          el('button', { class: 'btn btn-primary btn-sm', text: 'Connect Google Calendar', onclick: onConnect }),
        ];
        break;
      case 'connected': {
        const $familySection = el('div', { class: 'space-y-3 pt-3 border-t border-base-300' });
        loadFamilyCalendars($familySection);
        content = [
          el('p', { class: 'text-sm font-medium', text: 'Connected' }),
          cal.access === 'read_only'
            ? el('p', { class: 'text-xs opacity-70', text: 'Read-only access (Chorus cannot modify your calendar).' })
            : null,
          el('button', { class: 'btn btn-outline btn-error btn-sm', text: 'Disconnect', onclick: onDisconnect }),
          $familySection,
        ];
        break;
      }
      case 'broken':
        content = [
          el('p', { class: 'text-sm text-error', text: 'Google Calendar connection is broken.' }),
          el('div', { class: 'flex gap-2' },
            el('button', { class: 'btn btn-primary btn-sm', text: 'Reconnect', onclick: onConnect }),
            el('button', { class: 'btn btn-ghost btn-sm text-error', text: 'Disconnect', onclick: onDisconnect })),
        ];
        break;
      case 'unavailable':
      default:
        content = [
          el('p', { class: 'text-sm opacity-70', text: 'Google Calendar is not configured.' }),
        ];
        break;
    }
    $calCard.replaceChildren(
      el('div', { class: 'card-body gap-3' },
        el('h2', { class: 'card-title', text: 'Google Calendar' }),
        $calErr,
        ...content));
  }

  let cal = { status: 'unavailable' };
  try {
    cal = await api('GET', '/me/google-calendar');
  } catch {
    cal = { status: 'unavailable' };
  }
  renderCalendarCard(cal);

  $app.replaceChildren(el('div', { class: 'max-w-2xl mx-auto space-y-6' },
    el('button', { class: 'btn btn-ghost btn-sm', text: '← Back', onclick: () => homeView() }),
    $outcomeAlert,
    card('Profile', el('p', { class: 'text-sm opacity-70', text: session.user.email }), form({
      fields: [{ name: 'display_name', label: 'Display name', required: true, value: session.user.display_name }],
      submit: 'Save',
      onSubmit: async (v) => { session.user = await api('PUT', '/me', v); renderNav(); toast('Saved', 'success'); },
    })),
    $calCard,
    hasPassword
      ? card('Change password', form({
          fields: [
            { name: 'current_password', label: 'Current password', type: 'password', required: true, autocomplete: 'current-password' },
            { name: 'new_password', label: 'New password', type: 'password', required: true, autocomplete: 'new-password' },
          ],
          submit: 'Change password',
          onSubmit: async (v, $form) => { await api('POST', '/me/password', v); $form.reset(); toast('Password changed. Other devices were signed out.', 'success'); accountView(); },
        }))
      : card('Password', el('p', { class: 'text-sm opacity-70', text: 'You sign in with Google; no password is set.' })),
    card('Signed-in devices', $sessionsErr, el('ul', {}, sessionRows)),
    card('Delete account',
      el('p', { class: 'text-sm', text: 'Permanently deletes your account, and with it your household and its family members.' }),
      hasPassword
        ? form({
            fields: [{ name: 'password', label: 'Confirm password', type: 'password', required: true, autocomplete: 'current-password' }],
            submit: 'Delete my account',
            onSubmit: async (v) => { await api('DELETE', '/me', v); clearTokens(); goHome(); },
          })
        : form({
            fields: [{ name: 'email', label: 'Confirm email', type: 'email', required: true, autocomplete: 'email' }],
            submit: 'Delete my account',
            onSubmit: async (v) => { await api('DELETE', '/me', v); clearTokens(); goHome(); },
          }))));
}

boot().catch((err) => showError($app, err));
