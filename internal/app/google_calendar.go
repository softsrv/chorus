package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
)

const (
	googleRevokeURL = "https://oauth2.googleapis.com/revoke"
	// googleCalendarAPIURL is the Calendar API v3 base. Chorus reads calendar
	// lists and entries from it; its only write is a PATCH of an entry's
	// title (summary) when a member tags the entry.
	googleCalendarAPIURL = "https://www.googleapis.com/calendar/v3"
)

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
	APIBaseURL   string // Calendar API v3 base; default googleCalendarAPIURL
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
	apiURL    string
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
	apiURL := strings.TrimRight(cfg.APIBaseURL, "/")
	if apiURL == "" {
		apiURL = googleCalendarAPIURL
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
		apiURL:    apiURL,
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

// CalendarStateTTL is how long a connect stays completable. The browser
// binding cookie should not outlive it.
const CalendarStateTTL = calendarStateTTL

// calendarStateHash is the stored hash of a calendar state. It covers both
// the state Google echoes back and the browser nonce held only in the
// initiating browser's cookie, so a callback completes only when the browser
// returning from Google is the one that started the connect.
func calendarStateHash(state, nonce string) string {
	return auth.HashToken(state + ":" + nonce)
}

// RedirectURL is the configured callback URL.
func (g *GoogleCalendar) RedirectURL() string { return g.oauth.RedirectURL }

// Connect mints a single-use state bound to userID and to a fresh browser
// nonce (stored hashed together, purpose calendar, with its PKCE verifier and
// a short expiry). It returns the URL to send the person to and the nonce,
// which the caller must hand only to the initiating browser (an HttpOnly
// cookie); the callback refuses without it.
func (g *GoogleCalendar) Connect(ctx context.Context, userID uuid.UUID) (string, string, error) {
	state, err := randomURLToken()
	if err != nil {
		return "", "", err
	}
	nonce, err := randomURLToken()
	if err != nil {
		return "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	id, err := uuid.NewV7()
	if err != nil {
		return "", "", fmt.Errorf("generate state id: %w", err)
	}
	uid := userID
	if _, err := g.q.InsertOAuthState(ctx, db.InsertOAuthStateParams{
		ID:           id,
		StateHash:    calendarStateHash(state, nonce),
		Purpose:      oauthPurposeCalendar,
		UserID:       &uid,
		PkceVerifier: verifier,
		ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(calendarStateTTL), Valid: true},
	}); err != nil {
		return "", "", fmt.Errorf("insert oauth state: %w", err)
	}
	return g.AuthCodeURL(state, verifier), nonce, nil
}

// Callback completes the Google side of a calendar connect for the user the
// state was minted for, provided nonce is the browser nonce Connect issued
// with that state. It returns ErrGoogleCalendarCancelled when Google reports
// an error, and ErrGoogleCalendarFailed for anything else that goes wrong
// (including a missing or mismatched nonce, which never reaches Google); the
// cause is only logged.
func (g *GoogleCalendar) Callback(ctx context.Context, state, code, errParam, nonce string) error {
	if errParam != "" {
		slog.InfoContext(ctx, "google calendar: provider returned error", "error_param", truncate(errParam, 64))
		return ErrGoogleCalendarCancelled
	}
	if err := g.callback(ctx, state, code, nonce); err != nil {
		slog.WarnContext(ctx, "google calendar: callback refused", "error", err)
		return ErrGoogleCalendarFailed
	}
	return nil
}

func (g *GoogleCalendar) callback(ctx context.Context, state, code, nonce string) error {
	if state == "" {
		return errors.New("missing state")
	}
	if code == "" {
		return errors.New("missing code")
	}
	if nonce == "" {
		// The browser finishing consent is not the one that started it.
		return errors.New("missing browser binding")
	}
	st, err := g.q.ConsumeOAuthState(ctx, db.ConsumeOAuthStateParams{
		StateHash: calendarStateHash(state, nonce),
		Purpose:   oauthPurposeCalendar,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("state unknown, used, expired, for another flow or another browser")
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
	if accessTokenExpired(conn) {
		_, broken, err := g.refresh(ctx, conn)
		if err != nil && !errors.Is(err, errRefreshTransient) {
			return CalendarStatus{}, err
		}
		if broken {
			return CalendarStatus{Status: CalendarBroken, Access: conn.AccessLevel}, nil
		}
	}
	return CalendarStatus{Status: CalendarConnected, Access: conn.AccessLevel}, nil
}

func accessTokenExpired(conn db.GoogleCalendarConnection) bool {
	return conn.TokenExpiry.Valid && time.Now().Add(accessTokenSkew).After(conn.TokenExpiry.Time)
}

// errRefreshTransient means Google could not be reached or answered with a
// temporary failure; the connection is left as it was.
var errRefreshTransient = errors.New("google calendar: token refresh failed transiently")

// refresh trades the stored refresh token for a new access token and returns
// it. It reports broken=true once the connection has been marked broken.
// Transient failures are logged, leave the row untouched and return
// errRefreshTransient.
func (g *GoogleCalendar) refresh(ctx context.Context, conn db.GoogleCalendarConnection) (string, bool, error) {
	refreshToken, err := g.cipher.Decrypt(conn.RefreshTokenEnc)
	if err != nil {
		// Unreadable under the current key: only a reconnect can fix it.
		slog.WarnContext(ctx, "google calendar: stored refresh token unreadable", "error", err)
		broken, err := g.markBroken(ctx, conn.UserID)
		return "", broken, err
	}
	httpCtx := context.WithValue(ctx, oauth2.HTTPClient, g.client)
	tok, err := g.oauth.TokenSource(httpCtx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" &&
			(re.Response == nil || re.Response.StatusCode < http.StatusInternalServerError) {
			slog.InfoContext(ctx, "google calendar: refresh rejected, marking broken")
			broken, err := g.markBroken(ctx, conn.UserID)
			return "", broken, err
		}
		slog.WarnContext(ctx, "google calendar: refresh failed, will retry later", "error", err)
		return "", false, errRefreshTransient
	}
	accessEnc, err := g.cipher.Encrypt(tok.AccessToken)
	if err != nil {
		return "", false, fmt.Errorf("encrypt access token: %w", err)
	}
	if _, err := g.q.UpdateGoogleCalendarAccessToken(ctx, db.UpdateGoogleCalendarAccessTokenParams{
		UserID:         conn.UserID,
		AccessTokenEnc: accessEnc,
		TokenExpiry:    expiryTimestamp(tok.Expiry),
	}); err != nil {
		return "", false, fmt.Errorf("update access token: %w", err)
	}
	return tok.AccessToken, false, nil
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

// ── Calendar API access ───────────────────────────────────────────────────────
//
// Chorus reads the calendar list (to pick a family calendar), events.list and
// events.get (to show and tag entries). Its one write is patchEventSummary: a
// PATCH whose body carries only the entry's summary, so start, end, location,
// description and attendees are never sent. There is deliberately no insert,
// delete, move, quickAdd or full-body update.

var (
	// errCalendarNotConnected means the user has no stored connection.
	errCalendarNotConnected = errors.New("google calendar not connected")
	// errCalendarBroken means the stored connection can't be used until the
	// user reconnects (refresh rejected, or Google refused the token).
	errCalendarBroken = errors.New("google calendar connection broken")
	// errGoogleUnavailable means Google could not be reached or failed
	// temporarily; nothing was changed.
	errGoogleUnavailable = errors.New("google calendar temporarily unavailable")
)

const (
	// calendarAPIPageSize and calendarAPIMaxPages bound one listing.
	calendarAPIPageSize = 250
	calendarAPIMaxPages = 10
)

// GoogleCalendarInfo is one calendar from a user's Google calendar list.
type GoogleCalendarInfo struct {
	ID      string
	Name    string
	Primary bool
}

// GoogleEntry is one entry (event occurrence) of a Google calendar. Start and
// End are RFC 3339 for timed entries and YYYY-MM-DD when AllDay. Editable is
// what Google reports for the token's owner: the calendar's accessRole is
// writer or owner and the entry is not locked.
type GoogleEntry struct {
	ID       string
	Title    string
	Start    string
	End      string
	AllDay   bool
	Location string
	Editable bool
}

// canEditEntry is Google's editability rule for one entry of a calendar.
func canEditEntry(accessRole string, locked bool) bool {
	return (accessRole == "writer" || accessRole == "owner") && !locked
}

// validAccessToken returns a usable, decrypted access token for userID,
// refreshing an expired one. It returns errCalendarNotConnected,
// errCalendarBroken (the row is, or has just been, marked broken) or
// errGoogleUnavailable (a transient refresh failure).
func (g *GoogleCalendar) validAccessToken(ctx context.Context, userID uuid.UUID) (string, error) {
	token, _, err := g.validConnection(ctx, userID)
	return token, err
}

// validConnection is validAccessToken that also returns the connection's
// access level (CalendarReadWrite or CalendarReadOnly).
func (g *GoogleCalendar) validConnection(ctx context.Context, userID uuid.UUID) (string, string, error) {
	conn, err := g.q.GetGoogleCalendarConnectionByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", errCalendarNotConnected
		}
		return "", "", fmt.Errorf("get connection: %w", err)
	}
	if conn.Broken {
		return "", conn.AccessLevel, errCalendarBroken
	}
	if !accessTokenExpired(conn) {
		if token, err := g.cipher.Decrypt(conn.AccessTokenEnc); err == nil && token != "" {
			return token, conn.AccessLevel, nil
		}
		// An unreadable access token is replaced by refreshing.
	}
	token, broken, err := g.refresh(ctx, conn)
	switch {
	case errors.Is(err, errRefreshTransient):
		return "", conn.AccessLevel, errGoogleUnavailable
	case err != nil:
		return "", conn.AccessLevel, err
	case broken:
		return "", conn.AccessLevel, errCalendarBroken
	}
	return token, conn.AccessLevel, nil
}

// googleAPIError is a non-2xx answer from the Calendar API. rateLimited is
// set when Google's error reason says the request was rate limited (Google
// reports some rate limits as 403). Nothing else of Google's body is kept.
type googleAPIError struct {
	status      int
	rateLimited bool
}

func (e *googleAPIError) Error() string {
	return fmt.Sprintf("google calendar api: status %d", e.status)
}

// transientByGoogle reports whether Google's answer is a temporary failure
// (429, any 5xx, or a 403 whose reason is a rate limit): nothing is wrong
// with the token or the calendar, and a retry may succeed.
func transientByGoogle(err error) bool {
	var apiErr *googleAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.rateLimited || apiErr.status == http.StatusTooManyRequests || apiErr.status >= 500
}

// refusedByGoogle reports whether Google refused the token or the calendar
// (401, 403 or 404, but not a rate-limit 403): something only the connecting
// member can fix.
func refusedByGoogle(err error) bool {
	var apiErr *googleAPIError
	if !errors.As(err, &apiErr) || apiErr.rateLimited {
		return false
	}
	switch apiErr.status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// googleRateLimited reports whether a Calendar API error body names a rate
// limit or quota reason. Only the reason codes are read; the body is never passed on.
func googleRateLimited(body io.Reader) bool {
	var out struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&out); err != nil {
		return false
	}
	for _, e := range out.Error.Errors {
		switch e.Reason {
		// Google's usageLimits family: per-user and per-project rate limits
		// and the daily / project quota. All are temporary; none means the
		// token or the calendar is refused.
		case "rateLimitExceeded", "userRateLimitExceeded", "dailyLimitExceeded", "quotaExceeded":
			return true
		}
	}
	return false
}

// apiGet sends one authorized GET to the Calendar API and decodes the JSON
// answer into out.
func (g *GoogleCalendar) apiGet(ctx context.Context, token, path string, query url.Values, out any) error {
	return g.apiDo(ctx, http.MethodGet, token, path, query, nil, out)
}

// apiDo sends one authorized request to the Calendar API, with body (when
// non-nil) as its JSON payload, and decodes a 2xx JSON answer into out. A
// non-2xx answer is a *googleAPIError carrying only the status and, for a
// 403, whether Google's reason was a rate limit; Google's text is never read
// into an error.
func (g *GoogleCalendar) apiDo(ctx context.Context, method, token, path string, query url.Values, body, out any) error {
	target := g.apiURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode calendar request: %w", err)
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return fmt.Errorf("build calendar request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "google calendar: api request failed", "error", err)
		return errGoogleUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &googleAPIError{status: resp.StatusCode}
		if resp.StatusCode == http.StatusForbidden {
			apiErr.rateLimited = googleRateLimited(resp.Body)
		}
		return apiErr
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode calendar response: %w", err)
	}
	return nil
}

// eventPath is the Calendar API path of one entry of calendarID.
func eventPath(calendarID, eventID string) string {
	return "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(eventID)
}

// calendarAccessRole returns the token owner's accessRole on calendarID
// (owner, writer, reader or freeBusyReader), from their calendar list.
func (g *GoogleCalendar) calendarAccessRole(ctx context.Context, token, calendarID string) (string, error) {
	var out struct {
		AccessRole string `json:"accessRole"`
	}
	if err := g.apiGet(ctx, token, "/users/me/calendarList/"+url.PathEscape(calendarID), nil, &out); err != nil {
		return "", err
	}
	return out.AccessRole, nil
}

// getEvent reads one entry of calendarID: its current title and whether the
// token's owner may edit it.
func (g *GoogleCalendar) getEvent(ctx context.Context, token, calendarID, eventID string) (GoogleEntry, error) {
	role, err := g.calendarAccessRole(ctx, token, calendarID)
	if err != nil {
		return GoogleEntry{}, err
	}
	var it struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Locked  bool   `json:"locked"`
	}
	if err := g.apiGet(ctx, token, eventPath(calendarID, eventID), nil, &it); err != nil {
		return GoogleEntry{}, err
	}
	if it.Status == "cancelled" {
		return GoogleEntry{}, &googleAPIError{status: http.StatusGone}
	}
	return GoogleEntry{ID: it.ID, Title: it.Summary, Editable: canEditEntry(role, it.Locked)}, nil
}

// eventSummaryPatch is the whole body of a tag change: only the summary.
type eventSummaryPatch struct {
	Summary string `json:"summary"`
}

// patchEventSummary changes only the title of one entry and returns the title
// Google stored.
func (g *GoogleCalendar) patchEventSummary(ctx context.Context, token, calendarID, eventID, summary string) (string, error) {
	var out struct {
		Summary string `json:"summary"`
	}
	if err := g.apiDo(ctx, http.MethodPatch, token, eventPath(calendarID, eventID), nil, eventSummaryPatch{Summary: summary}, &out); err != nil {
		return "", err
	}
	return out.Summary, nil
}

// listCalendars returns the calendars on the token owner's calendar list.
func (g *GoogleCalendar) listCalendars(ctx context.Context, token string) ([]GoogleCalendarInfo, error) {
	out := []GoogleCalendarInfo{}
	pageToken := ""
	for range calendarAPIMaxPages {
		q := url.Values{"maxResults": {strconv.Itoa(calendarAPIPageSize)}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var page struct {
			Items []struct {
				ID              string `json:"id"`
				Summary         string `json:"summary"`
				SummaryOverride string `json:"summaryOverride"`
				Primary         bool   `json:"primary"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := g.apiGet(ctx, token, "/users/me/calendarList", q, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if it.ID == "" {
				continue
			}
			name := it.SummaryOverride
			if name == "" {
				name = it.Summary
			}
			out = append(out, GoogleCalendarInfo{ID: it.ID, Name: name, Primary: it.Primary})
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

// listEvents returns the entries of calendarID overlapping [from, to), with
// recurring entries expanded into their occurrences. The calendar id always
// comes from the caller; there is no default calendar.
func (g *GoogleCalendar) listEvents(ctx context.Context, token, calendarID string, from, to time.Time) ([]GoogleEntry, error) {
	out := []GoogleEntry{}
	path := "/calendars/" + url.PathEscape(calendarID) + "/events"
	pageToken := ""
	for range calendarAPIMaxPages {
		q := url.Values{
			"timeMin":      {from.UTC().Format(time.RFC3339)},
			"timeMax":      {to.UTC().Format(time.RFC3339)},
			"singleEvents": {"true"},
			"orderBy":      {"startTime"},
			"maxResults":   {strconv.Itoa(calendarAPIPageSize)},
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var page struct {
			AccessRole string `json:"accessRole"`
			Items      []struct {
				ID       string     `json:"id"`
				Status   string     `json:"status"`
				Summary  string     `json:"summary"`
				Location string     `json:"location"`
				Locked   bool       `json:"locked"`
				Start    googleWhen `json:"start"`
				End      googleWhen `json:"end"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := g.apiGet(ctx, token, path, q, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if it.Status == "cancelled" {
				continue
			}
			e := GoogleEntry{ID: it.ID, Title: it.Summary, Location: it.Location, Editable: canEditEntry(page.AccessRole, it.Locked)}
			if it.Start.Date != "" {
				e.AllDay = true
				e.Start, e.End = it.Start.Date, it.End.Date
			} else {
				e.Start, e.End = it.Start.DateTime, it.End.DateTime
			}
			out = append(out, e)
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

// googleWhen is an event start or end: date for all-day entries, dateTime
// otherwise.
type googleWhen struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
}
