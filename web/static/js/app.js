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
  if (session.user) return homeView();
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
let tab = 'chores';

async function homeView() {
  renderNav();
  if (!session.user.email_verified) return unverifiedView();

  households = (await api('GET', '/households')).items;
  let remembered = null;
  try { remembered = localStorage.getItem(SELECTED_KEY); } catch { /* storage disabled */ }
  selected = households.find((h) => h.id === remembered) || households[0] || null;
  renderHome();
}

function selectHousehold(h) {
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

function renderHome() {
  const list = el('ul', { class: 'menu bg-base-100 rounded-box w-full' },
    households.length === 0 && el('li', { class: 'menu-disabled' }, el('span', { text: 'No households yet' })),
    households.map((h) => el('li', {},
      el('a', {
        class: selected?.id === h.id ? 'menu-active' : '', href: '#',
        onclick: (e) => { e.preventDefault(); selectHousehold(h); },
      }, el('span', { class: 'flex-1', text: h.name }), el('span', { class: 'badge badge-sm', text: h.role })))));

  const sidebar = el('div', { class: 'space-y-4' },
    list,
    card(null,
      el('h3', { class: 'font-semibold', text: 'New household' }),
      form({
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
      }),
      el('div', { class: 'divider my-0' }),
      el('h3', { class: 'font-semibold', text: 'Join with invite code' }),
      form({
        fields: [{ name: 'code', label: 'Code', placeholder: 'XXXX-XXXX-XXXX', required: true }],
        submit: 'Join',
        onSubmit: async (v) => {
          const h = await api('POST', '/invites/accept', v);
          households.push(h);
          selectHousehold(h);
        },
      })));

  const main = selected ? householdPanel(selected) : card('Get started', el('p', { text: 'Create a household or join one with an invite code.' }));
  $app.replaceChildren(el('div', { class: 'grid gap-6 md:grid-cols-[16rem_1fr]' }, sidebar, main));
}

function householdPanel(h) {
  const $content = el('div');
  const tabs = [['chores', 'Chores'], ['events', 'Calendar'], ['members', 'Members']];
  const header = el('div', { class: 'flex flex-wrap items-center gap-2' },
    el('h2', { class: 'text-2xl font-bold flex-1', text: h.name }),
    el('span', { class: 'text-sm opacity-60', text: h.timezone }),
    h.role === 'admin' && el('button', {
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

  const loaders = { chores: choresTab, events: eventsTab, members: membersTab };
  loaders[tab](h, $content).catch((err) => showError($content, err));
  return el('div', { class: 'space-y-4 min-w-0' }, header, tabBar, $content);
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

async function membersFor(h) {
  return (await api('GET', `/households/${h.id}/members`)).items;
}

// ── Chores ────────────────────────────────────────────────────────────────────

let choreStatus = 'open';

async function choresTab(h, $root) {
  const [members, page] = await Promise.all([
    membersFor(h),
    api('GET', `/households/${h.id}/chores?status=${choreStatus}&limit=100`),
  ]);
  const nameOf = Object.fromEntries(members.map((m) => [m.user_id, m.display_name]));
  const assigneeOptions = [['', 'Unassigned'], ...members.map((m) => [m.user_id, m.display_name])];
  const rerender = () => { $root.replaceChildren(); choresTab(h, $root).catch((err) => showError($root, err)); };

  const choreFields = (c = {}) => [
    { name: 'title', label: 'Title', required: true, value: c.title },
    { name: 'assignee_id', label: 'Assignee', type: 'select', options: assigneeOptions, value: c.assignee_id || '' },
    { name: 'due_on', label: 'Due', type: 'date', value: c.due_on || '' },
  ];
  const toBody = (v) => ({ title: v.title, assignee_id: v.assignee_id || null, due_on: v.due_on || null });

  const filter = el('div', { class: 'join' },
    ['open', 'done', 'all'].map((s) => el('button', {
      class: `btn btn-sm join-item ${s === choreStatus ? 'btn-active' : ''}`, text: s,
      onclick: () => { choreStatus = s; rerender(); },
    })));

  const rows = page.items.map((c) => {
    const $row = el('li', { class: 'flex items-center gap-3 py-2 border-b border-base-300' },
      el('input', {
        type: 'checkbox', class: 'checkbox', checked: c.completed, title: c.completed ? 'Reopen' : 'Complete',
        onchange: action(async () => {
          await api(c.completed ? 'DELETE' : 'PUT', `/households/${h.id}/chores/${c.id}/completion`);
          rerender();
        }),
      }),
      el('div', { class: 'flex-1 min-w-0' },
        el('div', { class: c.completed ? 'line-through opacity-60' : '', text: c.title }),
        el('div', { class: 'text-xs opacity-60' },
          c.assignee_id ? `→ ${nameOf[c.assignee_id] || 'former member'}` : 'Unassigned',
          c.due_on ? ` · due ${c.due_on}` : '')),
      el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Edit',
        onclick: () => $row.replaceWith(el('li', { class: 'py-2' }, form({
          fields: choreFields(c), submit: 'Save', inline: true,
          onSubmit: async (v) => { await api('PUT', `/households/${h.id}/chores/${c.id}`, { ...toBody(v), description: c.description }); rerender(); },
        }))),
      }),
      el('button', {
        class: 'btn btn-ghost btn-xs text-error', text: 'Delete',
        onclick: confirmClick('Sure?', async () => { await api('DELETE', `/households/${h.id}/chores/${c.id}`); rerender(); }),
      }));
    return $row;
  });

  $root.replaceChildren(card(null,
    el('div', { class: 'flex items-center justify-between' }, el('h3', { class: 'font-semibold', text: 'Chores' }), filter),
    el('ul', {}, rows.length ? rows : el('li', { class: 'opacity-60 py-2', text: 'Nothing here.' })),
    page.next_offset !== null && el('p', { class: 'text-xs opacity-60', text: 'Showing the first 100.' }),
    el('div', { class: 'divider my-0' }),
    form({
      fields: choreFields(), submit: 'Add chore', inline: true,
      onSubmit: async (v) => { await api('POST', `/households/${h.id}/chores`, toBody(v)); rerender(); },
    })));
}

// ── Events ────────────────────────────────────────────────────────────────────

let monthOffset = 0;

async function eventsTab(h, $root) {
  const now = new Date();
  const from = new Date(now.getFullYear(), now.getMonth() + monthOffset, 1);
  const to = new Date(now.getFullYear(), now.getMonth() + monthOffset + 1, 1);
  const qs = new URLSearchParams({ from: from.toISOString(), to: to.toISOString() });
  const { items } = await api('GET', `/households/${h.id}/events?${qs}`);
  const rerender = () => { $root.replaceChildren(); eventsTab(h, $root).catch((err) => showError($root, err)); };

  // datetime-local inputs are in the browser's local zone.
  const localInput = (d) => new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  const eventFields = (e = {}) => [
    { name: 'title', label: 'Title', required: true, value: e.title },
    { name: 'starts_at', label: 'Starts', type: 'datetime-local', required: true, value: e.starts_at ? localInput(new Date(e.starts_at)) : '' },
    { name: 'ends_at', label: 'Ends', type: 'datetime-local', value: e.ends_at ? localInput(new Date(e.ends_at)) : '' },
    { name: 'location', label: 'Location', value: e.location },
    { name: 'all_day', label: 'All day', type: 'checkbox', value: e.all_day },
  ];
  const toBody = (v) => {
    let start = new Date(v.starts_at);
    let end = v.ends_at ? new Date(v.ends_at) : new Date(start.getTime() + 60 * 60000);
    if (v.all_day) {
      start = new Date(start.getFullYear(), start.getMonth(), start.getDate());
      end = new Date(start.getFullYear(), start.getMonth(), start.getDate() + 1);
    }
    return { title: v.title, location: v.location, all_day: v.all_day, starts_at: start.toISOString(), ends_at: end.toISOString() };
  };

  const nav = el('div', { class: 'join' },
    el('button', { class: 'btn btn-sm join-item', text: '‹', onclick: () => { monthOffset--; rerender(); } }),
    el('button', { class: 'btn btn-sm join-item', text: from.toLocaleDateString(undefined, { month: 'long', year: 'numeric' }), onclick: () => { monthOffset = 0; rerender(); } }),
    el('button', { class: 'btn btn-sm join-item', text: '›', onclick: () => { monthOffset++; rerender(); } }));

  const rows = items.map((e) => {
    const when = e.all_day ? new Date(e.starts_at).toLocaleDateString() + ' (all day)' : `${fmtDate(e.starts_at)} – ${fmtDate(e.ends_at)}`;
    const $row = el('li', { class: 'flex items-center gap-3 py-2 border-b border-base-300' },
      el('div', { class: 'flex-1 min-w-0' },
        el('div', { text: e.title }),
        el('div', { class: 'text-xs opacity-60', text: when + (e.location ? ` · ${e.location}` : '') })),
      el('button', {
        class: 'btn btn-ghost btn-xs', text: 'Edit',
        onclick: () => $row.replaceWith(el('li', { class: 'py-2' }, form({
          fields: eventFields(e), submit: 'Save', inline: true,
          onSubmit: async (v) => { await api('PUT', `/households/${h.id}/events/${e.id}`, { ...toBody(v), description: e.description }); rerender(); },
        }))),
      }),
      el('button', {
        class: 'btn btn-ghost btn-xs text-error', text: 'Delete',
        onclick: confirmClick('Sure?', async () => { await api('DELETE', `/households/${h.id}/events/${e.id}`); rerender(); }),
      }));
    return $row;
  });

  $root.replaceChildren(card(null,
    el('div', { class: 'flex items-center justify-between' }, el('h3', { class: 'font-semibold', text: 'Calendar' }), nav),
    el('ul', {}, rows.length ? rows : el('li', { class: 'opacity-60 py-2', text: 'No events this month.' })),
    el('div', { class: 'divider my-0' }),
    form({
      fields: eventFields(), submit: 'Add event', inline: true,
      onSubmit: async (v) => { await api('POST', `/households/${h.id}/events`, toBody(v)); rerender(); },
    })));
}

// ── Members ───────────────────────────────────────────────────────────────────

async function membersTab(h, $root) {
  const members = await membersFor(h);
  const isAdmin = h.role === 'admin';
  const me = session.user.id;
  const rerender = () => { $root.replaceChildren(); membersTab(h, $root).catch((err) => showError($root, err)); };
  const $inviteResult = el('div');

  const rows = members.map((m) => el('li', { class: 'flex items-center gap-3 py-2 border-b border-base-300' },
    el('span', { class: 'flex-1', text: m.display_name + (m.user_id === me ? ' (you)' : '') }),
    el('span', { class: 'badge', text: m.role }),
    isAdmin && el('button', {
      class: 'btn btn-ghost btn-xs', text: m.role === 'admin' ? 'Make member' : 'Make admin',
      onclick: action(async () => {
        await api('PUT', `/households/${h.id}/members/${m.user_id}`, { role: m.role === 'admin' ? 'member' : 'admin' });
        if (m.user_id === me) return homeView();
        rerender();
      }),
    }),
    (isAdmin || m.user_id === me) && el('button', {
      class: 'btn btn-ghost btn-xs text-error', text: m.user_id === me ? 'Leave' : 'Remove',
      onclick: confirmClick('Sure?', async () => {
        await api('DELETE', `/households/${h.id}/members/${m.user_id}`);
        if (m.user_id === me) return homeView();
        rerender();
      }),
    })));

  $root.replaceChildren(card(null,
    el('h3', { class: 'font-semibold', text: 'Members' }),
    el('ul', {}, rows),
    isAdmin && el('div', { class: 'space-y-2' },
      el('button', {
        class: 'btn btn-sm', text: 'Create invite code',
        onclick: action(async () => {
          const inv = await api('POST', `/households/${h.id}/invites`);
          $inviteResult.replaceChildren(alertBox('info', `Code ${inv.code} — single use, expires ${fmtDate(inv.expires_at)}.`));
        }, $inviteResult),
      }),
      $inviteResult)));
}

// ── Account ───────────────────────────────────────────────────────────────────

async function accountView() {
  renderNav();
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

  $app.replaceChildren(el('div', { class: 'max-w-2xl mx-auto space-y-6' },
    el('button', { class: 'btn btn-ghost btn-sm', text: '← Back', onclick: () => homeView() }),
    card('Profile', el('p', { class: 'text-sm opacity-70', text: session.user.email }), form({
      fields: [{ name: 'display_name', label: 'Display name', required: true, value: session.user.display_name }],
      submit: 'Save',
      onSubmit: async (v) => { session.user = await api('PUT', '/me', v); renderNav(); toast('Saved', 'success'); },
    })),
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
      el('p', { class: 'text-sm', text: 'Permanently deletes your account. Households where you are the only member are deleted too.' }),
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
