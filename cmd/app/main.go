package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/email"
	internalhttp "github.com/softsrv/chorus/internal/http"
	"github.com/softsrv/chorus/internal/http/handlers"
)

func main() {
	cfg := mustLoadConfig()
	setupLogger(cfg.AppEnv)

	slog.Info("starting", "env", cfg.AppEnv, "port", cfg.Port)

	// ── Database ──────────────────────────────────────────────────────────────
	if cfg.DBMinConns > cfg.DBMaxConns {
		slog.Error("DB_MIN_CONNS must not exceed DB_MAX_CONNS",
			"min", cfg.DBMinConns, "max", cfg.DBMaxConns)
		os.Exit(1)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		slog.Error("parse database url", "error", err)
		os.Exit(1)
	}
	poolCfg.MaxConns = int32(cfg.DBMaxConns)
	poolCfg.MinConns = int32(cfg.DBMinConns)
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 10 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		slog.Error("connect to database", "error", err)
		os.Exit(1)
	}

	queries := db.New(pool)

	// ── Email ─────────────────────────────────────────────────────────────────
	mailer := email.NewSMTPMailer(
		cfg.SMTPHost, cfg.SMTPPort,
		cfg.SMTPUsername, cfg.SMTPPassword,
		cfg.SMTPFromEmail, cfg.SMTPFromName,
	)

	// ── Services ──────────────────────────────────────────────────────────────
	authSvc := app.NewAuthService(queries, pool, mailer, app.AuthServiceConfig{
		JWTSecret:      cfg.JWTSecret,
		AccessExpiry:   cfg.JWTAccessExpiry,
		RefreshExpiry:  cfg.RefreshTokenExpiry,
		BCryptCost:     cfg.BCryptCost,
		PasswordMinLen: cfg.PasswordMinLen,
		AppBaseURL:     cfg.AppBaseURL,
		AppName:        cfg.SMTPFromName,
	})
	userSvc := app.NewUserService(queries)
	householdSvc := app.NewHouseholdService(queries, pool)
	choreSvc := app.NewChoreService(queries)
	eventSvc := app.NewEventService(queries)

	// Google sign-in is optional: it is enabled only when both the client id
	// and secret are set, and the app runs normally without them.
	var googleSignIn *app.GoogleSignIn
	if cfg.Google.Enabled {
		googleSignIn = app.NewGoogleSignIn(queries, authSvc, app.GoogleSignInConfig{
			ClientID:     cfg.Google.ClientID,
			ClientSecret: cfg.Google.ClientSecret,
			RedirectURL:  strings.TrimRight(cfg.AppBaseURL, "/") + "/api/v1/auth/google/callback",
		})
	}

	// Google Calendar is optional too: it needs Google configured and a
	// GOOGLE_TOKEN_KEY (standard base64 of 32 bytes) to encrypt stored tokens.
	// Without either the calendar status reports "unavailable".
	var googleCalendar *app.GoogleCalendar
	if cfg.Google.Enabled {
		if cipher, err := googleTokenCipher(cfg.Google.TokenKey); err != nil {
			slog.Warn("Google Calendar is disabled", "reason", err.Error())
		} else {
			googleCalendar = app.NewGoogleCalendar(queries, cipher, app.GoogleCalendarConfig{
				ClientID:     cfg.Google.ClientID,
				ClientSecret: cfg.Google.ClientSecret,
				RedirectURL:  strings.TrimRight(cfg.AppBaseURL, "/") + "/api/v1/me/google-calendar/callback",
			})
		}
	}

	// ── Background context (token cleanup + rate-limiter sweepers) ────────────
	// cleanupCtx is cancelled after the HTTP server drains, giving background
	// goroutines a clean signal to stop without racing active requests.
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())

	// ── Router ────────────────────────────────────────────────────────────────
	handler := internalhttp.NewRouter(cleanupCtx, internalhttp.RouterConfig{
		Queries:           queries,
		Pool:              pool,
		AuthSvc:           authSvc,
		UserSvc:           userSvc,
		HouseholdSvc:      householdSvc,
		ChoreSvc:          choreSvc,
		EventSvc:          eventSvc,
		GoogleSignIn:      googleSignIn,
		GoogleCalendar:    googleCalendar,
		JWTSecret:         cfg.JWTSecret,
		Secure:            cfg.AppEnv == "production",
		TrustedProxyCount: cfg.TrustedProxyCount,
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// ── Token cleanup background worker ───────────────────────────────────────
	go app.RunTokenCleanup(cleanupCtx, queries)

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("listen", "error", listenErr)
			os.Exit(1)
		}
	}()

	<-sigCtx.Done()
	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown", "error", err)
	}

	// Stop background goroutines (token cleanup, rate-limiter sweepers).
	stopCleanup()

	// Wait for any in-flight email deliveries to complete before closing the pool.
	authSvc.Shutdown()

	pool.Close()
	slog.Info("shutdown complete")
}

// googleTokenCipher builds the token cipher from GOOGLE_TOKEN_KEY.
func googleTokenCipher(key string) (*app.TokenCipher, error) {
	if key == "" {
		return nil, errors.New("GOOGLE_TOKEN_KEY is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("GOOGLE_TOKEN_KEY must be standard base64 of 32 bytes")
	}
	return app.NewTokenCipher(raw)
}

// ── Config ────────────────────────────────────────────────────────────────────

type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	TokenKey     string
	Enabled      bool
}

type config struct {
	AppEnv             string
	Port               string
	AppBaseURL         string
	DatabaseURL        string
	DBMaxConns         int
	DBMinConns         int
	TrustedProxyCount  int
	JWTSecret          string
	JWTAccessExpiry    time.Duration
	RefreshTokenExpiry time.Duration
	BCryptCost         int
	PasswordMinLen     int
	SMTPHost           string
	SMTPPort           string
	SMTPUsername       string
	SMTPPassword       string
	SMTPFromEmail      string
	SMTPFromName       string
	Google             GoogleConfig
}

func mustLoadConfig() config {
	cfg, err := loadConfig(os.Getenv, slog.Default())
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	return cfg
}

func loadConfig(getenv func(string) string, logger *slog.Logger) (config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if logger == nil {
		logger = slog.Default()
	}

	getEnvOrDefault := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	requireEnv := func(key string) (string, error) {
		v := getenv(key)
		if v == "" {
			return "", fmt.Errorf("required environment variable %s is not set", key)
		}
		return v, nil
	}

	appBaseURL, err := requireEnv("APP_BASE_URL")
	if err != nil {
		return config{}, err
	}
	databaseURL, err := requireEnv("DATABASE_URL")
	if err != nil {
		return config{}, err
	}
	jwtSecret, err := requireEnv("JWT_SECRET")
	if err != nil {
		return config{}, err
	}
	smtpHost, err := requireEnv("SMTP_HOST")
	if err != nil {
		return config{}, err
	}
	smtpPort, err := requireEnv("SMTP_PORT")
	if err != nil {
		return config{}, err
	}
	smtpFromEmail, err := requireEnv("SMTP_FROM_EMAIL")
	if err != nil {
		return config{}, err
	}

	if len(jwtSecret) < 32 {
		return config{}, errors.New("JWT_SECRET must be at least 32 bytes")
	}

	cfg := config{
		AppEnv:        getEnvOrDefault("APP_ENV", "development"),
		Port:          getEnvOrDefault("PORT", "8080"),
		AppBaseURL:    appBaseURL,
		DatabaseURL:   databaseURL,
		JWTSecret:     jwtSecret,
		SMTPHost:      smtpHost,
		SMTPPort:      smtpPort,
		SMTPUsername:  getenv("SMTP_USERNAME"),
		SMTPPassword:  getenv("SMTP_PASSWORD"),
		SMTPFromEmail: smtpFromEmail,
		SMTPFromName:  getEnvOrDefault("SMTP_FROM_NAME", "Chorus"),
		Google: GoogleConfig{
			ClientID:     getEnvOrDefault("GOOGLE_CLIENT_ID", ""),
			ClientSecret: getEnvOrDefault("GOOGLE_CLIENT_SECRET", ""),
			TokenKey:     getEnvOrDefault("GOOGLE_TOKEN_KEY", ""),
		},
	}

	hasID := cfg.Google.ClientID != ""
	hasSecret := cfg.Google.ClientSecret != ""
	if hasID && hasSecret {
		cfg.Google.Enabled = true
	} else if hasID || hasSecret {
		logger.Warn("Google sign-in is disabled: both GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET must be set")
	}

	cfg.DBMaxConns, err = strconv.Atoi(getEnvOrDefault("DB_MAX_CONNS", "25"))
	if err != nil {
		cfg.DBMaxConns = 25
	}

	cfg.DBMinConns, err = strconv.Atoi(getEnvOrDefault("DB_MIN_CONNS", "5"))
	if err != nil {
		cfg.DBMinConns = 5
	}

	cfg.TrustedProxyCount, err = strconv.Atoi(getEnvOrDefault("TRUSTED_PROXY_COUNT", "0"))
	if err != nil {
		cfg.TrustedProxyCount = 0
	}

	cfg.JWTAccessExpiry, err = time.ParseDuration(getEnvOrDefault("JWT_ACCESS_EXPIRY", "15m"))
	if err != nil || cfg.JWTAccessExpiry <= 0 {
		cfg.JWTAccessExpiry = 15 * time.Minute
	}

	cfg.RefreshTokenExpiry, err = time.ParseDuration(getEnvOrDefault("REFRESH_TOKEN_EXPIRY", "720h"))
	if err != nil {
		cfg.RefreshTokenExpiry = 720 * time.Hour
	}

	cfg.BCryptCost, err = strconv.Atoi(getEnvOrDefault("BCRYPT_COST", "12"))
	if err != nil || cfg.BCryptCost < 10 || cfg.BCryptCost > 15 {
		cfg.BCryptCost = 12
	}

	cfg.PasswordMinLen, err = strconv.Atoi(getEnvOrDefault("PASSWORD_MIN_LENGTH", "8"))
	if err != nil || cfg.PasswordMinLen < 8 {
		cfg.PasswordMinLen = 8
	}

	return cfg, nil
}

func setupLogger(env string) {
	var handler slog.Handler
	if env == "production" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	}
	slog.SetDefault(slog.New(handler))
}

// Compile-time checks.
var _ handlers.DBPinger = (*pgxpool.Pool)(nil)
var _ db.DBTX = (*pgxpool.Pool)(nil)
