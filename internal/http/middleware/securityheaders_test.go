package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/softsrv/chorus/internal/http/middleware"
)

// CLM-8: Assert Content-Security-Policy set by SecurityHeaders matches the exact
// required directives, introduces frame-src, and has no 'unsafe-inline', 'unsafe-eval' or '*'.
func TestSecurityHeaders_CSPDirectives(t *testing.T) {
	t.Parallel()

	testModes := []struct {
		name   string
		secure bool
	}{
		{name: "insecure (development)", secure: false},
		{name: "secure (production)", secure: true},
	}

	expectedDirectives := map[string][]string{
		"default-src":     {"'self'"},
		"script-src":      {"'self'", "https://accounts.google.com/gsi/client"},
		"style-src":       {"'self'", "https://accounts.google.com/gsi/"},
		"img-src":         {"'self'", "data:"},
		"font-src":        {"'self'"},
		"connect-src":     {"'self'", "https://accounts.google.com/gsi/"},
		"frame-src":       {"https://accounts.google.com/gsi/"},
		"object-src":      {"'none'"},
		"base-uri":        {"'none'"},
		"form-action":     {"'self'"},
		"frame-ancestors": {"'none'"},
	}

	for _, mode := range testModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)

			handler := middleware.SecurityHeaders(mode.secure, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			handler.ServeHTTP(rec, req)

			csp := rec.Header().Get("Content-Security-Policy")
			if csp == "" {
				t.Fatalf("Content-Security-Policy header is missing")
			}

			// Assert no 'unsafe-inline', 'unsafe-eval' or wildcard appears.
			for _, forbidden := range []string{"'unsafe-inline'", "'unsafe-eval'", "*"} {
				if strings.Contains(csp, forbidden) {
					t.Errorf("Content-Security-Policy contains forbidden token %q: %q", forbidden, csp)
				}
			}

			// Parse directives separated by ';'
			directives := make(map[string][]string)
			for _, part := range strings.Split(csp, ";") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				fields := strings.Fields(part)
				name := fields[0]
				values := fields[1:]
				directives[name] = values
			}

			if len(directives) != len(expectedDirectives) {
				t.Errorf("directive count = %d, want %d (directives: %v)", len(directives), len(expectedDirectives), directives)
			}

			for name, wantValues := range expectedDirectives {
				gotValues, ok := directives[name]
				if !ok {
					t.Errorf("%s directive missing from CSP: %q", name, csp)
					continue
				}
				if !slices.Equal(gotValues, wantValues) {
					t.Errorf("%s = %v, want exactly %v", name, gotValues, wantValues)
				}
			}
		})
	}
}

// CLM-9: Assert Cross-Origin-Opener-Policy is same-origin-allow-popups on every response,
// while Cross-Origin-Resource-Policy stays same-origin and X-Frame-Options stays DENY.
func TestSecurityHeaders_OpenerAndFraming(t *testing.T) {
	t.Parallel()

	testModes := []struct {
		name   string
		secure bool
	}{
		{name: "insecure (development)", secure: false},
		{name: "secure (production)", secure: true},
	}

	for _, mode := range testModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)

			handler := middleware.SecurityHeaders(mode.secure, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			handler.ServeHTTP(rec, req)

			if got := rec.Header().Get("Cross-Origin-Opener-Policy"); got != "same-origin-allow-popups" {
				t.Errorf("Cross-Origin-Opener-Policy = %q, want %q", got, "same-origin-allow-popups")
			}
			if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
				t.Errorf("Cross-Origin-Resource-Policy = %q, want %q", got, "same-origin")
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
			}
		})
	}
}
