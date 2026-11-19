# Chorus API v1

The contract the iOS/Android apps and the web test client build against. Base path: `/api/v1`.

## Conventions

- **JSON only.** Request bodies require `Content-Type: application/json`. Unknown fields, trailing data, and bodies over 64 KiB are rejected.
- **IDs** are UUIDv7 strings. **Timestamps** are RFC 3339 in UTC (`2026-11-05T18:00:00Z`).
- **PUT is full replacement.** An omitted optional field is cleared (one exception: a quest PUT that omits `streak_bonus` keeps it). Send the whole object.
- **Lists** return `{"items": [...]}`. Paginated lists also return `next_offset` (`null` on the last page).
- **Not found vs. forbidden.** Resources in a household you don't belong to return `404`, the same as nonexistent ones. Each household has one login, its admin; a `403` means a PIN check failed (see below).
- **Errors** always use this shape. Branch on `code`. `message` is for humans and may change.

```json
{"error": {"code": "validation_failed", "message": "One or more fields are invalid.", "fields": {"title": "is required"}}}
```

| Status | `code` | Meaning |
|---|---|---|
| 400 | `invalid_json`, `invalid_token`, `google_calendar_failed` | `invalid_json` / `invalid_token`: malformed request or bad emailed token. `google_calendar_failed`: `POST /me/google-calendar/connect` could not exchange the code (rejected, reused, empty, missing required scopes or unsupported platform); the message is generic, no Google text. |
| 401 | `unauthenticated`, `token_expired`, `invalid_credentials`, `invalid_refresh_token`, `google_sign_in_failed` | See auth flow below |
| 403 | `email_not_verified`, `forbidden`, `wrong_pin`, `admin_pin_required` | `wrong_pin`: a member or admin PIN did not match. `admin_pin_required`: an admin PIN is set and the `X-Admin-PIN` header was missing. Neither signs you out. |
| 404 | `not_found` | |
| 409 | `already_verified`, `quest_taken`, `already_participant`, `age_restricted` | Conflict: email is already verified, or quest conflict (already taken, member already participant, or a member outside the quest's age range on accept or share). `age_restricted` carries the message `This quest is outside that member's age range.` |
| 413 / 415 | `body_too_large` / `unsupported_media_type` | |
| 422 | `validation_failed`, `insufficient_points` | `fields` maps field → message. `insufficient_points`: a reward approval the member can no longer afford; `message` (and `fields.points`) is the reason to show, e.g. `Can't approve Pizza night for Alex: Alex has 90 points and it costs 150. Nothing was taken.` |
| 423 | `account_locked` | 10 failed logins locks for 1 hour |
| 429 | `rate_limited` | Honor `Retry-After` |

## Authentication

Bearer tokens only (`Authorization: Bearer <access_token>`). The API never reads cookies, so it needs no CSRF protection.

1. `POST /auth/register` → `202`. The response is identical whether or not the email already exists. An existing owner is emailed instead.
2. User clicks the emailed link: `{APP_BASE_URL}/verify-email?token=…`. The client posts that token to `POST /auth/verify-email` and gets a token pair (auto-login). For native apps, configure universal links / app links for that path.
3. `POST /auth/login` → token pair. Unverified users can sign in, but every endpoint except `GET/DELETE /me`, `/me/resend-verification`, and `/auth/logout` returns `403 email_not_verified`.
4. Access tokens last 15 min. On `401 token_expired`, call `POST /auth/refresh` with the refresh token (30 days, not rotated) and retry. Any other `401` means sign in again.
5. Each login is a **session** (one per device). Logout or revoking a session invalidates its access and refresh tokens **immediately**.

Token pair response:

```json
{"token_type": "Bearer", "access_token": "…", "access_token_expires_at": "…",
 "refresh_token": "…", "refresh_token_expires_at": "…", "user": {…}}
```

Native apps: store the refresh token in Keychain / Android Keystore, and send `device_name` (e.g. "Sam's iPhone") on login/refresh/verify/google so the sessions list is readable.

For Google sign-in (`POST /auth/google`), the web uses Google Identity Services, iOS the GoogleSignIn SDK, and Android Credential Manager with Sign in with Google. Before signing in, apps call `GET /auth/config`, which returns the web client ID and the `google_sign_in` and `google_calendar` flags (`{"google_sign_in": bool, "google_calendar": bool, "google_client_id": string}`). When Google sign-in is not configured on the server, `google_sign_in` reports `false` and `POST /auth/google` responds with `404 not_found` without contacting Google. For `aud`, the server accepts the configured web client ID, plus optional iOS and Android client IDs when configured; when the iOS or Android client ID is not configured, only web tokens are accepted. Every failure (forged signature, expired token, untrusted issuer, unauthorized `aud`, unverified email, or an email conflict with an unverified account or a different Google identity) returns one generic error (`401 google_sign_in_failed`), revealing nothing about why, and creates no account or session.

## Endpoints

### Auth (public, rate limited per IP)

| Method & path | Body | Response |
|---|---|---|
| `GET /auth/config` | none | `{google_sign_in, google_calendar, google_client_id}` |
| `POST /auth/register` | `email, password, display_name` | `202` |
| `POST /auth/login` | `email, password, device_name?` | `200` token pair |
| `POST /auth/google` | `id_token, device_name?` | `200` token pair |
| `POST /auth/refresh` | `refresh_token, device_name?` | `200` token pair |
| `POST /auth/verify-email` | `token, device_name?` | `200` token pair |
| `POST /auth/forgot-password` | `email` | `202` (always) |
| `POST /auth/reset-password` | `token, password` | `204`; revokes all sessions |
| `POST /auth/logout` 🔒 | none | `204`; revokes the current session |

### Current user 🔒

| Method & path | Body | Response |
|---|---|---|
| `GET /me` | none | user |
| `PUT /me` | `display_name` | user |
| `POST /me/password` | `current_password, new_password` | `204`; signs out other sessions |
| `DELETE /me` | `password` | `204`. Deletes your account and the household it runs, with its family members. |
| `POST /me/resend-verification` | none | `202` (max 3/hour) |
| `GET /me/sessions` | none | `{items: [session]}`, `current: true` marks this device |
| `DELETE /me/sessions/{id}` | none | `204` |
| `GET /me/google-calendar` | none | `{status, access?}` |
| `POST /me/google-calendar/connect` | `code, platform?` | `{status, access?}` (verified bearer; generic 4xx on a bad code; `platform`: `web`, `ios`, or `android`; web exchanged with `postmessage`, iOS and Android with empty `redirect_uri`; rate limited to 30 per 15 minutes per IP, then `429`) |
| `DELETE /me/google-calendar` | none | `{status: "not_connected"}` |

To connect Google Calendar, apps obtain a one-time server authorization code and post it to `POST /me/google-calendar/connect`:
- **Web**: Google Identity Services code client in popup mode (exchanged with `postmessage`).
- **iOS**: `GoogleSignIn` with `serverClientID` set to the web client ID (`serverAuthCode`).
- **Android**: Identity Services Authorization API with `requestOfflineAccess(webClientId)`.

### Households 🔒

Each household has exactly one login account (its administrator); invites and login members no longer exist. Family members are household-scoped profiles with a name, an optional 4-digit PIN and points. A member is `{id, household_id, name, has_pin, points, created_at, updated_at}`; PINs are stored only as bcrypt hashes and never returned. While an admin PIN is set, every member-management write needs it in the `X-Admin-PIN` header (`403 admin_pin_required` if missing, `403 wrong_pin` if wrong, nothing changes). PIN routes are rate limited per login (120 per 10 minutes, then `429`). The first verified sign-in of an account with no household creates one, named "<display name>'s household", with that account as admin. Points award: `{id, kind, reason, points, earned_at}`. `kind` is `quest`, `combo`, `streak`, `admin`, or `reward`; `reason` is the quest name (e.g. `Feed the cat`), combo name, `streak: <quest name> <streak number>`, the admin's reason (`Adjusted by admin` when left blank), or `Redeemed: <description>`. `points` is signed for `admin` (e.g. `-5` for a removal) and `reward` (always negative, e.g. `-100`), and never negative otherwise.

| Method & path | Body | Notes |
|---|---|---|
| `GET /households` | none | Yours, with your `role` (`admin`) |
| `POST /households` | `name, timezone?` | Creator becomes admin. `timezone` is IANA (default `UTC`). |
| `GET /households/{id}` | none | |
| `PUT /households/{id}` | `name, timezone` | admin |
| `DELETE /households/{id}` | none | admin; deletes all quests and family members. The user's next sign-in creates a new household for them |
| `GET /households/{id}/family-members` | none | `{items: [member]}`; `{items: []}` when there are none. A member never carries a birthday or an age |
| `GET /households/{id}/family-members/birthdays` | none | admin; optional `X-Admin-PIN` header if set. `{items: [{member_id, birthday, age}]}`, one per member of this household only; `birthday` is `"YYYY-MM-DD"` or `null` when not set, `age` is the member's whole years today in the household's timezone, or `null` when no birthday is set; `{items: []}` when there are no members. `403 admin_pin_required` / `wrong_pin`; `404` for a non-member. Shares the PIN rate limit |
| `POST /households/{id}/family-members` | `name, pin?` | admin; optional `X-Admin-PIN` header if set |
| `GET /households/{id}/family-members/{memberId}` | none | member (never with a birthday or an age) |
| `PUT /households/{id}/family-members/{memberId}` | `name` | admin; optional `X-Admin-PIN` header if set |
| `DELETE /households/{id}/family-members/{memberId}` | none | admin; optional `X-Admin-PIN` header if set. Quests the member holds, including ones waiting for approval, go back to the pool; pending reward requests are cancelled |
| `PUT /households/{id}/family-members/{memberId}/pin` | `pin` | admin; 4-digit PIN; optional `X-Admin-PIN` header if set |
| `DELETE /households/{id}/family-members/{memberId}/pin` | none | admin; optional `X-Admin-PIN` header if set |
| `PUT /households/{id}/family-members/{memberId}/birthday` | `birthday` (`"YYYY-MM-DD"`, or `null` to clear it) | admin; optional `X-Admin-PIN` header if set. `200 {member_id, birthday, age}` (as in the birthdays list; both `null` once cleared). `422` on `birthday` for a malformed, empty or impossible date (`must be a valid date (YYYY-MM-DD)`) or a date after today in the household's timezone (`can't be in the future`; today is allowed); nothing is saved on a `422`. `403 admin_pin_required` / `wrong_pin`; `404` when the member is not in this household or the caller is not a member. Shares the PIN rate limit. A new birthday applies from the next accept or share: quests the member already holds or is on stay theirs |
| `POST /households/{id}/family-members/{memberId}/verify` | `pin` (empty for a member with no PIN) | `200` member, or `403 wrong_pin`. Sets no cookie or token: each later member action carries `member_id` and `pin` |
| `GET /households/{id}/family-members/{memberId}/points-history` | `?limit=1..100` (default 50), `?offset=` | Member or admin; sends `X-Member-PIN` or `X-Admin-PIN` header. The household admin needs no header when no admin PIN is set, whatever the member's own PIN. `{items: [points award]}` newest first; `{items: []}` when none; `403 wrong_pin`. Rate limited on the history budget it shares with `reward-history` and `reward-redemptions/pending` (every read counts), so a member pick that reads both histories spends two; only a wrong or missing PIN also counts against the PIN limit member actions share, so a correct read never uses that up |
| `GET /households/{id}/family-members/{memberId}/reward-redemptions/pending` | none | Member or admin; sends `X-Member-PIN` or `X-Admin-PIN` header (the household admin needs no header when no admin PIN is set). `{items: [redemption]}` oldest first; `{items: []}` when none; `403 wrong_pin`. Rate limited on the history budget it shares with `points-history` and `reward-history` (every read counts); only a wrong or missing PIN also counts against the PIN limit member actions share, so a correct read never uses that up |
| `GET /households/{id}/family-members/{memberId}/reward-history` | `?limit=1..100` (default 50), `?offset=` | Member or admin; sends `X-Member-PIN` or `X-Admin-PIN` header (the household admin needs no header when no admin PIN is set, whatever the member's own PIN). The member's approved redemptions only (pending, rejected and cancelled requests are left out): `{items: [redemption]}` newest approval first (`decided_at` is the date; `description` and `cost` are as snapshotted at request time, unchanged by a later edit or delete of the reward); `{items: []}` when none; `422` for a non-integer `limit`/`offset`; `403 wrong_pin`; `404` when the member is not in this household or the caller is not a member. Rate limited on the history budget it shares with `points-history` and `reward-redemptions/pending` (every read counts), so a member pick that reads both histories spends two; only a wrong or missing PIN also counts against the PIN limit member actions share, so a correct read never uses that up |
| `POST /households/{id}/family-members/{memberId}/points-adjustments` | `points, reason?` | admin; optional `X-Admin-PIN` header if set. `points` is a whole number, non-zero, at most 1000000 either way (negative removes); `reason` up to 200 characters, blank stored as `Adjusted by admin`. One transaction changes the total and adds a `kind: admin` points award. `200` member at the new total. `422` on `points` when a removal would take the total below 0 (`<name> only has <points> points — you can't remove <n>.`), nothing changes. PIN rate limited |
| `GET /households/{id}/admin-pin` | none | `{set: bool}` |
| `PUT /households/{id}/admin-pin` | `pin` | admin; 4-digit PIN; `X-Admin-PIN` header (current PIN) required if already set; `{set: true}` |
| `DELETE /households/{id}/admin-pin` | none | admin; `X-Admin-PIN` header required; `204` |
| `POST /households/{id}/admin-pin/verify` | `pin` | `200 {}`, or `403 wrong_pin` |

### Quests 🔒 — `/households/{id}/quests`, `/quest-library`

Quests replace chores: the old `/households/{id}/chores` routes are removed and answer `404 not_found`. Every existing chore was carried over into a quest with the same id (0 points, daily); the chore details stay in the database as a read-only archive.

Quest: `id, household_id, library_id, name, points, repeat_days, notes, streak_bonus, shareable, teamwork_bonus, shared, participants, available, claimed_by, claimed_at, claim_expires_at, awaiting_approval, completed_at, available_at, created_at, updated_at`. `claim_expires_at` is the deadline of a held quest: the UTC instant of the next local midnight in the household's timezone after the quest was first accepted, `null` when nobody holds it. It does not move when the quest is shared or when the holder hands it to another member on it, so the whole quest keeps the original accepter's day. `library_id` is set when the quest was copied from the library. `repeat_days` is the number of local days after the day the member marked it done before it is back at midnight (1 = daily, 30 = monthly). `streak_bonus` is the bonus points awarded on the 3rd completion in a row and every completion after (defaults to 0, which pays no streak bonus). `done_at` is removed from quest JSON. `awaiting_approval` is a household-scoped flag indicating that at least one completion for this quest is currently waiting for administrator approval. Sharing adds `shareable, teamwork_bonus, shared, participants`: `shareable` (bool) says whether the holder may share the quest; `teamwork_bonus` (whole number ≥ 0, always 0 when not shareable) is what each member of a shared completion earns on top of their split; `shared` is true while two or more members are on the quest; `participants` lists the member ids on it, earliest first, and is `[]` when it is not shared. Existing quests and quests copied from the library are not shareable and have a teamwork bonus of 0. Age ranges add `min_age, max_age, ineligible_member_ids`: `min_age` and `max_age` are whole numbers or `null`, `null` meaning no limit on that side, and both limits are inclusive (min 8 / max 12 is ages 8 through 12); both `null` is all ages. `ineligible_member_ids` lists the ids of this household's members whose age today, in the household's timezone, is outside the range; it is `[]` for an all-ages quest, and a member with no birthday never appears in it. Quest JSON never carries a birthday or an age. Existing quests and quests copied from the library are all ages.

Library quest: `id, name, points, repeat_days, notes, streak_bonus`. The library is the same for every household and read-only: there is no route that writes to it. Seeded library defaults are 5 for daily quests and 10 for less frequent quests.

| Method & path | Body / query | Notes |
|---|---|---|
| `GET /quest-library` | none | `{items: [library quest]}` |
| `GET /households/{id}/quests` | `?limit=1..100` (default 50), `?offset=` | Any member. `{items: [quest], next_offset}`; `{items: []}` when there are none |
| `POST /households/{id}/quests` | `name, points?, repeat_days?, notes?, streak_bonus?, shareable?, teamwork_bonus?, min_age?, max_age?` | admin; `X-Admin-PIN` header if set. `points` defaults to 0, `repeat_days` to 1, `streak_bonus` to 0, `shareable` to false, `teamwork_bonus` to 0, `min_age` and `max_age` to `null` (no limit). `201` quest |
| `POST /households/{id}/quests/from-library` | `library_id` | admin; `X-Admin-PIN` header if set. Copies the library quest's name, points, days, notes and streak bonus; editing the copy never changes the library. The copy is all ages (`min_age` and `max_age` `null`). `201` quest, `404` for an unknown `library_id` |
| `GET /households/{id}/quests/{questId}` | none | Any member |
| `PUT /households/{id}/quests/{questId}` | same as POST | admin; `X-Admin-PIN` header if set. Full replacement (omitted `shareable` is false, omitted `teamwork_bonus` is 0; the age range is replaced in full, so an omitted or `null` `min_age` / `max_age` clears that limit), except that an omitted (or `null`) `streak_bonus` keeps the quest's current streak bonus; the other omitted fields are cleared to their defaults. An invalid `streak_bonus` is still `422` |
| `DELETE /households/{id}/quests/{questId}` | none | admin; `X-Admin-PIN` header if set. `204` |
| `POST /households/{id}/quests/{questId}/accept` | `member_id, pin?` | The acting family member takes an available quest and has until the end of that day in the household's timezone (`claim_expires_at`, the next local midnight) to mark it done. `200` quest; `409 quest_taken` when another member holds it or it is waiting for its midnight; `409 age_restricted` when the quest is available but the member's age today (household timezone) is outside its range: nothing is written and the quest stays available. A member with no birthday, or a quest with no limits, always passes |
| `POST /households/{id}/quests/{questId}/abandon` | `member_id, pin?` | The holder abandons the quest, which returns it to the pool for everyone. On a shared quest any member on it can abandon: only they leave, and the quest stays with the others (see the lifecycle below). `200` quest; `404` when the member does not hold it. A quest abandoned by its last member returns to the pool with `claim_expires_at` `null`; when it stays with the others on a shared quest, `claim_expires_at` is unchanged |
| `POST /households/{id}/quests/{questId}/share` | `member_id, pin?, target_member_id, target_pin?` | The member holding a shareable quest adds another member of the same household, who confirms with their own PIN (`target_pin`, omitted or empty when they have none) as on accept. Only the holder can share; any number of members can be on one quest. `200` quest (`shared: true`, `participants` lists everyone on it). `422` on `member_id` / `target_member_id` when missing; `403 wrong_pin` for either wrong PIN (a wrong `target_pin` also carries `fields: {target_pin}`, so a client can tell it from the acting member's); `409 already_participant` when the target is the member themselves or already on it; `409 age_restricted` when the target's age today (household timezone) is outside the quest's range: nothing is written (a member with no birthday, or a quest with no limits, always passes; clients can tell an out-of-range target in advance from the quest's `ineligible_member_ids`); `404` when the quest is not shareable, not held by the member, its day has ended (`claim_expires_at` has passed: refused as for a quest the member no longer holds), or the quest or target is not in this household or does not exist. A refused share changes nothing |
| `POST /households/{id}/quests/{questId}/done` | `member_id, pin?` | The holder, or any member on a shared quest, marks the quest done; a shared quest completes for every member on it at once. In one transaction, the quest is released immediately back to the pool, its cooldown begins from now (`available_at` = local midnight in the household's timezone, `repeat_days` days after the day marked done), and a pending completion is recorded for administrator approval. Marking done also sets or updates the marking member's streak for this quest: streak expiry is set to local midnight after (local date of `done_at` + 2 × `repeat_days` + 1 day) in the household's timezone. If marked done before or on expiry, streak count increments by 1 (or starts at 1 if none); if marked done after expiry, streak count resets to 1. No points move yet. `200` quest; `404` when the member does not hold it, or when the quest's day has ended (`claim_expires_at` has passed, even if the automatic return to the pool has not run yet): no completion is recorded and no streak moves |
| `GET /households/{id}/quests/pending` | none | admin; `X-Admin-PIN` header if set. `{items: [{completion_id, quest_id, name, points, teamwork_bonus, member_id, member_name, done_at, members}]}` of completions waiting for approval, one item per mark-done (a shared quest's names every member), oldest `done_at` first. `member_id` and `member_name` are the member who marked it done; `members` is `[{member_id, member_name, points}]`, one entry per member on it with the split each will receive (one entry when not shared); `teamwork_bonus` is what each will get on top, 0 when not shared; `{items: []}` when nothing is waiting |
| `POST /households/{id}/quest-completions/{completionId}/approve` | none or `{}` | admin; `X-Admin-PIN` header if set. Approves one pending completion, credits every member recorded on it, pays earned combo bonuses to each, and pays the quest's streak bonus to the member who marked it done if their streak count for this quest is ≥ 3 and `streak_bonus` > 0. With n members, each gets the chore's current points divided by n, rounded up, plus the quest's current teamwork bonus when n is more than 1 (100 points, bonus 20: two members get 50 + 20 = 70 each, three get 34 + 20 = 54 each); a completion that is not shared pays its one member the full points and no bonus. All members are credited in one transaction or none are. `200 {completion, member, combo_bonuses, members, streak_bonus, streak_count}` where `completion` has `{id, quest_id, member_id, member_ids, done_at, approved_at, status}` (`member_ids` lists every member credited), `member` is the member who marked it done with their new total (all bonuses included), `combo_bonuses` is always present (`[]` when none) with entries `{combo_id, name, bonus_points}` paid to that member, `members` is `[{member, points_awarded, teamwork_bonus}]`, one per member credited with their new total, `streak_bonus` is the bonus points paid (0 when count < 3 or streak_bonus is 0), and `streak_count` is the marking member's streak count for the quest as fixed when this completion was marked done (0 for a completion marked done before streaks existed). Every points history entry an approval writes is earned at the approval time (eligibility still judged by the mark-done date): each member's split and their teamwork bonus each write a `quest` entry named after the quest, the streak bonus a `streak` entry and each combo bonus a `combo` entry. Points are credited once; repeat or concurrent approval returns `404`. Combos and streaks are judged by the completion's marked-done date in the household timezone, even if approved on a later day |
| `POST /households/{id}/quest-completions/{completionId}/reject` | none or `{}` | admin; `X-Admin-PIN` header if set. Rejects one pending completion without crediting points or any teamwork bonus to anyone on it. The rejected completion's streak effect is undone, even when the member marked the quest done again after it: the member's streak for the quest is recomputed from their remaining pending and approved completions in mark-done order (count and expiration as if the rejected one had never been marked done; the streak is removed when none remain). Their other pending completions of the quest are renumbered to match; approved ones keep their streak number and any bonus already paid. If that completion started the quest's running cooldown, the cooldown is cancelled and the chore returns to available immediately. Other completions and any current holder are untouched. `200 {completion: {id, quest_id, member_id, done_at, status: "rejected"}}`; `404` when not pending |
| `POST /households/{id}/quest-schedules` | `quest_id, member_id, date, repeat?` | admin; `X-Admin-PIN` header if set. Schedules the quest for the member on `date`, a household-local `YYYY-MM-DD` that is today or later in the household's timezone. `repeat` (boolean, default `false`) makes the schedule repeat from `date` on, every time the quest's cooldown ends (see Repeated schedules below); there is no interval field. `201` quest schedule. `422` with `fields.date` when `date` is missing, not a real `YYYY-MM-DD` date, or in the past; `404` when the quest or member is not in this household or does not exist (nothing is written). A schedule for today made after today's midnight pass has run is assigned at once |
| `GET /households/{id}/quest-schedules` | none | admin; `X-Admin-PIN` header if set. `{items: [quest schedule]}`, soonest `start_date` first; `{items: []}` when no quests are scheduled |
| `DELETE /households/{id}/quest-schedules/{scheduleId}` | none | admin; `X-Admin-PIN` header if set. Removes the schedule so it never fires again; a quest it already assigned stays with the member. `204`; `404` for a schedule not in this household |
| `GET /households/{id}/family-members/{memberId}/upcoming-quests` | none | Any member; no PIN, not PIN rate limited. The member's scheduled quests still to come: `{items: [upcoming quest]}`, soonest `next_date` first (then quest name); `{items: []}` when nothing is scheduled. A one-off whose day has passed is left out. `404` when the member is not in this household or the caller is not a member |

Quest lifecycle: available → `accept` (held by a member) → `done` (quest is released immediately; `available_at` = 00:00 in the household's timezone, `repeat_days` days after the day the member marked it done; pending completion created with `awaiting_approval: true` on the quest). A held quest lasts only until the end of the day it was accepted: at the household's local midnight (`claim_expires_at`), if it is not marked done, it is abandoned automatically by everyone on it and returns to the pool, available to everyone and able to be accepted again, with no points and no completion recorded for anyone, and any sharing cleared. The return runs within about a minute of midnight; from midnight on, `done` and `share` already answer `404`. Quests resting on a cooldown keep their `available_at`, and completions awaiting approval are untouched. A quest held when this rule was introduced returns to the pool at the first midnight after that. When the cooldown expires, the quest returns to available in the pool even while an earlier completion awaits approval — a member can complete a daily chore multiple days in a row without waiting for admin approval. Admin approves (`POST /households/{id}/quest-completions/{completionId}/approve`) crediting points, paying eligible combos as of the completion's mark-done date, and paying any streak bonus earned (from 3rd consecutive completion in a row), or rejects (`POST /households/{id}/quest-completions/{completionId}/reject`, undoing the completion's streak effect and cancelling the running cooldown if started by this completion, so the chore is available again right away). When a family member is removed from the household, their pending completions are cancelled with no points awarded; a chore resting on a cooldown that one of those completions started is available again right away, as on reject, and every other cooldown is left intact. On a shared quest only that member's part goes: abandoning, or being removed from the household, takes the member off a held quest (the earliest remaining member becomes the holder; once one member is left the quest is no longer shared, so no split and no bonus; it returns to the pool only when nobody is left), and removal takes the member off a pending shared completion (the earliest remaining member becomes its `member_id`), cancelling it only when nobody is left. No one's points move. The `000015` down migration refuses to run while any pending completion records more than one member: approve or send those back first. Declining is client-side only and writes nothing. Member actions name the acting family member in the body; a wrong member PIN is `403 wrong_pin`. On admin-gated actions a missing `X-Admin-PIN` (once one is set) is `403 admin_pin_required` and a wrong one `403 wrong_pin`; neither ever signs anyone out, and neither changes anything. Quest actions share the PIN rate limit.

Quest schedule: `id, quest_id, quest_name, member_id, member_name, start_date, repeat, created_at`. `start_date` is the household-local date (`YYYY-MM-DD`) the quest is assigned on (for a repeated schedule, the first date it can be); `repeat` is a boolean, `true` for a repeated schedule; `created_at` is UTC. Deleting the quest or the member deletes its schedules. Schedule routes are admin-only: a non-admin gets `403`, a missing `X-Admin-PIN` (once one is set) `403 admin_pin_required` and a wrong one `403 wrong_pin`, changing nothing; a non-member gets `404`. They share the PIN rate limit.

Upcoming quest: `id, quest_id, quest_name, start_date, next_date, repeat`. `id` is the schedule's id. `start_date` is the schedule's household-local date (`YYYY-MM-DD`); `next_date` is the household-local date (`YYYY-MM-DD`) of the next midnight pass that can assign it; `repeat` is whether the schedule repeats.

Midnight pass: `app.RunQuestRollover` runs at startup and then every minute, and rolls each household over once at its own local midnight. For each household, in one transaction: first the end-of-day rule above returns quests held past their deadline to the pool, then every schedule due that day assigns its quest to its member exactly as `accept` does (held by the member, `claim_expires_at` at the end of that local day), so a quest just returned can be assigned again at once. When on that day the quest is held by another member or resting on its cooldown (`available_at` in the future, including after a completion still awaiting approval), the day is skipped: nobody loses a held quest, the cooldown is kept, and a one-off schedule is finished and deleted. When the member's age that day is outside the quest's age range, the assignment is skipped and the schedule is kept. A failure for one household is logged with its cause and the other households still roll over. A midnight missed while the server was down runs once on restart, and running the pass twice never assigns twice.

Repeated schedules: a schedule with `repeat: true` is due at every local midnight from its `start_date` on, and the pass keeps it after it assigns the quest. It assigns the quest whenever the quest is free at that midnight: not held by anyone, not resting (`available_at` reached), and the member within the quest's age range that day. There is no interval field. The gap n between assignments is the quest's own `repeat_days`: once the member marks it done, its cooldown ends at the midnight n local days later, and that midnight's pass assigns it again (1 = every day, 7 = weekly). If the member abandoned it, or the end-of-day rule put it back in the pool unfinished, the next midnight assigns it again. A day it can't be assigned (held by another member, resting, or the member outside the age range) is skipped, the schedule is kept, and the next midnight tries again. A one-off schedule (`repeat: false`) is due only on its `start_date`. That day's pass deletes it, whether it assigned the quest or skipped the day. The one exception is a one-off skipped because the member was outside the age range: it is kept, as above. Removing a schedule (`DELETE`) stops all future assignments, and a quest it already assigned stays with the member.

Validation is per field and all at once: a `422` lists every bad field in `fields` — `name` (1–200 characters), `points` (a whole number ≥ 0; a non-numeric value is rejected), `repeat_days` (a whole number ≥ 1), `notes` (≤ 2000 characters), `streak_bonus` (a whole number ≥ 0; a non-numeric or negative value is rejected), `teamwork_bonus` (a whole number ≥ 0; a non-numeric value is rejected; a bonus above 0 while `shareable` is false is rejected), `min_age` and `max_age` (each a whole number ≥ 0 or `null`; a non-numeric, fractional or negative value is rejected on its own field; when both are set and `min_age` is above `max_age` the error is on `max_age` with the message `Minimum age can't be above the maximum age.`). Nothing is saved on a `422`. A non-member, or a quest id from another household, gets `404`. Quest writes share the PIN rate limit.

### Rewards 🔒 — `/households/{id}/rewards`, `/reward-library`

Rewards are a priced list created by the administrator or copied from the library. Each reward has a text description and a point cost. A member redeems a reward with their `member_id` and PIN: this records a pending request that keeps the reward's description and cost as they were at that moment, and their points do not change yet. A member may have several rewards pending at once, but only one request per reward. When the administrator approves, the cost is taken from the member's points once, in the same transaction, as a `reward` points award (`-<cost>`, reason `Redeemed: <description>`) that shows in the member's points history. If the member no longer has enough points, the approval is refused and the request stays pending so it can be rejected. Rejecting takes nothing. Removing a member cancels their pending requests. Editing or deleting a reward after the request leaves the request's description and cost as they were. Approved requests make up the member's reward history (`GET /households/{id}/family-members/{memberId}/reward-history`, read like points history). Nothing else here changes any family member's points, and there is no buy or spend route.

Redemption: `id, member_id, reward_id, description, cost, status, requested_at, decided_at`. `reward_id` is `null` once the reward has been deleted. `status` is `pending`, `approved`, `rejected` or `cancelled`. `decided_at` is `null` while pending. Items in the admin's pending list also carry `member_name`.

Reward: `id, household_id, library_id, description, cost, created_at, updated_at`. `library_id` is set when the reward was copied from the library.

Library reward: `id, description, cost`. The reward library is global, read-only and seeded with 6 starter ideas; it holds no household's own data and is ordered stably by cost then description.

| Method & path | Body | Notes |
|---|---|---|
| `GET /reward-library` | none | Global read-only library. `{items: [library reward]}` |
| `GET /households/{id}/rewards` | none | Any member. `{items: [reward]}`; `{items: []}` when there are none |
| `POST /households/{id}/rewards` | `description, cost` | admin; `X-Admin-PIN` header if set. `201` reward |
| `POST /households/{id}/rewards/from-library` | `library_id` | admin; `X-Admin-PIN` header if set. Copies the library reward's description and cost; editing or deleting the copy never changes the library. `201` reward, `404` for an unknown `library_id` |
| `GET /households/{id}/rewards/{rewardId}` | none | Any member |
| `PUT /households/{id}/rewards/{rewardId}` | `description, cost` | admin; `X-Admin-PIN` header if set. Full replacement |
| `DELETE /households/{id}/rewards/{rewardId}` | none | admin; `X-Admin-PIN` header if set. `204` |
| `POST /households/{id}/rewards/{rewardId}/redeem` | `member_id, pin?` | The acting family member asks to redeem the reward. `201` redemption (pending) for a new request; `200` with the existing request when this member already has this reward pending (no second request). `422` on `points` when the member has fewer points than the cost (`<name> has <points> points — <description> costs <cost>.`), and nothing is recorded. `403 wrong_pin`; `404` for an unknown reward, a member or reward from another household, or a non-member. PIN rate limited, like quest actions |
| `GET /households/{id}/reward-redemptions/pending` | none | admin; `X-Admin-PIN` header if set. `{items: [redemption with member_name]}`, oldest first; `{items: []}` when nothing is waiting. `403 admin_pin_required` / `wrong_pin`; `404` for a non-member. PIN rate limited |
| `POST /households/{id}/reward-redemptions/{redemptionId}/approve` | none or `{}` | admin; `X-Admin-PIN` header if set. Takes the request's cost from the member's points once and writes a `reward` points award. `200 {redemption, member}`, where `member` shows the new total. `422 insufficient_points` when the member now has fewer points than the cost, with the reason as `message` and on `points` (`Can't approve <description> for <name>: <name> has <points> points and it costs <cost>. Nothing was taken.`); no points move and the request stays pending. `404` when the request is already approved, rejected or cancelled (including by a repeat or simultaneous approve), or is from another household. `403 admin_pin_required` / `wrong_pin`, and nothing changes. PIN rate limited |
| `POST /households/{id}/reward-redemptions/{redemptionId}/reject` | none or `{}` | admin; `X-Admin-PIN` header if set. No points move and nothing is added to points history. `200 {redemption}` with `status: "rejected"`. `404` when the request is not pending or is from another household. `403 admin_pin_required` / `wrong_pin`. PIN rate limited |

Validation is per field: a `422` lists bad fields in `fields` — `description` (1–200 characters after trimming), `cost` (required whole number ≥ 0 that fits int32; non-numeric or fractional values rejected). On `/from-library`, a missing `library_id` returns `422`. Nothing is saved on a `422`. Every household reward operation checks household membership first: non-members or a reward id from another household return `404 not_found`. While an admin PIN is set, write endpoints require `X-Admin-PIN` (`403 admin_pin_required` if missing, `403 wrong_pin` if wrong). Reward writes share the PIN rate limit.

### Combos 🔒 — `/households/{id}/combos`

Combos associate two or more chores with one another: completing all chores of a combo on the same day earns a bonus point payout for the member who completed them.

Combo: `id, name, bonus_points, quest_ids`.

| Method & path | Body | Notes |
|---|---|---|
| `GET /households/{id}/combos` | none | Any member. `{items: [combo]}`; `{items: []}` when there are none |
| `POST /households/{id}/combos` | `name, bonus_points, quest_ids` | admin; `X-Admin-PIN` header if set. `201` combo |
| `GET /households/{id}/combos/{comboId}` | none | Any member. `200` combo |
| `PUT /households/{id}/combos/{comboId}` | `name, bonus_points, quest_ids` | admin; `X-Admin-PIN` header if set. Full replacement. `200` combo |
| `DELETE /households/{id}/combos/{comboId}` | none | admin; `X-Admin-PIN` header if set. `204` |

Validation is per field: a `422` lists bad fields in `fields` — `name` (1–200 characters after trimming), `bonus_points` (required whole number ≥ 0 that fits int32; non-numeric or fractional values rejected), `quest_ids` (array of at least 2 distinct existing household chore IDs). Nothing is saved on a `422`.

Every household combo operation checks household membership first: non-members or a combo id from another household return `404 not_found`. While an admin PIN is set, write endpoints require `X-Admin-PIN` (`403 admin_pin_required` if missing, `403 wrong_pin` if wrong). Combo writes share the PIN rate limit.

How bonuses are earned:
- A chore counts toward a combo's day by when the member marked it done, judged on the household's local clock (its timezone).
- A bonus is paid for each combo whose full set of chores was marked done by the same member on the same household day. The bonus is paid at the approval of the last chore, even if approvals come on a later day.
- A combo pays again every time a member completes its full set again on a household day. Each set of completions pays its bonus at most once; repeat or concurrent approvals never pay twice.
- A chore may belong to several combos; completing it can finish several combos and pay each bonus.
- Chores marked done on different household days, or by different members, pay no bonus (only chore points move).
- Editing or deleting a combo never changes bonuses already paid; changes affect only later completions. A combo that ends up with fewer than two chores (e.g. after deleting a chore) pays nothing.
- Completions marked done before this release are not counted toward combo bonuses.

### Family calendar 🔒 — `/households/{id}/calendar`, `/households/{id}/events`

One member who has connected Google Calendar picks one of their own calendars as the household's family calendar. Every member then sees its entries, which the server fetches with that member's Google token. Chorus has no route that creates, moves or deletes an entry. Its one write to Google is an entry's title, when a member tags it: the tagged members' display names go in one trailing group, e.g. `Dentist [Mom, Sam]`. Chorus stores no title of its own. The family calendar is cleared when its member disconnects Google or deletes their account.

`status` is `ok`, `not_configured` (none chosen), `broken` (the connecting member must reconnect) or `unavailable` (Google Calendar not configured on this server).

| Method & path | Body / query | Notes |
|---|---|---|
| `GET /me/google-calendar/calendars` | none | Caller's own calendars: `{status, items: [{id, name, primary}]}` |
| `GET /households/{id}/calendar` | none | `{status, calendar_id?, calendar_name?, set_by?: {id, display_name}}` |
| `PUT /households/{id}/calendar` | `calendar_id` | Must be on the caller's own calendar list (else 422). Only the member who set it may change it (else 403) |
| `DELETE /households/{id}/calendar` | none | 204. Only the member who set it (else 403) |
| `GET /households/{id}/events` | `?from=&to=` (RFC 3339, required, ≤ 100 days) | `{status, items: [{id, title, start, end, all_day, location, tags, can_tag}], members: [{member_id, name, taggable, reason?}]}`. `start`/`end` are RFC 3339, or `YYYY-MM-DD` when `all_day`. `title` is Google's; `tags` are the member ids read from its trailing `[..]` group. `can_tag` needs a read/write connection and an entry Google reports editable. A family member is not taggable (`reason` `duplicate_name` or `reserved_character`) when their name matches another member's case-insensitively or contains `,`, `[` or `]`. Recurring entries come back as occurrences. 503 `google_unavailable` if Google fails temporarily |
| `PUT /households/{id}/calendar/entries/{eventID}/tags` | `tags` (family member ids; `[]` untags) | Any member. Sends Google only the new title, with the connecting member's token, and answers `{id, title, tags, can_tag}` with the title Google stored. Names in the group that aren't members are kept after the tagged ones. 422 for a tag that isn't a taggable member, 403 when the entry can't be tagged (`can_tag` false), 404 `not_found` with no family calendar. If Google refuses: 404 `entry_gone`, 403 `not_editable`, 502 `google_rejected` |

### Operational

`GET /health` (liveness) and `GET /ready` (checks the DB) live outside `/api`.
