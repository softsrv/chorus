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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

const (
	calScopeList     = "https://www.googleapis.com/auth/calendar.calendarlist.readonly"
	calScopeEvents   = "https://www.googleapis.com/auth/calendar.events"
	calScopeEventsRO = "https://www.googleapis.com/auth/calendar.events.readonly"
	calBothScopes    = calScopeList + " " + calScopeEvents
)

// calendarKey is the 32-byte token key the test service encrypts with.
var calendarKey = []byte("0123456789abcdef0123456789abcdef")

type codeExchange struct {
	Code         string
	RedirectURI  string
	ClientID     string
	ClientSecret string
}

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
	exchanges     []codeExchange
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
			clientID, clientSecret, _ := r.BasicAuth()
			if clientID == "" {
				clientID = r.Form.Get("client_id")
			}
			if clientSecret == "" {
				clientSecret = r.Form.Get("client_secret")
			}
			f.exchanges = append(f.exchanges, codeExchange{
				Code:         r.Form.Get("code"),
				RedirectURI:  r.Form.Get("redirect_uri"),
				ClientID:     clientID,
				ClientSecret: clientSecret,
			})
			if f.tokenStatus != 0 && f.tokenStatus != http.StatusOK {
				w.WriteHeader(f.tokenStatus)
				fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"%s"}`, sentinel)
				return
			}
			if r.Form.Get("code") == "" {
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

func (f *fakeCalendarGoogle) lastExchange() codeExchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.exchanges) == 0 {
		return codeExchange{}
	}
	return f.exchanges[len(f.exchanges)-1]
}

type calendarServer struct {
	*testServer
	cipher *app.TokenCipher
}

// newCalendarTestServer is newTestServer with Google Calendar wired to fake
// (or unavailable when fake is nil). The token and revoke endpoints point
// at the fake.
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
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			TokenURL:     fake.srv.URL + "/token",
			RevokeURL:    fake.srv.URL + "/revoke",
			APIBaseURL:   fake.srv.URL + "/calendar/v3",
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:        app.NewUserService(q),
		HouseholdSvc:   app.NewHouseholdService(q, pool),
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

func (s *calendarServer) connect(token string, body any) (int, calendarStatus, *http.Response) {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal connect body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", s.srv.URL+"/api/v1/me/google-calendar/connect", rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		s.t.Fatalf("POST connect: %v", err)
	}
	defer resp.Body.Close()
	var st calendarStatus
	_ = json.NewDecoder(resp.Body).Decode(&st)
	return resp.StatusCode, st, resp
}

func (s *calendarServer) connectCode(token, code string) (int, calendarStatus) {
	s.t.Helper()
	status, st, _ := s.connect(token, map[string]string{"code": code})
	return status, st
}

func (s *calendarServer) connectFlow(token string) calendarStatus {
	s.t.Helper()
	status, st, _ := s.connect(token, map[string]string{"code": "code-" + uuid.NewString()})
	if status != http.StatusOK {
		s.t.Fatalf("connectFlow: status %d, want 200", status)
	}
	return st
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
	if st := s.connectFlow(user.AccessToken); st.Status != "connected" || st.Access != "read_write" {
		t.Fatalf("connectFlow = %+v, want connected read_write", st)
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
	if status := s.do("POST", "/api/v1/me/google-calendar/connect", unverified.AccessToken, map[string]string{"code": "c"}, &e); status != http.StatusForbidden || e.Error.Code != "email_not_verified" {
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
	if status := off.do("POST", "/api/v1/me/google-calendar/connect", offUser.AccessToken, map[string]string{"code": "c"}, &e); status != http.StatusNotFound {
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

// CLM-1: POST /api/v1/me/google-calendar/connect with a bearer token of a verified
// account and body {code} exchanges the code at Google's token endpoint using the
// web client's id and secret, stores the access and refresh tokens encrypted
// (never plaintext) as one connection for the caller, and answers 200 with the
// same JSON body as GET /me/google-calendar (status and access). No redirect is
// returned and no Set-Cookie header is sent.
func TestGoogleCalendar_CallbackSuccess(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calsuccess")

	req, _ := http.NewRequest("POST", s.srv.URL+"/api/v1/me/google-calendar/connect", bytes.NewReader([]byte(`{"code":"code-1"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("Location header %q; want no redirect", loc)
	}
	if len(resp.Cookies()) > 0 || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("response set cookies: %v", resp.Header.Values("Set-Cookie"))
	}

	var connBody calendarStatus
	if err := json.NewDecoder(resp.Body).Decode(&connBody); err != nil {
		t.Fatalf("decode connect response: %v", err)
	}
	if connBody.Status != "connected" || connBody.Access != "read_write" {
		t.Errorf("connect body = %+v, want connected read_write", connBody)
	}

	// Assert body equals subsequent GET /me/google-calendar body
	getStatus := s.status(user.AccessToken)
	if getStatus != connBody {
		t.Errorf("subsequent GET status = %+v, want equal to connect response %+v", getStatus, connBody)
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

	// Second POST with already consumed code is refused
	fake.set(func(f *fakeCalendarGoogle) { f.tokenStatus = http.StatusBadRequest })
	status, _, _ := s.connect(user.AccessToken, map[string]string{"code": "code-replayed"})
	if status != http.StatusBadRequest {
		t.Errorf("replayed code status = %d, want 400", status)
	}
	if n := s.connectionCount(user.User.ID); n != 1 {
		t.Errorf("connections after replay = %d, want 1", n)
	}
	if st := s.status(user.AccessToken); st.Status != "connected" || st.Access != "read_write" {
		t.Errorf("status = %+v, want connected read_write", st)
	}
}

// CLM-2: The connect body accepts an optional platform. When absent or "web"
// the server exchanges the code with redirect_uri=postmessage; for "ios" or
// "android" it exchanges with an empty redirect_uri; always with the web
// client's id and secret. Any other platform value is refused with a 4xx and
// Google is never called.
func TestGoogleCalendar_ConnectPlatformRedirectURI(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calplatform")

	cases := []struct {
		name       string
		body       map[string]string
		wantURI    string
		wantCode   int
		callGoogle bool
	}{
		{"absent platform", map[string]string{"code": "code-absent"}, "postmessage", http.StatusOK, true},
		{"web platform", map[string]string{"code": "code-web", "platform": "web"}, "postmessage", http.StatusOK, true},
		{"ios platform", map[string]string{"code": "code-ios", "platform": "ios"}, "", http.StatusOK, true},
		{"android platform", map[string]string{"code": "code-android", "platform": "android"}, "", http.StatusOK, true},
		{"windows platform", map[string]string{"code": "code-win", "platform": "windows"}, "", http.StatusBadRequest, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hitsBefore := fake.codeHits.Load()
			status, _, _ := s.connect(user.AccessToken, tc.body)
			if status != tc.wantCode {
				t.Fatalf("status = %d, want %d", status, tc.wantCode)
			}
			if !tc.callGoogle {
				if fake.codeHits.Load() != hitsBefore {
					t.Errorf("fake token endpoint hit on refused platform: hits=%d want %d", fake.codeHits.Load(), hitsBefore)
				}
				return
			}
			if fake.codeHits.Load() != hitsBefore+1 {
				t.Fatalf("fake token endpoint hits = %d, want %d", fake.codeHits.Load(), hitsBefore+1)
			}
			ex := fake.lastExchange()
			if ex.RedirectURI != tc.wantURI {
				t.Errorf("exchange redirect_uri = %q, want %q", ex.RedirectURI, tc.wantURI)
			}
			if ex.ClientID != "client-id" || ex.ClientSecret != "client-secret" {
				t.Errorf("exchange client_id/secret = %q/%q, want client-id/client-secret", ex.ClientID, ex.ClientSecret)
			}
		})
	}
}

// CLM-4: A code Google rejects (invalid_grant, reused code, any token-endpoint
// failure or empty code) yields a 4xx JSON error with the generic calendar-connect
// message; the response body contains no text from Google's error body or internal
// error, and no connection row, token or other state is stored (an existing
// connection is left as it was).
func TestGoogleCalendar_CallbackRefusals(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calrefuse")

	cases := []struct {
		name        string
		tokenStatus int
		body        map[string]string
		callGoogle  bool
	}{
		{"empty code", 0, map[string]string{"code": ""}, false},
		{"whitespace code", 0, map[string]string{"code": "   "}, false},
		{"token endpoint 400", http.StatusBadRequest, map[string]string{"code": "code-400"}, true},
		{"token endpoint 500", http.StatusInternalServerError, map[string]string{"code": "code-500"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.set(func(f *fakeCalendarGoogle) { f.tokenStatus = tc.tokenStatus })
			hits := fake.codeHits.Load()
			status, body := s.doRaw("POST", "/api/v1/me/google-calendar/connect", user.AccessToken, tc.body)
			if status < 400 || status >= 500 {
				t.Fatalf("status = %d, want 4xx", status)
			}
			for _, leak := range []string{sentinel, "invalid_grant", "oauth2", "pgx", "sql", "exchange"} {
				if strings.Contains(body, leak) {
					t.Errorf("response leaks %q: body %q", leak, body)
				}
			}
			if !tc.callGoogle && fake.codeHits.Load() != hits {
				t.Error("token endpoint was hit for empty code")
			}
			if n := s.connectionCount(user.User.ID); n != 0 {
				t.Errorf("connections = %d, want 0", n)
			}
		})
	}
}

// CLM-3: the access level comes from the scopes Google granted; with no
// calendar scope the token is revoked and nothing is stored.
func TestGoogleCalendar_GrantedScopes(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)

	t.Run("both scopes", func(t *testing.T) {
		u := s.signUp("calboth")
		fake.set(func(f *fakeCalendarGoogle) { f.grantedScope = calBothScopes })
		status, st := s.connectCode(u.AccessToken, "code-both")
		if status != http.StatusOK {
			t.Fatalf("connect status %d", status)
		}
		if st.Status != "connected" || st.Access != "read_write" {
			t.Errorf("status = %+v, want connected read_write", st)
		}
		if st := s.status(u.AccessToken); st.Status != "connected" || st.Access != "read_write" {
			t.Errorf("status = %+v, want connected read_write", st)
		}
	})
	t.Run("read without events", func(t *testing.T) {
		u := s.signUp("calro")
		fake.set(func(f *fakeCalendarGoogle) { f.grantedScope = calScopeList + " " + calScopeEventsRO })
		status, st := s.connectCode(u.AccessToken, "code-ro")
		if status != http.StatusOK {
			t.Fatalf("connect status %d", status)
		}
		if st.Status != "connected" || st.Access != "read_only" {
			t.Errorf("status = %+v, want connected read_only", st)
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
		status, _ := s.connectCode(u.AccessToken, "code-none")
		if status != http.StatusBadRequest {
			t.Errorf("status %d, want 400", status)
		}
		if n := s.connectionCount(u.User.ID); n != 0 {
			t.Errorf("connections = %d, want 0", n)
		}
		revoked := fake.revokedTokens()
		if len(revoked) != revokedBefore+1 || revoked[len(revoked)-1] != fake.lastIssued() {
			t.Errorf("revoked = %v, want token %q revoked", revoked, fake.lastIssued())
		}
	})
}

// CLM-7: without a refresh token a code exchange is only stored when the user
// already holds one, which is kept while the access token is updated.
func TestGoogleCalendar_NoRefreshToken(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	fake.set(func(f *fakeCalendarGoogle) { f.refreshToken = "" })

	t.Run("no stored refresh token", func(t *testing.T) {
		u := s.signUp("calnorefresh")
		status, _ := s.connectCode(u.AccessToken, "code-norefresh")
		if status != http.StatusBadRequest {
			t.Errorf("status %d, want 400", status)
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
		status, st := s.connectCode(u.AccessToken, "code-keeprefresh")
		if status != http.StatusOK {
			t.Fatalf("status %d, want 200", status)
		}
		c := s.connection(u.User.ID)
		if got := s.decrypt(c.RefreshTokenEnc); got != "stored-refresh-R" {
			t.Errorf("refresh token = %q, want the stored R", got)
		}
		if got := s.decrypt(c.AccessTokenEnc); got == "old-access" || got != fake.lastIssued() {
			t.Errorf("access token = %q, want the new %q", got, fake.lastIssued())
		}
		if st.Status != "connected" {
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

// The Account page at /account serves the web client.
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

// CLM-6: GET /api/v1/me/google-calendar/callback answers 404 (no handler, no redirect);
// no response from any calendar route carries a Set-Cookie header and no handler
// reads the chorus_gcal_bind cookie, which no longer exists in the code.
func TestGoogleCalendar_Callback404AndNoCookies(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)
	user := s.signUp("calclm6")

	// 1. GET callback without bearer token
	req1, _ := http.NewRequest("GET", s.srv.URL+"/api/v1/me/google-calendar/callback?state=s&code=c", nil)
	resp1, err := noRedirect.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNotFound {
		t.Errorf("anonymous callback status = %d, want 404", resp1.StatusCode)
	}
	if resp1.Header.Get("Set-Cookie") != "" {
		t.Errorf("anonymous callback sent Set-Cookie: %q", resp1.Header.Get("Set-Cookie"))
	}

	// 2. GET callback with bearer token
	req2, _ := http.NewRequest("GET", s.srv.URL+"/api/v1/me/google-calendar/callback?state=s&code=c", nil)
	req2.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp2, err := noRedirect.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("authed callback status = %d, want 404", resp2.StatusCode)
	}
	if resp2.Header.Get("Set-Cookie") != "" {
		t.Errorf("authed callback sent Set-Cookie: %q", resp2.Header.Get("Set-Cookie"))
	}

	// 3. GET callback with cookie
	req3, _ := http.NewRequest("GET", s.srv.URL+"/api/v1/me/google-calendar/callback?state=s&code=c", nil)
	req3.AddCookie(&http.Cookie{Name: "chorus_gcal_bind", Value: "test-cookie"})
	resp3, err := noRedirect.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("callback with cookie status = %d, want 404", resp3.StatusCode)
	}
	if resp3.Header.Get("Set-Cookie") != "" {
		t.Errorf("callback with cookie sent Set-Cookie: %q", resp3.Header.Get("Set-Cookie"))
	}

	// 4. Every calendar route response has no Set-Cookie header
	calendarRoutes := []struct {
		method string
		path   string
		body   any
	}{
		{"GET", "/api/v1/me/google-calendar", nil},
		{"POST", "/api/v1/me/google-calendar/connect", map[string]string{"code": "code-cookie-check"}},
		{"DELETE", "/api/v1/me/google-calendar", nil},
		{"GET", "/api/v1/me/google-calendar/calendars", nil},
	}
	for _, cr := range calendarRoutes {
		var rdr io.Reader
		if cr.body != nil {
			b, _ := json.Marshal(cr.body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(cr.method, s.srv.URL+cr.path, rdr)
		if cr.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+user.AccessToken)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", cr.method, cr.path, err)
		}
		resp.Body.Close()
		if resp.Header.Get("Set-Cookie") != "" {
			t.Errorf("%s %s sent Set-Cookie: %q", cr.method, cr.path, resp.Header.Get("Set-Cookie"))
		}
	}
}

// CLM-12: endpoints act only on the authenticated caller and take no user ID.
// No response ever contains tokens. Another login cannot see or delete a
// user's connection, nor reach the household it runs. Route patterns have no
// ID parameter.
func TestGoogleCalendar_IsolationAndNoLeak(t *testing.T) {
	fake := newFakeCalendarGoogle(t)
	s := newCalendarTestServer(t, fake)

	// A runs a household; B is another login, outside it (a household has
	// exactly one login).
	userA := s.signUp("calclm12a")
	userB := s.signUp("calclm12b")
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", userA.AccessToken,
		map[string]string{"name": "Household 12", "timezone": "UTC"}, &h)
	var notFound apiError
	s.expect(http.StatusNotFound, "GET", "/api/v1/households/"+h.ID.String()+"/calendar", userB.AccessToken, nil, &notFound)
	if notFound.Error.Code != "not_found" {
		t.Errorf("B reads A's household calendar: code %q, want not_found", notFound.Error.Code)
	}

	knownAccess := "cal-access-token-clm12-secret-xyz"
	knownRefresh := "cal-refresh-token-clm12-secret-abc"
	fake.set(func(f *fakeCalendarGoogle) {
		f.customAccess = knownAccess
		f.refreshToken = knownRefresh
		f.grantedScope = calBothScopes
	})

	// A connects: verify connect response body does not leak tokens
	connectStatus, connectBody := s.doRaw("POST", "/api/v1/me/google-calendar/connect", userA.AccessToken, map[string]string{"code": "code-clm12"})
	if connectStatus != http.StatusOK {
		t.Fatalf("connect status: %d, body %s", connectStatus, connectBody)
	}
	var connOut calendarStatus
	if err := json.Unmarshal([]byte(connectBody), &connOut); err != nil {
		t.Fatalf("decode connect response: %v", err)
	}
	if connOut.Status != "connected" {
		t.Fatalf("connect status = %q, want connected", connOut.Status)
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
	status, st, _ := s.connect(user.AccessToken, map[string]string{"code": "code-clm13"})
	if status != http.StatusOK || st.Status != "connected" {
		t.Fatalf("connect status %d (%+v), want 200 connected", status, st)
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
	stRefresh := s.status(user.AccessToken)
	if stRefresh.Status != "connected" {
		t.Fatalf("status after refresh = %+v, want connected", stRefresh)
	}

	// Assert the fake refresh endpoint received the original refresh token
	if got := fake.lastRefreshToken(); got != knownRefresh {
		t.Errorf("fake refresh endpoint received %q, want original %q", got, knownRefresh)
	}
}
