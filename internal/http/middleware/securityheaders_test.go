package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/softsrv/chorus/internal/http/middleware"
)

// CLM-14: Assert Content-Security-Policy set by SecurityHeaders has connect-src
// exactly 'self' and form-action exactly 'self'.
func TestSecurityHeaders_CSPDirectives(t *testing.T) {
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

			csp := rec.Header().Get("Content-Security-Policy")
			if csp == "" {
				t.Fatalf("Content-Security-Policy header is missing")
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

			// Assert connect-src has exactly 'self'
			connectSrc, ok := directives["connect-src"]
			if !ok {
				t.Errorf("connect-src directive missing from CSP: %q", csp)
			} else if len(connectSrc) != 1 || connectSrc[0] != "'self'" {
				t.Errorf("connect-src = %v, want exactly ['self']", connectSrc)
			}

			// Assert form-action has exactly 'self'
			formAction, ok := directives["form-action"]
			if !ok {
				t.Errorf("form-action directive missing from CSP: %q", csp)
			} else if len(formAction) != 1 || formAction[0] != "'self'" {
				t.Errorf("form-action = %v, want exactly ['self']", formAction)
			}
		})
	}
}
