// All of the web client's mutable state, in one object. Every module reads and
// writes state.x; no module exports a mutable `let`. Module memory only: the
// active member and the PINs are never written to localStorage,
// sessionStorage, cookies or the URL, so a reload starts in the
// administrator's view and asks for any PIN again.

const initialState = () => ({
  // The auth config read, shared by the sign-in and Account views.
  authConfigPromise: null,
  // The focus listener of the rendered Google sign-in button, if any.
  removeButtonFocusListener: null,
  households: [],
  selected: null,
  tab: 'quests',
  activeMember: null, // { id, name, pin } after a successful verify
  adminPin: null, // the admin PIN once entered, sent as X-Admin-PIN
  adminPinState: { householdId: null, set: false }, // GET .../admin-pin
  // memberHistory is the active member's points and reward history, read once
  // per member selection (and again after an admin adjusts that member's
  // points on the Family tab): { member (the activeMember object it was read
  // for), ready }.
  memberHistory: null,
  // memberBarPoints re-reads the active member's total into the member bar
  // drawn last (set by memberBar); null before one is drawn.
  memberBarPoints: null,
  // familyView marks the Family tab's member view in flight; a points-history
  // answer for a view the admin has since left is not rendered.
  familyView: null,
  // The Calendar tab's month, relative to the current one.
  monthOffset: 0,
});

export const state = initialState();

// resetState puts every field back to its starting value. boot() calls it
// first, so a re-loaded app starts clean even though this module stays cached.
export function resetState() {
  state.removeButtonFocusListener?.();
  Object.assign(state, initialState());
}

// views are the top-level views app.js owns (goHome, homeView, renderNav,
// renderHome). app.js fills them in when it loads; section modules call them
// through here, so no module imports app.js.
export const views = {};
