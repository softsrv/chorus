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
	// Google account identity, for the tokeninfo endpoint: codeSub is the
	// account a code exchange's access token belongs to; refreshSubs maps a
	// refresh token to its account, and tokens minted from it carry that sub.
	codeSub     string
	refreshSubs map[string]string
	accessSubs  map[string]string // access token -> sub, filled as issued
}

func newFakeCalendarGoogle(t *testing.T) *fakeCalendarGoogle {
	t.Helper()
	f := &fakeCalendarGoogle{grantedScope: calBothScopes, refreshToken: "refresh-" + uuid.NewString(),
		codeSub: "google-sub-A", refreshSubs: map[string]string{}, accessSubs: map[string]string{}}
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
			f.accessSubs[access] = f.codeSub
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
			f.accessSubs[access] = f.refreshSubs[r.Form.Get("refresh_token")]
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
	mux.HandleFunc("POST /tokeninfo", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// The token must arrive in the form body, never the URL query.
		sub := ""
		if r.URL.RawQuery == "" {
			sub = f.accessSubs[r.PostForm.Get("access_token")]
		}
		if sub == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"invalid_token","error_description":"%s"}`, sentinel)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": sub})
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
			TokenInfoURL: fake.srv.URL + "/tokeninfo",
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
