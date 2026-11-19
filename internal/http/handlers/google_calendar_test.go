package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/middleware"
)

type mockCalendarSvc struct {
	status        app.CalendarStatus
	statusErr     error
	connectRes    app.CalendarStatus
	connectErr    error
	gotCode       string
	gotPlatform   string
	disconnectErr error
}

func (m *mockCalendarSvc) Status(ctx context.Context, userID uuid.UUID) (app.CalendarStatus, error) {
	return m.status, m.statusErr
}

func (m *mockCalendarSvc) Connect(ctx context.Context, userID uuid.UUID, code, platform string) (app.CalendarStatus, error) {
	m.gotCode = code
	m.gotPlatform = platform
	return m.connectRes, m.connectErr
}

func (m *mockCalendarSvc) Disconnect(ctx context.Context, userID uuid.UUID) error {
	return m.disconnectErr
}

func authedRequest(method, target string, body any, uid uuid.UUID) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	sessionID := uuid.New()
	tp, _ := auth.IssueAccessToken(uid, sessionID, authTestSecret, time.Hour)
	req.Header.Set("Authorization", "Bearer "+tp.AccessToken)

	fetcher := &stubFetcher{
		user:      db.User{ID: uid, EmailVerified: true},
		sessionID: sessionID,
	}
	var authedReq *http.Request
	middleware.Authenticate(fetcher, authTestSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authedReq = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	return authedReq
}

// CLM-6: A static check test that fails if 'chorus_gcal_bind' or 'SetCookie'
// appears in any non-test source file under internal/http/handlers.
func TestGoogleCalendarHandler_NoCookieGrep(t *testing.T) {
	candidates := []string{".", "internal/http/handlers", "../handlers"}
	var dir string
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			if _, err := os.Stat(filepath.Join(c, "google_calendar.go")); err == nil {
				dir = c
				break
			}
		}
	}
	if dir == "" {
		t.Fatal("could not find internal/http/handlers directory")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		content := string(data)
		if strings.Contains(content, "chorus_gcal_bind") {
			t.Errorf("file %s contains forbidden string 'chorus_gcal_bind'", path)
		}
		if strings.Contains(content, "SetCookie") {
			t.Errorf("file %s calls SetCookie", path)
		}
	}
}

func TestGoogleCalendarHandler_Status(t *testing.T) {
	uid := uuid.New()
	t.Run("unconfigured", func(t *testing.T) {
		h := &GoogleCalendarHandler{svc: nil}
		rec := httptest.NewRecorder()
		h.Status(rec, authedRequest("GET", "/api/v1/me/google-calendar", nil, uid))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp calendarStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != calendarStatusUnavailable {
			t.Errorf("status = %q, want %q", resp.Status, calendarStatusUnavailable)
		}
	})

	t.Run("connected", func(t *testing.T) {
		svc := &mockCalendarSvc{
			status: app.CalendarStatus{Status: app.CalendarConnected, Access: "read_write"},
		}
		h := &GoogleCalendarHandler{svc: svc}
		rec := httptest.NewRecorder()
		h.Status(rec, authedRequest("GET", "/api/v1/me/google-calendar", nil, uid))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp calendarStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != app.CalendarConnected || resp.Access != "read_write" {
			t.Errorf("got %+v, want connected read_write", resp)
		}
	})
}

func TestGoogleCalendarHandler_Connect(t *testing.T) {
	uid := uuid.New()
	t.Run("unconfigured", func(t *testing.T) {
		h := &GoogleCalendarHandler{svc: nil}
		rec := httptest.NewRecorder()
		h.Connect(rec, authedRequest("POST", "/api/v1/me/google-calendar/connect", map[string]string{"code": "auth-code"}, uid))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("empty code", func(t *testing.T) {
		svc := &mockCalendarSvc{}
		h := &GoogleCalendarHandler{svc: svc}
		rec := httptest.NewRecorder()
		h.Connect(rec, authedRequest("POST", "/api/v1/me/google-calendar/connect", map[string]string{"code": "   "}, uid))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("valid connect", func(t *testing.T) {
		svc := &mockCalendarSvc{
			connectRes: app.CalendarStatus{Status: app.CalendarConnected, Access: "read_only"},
		}
		h := &GoogleCalendarHandler{svc: svc}
		rec := httptest.NewRecorder()
		h.Connect(rec, authedRequest("POST", "/api/v1/me/google-calendar/connect", map[string]string{"code": "auth-code", "platform": "web"}, uid))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if svc.gotCode != "auth-code" || svc.gotPlatform != "web" {
			t.Errorf("svc got code=%q platform=%q, want auth-code web", svc.gotCode, svc.gotPlatform)
		}
		var resp calendarStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != app.CalendarConnected || resp.Access != "read_only" {
			t.Errorf("got %+v, want connected read_only", resp)
		}
	})
}

func TestGoogleCalendarHandler_Disconnect(t *testing.T) {
	uid := uuid.New()
	t.Run("unconfigured", func(t *testing.T) {
		h := &GoogleCalendarHandler{svc: nil}
		rec := httptest.NewRecorder()
		h.Disconnect(rec, authedRequest("DELETE", "/api/v1/me/google-calendar", nil, uid))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("connected", func(t *testing.T) {
		svc := &mockCalendarSvc{}
		h := &GoogleCalendarHandler{svc: svc}
		rec := httptest.NewRecorder()
		h.Disconnect(rec, authedRequest("DELETE", "/api/v1/me/google-calendar", nil, uid))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp calendarStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != app.CalendarNotConnected {
			t.Errorf("status = %q, want %q", resp.Status, app.CalendarNotConnected)
		}
	})
}
