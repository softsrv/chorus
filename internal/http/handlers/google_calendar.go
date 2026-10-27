package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

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
	Connect(ctx context.Context, userID uuid.UUID) (string, error)
	Callback(ctx context.Context, state, code, errParam string) error
	Disconnect(ctx context.Context, userID uuid.UUID) error
}

// GoogleCalendarHandler serves /api/v1/me/google-calendar. A nil service means
// Google Calendar is not configured on this deployment.
type GoogleCalendarHandler struct {
	svc googleCalendarServicer
}

// NewGoogleCalendarHandler constructs a GoogleCalendarHandler. svc may be nil.
func NewGoogleCalendarHandler(svc *app.GoogleCalendar) *GoogleCalendarHandler {
	h := &GoogleCalendarHandler{}
	if svc != nil {
		h.svc = svc
	}
	return h
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
	target, err := h.svc.Connect(r.Context(), currentUser(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
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

// Callback is where Google sends the browser back. It is public: the state
// carries the user. It always redirects to the Account page with only a fixed
// outcome; no internal or Google error text is passed on.
func (h *GoogleCalendarHandler) Callback(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	q := r.URL.Query()
	err := h.svc.Callback(r.Context(), q.Get("state"), q.Get("code"), q.Get("error"))
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
