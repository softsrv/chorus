# Chorus API v1

The contract the iOS/Android apps and the web test client build against. Base path: `/api/v1`.

## Conventions

- **JSON only.** Request bodies require `Content-Type: application/json`. Unknown fields, trailing data, and bodies over 64 KiB are rejected.
- **IDs** are UUIDv7 strings. **Timestamps** are RFC 3339 in UTC (`2026-11-05T18:00:00Z`).
- **PUT is full replacement.** An omitted optional field is cleared. Send the whole object.
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

Each household has exactly one login account (its administrator); invites and login members no longer exist. Family members are household-scoped profiles with a name, an optional 4-digit PIN and points. A member is `{id, household_id, name, has_pin, points, created_at, updated_at}`; PINs are stored only as bcrypt hashes and never returned. While an admin PIN is set, every member-management write needs it in the `X-Admin-PIN` header (`403 admin_pin_required` if missing, `403 wrong_pin` if wrong, nothing changes). PIN routes are rate limited per login (120 per 10 minutes, then `429`). The first verified sign-in of an account with no household creates one, named "<display name>'s household", with that account as admin.

| Method & path | Body | Notes |
|---|---|---|
| `GET /households` | none | Yours, with your `role` (`admin`) |
| `POST /households` | `name, timezone?` | Creator becomes admin. `timezone` is IANA (default `UTC`). |
| `GET /households/{id}` | none | |
| `PUT /households/{id}` | `name, timezone` | admin |
| `DELETE /households/{id}` | none | admin; deletes all quests and family members |
| `GET /households/{id}/family-members` | none | `{items: [member]}`; `{items: []}` when there are none |
| `POST /households/{id}/family-members` | `name, pin?` | admin; optional `X-Admin-PIN` header if set |
| `GET /households/{id}/family-members/{memberId}` | none | member |
| `PUT /households/{id}/family-members/{memberId}` | `name` | admin; optional `X-Admin-PIN` header if set |
| `DELETE /households/{id}/family-members/{memberId}` | none | admin; optional `X-Admin-PIN` header if set |
| `PUT /households/{id}/family-members/{memberId}/pin` | `pin` | admin; 4-digit PIN; optional `X-Admin-PIN` header if set |
| `DELETE /households/{id}/family-members/{memberId}/pin` | none | admin; optional `X-Admin-PIN` header if set |
| `POST /households/{id}/family-members/{memberId}/verify` | `pin` (empty for a member with no PIN) | `200` member, or `403 wrong_pin`. Sets no cookie or token: each later member action carries `member_id` and `pin` |
| `GET /households/{id}/admin-pin` | none | `{set: bool}` |
| `PUT /households/{id}/admin-pin` | `pin` | admin; 4-digit PIN; `X-Admin-PIN` header (current PIN) required if already set; `{set: true}` |
| `DELETE /households/{id}/admin-pin` | none | admin; `X-Admin-PIN` header required; `204` |
| `POST /households/{id}/admin-pin/verify` | `pin` | `200 {}`, or `403 wrong_pin` |

### Quests 🔒 — `/households/{id}/quests`, `/quest-library`

Quests replace chores: the old `/households/{id}/chores` routes are removed and answer `404 not_found`. Every existing chore was carried over into a quest with the same id (0 points, daily); the chore details stay in the database as a read-only archive.

Quest: `id, household_id, library_id, name, points, repeat_days, notes, available, claimed_by, claimed_at, done_at, awaiting_approval, completed_at, available_at, created_at, updated_at`. `library_id` is set when the quest was copied from the library. `repeat_days` is the number of days until the quest is available again after completion (1 = daily, 30 = monthly).

Library quest: `id, name, points, repeat_days, notes`. The library is the same for every household and read-only: there is no route that writes to it.

| Method & path | Body / query | Notes |
|---|---|---|
| `GET /quest-library` | none | `{items: [library quest]}` |
| `GET /households/{id}/quests` | `?limit=1..100` (default 50), `?offset=` | Any member. `{items: [quest], next_offset}`; `{items: []}` when there are none |
| `POST /households/{id}/quests` | `name, points?, repeat_days?, notes?` | admin; `X-Admin-PIN` header if set. `points` defaults to 0, `repeat_days` to 1. `201` quest |
| `POST /households/{id}/quests/from-library` | `library_id` | admin; `X-Admin-PIN` header if set. Copies the library quest's name, points, days and notes; editing the copy never changes the library. `201` quest, `404` for an unknown `library_id` |
| `GET /households/{id}/quests/{questId}` | none | Any member |
| `PUT /households/{id}/quests/{questId}` | same as POST | admin; `X-Admin-PIN` header if set. Full replacement |
| `DELETE /households/{id}/quests/{questId}` | none | admin; `X-Admin-PIN` header if set. `204` |
| `POST /households/{id}/quests/{questId}/accept` | `member_id, pin?` | The acting family member takes an available quest. `200` quest; `409 quest_taken` when another member holds it or it is waiting out its repeat days |
| `POST /households/{id}/quests/{questId}/abandon` | `member_id, pin?` | The holder puts the quest back in the pool for everyone. `200` quest; `404` when the member does not hold it or it is waiting for approval |
| `POST /households/{id}/quests/{questId}/done` | `member_id, pin?` | The holder marks the quest done; it waits for the administrator and no points move yet. `200` quest; `404` when the member does not hold it or it is already waiting |
| `GET /households/{id}/quests/pending` | none | admin; `X-Admin-PIN` header if set. `{items: [quest + member_name]}` of quests waiting for approval, oldest first; `{items: []}` when nothing is waiting |
| `POST /households/{id}/quests/{questId}/approve` | none or `{}` | admin; `X-Admin-PIN` header if set. Credits the quest's points to its holder in the same transaction, releases the quest and starts its repeat countdown. `200 {quest, member}` with the member's new `points`; `404` when the quest is not waiting (so a repeated approval credits nothing) |
| `POST /households/{id}/quests/{questId}/reject` | none or `{}` | admin; `X-Admin-PIN` header if set. Sends the quest back: it stays with the same member, unfinished, no points. `200` quest; `404` when the quest is not waiting |

Quest lifecycle: available → `accept` (held by a member) → `done` (`awaiting_approval: true`) → `approve` (points credited; `available_at` = approval time + `repeat_days`) or `reject` (back to held). Declining is client-side only and writes nothing. Member actions name the acting family member in the body; a wrong member PIN is `403 wrong_pin`. On admin-gated actions a missing `X-Admin-PIN` (once one is set) is `403 admin_pin_required` and a wrong one `403 wrong_pin`; neither ever signs anyone out, and neither changes anything. Quest actions share the PIN rate limit.

Validation is per field and all at once: a `422` lists every bad field in `fields` — `name` (1–200 characters), `points` (a whole number ≥ 0; a non-numeric value is rejected), `repeat_days` (a whole number ≥ 1), `notes` (≤ 2000 characters). Nothing is saved on a `422`. A non-member, or a quest id from another household, gets `404`. Quest writes share the PIN rate limit.

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
