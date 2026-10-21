package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/respond"
)

type userContextKey struct{}
type sessionContextKey struct{}

// UserFetcher is satisfied by *db.Queries.
type UserFetcher interface {
	GetUserForSession(ctx context.Context, arg db.GetUserForSessionParams) (db.User, error)
}

// Authenticate validates the bearer token in the Authorization header, checks
// that the session it was issued under is still live, and attaches the user
// and session ID to the request context.
//
// Only the Authorization header is accepted — never cookies. Browsers attach
// cookies to cross-site requests automatically, which is what makes CSRF
// possible; a bearer token has to be added deliberately by client code, so the
// API needs no CSRF defenses.
func Authenticate(queries UserFetcher, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenStr := bearerToken(r)
			if tokenStr == "" {
				unauthorized(w, "unauthenticated", "Missing bearer token.")
				return
			}

			claims, err := auth.ValidateAccessToken(tokenStr, jwtSecret)
			if err != nil {
				if errors.Is(err, auth.ErrTokenExpired) {
					// Distinct code so clients know to refresh rather than re-login.
					unauthorized(w, "token_expired", "Access token has expired.")
					return
				}
				unauthorized(w, "unauthenticated", "Invalid access token.")
				return
			}

			userID, uErr := uuid.Parse(claims.Subject)
			sessionID, sErr := uuid.Parse(claims.SessionID)
			if uErr != nil || sErr != nil {
				slog.WarnContext(r.Context(), "auth: malformed token claims", "subject", claims.Subject, "sid", claims.SessionID)
				unauthorized(w, "unauthenticated", "Invalid access token.")
				return
			}

			user, err := queries.GetUserForSession(r.Context(), db.GetUserForSessionParams{
				SessionID: sessionID,
				UserID:    userID,
			})
			if err != nil {
				// No row: user deleted or session revoked/expired.
				slog.InfoContext(r.Context(), "auth: session not active", "user_id", userID, "session_id", sessionID, "error", err)
				unauthorized(w, "unauthenticated", "Session is no longer valid.")
				return
			}

			ctx := context.WithValue(r.Context(), userContextKey{}, user)
			ctx = context.WithValue(ctx, sessionContextKey{}, sessionID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserFromContext retrieves the authenticated user from context.
func UserFromContext(ctx context.Context) (db.User, bool) {
	u, ok := ctx.Value(userContextKey{}).(db.User)
	return u, ok
}

// SessionIDFromContext retrieves the current session (refresh token) ID.
func SessionIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(sessionContextKey{}).(uuid.UUID)
	return id, ok
}

// RequireEmailVerified rejects requests from authenticated users whose email
// address has not yet been verified. Must be applied after Authenticate.
func RequireEmailVerified(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok || !user.EmailVerified {
			respond.Error(w, http.StatusForbidden, "email_not_verified", "Verify your email address to continue.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func unauthorized(w http.ResponseWriter, code, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="chorus"`)
	respond.Error(w, http.StatusUnauthorized, code, message)
}
