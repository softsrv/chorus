package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

// calendarStatusUnavailable is reported when the deployment has no Google
// Calendar configured.
const calendarStatusUnavailable = "unavailable"

// googleCalendarServicer is the subset of app.GoogleCalendar the handler needs.
type googleCalendarServicer interface {
	Status(ctx context.Context, userID uuid.UUID) (app.CalendarStatus, error)
	Connect(ctx context.Context, userID uuid.UUID, code, platform string) (app.CalendarStatus, error)
	Disconnect(ctx context.Context, userID uuid.UUID) error
}

// GoogleCalendarHandler serves /api/v1/me/google-calendar. A nil service means
// Google Calendar is not configured on this deployment.
type GoogleCalendarHandler struct {
	svc googleCalendarServicer
}

// NewGoogleCalendarHandler constructs a GoogleCalendarHandler. svc may be nil.
func NewGoogleCalendarHandler(svc *app.GoogleCalendar) *GoogleCalendarHandler {
	return &GoogleCalendarHandler{svc: svc}
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

// Connect exchanges a one-time authorization code with Google and stores the
// connection.
func (h *GoogleCalendarHandler) Connect(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	var req struct {
		Code     string `json:"code"`
		Platform string `json:"platform"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeError(w, r, app.ErrGoogleCalendarFailed)
		return
	}
	st, err := h.svc.Connect(r.Context(), currentUser(r), code, strings.TrimSpace(req.Platform))
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

// Callback is no longer supported; it returns 404 until router.go drops the route.
func (h *GoogleCalendarHandler) Callback(w http.ResponseWriter, _ *http.Request) {
	notFound(w)
}
