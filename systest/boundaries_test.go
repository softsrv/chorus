//go:build systest

package systest

import (
	"bytes"
	"net/http"
	"testing"
)

// TestSystem_HouseholdIsolationAndGoogleOff holds the running app to its two
// boundaries. A user gets the same 404 for another household's resources as
// for ones that don't exist. With Google unconfigured, the app reports Google
// off, even when the test runner's own environment carries Google settings.
func TestSystem_HouseholdIsolationAndGoogleOff(t *testing.T) {
	// Google settings in the runner's own environment. startApp builds the
	// app's environment explicitly, so none of these may reach the app; if
	// they leaked, sign-in would report on and google/start would redirect.
	// Obviously fake values; the token key is 32 zero bytes, base64.
	t.Setenv("GOOGLE_CLIENT_ID", "systest-fake-google-client-id.apps.example.test")
	t.Setenv("GOOGLE_CLIENT_SECRET", "systest-fake-google-client-secret")
	t.Setenv("GOOGLE_TOKEN_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	app := startApp(t, appOpts{})

	// Two sign-ups, well under the app's 5 per hour; reused below.
	a := app.signUp(t, uniqueEmail(t))
	b := app.signUp(t, uniqueEmail(t))
	if a.User.ID == b.User.ID {
		t.Fatalf("both sign-ups returned user %s", a.User.ID)
	}

	t.Run("household isolation", func(t *testing.T) {
		type idOnly struct {
			ID string `json:"id"`
		}
		create := func(what, path string, body any) string {
			t.Helper()
			var v idOnly
			app.do("POST", path, b.AccessToken, body).expect(t, "B creates "+what, http.StatusCreated).decode(t, &v)
			if v.ID == "" {
				t.Fatalf("B creates %s: no id in response", what)
			}
			return v.ID
		}

		hid := create("household", "/api/v1/households", map[string]string{"name": "Systest B Household", "timezone": "UTC"})
		hp := "/api/v1/households/" + hid
		member := create("family member", hp+"/family-members", map[string]string{"name": "Systest B Member", "pin": "1234"})
		quest1 := create("quest", hp+"/quests", map[string]any{"name": "Systest dishes", "points": 10, "repeat_days": 1})
		quest2 := create("second quest", hp+"/quests", map[string]any{"name": "Systest counters", "points": 5, "repeat_days": 1})
		reward := create("reward", hp+"/rewards", map[string]any{"description": "Systest movie night", "cost": 100})
		combo := create("combo", hp+"/combos", map[string]any{"name": "Systest crew", "bonus_points": 20, "quest_ids": []string{quest1, quest2}})

		// B can read its own data, so A's 404 below is about A, not the ids.
		for _, p := range []string{hp, hp + "/family-members/" + member, hp + "/quests/" + quest1, hp + "/rewards/" + reward, hp + "/combos/" + combo} {
			app.do("GET", p, b.AccessToken, nil).expect(t, "B reads own "+p, http.StatusOK)
		}

		// A random household id stands for "does not exist" on every route.
		ghost := "/api/v1/households/" + randUUID(t)
		cases := []struct {
			name, real, missing string
		}{
			{"household", hp, ghost},
			{"family-members list", hp + "/family-members", ghost + "/family-members"},
			{"family-member by id", hp + "/family-members/" + member, hp + "/family-members/" + randUUID(t)},
			{"quests list", hp + "/quests", ghost + "/quests"},
			{"quest by id", hp + "/quests/" + quest1, hp + "/quests/" + randUUID(t)},
			{"rewards list", hp + "/rewards", ghost + "/rewards"},
			{"reward by id", hp + "/rewards/" + reward, hp + "/rewards/" + randUUID(t)},
			{"combos list", hp + "/combos", ghost + "/combos"},
			{"combo by id", hp + "/combos/" + combo, hp + "/combos/" + randUUID(t)},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got := app.do("GET", c.real, a.AccessToken, nil).expect(t, "A reads B's "+c.name, http.StatusNotFound)
				none := app.do("GET", c.missing, a.AccessToken, nil).expect(t, "A reads nonexistent "+c.name, http.StatusNotFound)
				if !bytes.Equal(got.Body, none.Body) {
					t.Fatalf("A's response for B's %s differs from a nonexistent one:\nB's:     %s\nmissing: %s", c.name, got.Body, none.Body)
				}
			})
		}
	})

	t.Run("google off", func(t *testing.T) {
		var cfg map[string]bool
		app.do("GET", "/api/v1/auth/config", "", nil).expect(t, "GET /api/v1/auth/config", http.StatusOK).decode(t, &cfg)
		if len(cfg) != 2 || cfg["google_sign_in"] || cfg["google_calendar"] {
			t.Fatalf("auth config = %v, want {google_sign_in:false google_calendar:false}", cfg)
		}
		if _, ok := cfg["google_sign_in"]; !ok {
			t.Fatalf("auth config = %v, missing google_sign_in", cfg)
		}
		if _, ok := cfg["google_calendar"]; !ok {
			t.Fatalf("auth config = %v, missing google_calendar", cfg)
		}

		app.do("GET", "/api/v1/auth/google/start", "", nil).expect(t, "GET /api/v1/auth/google/start", http.StatusNotFound)

		var status struct {
			Status string `json:"status"`
			Access string `json:"access"`
		}
		app.do("GET", "/api/v1/me/google-calendar", a.AccessToken, nil).
			expect(t, "GET /api/v1/me/google-calendar", http.StatusOK).decode(t, &status)
		if status.Status != "unavailable" || status.Access != "" {
			t.Fatalf("calendar status = %+v, want status unavailable and no access", status)
		}
	})
}

// randUUID is a random version-4 UUID: an id no row has.
func randUUID(t *testing.T) string {
	t.Helper()
	h := randHex(t, 16)
	// Set the version (4) and variant (10xx) nibbles.
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}
