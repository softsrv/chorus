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
| 400 | `invalid_json`, `invalid_token` | Malformed request or bad emailed token |
| 401 | `unauthenticated`, `token_expired`, `invalid_credentials`, `invalid_refresh_token` | See auth flow below |
| 403 | `email_not_verified`, `forbidden`, `wrong_pin`, `admin_pin_required` | `wrong_pin`: a member or admin PIN did not match. `admin_pin_required`: an admin PIN is set and the `X-Admin-PIN` header was missing. Neither signs you out. |
| 404 | `not_found` | |
| 409 | `already_verified` | Email is already verified |
| 413 / 415 | `body_too_large` / `unsupported_media_type` | |
| 422 | `validation_failed` | `fields` maps field → message |
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

Native apps: store the refresh token in Keychain / Android Keystore, and send `device_name` (e.g. "Sam's iPhone") on login/refresh/verify so the sessions list is readable.

## Endpoints

### Auth (public, rate limited per IP)

| Method & path | Body | Response |
|---|---|---|
| `POST /auth/register` | `email, password, display_name` | `202` |
| `POST /auth/login` | `email, password, device_name?` | `200` token pair |
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

### Households 🔒

Each household has exactly one login account (its administrator); invites and login members no longer exist. Family members are household-scoped profiles with a name, an optional 4-digit PIN and points. A member is `{id, household_id, name, has_pin, points, created_at, updated_at}`; PINs are stored only as bcrypt hashes and never returned. While an admin PIN is set, every member-management write needs it in the `X-Admin-PIN` header (`403 admin_pin_required` if missing, `403 wrong_pin` if wrong, nothing changes). PIN routes are rate limited per login (120 per 10 minutes, then `429`). The first verified sign-in of an account with no household creates one, named "<display name>'s household", with that account as admin. Points award: `{id, kind, reason, points, earned_at}`. `kind` is `quest`, `combo`, or `streak`; `reason` is the quest name (e.g. `Feed the cat`), combo name, or `streak: <quest name> <streak number>`.

| Method & path | Body | Notes |
|---|---|---|
| `GET /households` | none | Yours, with your `role` (`admin`) |
| `POST /households` | `name, timezone?` | Creator becomes admin. `timezone` is IANA (default `UTC`). |
| `GET /households/{id}` | none | |
| `PUT /households/{id}` | `name, timezone` | admin |
| `DELETE /households/{id}` | none | admin; deletes all quests and family members. The user's next sign-in creates a new household for them |
| `GET /households/{id}/family-members` | none | `{items: [member]}`; `{items: []}` when there are none |
| `POST /households/{id}/family-members` | `name, pin?` | admin; optional `X-Admin-PIN` header if set |
| `GET /households/{id}/family-members/{memberId}` | none | member |
| `PUT /households/{id}/family-members/{memberId}` | `name` | admin; optional `X-Admin-PIN` header if set |
| `DELETE /households/{id}/family-members/{memberId}` | none | admin; optional `X-Admin-PIN` header if set. Quests the member holds, including ones waiting for approval, go back to the pool |
| `PUT /households/{id}/family-members/{memberId}/pin` | `pin` | admin; 4-digit PIN; optional `X-Admin-PIN` header if set |
| `DELETE /households/{id}/family-members/{memberId}/pin` | none | admin; optional `X-Admin-PIN` header if set |
| `POST /households/{id}/family-members/{memberId}/verify` | `pin` (empty for a member with no PIN) | `200` member, or `403 wrong_pin`. Sets no cookie or token: each later member action carries `member_id` and `pin` |
| `GET /households/{id}/family-members/{memberId}/points-history` | `?limit=1..100` (default 50), `?offset=` | Member or admin; sends `X-Member-PIN` or `X-Admin-PIN` header. `{items: [points award]}` newest first; `{items: []}` when none; `403 wrong_pin`. PIN rate limited on a budget of its own, so reading it never uses up the limit member actions share |
| `GET /households/{id}/admin-pin` | none | `{set: bool}` |
| `PUT /households/{id}/admin-pin` | `pin` | admin; 4-digit PIN; `X-Admin-PIN` header (current PIN) required if already set; `{set: true}` |
| `DELETE /households/{id}/admin-pin` | none | admin; `X-Admin-PIN` header required; `204` |
| `POST /households/{id}/admin-pin/verify` | `pin` | `200 {}`, or `403 wrong_pin` |

### Quests 🔒 — `/households/{id}/quests`, `/quest-library`

Quests replace chores: the old `/households/{id}/chores` routes are removed and answer `404 not_found`. Every existing chore was carried over into a quest with the same id (0 points, daily); the chore details stay in the database as a read-only archive.

Quest: `id, household_id, library_id, name, points, repeat_days, notes, streak_bonus, shareable, teamwork_bonus, shared, participants, available, claimed_by, claimed_at, awaiting_approval, completed_at, available_at, created_at, updated_at`. `library_id` is set when the quest was copied from the library. `repeat_days` is the number of local days after the day the member marked it done before it is back at midnight (1 = daily, 30 = monthly). `streak_bonus` is the bonus points awarded on the 3rd completion in a row and every completion after (defaults to 0, which pays no streak bonus). `done_at` is removed from quest JSON. `awaiting_approval` is a household-scoped flag indicating that at least one completion for this quest is currently waiting for administrator approval. Sharing adds `shareable, teamwork_bonus, shared, participants`: `shareable` (bool) says whether the holder may share the quest; `teamwork_bonus` (whole number ≥ 0, always 0 when not shareable) is what each member of a shared completion earns on top of their split; `shared` is true while two or more members are on the quest; `participants` lists the member ids on it, earliest first, and is `[]` when it is not shared. Existing quests and quests copied from the library are not shareable and have a teamwork bonus of 0.

Library quest: `id, name, points, repeat_days, notes, streak_bonus`. The library is the same for every household and read-only: there is no route that writes to it. Seeded library defaults are 5 for daily quests and 10 for less frequent quests.

| Method & path | Body / query | Notes |
|---|---|---|
| `GET /quest-library` | none | `{items: [library quest]}` |
| `GET /households/{id}/quests` | `?limit=1..100` (default 50), `?offset=` | Any member. `{items: [quest], next_offset}`; `{items: []}` when there are none |
| `POST /households/{id}/quests` | `name, points?, repeat_days?, notes?, streak_bonus?, shareable?, teamwork_bonus?` | admin; `X-Admin-PIN` header if set. `points` defaults to 0, `repeat_days` to 1, `streak_bonus` to 0, `shareable` to false, `teamwork_bonus` to 0. `201` quest |
| `POST /households/{id}/quests/from-library` | `library_id` | admin; `X-Admin-PIN` header if set. Copies the library quest's name, points, days, notes and streak bonus; editing the copy never changes the library. `201` quest, `404` for an unknown `library_id` |
| `GET /households/{id}/quests/{questId}` | none | Any member |
| `PUT /households/{id}/quests/{questId}` | same as POST | admin; `X-Admin-PIN` header if set. Full replacement (omitted `shareable` is false, omitted `teamwork_bonus` is 0), except that an omitted (or `null`) `streak_bonus` keeps the quest's current streak bonus; the other omitted fields are cleared to their defaults. An invalid `streak_bonus` is still `422` |
| `DELETE /households/{id}/quests/{questId}` | none | admin; `X-Admin-PIN` header if set. `204` |
| `POST /households/{id}/quests/{questId}/accept` | `member_id, pin?` | The acting family member takes an available quest. `200` quest; `409 quest_taken` when another member holds it or it is waiting for its midnight |
| `POST /households/{id}/quests/{questId}/abandon` | `member_id, pin?` | The holder abandons the quest, which returns it to the pool for everyone. On a shared quest any member on it can abandon: only they leave, and the quest stays with the others (see the lifecycle below). `200` quest; `404` when the member does not hold it |
| `POST /households/{id}/quests/{questId}/share` | `member_id, pin?, target_member_id, target_pin?` | The member holding a shareable quest adds another member of the same household, who confirms with their own PIN (`target_pin`, omitted or empty when they have none) as on accept. Only the holder can share; any number of members can be on one quest. `200` quest (`shared: true`, `participants` lists everyone on it). `422` on `member_id` / `target_member_id` when missing; `403 wrong_pin` for either wrong PIN (a wrong `target_pin` also carries `fields: {target_pin}`, so a client can tell it from the acting member's); `409 already_participant` when the target is the member themselves or already on it; `404` when the quest is not shareable, not held by the member, or the quest or target is not in this household or does not exist. A refused share changes nothing |
| `POST /households/{id}/quests/{questId}/done` | `member_id, pin?` | The holder, or any member on a shared quest, marks the quest done; a shared quest completes for every member on it at once. In one transaction, the quest is released immediately back to the pool, its cooldown begins from now (`available_at` = local midnight in the household's timezone, `repeat_days` days after the day marked done), and a pending completion is recorded for administrator approval. Marking done also sets or updates the marking member's streak for this quest: streak expiry is set to local midnight after (local date of `done_at` + 2 × `repeat_days` + 1 day) in the household's timezone. If marked done before or on expiry, streak count increments by 1 (or starts at 1 if none); if marked done after expiry, streak count resets to 1. No points move yet. `200` quest; `404` when the member does not hold it |
| `GET /households/{id}/quests/pending` | none | admin; `X-Admin-PIN` header if set. `{items: [{completion_id, quest_id, name, points, teamwork_bonus, member_id, member_name, done_at, members}]}` of completions waiting for approval, one item per mark-done (a shared quest's names every member), oldest `done_at` first. `member_id` and `member_name` are the member who marked it done; `members` is `[{member_id, member_name, points}]`, one entry per member on it with the split each will receive (one entry when not shared); `teamwork_bonus` is what each will get on top, 0 when not shared; `{items: []}` when nothing is waiting |
| `POST /households/{id}/quest-completions/{completionId}/approve` | none or `{}` | admin; `X-Admin-PIN` header if set. Approves one pending completion, credits every member recorded on it, pays earned combo bonuses to each, and pays the quest's streak bonus to the member who marked it done if their streak count for this quest is ≥ 3 and `streak_bonus` > 0. With n members, each gets the chore's current points divided by n, rounded up, plus the quest's current teamwork bonus when n is more than 1 (100 points, bonus 20: two members get 50 + 20 = 70 each, three get 34 + 20 = 54 each); a completion that is not shared pays its one member the full points and no bonus. All members are credited in one transaction or none are. `200 {completion, member, combo_bonuses, members, streak_bonus, streak_count}` where `completion` has `{id, quest_id, member_id, member_ids, done_at, approved_at, status}` (`member_ids` lists every member credited), `member` is the member who marked it done with their new total (all bonuses included), `combo_bonuses` is always present (`[]` when none) with entries `{combo_id, name, bonus_points}` paid to that member, `members` is `[{member, points_awarded, teamwork_bonus}]`, one per member credited with their new total, `streak_bonus` is the bonus points paid (0 when count < 3 or streak_bonus is 0), and `streak_count` is the marking member's streak count for the quest as fixed when this completion was marked done (0 for a completion marked done before streaks existed). Every points history entry an approval writes is earned at the approval time (eligibility still judged by the mark-done date): each member's split and their teamwork bonus each write a `quest` entry named after the quest, the streak bonus a `streak` entry and each combo bonus a `combo` entry. Points are credited once; repeat or concurrent approval returns `404`. Combos and streaks are judged by the completion's marked-done date in the household timezone, even if approved on a later day |
| `POST /households/{id}/quest-completions/{completionId}/reject` | none or `{}` | admin; `X-Admin-PIN` header if set. Rejects one pending completion without crediting points or any teamwork bonus to anyone on it. The rejected completion's streak effect is undone, even when the member marked the quest done again after it: the member's streak for the quest is recomputed from their remaining pending and approved completions in mark-done order (count and expiration as if the rejected one had never been marked done; the streak is removed when none remain). Their other pending completions of the quest are renumbered to match; approved ones keep their streak number and any bonus already paid. If that completion started the quest's running cooldown, the cooldown is cancelled and the chore returns to available immediately. Other completions and any current holder are untouched. `200 {completion: {id, quest_id, member_id, done_at, status: "rejected"}}`; `404` when not pending |

Quest lifecycle: available → `accept` (held by a member) → `done` (quest is released immediately; `available_at` = 00:00 in the household's timezone, `repeat_days` days after the day the member marked it done; pending completion created with `awaiting_approval: true` on the quest). When the cooldown expires, the quest returns to available in the pool even while an earlier completion awaits approval — a member can complete a daily chore multiple days in a row without waiting for admin approval. Admin approves (`POST /households/{id}/quest-completions/{completionId}/approve`) crediting points, paying eligible combos as of the completion's mark-done date, and paying any streak bonus earned (from 3rd consecutive completion in a row), or rejects (`POST /households/{id}/quest-completions/{completionId}/reject`, undoing the completion's streak effect and cancelling the running cooldown if started by this completion, so the chore is available again right away). When a family member is removed from the household, their pending completions are cancelled with no points awarded; a chore resting on a cooldown that one of those completions started is available again right away, as on reject, and every other cooldown is left intact. On a shared quest only that member's part goes: abandoning, or being removed from the household, takes the member off a held quest (the earliest remaining member becomes the holder; once one member is left the quest is no longer shared, so no split and no bonus; it returns to the pool only when nobody is left), and removal takes the member off a pending shared completion (the earliest remaining member becomes its `member_id`), cancelling it only when nobody is left. No one's points move. The `000015` down migration refuses to run while any pending completion records more than one member: approve or send those back first. Declining is client-side only and writes nothing. Member actions name the acting family member in the body; a wrong member PIN is `403 wrong_pin`. On admin-gated actions a missing `X-Admin-PIN` (once one is set) is `403 admin_pin_required` and a wrong one `403 wrong_pin`; neither ever signs anyone out, and neither changes anything. Quest actions share the PIN rate limit.

Validation is per field and all at once: a `422` lists every bad field in `fields` — `name` (1–200 characters), `points` (a whole number ≥ 0; a non-numeric value is rejected), `repeat_days` (a whole number ≥ 1), `notes` (≤ 2000 characters), `streak_bonus` (a whole number ≥ 0; a non-numeric or negative value is rejected), `teamwork_bonus` (a whole number ≥ 0; a non-numeric value is rejected; a bonus above 0 while `shareable` is false is rejected). Nothing is saved on a `422`. A non-member, or a quest id from another household, gets `404`. Quest writes share the PIN rate limit.

### Rewards 🔒 — `/households/{id}/rewards`, `/reward-library`

Rewards are a priced list created by the administrator or copied from the library. Each reward has a text description and a point cost. Rewards cannot be redeemed, bought or spent yet: no redemption or spending route exists, and no reward action changes any family member's points.

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
