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
		PasswordHash: "$2a$12$placeholder",
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
