# 0001. Authorization in the service layer; non-members get 404

## Status

accepted

## Context

Chorus organizes chores, events, and member data within households. When a request asks for a resource in a household the caller does not belong to, or attempts to access a household resource without authorization, the system must prevent cross-household data leaks and enumeration attacks.

Two key concerns arise:
1. Where authorization checks should be placed: HTTP handlers, service layer, or database layer.
2. What status code to return when a caller accesses an unauthorized or foreign resource: `403 Forbidden` vs. `404 Not Found`. Returning `403 Forbidden` confirms that a resource exists with that identifier, allowing attackers to probe and enumerate valid household or resource IDs.

## Decision

1. **Authorization lives in the service layer (`internal/app/`):**
   - Every household-scoped service method begins with `getHousehold` or `requireAdmin` (defined in `internal/app/household_service.go`).
   - Every chore and event SQL query also filters by `household_id` to guarantee tenant isolation at both the service and database query levels.
2. **Non-members get `404 Not Found`, never `403 Forbidden`:**
   - When a caller does not belong to the household associated with a requested resource, the service returns `ErrNotFound`, which the HTTP transport maps to HTTP status 404.
   - Resource existence is never confirmed across household boundaries.

## Consequences

- IDs cannot be probed or enumerated across tenant boundaries.
- New household-scoped domain operations must follow both rules: begin with a membership check in `internal/app/` and include `household_id` filters in SQL queries.
- Handlers remain thin transport adapters without domain authorization logic.

## Enforced by

- `internal/app/household_service_integration_test.go`: `TestIntegration_HouseholdIsolation`
- `internal/app/household_isolation_more_integration_test.go`: `TestIntegration_HouseholdIsolation_Birthdays`, `TestIntegration_HouseholdIsolation_QuestAges`
