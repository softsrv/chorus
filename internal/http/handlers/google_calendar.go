package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

// The Account page the calendar callback lands on, with only a fixed outcome.
const (
	calendarReturnConnected = "/account?google_calendar=connected"
	calendarReturnFailed    = "/account?google_calendar=failed"
)

// calendarStatusUnavailable is reported when the deployment has no Google
// Calendar configured.
const calendarStatusUnavailable = "unavailable"

// googleCalendarServicer is the subset of app.GoogleCalendar the handler needs.
type googleCalendarServicer interface {
	Status(ctx context.Context, userID uuid.UUID) (app.CalendarStatus, error)
	Connect(ctx context.Context, userID uuid.UUID) (string, string, error)
	Callback(ctx context.Context, state, code, errParam, nonce string) error
	Disconnect(ctx context.Context, userID uuid.UUID) error
}

// calendarBindingCookie holds the browser nonce that ties a calendar connect
// to the browser that started it. The API is otherwise header-only; this
// cookie is scoped to the callback path and read nowhere else.
const calendarBindingCookie = "chorus_gcal_bind"

// calendarCallbackPath is the default callback path (the cookie's scope) when
// no redirect URL is configured.
const calendarCallbackPath = "/api/v1/me/google-calendar/callback"

// GoogleCalendarHandler serves /api/v1/me/google-calendar. A nil service means
// Google Calendar is not configured on this deployment.
type GoogleCalendarHandler struct {
	svc          googleCalendarServicer
	cookiePath   string
	cookieSecure bool
}

// NewGoogleCalendarHandler constructs a GoogleCalendarHandler. svc may be nil.
// The binding cookie is scoped to the configured callback path and marked
// Secure when that callback is https.
func NewGoogleCalendarHandler(svc *app.GoogleCalendar) *GoogleCalendarHandler {
	h := &GoogleCalendarHandler{cookiePath: calendarCallbackPath}
	if svc != nil {
		h.svc = svc
		if u, err := url.Parse(svc.RedirectURL()); err == nil && u.Path != "" {
			h.cookiePath = u.Path
			h.cookieSecure = u.Scheme == "https"
		}
	}
	return h
}

// setBindingCookie hands the browser nonce to the browser that called
// connect: HttpOnly, SameSite=Lax (sent on Google's top-level redirect back),
// Secure when the callback is https, and only for the callback path.
func (h *GoogleCalendarHandler) setBindingCookie(w http.ResponseWriter, nonce string) {
	http.SetCookie(w, &http.Cookie{
		Name:     calendarBindingCookie,
		Value:    nonce,
		Path:     h.cookiePath,
		MaxAge:   int(app.CalendarStateTTL / time.Second),
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearBindingCookie removes the binding cookie; every callback does this.
func (h *GoogleCalendarHandler) clearBindingCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     calendarBindingCookie,
		Value:    "",
		Path:     h.cookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Enabled reports whether Google Calendar is configured.
func (h *GoogleCalendarHandler) Enabled() bool { return h.svc != nil }

// calendarStatusResponse carries no token or Google detail, only the state.
type calendarStatusResponse struct {
	Status string `json:"status"`
	Access string `json:"access,omitempty"`
}

// Status reports the signed-in user's connection.
func (h *GoogleCalendarHandler) Status(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		respond.JSON(w, http.StatusOK, calendarStatusResponse{Status: calendarStatusUnavailable})
		return
	}
	st, err := h.svc.Status(r.Context(), currentUser(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := calendarStatusResponse{Status: st.Status}
	if st.Status == app.CalendarConnected || st.Status == app.CalendarBroken {
		resp.Access = st.Access
	}
	respond.JSON(w, http.StatusOK, resp)
}

// Connect returns the Google consent URL the client should navigate to.
func (h *GoogleCalendarHandler) Connect(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	target, nonce, err := h.svc.Connect(r.Context(), currentUser(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	h.setBindingCookie(w, nonce)
	respond.JSON(w, http.StatusOK, map[string]string{"url": target})
}

// Disconnect removes the signed-in user's connection. Google revoking the
// token is best-effort, so this succeeds whether or not Google answered.
func (h *GoogleCalendarHandler) Disconnect(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	if err := h.svc.Disconnect(r.Context(), currentUser(r)); err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, calendarStatusResponse{Status: app.CalendarNotConnected})
}

// Callback is where Google sends the browser back. It carries no bearer
// token: the state names the user, and the binding cookie set by connect
// proves this is the browser that started it. A missing or mismatched cookie
// fails before any code exchange. It always clears the cookie and redirects
// to the Account page with only a fixed outcome; no internal or Google error
// text is passed on.
func (h *GoogleCalendarHandler) Callback(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	var nonce string
	if c, err := r.Cookie(calendarBindingCookie); err == nil {
		nonce = c.Value
	}
	h.clearBindingCookie(w)
	q := r.URL.Query()
	err := h.svc.Callback(r.Context(), q.Get("state"), q.Get("code"), q.Get("error"), nonce)
	switch {
	case err == nil:
		http.Redirect(w, r, calendarReturnConnected, http.StatusSeeOther)
	case errors.Is(err, app.ErrGoogleCalendarCancelled), errors.Is(err, app.ErrGoogleCalendarFailed):
		http.Redirect(w, r, calendarReturnFailed, http.StatusSeeOther)
	default:
		slog.ErrorContext(r.Context(), "google calendar callback", "error", err)
		http.Redirect(w, r, calendarReturnFailed, http.StatusSeeOther)
	}
}
