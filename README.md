# Chorus

Family chores and calendar. This repo is the backend: a JSON API for the upcoming iOS/Android apps, plus a small web client for exercising it.

Built on the softsrv starter: Go, Postgres (pgx + sqlc), golang-migrate, Tailwind/DaisyUI.

## Quick start

Requires Go, Docker, and the `migrate`, `sqlc`, and `tailwindcss` CLIs. The app runs in a container (`air` hot-reloads the bind-mounted source), so you don't need `air` installed locally.

```bash
cp .env.example .env
# set JWT_SECRET, e.g.: openssl rand -base64 48
make dev
```

Then open http://localhost:8080. Register, then open the verification email in smtp4dev at http://localhost:5080. The right-hand panel logs every API request and response.

## What's here

- **Accounts:** register, email verification, login, refresh, logout, password reset/change, per-device sessions, account deletion
- **Households:** create, family members with optional PINs, admin PIN protection
- **Chores:** CRUD, assign to a member, due dates, complete/reopen, filters, pagination
- **Events:** CRUD, date-range queries for calendar views

The API contract is in [docs/API.md](docs/API.md). Development notes are in [CLAUDE.md](CLAUDE.md).

## Testing

```bash
make test               # unit tests
make test-integration   # against the chorus_test DB (docker compose creates it)
make test-system        # system tests: run the real app binary against Postgres and a capture SMTP server
make test-system-image  # same tests against the built Docker image (Linux-only: host networking)
make check              # everything CI runs, including boundary check, system tests, govulncheck and secret-scan
```

## CI and images

GitHub Actions (`.github/workflows/ci.yml`) runs the full test suite and system tests (job `ci`) against Postgres on pushes to `main` and on every pull request before any image build or push. CI runs gitleaks via `make secret-scan` and `.gitleaks.toml` holds the dev/test allowlist. Fixtures and config must use obviously fake values or be added to `.gitleaks.toml` on purpose. Both image jobs (PR build-only and `main` build-push) run the system tests against the built image beside Postgres and a capture SMTP server. Passing merges to `main` push the tested image (`trriplejay/chorus:latest` and `trriplejay/chorus:sha-<commit>`) to Docker Hub only after they pass. The repo needs the secrets `DOCKER_USERNAME` and `DOCKER_PASSWORD` (a Docker Hub access token).
