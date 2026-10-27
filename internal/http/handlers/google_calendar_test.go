package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
)

type fakeCalendarSvc struct {
	nonce     string
	gotNonce  string
	callbacks int
}

func (f *fakeCalendarSvc) Status(context.Context, uuid.UUID) (app.CalendarStatus, error) {
	return app.CalendarStatus{}, nil
}
func (f *fakeCalendarSvc) Connect(context.Context, uuid.UUID) (string, string, error) {
	return "https://accounts.google.com/o/oauth2/auth?state=s", f.nonce, nil
}
func (f *fakeCalendarSvc) Callback(_ context.Context, _, _, _, nonce string) error {
	f.callbacks++
	f.gotNonce = nonce
	if nonce != f.nonce {
		return app.ErrGoogleCalendarFailed
	}
	return nil
}
func (f *fakeCalendarSvc) Disconnect(context.Context, uuid.UUID) error { return nil }

func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// The binding cookie connect sets is HttpOnly, SameSite=Lax, scoped to the
// callback path, Secure for an https callback; the callback passes the
// cookie's value to the service and always clears it.
func TestGoogleCalendarHandler_BindingCookie(t *testing.T) {
	svc := &fakeCalendarSvc{nonce: "nonce-1"}
	h := &GoogleCalendarHandler{svc: svc, cookiePath: "/api/v1/me/google-calendar/callback", cookieSecure: true}

	rec := httptest.NewRecorder()
	h.setBindingCookie(rec, "nonce-1")
	c := findCookie(rec.Result(), calendarBindingCookie)
	if c == nil {
		t.Fatal("no binding cookie set")
	}
	if c.Value != "nonce-1" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode ||
		c.Path != "/api/v1/me/google-calendar/callback" || c.MaxAge <= 0 || c.MaxAge > 600 {
		t.Errorf("cookie = %+v", c)
	}

	for _, tc := range []struct {
		name   string
		cookie string
		want   string
	}{
		{"matching cookie", "nonce-1", calendarReturnConnected},
		{"no cookie", "", calendarReturnFailed},
		{"other browser's cookie", "nonce-2", calendarReturnFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/me/google-calendar/callback?state=s&code=c", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: calendarBindingCookie, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			h.Callback(rec, req)
			if loc := rec.Header().Get("Location"); loc != tc.want {
				t.Errorf("Location %q, want %q", loc, tc.want)
			}
			if svc.gotNonce != tc.cookie {
				t.Errorf("service got nonce %q, want %q", svc.gotNonce, tc.cookie)
			}
			cleared := findCookie(rec.Result(), calendarBindingCookie)
			if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" {
				t.Errorf("callback did not clear the cookie: %+v", cleared)
			}
		})
	}
}
