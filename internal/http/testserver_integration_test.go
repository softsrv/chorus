//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sync"
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

// The shared harness for the integration tests: a real router over a real
// database, and the JSON client helpers the tests drive it with.

type mailbox struct {
	mu   sync.Mutex
	text map[string]string // recipient → latest plain-text body
}

func (m *mailbox) Send(to, _, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.text[to] = text
	return nil
}

var tokenRE = regexp.MustCompile(`token=([A-Za-z0-9_\-=]+)`)

func (m *mailbox) token(t *testing.T, to string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		m.mu.Lock()
		body := m.text[to]
		m.mu.Unlock()
		if match := tokenRE.FindStringSubmatch(body); match != nil {
			tok, _ := url.QueryUnescape(match[1])
			return tok
		}
	}
	t.Fatalf("no token emailed to %s", to)
	return ""
}

type testServer struct {
	t    *testing.T
	srv  *httptest.Server
	mail *mailbox
	pool *pgxpool.Pool
}

const secret = "router-integration-secret-at-least-32-bytes"

func newTestServer(t *testing.T) *testServer {
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
		BCryptCost: 4, PasswordMinLen: 8, AppBaseURL: "http://chorus.test", AppName: "Chorus",
	})
	t.Cleanup(authSvc.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		FamilySvc:    app.NewFamilyService(q, pool, 4),
		JWTSecret:    secret,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &testServer{t: t, srv: srv, mail: mail, pool: pool}
}

// do sends a JSON request and decodes a JSON response into out (if non-nil).
func (s *testServer) do(method, path, token string, body any, out any) int {
	return s.doWithHeaders(method, path, token, nil, body, out)
}

func (s *testServer) doWithHeaders(method, path, token string, headers map[string]string, body any, out any) int {
	s.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, s.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			s.t.Fatalf("%s %s: decode (status %d): %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

func (s *testServer) expect(want int, method, path, token string, body any, out any) {
	s.t.Helper()
	if got := s.do(method, path, token, body, out); got != want {
		s.t.Fatalf("%s %s: status %d, want %d", method, path, got, want)
	}
}

func (s *testServer) expectWithHeaders(want int, method, path, token string, headers map[string]string, body any, out any) {
	s.t.Helper()
	if got := s.doWithHeaders(method, path, token, headers, body, out); got != want {
		s.t.Fatalf("%s %s: status %d, want %d", method, path, got, want)
	}
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID            uuid.UUID `json:"id"`
		EmailVerified bool      `json:"email_verified"`
	} `json:"user"`
}

// signUp registers and verifies a user via the API and returns their tokens.
func (s *testServer) signUp(name string) tokens {
	s.t.Helper()
	email := fmt.Sprintf("%s-%s@example.com", name, uuid.NewString()[:8])
	s.t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", email)
	})
	s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
		map[string]string{"email": email, "password": "correct-password-123", "display_name": name}, nil)

	var tok tokens
	s.expect(http.StatusOK, "POST", "/api/v1/auth/verify-email", "",
		map[string]string{"token": s.mail.token(s.t, email)}, &tok)
	if !tok.User.EmailVerified || tok.AccessToken == "" {
		s.t.Fatalf("verify-email returned %+v", tok)
	}
	return tok
}

type apiError struct {
	Error struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	} `json:"error"`
}

// passwordless is a Google-only account (NULL password_hash) signed in on one
// session. It is built straight from the queries the Google sign-in flow uses,
// so these tests don't depend on talking to Google.
type passwordless struct {
	ID          uuid.UUID
	Email       string
	SessionID   uuid.UUID
	AccessToken string
}

func (s *testServer) signUpPasswordless(name string) passwordless {
	s.t.Helper()
	ctx := context.Background()
	q := db.New(s.pool)
	id := uuid.Must(uuid.NewV7())
	email := fmt.Sprintf("%s-%s@example.com", name, uuid.NewString()[:8])
	user, err := q.CreateGoogleUser(ctx, db.CreateGoogleUserParams{
		ID: id, Email: email, DisplayName: name,
		GoogleSubject: pgtype.Text{String: "google-sub-" + id.String(), Valid: true},
	})
	if err != nil {
		s.t.Fatalf("CreateGoogleUser: %v", err)
	}
	s.t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", user.ID) })

	_, hashed, err := auth.GenerateRefreshToken()
	if err != nil {
		s.t.Fatalf("GenerateRefreshToken: %v", err)
	}
	sessionID := uuid.Must(uuid.NewV7())
	if _, err := q.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		ID: sessionID, UserID: user.ID, TokenHash: hashed,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		s.t.Fatalf("InsertRefreshToken: %v", err)
	}
	tp, err := auth.IssueAccessToken(user.ID, sessionID, secret, 15*time.Minute)
	if err != nil {
		s.t.Fatalf("IssueAccessToken: %v", err)
	}
	return passwordless{ID: user.ID, Email: email, SessionID: sessionID, AccessToken: tp.AccessToken}
}

func (s *testServer) userExists(id uuid.UUID) bool {
	s.t.Helper()
	var found bool
	if err := s.pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)", id).Scan(&found); err != nil {
		s.t.Fatalf("user exists: %v", err)
	}
	return found
}
