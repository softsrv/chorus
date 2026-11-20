# 0004. CI gates every change; images are pushed only after tests pass

## Status

accepted

## Context

To ensure system reliability, avoid regressions, and prevent broken deployments, code changes must be validated against comprehensive testing suites (unit, archtest boundaries, integration with real Postgres, JS tests, system tests, secret scanning, and vulnerability checks) before merging and releasing container images.

Deploying untested images or pushing container images prior to test completion risks shipping broken software to production.

## Decision

1. **Automated CI gating:**
   - GitHub Actions (`.github/workflows/ci.yml`) runs on every pull request and push to `main`.
   - CI executes the full verification suite against a live PostgreSQL service container: formatting checks (`make fmt-check`), module verification (`make mod-verify`), secret scanning with gitleaks (`make secret-scan`), sqlc staleness checks (`make sqlc-check`), migration round-trip tests, Go vet, import boundaries and architectural tests (`internal/archtest/`), race-enabled unit tests, JS tests (`make test-js`), integration tests, system tests (`make test-system`), and vulnerability checking (`make vulncheck`).
2. **Container image pipeline & post-test release:**
   - Container images are built from `Dockerfile` in the repository root.
   - Pull requests trigger `docker-build-only` via `.github/workflows/docker-image.yml`, building the image (`push: false`, `load: true`) and executing system tests against the containerized application (`SYSTEST_IMAGE=chorus:systest`), ensuring the image build and runtime entrypoint succeed without pushing artifacts.
   - Pushes to `main` trigger `docker-build-push`, which builds the image, runs the container system tests against it, and pushes `trriplejay/chorus:sha-<commit>` and `trriplejay/chorus:latest` to Docker Hub only after all tests pass.
3. **Toolchain environment:**
   - The Go toolchain in `go.mod` declares `go 1.27` with no toolchain pin line.
   - The container image builds on `golang:1.27-alpine` as specified in `Dockerfile`.
   - Note on toolchain consistency: The tree declares Go 1.27 with no toolchain pin. The tree's actual toolchain state (`go 1.27` and `golang:1.27-alpine`) is accurately recorded here; mismatch with older external standing rules mentioning an earlier 1.26 toolchain pin is noted as a follow-up, not altered in this documentation.
4. **Branch protection and review gates:**
   - `.github/CODEOWNERS` specifies code ownership.
   - Pull requests require passing CI checks and review approval.

## Consequences

- Defective container images cannot be pushed to the registry.
- Pull requests validate both binary and containerized system tests before merge.
- High test coverage and strict security/boundary checks are continuously enforced on every change.

## Enforced by

- `.github/workflows/ci.yml`: `ci`, `docker-build-push`, `docker-build-only`
- `.github/workflows/docker-image.yml`: `build` job, `System tests (Docker image)` step
- `internal/archtest/codeowners_test.go`: `TestCodeowners`
- `Makefile`: `check`, `test-system-image`
