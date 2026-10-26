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
	"strings"
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

// These tests drive the full HTTP stack (middleware, routing, handlers,
// services) against a real database, the same way a mobile client would.

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
		BCryptCost: 10, PasswordMinLen: 8, AppBaseURL: "http://chorus.test", AppName: "Chorus",
	})
	t.Cleanup(authSvc.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		ChoreSvc:     app.NewChoreService(q),
		EventSvc:     app.NewEventService(q),
		JWTSecret:    secret,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &testServer{t: t, srv: srv, mail: mail, pool: pool}
}

// do sends a JSON request and decodes a JSON response into out (if non-nil).
func (s *testServer) do(method, path, token string, body any, out any) int {
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
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	} `json:"error"`
}

func TestAPI_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("parent")
	kid := s.signUp("kid")

	// Household + invite + join.
	var h struct {
		ID   uuid.UUID `json:"id"`
		Role string    `json:"role"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "The Smiths", "timezone": "America/Chicago"}, &h)
	if h.Role != "admin" {
		t.Errorf("creator role: got %q", h.Role)
	}
	hp := "/api/v1/households/" + h.ID.String()

	var inv struct{ Code string }
	s.expect(http.StatusCreated, "POST", hp+"/invites", parent.AccessToken, nil, &inv)
	s.expect(http.StatusOK, "POST", "/api/v1/invites/accept", kid.AccessToken, map[string]string{"code": inv.Code}, nil)

	var members struct{ Items []map[string]any }
	s.expect(http.StatusOK, "GET", hp+"/members", kid.AccessToken, nil, &members)
	if len(members.Items) != 2 {
		t.Fatalf("members: got %d", len(members.Items))
	}

	// Chore CRUD.
	var chore struct {
		ID         uuid.UUID  `json:"id"`
		Title      string     `json:"title"`
		AssigneeID *uuid.UUID `json:"assignee_id"`
		DueOn      *string    `json:"due_on"`
		Completed  bool       `json:"completed"`
	}
	s.expect(http.StatusCreated, "POST", hp+"/chores", parent.AccessToken, map[string]any{
		"title": "Take out trash", "assignee_id": kid.User.ID, "due_on": "2026-11-01",
	}, &chore)
	if chore.DueOn == nil || *chore.DueOn != "2026-11-01" || *chore.AssigneeID != kid.User.ID {
		t.Errorf("created chore: %+v", chore)
	}
	cp := hp + "/chores/" + chore.ID.String()

	var mine struct {
		Items      []map[string]any `json:"items"`
		NextOffset *int             `json:"next_offset"`
	}
	s.expect(http.StatusOK, "GET", hp+"/chores?assignee=me", kid.AccessToken, nil, &mine)
	if len(mine.Items) != 1 || mine.NextOffset != nil {
		t.Errorf("kid's chores: %+v", mine)
	}

	s.expect(http.StatusOK, "PUT", cp+"/completion", kid.AccessToken, nil, &chore)
	if !chore.Completed {
		t.Error("chore should be completed")
	}
	s.expect(http.StatusOK, "PUT", cp, parent.AccessToken, map[string]any{"title": "Trash + recycling"}, &chore)
	if chore.Title != "Trash + recycling" || chore.AssigneeID != nil || chore.DueOn != nil {
		t.Errorf("PUT is a full replacement; got %+v", chore)
	}

	// Pagination.
	for i := range 3 {
		s.expect(http.StatusCreated, "POST", hp+"/chores", parent.AccessToken, map[string]any{"title": fmt.Sprintf("chore %d", i)}, nil)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextOffset *int             `json:"next_offset"`
	}
	s.expect(http.StatusOK, "GET", hp+"/chores?status=all&limit=2", parent.AccessToken, nil, &page)
	if len(page.Items) != 2 || page.NextOffset == nil || *page.NextOffset != 2 {
		t.Fatalf("page 1: %d items, next %v", len(page.Items), page.NextOffset)
	}
	s.expect(http.StatusOK, "GET", hp+"/chores?status=all&limit=2&offset=2", parent.AccessToken, nil, &page)
	if len(page.Items) != 2 || page.NextOffset != nil {
		t.Fatalf("page 2: %d items, next %v", len(page.Items), page.NextOffset)
	}
	// The documented bounds are inclusive.
	s.expect(http.StatusOK, "GET", hp+"/chores?limit=100", parent.AccessToken, nil, nil)
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/chores?limit=101", parent.AccessToken, nil, nil)
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/chores?limit=0", parent.AccessToken, nil, nil)

	s.expect(http.StatusNoContent, "DELETE", cp, kid.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", cp, kid.AccessToken, nil, nil)

	// Events.
	start := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	var ev struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", hp+"/events", kid.AccessToken, map[string]any{
		"title": "Soccer", "starts_at": start, "ends_at": start.Add(90 * time.Minute), "location": "Park",
	}, &ev)
	var evs struct{ Items []map[string]any }
	q := url.Values{"from": {"2026-11-01T00:00:00Z"}, "to": {"2026-12-01T00:00:00Z"}}
	s.expect(http.StatusOK, "GET", hp+"/events?"+q.Encode(), parent.AccessToken, nil, &evs)
	if len(evs.Items) != 1 {
		t.Errorf("events in November: got %d", len(evs.Items))
	}
	var bad apiError
	s.expect(http.StatusUnprocessableEntity, "POST", hp+"/events", kid.AccessToken, map[string]any{
		"title": "Backwards", "starts_at": start, "ends_at": start.Add(-time.Hour),
	}, &bad)
	if bad.Error.Fields["ends_at"] == "" {
		t.Errorf("expected ends_at field error, got %+v", bad)
	}

	// Outsiders see nothing.
	outsider := s.signUp("outsider")
	s.expect(http.StatusNotFound, "GET", hp, outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", hp+"/events/"+ev.ID.String(), outsider.AccessToken, nil, nil)
}

func TestAPI_SessionsAndLogout(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("sessions")

	s.expect(http.StatusOK, "GET", "/api/v1/me", u.AccessToken, nil, nil)

	// Refresh issues a working access token.
	var refreshed tokens
	s.expect(http.StatusOK, "POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": u.RefreshToken}, &refreshed)
	s.expect(http.StatusOK, "GET", "/api/v1/me", refreshed.AccessToken, nil, nil)

	// Logout kills both tokens immediately — not when the access token expires.
	s.expect(http.StatusNoContent, "POST", "/api/v1/auth/logout", u.AccessToken, nil, nil)
	var e apiError
	s.expect(http.StatusUnauthorized, "GET", "/api/v1/me", u.AccessToken, nil, &e)
	if e.Error.Code != "unauthenticated" {
		t.Errorf("after logout: code %q", e.Error.Code)
	}
	s.expect(http.StatusUnauthorized, "POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": u.RefreshToken}, nil)
}

func TestAPI_RequestHardening(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("hardening")

	var e apiError
	// Unknown fields are rejected rather than silently ignored.
	s.expect(http.StatusBadRequest, "POST", "/api/v1/households", u.AccessToken,
		map[string]any{"name": "x", "is_admin": true}, &e)
	if e.Error.Code != "invalid_json" {
		t.Errorf("unknown field: code %q", e.Error.Code)
	}

	// Non-JSON content types are rejected.
	req, _ := http.NewRequest("POST", s.srv.URL+"/api/v1/households", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+u.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form post: status %d, want 415", resp.StatusCode)
	}

	// Oversized bodies are rejected.
	s.expect(http.StatusRequestEntityTooLarge, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": strings.Repeat("x", 100<<10)}, nil)

	// Unknown API routes return JSON 404s, not the web client.
	s.expect(http.StatusNotFound, "GET", "/api/v1/nope", u.AccessToken, nil, &e)

	// Unverified users can see their account but nothing else.
	email := fmt.Sprintf("unverified-%s@example.com", uuid.NewString()[:8])
	t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", email) })
	s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
		map[string]string{"email": email, "password": "correct-password-123", "display_name": "U"}, nil)
	var unv tokens
	s.expect(http.StatusOK, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": email, "password": "correct-password-123"}, &unv)
	s.expect(http.StatusOK, "GET", "/api/v1/me", unv.AccessToken, nil, nil)
	s.expect(http.StatusForbidden, "GET", "/api/v1/households", unv.AccessToken, nil, &e)
	if e.Error.Code != "email_not_verified" {
		t.Errorf("unverified: code %q", e.Error.Code)
	}

	// Responses carry no-store and security headers.
	resp, err = http.Get(s.srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for h, want := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s: got %q, want %q", h, got, want)
		}
	}
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

// CLM-11: /me reports has_password, and a passwordless user asking to change
// a password gets a field validation error rather than invalid credentials.
func TestAPI_PasswordlessAccountBasics(t *testing.T) {
	s := newTestServer(t)
	pwUser := s.signUp("haspw")
	gUser := s.signUpPasswordless("nopw")

	var me struct {
		HasPassword *bool `json:"has_password"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/me", pwUser.AccessToken, nil, &me)
	if me.HasPassword == nil || !*me.HasPassword {
		t.Errorf("password user: has_password = %v, want true", me.HasPassword)
	}
	me.HasPassword = nil
	s.expect(http.StatusOK, "GET", "/api/v1/me", gUser.AccessToken, nil, &me)
	if me.HasPassword == nil || *me.HasPassword {
		t.Errorf("passwordless user: has_password = %v, want false", me.HasPassword)
	}

	var before, after db.User
	q := db.New(s.pool)
	before, _ = q.GetUserByID(context.Background(), gUser.ID)
	var e apiError
	s.expect(http.StatusUnprocessableEntity, "POST", "/api/v1/me/password", gUser.AccessToken,
		map[string]string{"current_password": "anything", "new_password": "brand-new-password"}, &e)
	if e.Error.Code == "invalid_credentials" || e.Error.Fields["current_password"] == "" || e.Error.Fields["current_password"] == "is incorrect" {
		t.Errorf("passwordless change password: got %+v, want a current_password field error that isn't invalid credentials", e)
	}
	after, _ = q.GetUserByID(context.Background(), gUser.ID)
	if after.PasswordHash != before.PasswordHash || after.PasswordHash.Valid || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		t.Errorf("passwordless user row changed: before %+v after %+v", before.PasswordHash, after.PasswordHash)
	}

	// CLM-13: password sign-in to a passwordless account is invalid credentials.
	s.expect(http.StatusUnauthorized, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": gUser.Email, "password": "correct-password-123"}, &e)
	if e.Error.Code != "invalid_credentials" {
		t.Errorf("passwordless login: code %q, want invalid_credentials", e.Error.Code)
	}
}

// CLM-10: a passwordless user deletes their account by typing their email
// from a recently created session; password users still need their password.
func TestAPI_DeleteAccountPasswordless(t *testing.T) {
	s := newTestServer(t)

	t.Run("matching email from a fresh session deletes", func(t *testing.T) {
		u := s.signUpPasswordless("delok")
		s.expect(http.StatusNoContent, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": "  " + strings.ToUpper(u.Email) + " "}, nil)
		if s.userExists(u.ID) {
			t.Error("user still exists after delete")
		}
	})

	t.Run("wrong email is refused", func(t *testing.T) {
		u := s.signUpPasswordless("delwrong")
		var e apiError
		s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": "someone-else@example.com"}, &e)
		if e.Error.Fields["email"] == "" {
			t.Errorf("wrong email: expected email field error, got %+v", e)
		}
		s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"password": "correct-password-123"}, nil)
		if !s.userExists(u.ID) {
			t.Error("user deleted despite wrong email")
		}
	})

	t.Run("stale session is refused with re-sign-in message", func(t *testing.T) {
		u := s.signUpPasswordless("delstale")
		if _, err := s.pool.Exec(context.Background(),
			"UPDATE refresh_tokens SET created_at = NOW() - INTERVAL '11 minutes' WHERE id = $1", u.SessionID); err != nil {
			t.Fatalf("backdate session: %v", err)
		}
		var e apiError
		s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": u.Email}, &e)
		if !strings.Contains(e.Error.Fields["email"], "sign in with Google again") {
			t.Errorf("stale session: expected re-sign-in message, got %+v", e)
		}
		if !s.userExists(u.ID) {
			t.Error("user deleted from a stale session")
		}
	})

	t.Run("sole admin of a household with members is blocked", func(t *testing.T) {
		u := s.signUpPasswordless("deladmin")
		other := s.signUp("delmember")
		var h struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
			map[string]string{"name": "Google Home", "timezone": "UTC"}, &h)
		var inv struct{ Code string }
		s.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/invites", u.AccessToken, nil, &inv)
		s.expect(http.StatusOK, "POST", "/api/v1/invites/accept", other.AccessToken, map[string]string{"code": inv.Code}, nil)

		var e apiError
		s.expect(http.StatusConflict, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": u.Email}, &e)
		if e.Error.Code != "sole_admin" {
			t.Errorf("sole admin: code %q, want sole_admin", e.Error.Code)
		}
		if !s.userExists(u.ID) {
			t.Error("sole admin deleted")
		}
	})

	t.Run("password user still needs the correct password", func(t *testing.T) {
		u := s.signUp("delpw")
		for _, body := range []map[string]string{
			{"password": "wrong-password"},
			{"password": ""},
			{"email": "whatever@example.com"},
		} {
			var e apiError
			s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken, body, &e)
			if e.Error.Fields["password"] != "is incorrect" {
				t.Errorf("password user %v: got %+v, want password 'is incorrect'", body, e)
			}
		}
		if !s.userExists(u.User.ID) {
			t.Error("password user deleted without the right password")
		}
	})
}
