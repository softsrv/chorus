package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

// googleSignInServicer is the subset of app.GoogleSignIn the handler needs.
type googleSignInServicer interface {
	SignIn(ctx context.Context, idToken string, meta app.DeviceMeta) (app.TokenResult, error)
	WebClientID() string
}

// GoogleHandler serves the public auth config and Google sign-in.
// A nil service means Google sign-in is not configured.
type GoogleHandler struct {
	svc             googleSignInServicer
	calendarEnabled bool
	meta            *AuthHandler // reuses deviceMeta so sessions are labelled like Login's
}

// NewGoogleHandler constructs a GoogleHandler. svc may be nil (disabled);
// calendarEnabled reports whether the Google Calendar service is configured.
func NewGoogleHandler(svc *app.GoogleSignIn, calendarEnabled bool, trustedProxyCount int) *GoogleHandler {
	h := &GoogleHandler{calendarEnabled: calendarEnabled, meta: &AuthHandler{trustedProxyCount: trustedProxyCount}}
	if svc != nil {
		h.svc = svc
	}
	return h
}

// Enabled reports whether Google sign-in is configured.
func (h *GoogleHandler) Enabled() bool { return h.svc != nil }

// authConfigJSON is the public GET /auth/config body. It carries only the web
// client ID, never the secret or the iOS and Android client IDs.
type authConfigJSON struct {
	GoogleSignIn   bool   `json:"google_sign_in"`
	GoogleCalendar bool   `json:"google_calendar"`
	GoogleClientID string `json:"google_client_id"`
}

// AuthConfig tells clients which optional sign-in features are available and
// the web client ID Google's sign-in needs. Public: the sign-in page needs it
// before anyone is signed in.
func (h *GoogleHandler) AuthConfig(w http.ResponseWriter, r *http.Request) {
	body := authConfigJSON{GoogleSignIn: h.Enabled(), GoogleCalendar: h.Enabled() && h.calendarEnabled}
	if h.Enabled() {
		body.GoogleClientID = h.svc.WebClientID()
	}
	respond.JSON(w, http.StatusOK, body)
}

// GoogleSignIn exchanges a Google ID token from any configured app (web, iOS,
// Android) for the same token JSON as login. Every refusal of the token or
// the identity gets one generic error; the cause is only logged.
func (h *GoogleHandler) GoogleSignIn(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	var req struct {
		IDToken    string `json:"id_token"`
		DeviceName string `json:"device_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	idToken := strings.TrimSpace(req.IDToken)
	if idToken == "" {
		respond.ValidationError(w, map[string]string{"id_token": "is required"})
		return
	}
	result, err := h.svc.SignIn(r.Context(), idToken, h.meta.deviceMeta(r, req.DeviceName))
	if err != nil {
		if errors.Is(err, app.ErrGoogleSignInFailed) {
			respond.Error(w, http.StatusUnauthorized, "google_sign_in_failed", "Google sign-in could not be completed. Try again.")
			return
		}
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toTokens(result))
}

func notFound(w http.ResponseWriter) {
	respond.Error(w, http.StatusNotFound, "not_found", "No such endpoint.")
}
