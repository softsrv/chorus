# Chorus API v1

The contract the iOS/Android apps and the web test client build against. Base path: `/api/v1`.

## Conventions

- **JSON only.** Request bodies require `Content-Type: application/json`. Unknown fields, trailing data, and bodies over 64 KiB are rejected.
- **IDs** are UUIDv7 strings. **Timestamps** are RFC 3339 in UTC (`2026-11-05T18:00:00Z`). **Dates** (chore due dates) are `YYYY-MM-DD`.
- **PUT is full replacement.** An omitted optional field is cleared. Send the whole object.
- **Lists** return `{"items": [...]}`. Paginated lists also return `next_offset` (`null` on the last page).
- **Not found vs. forbidden.** Resources in a household you don't belong to return `404`, the same as nonexistent ones. `403 forbidden` means you're a member but lack the role (e.g. non-admin creating an invite).
- **Errors** always use this shape. Branch on `code`. `message` is for humans and may change.

```json
{"error": {"code": "validation_failed", "message": "One or more fields are invalid.", "fields": {"title": "is required"}}}
```

| Status | `code` | Meaning |
|---|---|---|
| 400 | `invalid_json`, `invalid_token`, `invalid_invite` | Malformed request, bad emailed token, or bad invite code |
| 401 | `unauthenticated`, `token_expired`, `invalid_credentials`, `invalid_refresh_token` | See auth flow below |
| 403 | `email_not_verified`, `forbidden` | |
| 404 | `not_found` | |
| 409 | `last_admin`, `sole_admin`, `already_member`, `already_verified` | Conflicts with household rules |
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
| `DELETE /me` | `password` | `204`. `409 sole_admin` if you're the only admin of a household with other members. Households where you're the only member are deleted. |
| `POST /me/resend-verification` | none | `202` (max 3/hour) |
| `GET /me/sessions` | none | `{items: [session]}`, `current: true` marks this device |
| `DELETE /me/sessions/{id}` | none | `204` |

### Households 🔒

Roles: `admin` (manage household, members, invites) and `member`. Every member can manage chores and sees the household's family calendar.

| Method & path | Body | Notes |
|---|---|---|
| `GET /households` | none | Yours, with your `role` |
| `POST /households` | `name, timezone?` | Creator becomes admin. `timezone` is IANA (default `UTC`). |
| `GET /households/{id}` | none | |
| `PUT /households/{id}` | `name, timezone` | admin |
| `DELETE /households/{id}` | none | admin; deletes all chores/events |
| `GET /households/{id}/members` | none | `{user_id, display_name, role, joined_at}` |
| `PUT /households/{id}/members/{userId}` | `role` | admin; `409 last_admin` if demoting the last admin |
| `DELETE /households/{id}/members/{userId}` | none | admin, or yourself to leave. The last admin can't leave while others remain. The last member leaving deletes the household. |
| `POST /households/{id}/invites` | none | admin. `{code: "XXXX-XXXX-XXXX", expires_at}`, single use, 7 days |
| `POST /invites/accept` | `code` | Case and dashes don't matter. 10 attempts/hour per user. |

### Chores 🔒 — `/households/{id}/chores`

Chore: `id, household_id, title, description, assignee_id, due_on, completed, completed_at, completed_by, created_by, created_at, updated_at`

| Method & path | Body / query | Notes |
|---|---|---|
| `GET /` | `?status=open\|done\|all` (default `open`), `?assignee=me\|<userId>`, `?limit=1..100` (default 50), `?offset=` | Ordered by due date (undated last) |
| `POST /` | `title, description?, assignee_id?, due_on?` | Assignee must be a member (`422`). If the assignee leaves, the chore becomes unassigned. |
| `GET /{choreId}` | none | |
| `PUT /{choreId}` | same as POST | Full replacement |
| `DELETE /{choreId}` | none | |
| `PUT /{choreId}/completion` | none | Mark done |
| `DELETE /{choreId}/completion` | none | Reopen |

### Family calendar 🔒 — `/households/{id}/calendar`, `/households/{id}/events`

One member who has connected Google Calendar picks one of their own calendars as the household's family calendar. Every member then sees its entries, which the server fetches with that member's Google token. Chorus has no route that creates, moves or deletes an entry. Its one write to Google is an entry's title, when a member tags it: the tagged members' display names go in one trailing group, e.g. `Dentist [Mom, Sam]`. Chorus stores no title of its own. The family calendar is cleared when its member disconnects Google, leaves or is removed, or deletes their account.

`status` is `ok`, `not_configured` (none chosen), `broken` (the connecting member must reconnect) or `unavailable` (Google Calendar not configured on this server).

| Method & path | Body / query | Notes |
|---|---|---|
| `GET /me/google-calendar/calendars` | none | Caller's own calendars: `{status, items: [{id, name, primary}]}` |
| `GET /households/{id}/calendar` | none | `{status, calendar_id?, calendar_name?, set_by?: {id, display_name}}` |
| `PUT /households/{id}/calendar` | `calendar_id` | Must be on the caller's own calendar list (else 422). Only the member who set it may change it (else 403) |
| `DELETE /households/{id}/calendar` | none | 204. Only the member who set it (else 403) |
| `GET /households/{id}/events` | `?from=&to=` (RFC 3339, required, ≤ 100 days) | `{status, items: [{id, title, start, end, all_day, location, tags, can_tag}], members: [{user_id, display_name, taggable, reason?}]}`. `start`/`end` are RFC 3339, or `YYYY-MM-DD` when `all_day`. `title` is Google's; `tags` are the member ids read from its trailing `[..]` group. `can_tag` needs a read/write connection and an entry Google reports editable. A member is not taggable (`reason` `duplicate_name` or `reserved_character`) when their name matches another member's case-insensitively or contains `,`, `[` or `]`. Recurring entries come back as occurrences. 503 `google_unavailable` if Google fails temporarily |
| `PUT /households/{id}/calendar/entries/{eventID}/tags` | `tags` (member user ids; `[]` untags) | Any member. Sends Google only the new title, with the connecting member's token, and answers `{id, title, tags, can_tag}` with the title Google stored. Names in the group that aren't members are kept after the tagged ones. 422 for a tag that isn't a taggable member, 403 when the entry can't be tagged (`can_tag` false), 404 `not_found` with no family calendar. If Google refuses: 404 `entry_gone`, 403 `not_editable`, 502 `google_rejected` |

### Operational

`GET /health` (liveness) and `GET /ready` (checks the DB) live outside `/api`.
