package app

import (
	"net/url"
	"sort"
	"strings"
	"testing"
)

// CLM-2: the calendar authorization URL asks for exactly the calendar list
// and events scopes, offline access and forced consent.
func TestGoogleCalendar_AuthCodeURLScopeSet(t *testing.T) {
	g := NewGoogleCalendar(nil, nil, GoogleCalendarConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://chorus.test/api/v1/me/google-calendar/callback",
	})

	raw := g.AuthCodeURL("some-state", "some-verifier-some-verifier-some-verifier-12")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth URL %q: %v", raw, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != googleAuthURL {
		t.Errorf("auth endpoint = %q, want %q", got, googleAuthURL)
	}
	q := u.Query()
	scopes := strings.Fields(q.Get("scope"))
	sort.Strings(scopes)
	want := []string{
		"https://www.googleapis.com/auth/calendar.calendarlist.readonly",
		"https://www.googleapis.com/auth/calendar.events",
	}
	if strings.Join(scopes, " ") != strings.Join(want, " ") {
		t.Errorf("scope set = %v, want exactly %v", scopes, want)
	}
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Errorf("access_type=%q prompt=%q, want offline and consent", q.Get("access_type"), q.Get("prompt"))
	}
	if q.Get("redirect_uri") != "http://chorus.test/api/v1/me/google-calendar/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != "some-state" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Errorf("auth URL missing state or PKCE S256 challenge: %v", q)
	}
}

// CLM-6: the access level comes from the scopes Google actually granted.
func TestGrantedAccess(t *testing.T) {
	const (
		list     = "https://www.googleapis.com/auth/calendar.calendarlist.readonly"
		events   = "https://www.googleapis.com/auth/calendar.events"
		eventsRO = "https://www.googleapis.com/auth/calendar.events.readonly"
	)
	cases := []struct {
		scope  string
		access string
		ok     bool
	}{
		{list + " " + events, CalendarReadWrite, true},
		{events + " " + list + " openid", CalendarReadWrite, true},
		{"https://www.googleapis.com/auth/calendar", CalendarReadWrite, true},
		{list + " " + eventsRO, CalendarReadOnly, true},
		{list, CalendarReadOnly, true},
		{eventsRO, CalendarReadOnly, true},
		// events without the calendar list can't pick a calendar: read_only.
		{events, CalendarReadOnly, true},
		{"openid email", "", false},
		{"", "", false},
		// A look-alike that is not a Google calendar scope grants nothing.
		{"https://www.googleapis.com/auth/calendarx", "", false},
	}
	for _, tc := range cases {
		access, ok := grantedAccess(tc.scope)
		if access != tc.access || ok != tc.ok {
			t.Errorf("grantedAccess(%q) = (%q, %v), want (%q, %v)", tc.scope, access, ok, tc.access, tc.ok)
		}
	}
}
