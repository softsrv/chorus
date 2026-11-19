//go:build integration

package http_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

// sentinel is what a fake Google puts in every error body. It must never
// reach a response body or a redirect Location.
const sentinel = "INTERNAL-SENTINEL"

// Obviously fake client IDs: the accepted ID-token audiences.
const (
	webClientID     = "web-client.test"
	iosClientID     = "ios-client.test"
	androidClientID = "android-client.test"
	fakeSecret      = "fake-client-secret"
)

// testKeyID names the fake Google signing key in the JWKS and token headers.
const testKeyID = "test-kid-1"

// googleKey and forgerKey are throwaway RSA keys generated at runtime; none is
// committed. forgerKey signs "forged" tokens under googleKey's key id.
var (
	googleKey = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey() })
	forgerKey = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey() })
)

func mustRSAKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

// fakeGoogleKeys serves Google's JWKS endpoint with the runtime test key and
// counts every request, so tests can assert Google was never contacted.
type fakeGoogleKeys struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newFakeGoogleKeys(t *testing.T) *fakeGoogleKeys {
	t.Helper()
	f := &fakeGoogleKeys{}
	pub := googleKey().PublicKey
	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": testKeyID,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/certs" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, sentinel)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// idClaims returns the claims of a valid Google ID token for sub and email,
// issued for the web client. Tests change or delete fields to build bad ones.
func idClaims(sub, email string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": webClientID, "sub": sub,
		"email": email, "email_verified": true, "name": "Google Person",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// signID signs claims RS256 with the fake Google key under testKeyID.
func signID(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	return signIDWith(t, googleKey(), claims)
}

func signIDWith(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKeyID
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}
	return s
}

// googleOpts configures newGoogleTestServer. A nil keys disables Google
// sign-in; clientIDs are the accepted audiences (web first); calendar wires a
// Google Calendar service so config reports it on.
type googleOpts struct {
	keys      *fakeGoogleKeys
	clientIDs []string
	calendar  bool
}

// newGoogleTestServer is newTestServer with Google sign-in wired to the fake
// key set (or disabled when opts.keys is nil).
func newGoogleTestServer(t *testing.T, opts googleOpts) *testServer {
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
	if opts.keys != nil {
		ids := opts.clientIDs
		if ids == nil {
			ids = []string{webClientID}
		}
		google = app.NewGoogleSignIn(q, authSvc, app.GoogleSignInConfig{
			ClientIDs: ids,
			JWKSURL:   opts.keys.srv.URL + "/certs",
		})
	}
	var calendar *app.GoogleCalendar
	if opts.calendar {
		cipher, err := app.NewTokenCipher(calendarKey)
		if err != nil {
			t.Fatalf("NewTokenCipher: %v", err)
		}
		calendar = app.NewGoogleCalendar(q, cipher, app.GoogleCalendarConfig{
			ClientID: webClientID, ClientSecret: fakeSecret, RedirectURL: calCallbackURL,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:        app.NewUserService(q),
		HouseholdSvc:   app.NewHouseholdService(q, pool),
		GoogleSignIn:   google,
		GoogleCalendar: calendar,
		JWTSecret:      secret,
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

// postRaw posts body as-is (with the JSON content type) and returns the
// status and raw response body.
func (s *testServer) postRaw(path string, body []byte) (int, string) {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirect.Do(req)
	if err != nil {
		s.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
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

// googleSignIn posts an ID token to POST /auth/google. It returns the status,
// the decoded tokens (on 200) and the raw body.
func (s *testServer) googleSignIn(idToken, device string) (int, googleTokens, string) {
	s.t.Helper()
	b, _ := json.Marshal(map[string]string{"id_token": idToken, "device_name": device})
	status, body := s.postRaw("/api/v1/auth/google", b)
	var out googleTokens
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			s.t.Fatalf("decode sign-in body %q: %v", body, err)
		}
	}
	return status, out, body
}

type rowCounts struct{ users, households, refreshTokens int }

func (s *testServer) counts() rowCounts {
	s.t.Helper()
	var c rowCounts
	if err := s.pool.QueryRow(context.Background(), `SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM households),
		(SELECT COUNT(*) FROM refresh_tokens)`).Scan(&c.users, &c.households, &c.refreshTokens); err != nil {
		s.t.Fatalf("counts: %v", err)
	}
	return c
}

// cleanupEmail deletes the user (and the households it alone administers) at
// the end of the test.
func (s *testServer) cleanupEmail(email string) {
	s.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.pool.Exec(ctx, `DELETE FROM households WHERE id IN (
			SELECT household_id FROM household_members m JOIN users u ON u.id = m.user_id WHERE u.email = $1)`, email)
		_, _ = s.pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email)
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

// adminHouseholds counts the households userID administers.
func (s *testServer) adminHouseholds(userID uuid.UUID) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM household_members WHERE user_id = $1 AND role = 'admin'", userID).Scan(&n); err != nil {
		s.t.Fatalf("count households: %v", err)
	}
	return n
}

// forgedBody signs in with a token signed by the wrong key and returns the
// refusal's status and body: the one generic error every refusal must match.
func forgedBody(t *testing.T, s *testServer) (int, string) {
	t.Helper()
	claims := idClaims("sub-"+uuid.NewString(), "forged-"+uuid.NewString()[:8]+"@example.com")
	status, _, body := s.googleSignIn(signIDWith(t, forgerKey(), claims), "forged")
	if status < 400 || status >= 500 {
		t.Fatalf("forged token: status %d, want 4xx (body %q)", status, body)
	}
	return status, body
}

// CLM-4: a valid token for a new Google identity creates one verified,
// passwordless user linked to the subject, its household with the user as
// admin, and one session under the sent device name.
func TestGoogle_NewUserSignIn(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})

	local := "newuser-" + uuid.NewString()[:8]
	normalized := local + "@example.com"
	s.cleanupEmail(normalized)
	sub := "sub-" + uuid.NewString()
	before := s.counts()

	status, tok, body := s.googleSignIn(signID(t, idClaims(sub, "  "+strings.ToUpper(local)+"@Example.COM")), "Sam's Laptop")
	if status != http.StatusOK || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("sign-in: status %d body %q", status, body)
	}
	for _, field := range []string{`"token_type"`, `"access_token_expires_at"`, `"refresh_token_expires_at"`} {
		if !strings.Contains(body, field) {
			t.Errorf("sign-in body lacks %s, want the login token pair: %q", field, body)
		}
	}
	if tok.User.Email != normalized || !tok.User.EmailVerified || tok.User.HasPassword {
		t.Errorf("sign-in user = %+v, want %s verified without password", tok.User, normalized)
	}

	u := s.userByEmail(normalized)
	if u.ID != tok.User.ID || !u.EmailVerified || u.PasswordHash.Valid || !u.GoogleSubject.Valid || u.GoogleSubject.String != sub {
		t.Errorf("user row = %+v, want verified, NULL password_hash, google_subject %s", u, sub)
	}
	if after := s.counts(); after.users != before.users+1 || after.households != before.households+1 || after.refreshTokens != before.refreshTokens+1 {
		t.Errorf("rows: before %+v after %+v, want one more user, household and session", before, after)
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

	var households struct {
		Items []struct {
			Role string `json:"role"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households", tok.AccessToken, nil, &households)
	if len(households.Items) != 1 || households.Items[0].Role != "admin" {
		t.Errorf("expected 1 admin household after Google sign-in, got %+v", households.Items)
	}
}

// CLM-2: the accepted audiences are the web client ID plus the iOS and
// Android IDs when configured. With only web configured, an iOS or Android
// token gets the same refusal as a forged one.
func TestGoogle_AudienceSet(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	webOnly := newGoogleTestServer(t, googleOpts{keys: keys, clientIDs: []string{webClientID}})
	all := newGoogleTestServer(t, googleOpts{keys: keys, clientIDs: []string{webClientID, iosClientID, androidClientID}})
	forgedStatus, forged := forgedBody(t, webOnly)

	signInAs := func(s *testServer, aud string) (int, googleTokens, string) {
		email := "aud-" + uuid.NewString()[:8] + "@example.com"
		s.cleanupEmail(email)
		claims := idClaims("sub-"+uuid.NewString(), email)
		claims["aud"] = aud
		return s.googleSignIn(signID(t, claims), "device-"+aud)
	}

	if status, tok, body := signInAs(webOnly, webClientID); status != http.StatusOK || tok.AccessToken == "" {
		t.Errorf("web-only server, web token: status %d body %q", status, body)
	}
	for _, aud := range []string{iosClientID, androidClientID} {
		before := webOnly.counts()
		status, _, body := signInAs(webOnly, aud)
		if status != forgedStatus || body != forged {
			t.Errorf("web-only server, %s token: %d %q, want the forged-token refusal %d %q", aud, status, body, forgedStatus, forged)
		}
		if after := webOnly.counts(); after != before {
			t.Errorf("%s refusal changed rows: before %+v after %+v", aud, before, after)
		}
	}
	for _, aud := range []string{webClientID, iosClientID, androidClientID} {
		if status, tok, body := signInAs(all, aud); status != http.StatusOK || tok.AccessToken == "" {
			t.Errorf("all-apps server, %s token: status %d body %q", aud, status, body)
		}
	}
}

// CLM-5: a Google identity whose verified email matches a verified password
// account signs into that account and links it; the password keeps working.
func TestGoogle_LinksExistingPasswordAccount(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})

	tag := uuid.NewString()[:8]
	registered := "Mixed-" + tag + "@Case.example"
	normalized := strings.ToLower(registered)
	s.cleanupEmail(normalized)
	s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
		map[string]string{"email": registered, "password": "correct-password-123", "display_name": "Mixed"}, nil)
	s.expect(http.StatusOK, "POST", "/api/v1/auth/verify-email", "",
		map[string]string{"token": s.mail.token(t, normalized)}, nil)
	before := s.userByEmail(normalized)

	sub := "sub-" + uuid.NewString()
	usersBefore := s.counts().users

	status, tok, body := s.googleSignIn(signID(t, idClaims(sub, "mixed-"+tag+"@case.example")), "linking")
	if status != http.StatusOK || tok.User.ID != before.ID {
		t.Fatalf("sign-in: status %d user %s body %q, want existing %s", status, tok.User.ID, body, before.ID)
	}
	if got := s.counts().users; got != usersBefore {
		t.Errorf("users count %d, want %d (no new user)", got, usersBefore)
	}
	after := s.userByEmail(normalized)
	if after.GoogleSubject.String != sub || after.PasswordHash != before.PasswordHash {
		t.Errorf("linked row: subject %q (want %q), password hash changed %v", after.GoogleSubject.String, sub, after.PasswordHash != before.PasswordHash)
	}
	s.expect(http.StatusOK, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": registered, "password": "correct-password-123"}, nil)

	// Later sign-in by subject, even if Google now reports another email.
	if status, again, body := s.googleSignIn(signID(t, idClaims(sub, "renamed-"+tag+"@example.com")), "again"); status != http.StatusOK || again.User.ID != before.ID {
		t.Errorf("repeat sign-in: status %d user %s body %q, want %s", status, again.User.ID, body, before.ID)
	}

	// A password-locked account can still sign in with Google.
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE users SET locked_until = NOW() + INTERVAL '1 hour', failed_login_attempts = 10 WHERE id = $1", before.ID); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if status, locked, body := s.googleSignIn(signID(t, idClaims(sub, normalized)), "locked"); status != http.StatusOK || locked.User.ID != before.ID {
		t.Errorf("locked account Google sign-in: status %d user %s body %q", status, locked.User.ID, body)
	}
}

// CLM-7: forged, expired, wrong-issuer, wrong-audience and unverified-email
// tokens all get one identical generic refusal that leaks nothing, and
// create no user, household or session.
func TestGoogle_RefusesBadTokens(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})
	email := "bad-" + uuid.NewString()[:8] + "@example.com"
	s.cleanupEmail(email)
	claims := func(edit func(jwt.MapClaims)) jwt.MapClaims {
		c := idClaims("sub-"+uuid.NewString(), email)
		edit(c)
		return c
	}

	cases := []struct {
		name  string
		token string
	}{
		{"forged signature", signIDWith(t, forgerKey(), claims(func(jwt.MapClaims) {}))},
		{"expired", signID(t, claims(func(c jwt.MapClaims) {
			c["iat"] = time.Now().Add(-3 * time.Hour).Unix()
			c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
		}))},
		{"wrong issuer", signID(t, claims(func(c jwt.MapClaims) { c["iss"] = "https://issuer.example" }))},
		{"wrong audience", signID(t, claims(func(c jwt.MapClaims) { c["aud"] = "other-app.test" }))},
		{"unverified email", signID(t, claims(func(c jwt.MapClaims) { c["email_verified"] = false }))},
	}
	var firstStatus int
	var firstBody string
	for i, tc := range cases {
		before := s.counts()
		status, _, body := s.googleSignIn(tc.token, "bad")
		if status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want %d", tc.name, status, http.StatusUnauthorized)
		}
		var e apiError
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Errorf("%s: body %q is not a JSON error: %v", tc.name, body, err)
		} else if e.Error.Code != "google_sign_in_failed" {
			t.Errorf("%s: error.code %q, want %q", tc.name, e.Error.Code, "google_sign_in_failed")
		}
		if i == 0 {
			firstStatus, firstBody = status, body
		} else if status != firstStatus || body != firstBody {
			t.Errorf("%s: %d %q, want the identical refusal %d %q", tc.name, status, body, firstStatus, firstBody)
		}
		lower := strings.ToLower(body)
		for _, leak := range []string{strings.ToLower(sentinel), "signature", "expired", "audience", "issuer", "jwt", "verified", "pgx", "sql"} {
			if strings.Contains(lower, leak) {
				t.Errorf("%s: body leaks %q: %q", tc.name, leak, body)
			}
		}
		if after := s.counts(); after != before {
			t.Errorf("%s: rows changed: before %+v after %+v", tc.name, before, after)
		}
	}
}

// CLM-7: an identity whose email Google has not verified, or that lacks a
// subject or email, never links to or creates an account.
func TestGoogle_RefusesUnverifiedIdentity(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})
	forgedStatus, forged := forgedBody(t, s)

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
		verified any // nil = claim omitted
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
			c := idClaims(tc.sub, tc.email)
			if tc.verified == nil {
				delete(c, "email_verified")
			} else {
				c["email_verified"] = tc.verified
			}
			before := s.counts()
			status, _, body := s.googleSignIn(signID(t, c), "unverified")
			if status != forgedStatus || body != forged {
				t.Errorf("status %d body %q, want the generic refusal %d %q", status, body, forgedStatus, forged)
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
}

// CLM-8: a valid token whose email matches an unverified password account,
// or an account linked to a different Google subject, is refused with the
// generic error and changes nothing.
func TestGoogle_RefusesConflictingAccounts(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})
	forgedStatus, forged := forgedBody(t, s)

	t.Run("unverified password account is not linked", func(t *testing.T) {
		// Whoever registered it may not own the address; linking would give
		// them the Google user's sign-in.
		email := "unclaimed-" + uuid.NewString()[:8] + "@example.com"
		s.cleanupEmail(email)
		s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
			map[string]string{"email": email, "password": "correct-password-123", "display_name": "U"}, nil)
		userBefore := s.userByEmail(email)
		before := s.counts()
		status, _, body := s.googleSignIn(signID(t, idClaims("sub-"+uuid.NewString(), email)), "conflict")
		if status != forgedStatus || body != forged {
			t.Errorf("status %d body %q, want the generic refusal %d %q", status, body, forgedStatus, forged)
		}
		if after := s.counts(); after != before {
			t.Errorf("rows changed: before %+v after %+v", before, after)
		}
		if u := s.userByEmail(email); u != userBefore {
			t.Errorf("user row changed: before %+v after %+v", userBefore, u)
		}
	})

	t.Run("account linked to another Google identity", func(t *testing.T) {
		email := "linked-" + uuid.NewString()[:8] + "@example.com"
		s.cleanupEmail(email)
		subA := "sub-a-" + uuid.NewString()
		if status, _, body := s.googleSignIn(signID(t, idClaims(subA, email)), "a"); status != http.StatusOK {
			t.Fatalf("setup sign-in as sub A: status %d body %q", status, body)
		}
		userBefore := s.userByEmail(email)
		before := s.counts()
		status, _, body := s.googleSignIn(signID(t, idClaims("sub-b-"+uuid.NewString(), email)), "b")
		if status != forgedStatus || body != forged {
			t.Errorf("status %d body %q, want the generic refusal %d %q", status, body, forgedStatus, forged)
		}
		if after := s.counts(); after != before {
			t.Errorf("rows changed: before %+v after %+v", before, after)
		}
		if u := s.userByEmail(email); u != userBefore || u.GoogleSubject.String != subA {
			t.Errorf("user row changed: before %+v after %+v", userBefore, u)
		}
	})
}

// CLM-9: a missing, empty or whitespace id_token, an empty or malformed body,
// or an oversized body gets a 4xx JSON error without contacting Google's key
// endpoint, and creates nothing.
func TestGoogle_MalformedRequests(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})

	huge := `{"id_token":"` + strings.Repeat("a", 70<<10) + `"}`
	cases := map[string]string{
		"missing id_token":    `{"device_name":"d"}`,
		"empty id_token":      `{"id_token":""}`,
		"whitespace id_token": `{"id_token":"   \t\n "}`,
		"empty body":          ``,
		"malformed json":      `{"id_token":`,
		"oversized body":      huge,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			before := s.counts()
			status, resp := s.postRaw("/api/v1/auth/google", []byte(body))
			if status < 400 || status >= 500 {
				t.Errorf("status %d, want 4xx (body %q)", status, resp)
			}
			var e apiError
			if err := json.Unmarshal([]byte(resp), &e); err != nil || e.Error.Code == "" {
				t.Errorf("body %q is not a JSON error", resp)
			}
			if after := s.counts(); after != before {
				t.Errorf("rows changed: before %+v after %+v", before, after)
			}
		})
	}
	if n := keys.hits.Load(); n != 0 {
		t.Errorf("Google's key endpoint got %d requests for malformed requests, want 0", n)
	}
}

// CLM-10, CLM-11: config reports Google sign-in, calendar and the web client
// ID (never the secret or the native IDs); with Google unconfigured the
// sign-in route is the JSON 404 and Google is never contacted. The old
// redirect routes are gone.
func TestGoogle_DisabledAndConfig(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	off := newGoogleTestServer(t, googleOpts{})

	type authConfig struct {
		GoogleSignIn   bool   `json:"google_sign_in"`
		GoogleCalendar bool   `json:"google_calendar"`
		GoogleClientID string `json:"google_client_id"`
	}
	readConfig := func(s *testServer) (authConfig, string) {
		status, _, raw := s.get("/api/v1/auth/config")
		if status != http.StatusOK {
			t.Fatalf("GET /auth/config: status %d", status)
		}
		var c authConfig
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatalf("decode config %q: %v", raw, err)
		}
		return c, raw
	}

	if c, raw := readConfig(off); c != (authConfig{}) {
		t.Errorf("disabled config = %s, want sign-in and calendar off, no client id", raw)
	}
	var e apiError
	token := signID(t, idClaims("sub-"+uuid.NewString(), "off-"+uuid.NewString()[:8]+"@example.com"))
	if status := off.do("POST", "/api/v1/auth/google", "", map[string]string{"id_token": token}, &e); status != http.StatusNotFound || e.Error.Code != "not_found" {
		t.Errorf("sign-in disabled: status %d code %q, want 404 not_found", status, e.Error.Code)
	}
	if n := keys.hits.Load(); n != 0 {
		t.Errorf("fake Google got %d hits with Google disabled", n)
	}
	pw := off.signUp("stillworks")
	var me struct {
		Email string `json:"email"`
	}
	off.expect(http.StatusOK, "GET", "/api/v1/me", pw.AccessToken, nil, &me)
	off.expect(http.StatusOK, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": me.Email, "password": "correct-password-123"}, nil)

	all := []string{webClientID, iosClientID, androidClientID}
	on := newGoogleTestServer(t, googleOpts{keys: keys, clientIDs: all})
	if c, raw := readConfig(on); c != (authConfig{GoogleSignIn: true, GoogleClientID: webClientID}) {
		t.Errorf("enabled config without calendar = %s", raw)
	}
	cal := newGoogleTestServer(t, googleOpts{keys: keys, clientIDs: all, calendar: true})
	c, raw := readConfig(cal)
	if c != (authConfig{GoogleSignIn: true, GoogleCalendar: true, GoogleClientID: webClientID}) {
		t.Errorf("enabled config with calendar = %s", raw)
	}
	for _, secretish := range []string{fakeSecret, iosClientID, androidClientID} {
		if strings.Contains(raw, secretish) {
			t.Errorf("config leaks %q: %s", secretish, raw)
		}
	}

	// CLM-12: the redirect sign-in routes are gone, configured or not; none
	// redirects or sets a cookie, and the old landing page is no longer the
	// single-page app.
	_, indexLoc, index := on.get("/")
	if indexLoc != "" || index == "" {
		t.Fatalf("GET /: Location %q, empty body %v; want the SPA index", indexLoc, index == "")
	}
	for _, s := range []*testServer{off, on} {
		for _, rt := range []struct{ method, path string }{
			{"GET", "/api/v1/auth/google/start"},
			{"GET", "/api/v1/auth/google/callback?state=x&code=y"},
			{"POST", "/api/v1/auth/google/redeem"},
			{"GET", "/google-sign-in?code=x"},
		} {
			req, _ := http.NewRequest(rt.method, s.srv.URL+rt.path, strings.NewReader(`{"code":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := noRedirect.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", rt.method, rt.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Location") != "" || len(resp.Header.Values("Set-Cookie")) != 0 {
				t.Errorf("%s %s: status %d Location %q Set-Cookie %q, want 404 with neither",
					rt.method, rt.path, resp.StatusCode, resp.Header.Get("Location"), resp.Header.Values("Set-Cookie"))
			}
			if string(body) == index {
				t.Errorf("%s %s served the single-page app index", rt.method, rt.path)
			}
		}
	}
}

// CLM-17: POST /auth/google sits behind one per-IP limiter (30 per 15
// minutes, like the other public auth routes); past it the caller gets 429
// with Retry-After and Google is never contacted.
func TestGoogle_SignInRateLimited(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})
	empty := []byte(`{"id_token":""}`)
	for i := range 30 {
		if status, body := s.postRaw("/api/v1/auth/google", empty); status == http.StatusTooManyRequests {
			t.Fatalf("request %d limited early: %s", i+1, body)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+"/api/v1/auth/google", bytes.NewReader(empty))
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST /auth/google: %v", err)
	}
	defer resp.Body.Close()
	var e apiError
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || e.Error.Code != "rate_limited" {
		t.Fatalf("31st request: status %d Retry-After %q code %q, want 429 rate_limited with Retry-After",
			resp.StatusCode, resp.Header.Get("Retry-After"), e.Error.Code)
	}
	if n := keys.hits.Load(); n != 0 {
		t.Errorf("fake Google got %d hits for empty tokens", n)
	}
}

// CLM-18: concurrent first sign-ins for one new identity end with exactly one
// user and one household, and no caller gets a 5xx.
func TestGoogle_ConcurrentFirstSignIn(t *testing.T) {
	keys := newFakeGoogleKeys(t)
	s := newGoogleTestServer(t, googleOpts{keys: keys})
	email := "race-" + uuid.NewString()[:8] + "@example.com"
	s.cleanupEmail(email)
	token := signID(t, idClaims("sub-"+uuid.NewString(), email))
	_, generic := forgedBody(t, s)

	const n = 8
	type result struct {
		status int
		tok    googleTokens
		body   string
	}
	results := make([]result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, _ := json.Marshal(map[string]string{"id_token": token, "device_name": fmt.Sprintf("racer-%d", i)})
			resp, err := http.Post(s.srv.URL+"/api/v1/auth/google", "application/json", bytes.NewReader(b))
			if err != nil {
				results[i] = result{body: err.Error()}
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			r := result{status: resp.StatusCode, body: string(raw)}
			if r.status == http.StatusOK {
				_ = json.Unmarshal(raw, &r.tok)
			}
			results[i] = r
		}()
	}
	close(start)
	wg.Wait()

	var ids = map[uuid.UUID]bool{}
	for i, r := range results {
		switch {
		case r.status == http.StatusOK && r.tok.AccessToken != "":
			ids[r.tok.User.ID] = true
		case r.status != http.StatusOK && r.body == generic:
			// a clean generic refusal for a loser
		default:
			t.Errorf("caller %d: status %d body %q, want a session or the generic refusal", i, r.status, r.body)
		}
	}
	var users int
	if err := s.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM users WHERE email = $1", email).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 || len(ids) > 1 {
		t.Fatalf("users with %s = %d, distinct signed-in ids %d; want exactly one account", email, users, len(ids))
	}
	if h := s.adminHouseholds(s.userByEmail(email).ID); h != 1 {
		t.Errorf("households for the new account = %d, want 1", h)
	}
}
