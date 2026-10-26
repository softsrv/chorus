package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/users"
)

// Google's public endpoints. Each can be overridden in GoogleSignInConfig so
// tests can point the flow at a fake.
const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

const (
	// oauthPurposeSignIn tags state rows minted by this flow. The calendar
	// connect flow shares oauth_states with its own purpose, and
	// ConsumeOAuthState filters on purpose so one can't complete the other.
	oauthPurposeSignIn = "sign_in"
	signInStateTTL     = 10 * time.Minute
	signInHandoffTTL   = 2 * time.Minute
	googleHTTPTimeout  = 10 * time.Second
)

// googleSignInScopes are the only scopes the sign-in flow ever asks for:
// identity, never calendar access.
var googleSignInScopes = []string{"openid", "email", "profile"}

var (
	// ErrGoogleSignInCancelled means the person cancelled or denied on Google.
	ErrGoogleSignInCancelled = errors.New("google sign-in cancelled")
	// ErrGoogleSignInFailed is the single generic outcome for every other
	// callback failure. The cause is logged, never shown.
	ErrGoogleSignInFailed = errors.New("google sign-in failed")
	// ErrSignInCodeInvalid means a hand-off code is unknown, used or expired.
	ErrSignInCodeInvalid = errors.New("sign-in code is invalid or expired")
)

// GoogleSignInConfig configures GoogleSignIn. The URL fields default to
// Google's endpoints when empty.
type GoogleSignInConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string // APP_BASE_URL + /api/v1/auth/google/callback
	AuthURL      string
	TokenURL     string
	UserInfoURL  string
	HTTPClient   *http.Client
}

// GoogleSignIn runs "Sign in with Google": the OAuth authorization-code flow
// with PKCE, then a short-lived single-use hand-off code that the web client
// redeems for the standard token pair.
type GoogleSignIn struct {
	q           *db.Queries
	authSvc     *AuthService
	oauth       oauth2.Config
	userInfoURL string
	client      *http.Client
}

// NewGoogleSignIn builds the sign-in service. authSvc issues the token pair.
func NewGoogleSignIn(q *db.Queries, authSvc *AuthService, cfg GoogleSignInConfig) *GoogleSignIn {
	authURL := cfg.AuthURL
	if authURL == "" {
		authURL = googleAuthURL
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = googleTokenURL
	}
	userInfoURL := cfg.UserInfoURL
	if userInfoURL == "" {
		userInfoURL = googleUserInfoURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: googleHTTPTimeout}
	}
	return &GoogleSignIn{
		q:       q,
		authSvc: authSvc,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       googleSignInScopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   authURL,
				TokenURL:  tokenURL,
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		userInfoURL: userInfoURL,
		client:      client,
	}
}

// AuthCodeURL builds the Google authorization URL for a state value and PKCE
// verifier. Start uses it; it is exported so its scopes can be checked.
func (g *GoogleSignIn) AuthCodeURL(state, verifier string) string {
	return g.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "select_account"),
	)
}

// Start mints a single-use state (stored hashed, purpose sign_in, with its
// PKCE verifier and a short expiry) and returns the URL to send the person to.
func (g *GoogleSignIn) Start(ctx context.Context) (string, error) {
	state, err := randomURLToken()
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate state id: %w", err)
	}
	if _, err := g.q.InsertOAuthState(ctx, db.InsertOAuthStateParams{
		ID:           id,
		StateHash:    auth.HashToken(state),
		Purpose:      oauthPurposeSignIn,
		PkceVerifier: verifier,
		ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(signInStateTTL), Valid: true},
	}); err != nil {
		return "", fmt.Errorf("insert oauth state: %w", err)
	}
	return g.AuthCodeURL(state, verifier), nil
}

// Callback completes the Google side of sign-in and returns a raw hand-off
// code for the web client. It returns ErrGoogleSignInCancelled when Google
// reports an error (the person cancelled or denied), and ErrGoogleSignInFailed
// for anything else that goes wrong; the cause is only logged.
func (g *GoogleSignIn) Callback(ctx context.Context, state, code, errParam string) (string, error) {
	if errParam != "" {
		slog.InfoContext(ctx, "google sign-in: provider returned error", "error_param", truncate(errParam, 64))
		return "", ErrGoogleSignInCancelled
	}
	handoff, err := g.callback(ctx, state, code)
	if err != nil {
		slog.WarnContext(ctx, "google sign-in: callback refused", "error", err)
		return "", ErrGoogleSignInFailed
	}
	return handoff, nil
}

func (g *GoogleSignIn) callback(ctx context.Context, state, code string) (string, error) {
	if state == "" {
		return "", errors.New("missing state")
	}
	if code == "" {
		return "", errors.New("missing code")
	}
	st, err := g.q.ConsumeOAuthState(ctx, db.ConsumeOAuthStateParams{
		StateHash: auth.HashToken(state),
		Purpose:   oauthPurposeSignIn,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errors.New("state unknown, used, expired or for another flow")
		}
		return "", fmt.Errorf("consume state: %w", err)
	}

	httpCtx := context.WithValue(ctx, oauth2.HTTPClient, g.client)
	tok, err := g.oauth.Exchange(httpCtx, code, oauth2.VerifierOption(st.PkceVerifier))
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	if err := requireGrantedScope(tok, "email"); err != nil {
		return "", err
	}

	info, err := g.fetchUserInfo(httpCtx, tok)
	if err != nil {
		return "", err
	}
	user, err := g.resolveUser(ctx, info)
	if err != nil {
		return "", err
	}
	return g.mintHandoff(ctx, user.ID)
}

// requireGrantedScope checks the scopes Google actually granted. Granular
// consent can grant less than was asked for, so a successful exchange alone
// does not prove the access was granted. Per RFC 6749 §5.1 an absent scope
// field means the requested scopes were granted in full.
func requireGrantedScope(tok *oauth2.Token, want string) error {
	granted, _ := tok.Extra("scope").(string)
	if granted == "" {
		return nil
	}
	for _, s := range strings.Fields(granted) {
		if s == want || s == "https://www.googleapis.com/auth/userinfo."+want {
			return nil
		}
	}
	return fmt.Errorf("scope %q not granted", want)
}

// googleUserInfo is the subset of the OpenID Connect userinfo response used.
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

func (g *GoogleSignIn) fetchUserInfo(ctx context.Context, tok *oauth2.Token) (googleUserInfo, error) {
	var info googleUserInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.userInfoURL, nil)
	if err != nil {
		return info, fmt.Errorf("build userinfo request: %w", err)
	}
	tok.SetAuthHeader(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return info, fmt.Errorf("userinfo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return info, fmt.Errorf("decode userinfo: %w", err)
	}
	return info, nil
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

func (g *GoogleSignIn) mintHandoff(ctx context.Context, userID uuid.UUID) (string, error) {
	raw, err := randomURLToken()
	if err != nil {
		return "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate handoff id: %w", err)
	}
	if _, err := g.q.InsertSignInHandoff(ctx, db.InsertSignInHandoffParams{
		ID:        id,
		CodeHash:  auth.HashToken(raw),
		UserID:    userID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(signInHandoffTTL), Valid: true},
	}); err != nil {
		return "", fmt.Errorf("insert handoff: %w", err)
	}
	return raw, nil
}

// Redeem exchanges a hand-off code for the standard token pair. The code is
// consumed by a conditional UPDATE, so it works once and only before it
// expires; anything else is ErrSignInCodeInvalid and creates no session.
func (g *GoogleSignIn) Redeem(ctx context.Context, code string, meta DeviceMeta) (TokenResult, error) {
	if code == "" {
		return TokenResult{}, ErrSignInCodeInvalid
	}
	userID, err := g.q.RedeemSignInHandoff(ctx, auth.HashToken(code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenResult{}, ErrSignInCodeInvalid
		}
		return TokenResult{}, fmt.Errorf("redeem handoff: %w", err)
	}
	user, err := g.q.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenResult{}, ErrSignInCodeInvalid
		}
		return TokenResult{}, fmt.Errorf("get user: %w", err)
	}
	return g.authSvc.issueTokenPair(ctx, user, meta)
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
