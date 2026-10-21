package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

// ── Config ────────────────────────────────────────────────────────────────────

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
}

func mustLoadConfig() config {
	cfg := config{
		AppEnv:        getEnvOrDefault("APP_ENV", "development"),
		Port:          getEnvOrDefault("PORT", "8080"),
		AppBaseURL:    mustGetEnv("APP_BASE_URL"),
		DatabaseURL:   mustGetEnv("DATABASE_URL"),
		JWTSecret:     mustGetEnv("JWT_SECRET"),
		SMTPHost:      mustGetEnv("SMTP_HOST"),
		SMTPPort:      mustGetEnv("SMTP_PORT"),
		SMTPUsername:  os.Getenv("SMTP_USERNAME"),
		SMTPPassword:  os.Getenv("SMTP_PASSWORD"),
		SMTPFromEmail: mustGetEnv("SMTP_FROM_EMAIL"),
		SMTPFromName:  getEnvOrDefault("SMTP_FROM_NAME", "Chorus"),
	}

	if len(cfg.JWTSecret) < 32 {
		slog.Error("JWT_SECRET must be at least 32 bytes")
		os.Exit(1)
	}

	var err error
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

	return cfg
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error(fmt.Sprintf("required environment variable %s is not set", key))
		os.Exit(1)
	}
	return v
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
