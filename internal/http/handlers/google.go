package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

// googleSignInReturnPath is the web client page the callback lands on. It
// reads either ?code=<hand-off code> or ?error=cancelled|failed.
const googleSignInReturnPath = "/google-sign-in"

// googleSignInServicer is the subset of app.GoogleSignIn the handler needs.
type googleSignInServicer interface {
	Start(ctx context.Context) (string, error)
	Callback(ctx context.Context, state, code, errParam string) (string, error)
	Redeem(ctx context.Context, code string, meta app.DeviceMeta) (app.TokenResult, error)
}

// GoogleHandler serves the public auth config and the Google sign-in routes.
// A nil service means Google sign-in is not configured.
type GoogleHandler struct {
	svc  googleSignInServicer
	meta *AuthHandler // reuses deviceMeta so sessions are labelled like Login's
}

// NewGoogleHandler constructs a GoogleHandler. svc may be nil (disabled).
func NewGoogleHandler(svc *app.GoogleSignIn, trustedProxyCount int) *GoogleHandler {
	h := &GoogleHandler{meta: &AuthHandler{trustedProxyCount: trustedProxyCount}}
	if svc != nil {
		h.svc = svc
	}
	return h
}

// Enabled reports whether Google sign-in is configured.
func (h *GoogleHandler) Enabled() bool { return h.svc != nil }

// AuthConfig tells clients which optional sign-in features are available.
// Public: the sign-in page needs it before anyone is signed in.
func (h *GoogleHandler) AuthConfig(w http.ResponseWriter, r *http.Request) {
	respond.JSON(w, http.StatusOK, map[string]bool{
		"google_sign_in":  h.Enabled(),
		"google_calendar": false,
	})
}

// GoogleStart sends the browser to Google's consent screen.
func (h *GoogleHandler) GoogleStart(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	target, err := h.svc.Start(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "google sign-in start", "error", err)
		redirectToSPA(w, r, url.Values{"error": {"failed"}})
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// GoogleCallback is where Google sends the browser back. It always redirects
// to the web client: with a hand-off code on success, otherwise with only a
// generic reason. No internal or Google error text is ever passed on.
func (h *GoogleHandler) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	q := r.URL.Query()
	code, err := h.svc.Callback(r.Context(), q.Get("state"), q.Get("code"), q.Get("error"))
	switch {
	case err == nil:
		redirectToSPA(w, r, url.Values{"code": {code}})
	case errors.Is(err, app.ErrGoogleSignInCancelled):
		redirectToSPA(w, r, url.Values{"error": {"cancelled"}})
	default:
		redirectToSPA(w, r, url.Values{"error": {"failed"}})
	}
}

// GoogleRedeem exchanges a hand-off code for the same token JSON as login.
func (h *GoogleHandler) GoogleRedeem(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		notFound(w)
		return
	}
	var req struct {
		Code       string `json:"code"`
		DeviceName string `json:"device_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.svc.Redeem(r.Context(), req.Code, h.meta.deviceMeta(r, req.DeviceName))
	if err != nil {
		if errors.Is(err, app.ErrSignInCodeInvalid) {
			respond.Error(w, http.StatusBadRequest, "invalid_code", "Google sign-in could not be completed. Try again.")
			return
		}
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toTokens(result))
}

func redirectToSPA(w http.ResponseWriter, r *http.Request, v url.Values) {
	http.Redirect(w, r, googleSignInReturnPath+"?"+v.Encode(), http.StatusFound)
}

func notFound(w http.ResponseWriter) {
	respond.Error(w, http.StatusNotFound, "not_found", "No such endpoint.")
}
