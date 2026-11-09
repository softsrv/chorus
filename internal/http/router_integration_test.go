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
		FamilySvc:    app.NewFamilyService(q, 10),
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
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	} `json:"error"`
}

func TestAPI_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("parent")

	// Verify auto-provisioning created an admin household on first sign-in.
	var initialHouseholds struct {
		Items []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
			Role string    `json:"role"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households", parent.AccessToken, nil, &initialHouseholds)
	if len(initialHouseholds.Items) != 1 || initialHouseholds.Items[0].Role != "admin" {
		t.Errorf("expected 1 admin household after sign-up, got %+v", initialHouseholds.Items)
	}

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

	// Chore CRUD.
	var chore struct {
		ID         uuid.UUID  `json:"id"`
		Title      string     `json:"title"`
		AssigneeID *uuid.UUID `json:"assignee_id"`
		DueOn      *string    `json:"due_on"`
		Completed  bool       `json:"completed"`
	}
	s.expect(http.StatusCreated, "POST", hp+"/chores", parent.AccessToken, map[string]any{
		"title": "Take out trash", "assignee_id": parent.User.ID, "due_on": "2026-11-01",
	}, &chore)
	if chore.DueOn == nil || *chore.DueOn != "2026-11-01" || *chore.AssigneeID != parent.User.ID {
		t.Errorf("created chore: %+v", chore)
	}
	cp := hp + "/chores/" + chore.ID.String()

	var mine struct {
		Items      []map[string]any `json:"items"`
		NextOffset *int             `json:"next_offset"`
	}
	s.expect(http.StatusOK, "GET", hp+"/chores?assignee=me", parent.AccessToken, nil, &mine)
	if len(mine.Items) != 1 || mine.NextOffset != nil {
		t.Errorf("parent's chores: %+v", mine)
	}

	s.expect(http.StatusOK, "PUT", cp+"/completion", parent.AccessToken, nil, &chore)
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

	s.expect(http.StatusNoContent, "DELETE", cp, parent.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", cp, parent.AccessToken, nil, nil)

	// Outsiders see nothing.
	outsider := s.signUp("outsider")
	s.expect(http.StatusNotFound, "GET", hp, outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", hp+"/calendar", outsider.AccessToken, nil, nil)
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

	t.Run("deletes the user's household", func(t *testing.T) {
		u := s.signUpPasswordless("deladmin")
		var h struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
			map[string]string{"name": "Google Home", "timezone": "UTC"}, &h)
		memberID, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		if _, err := s.pool.Exec(context.Background(),
			"INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, $3)", memberID, h.ID, "Kid"); err != nil {
			t.Fatalf("insert family member: %v", err)
		}

		s.expect(http.StatusNoContent, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": u.Email}, nil)

		if s.userExists(u.ID) {
			t.Error("user still exists after delete")
		}

		var householdCount int
		if err := s.pool.QueryRow(context.Background(),
			"SELECT COUNT(*) FROM households WHERE id = $1", h.ID).Scan(&householdCount); err != nil {
			t.Fatalf("count households: %v", err)
		}
		if householdCount != 0 {
			t.Errorf("household still exists: count = %d, want 0", householdCount)
		}

		var memberCount int
		if err := s.pool.QueryRow(context.Background(),
			"SELECT COUNT(*) FROM family_members WHERE household_id = $1", h.ID).Scan(&memberCount); err != nil {
			t.Fatalf("count family members: %v", err)
		}
		if memberCount != 0 {
			t.Errorf("family members still exist: count = %d, want 0", memberCount)
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

func TestAPI_FamilyMembersAndAdminPin(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("famadmin")

	// Fresh auto household
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "Admin Household", "timezone": "America/New_York"}, &h)

	// CLM-6: GET family-members returns {"items":[]} (non-nil empty array)
	var listResp struct {
		Items []struct {
			ID     uuid.UUID `json:"id"`
			Name   string    `json:"name"`
			HasPIN bool      `json:"has_pin"`
			Points int32     `json:"points"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken, nil, &listResp)
	if listResp.Items == nil || len(listResp.Items) != 0 {
		t.Fatalf("expected empty non-nil slice, got %+v", listResp.Items)
	}

	// GET admin-pin status: initially false
	var adminPinStatus struct {
		Set bool `json:"set"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken, nil, &adminPinStatus)
	if adminPinStatus.Set {
		t.Errorf("admin-pin status initially set = true, want false")
	}

	// Create member without PIN
	var m1 struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		HasPIN bool      `json:"has_pin"`
		Points int32     `json:"points"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "Sam"}, &m1)
	if m1.Name != "Sam" || m1.HasPIN || m1.Points != 0 {
		t.Errorf("created m1: got %+v", m1)
	}

	// Create member with PIN
	var m2 struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		HasPIN bool      `json:"has_pin"`
		Points int32     `json:"points"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "Alex", "pin": "1234"}, &m2)
	if m2.Name != "Alex" || !m2.HasPIN || m2.Points != 0 {
		t.Errorf("created m2: got %+v", m2)
	}

	// List shows both
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken, nil, &listResp)
	if len(listResp.Items) != 2 {
		t.Fatalf("list count: got %d, want 2", len(listResp.Items))
	}

	// Rename member
	var m1Renamed struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	s.expect(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+m1.ID.String(), u.AccessToken,
		map[string]string{"name": "Samantha"}, &m1Renamed)
	if m1Renamed.Name != "Samantha" {
		t.Errorf("renamed m1: got %q, want Samantha", m1Renamed.Name)
	}

	// CLM-7: Verify route
	// Verify m2 with correct PIN returns 200 with member JSON
	var vResp struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		HasPIN bool      `json:"has_pin"`
		Points int32     `json:"points"`
	}
	req, _ := http.NewRequest("POST", s.srv.URL+"/api/v1/households/"+h.ID.String()+"/family-members/"+m2.ID.String()+"/verify", strings.NewReader(`{"pin":"1234"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+u.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("verify request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify correct pin status %d, want 200", resp.StatusCode)
	}
	if cookie := resp.Header.Get("Set-Cookie"); cookie != "" {
		t.Errorf("verify response set cookie: %q", cookie)
	}
	if err := json.NewDecoder(resp.Body).Decode(&vResp); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if vResp.ID != m2.ID || vResp.Name != "Alex" || !vResp.HasPIN {
		t.Errorf("verify resp: got %+v", vResp)
	}

	// Verify m1 (no PIN) with empty PIN succeeds
	s.expect(http.StatusOK, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m1.ID.String()+"/verify", u.AccessToken,
		map[string]string{"pin": ""}, &vResp)
	if vResp.ID != m1.ID || vResp.Name != "Samantha" || vResp.HasPIN {
		t.Errorf("verify m1 resp: got %+v", vResp)
	}

	// Verify m2 with wrong PIN gives 403 wrong_pin
	var errResp apiError
	s.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m2.ID.String()+"/verify", u.AccessToken,
		map[string]string{"pin": "9999"}, &errResp)
	if errResp.Error.Code != "wrong_pin" {
		t.Errorf("wrong pin code: got %q, want wrong_pin", errResp.Error.Code)
	}

	// Session token still works afterwards
	s.expect(http.StatusOK, "GET", "/api/v1/me", u.AccessToken, nil, nil)

	// Set invalid PINs: 123, 12345, abcd -> 422 field pin
	for _, badPin := range []string{"123", "12345", "abcd"} {
		var vErr apiError
		s.expect(http.StatusUnprocessableEntity, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+m1.ID.String()+"/pin", u.AccessToken,
			map[string]string{"pin": badPin}, &vErr)
		if vErr.Error.Fields["pin"] == "" {
			t.Errorf("bad pin %q: expected validation error on pin, got %+v", badPin, vErr)
		}
	}

	// Check response JSON bodies never leak "pin" or "pin_hash"
	rawListResp, _ := http.NewRequest("GET", s.srv.URL+"/api/v1/households/"+h.ID.String()+"/family-members", nil)
	rawListResp.Header.Set("Authorization", "Bearer "+u.AccessToken)
	rResp, err := http.DefaultClient.Do(rawListResp)
	if err != nil {
		t.Fatalf("get list: %v", err)
	}
	defer rResp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(rResp.Body)
	rawBody := buf.String()
	if strings.Contains(rawBody, "pin_hash") || strings.Contains(rawBody, "1234") {
		t.Errorf("family members response leaked pin/pin_hash: %s", rawBody)
	}

	// Direct database check: stored hashes are bcrypt and != raw PIN
	var storedMemberHash, storedAdminHash string
	if err := s.pool.QueryRow(context.Background(), "SELECT pin_hash FROM family_members WHERE id = $1", m2.ID).Scan(&storedMemberHash); err != nil {
		t.Fatalf("query member pin hash: %v", err)
	}
	if storedMemberHash == "1234" || !strings.HasPrefix(storedMemberHash, "$2a$") {
		t.Errorf("stored member hash is not bcrypt: %q", storedMemberHash)
	}

	// CLM-10: Admin PIN lifecycle and management write gates
	// Set admin PIN "4321"
	s.expect(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken,
		map[string]string{"pin": "4321"}, nil)

	// Admin pin status is now true
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken, nil, &adminPinStatus)
	if !adminPinStatus.Set {
		t.Errorf("admin-pin status want true, got false")
	}

	// Verify admin PIN
	s.expect(http.StatusOK, "POST", "/api/v1/households/"+h.ID.String()+"/admin-pin/verify", u.AccessToken,
		map[string]string{"pin": "4321"}, nil)
	s.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/admin-pin/verify", u.AccessToken,
		map[string]string{"pin": "0000"}, nil)

	// Check stored admin PIN hash
	if err := s.pool.QueryRow(context.Background(), "SELECT pin_hash FROM household_admin_pins WHERE household_id = $1", h.ID).Scan(&storedAdminHash); err != nil {
		t.Fatalf("query admin pin hash: %v", err)
	}
	if storedAdminHash == "4321" || !strings.HasPrefix(storedAdminHash, "$2a$") {
		t.Errorf("stored admin pin hash is not bcrypt: %q", storedAdminHash)
	}

	// With admin PIN set:
	// Create without header -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "GatedKid"}, &errResp)
	if errResp.Error.Code != "admin_pin_required" {
		t.Errorf("create without header: got %q, want admin_pin_required", errResp.Error.Code)
	}

	// Create with wrong header -> 403 wrong_pin
	s.expectWithHeaders(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"X-Admin-PIN": "9999"}, map[string]string{"name": "GatedKid"}, &errResp)
	if errResp.Error.Code != "wrong_pin" {
		t.Errorf("create with wrong header: got %q, want wrong_pin", errResp.Error.Code)
	}

	// Create with right header -> 201 Created
	var gatedKid struct{ ID uuid.UUID }
	s.expectWithHeaders(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, map[string]string{"name": "GatedKid"}, &gatedKid)

	// Rename without header -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken,
		map[string]string{"name": "RenamedGated"}, &errResp)

	// Rename with wrong header -> 403 wrong_pin
	s.expectWithHeaders(http.StatusForbidden, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken,
		map[string]string{"X-Admin-PIN": "0000"}, map[string]string{"name": "RenamedGated"}, &errResp)

	// Rename with right header -> 200 OK
	s.expectWithHeaders(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, map[string]string{"name": "RenamedGated"}, nil)

	// Set member PIN without header -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String()+"/pin", u.AccessToken,
		map[string]string{"pin": "5555"}, &errResp)

	// Set member PIN with right header -> 200 OK
	s.expectWithHeaders(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String()+"/pin", u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, map[string]string{"pin": "5555"}, nil)

	// Clear admin PIN with right current admin PIN -> 204
	s.expectWithHeaders(http.StatusNoContent, "DELETE", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, nil, nil)

	// Admin pin status is false again
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken, nil, &adminPinStatus)
	if adminPinStatus.Set {
		t.Errorf("admin-pin status want false after clear, got true")
	}

	// Clearing reopened gate: Delete family member without X-Admin-PIN header succeeds
	s.expect(http.StatusNoContent, "DELETE", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken, nil, nil)
}

func TestAPI_PINRateLimit(t *testing.T) {
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

	// Build router with small PINRateLimit = 3
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		ChoreSvc:     app.NewChoreService(q),
		FamilySvc:    app.NewFamilyService(q, 10),
		JWTSecret:    secret,
		PINRateLimit: 3,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ts := &testServer{t: t, srv: srv, mail: mail, pool: pool}

	u := ts.signUp("rluser")
	var h struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "RL Home", "timezone": "UTC"}, &h)

	var m struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "Kid", "pin": "1234"}, &m)

	// Make 3 wrong guesses
	for i := 0; i < 3; i++ {
		ts.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m.ID.String()+"/verify", u.AccessToken,
			map[string]string{"pin": "0000"}, nil)
	}

	// 4th attempt returns 429 Too Many Requests, even with correct PIN
	var rlErr apiError
	ts.expect(http.StatusTooManyRequests, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m.ID.String()+"/verify", u.AccessToken,
		map[string]string{"pin": "1234"}, &rlErr)
	if rlErr.Error.Code != "rate_limited" {
		t.Errorf("expected rate_limited, got %q", rlErr.Error.Code)
	}
}

func TestAPI_RemovedInviteAndMemberRoutes404(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("removedroutes")

	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "Home", "timezone": "UTC"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	var e apiError
	// POST /households/{id}/invites -> 404
	s.expect(http.StatusNotFound, "POST", hp+"/invites", u.AccessToken, nil, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("POST /invites: code %q, want not_found", e.Error.Code)
	}

	// POST /invites/accept -> 404
	s.expect(http.StatusNotFound, "POST", "/api/v1/invites/accept", u.AccessToken, map[string]string{"code": "ABCDEFGH"}, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("POST /invites/accept: code %q, want not_found", e.Error.Code)
	}

	// GET /households/{id}/members -> 404
	s.expect(http.StatusNotFound, "GET", hp+"/members", u.AccessToken, nil, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("GET /members: code %q, want not_found", e.Error.Code)
	}

	// PUT /households/{id}/members/{uid} -> 404
	s.expect(http.StatusNotFound, "PUT", hp+"/members/"+u.User.ID.String(), u.AccessToken, map[string]string{"role": "member"}, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("PUT /members/{uid}: code %q, want not_found", e.Error.Code)
	}

	// DELETE /households/{id}/members/{uid} -> 404
	s.expect(http.StatusNotFound, "DELETE", hp+"/members/"+u.User.ID.String(), u.AccessToken, nil, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("DELETE /members/{uid}: code %q, want not_found", e.Error.Code)
	}
}
