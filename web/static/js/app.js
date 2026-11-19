// Chorus web test client. Plain DOM, no framework. All user-supplied text is
// inserted with textContent (never innerHTML), so API data can't inject markup.

import { api, clearTokens, refresh, session } from './api.js';
import { resetState, state, views } from './state.js';
import { action, alertBox, appRoot, card, el, form, installApiLog, showError, toast } from './dom.js';
import { clearMemberState, confirmClick, managementVisible, memberBar } from './household.js';
import { authView, resetPasswordView, verifyEmailView } from './auth.js';
import { questsTab } from './quests.js';
import { rewardsTab } from './rewards.js';
import { familyTab } from './family.js';
import { eventsTab } from './events.js';
import { accountView } from './account.js';

const $nav = document.getElementById('nav-actions');

// ── Routing ───────────────────────────────────────────────────────────────────

// boot starts the client on page load: it resets all client state first (so a
// re-loaded app starts clean although the sibling modules stay cached), hands
// the section modules the top-level views, wires the API log panel, then
// routes.
function boot() {
  resetState();
  Object.assign(views, { goHome, homeView, renderNav, renderHome });
  installApiLog();
  return route();
}

// route shows the view for the current URL and session.
async function route() {
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
  route();
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

// ── Home: households ──────────────────────────────────────────────────────────

const SELECTED_KEY = 'chorus.household';

async function homeView() {
  renderNav();
  if (!session.user.email_verified) return unverifiedView();

  state.households = (await api('GET', '/households')).items;
  let remembered = null;
  try { remembered = localStorage.getItem(SELECTED_KEY); } catch { /* storage disabled */ }
  const next = state.households.find((h) => h.id === remembered) || state.households[0] || null;
  if (next?.id !== state.selected?.id) clearMemberState();
  state.selected = next;
  renderHome();
}

function selectHousehold(h) {
  clearMemberState();
  state.selected = h;
  try { localStorage.setItem(SELECTED_KEY, h ? h.id : ''); } catch { /* storage disabled */ }
  renderHome();
}

function unverifiedView() {
  const $msg = el('div');
  appRoot().replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Verify your email',
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
      state.households.push(h);
      selectHousehold(h);
    },
  });
}

function renderHome() {
  if (state.households.length === 0 || !state.selected) {
    // No household (normally one is created at first sign-in): the create form
    // is the main view.
    appRoot().replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('New household', newHouseholdForm())));
    return;
  }

  const list = el('ul', { class: 'menu bg-base-100 rounded-box w-full' },
    state.households.map((h) => el('li', {},
      el('a', {
        class: state.selected?.id === h.id ? 'menu-active' : '', href: '#',
        onclick: (e) => { e.preventDefault(); selectHousehold(h); },
      }, el('span', { class: 'flex-1', text: h.name }), el('span', { class: 'badge badge-sm', text: h.role })))));

  const sidebar = el('div', { class: 'space-y-4' },
    list,
    managementVisible() && card(null, el('h3', { class: 'font-semibold', text: 'New household' }), newHouseholdForm()));

  appRoot().replaceChildren(el('div', { class: 'grid gap-6 md:grid-cols-[16rem_1fr]' }, sidebar, householdPanel(state.selected)));
}

function householdPanel(h) {
  const $content = el('div');
  if (state.adminPinState.householdId !== h.id) {
    api('GET', `/households/${h.id}/admin-pin`)
      .then((r) => {
        if (state.selected?.id !== h.id) return;
        state.adminPinState = { householdId: h.id, set: !!r.set };
        renderHome();
      })
      .catch((err) => showError($content, err));
    return el('div', { class: 'space-y-4 min-w-0' },
      el('h2', { class: 'text-2xl font-bold', text: h.name }),
      $content);
  }

  const manage = managementVisible();
  const tabs = [['quests', 'Quests'], ['rewards', 'Rewards'], manage && ['family', 'Family'], ['events', 'Calendar']].filter(Boolean);
  if (!tabs.some(([id]) => id === state.tab)) state.tab = 'quests';

  const header = el('div', { class: 'flex flex-wrap items-center gap-2' },
    el('h2', { class: 'text-2xl font-bold flex-1', text: h.name }),
    el('span', { class: 'text-sm opacity-60', text: h.timezone }),
    manage && h.role === 'admin' && el('button', {
      class: 'btn btn-error btn-outline btn-sm', text: 'Delete household',
      onclick: confirmClick('Delete household?', async () => {
        await api('DELETE', `/households/${h.id}`);
        state.households = state.households.filter((x) => x.id !== h.id);
        selectHousehold(state.households[0] || null);
      }),
    }));
  const tabBar = el('div', { role: 'tablist', class: 'tabs tabs-border' },
    tabs.map(([id, label]) => el('a', {
      role: 'tab', class: `tab ${id === state.tab ? 'tab-active' : ''}`, text: label, href: '#',
      onclick: (e) => { e.preventDefault(); state.tab = id; renderHome(); },
    })));

  const loaders = { quests: questsTab, rewards: rewardsTab, family: familyTab, events: eventsTab };
  (loaders[state.tab] || questsTab)(h, $content).catch((err) => showError($content, err));
  return el('div', { class: 'space-y-4 min-w-0' }, header, memberBar(h), tabBar, $content);
}

boot().catch((err) => showError(appRoot(), err));
