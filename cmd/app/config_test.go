package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func requiredEnv() map[string]string {
	return map[string]string{
		"APP_BASE_URL":    "http://localhost:8080",
		"DATABASE_URL":    "postgres://x",
		"JWT_SECRET":      strings.Repeat("s", 32),
		"SMTP_HOST":       "localhost",
		"SMTP_PORT":       "25",
		"SMTP_FROM_EMAIL": "no-reply@example.com",
	}
}

func load(t *testing.T, env map[string]string) (config, error, string) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg, err := loadConfig(func(k string) string { return env[k] }, logger)
	return cfg, err, buf.String()
}

func TestLoadConfig_GoogleUnsetIsDisabled(t *testing.T) {
	cfg, err, logs := load(t, requiredEnv())
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Google.Enabled {
		t.Fatal("Google enabled with no GOOGLE_* set")
	}
	if logs != "" {
		t.Fatalf("unexpected log output: %s", logs)
	}
}

func TestLoadConfig_GoogleOnlyOneOfIDSecretStaysDisabledAndWarns(t *testing.T) {
	for _, key := range []string{"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"} {
		env := requiredEnv()
		env[key] = "x"
		cfg, err, logs := load(t, env)
		if err != nil {
			t.Fatalf("%s: loadConfig: %v", key, err)
		}
		if cfg.Google.Enabled {
			t.Fatalf("%s only: Google enabled", key)
		}
		if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "GOOGLE_CLIENT_ID") {
			t.Fatalf("%s only: expected warning, got %q", key, logs)
		}
	}
}

func TestLoadConfig_GoogleBothSetIsEnabled(t *testing.T) {
	env := requiredEnv()
	env["GOOGLE_CLIENT_ID"] = "id"
	env["GOOGLE_CLIENT_SECRET"] = "secret"
	cfg, err, logs := load(t, env)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.Google.Enabled || cfg.Google.ClientID != "id" || cfg.Google.ClientSecret != "secret" {
		t.Fatalf("unexpected Google config: %+v", cfg.Google)
	}
	if logs != "" {
		t.Fatalf("unexpected log output: %s", logs)
	}
}

func TestLoadConfig_RequiredVarsStillRequired(t *testing.T) {
	env := requiredEnv()
	delete(env, "SMTP_HOST")
	if _, err, _ := load(t, env); err == nil {
		t.Fatal("expected error for missing SMTP_HOST")
	}
	env = requiredEnv()
	env["JWT_SECRET"] = "short"
	if _, err, _ := load(t, env); err == nil {
		t.Fatal("expected error for short JWT_SECRET")
	}
}
