import { api, clearTokens, session } from './api.js';
import { views } from './state.js';
import { action, alertBox, appRoot, card, el, fmtDate, form, showError, toast } from './dom.js';
import { getAuthConfig } from './auth.js';

// The calendar scopes the server asks Google for; it derives read-only or
// read/write from what Google actually grants.
const CALENDAR_SCOPES = 'https://www.googleapis.com/auth/calendar.calendarlist.readonly https://www.googleapis.com/auth/calendar.events';
const CAL_CONNECTED = 'Google Calendar connected.';
const CAL_CONNECT_FAILED = 'Could not connect Google Calendar. Please try again.';

export async function accountView() {
  views.renderNav();
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
        if (s.current) { clearTokens(); return views.goHome(); }
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

  appRoot().replaceChildren(el('div', { class: 'max-w-2xl mx-auto space-y-6' },
    el('button', { class: 'btn btn-ghost btn-sm', text: '← Back', onclick: () => views.homeView() }),
    card('Profile', el('p', { class: 'text-sm opacity-70', text: session.user.email }), form({
      fields: [{ name: 'display_name', label: 'Display name', required: true, value: session.user.display_name }],
      submit: 'Save',
      onSubmit: async (v) => { session.user = await api('PUT', '/me', v); views.renderNav(); toast('Saved', 'success'); },
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
            onSubmit: async (v) => { await api('DELETE', '/me', v); clearTokens(); views.goHome(); },
          })
        : form({
            fields: [{ name: 'email', label: 'Confirm email', type: 'email', required: true, autocomplete: 'email' }],
            submit: 'Delete my account',
            onSubmit: async (v) => { await api('DELETE', '/me', v); clearTokens(); views.goHome(); },
          }))));
}
