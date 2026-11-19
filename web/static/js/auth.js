import { authConfig, clearTokens, DEVICE_NAME, googleSignIn, publicApi, setTokens } from './api.js';
import { state, views } from './state.js';
import { alertBox, appRoot, card, el, form, toast } from './dom.js';

export function getAuthConfig() {
  if (!state.authConfigPromise) {
    state.authConfigPromise = authConfig().catch(() => ({ google_sign_in: false, google_calendar: false }));
  }
  return state.authConfigPromise;
}

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
    views.goHome();
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
    state.removeButtonFocusListener?.();
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
    state.removeButtonFocusListener = () => {
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

export async function authView(tab, alertMsg = null) {
  views.renderNav();
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
        views.goHome();
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
  appRoot().replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Welcome to Chorus', tabBar, $body)));
}

export async function verifyEmailView(token) {
  appRoot().replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Verifying email…')));
  try {
    setTokens(await publicApi('POST', '/auth/verify-email', { token, device_name: DEVICE_NAME }));
    toast('Email verified!', 'success');
    views.goHome();
  } catch (err) {
    appRoot().replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Verification failed',
      alertBox('error', err.message),
      el('a', { class: 'btn', href: '/', text: 'Back to sign in' }))));
  }
}

export function resetPasswordView(token) {
  views.renderNav();
  appRoot().replaceChildren(el('div', { class: 'max-w-md mx-auto' }, card('Choose a new password', form({
    fields: [{ name: 'password', label: 'New password', type: 'password', required: true, autocomplete: 'new-password' }],
    submit: 'Reset password',
    onSubmit: async (v) => {
      await publicApi('POST', '/auth/reset-password', { token, password: v.password });
      toast('Password reset. Sign in with your new password.', 'success');
      clearTokens();
      views.goHome();
    },
  }))));
}
