import { api, ApiError } from './api.js';
import { state } from './state.js';
import { action, alertBox, card, el, form, showError } from './dom.js';
import { activeMemberAction, adminRequest, confirmClick, managementUnlocked, managementVisible, memberActionFailed, pinForm } from './household.js';

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
export async function rewardsTab(h, $root, notice = null) {
  const rerender = (newNotice = null) => { $root.replaceChildren(); rewardsTab(h, $root, newNotice).catch((err) => showError($root, err)); };
  const visible = managementVisible();
  const unlocked = visible && managementUnlocked();
  const isMember = Boolean(state.activeMember);
  const base = `/households/${h.id}/rewards`;

  const [page, library, membersRes, pendingRes] = await Promise.all([
    api('GET', base),
    visible ? api('GET', '/reward-library') : Promise.resolve({ items: [] }),
    isMember ? api('GET', `/households/${h.id}/family-members`) : Promise.resolve({ items: [] }),
    isMember ? api('GET', `/households/${h.id}/family-members/${state.activeMember.id}/reward-redemptions/pending`, undefined, {
      headers: { 'X-Member-PIN': state.activeMember.pin ?? '' },
    }).catch((err) => {
      if (memberActionFailed(err)) return null;
      // Unknown pending state: say so, and offer no Redeem (one may be waiting).
      console.error(err);
      return { failed: err };
    }) : Promise.resolve({ items: [] }),
  ]);
  if (isMember && !state.activeMember) return;
  const rewards = page.items || [];
  const memberList = membersRes?.items || membersRes || [];
  const currentMember = isMember ? (memberList.find((m) => m.id === state.activeMember.id) || state.activeMember) : null;
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
      state.adminPin = pin;
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
