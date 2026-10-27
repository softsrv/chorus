package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
)

const googleRevokeURL = "https://oauth2.googleapis.com/revoke"

// Google Calendar scopes. Connect asks for exactly these two: the calendar
// list (to pick a calendar) and read/write access to events.
const (
	ScopeCalendarListReadonly = "https://www.googleapis.com/auth/calendar.calendarlist.readonly"
	ScopeCalendarEvents       = "https://www.googleapis.com/auth/calendar.events"

	scopeCalendarPrefix = "https://www.googleapis.com/auth/calendar"
	scopeCalendarFull   = "https://www.googleapis.com/auth/calendar"
)

const (
	oauthPurposeCalendar = "calendar"
	calendarStateTTL     = 10 * time.Minute
	// accessTokenSkew refreshes a little before the recorded expiry so a token
	// isn't handed out moments before Google stops accepting it.
	accessTokenSkew = time.Minute
)

// Connection statuses and access levels reported by Status.
const (
	CalendarNotConnected = "not_connected"
	CalendarConnected    = "connected"
	CalendarBroken       = "broken"

	CalendarReadWrite = "read_write"
	CalendarReadOnly  = "read_only"
)

// googleCalendarScopes is the exact set Connect requests.
var googleCalendarScopes = []string{ScopeCalendarListReadonly, ScopeCalendarEvents}

var (
	// ErrGoogleCalendarCancelled means the person cancelled or denied on Google.
	ErrGoogleCalendarCancelled = errors.New("google calendar connect cancelled")
	// ErrGoogleCalendarFailed is the single generic outcome for every other
	// callback failure. The cause is logged, never shown.
	ErrGoogleCalendarFailed = errors.New("google calendar connect failed")
)

// GoogleCalendarConfig configures GoogleCalendar. The URL fields default to
// Google's endpoints when empty so tests can point the flow at a fake.
type GoogleCalendarConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string // APP_BASE_URL + /api/v1/me/google-calendar/callback
	AuthURL      string
	TokenURL     string
	RevokeURL    string
	HTTPClient   *http.Client
}

// CalendarStatus is a user's connection state. Access is empty when not
// connected.
type CalendarStatus struct {
	Status string
	Access string
}

// GoogleCalendar connects a Chorus user's Google Calendar: the OAuth
// authorization-code flow with PKCE and offline access, storing the tokens
// encrypted, one connection per user.
type GoogleCalendar struct {
	q         *db.Queries
	cipher    *TokenCipher
	oauth     oauth2.Config
	revokeURL string
	client    *http.Client
}

// NewGoogleCalendar builds the calendar connection service.
func NewGoogleCalendar(q *db.Queries, cipher *TokenCipher, cfg GoogleCalendarConfig) *GoogleCalendar {
	authURL := cfg.AuthURL
	if authURL == "" {
		authURL = googleAuthURL
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = googleTokenURL
	}
	revokeURL := cfg.RevokeURL
	if revokeURL == "" {
		revokeURL = googleRevokeURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: googleHTTPTimeout}
	}
	return &GoogleCalendar{
		q:      q,
		cipher: cipher,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       googleCalendarScopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   authURL,
				TokenURL:  tokenURL,
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		revokeURL: revokeURL,
		client:    client,
	}
}

// AuthCodeURL builds the Google authorization URL for a state value and PKCE
// verifier. Connect uses it; it is exported so its scopes can be checked.
func (g *GoogleCalendar) AuthCodeURL(state, verifier string) string {
	return g.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
}

// Connect mints a single-use state bound to userID (stored hashed, purpose
// calendar, with its PKCE verifier and a short expiry) and returns the URL to
// send the person to.
func (g *GoogleCalendar) Connect(ctx context.Context, userID uuid.UUID) (string, error) {
	state, err := randomURLToken()
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate state id: %w", err)
	}
	uid := userID
	if _, err := g.q.InsertOAuthState(ctx, db.InsertOAuthStateParams{
		ID:           id,
		StateHash:    auth.HashToken(state),
		Purpose:      oauthPurposeCalendar,
		UserID:       &uid,
		PkceVerifier: verifier,
		ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(calendarStateTTL), Valid: true},
	}); err != nil {
		return "", fmt.Errorf("insert oauth state: %w", err)
	}
	return g.AuthCodeURL(state, verifier), nil
}

// Callback completes the Google side of a calendar connect for the user the
// state was minted for. It returns ErrGoogleCalendarCancelled when Google
// reports an error, and ErrGoogleCalendarFailed for anything else that goes
// wrong; the cause is only logged.
func (g *GoogleCalendar) Callback(ctx context.Context, state, code, errParam string) error {
	if errParam != "" {
		slog.InfoContext(ctx, "google calendar: provider returned error", "error_param", truncate(errParam, 64))
		return ErrGoogleCalendarCancelled
	}
	if err := g.callback(ctx, state, code); err != nil {
		slog.WarnContext(ctx, "google calendar: callback refused", "error", err)
		return ErrGoogleCalendarFailed
	}
	return nil
}

func (g *GoogleCalendar) callback(ctx context.Context, state, code string) error {
	if state == "" {
		return errors.New("missing state")
	}
	if code == "" {
		return errors.New("missing code")
	}
	st, err := g.q.ConsumeOAuthState(ctx, db.ConsumeOAuthStateParams{
		StateHash: auth.HashToken(state),
		Purpose:   oauthPurposeCalendar,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("state unknown, used, expired or for another flow")
		}
		return fmt.Errorf("consume state: %w", err)
	}
	if st.UserID == nil {
		return errors.New("calendar state has no user")
	}
	userID := *st.UserID

	httpCtx := context.WithValue(ctx, oauth2.HTTPClient, g.client)
	tok, err := g.oauth.Exchange(httpCtx, code, oauth2.VerifierOption(st.PkceVerifier))
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}

	granted, _ := tok.Extra("scope").(string)
	access, ok := grantedAccess(granted)
	if !ok {
		// Nothing usable was granted: give the token back and store nothing.
		g.revoke(ctx, tok.AccessToken)
		return errors.New("no calendar scope granted")
	}

	accessEnc, err := g.cipher.Encrypt(tok.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt access token: %w", err)
	}

	var refreshEnc []byte
	if tok.RefreshToken != "" {
		if refreshEnc, err = g.cipher.Encrypt(tok.RefreshToken); err != nil {
			return fmt.Errorf("encrypt refresh token: %w", err)
		}
	} else {
		// Google only sends a refresh token on first consent. Without one the
		// connection can't outlive the access token, so it needs a stored
		// refresh token that still works (a broken row's does not).
		existing, err := g.q.GetGoogleCalendarConnectionByUser(ctx, userID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("no refresh token returned and none stored")
			}
			return fmt.Errorf("get connection: %w", err)
		}
		if existing.Broken {
			return errors.New("no refresh token returned and the stored one is broken")
		}
		refreshEnc = existing.RefreshTokenEnc
	}

	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate connection id: %w", err)
	}
	if _, err := g.q.UpsertGoogleCalendarConnection(ctx, db.UpsertGoogleCalendarConnectionParams{
		ID:              id,
		UserID:          userID,
		AccessTokenEnc:  accessEnc,
		RefreshTokenEnc: refreshEnc,
		TokenExpiry:     expiryTimestamp(tok.Expiry),
		AccessLevel:     access,
	}); err != nil {
		return fmt.Errorf("upsert connection: %w", err)
	}
	return nil
}

// grantedAccess decides the access level from the scope field of the token
// response: the scopes Google actually granted, which granular consent can
// make narrower than the request. Both the calendar list and read/write
// events → read_write; any other calendar scope → read_only; no calendar scope
// at all → not usable. An absent scope field is treated as nothing granted:
// Google always sends it, and assuming the request was granted in full is
// the mistake this check exists to prevent.
func grantedAccess(scope string) (string, bool) {
	var list, events, anyCalendar bool
	for _, s := range strings.Fields(scope) {
		switch s {
		case scopeCalendarFull:
			list, events = true, true
		case ScopeCalendarEvents:
			events = true
		case ScopeCalendarListReadonly, scopeCalendarPrefix + ".calendarlist", scopeCalendarPrefix + ".readonly":
			list = true
		}
		if s == scopeCalendarFull || strings.HasPrefix(s, scopeCalendarPrefix+".") {
			anyCalendar = true
		}
	}
	switch {
	case list && events:
		return CalendarReadWrite, true
	case anyCalendar:
		return CalendarReadOnly, true
	default:
		return "", false
	}
}

// Status reports userID's connection. When the stored access token has
// expired it refreshes it: Google rejecting the refresh (invalid_grant) marks
// the connection broken, a transient failure leaves it as it was.
func (g *GoogleCalendar) Status(ctx context.Context, userID uuid.UUID) (CalendarStatus, error) {
	conn, err := g.q.GetGoogleCalendarConnectionByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CalendarStatus{Status: CalendarNotConnected}, nil
		}
		return CalendarStatus{}, fmt.Errorf("get connection: %w", err)
	}
	if conn.Broken {
		return CalendarStatus{Status: CalendarBroken, Access: conn.AccessLevel}, nil
	}
	if conn.TokenExpiry.Valid && time.Now().Add(accessTokenSkew).After(conn.TokenExpiry.Time) {
		broken, err := g.refresh(ctx, conn)
		if err != nil {
			return CalendarStatus{}, err
		}
		if broken {
			return CalendarStatus{Status: CalendarBroken, Access: conn.AccessLevel}, nil
		}
	}
	return CalendarStatus{Status: CalendarConnected, Access: conn.AccessLevel}, nil
}

// refresh trades the stored refresh token for a new access token. It reports
// broken=true once the connection has been marked broken. Transient failures
// are logged and leave the row untouched.
func (g *GoogleCalendar) refresh(ctx context.Context, conn db.GoogleCalendarConnection) (bool, error) {
	refreshToken, err := g.cipher.Decrypt(conn.RefreshTokenEnc)
	if err != nil {
		// Unreadable under the current key: only a reconnect can fix it.
		slog.WarnContext(ctx, "google calendar: stored refresh token unreadable", "error", err)
		return g.markBroken(ctx, conn.UserID)
	}
	httpCtx := context.WithValue(ctx, oauth2.HTTPClient, g.client)
	tok, err := g.oauth.TokenSource(httpCtx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" &&
			(re.Response == nil || re.Response.StatusCode < http.StatusInternalServerError) {
			slog.InfoContext(ctx, "google calendar: refresh rejected, marking broken")
			return g.markBroken(ctx, conn.UserID)
		}
		slog.WarnContext(ctx, "google calendar: refresh failed, will retry later", "error", err)
		return false, nil
	}
	accessEnc, err := g.cipher.Encrypt(tok.AccessToken)
	if err != nil {
		return false, fmt.Errorf("encrypt access token: %w", err)
	}
	if _, err := g.q.UpdateGoogleCalendarAccessToken(ctx, db.UpdateGoogleCalendarAccessTokenParams{
		UserID:         conn.UserID,
		AccessTokenEnc: accessEnc,
		TokenExpiry:    expiryTimestamp(tok.Expiry),
	}); err != nil {
		return false, fmt.Errorf("update access token: %w", err)
	}
	return false, nil
}

func (g *GoogleCalendar) markBroken(ctx context.Context, userID uuid.UUID) (bool, error) {
	if _, err := g.q.MarkGoogleCalendarConnectionBroken(ctx, userID); err != nil {
		return false, fmt.Errorf("mark broken: %w", err)
	}
	return true, nil
}

// Disconnect deletes userID's connection and asks Google to revoke its grant.
// The revoke is best-effort: the connection is gone either way.
func (g *GoogleCalendar) Disconnect(ctx context.Context, userID uuid.UUID) error {
	conn, err := g.q.DeleteGoogleCalendarConnectionByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("delete connection: %w", err)
	}
	// Revoking the refresh token revokes the whole grant.
	token, err := g.cipher.Decrypt(conn.RefreshTokenEnc)
	if err != nil {
		slog.WarnContext(ctx, "google calendar: stored refresh token unreadable; skipping revoke", "error", err)
		return nil
	}
	g.revoke(ctx, token)
	return nil
}

// revoke asks Google to revoke token. Failures are logged and ignored.
func (g *GoogleCalendar) revoke(ctx context.Context, token string) {
	if token == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.revokeURL,
		strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		slog.WarnContext(ctx, "google calendar: build revoke request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.client.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "google calendar: revoke request failed", "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "google calendar: revoke refused", "status", resp.StatusCode)
	}
}

func expiryTimestamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}
