# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Chorus is a family chore and calendar app. This repo holds the backend JSON API that the future iOS/Android apps will use, plus a throwaway web test client. It was bootstrapped from `../starter`.

## Commands

```bash
make dev              # docker compose (Postgres + smtp4dev) → migrate → air + Tailwind watch
make run              # Run without hot-reload (needs `make services-up` first)
make test             # Unit tests (-race)
make test-integration # Migrates chorus_test, then runs integration tests against it
make check            # vet + unit + integration + govulncheck (what CI runs)
make vet / fmt / lint

# Database
make migrate-up | migrate-down | migrate-status
make migrate-create NAME=<name>
make sqlc-generate                # Regenerate internal/db/ from db/queries/
make services-reset               # Wipe the local DB volume

# Docker
make prod             # Multi-stage, distroless, non-root image
```

Single test: `go test -tags integration -run TestAPI_EndToEnd ./internal/http/` (needs `TEST_DATABASE_URL`; `set -a; . ./.env; set +a` loads it).

Local services: Postgres on `localhost:5433` (databases `chorus` and `chorus_test`), smtp4dev SMTP on `:2525` with its web UI at http://localhost:5080. Verification and reset emails land there.

## Architecture

```
cmd/app/main.go                 bootstrap only: config, pool, wiring, graceful shutdown
internal/http/router.go         all routes + middleware chain + rate limits
internal/http/handlers/         JSON in/out; DTOs in dto.go; error→status mapping in json.go
internal/http/middleware/       auth (bearer), recover, rate limit, security headers, body limit, logging, request ID
internal/http/respond/          JSON response + error envelope (shared by handlers and middleware)
internal/app/                   services: business rules + authorization
internal/db/                    sqlc-generated, never edit by hand
db/migrations, db/queries       golang-migrate SQL + sqlc queries
web/static/                     web test client (index.html + vanilla JS modules)
docs/API.md                     API contract for client developers
```

### Rules that keep the API hardened

- **Authorization lives in the service layer.** Every household-scoped service method starts with `getHousehold` or `requireAdmin` (`internal/app/household_service.go`), and every chore/event SQL query also filters by `household_id`. Non-members get `ErrNotFound`, never `ErrForbidden`, so IDs can't be probed. New household-scoped features must follow both rules. `TestIntegration_HouseholdIsolation` is the guard; extend it.
- **Handlers never expose sqlc models.** Map to the DTOs in `dto.go`.
- **Validation** goes in services via the `validator` helper (`internal/app/validate.go`) and returns `*app.ValidationError` (→ 422 with per-field messages). DB `CHECK` constraints back up the limits.
- **Unexpected errors** fall through `writeError` to a logged, generic 500. Never put `err.Error()` from internals in a response.
- **Request decoding** always goes through `decodeJSON` (strict: content type, unknown fields, single object).
- **Updates are PUT (full replacement).** Keeps nullable fields unambiguous.

### Auth design

- Bearer-only. The API ignores cookies, so there's no CSRF surface.
- Access token: HS256 JWT, 15 min, claims `sub` (user) + `sid` (session = refresh token row ID). `middleware.Authenticate` loads the user via `GetUserForSession`, which requires the session to be unrevoked and unexpired, so logout/revocation take effect immediately.
- Refresh token: 32 random bytes, SHA-256 hashed in the DB, 30 days, **not rotated** (kept from starter).
- Register never reveals whether an email exists (duplicate → 202 + "account exists" email). Login uses a dummy bcrypt compare for unknown emails.
- Email-verification and reset tokens are single-use, enforced with conditional `UPDATE … WHERE used_at IS NULL`.
- Password change revokes other sessions; reset revokes all. Account deletion re-checks the password.
- Lockout: 10 failed logins → locked 1 hour.

### Households

Roles `admin`/`member`. Last-admin invariants (`ErrLastAdmin`) are enforced under row locks (`withAdminLock`). Invites are single-use Crockford-base32 codes (hash stored), 7-day expiry, redeemed under `FOR UPDATE`. The `chores → household_members` composite FK unassigns chores when the assignee leaves.

### Middleware chain (outermost → innermost)

`RequestID → Logging → Recover → SecurityHeaders → BodyLimit(64 KiB) → mux`. `/api/` additionally gets `Cache-Control: no-store` and a 300 req/min per-IP limiter. Rate limiters are in-memory, so they're per-instance; move to Redis/Postgres before running more than one replica.

## Database

Migrations run manually (`make migrate-up`), never on startup. After editing `db/queries/*.sql`, run `make sqlc-generate` (CI fails if `internal/db` is stale). Daily 03:00 job (`app.RunTokenCleanup`) purges stale tokens and invites.

## Environment

Copy `.env.example` → `.env`. Required: `DATABASE_URL`, `JWT_SECRET` (≥32 bytes), `APP_BASE_URL`, `SMTP_*`. `APP_ENV=production` enables JSON logs, HSTS, and serving web assets from the embedded copy (development serves `web/static` from disk). Set `TRUSTED_PROXY_COUNT` when behind a reverse proxy, or rate limits key on the proxy's IP.

## Web test client

`web/static/index.html` + `js/api.js` (token handling, auto-refresh) + `js/app.js` (views). No framework, no build step beyond Tailwind/DaisyUI. Strict CSP (`script-src 'self'`, no inline styles), so no inline `<script>`/`style=`. Insert API data with `textContent` only. It's a test harness, so keep it simple; it is not the product UI.

## Tech constraints

- Go stdlib first; propose libraries with rationale before adding.
- Parameterized SQL only (sqlc enforces this).
- bcrypt cost 12 (config clamps to 10–15); passwords 8–72 bytes.
- UUIDv7 via `github.com/google/uuid`.
- `go.mod` declares `go 1.27` (major/minor only) and pins no toolchain; stdlib fixes come from the newest 1.27.x that setup-go and the golang:1.27 image resolve.
