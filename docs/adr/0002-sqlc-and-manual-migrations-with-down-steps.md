# 0002. sqlc queries and manual golang-migrate migrations, each with a down step

## Status

accepted

## Context

Database persistence requires predictable SQL execution, type-safe query interfaces in Go, and reliable schema migrations across development, testing, and production environments.

Automated migrations on application startup can cause race conditions in clustered deployments, mask schema failures, or run destructive changes without operator oversight. In addition, schema changes without tested rollbacks risk breaking disaster-recovery procedures.

## Decision

1. **Type-safe SQL via sqlc:**
   - All database queries are written in raw parameterized SQL (`db/queries/`) and generated into type-safe Go code using `sqlc` (`internal/db/`).
   - Code under `internal/db/` is generated and never edited by hand.
   - CI validates that generated code matches query definitions via `make sqlc-check`.
2. **Manual migrations via golang-migrate:**
   - Migrations never run automatically on application startup. They are applied explicitly by operators using `make migrate-up` (or via golang-migrate CLI tooling).
   - Each migration file in `db/migrations/` must ship a corresponding `.down.sql` file restoring the exact schema prior to that migration.
   - Schema evolutions follow expand-before-contract discipline: new structures are introduced before old structures are dropped, ensuring backward compatibility during rolling deployments.
   - Applied migrations are immutable: once committed and applied, an existing migration file is never modified; changes are introduced via new migrations.

## Consequences

- Zero ORM impedance mismatch; compile-time verification of SQL queries via `sqlc`.
- Schema deployments are explicit and safe across multiple replicas.
- Every migration is guaranteed to be reversible, preventing irreversible schema dead-ends.

## Enforced by

- `internal/app/migrations_roundtrip_integration_test.go`: `TestIntegration_MigrationsRoundTrip`
- `.github/workflows/ci.yml`: `Migrate round-trip` step
- `Makefile`: `sqlc-check`, `migrate-roundtrip-test`
