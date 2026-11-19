import { api, ApiError } from './api.js';
import { state } from './state.js';
import { action, alertBox, card, el, form, showError } from './dom.js';
import { adminRequest, checkPin, confirmClick, isAdminPinError, managementUnlocked, pinFailure, pinForm } from './household.js';
import { pointsHistoryTable, rewardHistoryTable } from './quests.js';

export async function familyTab(h, $root) {
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
          state.adminPin = pin;
          rerender();
        },
      })));
    return;
  }

  state.familyView = null; // any member view still loading is now stale
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
    if (isAdminPinError(err)) { state.adminPinState.set = true; rerender(); return; }
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
    state.familyView = view;
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
        if (state.familyView !== view) return null;
        if (err instanceof ApiError && (err.code === 'wrong_pin' || err.code === 'admin_pin_required')) {
          state.familyView = null;
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
    if (state.familyView !== view || !content || !rewardContent) return;
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
          if (state.activeMember?.id === m.id) {
            state.memberHistory = null;
            state.memberBarPoints?.();
          }
          if (state.familyView !== view) return;
          await showMember(updated);
        } catch (err) {
          if (state.familyView !== view) return;
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
    el('p', { class: 'text-sm', text: state.adminPinState.set ? 'Admin PIN set' : 'No admin PIN' }),
    $err,
    pinForm({
      label: state.adminPinState.set ? 'New admin PIN' : 'Admin PIN',
      submit: state.adminPinState.set ? 'Change admin PIN' : 'Set admin PIN',
      onPin: async (pin) => {
        await adminRequest('PUT', `/households/${h.id}/admin-pin`, { pin });
        state.adminPinState.set = true;
        state.adminPin = pin;
        rerender();
      },
    }),
    state.adminPinState.set && el('button', {
      class: 'btn btn-ghost btn-sm text-error', text: 'Remove admin PIN',
      onclick: confirmClick('Sure?', async () => {
        try {
          await adminRequest('DELETE', `/households/${h.id}/admin-pin`);
        } catch (err) {
          showError($err, pinFailure(err));
          return;
        }
        state.adminPinState.set = false;
        state.adminPin = null;
        rerender();
      }),
    }));
}
