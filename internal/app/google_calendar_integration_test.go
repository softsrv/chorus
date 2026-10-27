//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
)

// seedCalendarData stores a calendar connection and a pending calendar state
// for userID.
func seedCalendarData(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	cipher, err := app.NewTokenCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	ae, _ := cipher.Encrypt("access")
	re, _ := cipher.Encrypt("refresh")
	q := db.New(pool)
	if _, err := q.UpsertGoogleCalendarConnection(ctx, db.UpsertGoogleCalendarConnectionParams{
		ID: uuid.Must(uuid.NewV7()), UserID: userID, AccessTokenEnc: ae, RefreshTokenEnc: re,
		TokenExpiry: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, AccessLevel: "read_write",
	}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	uid := userID
	if _, err := q.InsertOAuthState(ctx, db.InsertOAuthStateParams{
		ID: uuid.Must(uuid.NewV7()), StateHash: auth.HashToken("state-" + uuid.NewString()),
		Purpose: "calendar", UserID: &uid, PkceVerifier: "verifier",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if n := calendarRows(t, pool, userID); n != 2 {
		t.Fatalf("seeded rows = %d, want 2", n)
	}
}

// calendarRows counts userID's connection and calendar state rows.
func calendarRows(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT COUNT(*) FROM google_calendar_connections WHERE user_id = $1) +
		(SELECT COUNT(*) FROM oauth_states WHERE user_id = $1 AND purpose = 'calendar')`, userID).Scan(&n); err != nil {
		t.Fatalf("count calendar rows: %v", err)
	}
	return n
}

// CLM-11: deleting an account removes its calendar connection and pending
// calendar state, on both the password and the passwordless paths.
func TestIntegration_DeleteAccountRemovesGoogleCalendar(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	svc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	t.Run("password account", func(t *testing.T) {
		res := registerVerified(t, pool, svc, mailer, "caldelete")
		seedCalendarData(t, pool, res.User.ID)
		if err := svc.DeleteAccount(ctx, res.User.ID, uuid.Nil, "correct-password-123", ""); err != nil {
			t.Fatalf("DeleteAccount: %v", err)
		}
		if n := calendarRows(t, pool, res.User.ID); n != 0 {
			t.Errorf("calendar rows after delete = %d, want 0", n)
		}
	})

	t.Run("passwordless account", func(t *testing.T) {
		user := createPasswordlessUser(t, pool, "caldeletegoogle")
		seedCalendarData(t, pool, user.ID)
		_, hashed, err := auth.GenerateRefreshToken()
		if err != nil {
			t.Fatalf("GenerateRefreshToken: %v", err)
		}
		sessionID := uuid.Must(uuid.NewV7())
		if _, err := db.New(pool).InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
			ID: sessionID, UserID: user.ID, TokenHash: hashed,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		}); err != nil {
			t.Fatalf("InsertRefreshToken: %v", err)
		}
		if err := svc.DeleteAccount(ctx, user.ID, sessionID, "", user.Email); err != nil {
			t.Fatalf("DeleteAccount: %v", err)
		}
		if n := calendarRows(t, pool, user.ID); n != 0 {
			t.Errorf("calendar rows after delete = %d, want 0", n)
		}
	})
}
