//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

// sentinel is what the fake Google puts in every error body. It must never
// reach a response body or a redirect Location.
const sentinel = "INTERNAL-SENTINEL"

// fakeGoogle serves the OAuth token and OpenID userinfo endpoints. Each test
// sets the identity and failure modes it needs.
type fakeGoogle struct {
	srv  *httptest.Server
	hits atomic.Int64

	mu            sync.Mutex
	tokenStatus   int    // 0 = 200
	grantedScope  string // scope field of the token response; "" = omitted
	sub           string
	email         string
	emailVerified any
	name          string
	codesSeen     []string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{grantedScope: "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile", emailVerified: true, name: "Google Person"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = r.ParseForm()
		if f.tokenStatus != 0 && f.tokenStatus != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.tokenStatus)
			fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"%s"}`, sentinel)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") == "" || r.Form.Get("code_verifier") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"invalid_request","error_description":"%s missing pkce"}`, sentinel)
			return
		}
		f.codesSeen = append(f.codesSeen, r.Form.Get("code"))
		resp := map[string]any{"access_token": "fake-access-" + r.Form.Get("code"), "token_type": "Bearer", "expires_in": 3600}
		if f.grantedScope != "" {
			resp["scope"] = f.grantedScope
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer fake-access-") {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, sentinel)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": f.sub, "email": f.email, "email_verified": f.emailVerified, "name": f.name,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) identity(sub, email string, verified any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sub, f.email, f.emailVerified = sub, email, verified
}

func (f *fakeGoogle) set(fn func(f *fakeGoogle)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// newGoogleTestServer is newTestServer with Google sign-in wired to fake (or
// disabled when fake is nil).
func newGoogleTestServer(t *testing.T, fake *fakeGoogle) *testServer {
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

	var google *app.GoogleSignIn
	if fake != nil {
		google = app.NewGoogleSignIn(q, authSvc, app.GoogleSignInConfig{
			ClientID: "client-id", ClientSecret: "client-secret",
			RedirectURL: "http://chorus.test/api/v1/auth/google/callback",
			AuthURL:     fake.srv.URL + "/auth",
			TokenURL:    fake.srv.URL + "/token",
			UserInfoURL: fake.srv.URL + "/userinfo",
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		ChoreSvc:     app.NewChoreService(q),
		GoogleSignIn: google,
		JWTSecret:    secret,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &testServer{t: t, srv: srv, mail: mail, pool: pool}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// get issues a GET without following redirects and returns status, Location
// and body.
func (s *testServer) get(path string) (int, string, string) {
	s.t.Helper()
	resp, err := noRedirect.Get(s.srv.URL + path)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

// startGoogle hits the start route and returns the state Google would echo.
func (s *testServer) startGoogle() string {
	s.t.Helper()
	status, loc, _ := s.get("/api/v1/auth/google/start")
	if status != http.StatusFound {
		s.t.Fatalf("start: status %d, want 302", status)
	}
	u, err := url.Parse(loc)
	if err != nil {
		s.t.Fatalf("start Location %q: %v", loc, err)
	}
	state := u.Query().Get("state")
	if state == "" || u.Query().Get("code_challenge") == "" {
		s.t.Fatalf("start Location missing state or PKCE: %q", loc)
	}
	return state
}

// callback calls the callback route and returns the parsed redirect query,
// the raw Location and the body. It fails unless the redirect goes to the
// web client's return path.
func (s *testServer) callback(query url.Values) (url.Values, string, string) {
	s.t.Helper()
	status, loc, body := s.get("/api/v1/auth/google/callback?" + query.Encode())
	if status != http.StatusFound {
		s.t.Fatalf("callback: status %d, want 302 (body %q)", status, body)
	}
	u, err := url.Parse(loc)
	if err != nil || u.Path != "/google-sign-in" || u.Host != "" {
		s.t.Fatalf("callback Location %q: want the /google-sign-in return path", loc)
	}
	return u.Query(), loc, body
}

// googleSignIn runs start + callback and returns the hand-off code.
func (s *testServer) googleSignIn() string {
	s.t.Helper()
	state := s.startGoogle()
	q, loc, _ := s.callback(url.Values{"state": {state}, "code": {"auth-code-" + uuid.NewString()}})
	if q.Get("code") == "" || q.Get("error") != "" {
		s.t.Fatalf("callback did not hand off a code: %q", loc)
	}
	return q.Get("code")
}

type googleTokens struct {
	tokens
	User struct {
		ID            uuid.UUID `json:"id"`
		Email         string    `json:"email"`
		EmailVerified bool      `json:"email_verified"`
		HasPassword   bool      `json:"has_password"`
	} `json:"user"`
}

func (s *testServer) redeem(code, device string) (int, googleTokens) {
	s.t.Helper()
	var out googleTokens
	status := s.do("POST", "/api/v1/auth/google/redeem", "", map[string]string{"code": code, "device_name": device}, &out)
	return status, out
}

type rowCounts struct{ users, refreshTokens, handoffs int }

func (s *testServer) counts() rowCounts {
	s.t.Helper()
	var c rowCounts
	if err := s.pool.QueryRow(context.Background(), `SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM refresh_tokens),
		(SELECT COUNT(*) FROM google_sign_in_handoffs)`).Scan(&c.users, &c.refreshTokens, &c.handoffs); err != nil {
		s.t.Fatalf("counts: %v", err)
	}
	return c
}

func (s *testServer) cleanupEmail(email string) {
	s.t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", email)
	})
}

func (s *testServer) userByEmail(email string) db.User {
	s.t.Helper()
	u, err := db.New(s.pool).GetUserByEmail(context.Background(), email)
	if err != nil {
		s.t.Fatalf("GetUserByEmail(%s): %v", email, err)
	}
	return u
}

// insertState stores a state row directly (for expired / other-purpose cases).
func (s *testServer) insertState(purpose string, expiresAt time.Time) string {
	s.t.Helper()
	raw := "test-state-" + uuid.NewString()
	id := uuid.Must(uuid.NewV7())
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO oauth_states (id, state_hash, purpose, pkce_verifier, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		id, auth.HashToken(raw), purpose, "verifier-"+uuid.NewString(), expiresAt); err != nil {
		s.t.Fatalf("insert state: %v", err)
	}
	s.t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), "DELETE FROM oauth_states WHERE id = $1", id) })
	return raw
}

// CLM-1: a new person signs in with Google and ends up with a passwordless,
// verified account and one session under the redeeming device's name.
func TestGoogle_NewUserSignIn(t *testing.T) {
	fake := newFakeGoogle(t)
	s := newGoogleTestServer(t, fake)

	local := "newuser-" + uuid.NewString()[:8]
	normalized := local + "@example.com"
	s.cleanupEmail(normalized)
	sub := "sub-" + uuid.NewString()
	fake.identity(sub, "  "+strings.ToUpper(local)+"@Example.COM", true)

	code := s.googleSignIn()
	status, tok := s.redeem(code, "Sam's Laptop")
	if status != http.StatusOK || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("redeem: status %d tokens %+v", status, tok)
	}
	if tok.User.Email != normalized || !tok.User.EmailVerified || tok.User.HasPassword {
		t.Errorf("redeem user = %+v, want %s verified without password", tok.User, normalized)
	}

	u := s.userByEmail(normalized)
	if u.ID != tok.User.ID || !u.EmailVerified || u.PasswordHash.Valid || !u.GoogleSubject.Valid || u.GoogleSubject.String != sub {
		t.Errorf("user row = %+v, want verified, NULL password_hash, google_subject %s", u, sub)
	}

	var sessions struct {
		Items []struct {
			DeviceName string `json:"device_name"`
			Current    bool   `json:"current"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/me/sessions", tok.AccessToken, nil, &sessions)
	if len(sessions.Items) != 1 || sessions.Items[0].DeviceName != "Sam's Laptop" || !sessions.Items[0].Current {
		t.Errorf("sessions = %+v, want exactly one current session named \"Sam's Laptop\"", sessions.Items)
	}
}

// CLM-3: a hand-off code works once and only before it expires.
func TestGoogle_HandoffSingleUse(t *testing.T) {
	fake := newFakeGoogle(t)
	s := newGoogleTestServer(t, fake)

	email := "handoff-" + uuid.NewString()[:8] + "@example.com"
	s.cleanupEmail(email)
	fake.identity("sub-"+uuid.NewString(), email, true)

	code := s.googleSignIn()
	if status, _ := s.redeem(code, "first"); status != http.StatusOK {
		t.Fatalf("first redeem: status %d", status)
	}
	before := s.counts()

	var e apiError
	if status := s.do("POST", "/api/v1/auth/google/redeem", "", map[string]string{"code": code, "device_name": "second"}, &e); status != http.StatusBadRequest {
		t.Errorf("second redeem: status %d, want 400", status)
	}
	if e.Error.Code != "invalid_code" {
		t.Errorf("second redeem code %q, want invalid_code", e.Error.Code)
	}

	// An expired hand-off row for a real user.
	u := s.userByEmail(email)
	expired := "expired-" + uuid.NewString()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO google_sign_in_handoffs (id, code_hash, user_id, expires_at) VALUES ($1, $2, $3, NOW() - INTERVAL '1 minute')`,
		uuid.Must(uuid.NewV7()), auth.HashToken(expired), u.ID); err != nil {
		t.Fatalf("insert expired handoff: %v", err)
	}
	before.handoffs++
	for name, c := range map[string]string{"expired": expired, "unknown": "unknown-" + uuid.NewString(), "empty": ""} {
		if status, _ := s.redeem(c, name); status != http.StatusBadRequest {
			t.Errorf("%s code: status %d, want 400", name, status)
		}
	}
	if after := s.counts(); after != before {
		t.Errorf("refused redemptions changed rows: before %+v after %+v", before, after)
	}
}

// CLM-4: Google returning an error (cancel/deny) sends the person back with a
// cancel reason, no code, and stores nothing.
func TestGoogle_CallbackCancelled(t *testing.T) {
	fake := newFakeGoogle(t)
	s := newGoogleTestServer(t, fake)
	fake.identity("sub-"+uuid.NewString(), "cancel-"+uuid.NewString()[:8]+"@example.com", true)

	state := s.startGoogle()
	before := s.counts()
	hitsBefore := fake.hits.Load()
	q, loc, _ := s.callback(url.Values{"state": {state}, "error": {"access_denied"}})
	if q.Get("error") != "cancelled" || q.Has("code") {
		t.Errorf("cancel Location %q: want error=cancelled and no code", loc)
	}
	if after := s.counts(); after != before {
		t.Errorf("cancel stored rows: before %+v after %+v", before, after)
	}
	if fake.hits.Load() != hitsBefore {
		t.Error("cancel contacted Google")
	}
}

// CLM-5: bad state or a refused code exchange is turned away with only a
// generic reason; nothing is created and no internal text leaks.
func TestGoogle_CallbackRefusals(t *testing.T) {
	fake := newFakeGoogle(t)
	s := newGoogleTestServer(t, fake)

	reused := func() url.Values {
		email := "reused-" + uuid.NewString()[:8] + "@example.com"
		s.cleanupEmail(email)
		fake.identity("sub-"+uuid.NewString(), email, true)
		state := s.startGoogle()
		q, loc, _ := s.callback(url.Values{"state": {state}, "code": {"first-code"}})
		if q.Get("code") == "" {
			t.Fatalf("setup sign-in failed: %q", loc)
		}
		return url.Values{"state": {state}, "code": {"second-code"}}
	}

	cases := []struct {
		name  string
		setup func() url.Values
	}{
		{"no state", func() url.Values { return url.Values{"code": {"c"}} }},
		{"random state", func() url.Values { return url.Values{"state": {"random-" + uuid.NewString()}, "code": {"c"}} }},
		{"reused state", reused},
		{"expired state", func() url.Values {
			return url.Values{"state": {s.insertState("sign_in", time.Now().Add(-time.Minute))}, "code": {"c"}}
		}},
		{"calendar state", func() url.Values {
			return url.Values{"state": {s.insertState("calendar", time.Now().Add(10*time.Minute))}, "code": {"c"}}
		}},
		{"token endpoint 400", func() url.Values {
			fake.set(func(f *fakeGoogle) { f.tokenStatus = http.StatusBadRequest })
			return url.Values{"state": {s.startGoogle()}, "code": {"c"}}
		}},
		{"token endpoint 500", func() url.Values {
			fake.set(func(f *fakeGoogle) { f.tokenStatus = http.StatusInternalServerError })
			return url.Values{"state": {s.startGoogle()}, "code": {"c"}}
		}},
		{"email scope not granted", func() url.Values {
			fake.set(func(f *fakeGoogle) { f.grantedScope = "openid" })
			return url.Values{"state": {s.startGoogle()}, "code": {"c"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.set(func(f *fakeGoogle) {
				f.tokenStatus = 0
				f.grantedScope = "openid email profile"
				f.sub, f.email, f.emailVerified = "sub-"+uuid.NewString(), "refused-"+uuid.NewString()[:8]+"@example.com", true
			})
			query := tc.setup()
			s.cleanupEmail(fake.email)
			before := s.counts()
			q, loc, body := s.callback(query)
			if q.Get("error") != "failed" || q.Has("code") {
				t.Errorf("Location %q: want error=failed and no code", loc)
			}
			for _, leak := range []string{sentinel, "oauth2", "invalid_grant", "exchange", "state", "pgx", "sql"} {
				if strings.Contains(loc, leak) || strings.Contains(body, leak) {
					t.Errorf("response leaks %q: Location %q body %q", leak, loc, body)
				}
			}
			if after := s.counts(); after != before {
				t.Errorf("rows changed: before %+v after %+v", before, after)
			}
		})
	}
}

// CLM-6: a Google identity whose verified email matches a password account
// signs into that account and links it; the password keeps working.
func TestGoogle_LinksExistingPasswordAccount(t *testing.T) {
	fake := newFakeGoogle(t)
	s := newGoogleTestServer(t, fake)

	tag := uuid.NewString()[:8]
	registered := "Mixed-" + tag + "@Case.example"
	normalized := strings.ToLower(registered)
	s.cleanupEmail(normalized)
	s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
		map[string]string{"email": registered, "password": "correct-password-123", "display_name": "Mixed"}, nil)
	var verified tokens
	s.expect(http.StatusOK, "POST", "/api/v1/auth/verify-email", "",
		map[string]string{"token": s.mail.token(t, normalized)}, &verified)
	before := s.userByEmail(normalized)

	sub := "sub-" + uuid.NewString()
	fake.identity(sub, "mixed-"+tag+"@case.example", true)
	usersBefore := s.counts().users

	status, tok := s.redeem(s.googleSignIn(), "linking")
	if status != http.StatusOK || tok.User.ID != before.ID {
		t.Fatalf("redeem: status %d user %s, want existing %s", status, tok.User.ID, before.ID)
	}
	if got := s.counts().users; got != usersBefore {
		t.Errorf("users count %d, want %d (no new user)", got, usersBefore)
	}
	var n int
	_ = s.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM users WHERE email = $1", normalized).Scan(&n)
	if n != 1 {
		t.Errorf("users with %s = %d, want 1", normalized, n)
	}
	after := s.userByEmail(normalized)
	if after.GoogleSubject.String != sub || after.PasswordHash != before.PasswordHash {
		t.Errorf("linked row: subject %q (want %q), password hash changed %v", after.GoogleSubject.String, sub, after.PasswordHash != before.PasswordHash)
	}
	s.expect(http.StatusOK, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": registered, "password": "correct-password-123"}, nil)

	// Later sign-in by subject, even if Google now reports another email.
	fake.identity(sub, "renamed-"+tag+"@example.com", true)
	if status, again := s.redeem(s.googleSignIn(), "again"); status != http.StatusOK || again.User.ID != before.ID {
		t.Errorf("repeat sign-in: status %d user %s, want %s", status, again.User.ID, before.ID)
	}

	// A password-locked account can still sign in with Google.
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE users SET locked_until = NOW() + INTERVAL '1 hour', failed_login_attempts = 10 WHERE id = $1", before.ID); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if status, locked := s.redeem(s.googleSignIn(), "locked"); status != http.StatusOK || locked.User.ID != before.ID {
		t.Errorf("locked account Google sign-in: status %d user %s", status, locked.User.ID)
	}
}

// CLM-7: an identity whose email is unverified, or that lacks a subject or
// email, never links to or creates an account.
func TestGoogle_RefusesUnverifiedIdentity(t *testing.T) {
	fake := newFakeGoogle(t)
	s := newGoogleTestServer(t, fake)

	victim := s.signUp("victim")
	victimRow, err := db.New(s.pool).GetUserByID(context.Background(), victim.User.ID)
	if err != nil {
		t.Fatalf("victim: %v", err)
	}
	stranger := "stranger-" + uuid.NewString()[:8] + "@example.com"
	s.cleanupEmail(stranger)

	cases := []struct {
		name     string
		sub      string
		email    string
		verified any
	}{
		{"unverified, existing account", "sub-" + uuid.NewString(), victimRow.Email, false},
		{"unverified string, existing account", "sub-" + uuid.NewString(), victimRow.Email, "false"},
		{"verified missing, existing account", "sub-" + uuid.NewString(), victimRow.Email, nil},
		{"unverified, no account", "sub-" + uuid.NewString(), stranger, false},
		{"empty subject, existing account", "", victimRow.Email, true},
		{"empty subject, no account", "", stranger, true},
		{"empty email", "sub-" + uuid.NewString(), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.identity(tc.sub, tc.email, tc.verified)
			state := s.startGoogle()
			before := s.counts()
			q, loc, _ := s.callback(url.Values{"state": {state}, "code": {"c"}})
			if q.Has("code") || q.Get("error") != "failed" {
				t.Errorf("Location %q: want error=failed and no code", loc)
			}
			if after := s.counts(); after != before {
				t.Errorf("rows changed: before %+v after %+v", before, after)
			}
			row, _ := db.New(s.pool).GetUserByID(context.Background(), victimRow.ID)
			if row.GoogleSubject.Valid {
				t.Errorf("victim was linked to %q", row.GoogleSubject.String)
			}
		})
	}

	// A Chorus account whose own email was never verified is not linked, even
	// to a Google-verified identity: whoever registered it may not own the
	// address, and linking would give them the Google user's sign-in.
	t.Run("unverified Chorus account is not linked", func(t *testing.T) {
		email := "unclaimed-" + uuid.NewString()[:8] + "@example.com"
		s.cleanupEmail(email)
		s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
			map[string]string{"email": email, "password": "correct-password-123", "display_name": "U"}, nil)
		fake.identity("sub-"+uuid.NewString(), email, true)
		state := s.startGoogle()
		before := s.counts()
		q, loc, _ := s.callback(url.Values{"state": {state}, "code": {"c"}})
		if q.Has("code") || q.Get("error") != "failed" {
			t.Errorf("Location %q: want error=failed and no code", loc)
		}
		if after := s.counts(); after != before {
			t.Errorf("rows changed: before %+v after %+v", before, after)
		}
		if u := s.userByEmail(email); u.GoogleSubject.Valid {
			t.Errorf("unverified account linked to %q", u.GoogleSubject.String)
		}
	})
}

// CLM-8: with Google unconfigured, config says so and the Google routes are
// 404s that never contact Google; with it configured, config says on.
func TestGoogle_DisabledAndConfig(t *testing.T) {
	fake := newFakeGoogle(t)
	off := newGoogleTestServer(t, nil)

	var cfg map[string]any
	off.expect(http.StatusOK, "GET", "/api/v1/auth/config", "", nil, &cfg)
	if cfg["google_sign_in"] != false || cfg["google_calendar"] != false || len(cfg) != 2 {
		t.Errorf("disabled config = %v, want both false", cfg)
	}
	for _, path := range []string{"/api/v1/auth/google/start", "/api/v1/auth/google/callback?state=x&code=y"} {
		status, loc, _ := off.get(path)
		if status != http.StatusNotFound || loc != "" {
			t.Errorf("GET %s: status %d Location %q, want 404", path, status, loc)
		}
	}
	var e apiError
	if status := off.do("POST", "/api/v1/auth/google/redeem", "", map[string]string{"code": "x", "device_name": "d"}, &e); status != http.StatusNotFound || e.Error.Code != "not_found" {
		t.Errorf("redeem disabled: status %d code %q, want 404 not_found", status, e.Error.Code)
	}
	if n := fake.hits.Load(); n != 0 {
		t.Errorf("fake Google got %d hits with Google disabled", n)
	}
	pw := off.signUp("stillworks")
	var me struct {
		Email string `json:"email"`
	}
	off.expect(http.StatusOK, "GET", "/api/v1/me", pw.AccessToken, nil, &me)
	off.expect(http.StatusOK, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": me.Email, "password": "correct-password-123"}, nil)

	on := newGoogleTestServer(t, fake)
	cfg = nil
	on.expect(http.StatusOK, "GET", "/api/v1/auth/config", "", nil, &cfg)
	if cfg["google_sign_in"] != true || cfg["google_calendar"] != false {
		t.Errorf("enabled config = %v, want google_sign_in true, google_calendar false", cfg)
	}
}
