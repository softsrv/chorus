package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/middleware"
	"github.com/softsrv/chorus/internal/http/respond"
)

const authTestSecret = "auth-test-secret-that-is-at-least-32-bytes!!"

// stubFetcher implements middleware.UserFetcher. It returns user only for the
// session it was configured with, mimicking the real session-liveness query.
type stubFetcher struct {
	user      db.User
	sessionID uuid.UUID
}

func (s *stubFetcher) GetUserForSession(_ context.Context, arg db.GetUserForSessionParams) (db.User, error) {
	if arg.SessionID != s.sessionID || arg.UserID != s.user.ID {
		return db.User{}, errors.New("no rows")
	}
	return s.user, nil
}

func makeTestToken(t *testing.T, userID, sessionID uuid.UUID, expiry time.Duration) string {
	t.Helper()
	tp, err := auth.IssueAccessToken(userID, sessionID, authTestSecret, expiry)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	return tp.AccessToken
}

func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body respond.ErrorBody
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code
}

func TestAuthMiddleware(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()
	validUser := db.User{ID: userID, Email: "test@example.com", EmailVerified: true}

	protected := middleware.Authenticate(&stubFetcher{user: validUser, sessionID: sessionID}, authTestSecret)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := middleware.UserFromContext(r.Context()); !ok {
				t.Error("user not found in context")
			}
			if sid, ok := middleware.SessionIDFromContext(r.Context()); !ok || sid != sessionID {
				t.Errorf("session id in context: got %v, want %v", sid, sessionID)
			}
			w.WriteHeader(http.StatusOK)
		}),
	)

	cases := []struct {
		name     string
		header   func() string
		cookie   bool
		wantCode int
		wantErr  string
	}{
		{
			name:     "valid bearer passes through",
			header:   func() string { return "Bearer " + makeTestToken(t, userID, sessionID, 15*time.Minute) },
			wantCode: http.StatusOK,
		},
		{
			name:     "scheme is case-insensitive",
			header:   func() string { return "bearer " + makeTestToken(t, userID, sessionID, 15*time.Minute) },
			wantCode: http.StatusOK,
		},
		{
			name:     "missing token",
			header:   func() string { return "" },
			wantCode: http.StatusUnauthorized,
			wantErr:  "unauthenticated",
		},
		{
			name:     "cookies are ignored",
			header:   func() string { return "" },
			cookie:   true,
			wantCode: http.StatusUnauthorized,
			wantErr:  "unauthenticated",
		},
		{
			name:     "expired token",
			header:   func() string { return "Bearer " + makeTestToken(t, userID, sessionID, -time.Second) },
			wantCode: http.StatusUnauthorized,
			wantErr:  "token_expired",
		},
		{
			name:     "revoked session",
			header:   func() string { return "Bearer " + makeTestToken(t, userID, uuid.New(), 15*time.Minute) },
			wantCode: http.StatusUnauthorized,
			wantErr:  "unauthenticated",
		},
		{
			name: "wrong signing secret",
			header: func() string {
				tp, _ := auth.IssueAccessToken(userID, sessionID, "some-other-secret-that-is-32-bytes-long", time.Minute)
				return "Bearer " + tp.AccessToken
			},
			wantCode: http.StatusUnauthorized,
			wantErr:  "unauthenticated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if h := tc.header(); h != "" {
				req.Header.Set("Authorization", h)
			}
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: "access_token", Value: makeTestToken(t, userID, sessionID, time.Minute)})
			}
			rr := httptest.NewRecorder()
			protected.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("status: got %d, want %d", rr.Code, tc.wantCode)
			}
			if tc.wantErr != "" {
				if got := errorCode(t, rr); got != tc.wantErr {
					t.Errorf("error code: got %q, want %q", got, tc.wantErr)
				}
				if rr.Header().Get("WWW-Authenticate") == "" {
					t.Error("expected WWW-Authenticate header on 401")
				}
			}
		})
	}
}

func TestRequireEmailVerified(t *testing.T) {
	t.Parallel()

	userID, sessionID := uuid.New(), uuid.New()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for _, verified := range []bool{true, false} {
		fetcher := &stubFetcher{user: db.User{ID: userID, EmailVerified: verified}, sessionID: sessionID}
		h := middleware.Authenticate(fetcher, authTestSecret)(middleware.RequireEmailVerified(ok))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+makeTestToken(t, userID, sessionID, time.Minute))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		want := http.StatusOK
		if !verified {
			want = http.StatusForbidden
		}
		if rr.Code != want {
			t.Errorf("verified=%v: got %d, want %d", verified, rr.Code, want)
		}
	}
}
