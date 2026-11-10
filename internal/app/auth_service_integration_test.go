//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// testDB connects to the database pointed to by TEST_DATABASE_URL.
// Run integration tests with: make test-integration
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// captureMailer records sent emails so tests can pull tokens out of links.
type captureMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

type sentMail struct{ to, subject, text string }

func (m *captureMailer) Send(to, subject, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, text})
	return nil
}

var tokenRE = regexp.MustCompile(`token=([A-Za-z0-9_\-=]+)`)

// waitForToken polls for an email to `to` whose subject contains subjectPart
// (sends happen on a background goroutine) and returns the link token in it.
func (m *captureMailer) waitForToken(t *testing.T, to, subjectPart string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		for _, s := range m.sent {
			if s.to == to && strings.Contains(s.subject, subjectPart) {
				m.mu.Unlock()
				match := tokenRE.FindStringSubmatch(s.text)
				if match == nil {
					t.Fatalf("no token in email %q", s.subject)
				}
				tok, _ := url.QueryUnescape(match[1])
				return tok
			}
		}
		m.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q email to %s", subjectPart, to)
	return ""
}

func (m *captureMailer) count(to, subjectPart string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sent {
		if s.to == to && strings.Contains(s.subject, subjectPart) {
			n++
		}
	}
	return n
}

func testAuthService(t *testing.T, pool *pgxpool.Pool, mailer *captureMailer) *app.AuthService {
	t.Helper()
	svc := app.NewAuthService(db.New(pool), pool, mailer, app.AuthServiceConfig{
		JWTSecret:      "integration-test-secret-32-bytes!!!",
		AccessExpiry:   15 * time.Minute,
		RefreshExpiry:  720 * time.Hour,
		BCryptCost:     10, // bcrypt's minimum sane cost; keeps tests fast
		PasswordMinLen: 8,
		AppBaseURL:     "http://localhost:8080",
		AppName:        "Chorus",
	})
	t.Cleanup(svc.Shutdown)
	return svc
}

// uniqueEmail returns an address no other test (or earlier run) uses.
func uniqueEmail(prefix string) string {
	return fmt.Sprintf("%s-%s@example.com", prefix, uuid.NewString()[:8])
}

// registerVerified registers a user, verifies their email, and returns the
// resulting signed-in token pair. The user is deleted at test end.
func registerVerified(t *testing.T, pool *pgxpool.Pool, svc *app.AuthService, mailer *captureMailer, prefix string) app.TokenResult {
	t.Helper()
	ctx := context.Background()
	email := uniqueEmail(prefix)
	if err := svc.Register(ctx, email, "correct-password-123", prefix); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", email)
	})
	res, err := svc.VerifyEmail(ctx, mailer.waitForToken(t, email, "Verify"), app.DeviceMeta{})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	return res
}

func TestIntegration_LoginFlow(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	email := uniqueEmail("login")
	const password = "correct-password-123"

	if err := svc.Register(ctx, email, password, "Login Test"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

	result, err := svc.Login(ctx, strings.ToUpper(email), password, app.DeviceMeta{})
	if err != nil {
		t.Fatalf("Login (email should be case-insensitive): %v", err)
	}
	if result.AccessToken == "" || result.RefreshToken == "" {
		t.Fatal("expected non-empty tokens")
	}
	if result.User.EmailVerified {
		t.Error("new user should not be verified")
	}

	if _, err = svc.Login(ctx, email, "wrong-password", app.DeviceMeta{}); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Errorf("wrong password: expected ErrInvalidCredentials, got %v", err)
	}
	if _, err = svc.Login(ctx, uniqueEmail("nobody"), password, app.DeviceMeta{}); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Errorf("unknown email: expected ErrInvalidCredentials, got %v", err)
	}
}

func TestIntegration_RegisterValidation(t *testing.T) {
	pool := testDB(t)
	svc := testAuthService(t, pool, &captureMailer{})

	err := svc.Register(context.Background(), "not-an-email", "short", "  ")
	var vErr *app.ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	for _, f := range []string{"email", "password", "display_name"} {
		if _, ok := vErr.Fields[f]; !ok {
			t.Errorf("expected field error for %s, got %v", f, vErr.Fields)
		}
	}
}

// Registering an existing email must look like success to the caller and
// notify the real owner instead.
func TestIntegration_RegisterDuplicateEmail(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	email := uniqueEmail("dup")
	if err := svc.Register(ctx, email, "first-password-123", "First"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

	if err := svc.Register(ctx, email, "second-password-123", "Second"); err != nil {
		t.Fatalf("duplicate Register should return nil, got %v", err)
	}
	svc.Shutdown() // flush background sends
	if n := mailer.count(email, "Your Chorus account"); n != 1 {
		t.Errorf("expected 1 account-exists email, got %d", n)
	}
	// The original password still works; the second one doesn't.
	if _, err := svc.Login(ctx, email, "second-password-123", app.DeviceMeta{}); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Errorf("second password must not work, got %v", err)
	}
}

func TestIntegration_VerifyEmailTokenSingleUse(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	email := uniqueEmail("verify")
	if err := svc.Register(ctx, email, "correct-password-123", "Verify"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

	token := mailer.waitForToken(t, email, "Verify")
	res, err := svc.VerifyEmail(ctx, token, app.DeviceMeta{})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if !res.User.EmailVerified {
		t.Error("user should be verified")
	}
	if _, err := svc.VerifyEmail(ctx, token, app.DeviceMeta{}); !errors.Is(err, app.ErrTokenUsed) {
		t.Errorf("second use: expected ErrTokenUsed, got %v", err)
	}
}

func TestIntegration_Refresh(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	result := registerVerified(t, pool, svc, mailer, "refresh")

	// Refresh returns a new access token but the same refresh token.
	result2, err := svc.Refresh(ctx, result.RefreshToken, app.DeviceMeta{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if result2.RefreshToken != result.RefreshToken {
		t.Error("refresh token must not change (no rotation)")
	}

	// Token can be refreshed again without error.
	if _, err = svc.Refresh(ctx, result.RefreshToken, app.DeviceMeta{}); err != nil {
		t.Errorf("second refresh of same token must succeed, got %v", err)
	}

	// After logout (which revokes the session), the refresh token is rejected.
	sessions, err := db.New(pool).ListActiveRefreshTokensByUserID(ctx, result.User.ID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d (err %v)", len(sessions), err)
	}
	if err := svc.Logout(ctx, sessions[0].ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err = svc.Refresh(ctx, result.RefreshToken, app.DeviceMeta{}); !errors.Is(err, app.ErrTokenRevoked) {
		t.Errorf("expected ErrTokenRevoked after logout, got %v", err)
	}
}

func TestIntegration_PasswordReset(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	signedIn := registerVerified(t, pool, svc, mailer, "reset")
	email := signedIn.User.Email

	if err := svc.CompletePasswordReset(ctx, "invalid-token", "newpassword123"); !errors.Is(err, app.ErrTokenNotFound) {
		t.Errorf("expected ErrTokenNotFound for invalid token, got %v", err)
	}

	if err := svc.RequestPasswordReset(ctx, email); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	token := mailer.waitForToken(t, email, "Reset")

	if err := svc.CompletePasswordReset(ctx, token, "brand-new-password"); err != nil {
		t.Fatalf("CompletePasswordReset: %v", err)
	}
	if err := svc.CompletePasswordReset(ctx, token, "another-password"); !errors.Is(err, app.ErrTokenUsed) {
		t.Errorf("token reuse: expected ErrTokenUsed, got %v", err)
	}
	if _, err := svc.Login(ctx, email, "brand-new-password", app.DeviceMeta{}); err != nil {
		t.Errorf("login with new password: %v", err)
	}
	// Existing sessions are revoked by a reset.
	if _, err := svc.Refresh(ctx, signedIn.RefreshToken, app.DeviceMeta{}); !errors.Is(err, app.ErrTokenRevoked) {
		t.Errorf("old session after reset: expected ErrTokenRevoked, got %v", err)
	}
}

func TestIntegration_AccountLockout(t *testing.T) {
	pool := testDB(t)
	svc := testAuthService(t, pool, &captureMailer{})
	ctx := context.Background()

	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	q := db.New(pool)
	email := uniqueEmail("lockout")
	user, err := q.CreateUser(ctx, db.CreateUserParams{
		ID:           id,
		Email:        email,
		DisplayName:  "Lockout",
		PasswordHash: pgtype.Text{String: "$2a$12$placeholder", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE id = $1", user.ID) })

	if err := q.LockAccount(ctx, db.LockAccountParams{
		ID:          user.ID,
		LockedUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("LockAccount: %v", err)
	}

	if _, err = svc.Login(ctx, email, "any-password", app.DeviceMeta{}); !errors.Is(err, app.ErrAccountLocked) {
		t.Errorf("expected ErrAccountLocked, got %v", err)
	}
}

// createPasswordlessUser inserts a Google-only account (NULL password_hash)
// directly through the queries the Google sign-in flow uses.
func createPasswordlessUser(t *testing.T, pool *pgxpool.Pool, prefix string) db.User {
	t.Helper()
	ctx := context.Background()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	user, err := db.New(pool).CreateGoogleUser(ctx, db.CreateGoogleUserParams{
		ID:            id,
		Email:         uniqueEmail(prefix),
		DisplayName:   prefix,
		GoogleSubject: pgtype.Text{String: "google-sub-" + id.String(), Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateGoogleUser: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", user.ID) })
	return user
}

// Password sign-in to a passwordless account is refused as invalid
// credentials, and guessing never counts toward or triggers the lockout.
func TestIntegration_PasswordlessLoginNeverLocks(t *testing.T) {
	pool := testDB(t)
	svc := testAuthService(t, pool, &captureMailer{})
	ctx := context.Background()

	user := createPasswordlessUser(t, pool, "passwordless")
	if user.PasswordHash.Valid {
		t.Fatalf("CreateGoogleUser stored a password hash: %+v", user.PasswordHash)
	}

	for _, pw := range append([]string{"", "correct-password-123"}, strings.Split(strings.Repeat("guess,", 12), ",")...) {
		if _, err := svc.Login(ctx, user.Email, pw, app.DeviceMeta{}); !errors.Is(err, app.ErrInvalidCredentials) {
			t.Fatalf("Login(%q): expected ErrInvalidCredentials, got %v", pw, err)
		}
	}

	got, err := db.New(pool).GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.FailedLoginAttempts != 0 || got.LockedUntil.Valid {
		t.Errorf("passwordless account was counted/locked: attempts=%d locked_until=%v", got.FailedLoginAttempts, got.LockedUntil)
	}
}

// A password account that also has Google linked keeps password sign-in and
// the existing lockout threshold unchanged.
func TestIntegration_GoogleLinkedPasswordUserLoginAndLockout(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()
	q := db.New(pool)

	signedIn := registerVerified(t, pool, svc, mailer, "linked")
	user := signedIn.User
	n, err := q.SetGoogleSubject(ctx, db.SetGoogleSubjectParams{
		ID:            user.ID,
		GoogleSubject: pgtype.Text{String: "google-sub-" + user.ID.String(), Valid: true},
	})
	if err != nil || n != 1 {
		t.Fatalf("SetGoogleSubject: n=%d err=%v", n, err)
	}

	if _, err := svc.Login(ctx, user.Email, "correct-password-123", app.DeviceMeta{}); err != nil {
		t.Fatalf("linked password user login: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := svc.Login(ctx, user.Email, "wrong-password", app.DeviceMeta{}); !errors.Is(err, app.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: expected ErrInvalidCredentials, got %v", i+1, err)
		}
	}
	if _, err := svc.Login(ctx, user.Email, "correct-password-123", app.DeviceMeta{}); !errors.Is(err, app.ErrAccountLocked) {
		t.Errorf("after threshold: expected ErrAccountLocked, got %v", err)
	}
}

func TestIntegration_FirstSignInProvisionsHousehold(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	t.Run("VerifyEmail yields exactly one admin household, later login adds none", func(t *testing.T) {
		email := uniqueEmail("provision")
		const password = "correct-password-123"
		if err := svc.Register(ctx, email, password, "Alice"); err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

		tok := mailer.waitForToken(t, email, "Verify")
		res, err := svc.VerifyEmail(ctx, tok, app.DeviceMeta{})
		if err != nil {
			t.Fatalf("VerifyEmail: %v", err)
		}

		// Check household created
		var count int
		var hName, hTZ, mRole string
		err = pool.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(MAX(h.name), ''), COALESCE(MAX(h.timezone), ''), COALESCE(MAX(m.role), '')
			FROM household_members m
			JOIN households h ON h.id = m.household_id
			WHERE m.user_id = $1
		`, res.User.ID).Scan(&count, &hName, &hTZ, &mRole)
		if err != nil {
			t.Fatalf("query household: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 household, got %d", count)
		}
		if hName != "Alice's household" {
			t.Errorf("household name: got %q, want %q", hName, "Alice's household")
		}
		if hTZ != "UTC" {
			t.Errorf("timezone: got %q, want %q", hTZ, "UTC")
		}
		if mRole != app.RoleAdmin {
			t.Errorf("role: got %q, want %q", mRole, app.RoleAdmin)
		}

		// Later Login adds none
		if _, err := svc.Login(ctx, email, password, app.DeviceMeta{}); err != nil {
			t.Fatalf("Login: %v", err)
		}
		var countAfter int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_members WHERE user_id = $1", res.User.ID).Scan(&countAfter); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if countAfter != 1 {
			t.Errorf("expected still 1 household after login, got %d", countAfter)
		}
	})

	t.Run("unverified Login adds none", func(t *testing.T) {
		email := uniqueEmail("unverified")
		const password = "correct-password-123"
		if err := svc.Register(ctx, email, password, "Bob"); err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

		res, err := svc.Login(ctx, email, password, app.DeviceMeta{})
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_members WHERE user_id = $1", res.User.ID).Scan(&count); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if count != 0 {
			t.Errorf("expected 0 households for unverified login, got %d", count)
		}
	})

	t.Run("N concurrent Logins give exactly one", func(t *testing.T) {
		email := uniqueEmail("concurrent")
		const password = "correct-password-123"
		if err := svc.Register(ctx, email, password, "Charlie"); err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

		// Mark email verified directly in DB to simulate verified user prior to first login
		var userID uuid.UUID
		if err := pool.QueryRow(ctx, "UPDATE users SET email_verified = true, household_provisioned_at = NULL WHERE email = $1 RETURNING id", email).Scan(&userID); err != nil {
			t.Fatalf("mark verified: %v", err)
		}

		const n = 10
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, errs[idx] = svc.Login(ctx, email, password, app.DeviceMeta{})
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent login %d failed: %v", i, err)
			}
		}

		var count int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_members WHERE user_id = $1", userID).Scan(&count); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if count != 1 {
			t.Errorf("expected exactly 1 household after concurrent logins, got %d", count)
		}
	})

	t.Run("after the household is deleted the next sign-in provisions a new one", func(t *testing.T) {
		res := registerVerified(t, pool, svc, mailer, "deleted-hh")
		households := app.NewHouseholdService(db.New(pool), pool)

		var oldID uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT household_id FROM household_members WHERE user_id = $1", res.User.ID).Scan(&oldID); err != nil {
			t.Fatalf("find household: %v", err)
		}
		if err := households.Delete(ctx, res.User.ID, oldID); err != nil {
			t.Fatalf("Delete household: %v", err)
		}

		if _, err := svc.Login(ctx, res.User.Email, "correct-password-123", app.DeviceMeta{}); err != nil {
			t.Fatalf("Login: %v", err)
		}

		var count int
		var newID uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_members WHERE user_id = $1", res.User.ID).Scan(&count); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 household after deletion and login, got %d", count)
		}
		if err := pool.QueryRow(ctx, "SELECT household_id FROM household_members WHERE user_id = $1", res.User.ID).Scan(&newID); err != nil {
			t.Fatalf("find new household: %v", err)
		}
		if newID == oldID {
			t.Errorf("expected a new household, got the deleted one")
		}

		// A second sign-in adds no further household.
		if _, err := svc.Login(ctx, res.User.Email, "correct-password-123", app.DeviceMeta{}); err != nil {
			t.Fatalf("second Login: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_members WHERE user_id = $1", res.User.ID).Scan(&count); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if count != 1 {
			t.Errorf("expected still 1 household, got %d", count)
		}
	})

	t.Run("a sign-in overlapping the household delete ends with a household", func(t *testing.T) {
		res := registerVerified(t, pool, svc, mailer, "overlap-del")

		var oldID uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT household_id FROM household_members WHERE user_id = $1", res.User.ID).Scan(&oldID); err != nil {
			t.Fatalf("find household: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck

		// In a manual tx take the users row lock (FOR UPDATE), delete the household
		// and set household_provisioned_at = NULL, mirroring HouseholdService.Delete in flight.
		if _, err := tx.Exec(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1 FOR UPDATE", res.User.ID); err != nil {
			t.Fatalf("lock user: %v", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM households WHERE id = $1", oldID); err != nil {
			t.Fatalf("delete household: %v", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE users SET household_provisioned_at = NULL WHERE id = $1", res.User.ID); err != nil {
			t.Fatalf("release provisioning: %v", err)
		}

		loginDone := make(chan error, 1)
		go func() {
			_, err := svc.Login(ctx, res.User.Email, "correct-password-123", app.DeviceMeta{})
			loginDone <- err
		}()

		// Assert it has not returned within a short wait because it is blocked on the row lock.
		select {
		case err := <-loginDone:
			t.Fatalf("Login returned while delete tx was still open: %v", err)
		case <-time.After(100 * time.Millisecond):
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit tx: %v", err)
		}

		select {
		case err := <-loginDone:
			if err != nil {
				t.Fatalf("Login failed after commit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Login timed out waiting for delete commit")
		}

		var count int
		var newID uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_members WHERE user_id = $1", res.User.ID).Scan(&count); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 household, got %d", count)
		}
		if err := pool.QueryRow(ctx, "SELECT household_id FROM household_members WHERE user_id = $1", res.User.ID).Scan(&newID); err != nil {
			t.Fatalf("query new household id: %v", err)
		}
		if newID == oldID {
			t.Errorf("expected new household id to differ from deleted id %v", oldID)
		}
	})

	t.Run("concurrent HouseholdService.Delete and Login still ends with a household", func(t *testing.T) {
		res := registerVerified(t, pool, svc, mailer, "concurrent-del")
		households := app.NewHouseholdService(db.New(pool), pool)

		var oldID uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT household_id FROM household_members WHERE user_id = $1", res.User.ID).Scan(&oldID); err != nil {
			t.Fatalf("find household: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var delErr, loginErr error

		go func() {
			defer wg.Done()
			delErr = households.Delete(ctx, res.User.ID, oldID)
		}()
		go func() {
			defer wg.Done()
			_, loginErr = svc.Login(ctx, res.User.Email, "correct-password-123", app.DeviceMeta{})
		}()

		wg.Wait()

		if delErr != nil {
			t.Fatalf("concurrent Delete: %v", delErr)
		}
		if loginErr != nil {
			t.Fatalf("concurrent Login: %v", loginErr)
		}

		var count int
		var newID uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_members WHERE user_id = $1", res.User.ID).Scan(&count); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 household, got %d", count)
		}
		if err := pool.QueryRow(ctx, "SELECT household_id FROM household_members WHERE user_id = $1", res.User.ID).Scan(&newID); err != nil {
			t.Fatalf("query new household id: %v", err)
		}
		if newID == oldID {
			t.Errorf("expected new household id to differ from deleted id %v", oldID)
		}
	})

	t.Run("long display name truncated to 100 runes", func(t *testing.T) {
		email := uniqueEmail("longname")
		// The longest display name the users CHECK allows: 50 runes, multibyte included.
		longName := strings.Repeat("A", 24) + "🌟" + strings.Repeat("B", 25)
		if n := len([]rune(longName)); n != 50 {
			t.Fatalf("test setup: display name is %d runes, want 50", n)
		}
		const password = "correct-password-123"
		if err := svc.Register(ctx, email, password, longName); err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

		tok := mailer.waitForToken(t, email, "Verify")
		res, err := svc.VerifyEmail(ctx, tok, app.DeviceMeta{})
		if err != nil {
			t.Fatalf("VerifyEmail: %v", err)
		}

		var hName string
		err = pool.QueryRow(ctx, `
			SELECT h.name FROM households h
			JOIN household_members m ON m.household_id = h.id
			WHERE m.user_id = $1
		`, res.User.ID).Scan(&hName)
		if err != nil {
			t.Fatalf("query household name: %v", err)
		}
		// 50 + len("'s household") = 62 runes, under the limit: nothing is cut.
		if want := longName + "'s household"; hName != want {
			t.Errorf("household name: got %q, want %q", hName, want)
		}

		// The 100-rune cut is unreachable from a real user (display_name is
		// capped at 50), so it is proven on the name builder itself.
		over := strings.Repeat("é", 95) + "🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟"
		got := app.DefaultHouseholdName(over)
		if want := string([]rune(over + "'s household")[:100]); got != want {
			t.Errorf("DefaultHouseholdName(105 runes): got %q (%d runes), want %q", got, len([]rune(got)), want)
		}
		if n := len([]rune(got)); n != 100 {
			t.Errorf("DefaultHouseholdName: got %d runes, want 100", n)
		}
	})

	t.Run("fault injection rolls back claim and issues no refresh token", func(t *testing.T) {
		// Install trigger that fails insertion into households if name starts with FAULT
		_, err := pool.Exec(ctx, `
			CREATE OR REPLACE FUNCTION fail_fault_household() RETURNS trigger AS $$
			BEGIN
				IF NEW.name LIKE 'FAULT%' THEN
					RAISE EXCEPTION 'injected fault on household creation';
				END IF;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql;

			DROP TRIGGER IF EXISTS trg_fault_household ON households;
			CREATE TRIGGER trg_fault_household BEFORE INSERT ON households
			FOR EACH ROW EXECUTE FUNCTION fail_fault_household();
		`)
		if err != nil {
			t.Fatalf("create trigger: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `
				DROP TRIGGER IF EXISTS trg_fault_household ON households;
				DROP FUNCTION IF EXISTS fail_fault_household();
			`)
		})

		email := uniqueEmail("fault")
		const password = "correct-password-123"
		if err := svc.Register(ctx, email, password, "FAULT_USER"); err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

		var userID uuid.UUID
		if err := pool.QueryRow(ctx, "UPDATE users SET email_verified = true, household_provisioned_at = NULL WHERE email = $1 RETURNING id", email).Scan(&userID); err != nil {
			t.Fatalf("mark verified: %v", err)
		}

		// Login should fail because provisionFirstHousehold fails
		_, err = svc.Login(ctx, email, password, app.DeviceMeta{})
		if err == nil {
			t.Fatal("expected Login to fail on injected provisioning fault, but got nil")
		}

		// Verify household_provisioned_at is still NULL
		var provAt pgtype.Timestamptz
		if err := pool.QueryRow(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1", userID).Scan(&provAt); err != nil {
			t.Fatalf("query household_provisioned_at: %v", err)
		}
		if provAt.Valid {
			t.Errorf("expected household_provisioned_at to be NULL after rollback, got %v", provAt.Time)
		}

		// Verify no refresh token was issued
		var tokenCount int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1", userID).Scan(&tokenCount); err != nil {
			t.Fatalf("query refresh tokens: %v", err)
		}
		if tokenCount != 0 {
			t.Errorf("expected 0 refresh tokens issued on failure, got %d", tokenCount)
		}
	})

	t.Run("backfill prevents auto-provisioning existing members", func(t *testing.T) {
		email := uniqueEmail("backfill")
		const password = "correct-password-123"
		if err := svc.Register(ctx, email, password, "BackfillUser"); err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email) })

		var userID uuid.UUID
		if err := pool.QueryRow(ctx, "UPDATE users SET email_verified = true, household_provisioned_at = NULL WHERE email = $1 RETURNING id", email).Scan(&userID); err != nil {
			t.Fatalf("mark verified: %v", err)
		}

		// Create an existing household directly
		hhID, _ := uuid.NewV7()
		if _, err := pool.Exec(ctx, "INSERT INTO households (id, name, timezone) VALUES ($1, $2, 'America/New_York')", hhID, "Original Household"); err != nil {
			t.Fatalf("insert household: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM households WHERE id = $1", hhID) })
		if _, err := pool.Exec(ctx, "INSERT INTO household_members (household_id, user_id, role) VALUES ($1, $2, 'admin')", hhID, userID); err != nil {
			t.Fatalf("insert member: %v", err)
		}

		// Run 000009 backfill query
		if _, err := pool.Exec(ctx, "UPDATE users SET household_provisioned_at = NOW() WHERE id IN (SELECT user_id FROM household_members)"); err != nil {
			t.Fatalf("run backfill: %v", err)
		}

		// Login
		if _, err := svc.Login(ctx, email, password, app.DeviceMeta{}); err != nil {
			t.Fatalf("Login: %v", err)
		}

		// Assert user still has exactly their 1 original household
		var hhCount int
		var hhName string
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(MAX(h.name), '')
			FROM household_members m
			JOIN households h ON h.id = m.household_id
			WHERE m.user_id = $1
		`, userID).Scan(&hhCount, &hhName); err != nil {
			t.Fatalf("query households: %v", err)
		}
		if hhCount != 1 || hhName != "Original Household" {
			t.Errorf("expected 1 original household, got count=%d name=%q", hhCount, hhName)
		}
	})
}
