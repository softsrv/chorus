package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/softsrv/chorus/internal/db"
)

// RunTokenCleanup runs the token maintenance job at startup and then daily at
// 03:00. It blocks until ctx is cancelled.
func RunTokenCleanup(ctx context.Context, q *db.Queries) {
	CleanupTokens(ctx, q)

	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}

		CleanupTokens(ctx, q)
	}
}

// CleanupTokens deletes expired and stale tokens and invites
// and logs the number of rows removed from each.
// Exported so tests and debug endpoints can trigger a run directly.
func CleanupTokens(ctx context.Context, q *db.Queries) {
	slog.Info("running token cleanup")

	n1, err := q.DeleteExpiredRefreshTokens(ctx)
	if err != nil {
		slog.Error("token cleanup: delete expired refresh tokens", "error", err)
	}
	n2, err := q.DeleteStalePasswordResetTokens(ctx)
	if err != nil {
		slog.Error("token cleanup: delete stale password reset tokens", "error", err)
	}
	n3, err := q.DeleteStaleVerificationCodes(ctx)
	if err != nil {
		slog.Error("token cleanup: delete stale verification codes", "error", err)
	}
	n4, err := q.DeleteStaleHouseholdInvites(ctx)
	if err != nil {
		slog.Error("token cleanup: delete stale household invites", "error", err)
	}
	n5, err := q.DeleteStaleOAuthStates(ctx)
	if err != nil {
		slog.Error("token cleanup: delete stale oauth states", "error", err)
	}
	n6, err := q.DeleteStaleSignInHandoffs(ctx)
	if err != nil {
		slog.Error("token cleanup: delete stale google sign-in handoffs", "error", err)
	}

	slog.Info("token cleanup complete",
		"refresh_tokens_deleted", n1,
		"reset_tokens_deleted", n2,
		"verification_codes_deleted", n3,
		"household_invites_deleted", n4,
		"oauth_states_deleted", n5,
		"google_sign_in_handoffs_deleted", n6,
	)
}
