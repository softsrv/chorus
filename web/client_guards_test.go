package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func readStatic(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(Static(false), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func clientJS(t *testing.T) map[string]string {
	return map[string]string{
		"js/app.js": readStatic(t, "js/app.js"),
		"js/api.js": readStatic(t, "js/api.js"),
	}
}

// removedClientFeatures are the login-member, invite and chore surfaces the
// single-login, quests-replace-chores change retires from the web client.
var removedClientFeatures = []string{
	"/invites", "/members", "/chores", "Create invite code", "Make admin",
	"Make member", "membersTab", "membersFor", "choresTab", "Add chore",
	"'Chores'", "Join with invite code", "XXXX-XXXX-XXXX", "/completion", "Reopen",
}

func findRemoved(src string) []string {
	var hits []string
	for _, tok := range removedClientFeatures {
		if strings.Contains(src, tok) {
			hits = append(hits, tok)
		}
	}
	return hits
}

func TestClientJS_NoLoginMembersInvitesOrChores(t *testing.T) {
	t.Run("detector fires", func(t *testing.T) {
		if len(findRemoved("await api('GET', `/households/${h.id}/members`);")) == 0 {
			t.Fatal("expected the detector to flag a /members call")
		}
	})
	for name, src := range clientJS(t) {
		if hits := findRemoved(src); len(hits) > 0 {
			t.Errorf("%s still contains retired client features: %v", name, hits)
		}
	}
	app := readStatic(t, "js/app.js")
	if !strings.Contains(app, "let tab = 'quests';") {
		t.Error("expected the default household tab to be 'quests'")
	}
	if !strings.Contains(app, "['quests', 'Quests']") || !strings.Contains(app, "['family', 'Family']") {
		t.Error("expected the Quests and Family tabs")
	}
}

// inlineMarkup matches the shapes the strict CSP forbids or that would insert
// API data as markup: innerHTML/outerHTML/insertAdjacentHTML assignments,
// style= attributes and setAttribute('style', ...).
var inlineMarkup = regexp.MustCompile(`\.(innerHTML|outerHTML)\s*=|insertAdjacentHTML|style=|setAttribute\(\s*['"]style['"]`)

// inlineScript matches a <script> element without a src attribute.
var inlineScript = regexp.MustCompile(`<script(?:\s+(?:type|nonce)="[^"]*")*\s*>`)

// scriptSrc matches <script> tags with a src attribute.
var scriptSrc = regexp.MustCompile(`<script\b[^>]*\s+src="([^"]+)"`)

const gisScriptURL = "https://accounts.google.com/gsi/client"

func isAllowedScriptSrc(src string) bool {
	if src == gisScriptURL {
		return true
	}
	if strings.HasPrefix(src, "//") || strings.Contains(src, "://") {
		return false
	}
	return true
}

func TestClient_NoInlineMarkupStylesOrScripts(t *testing.T) {
	t.Run("detector fires", func(t *testing.T) {
		for _, bad := range []string{"$x.innerHTML = res.name;", `el('div', { style: 'x' }); '<div style="color:red">'`, "n.setAttribute('style', 'x')"} {
			if !inlineMarkup.MatchString(bad) {
				t.Errorf("expected detector to match %q", bad)
			}
		}
		if !inlineScript.MatchString("<script>alert(1)</script>") {
			t.Error("expected detector to match an inline <script>")
		}
		if isAllowedScriptSrc("https://malicious.example.com/evil.js") {
			t.Error("expected detector to reject third-party script")
		}
		if isAllowedScriptSrc("https://accounts.google.com/gsi/client/extra") {
			t.Error("expected detector to reject non-exact GIS URL")
		}
		if !isAllowedScriptSrc("/static/js/app.js") {
			t.Error("expected detector to allow same-origin script")
		}
		if !isAllowedScriptSrc(gisScriptURL) {
			t.Error("expected detector to allow exact GIS URL")
		}
	})
	for name, src := range clientJS(t) {
		if m := inlineMarkup.FindString(src); m != "" {
			t.Errorf("%s contains forbidden markup insertion %q", name, m)
		}
	}
	html := readStatic(t, "index.html")
	if m := inlineMarkup.FindString(html); m != "" {
		t.Errorf("index.html contains forbidden inline style %q", m)
	}
	if m := inlineScript.FindString(html); m != "" {
		t.Errorf("index.html contains an inline script %q", m)
	}

	matches := scriptSrc.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		t.Error("index.html contains no script tags with src")
	}
	var foundGIS bool
	for _, m := range matches {
		src := m[1]
		if !isAllowedScriptSrc(src) {
			t.Errorf("index.html contains disallowed script src %q (want same-origin or %s)", src, gisScriptURL)
		}
		if src == gisScriptURL {
			foundGIS = true
		}
	}
	if !foundGIS {
		t.Errorf("index.html missing script src %q", gisScriptURL)
	}

	gisScript := regexp.MustCompile(`<script\b[^>]*src="https://accounts\.google\.com/gsi/client"[^>]*>`)
	gisMatch := gisScript.FindString(html)
	if gisMatch == "" {
		t.Error("index.html missing script tag for GIS library")
	} else if !strings.Contains(gisMatch, "async") {
		t.Errorf("GIS script tag %q missing async attribute", gisMatch)
	}
}

// removedGoogleRedirectFlow are the strings only the retired Google redirect
// sign-in and calendar-connect flows used: the /google-sign-in landing page,
// the hand-off code redeem, the start route and the Account page's
// ?google_calendar= return parsing. The SDK flows need none of them.
var removedGoogleRedirectFlow = []string{
	"/api/v1/auth/google/start", "/auth/google/redeem", "/google-sign-in",
	"google_calendar=", "googleRedeem",
}

func findGoogleRedirectFlow(src string) []string {
	var hits []string
	for _, tok := range removedGoogleRedirectFlow {
		if strings.Contains(src, tok) {
			hits = append(hits, tok)
		}
	}
	return hits
}

func TestClientJS_NoGoogleRedirectFlow(t *testing.T) {
	t.Run("detector fires", func(t *testing.T) {
		for _, bad := range []string{
			"window.location = '/api/v1/auth/google/start';",
			"await publicApi('POST', '/auth/google/redeem', { code });",
			"if (location.pathname === '/google-sign-in') {",
			"location.search = '?google_calendar=connected';",
			"export async function googleRedeem(code) {",
		} {
			if len(findGoogleRedirectFlow(bad)) == 0 {
				t.Errorf("expected detector to flag %q", bad)
			}
		}
		if hits := findGoogleRedirectFlow("await publicApi('POST', '/auth/google', { id_token: idToken });"); len(hits) != 0 {
			t.Errorf("detector flagged the SDK sign-in route: %v", hits)
		}
	})
	for name, src := range clientJS(t) {
		if hits := findGoogleRedirectFlow(src); len(hits) != 0 {
			t.Errorf("%s still references the Google redirect flow: %v", name, hits)
		}
	}
}

// pinToStorage matches a statement that hands something PIN-named to browser
// storage, a cookie or the URL.
var pinToStorage = regexp.MustCompile(`(?i)(localStorage|sessionStorage|document\.cookie|history\.(push|replace)State|location\.(href|search|hash))[^;\n]*pin`)

func TestClientJS_PinsNeverPersisted(t *testing.T) {
	t.Run("detector fires", func(t *testing.T) {
		if !pinToStorage.MatchString("sessionStorage.setItem('chorus.pin', activeMember.pin);") {
			t.Fatal("expected detector to flag a PIN written to sessionStorage")
		}
		if !pinToStorage.MatchString("document.cookie = `adminPin=${adminPin}`;") {
			t.Fatal("expected detector to flag a PIN written to a cookie")
		}
	})
	for name, src := range clientJS(t) {
		if m := pinToStorage.FindString(src); m != "" {
			t.Errorf("%s persists a PIN: %q", name, m)
		}
	}
	app := readStatic(t, "js/app.js")
	// Switching household must clear the active member and admin unlock.
	sel := regexp.MustCompile(`(?s)function selectHousehold\(h\) \{\s*clearMemberState\(\);`)
	if !sel.MatchString(app) {
		t.Error("expected selectHousehold to start with clearMemberState()")
	}
}

func TestAppJS_TagPickerNamesFamilyMembers(t *testing.T) {
	app := readStatic(t, "js/app.js")
	for _, want := range []string{"'Tag family members'", "tagSet.has(m.member_id)", "id: m.member_id", "let labelText = m.name;"} {
		if !strings.Contains(app, want) {
			t.Errorf("expected js/app.js to contain %q", want)
		}
	}
	if strings.Contains(app, "Tag household members") {
		t.Error("js/app.js still says 'Tag household members'")
	}
}

// The admin PIN travels only in the X-Admin-PIN header, and api() passes extra
// headers to both the first request and the token_expired retry.
func TestClientJS_AdminPinHeader(t *testing.T) {
	api := readStatic(t, "js/api.js")
	if n := strings.Count(api, "{ auth: true, headers }"); n != 2 {
		t.Errorf("expected api() to pass headers on the first attempt and the retry, found %d", n)
	}
	app := readStatic(t, "js/app.js")
	if !strings.Contains(app, "{ headers: { 'X-Admin-PIN': adminPin } }") {
		t.Error("expected admin-gated requests to send X-Admin-PIN")
	}
	if strings.Contains(app, "admin_pin:") {
		t.Error("the admin PIN must never travel in a request body")
	}
}

func TestClientJS_QuestsTab(t *testing.T) {
	app := readStatic(t, "js/app.js")

	t.Run("questsTab defined and routed", func(t *testing.T) {
		if !strings.Contains(app, "async function questsTab(") {
			t.Error("expected async function questsTab(")
		}
		if !strings.Contains(app, "quests: questsTab") {
			t.Error("expected loaders to map quests to questsTab")
		}
	})

	t.Run("empty state message", func(t *testing.T) {
		want := "No quests yet. Add one below or browse the library."
		if !strings.Contains(app, want) {
			t.Errorf("expected empty state %q", want)
		}
		if strings.Contains(app, "Nothing here.") {
			t.Error("found retired string 'Nothing here.'")
		}
	})

	t.Run("quest fields and labels", func(t *testing.T) {
		for _, field := range []string{"name: 'name'", "name: 'points'", "name: 'repeat_days'", "name: 'notes'"} {
			if !strings.Contains(app, field) {
				t.Errorf("expected quest field %s", field)
			}
		}
		if !strings.Contains(app, "label: 'Repeats after (days, back at midnight)'") {
			t.Error("expected repeat_days labelled 'Repeats after (days, back at midnight)'")
		}
		if strings.Contains(app, "until available again") {
			t.Error("expected 'until available again' to be absent")
		}
	})

	t.Run("API routes and methods", func(t *testing.T) {
		checks := []struct {
			name string
			want string
		}{
			{"list quests", "api('GET', `/households/${h.id}/quests?limit=100`)"},
			{"list library", "api('GET', '/quest-library')"},
			{"create quest", "adminRequest('POST', `/households/${h.id}/quests`, toBody(v))"},
			{"update quest", "adminRequest('PUT', `/households/${h.id}/quests/${q.id}`, toBody(v))"},
			{"delete quest", "adminRequest('DELETE', `/households/${h.id}/quests/${q.id}`)"},
			{"from library", "adminRequest('POST', `/households/${h.id}/quests/from-library`, { library_id: item.id })"},
		}
		for _, c := range checks {
			if !strings.Contains(app, c.want) {
				t.Errorf("%s: expected app.js to contain %q", c.name, c.want)
			}
		}
	})

	t.Run("library is read-only", func(t *testing.T) {
		if strings.Contains(app, "POST', '/quest-library") ||
			strings.Contains(app, "PUT', '/quest-library") ||
			strings.Contains(app, "DELETE', '/quest-library") {
			t.Error("quest-library must not be modified by client")
		}
	})

	t.Run("managementUnlocked gating", func(t *testing.T) {
		if !strings.Contains(app, "const unlocked = managementUnlocked();") {
			t.Error("expected questsTab to check managementUnlocked()")
		}
	})
}
