import { api, ApiError } from './api.js';
import { state, views } from './state.js';
import { action, el, form, toast } from './dom.js';

// The household view's shared controls: the active member and admin unlock
// (held in state.js), the admin-gated request, the PIN forms and the member bar.

// clearMemberState returns to the administrator's view with nothing unlocked.
export function clearMemberState() {
  state.activeMember = null;
  state.adminPin = null;
  state.adminPinState = { householdId: null, set: false };
  state.memberHistory = null;
  state.familyView = null;
}

// activeMemberAction is the one accessor member actions (accept, abandon, mark
// done) take their body from: { member_id, pin }, or null with no member active.
export function activeMemberAction() {
  return state.activeMember ? { member_id: state.activeMember.id, pin: state.activeMember.pin } : null;
}

// adminHeaders is the 4th api() argument for admin-gated requests.
function adminHeaders() {
  return state.adminPin ? { headers: { 'X-Admin-PIN': state.adminPin } } : {};
}

// adminRequest sends an admin-gated request. A 403 wrong_pin or
// admin_pin_required forgets the held admin PIN (it was changed or removed
// elsewhere) so management locks again; tokens are untouched.
export async function adminRequest(method, path, body) {
  try {
    return await api(method, path, body, adminHeaders());
  } catch (err) {
    if (err instanceof ApiError && (err.code === 'wrong_pin' || err.code === 'admin_pin_required')) {
      state.adminPin = null;
      if (err.code === 'admin_pin_required') state.adminPinState.set = true;
    }
    throw err;
  }
}

// memberActionFailed handles an error from a member action. A 403 wrong_pin
// clears the active member back to the administrator's view and says so; the
// session tokens are kept. Returns true when it handled the error.
export function memberActionFailed(err) {
  if (!(err instanceof ApiError) || err.code !== 'wrong_pin') return false;
  state.activeMember = null;
  toast('That PIN is no longer correct. Pick the member again.', 'error');
  views.renderHome();
  return true;
}

// managementVisible: the Family tab and management controls are shown in the
// administrator's view, or to anyone while no admin PIN is set.
export const managementVisible = () => !state.activeMember || !state.adminPinState.set;
// managementUnlocked: admin-gated writes can be sent.
export const managementUnlocked = () => !state.adminPinState.set || state.adminPin !== null;

const PIN_RE = /^\d{4}$/;

// checkPin rejects a PIN that is not exactly 4 digits before anything is sent,
// with the error on the field.
export function checkPin(pin, field = 'pin') {
  if (!PIN_RE.test(pin)) {
    throw new ApiError(422, { message: 'Check the highlighted field.', fields: { [field]: 'must be exactly 4 digits' } });
  }
}

// pinFailure rewrites PIN errors into clear messages: wrong PIN and rate limit.
export function pinFailure(err) {
  if (err instanceof ApiError && err.code === 'wrong_pin') return new ApiError(403, { code: 'wrong_pin', message: 'Wrong PIN. Try again.' });
  if (err instanceof ApiError && err.status === 429) return new ApiError(429, { message: 'Too many attempts, try again later.' });
  return err;
}

// pinForm is a one-field PIN form: validated client side, errors under it.
export function pinForm({ label = 'PIN', submit, onPin }) {
  return form({
    fields: [{ name: 'pin', label, type: 'password', required: true, autocomplete: 'off', placeholder: '4 digits' }],
    submit, inline: true,
    onSubmit: async (v) => {
      checkPin(v.pin);
      try { await onPin(v.pin); } catch (err) { throw pinFailure(err); }
    },
  });
}

export const isAdminPinError = (err) => err instanceof ApiError && err.status === 403 &&
  (err.code === 'wrong_pin' || err.code === 'admin_pin_required');

// memberBar shows whose view this is and switches it: pick a member (with
// their PIN when they have one), or go back to the administrator's view (with
// the admin PIN when one is set).
export function memberBar(h) {
  const $panel = el('div');
  const $who = el('span', { class: 'text-sm flex-1', text: `Viewing as: ${state.activeMember ? state.activeMember.name : 'Administrator'}` });
  // The active member's points come from the server on every render (never
  // computed here), so an approval shows up the next time the view is drawn.
  // state.memberBarPoints re-reads them on demand, for a write made outside the
  // member's own actions (an admin adjustment on the Family tab).
  const showPoints = () => {
    if (!state.activeMember) return;
    const shown = state.activeMember;
    api('GET', `/households/${h.id}/family-members`)
      .then((r) => {
        const m = (r.items || []).find((x) => x.id === shown.id);
        if (m && state.activeMember === shown) $who.textContent = `Viewing as: ${m.name} · ${m.points} points`;
      })
      .catch((err) => console.error('member points', err)); // the name alone stays; the tab surfaces API errors
  };
  state.memberBarPoints = showPoints;
  showPoints();

  const become = (m, pin) => {
    state.activeMember = { id: m.id, name: m.name, pin };
    state.adminPin = null; // management locks again while a member is active
    views.renderHome();
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
    if (!state.adminPinState.set) { state.activeMember = null; views.renderHome(); return; }
    $panel.replaceChildren(
      el('p', { class: 'text-sm', text: 'Enter the admin PIN to return to the administrator view.' }),
      pinForm({
        label: 'Admin PIN',
        submit: 'Unlock',
        onPin: async (pin) => {
          await api('POST', `/households/${h.id}/admin-pin/verify`, { pin });
          state.adminPin = pin;
          state.activeMember = null;
          views.renderHome();
        },
      }));
  };

  return el('div', { class: 'space-y-2' },
    el('div', { class: 'flex flex-wrap items-center gap-2' },
      $who,
      el('button', { class: 'btn btn-sm', text: 'Switch member', onclick: openPicker }),
      state.activeMember && el('button', { class: 'btn btn-sm btn-ghost', text: 'Administrator view', onclick: backToAdmin })),
    $panel);
}

// confirmClick turns a button into a two-step confirm (no blocking dialogs).
export function confirmClick(prompt, fn) {
  return action(async (e) => {
    const btn = e.currentTarget;
    if (btn.dataset.armed) return fn();
    btn.dataset.armed = '1';
    const original = btn.textContent;
    btn.textContent = prompt;
    setTimeout(() => { delete btn.dataset.armed; btn.textContent = original; }, 3000);
  });
}
