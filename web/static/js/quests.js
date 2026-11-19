import { api, ApiError } from './api.js';
import { state } from './state.js';
import { action, alertBox, card, el, fmtDate, form, showError } from './dom.js';
import { activeMemberAction, adminRequest, confirmClick, isAdminPinError, managementUnlocked, managementVisible, memberActionFailed, pinForm } from './household.js';

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

export async function questsTab(h, $root, notice = null) {
  const rerender = (next = null) => { $root.replaceChildren(); questsTab(h, $root, next).catch((err) => showError($root, err)); };
  const member = state.activeMember; // the view this render is for; a later pick makes it stale

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
  if (state.activeMember !== member) return; // a newer pick renders its own view; never overwrite it with this one
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
async function takeCard(h, quests, members, rerender, member = state.activeMember) {
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
  // selection (state.memberHistory is keyed on the state.activeMember object, which each
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
  if (state.memberHistory?.member !== member) {
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
    state.memberHistory = { member, ready };
  }
  const result = await state.memberHistory.ready;
  if (state.activeMember !== member) return null; // another member was picked meanwhile
  if (result.failure) {
    state.memberHistory = null;
    memberActionFailed(result.failure);
    return null;
  }
  const history = result.points.items;
  const historyError = result.points.error;
  const rewardHistory = result.rewards.items;
  const rewardHistoryError = result.rewards.error;

  const act = (verb, q) => action(async () => {
    if (state.activeMember !== member) return; // this card is stale: never send another member's identity for its quests
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
    if (state.activeMember !== member) return; // this card is stale: never send another member's identity
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
    // The deadline is the server's claim_expires_at, shown as sent (textContent
    // only); the client does no date arithmetic.
    section('Mine', parts.mine, (q) => row(q,
      q.claim_expires_at && el('span', { class: 'text-xs opacity-70', text: `Due by ${fmtDate(q.claim_expires_at)}` }),
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
export function rewardHistoryTable(items) {
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
export function pointsHistoryTable(history, emptyText, opts = {}) {
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
      state.adminPin = pin;
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
