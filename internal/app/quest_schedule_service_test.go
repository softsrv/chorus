package app

import (
	"testing"
	"time"
)

func TestNextDate(t *testing.T) {
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// 2026-11-20 11:00 UTC is 2026-11-21 00:00 in Auckland (NZDT, +13):
	// the local today differs from the UTC date.
	now := time.Date(2026, 11, 20, 11, 30, 0, 0, time.UTC)
	date := func(s string) *time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return &d
	}
	// available_at at 2026-11-24 10:30 UTC is 2026-11-24 23:30 local.
	resting := time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC)
	// available_at at 2026-11-24 11:30 UTC is 2026-11-25 00:30 local.
	restingPastLocalMidnight := time.Date(2026, 11, 24, 11, 30, 0, 0, time.UTC)

	cases := []struct {
		name         string
		start        string
		repeat       bool
		availableAt  *time.Time
		lastRollover *time.Time
		want         string
		wantOK       bool
	}{
		{name: "one-off in the future shows its date", start: "2026-11-23", want: "2026-11-23", wantOK: true},
		{name: "one-off today (local) is listed", start: "2026-11-21", want: "2026-11-21", wantOK: true},
		{name: "one-off dated the UTC today but before the local today is not listed", start: "2026-11-20", wantOK: false},
		{name: "one-off ignores rollover and rest", start: "2026-11-22", availableAt: &resting, lastRollover: date("2026-11-21"), want: "2026-11-22", wantOK: true},
		{name: "repeat starting later shows its start", start: "2026-11-25", repeat: true, lastRollover: date("2026-11-21"), want: "2026-11-25", wantOK: true},
		{name: "repeat whose today has rolled over shows tomorrow", start: "2026-11-01", repeat: true, lastRollover: date("2026-11-21"), want: "2026-11-22", wantOK: true},
		{name: "repeat whose today has not rolled over shows today", start: "2026-11-01", repeat: true, lastRollover: date("2026-11-20"), want: "2026-11-21", wantOK: true},
		{name: "repeat in a household that never rolled over shows today", start: "2026-11-01", repeat: true, want: "2026-11-21", wantOK: true},
		{name: "repeat resting shows the local date its rest ends", start: "2026-11-01", repeat: true, availableAt: &resting, lastRollover: date("2026-11-21"), want: "2026-11-24", wantOK: true},
		{name: "rest ending after local midnight is the next local day", start: "2026-11-01", repeat: true, availableAt: &restingPastLocalMidnight, lastRollover: date("2026-11-21"), want: "2026-11-25", wantOK: true},
		{name: "rest already over leaves the rollover date", start: "2026-11-01", repeat: true, availableAt: date("2026-11-10"), lastRollover: date("2026-11-21"), want: "2026-11-22", wantOK: true},
		{name: "malformed start date", start: "21/11/2026", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NextDate(tc.start, tc.repeat, tc.availableAt, tc.lastRollover, now, auckland)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("NextDate(%q, repeat=%v) = %q, %v; want %q, %v", tc.start, tc.repeat, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
