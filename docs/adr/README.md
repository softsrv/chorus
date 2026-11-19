# Architecture Decision Records

This directory records architectural decisions made for Chorus.

## Format

Each record follows this structure:

- **Title**: `# NNNN. <Title>`
- **Status**: `accepted`, `deprecated`, or `superseded by [NNNN](NNNN-kebab-title.md)`
- **Context**: The background, motivation, constraints, and alternatives considered
- **Decision**: What was chosen and the architectural rule or invariant established
- **Consequences**: The positive and negative tradeoffs, follow-up obligations, and impact on future work
- **Enforced by**: The automated tests, CI workflow steps, or build checks that enforce this decision

## Numbering rule

- Records use the filename format `NNNN-kebab-title.md` (e.g. `0001-service-layer-authorization-404-not-403.md`).
- Numbers are four-digit, sequentially assigned, append-only.
- Decisions are immutable: a reversed or revised decision is never edited away or deleted. It is marked `superseded by [NNNN](NNNN-kebab-title.md)` when a new ADR is accepted.

## Records

- [0001](0001-service-layer-authorization-404-not-403.md): Authorization in the service layer; non-members get 404
- [0002](0002-sqlc-and-manual-migrations-with-down-steps.md): sqlc queries and manual golang-migrate migrations, each with a down step
- [0003](0003-strict-csp-vanilla-web-client.md): Strict-CSP, framework-free web test client
- [0004](0004-ci-and-image-pipeline.md): CI gates every change; images are pushed only after tests pass
