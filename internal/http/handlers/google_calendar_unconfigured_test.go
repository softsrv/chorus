package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
)

// The router hands the constructor a nil *app.GoogleCalendar when Google
// Calendar is not configured (cmd/app/main.go). Built through the
// constructor, every route must answer unavailable / 404, never panic into a
// 500: a typed nil stored in the interface field would make Enabled true.
func TestNewGoogleCalendarHandler_NilServiceIsUnconfigured(t *testing.T) {
	var none *app.GoogleCalendar
	h := NewGoogleCalendarHandler(none)
	if h.Enabled() {
		t.Fatal("Enabled() = true for a nil service")
	}
	uid := uuid.New()

	rec := httptest.NewRecorder()
	h.Status(rec, authedRequest("GET", "/api/v1/me/google-calendar", nil, uid))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200", rec.Code)
	}
	var resp calendarStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != calendarStatusUnavailable {
		t.Errorf("status = %q, want %q", resp.Status, calendarStatusUnavailable)
	}

	rec = httptest.NewRecorder()
	h.Connect(rec, authedRequest("POST", "/api/v1/me/google-calendar/connect", map[string]string{"code": "fake-code"}, uid))
	if rec.Code != http.StatusNotFound {
		t.Errorf("connect: %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.Disconnect(rec, authedRequest("DELETE", "/api/v1/me/google-calendar", nil, uid))
	if rec.Code != http.StatusNotFound {
		t.Errorf("disconnect: %d, want 404", rec.Code)
	}
}
