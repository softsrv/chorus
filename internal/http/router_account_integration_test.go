//go:build integration

package http_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/db"
)

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
