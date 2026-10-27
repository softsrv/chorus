//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

const (
	calScopeList     = "https://www.googleapis.com/auth/calendar.calendarlist.readonly"
	calScopeEvents   = "https://www.googleapis.com/auth/calendar.events"
	calScopeEventsRO = "https://www.googleapis.com/auth/calendar.events.readonly"
	calBothScopes    = calScopeList + " " + calScopeEvents
	calCallbackURL   = "http://chorus.test/api/v1/me/google-calendar/callback"
)

// calendarKey is the 32-byte token key the test service encrypts with.
var calendarKey = []byte("0123456789abcdef0123456789abcdef")

// fakeCalendarGoogle serves Google's token endpoint (code exchange and
// refresh) and revoke endpoint, counting hits on each.
type fakeCalendarGoogle struct {
	srv         *httptest.Server
	codeHits    atomic.Int64
	refreshHits atomic.Int64
	revokeHits  atomic.Int64

	mu            sync.Mutex
	tokenStatus   int    // code exchange: 0 = 200
	grantedScope  string // scope field of the code-exchange response; "" = omitted
	refreshToken  string // refresh_token in the code-exchange response; "" = omitted
	customAccess  string // if non-empty, access_token in the code-exchange response
	refreshStatus int    // refresh grant: 0 = 200
	refreshError  string // error code in a refused refresh body
	revokeStatus  int    // 0 = 200
	revoked       []string
	issued        []string // access tokens handed out, in order
	refreshedWith []string // refresh tokens sent to the refresh endpoint
}

func newFakeCalendarGoogle(t *testing.T) *fakeCalendarGoogle {
	t.Helper()
	f := &fakeCalendarGoogle{grantedScope: calBothScopes, refreshToken: "refresh-" + uuid.NewString()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.codeHits.Add(1)
			if f.tokenStatus != 0 && f.tokenStatus != http.StatusOK {
				w.WriteHeader(f.tokenStatus)
				fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"%s"}`, sentinel)
				return
			}
			if r.Form.Get("code") == "" || r.Form.Get("code_verifier") == "" || r.Form.Get("redirect_uri") != calCallbackURL {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":"invalid_request","error_description":"%s"}`, sentinel)
				return
			}
			access := f.customAccess
			if access == "" {
				access = "cal-access-" + uuid.NewString()
			}
			f.issued = append(f.issued, access)
			resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600}
			if f.grantedScope != "" {
				resp["scope"] = f.grantedScope
			}
			if f.refreshToken != "" {
				resp["refresh_token"] = f.refreshToken
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "refresh_token":
			f.refreshHits.Add(1)
			f.refreshedWith = append(f.refreshedWith, r.Form.Get("refresh_token"))
			if f.refreshStatus != 0 && f.refreshStatus != http.StatusOK {
				w.WriteHeader(f.refreshStatus)
				if f.refreshError != "" {
					fmt.Fprintf(w, `{"error":"%s","error_description":"%s"}`, f.refreshError, sentinel)
				} else {
					fmt.Fprint(w, sentinel)
				}
				return
			}
			access := "cal-refreshed-" + uuid.NewString()
			f.issued = append(f.issued, access)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600})
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"unsupported_grant_type"}`)
		}
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		f.revokeHits.Add(1)
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.revoked = append(f.revoked, r.Form.Get("token"))
		if f.revokeStatus != 0 {
			w.WriteHeader(f.revokeStatus)
			fmt.Fprint(w, sentinel)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCalendarGoogle) set(fn func(f *fakeCalendarGoogle)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeCalendarGoogle) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

func (f *fakeCalendarGoogle) lastIssued() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.issued) == 0 {
		return ""
	}
	return f.issued[len(f.issued)-1]
}

func (f *fakeCalendarGoogle) lastRefreshToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refreshedWith) == 0 {
		return ""
	}
	return f.refreshedWith[len(f.refreshedWith)-1]
}

type calendarServer struct {
	*testServer
	cipher *app.TokenCipher
}

// newCalendarTestServer is newTestServer with Google Calendar wired to fake
// (or unavailable when fake is nil). The authorization URL keeps Google's
// real endpoint (it is only parsed, never fetched); the token and revoke
// endpoints point at the fake.
func newCalendarTestServer(t *testing.T, fake *fakeCalendarGoogle) *calendarServer {
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

	q := db.New(pool)
	mail := &mailbox{text: map[string]string{}}
	authSvc := app.NewAuthService(q, pool, mail, app.AuthServiceConfig{
		JWTSecret: secret, AccessExpiry: 15 * time.Minute, RefreshExpiry: 24 * time.Hour,
		BCryptCost: 10, PasswordMinLen: 8, AppBaseURL: "http://chorus.test", AppName: "Chorus",
	})
	t.Cleanup(authSvc.Shutdown)

	cipher, err := app.NewTokenCipher(calendarKey)
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	var calendar *app.GoogleCalendar
	if fake != nil {
		calendar = app.NewGoogleCalendar(q, cipher, app.GoogleCalendarConfig{
			ClientID: "client-id", ClientSecret: "client-secret",
			RedirectURL: calCallbackURL,
			TokenURL:    fake.srv.URL + "/token",
			RevokeURL:   fake.srv.URL + "/revoke",
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:        app.NewUserService(q),
		HouseholdSvc:   app.NewHouseholdService(q, pool),
		ChoreSvc:       app.NewChoreService(q),
		EventSvc:       app.NewEventService(q),
		GoogleCalendar: calendar,
		JWTSecret:      secret,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &calendarServer{testServer: &testServer{t: t, srv: srv, mail: mail, pool: pool}, cipher: cipher}
}

type calendarStatus struct {
	Status string `json:"status"`
	Access string `json:"access"`
}

func (s *calendarServer) status(token string) calendarStatus {
	s.t.Helper()
	var st calendarStatus
	s.expect(http.StatusOK, "GET", "/api/v1/me/google-calendar", token, nil, &st)
	return st
}

func (s *calendarServer) doRaw(method, path, token string, body any) (int, string) {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// connect calls the connect route and returns the parsed authorization URL.
func (s *calendarServer) connect(token string) *url.URL {
	s.t.Helper()
	var out struct {
		URL string `json:"url"`
	}
	s.expect(http.StatusOK, "POST", "/api/v1/me/google-calendar/connect", token, nil, &out)
	u, err := url.Parse(out.URL)
	if err != nil || u.Query().Get("state") == "" {
		s.t.Fatalf("connect url %q: want a URL with a state (%v)", out.URL, err)
	}
	return u
}

// calCallback GETs the callback and returns the Location and body. It fails
// unless the response is a 303 to the Account page.
func (s *calendarServer) calCallback(query url.Values) (string, string) {
	s.t.Helper()
	status, loc, body := s.get("/api/v1/me/google-calendar/callback?" + query.Encode())
	if status != http.StatusSeeOther {
		s.t.Fatalf("callback: status %d, want 303 (Location %q body %q)", status, loc, body)
	}
	return loc, body
}

const (
	calConnected = "/account?google_calendar=connected"
	calFailed    = "/account?google_calendar=failed"
)

// connectFlow runs connect + callback and returns the callback Location.
func (s *calendarServer) connectFlow(token string) string {
	s.t.Helper()
	state := s.connect(token).Query().Get("state")
	loc, _ := s.calCallback(url.Values{"state": {state}, "code": {"code-" + uuid.NewString()}})
	return loc
}

func (s *calendarServer) connectionCount(userID uuid.UUID) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM google_calendar_connections WHERE user_id = $1", userID).Scan(&n); err != nil {
		s.t.Fatalf("count connections: %v", err)
	}
	return n
}

func (s *calendarServer) connection(userID uuid.UUID) db.GoogleCalendarConnection {
	s.t.Helper()
	c, err := db.New(s.pool).GetGoogleCalendarConnectionByUser(context.Background(), userID)
	if err != nil {
		s.t.Fatalf("get connection: %v", err)
	}
	return c
}

func (s *calendarServer) decrypt(b []byte) string {
	s.t.Helper()
	v, err := s.cipher.Decrypt(b)
	if err != nil {
		s.t.Fatalf("decrypt: %v", err)
	}
	return v
}

// seedConnection stores a connection directly, encrypted with the service key.
func (s *calendarServer) seedConnection(userID uuid.UUID, access, refresh, level string, expiry time.Time, broken bool) {
	s.t.Helper()
	ctx := context.Background()
	ae, _ := s.cipher.Encrypt(access)
	re, _ := s.cipher.Encrypt(refresh)
	q := db.New(s.pool)
	if _, err := q.UpsertGoogleCalendarConnection(ctx, db.UpsertGoogleCalendarConnectionParams{
		ID: uuid.Must(uuid.NewV7()), UserID: userID, AccessTokenEnc: ae, RefreshTokenEnc: re,
		TokenExpiry: pgtype.Timestamptz{Time: expiry, Valid: true}, AccessLevel: level,
	}); err != nil {
		s.t.Fatalf("seed connection: %v", err)
	}
	if broken {
		if _, err := q.MarkGoogleCalendarConnectionBroken(ctx, userID); err != nil {
			s.t.Fatalf("mark broken: %v", err)
		}
	}
}

// insertCalendarState stores a calendar-purpose state for userID directly.
func (s *calendarServer) insertCalendarState(purpose string, userID uuid.UUID, expiresAt time.Time) string {
	s.t.Helper()
	raw := "test-state-" + uuid.NewString()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO oauth_states (id, state_hash, purpose, user_id, pkce_verifier, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.Must(uuid.NewV7()), auth.HashToken(raw), purpose, userID, "verifier-"+uuid.NewString(), expiresAt); err != nil {
		s.t.Fatalf("insert state: %v", err)
	}
	return raw
}

// CLM-1: status is not_connected for a verified user without a connection,
// connected after a connect; unverified and anonymous callers are refused.
// With Google Calendar unconfigured status reports unavailable.
func TestGoogleCalendar_Status(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calstatus")

	if st := s.status(user.AccessToken); st.Status != "not_connected" || st.Access != "" {
		t.Fatalf("status = %+v, want not_connected without access", st)
	}
	if loc := s.connectFlow(user.AccessToken); loc != calConnected {
		t.Fatalf("callback Location %q, want %q", loc, calConnected)
	}
	if st := s.status(user.AccessToken); st.Status != "connected" || st.Access != "read_write" {
		t.Errorf("status after connect = %+v, want connected read_write", st)
	}

	var e apiError
	if status := s.do("GET", "/api/v1/me/google-calendar", "", nil, &e); status != http.StatusUnauthorized {
		t.Errorf("anonymous status: %d, want 401", status)
	}
	unverified := s.signUp("calunverified")
	if _, err := s.pool.Exec(context.Background(), "UPDATE users SET email_verified = false WHERE id = $1", unverified.User.ID); err != nil {
		t.Fatalf("unverify: %v", err)
	}
	e = apiError{}
	if status := s.do("GET", "/api/v1/me/google-calendar", unverified.AccessToken, nil, &e); status != http.StatusForbidden || e.Error.Code != "email_not_verified" {
		t.Errorf("unverified status: %d %q, want 403 email_not_verified", status, e.Error.Code)
	}
	e = apiError{}
	if status := s.do("POST", "/api/v1/me/google-calendar/connect", unverified.AccessToken, nil, &e); status != http.StatusForbidden || e.Error.Code != "email_not_verified" {
		t.Errorf("unverified connect: %d %q, want 403 email_not_verified", status, e.Error.Code)
	}
	e = apiError{}
	if status := s.do("DELETE", "/api/v1/me/google-calendar", unverified.AccessToken, nil, &e); status != http.StatusForbidden || e.Error.Code != "email_not_verified" {
		t.Errorf("unverified disconnect: %d %q, want 403 email_not_verified", status, e.Error.Code)
	}

	fakeDisabled := newFakeCalendarGoogle(t)
	off := newCalendarTestServer(t, nil)
	offUser := off.signUp("caloff")
	if st := off.status(offUser.AccessToken); st.Status != "unavailable" {
		t.Errorf("unconfigured status = %+v, want unavailable", st)
	}
	if status := off.do("POST", "/api/v1/me/google-calendar/connect", offUser.AccessToken, nil, &e); status != http.StatusNotFound {
		t.Errorf("unconfigured connect: %d, want 404", status)
	}
	if status, _, _ := off.get("/api/v1/me/google-calendar/callback?state=x&code=y"); status != http.StatusNotFound {
		t.Errorf("unconfigured callback: %d, want 404", status)
	}
	if hits := fakeDisabled.codeHits.Load(); hits != 0 {
		t.Errorf("fake codeHits = %d, want 0", hits)
	}
	if hits := fakeDisabled.refreshHits.Load(); hits != 0 {
		t.Errorf("fake refreshHits = %d, want 0", hits)
	}
	if hits := fakeDisabled.revokeHits.Load(); hits != 0 {
		t.Errorf("fake revokeHits = %d, want 0", hits)
	}
}

// CLM-2: connect returns Google's authorization URL with exactly the two
// calendar scopes, offline access, forced consent and the callback
// redirect_uri, and stores a hashed calendar state bound to the caller.
func TestGoogleCalendar_ConnectURLAndState(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calconnect")

	u := s.connect(user.AccessToken)
	q := u.Query()
	if u.Scheme != "https" || u.Host != "accounts.google.com" {
		t.Errorf("auth URL host %s://%s, want https://accounts.google.com", u.Scheme, u.Host)
	}
	scopes := strings.Fields(q.Get("scope"))
	sort.Strings(scopes)
	if strings.Join(scopes, " ") != calScopeList+" "+calScopeEvents {
		t.Errorf("scopes = %v, want exactly [%s %s]", scopes, calScopeList, calScopeEvents)
	}
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("redirect_uri") != calCallbackURL {
		t.Errorf("access_type=%q prompt=%q redirect_uri=%q", q.Get("access_type"), q.Get("prompt"), q.Get("redirect_uri"))
	}

	var (
		purpose   string
		owner     *uuid.UUID
		expiresAt time.Time
		usedAt    pgtype.Timestamptz
	)
	if err := s.pool.QueryRow(context.Background(),
		"SELECT purpose, user_id, expires_at, used_at FROM oauth_states WHERE state_hash = $1",
		auth.HashToken(q.Get("state"))).Scan(&purpose, &owner, &expiresAt, &usedAt); err != nil {
		t.Fatalf("state row for the returned state (stored hashed): %v", err)
	}
	if purpose != "calendar" || owner == nil || *owner != user.User.ID || !expiresAt.After(time.Now()) || usedAt.Valid {
		t.Errorf("state row purpose=%q user=%v expires=%v used=%v; want calendar, caller, future, unused", purpose, owner, expiresAt, usedAt.Valid)
	}
	if fake.codeHits.Load() != 0 {
		t.Error("connect contacted Google's token endpoint")
	}
}

// CLM-3: a valid callback stores one read_write connection with encrypted
// tokens, consumes the state, and redirects to the Account page with success.
func TestGoogleCalendar_CallbackSuccess(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calsuccess")

	state := s.connect(user.AccessToken).Query().Get("state")
	loc, _ := s.calCallback(url.Values{"state": {state}, "code": {"code-1"}})
	if loc != calConnected {
		t.Fatalf("Location %q, want %q", loc, calConnected)
	}
	if n := s.connectionCount(user.User.ID); n != 1 {
		t.Fatalf("connections = %d, want 1", n)
	}
	c := s.connection(user.User.ID)
	if c.AccessLevel != "read_write" || c.Broken {
		t.Errorf("row access=%q broken=%v, want read_write, not broken", c.AccessLevel, c.Broken)
	}
	if got := s.decrypt(c.AccessTokenEnc); got != fake.lastIssued() {
		t.Errorf("stored access token %q, want %q", got, fake.lastIssued())
	}
	if got := s.decrypt(c.RefreshTokenEnc); got != fake.refreshToken {
		t.Errorf("stored refresh token %q, want %q", got, fake.refreshToken)
	}
	if strings.Contains(string(c.AccessTokenEnc), "cal-access-") || strings.Contains(string(c.RefreshTokenEnc), "refresh-") {
		t.Error("tokens stored in plaintext")
	}

	hits := fake.codeHits.Load()
	if loc, _ := s.calCallback(url.Values{"state": {state}, "code": {"code-2"}}); loc != calFailed {
		t.Errorf("replayed state Location %q, want %q", loc, calFailed)
	}
	if fake.codeHits.Load() != hits {
		t.Error("replayed state reached the token endpoint")
	}
	if n := s.connectionCount(user.User.ID); n != 1 {
		t.Errorf("connections after replay = %d, want 1", n)
	}
	if st := s.status(user.AccessToken); st.Status != "connected" || st.Access != "read_write" {
		t.Errorf("status = %+v, want connected read_write", st)
	}
}

// CLM-5: cancel, a bad state or a failed exchange stores nothing, never
// exchanges for a bad state, and redirects with only a generic failure.
func TestGoogleCalendar_CallbackRefusals(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calrefuse")
	other := s.signUp("calrefuseother")

	cases := []struct {
		name       string
		badState   bool // the token endpoint must not be hit
		setup      func() url.Values
		connection uuid.UUID
	}{
		{"cancel with valid state", true, func() url.Values {
			return url.Values{"state": {s.connect(user.AccessToken).Query().Get("state")}, "error": {"access_denied"}}
		}, user.User.ID},
		{"no state", true, func() url.Values { return url.Values{"code": {"c"}} }, user.User.ID},
		{"random state", true, func() url.Values {
			return url.Values{"state": {"random-" + uuid.NewString()}, "code": {"c"}}
		}, user.User.ID},
		{"expired state", true, func() url.Values {
			return url.Values{"state": {s.insertCalendarState("calendar", user.User.ID, time.Now().Add(-time.Minute))}, "code": {"c"}}
		}, user.User.ID},
		{"replayed state", true, func() url.Values {
			state := s.connect(other.AccessToken).Query().Get("state")
			fake.set(func(f *fakeCalendarGoogle) { f.tokenStatus = http.StatusBadRequest })
			if loc, _ := s.calCallback(url.Values{"state": {state}, "code": {"c"}}); loc != calFailed {
				t.Fatalf("setup: first use Location %q", loc)
			}
			fake.set(func(f *fakeCalendarGoogle) { f.tokenStatus = 0 })
			return url.Values{"state": {state}, "code": {"c"}}
		}, other.User.ID},
		{"sign-in purpose state", true, func() url.Values {
			return url.Values{"state": {s.insertCalendarState("sign_in", user.User.ID, time.Now().Add(10*time.Minute))}, "code": {"c"}}
		}, user.User.ID},
		{"token endpoint 400", false, func() url.Values {
			fake.set(func(f *fakeCalendarGoogle) { f.tokenStatus = http.StatusBadRequest })
			return url.Values{"state": {s.connect(user.AccessToken).Query().Get("state")}, "code": {"c"}}
		}, user.User.ID},
		{"token endpoint 500", false, func() url.Values {
			fake.set(func(f *fakeCalendarGoogle) { f.tokenStatus = http.StatusInternalServerError })
			return url.Values{"state": {s.connect(user.AccessToken).Query().Get("state")}, "code": {"c"}}
		}, user.User.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.set(func(f *fakeCalendarGoogle) { f.tokenStatus = 0 })
			query := tc.setup()
			hits := fake.codeHits.Load()
			loc, body := s.calCallback(query)
			if loc != calFailed {
				t.Errorf("Location %q, want exactly %q", loc, calFailed)
			}
			for _, leak := range []string{sentinel, "invalid_grant", "oauth2", "pgx", "sql", "state", "exchange"} {
				if strings.Contains(body, leak) || strings.Contains(loc, leak) {
					t.Errorf("response leaks %q: Location %q body %q", leak, loc, body)
				}
			}
			if tc.badState && fake.codeHits.Load() != hits {
				t.Error("token endpoint was hit for a refused state")
			}
			if n := s.connectionCount(tc.connection); n != 0 {
				t.Errorf("connections = %d, want 0", n)
			}
		})
	}
}

// CLM-6: the access level comes from the scopes Google granted; with no
// calendar scope the token is revoked and nothing is stored.
func TestGoogleCalendar_GrantedScopes(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)

	t.Run("both scopes", func(t *testing.T) {
		u := s.signUp("calboth")
		fake.set(func(f *fakeCalendarGoogle) { f.grantedScope = calBothScopes })
		if loc := s.connectFlow(u.AccessToken); loc != calConnected {
			t.Fatalf("Location %q", loc)
		}
		if st := s.status(u.AccessToken); st.Status != "connected" || st.Access != "read_write" {
			t.Errorf("status = %+v, want connected read_write", st)
		}
	})
	t.Run("read without events", func(t *testing.T) {
		u := s.signUp("calro")
		fake.set(func(f *fakeCalendarGoogle) { f.grantedScope = calScopeList + " " + calScopeEventsRO })
		if loc := s.connectFlow(u.AccessToken); loc != calConnected {
			t.Fatalf("Location %q", loc)
		}
		if c := s.connection(u.User.ID); c.AccessLevel != "read_only" {
			t.Errorf("row access = %q, want read_only", c.AccessLevel)
		}
		if st := s.status(u.AccessToken); st.Status != "connected" || st.Access != "read_only" {
			t.Errorf("status = %+v, want connected read_only", st)
		}
	})
	t.Run("no calendar scope", func(t *testing.T) {
		u := s.signUp("calnone")
		fake.set(func(f *fakeCalendarGoogle) { f.grantedScope = "openid email" })
		revokedBefore := len(fake.revokedTokens())
		if loc := s.connectFlow(u.AccessToken); loc != calFailed {
			t.Errorf("Location %q, want %q", loc, calFailed)
		}
		if n := s.connectionCount(u.User.ID); n != 0 {
			t.Errorf("connections = %d, want 0", n)
		}
		revoked := fake.revokedTokens()
		if len(revoked) != revokedBefore+1 || revoked[len(revoked)-1] != fake.lastIssued() {
			t.Errorf("revoked %v, want the issued token %q", revoked[revokedBefore:], fake.lastIssued())
		}
		if st := s.status(u.AccessToken); st.Status != "not_connected" {
			t.Errorf("status = %+v, want not_connected", st)
		}
	})
	t.Run("scope field omitted", func(t *testing.T) {
		u := s.signUp("calomitted")
		fake.set(func(f *fakeCalendarGoogle) { f.grantedScope = "" })
		if loc := s.connectFlow(u.AccessToken); loc != calFailed {
			t.Errorf("Location %q, want %q", loc, calFailed)
		}
		if n := s.connectionCount(u.User.ID); n != 0 {
			t.Errorf("connections = %d, want 0", n)
		}
	})
}

// CLM-7: without a refresh token a callback is only stored when the user
// already holds one, which is kept while the access token is updated.
func TestGoogleCalendar_NoRefreshToken(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	fake.set(func(f *fakeCalendarGoogle) { f.refreshToken = "" })

	t.Run("no stored refresh token", func(t *testing.T) {
		u := s.signUp("calnorefresh")
		if loc := s.connectFlow(u.AccessToken); loc != calFailed {
			t.Errorf("Location %q, want %q", loc, calFailed)
		}
		if n := s.connectionCount(u.User.ID); n != 0 {
			t.Errorf("connections = %d, want 0", n)
		}
		if st := s.status(u.AccessToken); st.Status != "not_connected" {
			t.Errorf("status = %+v, want not_connected", st)
		}
	})
	t.Run("stored refresh token kept", func(t *testing.T) {
		u := s.signUp("calkeeprefresh")
		s.seedConnection(u.User.ID, "old-access", "stored-refresh-R", "read_write", time.Now().Add(time.Hour), false)
		if loc := s.connectFlow(u.AccessToken); loc != calConnected {
			t.Fatalf("Location %q, want %q", loc, calConnected)
		}
		c := s.connection(u.User.ID)
		if got := s.decrypt(c.RefreshTokenEnc); got != "stored-refresh-R" {
			t.Errorf("refresh token = %q, want the stored R", got)
		}
		if got := s.decrypt(c.AccessTokenEnc); got == "old-access" || got != fake.lastIssued() {
			t.Errorf("access token = %q, want the new %q", got, fake.lastIssued())
		}
		if st := s.status(u.AccessToken); st.Status != "connected" {
			t.Errorf("status = %+v, want connected", st)
		}
	})
}

// CLM-8: an expired access token is refreshed on a status read: invalid_grant
// marks the connection broken, success stores the new token, and a transient
// failure leaves it connected.
func TestGoogleCalendar_RefreshOnStatus(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	expired := time.Now().Add(-time.Hour)

	t.Run("invalid_grant marks broken", func(t *testing.T) {
		u := s.signUp("calrevoked")
		s.seedConnection(u.User.ID, "old-access", "refresh-R", "read_write", expired, false)
		fake.set(func(f *fakeCalendarGoogle) { f.refreshStatus, f.refreshError = http.StatusBadRequest, "invalid_grant" })
		hits := fake.refreshHits.Load()
		if st := s.status(u.AccessToken); st.Status != "broken" {
			t.Errorf("status = %+v, want broken", st)
		}
		if fake.refreshHits.Load() != hits+1 {
			t.Error("status did not try a refresh")
		}
		if c := s.connection(u.User.ID); !c.Broken {
			t.Error("row not flagged broken")
		}
	})
	t.Run("successful refresh", func(t *testing.T) {
		u := s.signUp("calrefreshed")
		s.seedConnection(u.User.ID, "old-access", "refresh-R", "read_write", expired, false)
		fake.set(func(f *fakeCalendarGoogle) { f.refreshStatus, f.refreshError = 0, "" })
		if st := s.status(u.AccessToken); st.Status != "connected" {
			t.Errorf("status = %+v, want connected", st)
		}
		c := s.connection(u.User.ID)
		if got := s.decrypt(c.AccessTokenEnc); got == "old-access" || got != fake.lastIssued() {
			t.Errorf("access token = %q, want the refreshed %q", got, fake.lastIssued())
		}
		if c.Broken || !c.TokenExpiry.Time.After(time.Now()) {
			t.Errorf("row broken=%v expiry=%v, want not broken with a future expiry", c.Broken, c.TokenExpiry.Time)
		}
	})
	t.Run("transient 503", func(t *testing.T) {
		u := s.signUp("caltransient")
		s.seedConnection(u.User.ID, "old-access", "refresh-R", "read_write", expired, false)
		fake.set(func(f *fakeCalendarGoogle) { f.refreshStatus, f.refreshError = http.StatusServiceUnavailable, "" })
		if st := s.status(u.AccessToken); st.Status != "connected" {
			t.Errorf("status = %+v, want connected", st)
		}
		c := s.connection(u.User.ID)
		if c.Broken || s.decrypt(c.AccessTokenEnc) != "old-access" {
			t.Error("transient failure changed the row")
		}
	})
	t.Run("network failure", func(t *testing.T) {
		u := s.signUp("calnetwork")
		s.seedConnection(u.User.ID, "old-access", "refresh-R", "read_write", expired, false)
		down := newCalendarTestServer(t, newClosedFakeCalendarGoogle(t))
		if st := down.status(u.AccessToken); st.Status != "connected" {
			t.Errorf("status = %+v, want connected", st)
		}
		if c := s.connection(u.User.ID); c.Broken {
			t.Error("network failure marked the row broken")
		}
	})
	t.Run("unexpired token is not refreshed", func(t *testing.T) {
		u := s.signUp("calfresh")
		s.seedConnection(u.User.ID, "old-access", "refresh-R", "read_write", time.Now().Add(time.Hour), false)
		hits := fake.refreshHits.Load()
		if st := s.status(u.AccessToken); st.Status != "connected" {
			t.Errorf("status = %+v, want connected", st)
		}
		if fake.refreshHits.Load() != hits {
			t.Error("unexpired token was refreshed")
		}
	})
}

// newClosedFakeCalendarGoogle returns a fake whose server is already closed,
// so every call to it fails at the network level.
func newClosedFakeCalendarGoogle(t *testing.T) *fakeCalendarGoogle {
	f := newFakeCalendarGoogle(t)
	f.srv.Close()
	return f
}

// CLM-10: disconnect deletes the row, revokes at Google best-effort, and
// succeeds from connected and broken states even when the revoke fails.
func TestGoogleCalendar_Disconnect(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)

	cases := []struct {
		name         string
		revokeStatus int
		broken       bool
	}{
		{"connected", 0, false},
		{"revoke fails", http.StatusInternalServerError, false},
		{"broken", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := s.signUp("caldisconnect")
			refresh := "refresh-" + uuid.NewString()
			s.seedConnection(u.User.ID, "access-x", refresh, "read_write", time.Now().Add(time.Hour), tc.broken)
			fake.set(func(f *fakeCalendarGoogle) { f.revokeStatus = tc.revokeStatus })

			var out calendarStatus
			s.expect(http.StatusOK, "DELETE", "/api/v1/me/google-calendar", u.AccessToken, nil, &out)
			if out.Status != "not_connected" || out.Access != "" {
				t.Errorf("response = %+v, want not_connected", out)
			}
			if n := s.connectionCount(u.User.ID); n != 0 {
				t.Errorf("connections = %d, want 0", n)
			}
			revoked := fake.revokedTokens()
			if len(revoked) == 0 || revoked[len(revoked)-1] != refresh {
				t.Errorf("revoke received %v, want the stored token %q", revoked, refresh)
			}
			if st := s.status(u.AccessToken); st.Status != "not_connected" {
				t.Errorf("status = %+v, want not_connected", st)
			}
		})
	}
	fake.set(func(f *fakeCalendarGoogle) { f.revokeStatus = 0 })

	// Disconnecting with nothing connected is still a success.
	u := s.signUp("calnothing")
	var out calendarStatus
	s.expect(http.StatusOK, "DELETE", "/api/v1/me/google-calendar", u.AccessToken, nil, &out)
	if out.Status != "not_connected" {
		t.Errorf("response = %+v, want not_connected", out)
	}
}

// The calendar callback lands on /account, which serves the web client.
func TestGoogleCalendar_AccountPageServesIndex(t *testing.T) {
	s := newCalendarTestServer(t, nil)
	resp, err := noRedirect.Get(s.srv.URL + "/account?google_calendar=connected")
	if err != nil {
		t.Fatalf("GET /account: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("GET /account: status %d Content-Type %q, want 200 text/html", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// CLM-12: endpoints act only on the authenticated caller and take no user ID.
// No response ever contains tokens. Another household member cannot see or
// delete another user's connection. Route patterns have no ID parameter.
func TestGoogleCalendar_IsolationAndNoLeak(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)

	// Users A and B in one household
	userA := s.signUp("calclm12a")
	userB := s.signUp("calclm12b")
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", userA.AccessToken,
		map[string]string{"name": "Household 12", "timezone": "UTC"}, &h)
	var inv struct{ Code string }
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/invites", userA.AccessToken, nil, &inv)
	s.expect(http.StatusOK, "POST", "/api/v1/invites/accept", userB.AccessToken, map[string]string{"code": inv.Code}, nil)

	knownAccess := "cal-access-token-clm12-secret-xyz"
	knownRefresh := "cal-refresh-token-clm12-secret-abc"
	fake.set(func(f *fakeCalendarGoogle) {
		f.customAccess = knownAccess
		f.refreshToken = knownRefresh
		f.grantedScope = calBothScopes
	})

	// A connects: verify connect response body does not leak tokens
	connectStatus, connectBody := s.doRaw("POST", "/api/v1/me/google-calendar/connect", userA.AccessToken, nil)
	if connectStatus != http.StatusOK {
		t.Fatalf("connect status: %d, body %s", connectStatus, connectBody)
	}
	var connOut struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(connectBody), &connOut); err != nil {
		t.Fatalf("decode connect response: %v", err)
	}
	u, err := url.Parse(connOut.URL)
	if err != nil {
		t.Fatalf("parse connect URL: %v", err)
	}
	state := u.Query().Get("state")
	loc, _ := s.calCallback(url.Values{"state": {state}, "code": {"code-clm12"}})
	if loc != calConnected {
		t.Fatalf("Location %q, want %q", loc, calConnected)
	}

	// B's status is not_connected
	stB := s.status(userB.AccessToken)
	if stB.Status != "not_connected" {
		t.Errorf("B's status = %+v, want not_connected", stB)
	}

	// B's DELETE /api/v1/me/google-calendar leaves A's row intact
	var bDelOut calendarStatus
	s.expect(http.StatusOK, "DELETE", "/api/v1/me/google-calendar", userB.AccessToken, nil, &bDelOut)
	if bDelOut.Status != "not_connected" {
		t.Errorf("B delete output = %+v, want not_connected", bDelOut)
	}
	if n := s.connectionCount(userA.User.ID); n != 1 {
		t.Fatalf("A's connection count after B DELETE = %d, want 1", n)
	}
	cA := s.connection(userA.User.ID)
	if s.decrypt(cA.AccessTokenEnc) != knownAccess || s.decrypt(cA.RefreshTokenEnc) != knownRefresh {
		t.Errorf("A's row tokens were modified by B's delete")
	}

	// Bodies of A's status, connect, and disconnect responses contain neither token string
	statusAStatus, statusABody := s.doRaw("GET", "/api/v1/me/google-calendar", userA.AccessToken, nil)
	if statusAStatus != http.StatusOK {
		t.Fatalf("A status: %d", statusAStatus)
	}
	delAStatus, delABody := s.doRaw("DELETE", "/api/v1/me/google-calendar", userA.AccessToken, nil)
	if delAStatus != http.StatusOK {
		t.Fatalf("A delete: %d", delAStatus)
	}

	for _, tokenStr := range []string{knownAccess, knownRefresh} {
		if strings.Contains(connectBody, tokenStr) {
			t.Errorf("connect response body leaks token %q: %s", tokenStr, connectBody)
		}
		if strings.Contains(statusABody, tokenStr) {
			t.Errorf("status response body leaks token %q: %s", tokenStr, statusABody)
		}
		if strings.Contains(delABody, tokenStr) {
			t.Errorf("disconnect response body leaks token %q: %s", tokenStr, delABody)
		}
	}

	// Router assertion: no calendar route pattern contains a user or household id parameter
	var routerSrc []byte
	for _, p := range []string{"router.go", "internal/http/router.go", "../internal/http/router.go"} {
		if b, err := os.ReadFile(p); err == nil {
			routerSrc = b
			break
		}
	}
	if len(routerSrc) == 0 {
		t.Fatal("could not find router.go to assert route patterns")
	}
	var matchedRoutes int
	for _, line := range strings.Split(string(routerSrc), "\n") {
		if strings.Contains(line, "/me/google-calendar") && strings.Contains(line, "api.Handle") {
			matchedRoutes++
			quoteStart := strings.Index(line, "\"")
			quoteEnd := strings.LastIndex(line, "\"")
			if quoteStart != -1 && quoteEnd > quoteStart {
				routePattern := line[quoteStart+1 : quoteEnd]
				if strings.Contains(routePattern, "{") {
					t.Errorf("calendar route pattern %q contains parameter: %s", routePattern, line)
				}
			}
		}
	}
	if matchedRoutes == 0 {
		t.Error("no /me/google-calendar routes found in router.go")
	}
}

// CLM-13: stored access and refresh tokens are encrypted at rest with AES-GCM;
// no token plaintext appears in any database column, while refresh still works
// using the decrypted token.
func TestGoogleCalendar_EncryptedAtRest(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)

	knownAccess := "cal-access-token-clm13-secret-known"
	knownRefresh := "cal-refresh-token-clm13-secret-known"
	fake.set(func(f *fakeCalendarGoogle) {
		f.customAccess = knownAccess
		f.refreshToken = knownRefresh
		f.grantedScope = calBothScopes
	})

	user := s.signUp("calclm13")
	state := s.connect(user.AccessToken).Query().Get("state")
	loc, _ := s.calCallback(url.Values{"state": {state}, "code": {"code-clm13"}})
	if loc != calConnected {
		t.Fatalf("Location %q, want %q", loc, calConnected)
	}

	// Read the raw google_calendar_connections row by direct SQL (cast every column to text)
	// and assert neither plaintext appears in any column.
	var (
		idText, userIDText, accessEncText, refreshEncText string
		expiryText, accessLevelText, brokenText           string
		createdAtText, updatedAtText                      string
	)
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id::text, user_id::text, access_token_enc::text, refresh_token_enc::text,
		        token_expiry::text, access_level::text, broken::text, created_at::text, updated_at::text
		 FROM google_calendar_connections WHERE user_id = $1`,
		user.User.ID).Scan(
		&idText, &userIDText, &accessEncText, &refreshEncText,
		&expiryText, &accessLevelText, &brokenText, &createdAtText, &updatedAtText,
	); err != nil {
		t.Fatalf("read raw row: %v", err)
	}

	columns := []struct {
		name string
		val  string
	}{
		{"id", idText},
		{"user_id", userIDText},
		{"access_token_enc", accessEncText},
		{"refresh_token_enc", refreshEncText},
		{"token_expiry", expiryText},
		{"access_level", accessLevelText},
		{"broken", brokenText},
		{"created_at", createdAtText},
		{"updated_at", updatedAtText},
	}

	for _, col := range columns {
		if strings.Contains(col.val, knownAccess) {
			t.Errorf("column %s contains plaintext access token %q: %s", col.name, knownAccess, col.val)
		}
		if strings.Contains(col.val, knownRefresh) {
			t.Errorf("column %s contains plaintext refresh token %q: %s", col.name, knownRefresh, col.val)
		}
	}

	// Also verify raw byte slices in DB
	conn := s.connection(user.User.ID)
	if strings.Contains(string(conn.AccessTokenEnc), knownAccess) {
		t.Errorf("AccessTokenEnc bytes contain plaintext access token")
	}
	if strings.Contains(string(conn.RefreshTokenEnc), knownRefresh) {
		t.Errorf("RefreshTokenEnc bytes contain plaintext refresh token")
	}

	// Expire the access token (UPDATE token_expiry into the past)
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE google_calendar_connections SET token_expiry = $1 WHERE user_id = $2",
		time.Now().Add(-time.Hour), user.User.ID); err != nil {
		t.Fatalf("expire token: %v", err)
	}

	// Read status triggers lazy refresh
	st := s.status(user.AccessToken)
	if st.Status != "connected" {
		t.Fatalf("status after refresh = %+v, want connected", st)
	}

	// Assert the fake refresh endpoint received the original refresh token
	if got := fake.lastRefreshToken(); got != knownRefresh {
		t.Errorf("fake refresh endpoint received %q, want original %q", got, knownRefresh)
	}
}
