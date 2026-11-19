//go:build systest

package systest

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	scriptPath     = "/static/js/app.js"
	stylesheetPath = "/static/css/dist/app.css"
)

// staticRef matches every src="/static/..." and href="/static/..." on a page.
var staticRef = regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`)

// stylesheetBuilt reports whether the app under test embeds the generated
// stylesheet. The image always does (its Dockerfile runs Tailwind before
// building). The binary TestMain builds from this checkout embeds it only if
// web/static/css/dist/app.css exists here (`make tailwind`).
func stylesheetBuilt() bool {
	if os.Getenv("SYSTEST_IMAGE") != "" {
		return true
	}
	_, err := os.Stat("../web/static/css/dist/app.css")
	return err == nil
}

// TestSystem_CoreFlow drives the started app the way a new user does,
// against a freshly migrated database of its own with no users: health and
// readiness, the web client, a refused unauthenticated request, then sign-up
// through real SMTP, verification, login, and a household created and read
// back.
func TestSystem_CoreFlow(t *testing.T) {
	db := freshDatabase(t)
	if n := countUsers(t, db); n != 0 {
		t.Fatalf("fresh database has %d users, want 0", n)
	}
	app := startApp(t, appOpts{Env: map[string]string{"DATABASE_URL": db}}) // startApp already waited for /health 200 {status:ok}

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
		// The app runs from an empty directory, so every asset comes from
		// the embedded filesystem of the binary or image under test.
		index := app.do("GET", "/", "", nil).expect(t, "GET /", http.StatusOK)
		page := string(index.Body)
		if !strings.Contains(page, "<title>Chorus</title>") {
			t.Fatalf("GET / is not the app's page:\n%s", page)
		}

		// Every /static asset the page links must be linked and must load.
		linked := map[string]bool{}
		for _, m := range staticRef.FindAllStringSubmatch(page, -1) {
			linked[m[1]] = true
		}
		for _, want := range []string{scriptPath, stylesheetPath} {
			if !linked[want] {
				t.Fatalf("GET / does not reference %s:\n%s", want, page)
			}
		}
		paths := make([]string, 0, len(linked))
		for p := range linked {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		for _, p := range paths {
			if p == stylesheetPath && !stylesheetBuilt() {
				// Binary mode from a checkout where `make tailwind` never
				// ran: the stylesheet is gitignored and generated, so this
				// binary does not embed it. The image always builds it, and
				// every image job runs this test with the assertion on.
				t.Logf("%s not asserted: binary built without the generated stylesheet (run `make tailwind` to include it)", p)
				continue
			}
			r := app.do("GET", p, "", nil).expect(t, "GET "+p, http.StatusOK)
			if len(r.Body) == 0 {
				t.Fatalf("GET %s returned an empty body", p)
			}
			if p == stylesheetPath && !strings.HasPrefix(r.Header.Get("Content-Type"), "text/css") {
				t.Fatalf("GET %s Content-Type = %q, want text/css", p, r.Header.Get("Content-Type"))
			}
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
