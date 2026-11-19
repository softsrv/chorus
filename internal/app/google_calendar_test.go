package app

import (
	"testing"
)

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
