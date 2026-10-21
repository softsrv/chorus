# Chorus

Family chores and calendar. This repo is the backend: a JSON API for the upcoming iOS/Android apps, plus a small web client for exercising it.

Built on the softsrv starter: Go, Postgres (pgx + sqlc), golang-migrate, Tailwind/DaisyUI.

## Quick start

Requires Go, Docker, and the `migrate`, `sqlc`, `air`, and `tailwindcss` CLIs.

```bash
cp .env.example .env
# set JWT_SECRET, e.g.: openssl rand -base64 48
make dev
```

Then open http://localhost:8080. Register, then open the verification email in smtp4dev at http://localhost:5080. The right-hand panel logs every API request and response.

## What's here

- **Accounts:** register, email verification, login, refresh, logout, password reset/change, per-device sessions, account deletion
- **Households:** create, invite by code, roles (admin/member), leave/remove members
- **Chores:** CRUD, assign to a member, due dates, complete/reopen, filters, pagination
- **Events:** CRUD, date-range queries for calendar views

The API contract is in [docs/API.md](docs/API.md). Development notes are in [CLAUDE.md](CLAUDE.md).

## Testing

```bash
make test               # unit tests
make test-integration   # against the chorus_test DB (docker compose creates it)
make check              # everything CI runs, including govulncheck
```

## CI and images

GitHub Actions (`.github/workflows/ci.yml`) runs the full test suite against Postgres on every push and pull request, then builds the Docker image. On commits to `main` that pass, it pushes `trriplejay/chorus:latest` and `trriplejay/chorus:sha-<commit>` to Docker Hub. The repo needs the secrets `DOCKER_USERNAME` and `DOCKER_PASSWORD` (a Docker Hub access token).
