package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/users"
)

// googleTokenURL is Google's public token endpoint. GoogleCalendar can
// override it so tests can point it at a fake.
const googleTokenURL = "https://oauth2.googleapis.com/token"

const googleHTTPTimeout = 10 * time.Second

// ErrGoogleSignInFailed is the single generic outcome for every refused
// Google sign-in. The cause is logged, never shown.
var ErrGoogleSignInFailed = errors.New("google sign-in failed")

// GoogleSignInConfig configures GoogleSignIn.
type GoogleSignInConfig struct {
	// ClientIDs are the accepted ID-token audiences: the web client ID
	// first, then the iOS and Android client IDs when configured.
	ClientIDs []string
	// JWKSURL is where Google's ID-token signing keys are fetched;
	// defaults to Google's published key set.
	JWKSURL    string
	HTTPClient *http.Client
}

// GoogleSignIn runs "Sign in with Google": it verifies a Google ID token from
// the web, iOS or Android app and issues the standard token pair.
type GoogleSignIn struct {
	q           *db.Queries
	authSvc     *AuthService
	verifier    *idTokenVerifier
	webClientID string
}

// NewGoogleSignIn builds the sign-in service. authSvc issues the token pair.
func NewGoogleSignIn(q *db.Queries, authSvc *AuthService, cfg GoogleSignInConfig) *GoogleSignIn {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: googleHTTPTimeout}
	}
	return &GoogleSignIn{
		q:           q,
		authSvc:     authSvc,
		verifier:    newIDTokenVerifier(cfg.ClientIDs, cfg.JWKSURL, client),
		webClientID: firstOrEmpty(cfg.ClientIDs),
	}
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return strings.TrimSpace(s[0])
}

// googleUserInfo is the identity a verified Google ID token carries.
type googleUserInfo struct {
	Subject       string          `json:"sub"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
}

// verified reports whether Google says the email is verified. Google sends a
// JSON boolean; some endpoints send the string "true".
func (u googleUserInfo) verified() bool {
	switch strings.TrimSpace(string(u.EmailVerified)) {
	case "true", `"true"`:
		return true
	}
	return false
}

// resolveUser finds or creates the Chorus user for a Google identity: by
// Google subject first, then by Google-verified email (linking the subject to
// that account and keeping its password), else a new passwordless account.
// An identity without a subject, without an email, or whose email Google has
// not verified is refused: it is never linked and never creates an account.
func (g *GoogleSignIn) resolveUser(ctx context.Context, info googleUserInfo) (db.User, error) {
	subject := strings.TrimSpace(info.Subject)
	email := users.NormalizeEmail(info.Email)
	if subject == "" || email == "" {
		return db.User{}, errors.New("identity missing subject or email")
	}
	if !info.verified() {
		return db.User{}, errors.New("identity email not verified")
	}
	if err := users.ValidateEmail(email); err != nil {
		return db.User{}, errors.New("identity email invalid")
	}
	sub := pgtype.Text{String: subject, Valid: true}

	user, err := g.q.GetUserByGoogleSubject(ctx, sub)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, fmt.Errorf("get user by subject: %w", err)
	}

	user, err = g.q.GetUserByEmail(ctx, email)
	switch {
	case err == nil:
		// An unverified password account could have been registered by
		// someone else who knows its password; linking it would hand them
		// the Google user's sign-in. Refuse until the email is verified.
		if !user.EmailVerified {
			return db.User{}, errors.New("matching account email not verified")
		}
		n, err := g.q.SetGoogleSubject(ctx, db.SetGoogleSubjectParams{ID: user.ID, GoogleSubject: sub})
		if err != nil {
			return db.User{}, fmt.Errorf("link google subject: %w", err)
		}
		if n == 0 {
			return db.User{}, errors.New("account already linked to another google identity")
		}
		user.GoogleSubject = sub
		return user, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return db.User{}, fmt.Errorf("get user by email: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return db.User{}, fmt.Errorf("generate user id: %w", err)
	}
	user, err = g.q.CreateGoogleUser(ctx, db.CreateGoogleUserParams{
		ID:            id,
		Email:         email,
		DisplayName:   googleDisplayName(info.Name, email),
		GoogleSubject: sub,
	})
	if err != nil {
		return db.User{}, fmt.Errorf("create google user: %w", err)
	}
	return user, nil
}

// googleDisplayName uses Google's name when it passes the display-name rules,
// otherwise the email's local part, capped to the 50-character limit.
func googleDisplayName(name, email string) string {
	var v validator
	if dn := v.displayName(name); v.err() == nil {
		return dn
	}
	local, _, _ := strings.Cut(email, "@")
	if local == "" {
		local = "Chorus user"
	}
	if utf8.RuneCountInString(local) > 50 {
		local = string([]rune(local)[:50])
	}
	return local
}

// randomURLToken returns 32 random bytes, URL-safe base64 without padding.
func randomURLToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
