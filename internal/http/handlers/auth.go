package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/mssola/useragent"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/middleware"
	"github.com/softsrv/chorus/internal/http/respond"
)

// authServicer defines the subset of app.AuthService that AuthHandler requires.
// Accepting an interface (rather than the concrete type) makes the handler
// independently testable without a real database or SMTP server.
type authServicer interface {
	Register(ctx context.Context, email, password, displayName string) error
	Login(ctx context.Context, email, password string, meta app.DeviceMeta) (app.TokenResult, error)
	Logout(ctx context.Context, sessionID uuid.UUID) error
	Refresh(ctx context.Context, rawRefreshToken string, meta app.DeviceMeta) (app.TokenResult, error)
	RequestPasswordReset(ctx context.Context, rawEmail string) error
	CompletePasswordReset(ctx context.Context, rawToken, newPassword string) error
	VerifyEmail(ctx context.Context, rawToken string, meta app.DeviceMeta) (app.TokenResult, error)
}

// AuthHandler serves the /api/v1/auth endpoints.
type AuthHandler struct {
	auth              authServicer
	trustedProxyCount int // number of trusted reverse-proxy hops for client-IP extraction
}

// NewAuthHandler constructs an AuthHandler.
func NewAuthHandler(authSvc authServicer, trustedProxyCount int) *AuthHandler {
	return &AuthHandler{auth: authSvc, trustedProxyCount: trustedProxyCount}
}

// Register creates an account. The response is the same whether or not the
// email was already registered (see app.AuthService.Register).
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.auth.Register(r.Context(), req.Email, req.Password, req.DisplayName); err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, map[string]string{
		"message": "Check your email to verify your address, then sign in.",
	})
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.auth.Login(r.Context(), req.Email, req.Password, h.deviceMeta(r, req.DeviceName))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toTokens(result))
}

// Logout revokes the session behind the caller's access token. Both the access
// token and refresh token stop working immediately.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := middleware.SessionIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthenticated", "Not signed in.")
		return
	}
	if err := h.auth.Logout(r.Context(), sessionID); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
		DeviceName   string `json:"device_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.auth.Refresh(r.Context(), req.RefreshToken, h.deviceMeta(r, req.DeviceName))
	if err != nil {
		slog.InfoContext(r.Context(), "token refresh failed", "error", err)
		respond.Error(w, http.StatusUnauthorized, "invalid_refresh_token", "Refresh token is invalid or expired. Sign in again.")
		return
	}
	respond.JSON(w, http.StatusOK, toTokens(result))
}

func (h *AuthHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token      string `json:"token"`
		DeviceName string `json:"device_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.auth.VerifyEmail(r.Context(), req.Token, h.deviceMeta(r, req.DeviceName))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toTokens(result))
}

// ForgotPassword always responds 202 so it can't be used to discover which
// emails are registered.
func (h *AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// Error is intentionally not returned to the caller (prevents email enumeration),
	// but we log it so server-side failures don't go unnoticed.
	if err := h.auth.RequestPasswordReset(r.Context(), req.Email); err != nil {
		slog.ErrorContext(r.Context(), "request password reset", "error", err)
	}
	respond.JSON(w, http.StatusAccepted, map[string]string{
		"message": "If that email is registered, a reset link has been sent.",
	})
}

func (h *AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.auth.CompletePasswordReset(r.Context(), req.Token, req.Password); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// deviceMeta extracts client IP and device metadata from the request. Native
// clients should send a device_name (e.g. "Sam's iPhone"); otherwise one is
// derived from the User-Agent.
//
// Client-IP extraction goes through middleware.ClientIP, which only trusts
// X-Forwarded-For when the service is configured behind trusted proxies — so
// the stored IP can't be spoofed in a direct-to-internet deployment.
func (h *AuthHandler) deviceMeta(r *http.Request, clientDeviceName string) app.DeviceMeta {
	var addr *netip.Addr
	if parsed, err := netip.ParseAddr(middleware.ClientIP(r, h.trustedProxyCount)); err == nil {
		addr = &parsed
	}

	deviceName := sanitizeDeviceName(clientDeviceName)
	if deviceName == "" {
		ua := useragent.New(r.UserAgent())
		browser, _ := ua.Browser()
		deviceName = browser
		if os := ua.OS(); os != "" && browser != "" {
			deviceName = browser + " on " + os
		}
	}

	userAgent := r.UserAgent()
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}

	return app.DeviceMeta{DeviceName: deviceName, IPAddress: addr, UserAgent: userAgent}
}

// sanitizeDeviceName strips control characters and caps the length of a
// client-supplied device label. It's display-only, so it's cleaned rather
// than rejected.
func sanitizeDeviceName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if runes := []rune(s); len(runes) > 100 {
		s = string(runes[:100])
	}
	return s
}
