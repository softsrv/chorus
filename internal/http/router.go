package http

import (
	"context"
	"net/http"
	"time"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/handlers"
	"github.com/softsrv/chorus/internal/http/middleware"
	"github.com/softsrv/chorus/internal/http/respond"
	"github.com/softsrv/chorus/web"
)

// RouterConfig holds all dependencies required to build the router.
type RouterConfig struct {
	Queries           *db.Queries
	Pool              handlers.DBPinger
	AuthSvc           *app.AuthService
	UserSvc           *app.UserService
	HouseholdSvc      *app.HouseholdService
	ChoreSvc          *app.ChoreService
	FamilyCalendar    *app.FamilyCalendar // nil = built from Queries + GoogleCalendar
	GoogleSignIn      *app.GoogleSignIn   // nil = Google sign-in disabled
	GoogleCalendar    *app.GoogleCalendar // nil = Google Calendar unavailable
	JWTSecret         string
	Secure            bool // true in production
	TrustedProxyCount int
}

// NewRouter builds and returns the main http.Handler with all routes and middleware.
// ctx controls the lifetime of background goroutines (rate-limiter sweepers); it
// should be cancelled during application shutdown after the HTTP server drains.
func NewRouter(ctx context.Context, cfg RouterConfig) http.Handler {
	api := http.NewServeMux()

	// ── Handlers ──────────────────────────────────────────────────────────────
	authH := handlers.NewAuthHandler(cfg.AuthSvc, cfg.TrustedProxyCount)
	meH := handlers.NewMeHandler(cfg.AuthSvc, cfg.UserSvc)
	householdH := handlers.NewHouseholdHandler(cfg.HouseholdSvc)
	choreH := handlers.NewChoreHandler(cfg.ChoreSvc)
	familyCal := cfg.FamilyCalendar
	if familyCal == nil {
		// Same service main builds: it reports "unavailable" without Google.
		familyCal = app.NewFamilyCalendar(cfg.Queries, cfg.GoogleCalendar)
	}
	familyH := handlers.NewFamilyCalendarHandler(familyCal)
	googleH := handlers.NewGoogleHandler(cfg.GoogleSignIn, cfg.TrustedProxyCount)
	calendarH := handlers.NewGoogleCalendarHandler(cfg.GoogleCalendar)

	// ── Rate limiters ─────────────────────────────────────────────────────────
	// Each limiter spawns a sweep goroutine that exits when ctx is cancelled.
	// In-memory, so limits are per instance.
	ipKey := middleware.IPKeyFunc(cfg.TrustedProxyCount)
	userKey := middleware.UserKeyFunc(cfg.TrustedProxyCount)
	apiRL := middleware.NewRateLimiter(ctx, 300, time.Minute, ipKey)
	loginRL := middleware.NewRateLimiter(ctx, 10, 15*time.Minute, ipKey)
	registerRL := middleware.NewRateLimiter(ctx, 5, time.Hour, ipKey)
	refreshRL := middleware.NewRateLimiter(ctx, 30, time.Minute, ipKey)
	forgotRL := middleware.NewRateLimiter(ctx, 5, time.Hour, ipKey)
	resetRL := middleware.NewRateLimiter(ctx, 10, time.Hour, ipKey)
	verifyRL := middleware.NewRateLimiter(ctx, 10, time.Hour, ipKey)
	inviteRL := middleware.NewRateLimiter(ctx, 10, time.Hour, userKey)
	googleStartRL := middleware.NewRateLimiter(ctx, 30, 15*time.Minute, ipKey)
	googleCallbackRL := middleware.NewRateLimiter(ctx, 30, 15*time.Minute, ipKey)
	googleRedeemRL := middleware.NewRateLimiter(ctx, 30, 15*time.Minute, ipKey)
	calendarCallbackRL := middleware.NewRateLimiter(ctx, 30, 15*time.Minute, ipKey)

	limit := func(rl *middleware.RateLimiter, h http.HandlerFunc) http.Handler { return rl.Middleware(h) }

	authMW := middleware.Authenticate(cfg.Queries, cfg.JWTSecret)
	// authed: signed in, email may be unverified. Only for account basics.
	authed := func(h http.HandlerFunc) http.Handler { return authMW(h) }
	// verified: signed in with a verified email. The default for everything.
	verified := func(h http.HandlerFunc) http.Handler { return authMW(middleware.RequireEmailVerified(h)) }

	// ── Auth (public) ─────────────────────────────────────────────────────────
	api.Handle("POST /api/v1/auth/register", limit(registerRL, authH.Register))
	api.Handle("POST /api/v1/auth/login", limit(loginRL, authH.Login))
	api.Handle("POST /api/v1/auth/refresh", limit(refreshRL, authH.Refresh))
	api.Handle("POST /api/v1/auth/verify-email", limit(verifyRL, authH.VerifyEmail))
	api.Handle("POST /api/v1/auth/forgot-password", limit(forgotRL, authH.ForgotPassword))
	api.Handle("POST /api/v1/auth/reset-password", limit(resetRL, authH.ResetPassword))
	api.Handle("POST /api/v1/auth/logout", authed(authH.Logout))

	// ── Google sign-in (public). When Google isn't configured, config reports
	// it off and the other three answer the JSON 404 without contacting Google.
	api.HandleFunc("GET /api/v1/auth/config", googleH.AuthConfig)
	api.Handle("GET /api/v1/auth/google/start", limit(googleStartRL, googleH.GoogleStart))
	api.Handle("GET /api/v1/auth/google/callback", limit(googleCallbackRL, googleH.GoogleCallback))
	api.Handle("POST /api/v1/auth/google/redeem", limit(googleRedeemRL, googleH.GoogleRedeem))

	// ── Current user ──────────────────────────────────────────────────────────
	api.Handle("GET /api/v1/me", authed(meH.Get))
	api.Handle("DELETE /api/v1/me", authed(meH.Delete))
	api.Handle("POST /api/v1/me/resend-verification", authed(meH.ResendVerification))
	api.Handle("PUT /api/v1/me", verified(meH.Update))
	api.Handle("POST /api/v1/me/password", verified(meH.ChangePassword))
	api.Handle("GET /api/v1/me/sessions", verified(meH.ListSessions))
	api.Handle("DELETE /api/v1/me/sessions/{id}", verified(meH.RevokeSession))

	// ── Google Calendar connection. Status reports "unavailable" when Google
	// isn't configured; connect, disconnect and callback then answer the JSON
	// 404 without contacting Google. The callback is public: Google redirects
	// the browser there without a bearer token, and the state names the user.
	api.Handle("GET /api/v1/me/google-calendar", verified(calendarH.Status))
	api.Handle("POST /api/v1/me/google-calendar/connect", verified(calendarH.Connect))
	api.Handle("DELETE /api/v1/me/google-calendar", verified(calendarH.Disconnect))
	api.Handle("GET /api/v1/me/google-calendar/callback", limit(calendarCallbackRL, calendarH.Callback))
	api.Handle("GET /api/v1/me/google-calendar/calendars", verified(familyH.MyCalendars))

	// ── Households ────────────────────────────────────────────────────────────
	api.Handle("GET /api/v1/households", verified(householdH.List))
	api.Handle("POST /api/v1/households", verified(householdH.Create))
	api.Handle("GET /api/v1/households/{householdID}", verified(householdH.Get))
	api.Handle("PUT /api/v1/households/{householdID}", verified(householdH.Update))
	api.Handle("DELETE /api/v1/households/{householdID}", verified(householdH.Delete))
	api.Handle("GET /api/v1/households/{householdID}/members", verified(householdH.ListMembers))
	api.Handle("PUT /api/v1/households/{householdID}/members/{userID}", verified(householdH.UpdateMember))
	api.Handle("DELETE /api/v1/households/{householdID}/members/{userID}", verified(householdH.RemoveMember))
	api.Handle("POST /api/v1/households/{householdID}/invites", verified(householdH.CreateInvite))
	// Rate limiter sits inside auth so it can key on the user.
	api.Handle("POST /api/v1/invites/accept", authMW(middleware.RequireEmailVerified(limit(inviteRL, householdH.AcceptInvite))))

	// ── Chores ────────────────────────────────────────────────────────────────
	api.Handle("GET /api/v1/households/{householdID}/chores", verified(choreH.List))
	api.Handle("POST /api/v1/households/{householdID}/chores", verified(choreH.Create))
	api.Handle("GET /api/v1/households/{householdID}/chores/{choreID}", verified(choreH.Get))
	api.Handle("PUT /api/v1/households/{householdID}/chores/{choreID}", verified(choreH.Update))
	api.Handle("DELETE /api/v1/households/{householdID}/chores/{choreID}", verified(choreH.Delete))
	api.Handle("PUT /api/v1/households/{householdID}/chores/{choreID}/completion", verified(choreH.Complete))
	api.Handle("DELETE /api/v1/households/{householdID}/chores/{choreID}/completion", verified(choreH.Complete))

	// ── Family calendar ────────────────────────────────────────────────────
	// Read-only: entries come from the household's chosen Google calendar.
	// Chorus has no route that creates, edits or deletes an entry.
	api.Handle("GET /api/v1/households/{householdID}/calendar", verified(familyH.Status))
	api.Handle("PUT /api/v1/households/{householdID}/calendar", verified(familyH.Set))
	api.Handle("DELETE /api/v1/households/{householdID}/calendar", verified(familyH.Clear))
	api.Handle("GET /api/v1/households/{householdID}/events", verified(familyH.Events))

	// Unknown API paths get a JSON 404 rather than falling through to the web client.
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		respond.Error(w, http.StatusNotFound, "not_found", "No such endpoint.")
	})

	// ── Top-level mux: API, health checks, and the web test client ────────────
	mux := http.NewServeMux()
	mux.Handle("/api/", noStore(apiRL.Middleware(api)))
	mux.HandleFunc("GET /health", handlers.HandleLiveness)
	mux.HandleFunc("GET /ready", handlers.HandleReadiness(cfg.Pool))

	staticFS := web.Static(!cfg.Secure)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	// The web client is a single page; these are the paths emailed links (and
	// the Google sign-in and calendar callback redirects) land on.
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, staticFS, "index.html")
	}
	for _, p := range []string{"/{$}", "/verify-email", "/reset-password", "/forgot-password", "/google-sign-in", "/account"} {
		mux.HandleFunc("GET "+p, serveIndex)
	}

	// ── Global middleware chain (outermost first) ─────────────────────────────
	// Recover sits inside Logging so a recovered panic is logged as a 500.
	// BodyLimit is innermost so it wraps r.Body before any handler reads it.
	// API payloads are small JSON objects, so 64 KiB is generous.
	return middleware.RequestID(
		middleware.Logging(
			middleware.Recover(
				middleware.SecurityHeaders(cfg.Secure,
					middleware.BodyLimit(64<<10)(mux),
				),
			),
		),
	)
}

// noStore keeps API responses (which contain tokens and personal data) out of
// shared and browser caches.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
