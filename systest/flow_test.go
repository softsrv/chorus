//go:build systest

package systest

import (
	"net/http"
	"strings"
	"testing"
)

// TestSystem_CoreFlow drives the started app the way a new user does,
// against a migrated database with nothing seeded: health and readiness, the
// web client, a refused unauthenticated request, then sign-up through real
// SMTP, verification, login, and a household created and read back.
func TestSystem_CoreFlow(t *testing.T) {
	app := startApp(t, appOpts{}) // startApp already waited for /health 200 {status:ok}

	t.Run("health and readiness", func(t *testing.T) {
		var health struct {
			Status string `json:"status"`
		}
		app.do("GET", "/health", "", nil).expect(t, "GET /health", http.StatusOK).decode(t, &health)
		if health.Status != "ok" {
			t.Fatalf("GET /health status = %q, want ok", health.Status)
		}
		var ready struct {
			Status string `json:"status"`
		}
		app.do("GET", "/ready", "", nil).expect(t, "GET /ready", http.StatusOK).decode(t, &ready)
		if ready.Status != "ok" {
			t.Fatalf("GET /ready status = %q, want ok", ready.Status)
		}
	})

	t.Run("web client", func(t *testing.T) {
		// The app runs from an empty directory, so these come from the
		// embedded filesystem. The generated stylesheet is built only inside
		// the Dockerfile, so it is not asserted here.
		index := app.do("GET", "/", "", nil).expect(t, "GET /", http.StatusOK)
		page := string(index.Body)
		if !strings.Contains(page, "<title>Chorus</title>") {
			t.Fatalf("GET / is not the app's page:\n%s", page)
		}
		if !strings.Contains(page, `src="/static/js/app.js"`) {
			t.Fatalf("GET / does not reference /static/js/app.js:\n%s", page)
		}
		js := app.do("GET", "/static/js/app.js", "", nil).expect(t, "GET /static/js/app.js", http.StatusOK)
		if len(js.Body) == 0 {
			t.Fatal("GET /static/js/app.js returned an empty body")
		}
	})

	t.Run("sign-in required", func(t *testing.T) {
		app.do("GET", "/api/v1/me", "", nil).expect(t, "GET /api/v1/me without a token", http.StatusUnauthorized)
	})

	t.Run("register verify login household", func(t *testing.T) {
		email := uniqueEmail(t)
		verified := app.signUp(t, email) // register 202, mail over SMTP, verify-email token pair
		if !strings.EqualFold(verified.User.Email, email) {
			t.Fatalf("verified user email = %q, want %q", verified.User.Email, email)
		}

		var login tokens
		app.do("POST", "/api/v1/auth/login", "", map[string]string{"email": email, "password": fakePassword}).
			expect(t, "login", http.StatusOK).decode(t, &login)
		if login.AccessToken == "" || login.User.ID != verified.User.ID {
			t.Fatalf("login returned %+v, want a token for user %s", login, verified.User.ID)
		}

		type household struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Timezone string `json:"timezone"`
			Role     string `json:"role"`
		}
		want := household{Name: "Systest Household", Timezone: "Europe/Berlin"}
		var created household
		app.do("POST", "/api/v1/households", login.AccessToken, map[string]string{"name": want.Name, "timezone": want.Timezone}).
			expect(t, "create household", http.StatusCreated).decode(t, &created)
		if created.ID == "" || created.Name != want.Name || created.Timezone != want.Timezone {
			t.Fatalf("created household = %+v, want name %q timezone %q", created, want.Name, want.Timezone)
		}

		var got household
		app.do("GET", "/api/v1/households/"+created.ID, login.AccessToken, nil).
			expect(t, "get household", http.StatusOK).decode(t, &got)
		if got != created {
			t.Fatalf("read back %+v, want %+v", got, created)
		}
	})
}
