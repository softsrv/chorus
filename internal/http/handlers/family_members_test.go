package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/middleware"
	"github.com/softsrv/chorus/internal/http/respond"
)

const authTestSecret = "auth-test-secret-that-is-at-least-32-bytes!!"

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

type fakePointsHistoryService struct {
	familyServicer
	called    bool
	gotLimit  int32
	gotOffset int32
}

func (f *fakePointsHistoryService) PointsHistory(ctx context.Context, userID, householdID, memberID uuid.UUID, memberPin, adminPin string, limit, offset int32) ([]app.PointsAward, error) {
	f.called = true
	f.gotLimit = limit
	f.gotOffset = offset
	return []app.PointsAward{}, nil
}

func TestPointsHistory_ClampsPaging(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		query      string
		wantLimit  int32
		wantOffset int32
	}{
		{
			name:       "out of int32 upper limit clamped to 100",
			query:      "?limit=4294967346",
			wantLimit:  100,
			wantOffset: 0,
		},
		{
			name:       "negative out of int32 offset clamped to 0",
			query:      "?offset=-4294967296",
			wantLimit:  50,
			wantOffset: 0,
		},
		{
			name:       "limit 0 gives 1",
			query:      "?limit=0",
			wantLimit:  1,
			wantOffset: 0,
		},
		{
			name:       "limit -5 gives 1",
			query:      "?limit=-5",
			wantLimit:  1,
			wantOffset: 0,
		},
		{
			name:       "limit 101 gives 100",
			query:      "?limit=101",
			wantLimit:  100,
			wantOffset: 0,
		},
		{
			name:       "offset -1 gives 0",
			query:      "?offset=-1",
			wantLimit:  50,
			wantOffset: 0,
		},
		{
			name:       "no params give default 50 and 0",
			query:      "",
			wantLimit:  50,
			wantOffset: 0,
		},
		{
			name:       "in range values pass through unchanged",
			query:      "?limit=7&offset=3",
			wantLimit:  7,
			wantOffset: 3,
		},
	}

	householdID := uuid.New()
	memberID := uuid.New()
	userID := uuid.New()
	sessionID := uuid.New()
	user := db.User{ID: userID, Email: "test@example.com", EmailVerified: true}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fakeSvc := &fakePointsHistoryService{}
			h := NewFamilyMemberHandler(fakeSvc)

			fetcher := &stubFetcher{user: user, sessionID: sessionID}
			authMw := middleware.Authenticate(fetcher, authTestSecret)

			mux := http.NewServeMux()
			mux.Handle("GET /api/v1/households/{householdID}/family-members/{memberID}/points-history", authMw(http.HandlerFunc(h.PointsHistory)))

			token := makeTestToken(t, userID, sessionID, time.Minute)

			url := fmt.Sprintf("/api/v1/households/%s/family-members/%s/points-history%s", householdID, memberID, tc.query)
			req := httptest.NewRequest(http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+token)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
			}
			if !fakeSvc.called {
				t.Fatalf("PointsHistory service method was not called")
			}
			if fakeSvc.gotLimit != tc.wantLimit {
				t.Errorf("got limit = %d, want %d", fakeSvc.gotLimit, tc.wantLimit)
			}
			if fakeSvc.gotOffset != tc.wantOffset {
				t.Errorf("got offset = %d, want %d", fakeSvc.gotOffset, tc.wantOffset)
			}
		})
	}
}

func TestPointsHistory_NonIntegerIs422(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		query     string
		wantField string
	}{
		{
			name:      "non-integer limit abc",
			query:     "?limit=abc",
			wantField: "limit",
		},
		{
			name:      "non-integer offset 1.5",
			query:     "?offset=1.5",
			wantField: "offset",
		},
	}

	householdID := uuid.New()
	memberID := uuid.New()
	userID := uuid.New()
	sessionID := uuid.New()
	user := db.User{ID: userID, Email: "test@example.com", EmailVerified: true}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fakeSvc := &fakePointsHistoryService{}
			h := NewFamilyMemberHandler(fakeSvc)

			fetcher := &stubFetcher{user: user, sessionID: sessionID}
			authMw := middleware.Authenticate(fetcher, authTestSecret)

			mux := http.NewServeMux()
			mux.Handle("GET /api/v1/households/{householdID}/family-members/{memberID}/points-history", authMw(http.HandlerFunc(h.PointsHistory)))

			token := makeTestToken(t, userID, sessionID, time.Minute)

			url := fmt.Sprintf("/api/v1/households/%s/family-members/%s/points-history%s", householdID, memberID, tc.query)
			req := httptest.NewRequest(http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+token)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
			}
			if fakeSvc.called {
				t.Fatalf("service was called for invalid query params")
			}

			var body respond.ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode response body: %v", err)
			}

			if body.Error.Fields == nil || body.Error.Fields[tc.wantField] == "" {
				t.Errorf("expected field %q in validation error fields: %+v", tc.wantField, body.Error.Fields)
			}
		})
	}
}
