package app_test

import (
	"testing"
	"time"

	"github.com/softsrv/chorus/internal/app"
)

// TestNextScheduleDate pins the date an Upcoming item shows: a one-off is its
// start date; a repeated schedule is the latest of its start date, the next
// local day not yet rolled over and the local date its quest rests until,
// counted on the calendar in the household's zone.
func TestNextScheduleDate(t *testing.T) {
	load := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		return loc
	}
	auckland, chicago := load("Pacific/Auckland"), load("America/Chicago")
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	at := func(loc *time.Location, y int, m time.Month, d, h, min int) *time.Time {
		v := time.Date(y, m, d, h, min, 0, 0, loc)
		return &v
	}

	cases := []struct {
		name        string
		start       time.Time
		repeat      bool
		availableAt *time.Time
		rolledToday bool
		now         time.Time
		loc         *time.Location
		want        time.Time
	}{
		{"one-off is its start date", day(2026, 11, 21), false, nil, true, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 21)},
		{"one-off ignores a resting quest", day(2026, 11, 21), false, at(auckland, 2026, 11, 28, 0, 0), false, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 21)},
		{"repeat with a future start", day(2026, 11, 22), true, nil, true, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 22)},
		{"repeat past its start, today not rolled over", day(2026, 11, 10), true, nil, false, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 18)},
		{"repeat past its start, today rolled over", day(2026, 11, 10), true, nil, true, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 19)},
		{"repeat waits for the cooldown date", day(2026, 11, 10), true, at(auckland, 2026, 11, 25, 0, 0), true, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 25)},
		{"repeat with a future start is pushed by the cooldown", day(2026, 11, 20), true, at(auckland, 2026, 11, 25, 0, 0), true, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 25)},
		{"cooldown already over changes nothing", day(2026, 11, 10), true, at(auckland, 2026, 11, 17, 0, 0), true, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 19)},
		// 2026-11-18 20:00 UTC is already 19 Nov in Auckland (UTC+13).
		{"local date, not UTC, is today", day(2026, 11, 1), true, nil, false, time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC), auckland, day(2026, 11, 19)},
		// The cooldown ends at local midnight; as a UTC instant that is the
		// evening before, but the local date is the one that counts.
		{"cooldown instant is read in the household zone", day(2026, 11, 1), true, at(auckland, 2026, 11, 25, 0, 0), true, *at(auckland, 2026, 11, 18, 9, 0), auckland, day(2026, 11, 25)},
		// US spring-forward (Mar 8 2026, 23-hour day) and fall-back (Nov 1
		// 2026, 25-hour day): "tomorrow" is the next calendar day.
		{"tomorrow across spring forward", day(2026, 3, 1), true, nil, true, *at(chicago, 2026, 3, 8, 12, 0), chicago, day(2026, 3, 9)},
		{"tomorrow across fall back", day(2026, 10, 1), true, nil, true, *at(chicago, 2026, 11, 1, 12, 0), chicago, day(2026, 11, 2)},
		{"tomorrow late on the evening before fall back", day(2026, 10, 1), true, nil, true, *at(chicago, 2026, 10, 31, 23, 30), chicago, day(2026, 11, 1)},
		{"cooldown ending at the spring-forward midnight", day(2026, 3, 1), true, at(chicago, 2026, 3, 8, 0, 0), true, *at(chicago, 2026, 3, 6, 12, 0), chicago, day(2026, 3, 8)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := app.NextScheduleDate(c.start, c.repeat, c.availableAt, c.rolledToday, c.now, c.loc)
			if !got.Equal(c.want) {
				t.Errorf("next date = %s, want %s", got.Format(time.DateOnly), c.want.Format(time.DateOnly))
			}
		})
	}
}
