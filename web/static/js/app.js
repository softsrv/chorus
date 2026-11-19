// Chorus web test client. Plain DOM, no framework. All user-supplied text is
// inserted with textContent (never innerHTML), so API data can't inject markup.

import { api, publicApi, refresh, setTokens, clearTokens, setLogger, session, ApiError, DEVICE_NAME, authConfig, googleSignIn } from './api.js';

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
    else if (k === 'disabled') {
      node.disabled = Boolean(v);
      if (v) node.setAttribute('disabled', '');
      else delete node.attributes['disabled'];
    }
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
// A 'radio' field is a group of options ([value, label]) whose value is the
// checked one; a 'note' field is a line of help text with no input. Fields
// sharing a `group` are shown together in a fieldset with that legend, and
// `show(values)` hides a field while it returns false.
function form({ fields, submit, onSubmit, inline = false }) {
  const $error = el('div');
  const inputs = {};
  const hints = {};
  const radioGroups = {};
  const multi = new Set(fields.filter((f) => f.type === 'multiselect').map((f) => f.name));
  const fieldRows = fields.map((f) => {
    if (f.type === 'note') return { f, $row: el('p', { class: 'text-xs opacity-70', text: f.text }) };
    if (f.type === 'radio') {
      const radios = f.options.map(([value]) => el('input', {
        type: 'radio', name: f.name, value, class: 'radio radio-sm', checked: value === f.value,
        onclick: (e) => { for (const r of radios) r.checked = r === e.currentTarget; updateVisibility(); },
        onchange: () => updateVisibility(),
      }));
      radioGroups[f.name] = radios;
      return {
        f,
        $row: el('div', { role: 'radiogroup', class: 'flex flex-wrap gap-4' },
          f.options.map(([, label], i) => el('label', { class: 'label cursor-pointer justify-start gap-2' }, radios[i], el('span', { text: label })))),
      };
    }
    let input;
    if (f.type === 'select') {
      input = el('select', { name: f.name, class: 'select select-bordered w-full' },
        f.options.map(([value, label]) => el('option', { value, text: label, selected: value === (f.value ?? '') })));
    } else if (f.type === 'multiselect') {
      // value is the array of selected option values.
      const chosen = f.value || [];
      input = el('select', { name: f.name, multiple: true, class: 'select select-bordered w-full' },
        f.options.map(([value, label]) => el('option', { value, text: label, selected: chosen.includes(value) })));
    } else if (f.type === 'textarea') {
      input = el('textarea', { name: f.name, class: 'textarea textarea-bordered w-full', rows: 2, text: f.value ?? '' });
    } else if (f.type === 'checkbox') {
      input = el('input', {
        type: 'checkbox', name: f.name, class: 'checkbox', checked: !!f.value,
        onclick: () => {
          if (typeof window !== 'undefined' && typeof window.HTMLInputElement === 'undefined') {
            input.checked = !input.checked;
          }
          updateVisibility();
        },
        onchange: () => updateVisibility(),
      });
    } else {
      input = el('input', {
        type: f.type || 'text', name: f.name, class: 'input input-bordered w-full',
        required: f.required, value: f.value ?? '', placeholder: f.placeholder, autocomplete: f.autocomplete,
      });
    }
    inputs[f.name] = input;
    hints[f.name] = el('p', { class: 'text-error text-xs mt-1' });
    let $row;
    if (f.type === 'checkbox') {
      $row = el('label', { class: 'label cursor-pointer justify-start gap-2' }, input, el('span', { text: f.label }));
    } else {
      $row = el('label', { class: inline ? 'flex-1 min-w-32' : 'block' },
        el('span', { class: 'text-sm', text: f.label }), input, hints[f.name]);
    }
    return { f, $row };
  });

  const getValues = () => {
    const values = {};
    for (const [name, input] of Object.entries(inputs)) {
      if (input.type === 'checkbox') values[name] = input.checked;
      else if (multi.has(name)) values[name] = Array.from(input.children).filter((o) => o.selected).map((o) => o.value);
      else values[name] = input.value;
    }
    for (const [name, radios] of Object.entries(radioGroups)) values[name] = radios.find((r) => r.checked)?.value ?? '';
    return values;
  };

  // layout is the form's rows for these values: hidden fields left out, and
  // grouped fields gathered into their fieldset where the group first appears.
  const groups = {};
  const layout = (values) => {
    const top = [];
    for (const { f, $row } of fieldRows) {
      if (f.show && !f.show(values)) continue;
      if (!f.group) { top.push($row); continue; }
      const $set = (groups[f.group] ||= el('fieldset', { class: 'fieldset w-full border border-base-300 rounded p-2' }));
      if (!top.includes($set)) {
        $set.replaceChildren(el('legend', { class: 'fieldset-legend text-sm', text: f.group }));
        top.push($set);
      }
      $set.append($row);
    }
    return top;
  };

  const updateVisibility = () => {
    $form.replaceChildren($error, ...layout(getValues()), $submit);
  };

  const initialValues = {};
  for (const f of fields) {
    if (f.type === 'checkbox') initialValues[f.name] = !!f.value;
    else initialValues[f.name] = f.value ?? '';
  }
  const visibleRows = layout(initialValues);

  const $submit = el('button', { type: 'submit', class: 'btn btn-primary', text: submit });
  const $form = el('form', {
    class: inline ? 'flex flex-wrap items-end gap-2' : 'space-y-3',
    onsubmit: async (e) => {
      e.preventDefault();
      $error.replaceChildren();
      Object.values(hints).forEach((h) => { h.textContent = ''; });
      const values = getValues();
      $submit.disabled = true;
      try {
        await onSubmit(values, $form);
      } catch (err) {
        showError($error, err, hints);
      } finally {
        $submit.disabled = false;
      }
    },
  }, $error, ...visibleRows, $submit);
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
  return t;
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

const GIS_WAIT_TRIES = 50; // the GIS script loads async; wait up to ~5s for it
const GIS_WAIT_MS = 100;
const PERSON_CANCELLED = new Set(['user_cancel', 'tap_outside']);
// The rendered button opens Google's own window and reports nothing when it
// is closed. After a click, the page getting focus back with no credential
// following within this grace period is taken as the person cancelling. A
// guess can be wrong (a slow credential), so a credential that arrives after
// the cancel message withdraws it: the message only stands while nothing
// has arrived.
const GIS_CANCEL_GRACE_MS = 300;
let removeButtonFocusListener = null;

// googleButton returns the divider and a container that Google Identity
// Services renders its own button into. The credential (a Google ID token)
// goes to POST /auth/google; a dismissed prompt or an empty credential sends
// nothing. Messages are fixed strings shown via textContent, never server or
// Google error text.
function googleButton(cfg) {
  const $status = el('div');
  const $btn = el('div', { class: 'flex justify-center', 'data-google-button': '' });
  let inFlight = false;
  let clicked = false; // the button opened Google's window, no credential yet
  let graceTimer = null;
  let cancelToast = null; // the shown cancel toast, withdrawn by a credential

  const show = (msg) => {
    $status.replaceChildren(alertBox('error', msg));
    return toast(msg, 'error');
  };
  const cancelled = () => {
    cancelToast?.remove();
    cancelToast = show('Google sign-in was cancelled');
  };
  // withdrawCancel clears a cancel message shown by the focus timer or the
  // One Tap notification once a real credential arrives after all.
  const withdrawCancel = () => {
    cancelToast?.remove();
    cancelToast = null;
    $status.replaceChildren();
  };

  const onCredential = async (resp) => {
    clicked = false;
    clearTimeout(graceTimer);
    const idToken = resp?.credential;
    if (typeof idToken !== 'string' || idToken === '') return cancelled();
    if (inFlight) return;
    inFlight = true;
    withdrawCancel();
    try {
      await googleSignIn(idToken);
    } catch {
      inFlight = false;
      return show('Google sign-in failed');
    }
    goHome();
  };

  const render = (tries) => {
    const gis = window.google?.accounts?.id;
    if (!gis) {
      if (tries > 0) setTimeout(() => render(tries - 1), GIS_WAIT_MS);
      else $status.replaceChildren(alertBox('warning', 'Google sign-in is unavailable right now.'));
      return;
    }
    gis.initialize({ client_id: cfg.google_client_id, callback: onCredential });
    gis.renderButton($btn, {
      type: 'standard', theme: 'outline', size: 'large', text: 'signin_with',
      click_listener: () => { clicked = true; },
    });
    // One focus listener at a time: a re-rendered sign-in view replaces the
    // previous button's.
    removeButtonFocusListener?.();
    const onFocus = () => {
      if (!clicked) return;
      clearTimeout(graceTimer);
      graceTimer = setTimeout(() => {
        if (!clicked || inFlight) return;
        clicked = false;
        cancelled();
      }, GIS_CANCEL_GRACE_MS);
    };
    window.addEventListener('focus', onFocus);
    removeButtonFocusListener = () => {
      window.removeEventListener('focus', onFocus);
      clearTimeout(graceTimer);
    };
    gis.prompt((n) => {
      // One Tap closed by the person without a credential. Auto-cancels and
      // "not displayed" (no Google session) are not the person cancelling.
      if (n?.isSkippedMoment?.() && PERSON_CANCELLED.has(n.getSkippedReason?.())) cancelled();
    });
  };
  render(GIS_WAIT_TRIES);

  return [el('div', { class: 'divider', text: 'or' }), $btn, $status];
}

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
    if (cfg?.google_sign_in === true) $body.append(...googleButton(cfg));
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
    if (cfg?.google_sign_in === true) $body.append(...googleButton(cfg));
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
// memberHistory is the active member's points and reward history, read once
// per member selection (and again after an admin adjusts that member's points
// on the Family tab): { member (the activeMember object it was read for),
// ready }.
let memberHistory = null;
// memberBarPoints re-reads the active member's total into the member bar
// drawn last (set by memberBar); null before one is drawn.
let memberBarPoints = null;
// familyView marks the Family tab's member view in flight; a points-history
// answer for a view the admin has since left is not rendered.
let familyView = null;

// clearMemberState returns to the administrator's view with nothing unlocked.
function clearMemberState() {
  activeMember = null;
  adminPin = null;
  adminPinState = { householdId: null, set: false };
  memberHistory = null;
  familyView = null;
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
  const tabs = [['quests', 'Quests'], ['rewards', 'Rewards'], manage && ['family', 'Family'], ['events', 'Calendar']].filter(Boolean);
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

  const loaders = { quests: questsTab, rewards: rewardsTab, family: familyTab, events: eventsTab };
  (loaders[tab] || questsTab)(h, $content).catch((err) => showError($content, err));
  return el('div', { class: 'space-y-4 min-w-0' }, header, memberBar(h), tabBar, $content);
}

// memberBar shows whose view this is and switches it: pick a member (with
// their PIN when they have one), or go back to the administrator's view (with
// the admin PIN when one is set).
function memberBar(h) {
  const $panel = el('div');
  const $who = el('span', { class: 'text-sm flex-1', text: `Viewing as: ${activeMember ? activeMember.name : 'Administrator'}` });
  // The active member's points come from the server on every render (never
  // computed here), so an approval shows up the next time the view is drawn.
  // memberBarPoints re-reads them on demand, for a write made outside the
  // member's own actions (an admin adjustment on the Family tab).
  const showPoints = () => {
    if (!activeMember) return;
    const shown = activeMember;
    api('GET', `/households/${h.id}/family-members`)
      .then((r) => {
        const m = (r.items || []).find((x) => x.id === shown.id);
        if (m && activeMember === shown) $who.textContent = `Viewing as: ${m.name} · ${m.points} points`;
      })
      .catch((err) => console.error('member points', err)); // the name alone stays; the tab surfaces API errors
  };
  memberBarPoints = showPoints;
  showPoints();

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
          members.map((m) => el('button', { class: 'btn btn-sm', text: `${m.name} · ${m.points} points`, onclick: pick(m) })))
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

// Quest list partitions come only from the server's fields: available says the
// quest is in the pool, claimed_by says who holds it. A marked-done chore has
// no holder (claimed_by is null) and rests until its cooldown ends, then returns
// to available even if awaiting approval. The client does no date arithmetic;
// a resting quest shows its available_at as the server sent it. An available
// quest whose ineligible_member_ids (the server's age check) lists the member
// is left out of Available and kept in leftOut, so the view can say so; the
// client never reads a birthday or works out an age. Mine is never filtered:
// a member keeps a quest they hold whatever its range.
function partitionQuests(items, memberId) {
  const out = { available: [], mine: [], claimed: [], resting: [], leftOut: [] };
  for (const q of items) {
    if (q.available) ((q.ineligible_member_ids || []).includes(memberId) ? out.leftOut : out.available).push(q);
    else if ((q.claimed_by && q.claimed_by === memberId) || (q.participants || []).includes(memberId)) out.mine.push(q);
    else if (q.claimed_by) out.claimed.push(q);
    else out.resting.push(q);
  }
  return out;
}

// questRangeText is the one wording of a quest's age range: 'All ages',
// 'Ages 10–14', 'Ages 6+' or 'Up to 12'. Every view that names a range calls
// it; a sentence that needs it lower-cased uses lowerFirst.
function questRangeText(q) {
  const min = q.min_age ?? null;
  const max = q.max_age ?? null;
  if (min === null && max === null) return 'All ages';
  if (min !== null && max !== null) return `Ages ${min}–${max}`;
  if (min !== null) return `Ages ${min}+`;
  return `Up to ${max}`;
}

const lowerFirst = (s) => s.charAt(0).toLowerCase() + s.slice(1);

const isAdminPinError = (err) => err instanceof ApiError && err.status === 403 &&
  (err.code === 'wrong_pin' || err.code === 'admin_pin_required');

// questNotice maps an expected quest-action failure to the message shown after
// the list reloads: a lost accept race (409 quest_taken), a member outside the
// quest's age range on accept or share (409 age_restricted), or a quest that is
// gone, outside this household or no longer pending (404).
function questNotice(err) {
  if (!(err instanceof ApiError)) return null;
  if (err.status === 409 && err.code === 'quest_taken') return 'This quest was already taken.';
  if (err.status === 409 && err.code === 'already_participant') return 'That member is already on this quest.';
  if (err.status === 409 && err.code === 'age_restricted') return "This quest is outside that member's age range.";
  if (err.status === 404) return 'Quest not found.';
  return null;
}

// waitingBadge marks a quest that has a completion waiting for approval.
const waitingBadge = (q) => q.awaiting_approval && el('span', { class: 'badge badge-sm', text: 'Waiting for approval' });

const questMeta = (q) => `${q.points} points · resets at midnight after ${q.repeat_days} day(s)`;

// joinNames lists names with commas and 'and' before the last: 'A', 'A and B',
// 'A, B and C'.
const joinNames = (names) => (names.length <= 1 ? names.join('') : `${names.slice(0, -1).join(', ')} and ${names.at(-1)}`);

// pendingMembers is every member recorded on a pending completion, each with
// the name and the split award the server computed; an item without a members
// list is the one member who marked it done, paid the full points.
const pendingMembers = (q) => (q.members && q.members.length
  ? q.members
  : [{ member_id: q.member_id, member_name: q.member_name, points: q.points }]);

async function questsTab(h, $root, notice = null) {
  const rerender = (next = null) => { $root.replaceChildren(); questsTab(h, $root, next).catch((err) => showError($root, err)); };
  const member = activeMember; // the view this render is for; a later pick makes it stale

  const [page, library, family] = await Promise.all([
    api('GET', `/households/${h.id}/quests?limit=100`),
    api('GET', '/quest-library'),
    api('GET', `/households/${h.id}/family-members`),
  ]);
  const quests = page.items || [];
  const members = family.items || [];
  const combosData = managementVisible() ? await loadCombosData(h, page) : null;

  const cards = [
    notice && alertBox('warning', notice),
    member ? await takeCard(h, quests, members, rerender, member) : card(null,
      el('h3', { class: 'font-semibold', text: 'Quests' }),
      el('p', { class: 'text-sm opacity-70', text: 'Choose a family member to take quests.' })),
    !member && await approvalsCard(h, members, rerender),
    managementVisible() && manageQuestsCard(h, quests, library, page, rerender),
    combosData && manageCombosCard(h, combosData, rerender),
  ].filter(Boolean);
  if (activeMember !== member) return; // a newer pick renders its own view; never overwrite it with this one
  $root.replaceChildren(el('div', { class: 'space-y-4' }, cards));
}

// loadCombosData reads the combos and every page of quests (following
// next_offset from the first page), so the combos card names and offers every
// household quest. A failure here is the combos card's alone: it comes back as
// { error } and the rest of the tab still renders.
async function loadCombosData(h, firstPage) {
  try {
    const comboPage = await api('GET', `/households/${h.id}/combos`);
    const all = [...(firstPage.items || [])];
    let next = firstPage.next_offset;
    for (let pages = 1; next !== null && next !== undefined; pages += 1) {
      if (pages >= 1000) throw new Error('Too many quest pages.');
      const more = await api('GET', `/households/${h.id}/quests?limit=100&offset=${next}`);
      all.push(...(more.items || []));
      next = more.next_offset;
    }
    return { combos: comboPage.items || [], quests: all, error: null };
  } catch (err) {
    console.error(err);
    return { combos: [], quests: [], error: 'Could not load combos. Reload to try again.' };
  }
}

// takeCard is the active member's view: Available, Mine and Claimed lists, a
// quest detail with Accept/Decline, and Abandon/Mark done on their own quests.
// Every action sends activeMemberAction(); the server decides the outcome and
// the list is re-read from it afterwards. Every row whose quest still has a
// completion waiting for approval shows 'Waiting for approval', whoever holds
// it now: a chore re-taken after its cooldown keeps showing the earlier
// completion that waits, and Mark done stays, since each marking is its own
// completion.
async function takeCard(h, quests, members, rerender, member = activeMember) {
  const $err = el('div');
  const $detail = el('div');
  const $share = el('div');
  const nameOf = Object.fromEntries(members.map((m) => [m.id, m.name]));
  // Everything this card shows and sends belongs to `member`, the member it
  // was started for: the lists, the title, the history and the actions. A
  // different member picked (or the selection cleared) while the history
  // read is in flight makes this card stale, and it renders nothing.
  const parts = partitionQuests(quests, member.id);
  // The points history is this card's own read, made once per member
  // selection (memberHistory is keyed on the activeMember object, which each
  // pick replaces, and holds the one in-flight read for it), never on the
  // rerender after an accept, done or abandon: those actions award no
  // points, and the read has a PIN-limited budget of its own. Its failure (a
  // 429, a 5xx, a network error) shows inside the history section until the
  // member is picked again or the page reloads, and the quest lists still
  // render. A 403 wrong_pin goes to the same handler member actions use,
  // which returns to the picker; a stale read's wrong_pin is dropped.
  // The reward history (approved redemptions) is read alongside it, under the
  // same rules: once per selection, its own failure shown only in its own
  // section, a wrong_pin handed to memberActionFailed.
  if (memberHistory?.member !== member) {
    const read = (path, label) => api('GET', `/households/${h.id}/family-members/${member.id}/${path}`, undefined, {
      headers: { 'X-Member-PIN': member.pin ?? '' },
    }).then(
      (res) => ({ items: res?.items || [], error: null, failure: null }),
      (err) => {
        if (err instanceof ApiError && err.code === 'wrong_pin') return { items: [], error: null, failure: err };
        console.error(label, err);
        return { items: [], error: `Could not load ${label}. Reload to try again.`, failure: null };
      });
    const ready = Promise.all([read('points-history', 'points history'), read('reward-history', 'reward history')])
      .then(([points, rewards]) => ({ points, rewards, failure: points.failure || rewards.failure }));
    memberHistory = { member, ready };
  }
  const result = await memberHistory.ready;
  if (activeMember !== member) return null; // another member was picked meanwhile
  if (result.failure) {
    memberHistory = null;
    memberActionFailed(result.failure);
    return null;
  }
  const history = result.points.items;
  const historyError = result.points.error;
  const rewardHistory = result.rewards.items;
  const rewardHistoryError = result.rewards.error;

  const act = (verb, q) => action(async () => {
    if (activeMember !== member) return; // this card is stale: never send another member's identity for its quests
    const body = activeMemberAction();
    if (!body) return;
    try {
      await api('POST', `/households/${h.id}/quests/${q.id}/${verb}`, body);
    } catch (err) {
      if (memberActionFailed(err)) return;
      const msg = questNotice(err);
      if (msg) { rerender(msg); return; }
      throw err;
    }
    rerender();
  }, $err);

  const showDetail = (q) => $detail.replaceChildren(el('div', { class: 'p-3 bg-base-200 rounded space-y-2' },
    el('div', { class: 'font-semibold', text: q.name }),
    el('div', { class: 'text-xs opacity-60', text: questMeta(q) }),
    el('p', { class: 'text-sm whitespace-pre-line', text: q.notes || 'No notes.' }),
    el('div', { class: 'flex gap-2' },
      el('button', { class: 'btn btn-primary btn-sm', text: 'Accept', onclick: act('accept', q) }),
      el('button', { class: 'btn btn-ghost btn-sm', text: 'Decline', onclick: (e) => { e.preventDefault(); $detail.replaceChildren(); } }))));

  const row = (q, ...extra) => el('li', { class: 'flex flex-wrap items-center gap-2 py-2 border-b border-base-300' },
    el('div', { class: 'flex-1 min-w-0' },
      el('div', { class: 'font-medium', text: q.name }),
      el('div', { class: 'text-xs opacity-60', text: questMeta(q) })),
    ...extra);

  // shareCandidates: the household members a quest can still be shared with,
  // everyone but the active member and those already on it.
  const shareCandidates = (q) => {
    const on = new Set([member.id, ...(q.participants || [])]);
    return members.filter((m) => !on.has(m.id));
  };

  // share POSTs the active member's action plus the picked member and their
  // PIN. The server marks a wrong target PIN with fields.target_pin; it is
  // rethrown so the target's PIN form shows it. Any other wrong_pin is the
  // active member's, handled as on accept: the member is cleared and the
  // user is sent back to the picker.
  const share = async (q, m, targetPin) => {
    if (activeMember !== member) return; // this card is stale: never send another member's identity
    const body = activeMemberAction();
    if (!body) return;
    try {
      await api('POST', `/households/${h.id}/quests/${q.id}/share`, { ...body, target_member_id: m.id, target_pin: targetPin });
    } catch (err) {
      if (err instanceof ApiError && err.code === 'wrong_pin' && err.fields && err.fields.target_pin) throw err;
      if (memberActionFailed(err)) return;
      const msg = questNotice(err);
      if (msg) { rerender(msg); return; }
      throw err;
    }
    rerender();
  };

  const pickTarget = (q, m) => action(async () => {
    if (!m.has_pin) { await share(q, m, ''); return; }
    $share.replaceChildren(el('div', { class: 'p-3 bg-base-200 rounded space-y-2' },
      el('p', { class: 'text-sm', text: `${m.name} enters their PIN to accept, as when accepting a quest.` }),
      pinForm({ label: `PIN for ${m.name}`, submit: 'Share', onPin: (pin) => share(q, m, pin) })));
  }, $err);

  const showShare = (q) => {
    const ineligible = new Set(q.ineligible_member_ids || []);
    const reasonText = `${questRangeText(q)} only`;
    const renderCandidate = (m) => {
      if (ineligible.has(m.id)) {
        return el('span', { class: 'inline-flex items-center gap-1' },
          el('button', { class: 'btn btn-sm', text: m.name, disabled: true }),
          el('small', { class: 'text-xs opacity-70', text: reasonText }));
      }
      return el('button', { class: 'btn btn-sm', text: m.name, onclick: pickTarget(q, m) });
    };

    return $share.replaceChildren(el('div', { class: 'p-3 bg-base-200 rounded space-y-2' },
      el('div', { class: 'font-semibold', text: `Share ${q.name} with` }),
      el('div', { class: 'flex flex-wrap gap-2' },
        shareCandidates(q).map(renderCandidate)),
      el('button', { class: 'btn btn-ghost btn-sm', text: 'Cancel', onclick: (e) => { e.preventDefault(); $share.replaceChildren(); } })));
  };

  // Share is offered only on a shareable quest the active member holds, while
  // anyone is left to share with. Out-of-range candidates count: the picker
  // shows them disabled with the reason, even when none is in range.
  const shareButton = (q) => {
    if (!q.shareable || q.claimed_by !== member.id) return null;
    if (shareCandidates(q).length === 0) return null;
    return el('button', { class: 'btn btn-ghost btn-xs', text: 'Share', onclick: (e) => { e.preventDefault(); showShare(q); } });
  };

  const sharedWith = (q) => {
    if (!q.shared) return null;
    const others = (q.participants || []).filter((id) => id !== member.id).map((id) => nameOf[id] || 'a former member');
    return [
      el('span', { class: 'badge badge-sm', text: 'Shared' }),
      el('span', { class: 'text-xs opacity-70', text: `Shared with ${joinNames(others)}` }),
    ];
  };

  const section = (title, items, render, empty) => el('div', { class: 'space-y-1' },
    el('h4', { class: 'font-semibold text-sm opacity-80', text: title }),
    items.length ? el('ul', {}, items.map(render)) : el('p', { class: 'text-xs opacity-60', text: empty }));

  return card(null,
    el('h3', { class: 'font-semibold', text: `Quests for ${member.name}` }),
    $err,
    section('Available', parts.available,
      (q) => row(q,
        waitingBadge(q),
        el('button', { class: 'btn btn-sm', text: 'View', onclick: (e) => { e.preventDefault(); showDetail(q); } })),
      'No quests available right now.'),
    // A quest the server's age check leaves out for this member is named
    // under Available, so the member knows why it is missing.
    parts.leftOut.map((q) => el('p', { class: 'text-xs opacity-70', text: `${q.name} (${lowerFirst(questRangeText(q))}) is not listed for ${member.name}.` })),
    $detail,
    section('Mine', parts.mine, (q) => row(q,
      sharedWith(q),
      waitingBadge(q),
      shareButton(q),
      el('button', { class: 'btn btn-primary btn-xs', text: 'Mark done', onclick: act('done', q) }),
      el('button', { class: 'btn btn-ghost btn-xs', text: 'Abandon', onclick: act('abandon', q) }),
    ), 'You have no quests yet.'),
    $share,
    section('Claimed', parts.claimed,
      (q) => row(q, el('span', { class: 'text-xs opacity-70', text: `Taken by ${nameOf[q.claimed_by] || 'a former member'}` }),
        waitingBadge(q)),
      'Nobody else has a quest.'),
    parts.resting.length > 0 && section('Coming back later', parts.resting,
      (q) => row(q,
        q.available_at && el('span', { class: 'text-xs opacity-70', text: `Back ${fmtDate(q.available_at)}` }),
        waitingBadge(q)), ''),
    el('div', { class: 'space-y-1' },
      el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Points history' }),
      historyError ? alertBox('error', historyError) : pointsHistoryTable(history, 'No points yet. Points you earn will show up here.', { adminLabel: '(from admin)' }),
    ),
    el('div', { class: 'space-y-1' },
      el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Reward history' }),
      rewardHistoryError ? alertBox('error', rewardHistoryError) : rewardHistoryTable(rewardHistory),
    ));
}

// rewardHistoryTable renders a member's approved reward redemptions, newest
// first: the Date/Reward/Points table, the cost as the plain number it took,
// or 'No rewards redeemed yet' when there are none. The points history shows
// the same redemption as its own signed '-N Redeemed: …' row.
function rewardHistoryTable(items) {
  if (!items || items.length === 0) {
    return el('p', { class: 'text-xs opacity-60', text: 'No rewards redeemed yet' });
  }
  return el('table', { class: 'table table-sm' },
    el('thead', {}, el('tr', {},
      el('th', { text: 'Date' }),
      el('th', { text: 'Reward' }),
      el('th', { text: 'Points' }),
    )),
    el('tbody', {}, items.map((item) => el('tr', {},
      el('td', { text: fmtDate(item.decided_at) }),
      el('td', { text: item.description }),
      el('td', { text: String(item.cost) }),
    ))),
  );
}

// pointsHistoryTable renders the member's or admin's view of points awards:
// the Date/Points/Reason table, or an empty paragraph when there are none.
// Points are signed ('+N', or '-N' for an admin removal). opts.adminLabel is
// appended to the reason of each admin adjustment (kind 'admin'): the Family
// tab says '(admin adjustment)', the member's own card '(from admin)'.
function pointsHistoryTable(history, emptyText, opts = {}) {
  if (!history || history.length === 0) {
    return el('p', { class: 'text-xs opacity-60', text: emptyText });
  }
  const reasonOf = (item) => (item.kind === 'admin' && opts.adminLabel ? `${item.reason} ${opts.adminLabel}` : item.reason);
  return el('table', { class: 'table table-sm' },
    el('thead', {}, el('tr', {},
      el('th', { text: 'Date' }),
      el('th', { text: 'Points' }),
      el('th', { text: 'Reason' }),
    )),
    el('tbody', {}, history.map((item) => el('tr', {},
      el('td', { text: fmtDate(item.earned_at) }),
      el('td', { text: signedPoints(item.points) }),
      el('td', { text: reasonOf(item) }),
    ))),
  );
}

// signedPoints shows a points amount with its sign: '+5', '+0', '-5'.
function signedPoints(n) {
  return n < 0 ? String(n) : `+${n}`;
}

// approvalNotice is the plain-text notice after an approval that paid
// bonuses: every combo in the approve response's combo_bonuses with its bonus,
// and the streak bonus ('Streak bonus +N (streak n)') when streak_bonus > 0.
// It names the chore; null when no bonus was paid (or for a send-back).
function approvalNotice(q, res) {
  const bonuses = (res && res.combo_bonuses) || [];
  const paid = bonuses.map((b) => `combo bonus '${b.name}' +${b.bonus_points}`);
  if (res && res.streak_bonus > 0) paid.push(`Streak bonus +${res.streak_bonus} (streak ${res.streak_count})`);
  if (paid.length === 0) return null;
  const list = paid.join(', ');
  const who = pendingMembers(q);
  if (who.length < 2) return `Approved '${q.name}' for ${who[0].member_name}: +${who[0].points} points, ${list}`;
  const bonus = q.teamwork_bonus || 0;
  return `Approved '${q.name}' for ${joinNames(who.map((m) => m.member_name))}: +${who[0].points} points and +${bonus} teamwork bonus each, ${list}`;
}

// approvalsCard is the administrator's list of quests marked done and pending
// reward redemptions. Loading, approving, sending back and rejecting carry
// X-Admin-PIN (adminRequest). A 403 for the admin PIN says so, leaves the list
// as it was and never signs anyone out.
async function approvalsCard(h, members, rerender) {
  const $err = el('div');
  const unlockForm = () => pinForm({
    label: 'Admin PIN',
    submit: 'Unlock',
    onPin: async (pin) => {
      await api('POST', `/households/${h.id}/admin-pin/verify`, { pin });
      adminPin = pin;
      rerender();
    },
  });
  const wrongAdminPin = () => $err.replaceChildren(alertBox('error', 'Wrong administrator PIN.'), unlockForm());
  const points = el('div', { class: 'flex flex-wrap gap-2' },
    members.map((m) => el('span', { class: 'badge', text: `${m.name}: ${m.points} points` })));
  const wrap = (...body) => card(null, el('h3', { class: 'font-semibold', text: 'Approvals' }), $err, ...body,
    members.length > 0 && el('div', { class: 'divider my-0' }), members.length > 0 && points);

  if (!managementUnlocked()) {
    return wrap(el('p', { class: 'text-sm opacity-70', text: 'Enter the admin PIN to review approvals.' }), unlockForm());
  }
  // The two lists load independently: a failed reward-requests read is shown
  // in place of the reward rows and never hides the quest completions.
  const [qRead, rRead] = await Promise.allSettled([
    adminRequest('GET', `/households/${h.id}/quests/pending`),
    adminRequest('GET', `/households/${h.id}/reward-redemptions/pending`),
  ]);
  if ([qRead, rRead].some((s) => s.status === 'rejected' && isAdminPinError(s.reason))) {
    wrongAdminPin();
    return wrap();
  }
  if (qRead.status === 'rejected') throw qRead.reason;
  const pending = qRead.value.items || [];
  let pendingRewards = [];
  let rewardsError = null;
  if (rRead.status === 'fulfilled') {
    pendingRewards = rRead.value.items || [];
  } else {
    console.error(rRead.reason);
    rewardsError = alertBox('error', `Could not load reward requests: ${rRead.reason?.message || rRead.reason}`);
  }

  const act = (verb, q) => action(async () => {
    let res;
    try {
      const cid = q.completion_id;
      res = await adminRequest('POST', `/households/${h.id}/quest-completions/${cid}/${verb}`, {});
    } catch (err) {
      if (isAdminPinError(err)) { wrongAdminPin(); return; }
      const msg = questNotice(err);
      if (msg) { rerender(msg); return; }
      throw err;
    }
    rerender(approvalNotice(q, res));
  }, $err);

  const actReward = (verb, r) => action(async () => {
    try {
      await adminRequest('POST', `/households/${h.id}/reward-redemptions/${r.id}/${verb}`, {});
    } catch (err) {
      if (isAdminPinError(err)) { wrongAdminPin(); return; }
      if (err instanceof ApiError && err.status === 422) {
        // The server's reason (e.g. "... Nothing was taken.") is on points.
        $err.replaceChildren(alertBox('error', err.fields?.points || err.message));
        return;
      }
      // Already decided (here or in another session), cancelled when the
      // member left, or gone: reload the list so the stale row leaves, and
      // say why, as the quest path does.
      if (err instanceof ApiError && err.status === 404) {
        rerender('That reward request is no longer waiting for approval.');
        return;
      }
      throw err;
    }
    rerender();
  }, $err);

  // A shared completion names everyone on it and what each will receive:
  // the split points plus the teamwork bonus. A single member's keeps its
  // one line with the full points.
  const whoLines = (q) => {
    const who = pendingMembers(q);
    if (who.length < 2) {
      return [el('div', { class: 'text-xs opacity-60', text: `${who[0].member_name} · ${who[0].points} points` })];
    }
    const bonus = q.teamwork_bonus || 0;
    return [
      el('div', { class: 'text-xs opacity-60', text: `shared by ${joinNames(who.map((m) => m.member_name))}` }),
      ...who.map((m) => el('div', { class: 'text-xs', text: `${m.member_name} gets ${m.points} + ${bonus} bonus = ${m.points + bonus}` })),
    ];
  };

  const rows = pending.map((q) => el('li', { class: 'flex flex-wrap items-center gap-2 py-2 border-b border-base-300' },
    el('div', { class: 'flex-1 min-w-0' },
      el('div', { class: 'font-medium', text: q.name }),
      whoLines(q),
      q.done_at && el('div', { class: 'text-xs opacity-60', text: fmtDate(q.done_at) })),
    el('button', { class: 'btn btn-success btn-xs', text: 'Approve', onclick: act('approve', q) }),
    el('button', { class: 'btn btn-ghost btn-xs', text: 'Send back', onclick: act('reject', q) })));

  const rewardRows = pendingRewards.map((r) => el('li', { class: 'flex flex-wrap items-center gap-2 py-2 border-b border-base-300' },
    el('div', { class: 'flex-1 min-w-0' },
      el('div', { class: 'font-medium', text: r.member_name }),
      el('div', { class: 'text-xs opacity-60', text: `Reward: ${r.description}` }),
      el('div', { class: 'text-xs', text: signedPoints(-r.cost) })),
    el('button', { class: 'btn btn-success btn-xs', text: 'Approve', onclick: actReward('approve', r) }),
    el('button', { class: 'btn btn-ghost btn-xs', text: 'Reject', onclick: actReward('reject', r) })));

  const hasItems = rows.length > 0 || rewardRows.length > 0;
  if (!hasItems && !rewardsError) {
    return wrap(el('p', { class: 'text-sm opacity-70', text: 'Nothing waiting for approval.' }));
  }

  const sections = [];
  if (rows.length > 0) sections.push(el('ul', {}, rows));
  if (rewardRows.length > 0 || rewardsError) {
    sections.push(el('div', { class: 'space-y-1' },
      el('h4', { class: 'font-semibold text-sm opacity-80 mt-2', text: 'Reward requests' }),
      rewardsError || el('ul', {}, rewardRows)));
  }

  return wrap(...sections);
}

// manageQuestsCard lists every quest with the admin's add, edit, delete and
// library controls (shown while management is unlocked).
function manageQuestsCard(h, quests, library, page, rerender) {
  const unlocked = managementUnlocked();

  // A blank field is sent as typed, never as 0: the server owns validation
  // and reports an error on the field. The streak bonus is optional, so a
  // blank one is left out of the body: a new quest then saves 0 and an edit
  // keeps the quest's stored bonus (never zeroes it).
  const toNum = (val) => {
    if (typeof val === 'string' && val.trim() === '') return val;
    const n = Number(val);
    return Number.isNaN(n) ? val : n;
  };
  const isBlank = (val) => val === undefined || val === null || (typeof val === 'string' && val.trim() === '');

  // teamwork_bonus is sent only while Shareable is ticked; unticked, it is 0,
  // so a hidden bonus is never stored on a quest that cannot be shared.
  // min_age and max_age are null under 'All ages', and a blank one is null
  // (no limit on that side), never 0 or ''.
  const toAge = (v, val) => (v.ages !== 'range' || isBlank(val) ? null : toNum(val));
  const toBody = (v) => ({
    name: v.name,
    points: toNum(v.points),
    repeat_days: toNum(v.repeat_days),
    ...(isBlank(v.streak_bonus) ? {} : { streak_bonus: toNum(v.streak_bonus) }),
    notes: v.notes,
    shareable: !!v.shareable,
    teamwork_bonus: v.shareable ? toNum(v.teamwork_bonus ?? '') : 0,
    min_age: toAge(v, v.min_age),
    max_age: toAge(v, v.max_age),
  });

  const hasRange = (q) => (q.min_age ?? null) !== null || (q.max_age ?? null) !== null;
  const inRange = (v) => v.ages === 'range';
  const questFields = (q = {}) => [
    { name: 'name', label: 'Name', required: true, value: q.name },
    { name: 'points', label: 'Points', type: 'number', value: q.points ?? '' },
    { name: 'repeat_days', label: 'Repeats after (days, back at midnight)', type: 'number', value: q.repeat_days ?? '' },
    { name: 'streak_bonus', label: 'Streak bonus', type: 'number', value: q.streak_bonus ?? 0 },
    { name: 'notes', label: 'Notes', type: 'textarea', value: q.notes ?? '' },
    { name: 'ages', group: 'Ages', type: 'radio', options: [['all', 'All ages'], ['range', 'Age range']], value: hasRange(q) ? 'range' : 'all' },
    { name: 'min_age', group: 'Ages', label: 'Minimum age', type: 'number', value: q.min_age ?? '', show: inRange },
    { name: 'max_age', group: 'Ages', label: 'Maximum age', type: 'number', value: q.max_age ?? '', show: inRange },
    { name: 'ages_help', group: 'Ages', type: 'note', text: 'Leave one empty for no limit on that side.', show: inRange },
    { name: 'shareable', label: 'Shareable', type: 'checkbox', value: !!q.shareable },
    { name: 'teamwork_bonus', label: 'Teamwork bonus', type: 'number', value: q.teamwork_bonus ?? 0, show: (v) => !!v.shareable },
  ];

  // questSummary is a quest's or library quest's points, days and streak bonus.
  const questSummary = (q) => `${q.points} points · ${q.repeat_days} day(s) · streak bonus ${q.streak_bonus ?? 0}`;

  const rows = quests.map((q) => {

    const $row = el('li', { class: 'flex flex-wrap items-center gap-2 py-2 border-b border-base-300' },
      el('div', { class: 'flex-1 min-w-0' },
        el('div', { class: 'font-medium', text: q.name }),
        el('div', { class: 'text-xs opacity-60', text: questSummary(q) }),
        el('div', { class: 'text-xs opacity-60', text: `${q.points} points · ${q.repeat_days} day(s)` }),
        el('div', { class: 'text-xs' }, el('strong', { text: questRangeText(q) })),
        el('div', { class: 'text-xs opacity-60', text: `Shareable ${q.shareable ? 'Yes' : 'No'} · Teamwork bonus ${q.shareable ? q.teamwork_bonus : '-'}` }),
        q.notes ? el('div', { class: 'text-xs opacity-70 mt-1', text: q.notes }) : null),
      unlocked && el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Edit',
        onclick: () => $row.replaceWith(el('li', { class: 'py-2' }, form({
          fields: questFields(q), submit: 'Save', inline: true,
          onSubmit: async (v) => {
            await adminRequest('PUT', `/households/${h.id}/quests/${q.id}`, toBody(v));
            rerender();
          },
        }))),
      }),
      unlocked && el('button', {
        class: 'btn btn-ghost btn-xs text-error', text: 'Delete',
        onclick: confirmClick('Sure?', async () => {
          await adminRequest('DELETE', `/households/${h.id}/quests/${q.id}`);
          rerender();
        }),
      }));
    return $row;
  });

  const addForm = unlocked && form({
    fields: questFields(), submit: 'Add quest', inline: true,
    onSubmit: async (v) => {
      await adminRequest('POST', `/households/${h.id}/quests`, toBody(v));
      rerender();
    },
  });

  const libraryRows = (library.items || []).map((item) => el('li', { class: 'flex items-center justify-between py-1.5 border-b border-base-300 text-sm' },
    el('div', { class: 'flex-1 min-w-0' },
      el('span', { class: 'font-medium', text: item.name }),
      el('span', { class: 'text-xs opacity-60 ml-2', text: questSummary(item) }),
      item.notes ? el('div', { class: 'text-xs opacity-70', text: item.notes }) : null),
    unlocked && el('button', {
      class: 'btn btn-ghost btn-xs', text: 'Add',
      onclick: action(async () => {
        await adminRequest('POST', `/households/${h.id}/quests/from-library`, { library_id: item.id });
        rerender();
      }),
    })));

  return card(null,
    el('h3', { class: 'font-semibold', text: 'Manage quests' }),
    rows.length
      ? el('ul', {}, rows)
      : el('p', { class: 'opacity-70 py-2', text: 'No quests yet. Add one below or browse the library.' }),
    page.next_offset !== null && page.next_offset !== undefined && el('p', { class: 'text-xs opacity-60', text: 'Showing the first 100.' }),
    addForm && el('div', { class: 'divider my-0' }),
    addForm,
    el('div', { class: 'divider my-0' }),
    el('div', { class: 'space-y-2' },
      el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Quest Library' }),
      el('ul', {}, libraryRows)));
}

// manageCombosCard lists every combo (name, chores, bonus) with the admin's
// add, edit and delete controls, shown while management is unlocked. Writes
// carry the admin PIN only in X-Admin-PIN (adminRequest); a refused save keeps
// the form as typed and shows each field's error under it. quests is every
// household quest (all pages); if the combos read failed, the card shows only
// the error.
function manageCombosCard(h, { combos, quests, error }, rerender) {
  if (error) {
    return card(null,
      el('h3', { class: 'font-semibold', text: 'Manage combos' }),
      alertBox('error', error));
  }
  const unlocked = managementUnlocked();
  const base = `/households/${h.id}/combos`;
  const questMap = Object.fromEntries(quests.map((q) => [q.id, q.name]));

  // A blank bonus is sent as typed, never as 0: the server validates it.
  const toNum = (val) => {
    if (String(val).trim() === '') return val;
    const n = Number(val);
    return Number.isNaN(n) ? val : n;
  };
  const toBody = (v) => ({ name: v.name, bonus_points: toNum(v.bonus_points), quest_ids: v.quest_ids });
  const comboFields = (c = {}) => [
    { name: 'name', label: 'Name', value: c.name },
    { name: 'quest_ids', label: 'Quests', type: 'multiselect', value: c.quest_ids || [], options: quests.map((q) => [q.id, q.name]) },
    { name: 'bonus_points', label: 'Bonus points', type: 'number', value: c.bonus_points ?? '' },
  ];

  const rows = combos.map((c) => {
    const choreNames = (c.quest_ids || []).map((id) => questMap[id] || id).join(', ');
    const $row = el('tr', {},
      el('td', { class: 'font-medium', text: c.name }),
      el('td', { text: choreNames }),
      el('td', { text: `+${c.bonus_points}` }),
      el('td', {},
        unlocked && el('button', {
          class: 'btn btn-ghost btn-xs', text: 'Edit',
          onclick: () => $row.replaceWith(el('tr', {}, el('td', { colspan: '4' }, form({
            fields: comboFields(c), submit: 'Save', inline: true,
            onSubmit: async (v) => {
              // An id the picker had no option for was never deselectable: keep it.
              const unseen = (c.quest_ids || []).filter((id) => !Object.hasOwn(questMap, id) && !v.quest_ids.includes(id));
              await adminRequest('PUT', `${base}/${c.id}`, toBody({ ...v, quest_ids: [...v.quest_ids, ...unseen] }));
              rerender();
            },
          })))),
        }),
        unlocked && el('button', {
          class: 'btn btn-ghost btn-xs text-error', text: 'Delete',
          onclick: confirmClick('Sure?', async () => { await adminRequest('DELETE', `${base}/${c.id}`); rerender(); }),
        })));
    return $row;
  });

  const list = combos.length
    ? el('table', { class: 'table w-full' },
      el('thead', {},
        el('tr', {},
          el('th', { text: 'Combo' }),
          el('th', { text: 'Quests' }),
          el('th', { text: 'Bonus' }),
          el('th', {}))),
      el('tbody', {}, rows))
    : el('div', { class: 'text-sm' },
      el('p', { class: 'font-medium', text: 'No combos yet' }),
      el('p', { class: 'opacity-70', text: 'Create one to reward chores done together on the same day.' }));

  const addForm = unlocked && form({
    fields: comboFields(), submit: 'Create combo', inline: true,
    onSubmit: async (v) => { await adminRequest('POST', base, toBody(v)); rerender(); },
  });

  return card(null,
    el('h3', { class: 'font-semibold', text: 'Manage combos' }),
    list,
    addForm);
}

// ── Rewards ───────────────────────────────────────────────────────────────────

// rewardCost turns the cost input into the request value without judging it: a
// numeric string is sent as a number, anything else (blank, "abc") as typed, so
// the server decides what is invalid and reports it on the cost field.
function rewardCost(val) {
  if (String(val).trim() === '') return val;
  const n = Number(val);
  return Number.isNaN(n) ? val : n;
}

// rewardsTab lists the household's rewards. In the member view, members can
// redeem rewards if they have enough points, creating a request waiting for
// admin approval. In the administrator view, rewards are managed (add, edit,
// delete, copy from library) while management is unlocked with the admin PIN.
async function rewardsTab(h, $root, notice = null) {
  const rerender = (newNotice = null) => { $root.replaceChildren(); rewardsTab(h, $root, newNotice).catch((err) => showError($root, err)); };
  const visible = managementVisible();
  const unlocked = visible && managementUnlocked();
  const isMember = Boolean(activeMember);
  const base = `/households/${h.id}/rewards`;

  const [page, library, membersRes, pendingRes] = await Promise.all([
    api('GET', base),
    visible ? api('GET', '/reward-library') : Promise.resolve({ items: [] }),
    isMember ? api('GET', `/households/${h.id}/family-members`) : Promise.resolve({ items: [] }),
    isMember ? api('GET', `/households/${h.id}/family-members/${activeMember.id}/reward-redemptions/pending`, undefined, {
      headers: { 'X-Member-PIN': activeMember.pin ?? '' },
    }).catch((err) => {
      if (memberActionFailed(err)) return null;
      // Unknown pending state: say so, and offer no Redeem (one may be waiting).
      console.error(err);
      return { failed: err };
    }) : Promise.resolve({ items: [] }),
  ]);
  if (isMember && !activeMember) return;
  const rewards = page.items || [];
  const memberList = membersRes?.items || membersRes || [];
  const currentMember = isMember ? (memberList.find((m) => m.id === activeMember.id) || activeMember) : null;
  const memberPoints = currentMember ? (currentMember.points ?? 0) : 0;
  const pendingUnknown = Boolean(pendingRes?.failed);
  const pendingItems = pendingRes?.items || [];
  const pendingRewardIds = new Set(pendingItems.map((p) => p.reward_id));

  const costText = (r) => `${r.cost} points`;
  const toBody = (v) => ({ description: v.description, cost: rewardCost(v.cost) });
  const rewardFields = (r = {}) => [
    { name: 'description', label: 'Description', required: true, value: r.description },
    { name: 'cost', label: 'Cost (points)', type: 'number', value: r.cost ?? '' },
  ];

  const $notice = el('div');
  if (notice) $notice.replaceChildren(alertBox('info', notice));
  const $pendingErr = pendingUnknown
    ? alertBox('error', `Could not load your waiting requests, so Redeem is off for now: ${pendingRes.failed?.message || pendingRes.failed}`)
    : null;

  const rows = rewards.map((r) => {
    const $rowErr = el('div');
    const $actionArea = el('div', { class: 'flex items-center gap-2' });

    const renderActionArea = () => {
      $actionArea.replaceChildren();
      if (pendingRewardIds.has(r.id)) {
        $actionArea.append(el('span', { class: 'badge badge-sm badge-warning', text: 'Pending approval' }));
        return;
      }
      const enough = memberPoints >= r.cost;
      if (pendingUnknown) {
        $actionArea.append(el('button', { class: 'btn btn-sm', disabled: true, text: 'Redeem' }));
      } else if (enough) {
        const $btn = el('button', {
          class: 'btn btn-sm btn-primary',
          text: 'Redeem',
          onclick: async () => {
            $rowErr.replaceChildren();
            $btn.disabled = true;
            try {
              const body = activeMemberAction();
              await api('POST', `${base}/${r.id}/redeem`, body);
              pendingRewardIds.add(r.id);
              renderActionArea();
              $notice.replaceChildren(alertBox('info', `${r.description} requested — waiting for approval. Your points stay at ${memberPoints} until then.`));
            } catch (err) {
              $btn.disabled = false;
              if (memberActionFailed(err)) return;
              if (err instanceof ApiError && err.status === 422) {
                const msg = err.fields?.points || err.message;
                $rowErr.replaceChildren(el('div', { class: 'text-error text-xs mt-1', text: msg }));
                return;
              }
              // Gone (deleted by the admin): reload the list and say why.
              if (err instanceof ApiError && err.status === 404) {
                rerender(`${r.description} is no longer available.`);
                return;
              }
              console.error(err);
              $rowErr.replaceChildren(el('div', { class: 'text-error text-xs mt-1', text: `Could not request ${r.description}: ${err.message || err}` }));
            }
          },
        });
        $actionArea.append($btn);
      } else {
        $actionArea.append(
          el('button', { class: 'btn btn-sm', disabled: true, text: 'Redeem' }),
          el('span', { class: 'text-xs opacity-70', text: `Need ${r.cost - memberPoints} more points` }),
        );
      }
    };

    if (isMember) {
      renderActionArea();
    }

    const adminButtons = [
      unlocked && el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Edit',
        onclick: () => $row.replaceWith(el('li', { class: 'py-2' }, form({
          fields: rewardFields(r), submit: 'Save', inline: true,
          onSubmit: async (v) => { await adminRequest('PUT', `${base}/${r.id}`, toBody(v)); rerender(); },
        }))),
      }),
      unlocked && el('button', {
        class: 'btn btn-ghost btn-xs text-error', text: 'Delete',
        onclick: confirmClick('Sure?', async () => { await adminRequest('DELETE', `${base}/${r.id}`); rerender(); }),
      }),
    ];

    const $row = el('li', { class: 'py-2 border-b border-base-300' },
      el('div', { class: 'flex flex-wrap items-center gap-2' },
        el('div', { class: 'flex-1 min-w-0' },
          el('div', { class: 'font-medium', text: r.description }),
          el('div', { class: 'text-xs opacity-60', text: costText(r) })),
        isMember ? $actionArea : adminButtons,
      ),
      $rowErr,
    );
    return $row;
  });

  const addForm = unlocked && form({
    fields: rewardFields(), submit: 'Add reward', inline: true,
    onSubmit: async (v) => { await adminRequest('POST', base, toBody(v)); rerender(); },
  });

  // In the administrator's view with the admin PIN set but not entered, the
  // list is read-only until the PIN unlocks management.
  const unlockForm = visible && !unlocked && pinForm({
    label: 'Admin PIN',
    submit: 'Unlock',
    onPin: async (pin) => {
      await api('POST', `/households/${h.id}/admin-pin/verify`, { pin });
      adminPin = pin;
      rerender();
    },
  });

  const libraryRows = (library.items || []).map((item) => el('li', { class: 'flex items-center justify-between py-1.5 border-b border-base-300 text-sm' },
    el('div', { class: 'flex-1 min-w-0' },
      el('span', { class: 'font-medium', text: item.description }),
      el('span', { class: 'text-xs opacity-60 ml-2', text: costText(item) })),
    unlocked && el('button', {
      class: 'btn btn-ghost btn-xs', text: 'Add',
      onclick: action(async () => {
        await adminRequest('POST', `${base}/from-library`, { library_id: item.id });
        rerender();
      }),
    })));

  $root.replaceChildren(card(null,
    el('h3', { class: 'font-semibold', text: 'Rewards' }),
    $notice,
    $pendingErr,
    rows.length
      ? el('ul', {}, rows)
      : el('p', { class: 'opacity-70 py-2', text: 'No rewards yet. Add one below or browse the library for ideas.' }),
    unlockForm && el('p', { class: 'text-sm opacity-70', text: 'Enter the admin PIN to manage rewards.' }),
    unlockForm,
    addForm && el('div', { class: 'divider my-0' }),
    addForm,
    visible && el('div', { class: 'divider my-0' }),
    visible && el('div', { class: 'space-y-2' },
      el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Reward Library' }),
      el('ul', {}, libraryRows))));
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

  familyView = null; // any member view still loading is now stale
  const base = `/households/${h.id}/family-members`;
  const members = (await api('GET', base)).items;
  // Birthdays come only from the admin-PIN-gated read, here on the Family
  // tab; no other view asks for them. The age shown is the server's. A
  // refused admin PIN locks management again (adminRequest forgot it), so
  // the tab re-renders to its unlock form; any other failure is the birthday
  // controls' alone and the rest of the tab still renders.
  let birthdays = {};
  let birthdaysError = null;
  try {
    birthdays = Object.fromEntries(((await adminRequest('GET', `${base}/birthdays`)).items || [])
      .map((b) => [b.member_id, b]));
  } catch (err) {
    if (isAdminPinError(err)) { adminPinState.set = true; rerender(); return; }
    console.error('birthdays', err);
    birthdaysError = 'Could not load birthdays. Reload to try again.';
  }

  // birthdayRow is one member's birthday controls: the date input, the
  // server's age, Save birthday and (when one is set) Clear. A save or clear
  // re-renders the row from the reply; a refusal shows its message on the
  // row and leaves the shown birthday and age as they were.
  const birthdayRow = (m, b) => {
    if (birthdaysError) return el('p', { class: 'text-error text-xs w-full', text: birthdaysError });
    const $err = el('div', { class: 'text-error text-xs w-full' });
    const $date = el('input', { type: 'date', name: 'birthday', class: 'input input-bordered input-sm', value: b?.birthday ?? '' });
    const send = async (birthday) => {
      $err.textContent = '';
      let res;
      try {
        res = await adminRequest('PUT', `${base}/${m.id}/birthday`, { birthday });
      } catch (err) {
        if (!(err instanceof ApiError)) throw err;
        $err.textContent = err.code === 'wrong_pin' ? 'Wrong administrator PIN.' : (err.fields.birthday || err.message);
        $date.value = b?.birthday ?? '';
        return;
      }
      $row.replaceWith(birthdayRow(m, res));
    };
    const $row = el('div', { class: 'flex flex-wrap items-center gap-2 w-full text-sm' },
      el('span', { text: 'Birthday' }),
      $date,
      b?.birthday && b.age !== null && b.age !== undefined && el('span', { text: `(age ${b.age})` }),
      !b?.birthday && el('small', { class: 'opacity-70', text: `Not set: ${m.name} can do every quest` }),
      el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Save birthday',
        onclick: action(async () => {
          if ($date.value === '') return;
          await send($date.value);
        }, $err),
      }),
      b?.birthday && el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Clear',
        onclick: confirmClick('Sure?', () => send(null)),
      }),
      $err);
    return $row;
  };

  // showMember swaps the family list for one member's view: heading, Back,
  // the slot for the adjust-points form, then the points history and the
  // reward history, each read with the admin's headers. A late answer for a
  // view the admin already left is dropped; a failed read shows an inline
  // error inside its own section; a refused admin PIN locks the tab again.
  const showMember = async (m) => {
    const view = {};
    familyView = view;
    const heading = () => el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Points history' });
    const $history = el('div', { class: 'space-y-1' },
      heading(), el('p', { class: 'text-xs opacity-60', text: 'Loading points history…' }));
    const rewardHeading = () => el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Reward history' });
    const $rewards = el('div', { class: 'space-y-1' },
      rewardHeading(), el('p', { class: 'text-xs opacity-60', text: 'Loading reward history…' }));
    $root.replaceChildren(card(null,
      el('h3', { class: 'font-semibold', text: `${m.name} — ${m.points} points` }),
      el('div', {}, el('button', { class: 'btn btn-ghost btn-xs', text: 'Back to family', onclick: () => rerender() })),
      adjustPointsForm(m, view),
      $history,
      $rewards));
    // Each read settles to its content, or null when the view is gone or the
    // admin PIN was refused (the tab re-renders locked).
    const load = async (path, label, render) => {
      try {
        return render((await adminRequest('GET', `${base}/${m.id}/${path}`)).items);
      } catch (err) {
        if (familyView !== view) return null;
        if (err instanceof ApiError && (err.code === 'wrong_pin' || err.code === 'admin_pin_required')) {
          familyView = null;
          rerender(); // adminRequest forgot the PIN: the tab locks again
          return null;
        }
        console.error(label, err);
        return alertBox('error', `Could not load ${label}. Reload to try again.`);
      }
    };
    const [content, rewardContent] = await Promise.all([
      load('points-history', 'points history', (items) => pointsHistoryTable(items, 'No points history yet.', { adminLabel: '(admin adjustment)' })),
      load('reward-history', 'reward history', (items) => rewardHistoryTable(items)),
    ]);
    if (familyView !== view || !content || !rewardContent) return;
    $history.replaceChildren(heading(), content);
    $rewards.replaceChildren(rewardHeading(), rewardContent);
  };

  // adjustPointsForm is the member view's 'Adjust points' fieldset: an amount,
  // Add (default) or Remove, an optional reason. Save POSTs the signed amount;
  // on 200 the view reopens with the member the API returned (new total, new
  // history). A field error (422) shows as an inline alert between the
  // direction and the reason, keeping what was entered.
  const adjustPointsForm = (m, view) => {
    const $points = el('input', { type: 'number', name: 'points', min: '1', step: '1', class: 'input input-bordered input-sm w-24' });
    const $add = el('input', { type: 'radio', name: 'direction', value: 'add', class: 'radio radio-sm', checked: true });
    const $remove = el('input', { type: 'radio', name: 'direction', value: 'remove', class: 'radio radio-sm' });
    const $reason = el('input', { type: 'text', name: 'reason', maxlength: '200', class: 'input input-bordered input-sm w-full' });
    const $alert = el('div');
    const $save = el('button', { type: 'submit', class: 'btn btn-primary btn-sm', text: 'Save' });
    const showAlert = (message) => $alert.replaceChildren(el('p', { role: 'alert', class: 'text-error text-xs', text: message }));
    return el('form', {
      class: 'space-y-2',
      onsubmit: async (e) => {
        e.preventDefault();
        $alert.replaceChildren();
        const raw = String($points.value).trim();
        if (!/^\d+$/.test(raw) || Number(raw) === 0) {
          showAlert('Points must be a whole number above 0.');
          return;
        }
        const amount = Number(raw);
        const body = { points: $remove.checked ? -amount : amount, reason: $reason.value };
        $save.disabled = true;
        try {
          const updated = await adminRequest('POST', `${base}/${m.id}/points-adjustments`, body);
          // The member's own card caches its history once per pick: drop it,
          // and re-read the member bar's total, so the card shows this change.
          if (activeMember?.id === m.id) {
            memberHistory = null;
            memberBarPoints?.();
          }
          if (familyView !== view) return;
          await showMember(updated);
        } catch (err) {
          if (familyView !== view) return;
          if (err instanceof ApiError && (err.code === 'wrong_pin' || err.code === 'admin_pin_required')) {
            rerender(); // adminRequest forgot the PIN: the tab locks again
            return;
          }
          if (!(err instanceof ApiError)) console.error('adjust points', err);
          const fields = err instanceof ApiError ? Object.values(err.fields) : [];
          showAlert(fields.length ? fields.join(' ') : (err.message || String(err)));
        } finally {
          $save.disabled = false;
        }
      },
    }, el('fieldset', { class: 'space-y-2' },
      el('legend', { class: 'font-semibold text-sm', text: 'Adjust points' }),
      el('div', { class: 'flex flex-wrap items-center gap-3' },
        el('label', { class: 'flex items-center gap-2' }, el('span', { class: 'text-sm', text: 'Points' }), $points),
        el('label', { class: 'flex items-center gap-1' }, $add, el('span', { class: 'text-sm', text: 'Add' })),
        el('label', { class: 'flex items-center gap-1' }, $remove, el('span', { class: 'text-sm', text: 'Remove' }))),
      $alert,
      el('label', { class: 'block' }, el('span', { class: 'text-sm', text: 'Reason' }), $reason),
      $save));
  };

  const rows = members.map((m) => {
    const $row = el('li', { class: 'flex flex-wrap items-center gap-2 py-2 border-b border-base-300' },
      el('div', { class: 'flex-1 min-w-0' },
        el('div', { text: m.name }),
        el('div', { class: 'text-xs opacity-60', text: `${m.points} points` })),
      el('span', { class: 'badge badge-sm', text: m.has_pin ? 'PIN' : 'No PIN' }),
      el('button', {
        class: 'btn btn-ghost btn-xs', text: 'History',
        onclick: () => showMember(m).catch((err) => showError($root, err)),
      }),
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
      }),
      birthdayRow(m, birthdays[m.id]));
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
      rows.length > 0 && el('p', {}, el('small', { class: 'opacity-70', text: 'Birthdays are only shown here, behind your admin PIN.' })),
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

// The calendar scopes the server asks Google for; it derives read-only or
// read/write from what Google actually grants.
const CALENDAR_SCOPES = 'https://www.googleapis.com/auth/calendar.calendarlist.readonly https://www.googleapis.com/auth/calendar.events';
const CAL_CONNECTED = 'Google Calendar connected.';
const CAL_CONNECT_FAILED = 'Could not connect Google Calendar. Please try again.';

async function accountView() {
  renderNav();
  const cfg = await getAuthConfig();

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

  // Connecting opens Google's code-client popup; the page never navigates. The
  // one-time code goes to the server, which exchanges it with Google. Every
  // failure (popup error or closed, no code, the server refusing the code)
  // shows one fixed message and leaves the card as it was; no server or Google
  // text is shown.
  const calFailed = () => {
    $calErr.replaceChildren(alertBox('error', CAL_CONNECT_FAILED));
    toast(CAL_CONNECT_FAILED, 'error');
  };
  let connecting = false;
  const onCalendarCode = async (resp) => {
    const code = resp?.code;
    if (typeof code !== 'string' || code === '') return calFailed();
    if (connecting) return;
    connecting = true;
    let st;
    try {
      st = await api('POST', '/me/google-calendar/connect', { code, platform: 'web' });
    } catch (err) {
      // A lost or timed-out response (status 0) or a 5xx may follow a connect
      // the server completed: ask for the status instead of trusting the
      // failure. A 4xx refusal stored nothing, so it stays a failure.
      if (!(err?.status === 0 || err?.status >= 500)) return calFailed();
      try {
        st = await api('GET', '/me/google-calendar');
      } catch {
        return calFailed();
      }
      if (st?.status !== 'connected') return calFailed();
    } finally {
      connecting = false;
    }
    renderCalendarCard(st);
    $calErr.replaceChildren(alertBox('success', CAL_CONNECTED));
    toast(CAL_CONNECTED, 'success');
  };
  const onConnect = (e) => {
    e?.preventDefault?.();
    const oauth2 = window.google?.accounts?.oauth2;
    if (!oauth2 || !cfg?.google_client_id) return calFailed();
    $calErr.replaceChildren();
    // requestCode runs inside the click so the browser allows the popup.
    oauth2.initCodeClient({
      client_id: cfg.google_client_id,
      scope: CALENDAR_SCOPES,
      ux_mode: 'popup',
      callback: onCalendarCode,
      error_callback: () => calFailed(),
    }).requestCode();
  };

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
            : el('p', { class: 'text-xs opacity-70', text: 'Read and write access.' }),
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
