import { api, ApiError } from './api.js';
import { state } from './state.js';
import { action, alertBox, card, el, fmtDay, form, showError } from './dom.js';
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
    const scheduleHeading = () => el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Schedule a quest' });
    const $schedule = el('div', { class: 'space-y-1' },
      scheduleHeading(), el('p', { class: 'text-xs opacity-60', text: 'Loading quests…' }));
    const scheduledHeading = () => el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Scheduled' });
    const $scheduled = el('div', { class: 'space-y-1' },
      scheduledHeading(), el('p', { class: 'text-xs opacity-60', text: 'Loading scheduled quests…' }));
    const upcomingHeading = () => el('h4', { class: 'font-semibold text-sm opacity-80', text: 'Upcoming' });
    const $upcoming = el('div', { class: 'space-y-1' },
      upcomingHeading(), el('p', { class: 'text-xs opacity-60', text: 'Loading upcoming quests…' }));
    $root.replaceChildren(card(null,
      el('h3', { class: 'font-semibold', text: `${m.name} — ${m.points} points` }),
      el('div', {}, el('button', { class: 'btn btn-ghost btn-xs', text: 'Back to family', onclick: () => rerender() })),
      adjustPointsForm(m, view),
      $schedule,
      $scheduled,
      $upcoming,
      $history,
      $rewards));
    // Each read settles to its content, or null when the view is gone or the
    // admin PIN was refused (the tab re-renders locked).
    const load = async (path, label, render) => {
      try {
        return render((await adminRequest('GET', path)).items);
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
    const schedules = `/households/${h.id}/quest-schedules`;
    const [content, rewardContent, scheduleContent, scheduledContent, upcomingContent] = await Promise.all([
      load(`${base}/${m.id}/points-history`, 'points history', (items) => pointsHistoryTable(items, 'No points history yet.', { adminLabel: '(admin adjustment)' })),
      load(`${base}/${m.id}/reward-history`, 'reward history', (items) => rewardHistoryTable(items)),
      load(`/households/${h.id}/quests?limit=100`, 'quests', (items) => scheduleQuestForm(m, view, items || [])),
      load(schedules, 'scheduled quests', (items) => scheduledList(m, view, (items || []).filter((s) => s.member_id === m.id))),
      load(`${base}/${m.id}/upcoming-quests`, 'upcoming quests', (items) => upcomingList(items || [])),
    ]);
    if (state.familyView !== view || !content || !rewardContent || !scheduleContent || !scheduledContent || !upcomingContent) return;
    $history.replaceChildren(heading(), content);
    $rewards.replaceChildren(rewardHeading(), rewardContent);
    $schedule.replaceChildren(scheduleHeading(), scheduleContent);
    $scheduled.replaceChildren(scheduledHeading(), scheduledContent);
    $upcoming.replaceChildren(upcomingHeading(), upcomingContent);
  };

  // A refused admin PIN on a schedule write locks the tab, as load() does.
  // Returns true when it handled the error.
  const lockOnPinError = (err) => {
    if (!isAdminPinError(err)) return false;
    state.familyView = null;
    rerender(); // adminRequest forgot the PIN: the tab locks again
    return true;
  };

  // scheduleQuestForm is the member view's 'Schedule a quest' form: a Quest
  // select of the household's quests, a Date input and Schedule. It POSTs
  // {quest_id, member_id, date, repeat} with the admin's headers; on 201 the view
  // reopens so 'Scheduled' shows the new schedule. A refusal (422 on date,
  // 404 for a gone quest) shows inline, keeping what was entered.
  const scheduleQuestForm = (m, view, quests) => {
    if (quests.length === 0) return el('p', { class: 'text-xs opacity-60', text: 'No quests to schedule yet.' });
    const $quest = el('select', { name: 'quest_id', class: 'select select-bordered select-sm' },
      quests.map((q) => el('option', { value: q.id, text: q.name })));
    $quest.value = quests[0].id;
    const $date = el('input', { type: 'date', name: 'date', class: 'input input-bordered input-sm' });
    const $repeat = el('input', {
      type: 'checkbox', name: 'repeat', class: 'checkbox checkbox-sm',
      onclick: () => {
        if (typeof window !== 'undefined' && typeof window.HTMLInputElement === 'undefined') {
          $repeat.checked = !$repeat.checked;
        }
      },
    });
    const $alert = el('div');
    const $submit = el('button', { type: 'submit', class: 'btn btn-primary btn-sm', text: 'Schedule' });
    const showAlert = (message) => $alert.replaceChildren(el('p', { role: 'alert', class: 'text-error text-xs', text: message }));
    return el('form', {
      class: 'space-y-2',
      onsubmit: async (e) => {
        e.preventDefault();
        $alert.replaceChildren();
        $submit.disabled = true;
        try {
          await adminRequest('POST', `/households/${h.id}/quest-schedules`,
            { quest_id: $quest.value, member_id: m.id, date: $date.value, repeat: Boolean($repeat.checked) });
          if (state.familyView !== view) return;
          await showMember(m);
        } catch (err) {
          if (state.familyView !== view) return;
          if (lockOnPinError(err)) return;
          if (!(err instanceof ApiError)) console.error('schedule quest', err);
          const fields = err instanceof ApiError ? Object.values(err.fields || {}) : [];
          showAlert(fields.length ? fields.join(' ') : (err.message || String(err)));
        } finally {
          $submit.disabled = false;
        }
      },
    },
    el('div', { class: 'flex flex-wrap items-center gap-3' },
      el('label', { class: 'flex items-center gap-2' }, el('span', { class: 'text-sm', text: 'Quest' }), $quest),
      el('label', { class: 'flex items-center gap-2' }, el('span', { class: 'text-sm', text: 'Date' }), $date),
      el('label', { class: 'flex items-center gap-2 cursor-pointer' },
        $repeat,
        el('span', { class: 'text-sm', text: "Repeat (every time the quest's cooldown ends)" }))),
    $alert,
    $submit);
  };

  // upcomingList is the member's upcoming scheduled quests as the API orders
  // them (soonest first): '<Ddd D Mon> — <quest>' and a Repeats badge for a
  // repeated schedule; an empty line when nothing is coming up.
  const upcomingList = (items) => {
    if (items.length === 0) return el('p', { class: 'text-xs opacity-60', text: 'No upcoming quests.' });
    return el('ul', { class: 'space-y-1' }, items.map((i) => el('li', { class: 'flex flex-wrap items-center gap-2 text-sm' },
      el('span', { text: `${fmtDay(i.next_date)} — ${i.quest_name}` }),
      i.repeat && el('span', { class: 'badge badge-sm', text: 'Repeats' }))));
  };

  // scheduledList is the member's schedules: '<quest> — from <date>' (with
  // ', repeats' for a repeated schedule) and a
  // Remove (confirm with 'Sure?') that DELETEs it and reopens the view; an
  // empty line when nothing is scheduled.
  const scheduledList = (m, view, items) => {
    if (items.length === 0) return el('p', { class: 'text-xs opacity-60', text: 'No quests are scheduled.' });
    return el('ul', { class: 'space-y-1' }, items.map((s) => {
      const $err = el('div', { class: 'text-error text-xs w-full' });
      return el('li', { class: 'flex flex-wrap items-center gap-2 text-sm' },
        el('span', { text: `${s.quest_name} — from ${s.start_date}${s.repeat ? ', repeats' : ''}` }),
        el('button', {
          class: 'btn btn-ghost btn-xs', text: 'Remove',
          onclick: confirmClick('Sure?', async () => {
            $err.textContent = '';
            try {
              await adminRequest('DELETE', `/households/${h.id}/quest-schedules/${s.id}`);
            } catch (err) {
              if (state.familyView !== view) return;
              if (lockOnPinError(err)) return;
              // Already gone (removed elsewhere): reopen to show the list as it is.
              if (!(err instanceof ApiError) || err.status !== 404) {
                $err.textContent = err.message || String(err);
                return;
              }
            }
            if (state.familyView !== view) return;
            await showMember(m);
          }),
        }),
        $err);
    }));
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
