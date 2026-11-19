import { api } from './api.js';
import { state } from './state.js';
import { alertBox, card, el, fmtDate, showError } from './dom.js';
import { accountView } from './account.js';

export async function eventsTab(h, $root) {
  const now = new Date();
  const from = new Date(now.getFullYear(), now.getMonth() + state.monthOffset, 1);
  const to = new Date(now.getFullYear(), now.getMonth() + state.monthOffset + 1, 1);
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
    el('button', { class: 'btn btn-sm join-item', text: '‹', onclick: () => { state.monthOffset--; rerender(); } }),
    el('button', { class: 'btn btn-sm join-item', text: from.toLocaleDateString(undefined, { month: 'long', year: 'numeric' }), onclick: () => { state.monthOffset = 0; rerender(); } }),
    el('button', { class: 'btn btn-sm join-item', text: '›', onclick: () => { state.monthOffset++; rerender(); } }));

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
