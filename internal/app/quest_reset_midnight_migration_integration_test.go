//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_QuestsResetAtMidnightMigration proves CLM-11: migration
// 000012 moves every resting quest (available_at in the future, completed_at
// set) to 00:00 in its household's timezone on completed_at's local day +
// repeat_days, and leaves expired and never-completed quests alone; the down
// migration puts resting quests back on completed_at + repeat_days.
//
// Both migrations update every row in the table, so the whole test runs in
// one transaction that is rolled back: other tests' rows are never rewritten.
func TestIntegration_QuestsResetAtMidnightMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	upSQL := readMigration(t, "000012_quests_reset_at_midnight.up.sql")
	downSQL := readMigration(t, "000012_quests_reset_at_midnight.down.sql")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The down migration adds calendar days in the session zone; pin it so
	// "repeat_days days" is exactly repeat_days*24h.
	if _, err := tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"); err != nil {
		t.Fatalf("set time zone: %v", err)
	}

	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}

	newHousehold := func(name, tz string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := tx.Exec(ctx, "INSERT INTO households (id, name, timezone) VALUES ($1, $2, $3)", id, name, tz); err != nil {
			t.Fatalf("insert household %s: %v", name, err)
		}
		return id
	}
	newQuest := func(householdID uuid.UUID, repeatDays int, completedAt, availableAt *time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO household_quests (id, household_id, name, repeat_days, completed_at, available_at)
			VALUES ($1, $2, 'Feed the cat', $3, $4, $5)`,
			id, householdID, repeatDays, completedAt, availableAt); err != nil {
			t.Fatalf("insert quest: %v", err)
		}
		return id
	}
	type state struct {
		completedAt, availableAt, updatedAt *time.Time
	}
	read := func(id uuid.UUID) state {
		t.Helper()
		var s state
		if err := tx.QueryRow(ctx,
			"SELECT completed_at, available_at, updated_at FROM household_quests WHERE id = $1", id,
		).Scan(&s.completedAt, &s.availableAt, &s.updatedAt); err != nil {
			t.Fatalf("read quest %s: %v", id, err)
		}
		return s
	}
	sameTime := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return a.Equal(*b)
	}
	unchanged := func(label string, id uuid.UUID, want state) {
		t.Helper()
		got := read(id)
		if !sameTime(got.completedAt, want.completedAt) || !sameTime(got.availableAt, want.availableAt) ||
			!sameTime(got.updatedAt, want.updatedAt) {
			t.Errorf("%s changed: got %+v, want %+v", label, got, want)
		}
	}
	at := func(tm time.Time) *time.Time { return &tm }

	utcHH := newHousehold("UTC Family", "UTC")
	chiHH := newHousehold("Chicago Family", "America/Chicago")

	// Yesterday 03:00 UTC is always in the past. Its local date is yesterday
	// in UTC but the day before in Chicago (21:00 or 22:00 local), so the two
	// households must land on different midnights. With repeat_days = 5 both
	// returns, before and after the up migration, stay in the future, so the
	// rows still count as resting for the down migration.
	now := time.Now().UTC()
	completed := time.Date(now.Year(), now.Month(), now.Day()-1, 3, 0, 0, 0, time.UTC)
	const repeatDays = 5
	oldReturn := completed.Add(repeatDays * 24 * time.Hour)

	utcResting := newQuest(utcHH, repeatDays, at(completed), at(oldReturn))
	chiResting := newQuest(chiHH, repeatDays, at(completed), at(oldReturn))
	// Back already: its available_at is in the past.
	expiredDone := now.Add(-25 * time.Hour).Truncate(time.Second)
	expired := newQuest(chiHH, 1, at(expiredDone), at(expiredDone.Add(24*time.Hour)))
	// Never completed: both timestamps NULL.
	never := newQuest(chiHH, 3, nil, nil)

	expiredBefore := read(expired)
	neverBefore := read(never)

	localMidnight := func(done time.Time, loc *time.Location, days int) time.Time {
		l := done.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day()+days, 0, 0, 0, 0, loc)
	}
	wantUTC := localMidnight(completed, time.UTC, repeatDays)
	wantChi := localMidnight(completed, chicago, repeatDays)
	if wantUTC.Equal(wantChi) {
		t.Fatalf("seed does not separate the households: both %v", wantUTC)
	}

	if _, err := tx.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run 000012 up: %v", err)
	}

	for _, c := range []struct {
		label string
		id    uuid.UUID
		want  time.Time
	}{
		{"UTC resting quest", utcResting, wantUTC},
		{"Chicago resting quest", chiResting, wantChi},
	} {
		got := read(c.id)
		if got.availableAt == nil || !got.availableAt.Equal(c.want) {
			t.Errorf("after up, %s available_at = %v, want %v", c.label, got.availableAt, c.want)
		}
		if got.completedAt == nil || !got.completedAt.Equal(completed) {
			t.Errorf("after up, %s completed_at = %v, want %v", c.label, got.completedAt, completed)
		}
	}
	unchanged("after up, expired quest", expired, expiredBefore)
	unchanged("after up, never-completed quest", never, neverBefore)

	if _, err := tx.Exec(ctx, downSQL); err != nil {
		t.Fatalf("run 000012 down: %v", err)
	}

	for _, c := range []struct {
		label string
		id    uuid.UUID
	}{
		{"UTC resting quest", utcResting},
		{"Chicago resting quest", chiResting},
	} {
		got := read(c.id)
		if got.availableAt == nil || !got.availableAt.Equal(oldReturn) {
			t.Errorf("after down, %s available_at = %v, want %v", c.label, got.availableAt, oldReturn)
		}
	}
	unchanged("after down, expired quest", expired, expiredBefore)
	unchanged("after down, never-completed quest", never, neverBefore)
}
