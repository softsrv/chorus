-- Intended data loss: the redirect-based Google sign-in and calendar connect
-- flows are removed (replaced by SDK credential exchange on POST /auth/google
-- and POST /me/google-calendar/connect). These two tables held only
-- short-lived, single-use rows for those flows: sign-in hand-off codes
-- (google_sign_in_handoffs) and OAuth state / PKCE verifiers (oauth_states).
-- The routes that wrote them are gone and nothing else reads them, so their
-- rows have no value once this runs. The down migration recreates both tables
-- empty.
DROP TABLE IF EXISTS google_sign_in_handoffs;
DROP TABLE IF EXISTS oauth_states;
