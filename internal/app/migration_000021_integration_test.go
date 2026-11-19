//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_QuestClaimExpiryMigration proves CLM-7 and CLM-11 at the
// schema: quest_claim_expires_at returns the local midnight that ends the
// claimed day (a quest accepted at 23:59 ends a minute later, two time zones
// differ for the same instant, and the 25-hour fall-back day ends at the right
// instant); applying 000021 over existing data gives every held quest the
// deadline of the first local midnight after the migration in its household's
// zone and leaves unheld quests NULL and every other column as it was; down
// drops the column and the function. Everything runs in one transaction that
// is rolled back.
func TestIntegration_QuestClaimExpiryMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	upSQL := readMigration(t, "000021_quest_claim_expiry.up.sql")
	downSQL := readMigration(t, "000021_quest_claim_expiry.down.sql")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Start from 000020: the down migration is safe when 000021 is absent.
	mustExec(downSQL)

	// Data that exists before 000021: two households in different zones, one
	// held quest in each, one unheld quest.
	chicagoHH, aucklandHH := uuid.New(), uuid.New()
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Chicago Claim Mig', 'America/Chicago')", chicagoHH)
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Auckland Claim Mig', 'Pacific/Auckland')", aucklandHH)
	chicagoMember, aucklandMember := uuid.New(), uuid.New()
	mustExec("INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, 'Sam')", chicagoMember, chicagoHH)
	mustExec("INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, 'Aroha')", aucklandMember, aucklandHH)
	claimedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newQuest := func(hh uuid.UUID, member *uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var at any
		if member != nil {
			at = claimedAt
		}
		mustExec(`INSERT INTO household_quests (id, household_id, name, claimed_by, claimed_at)
			VALUES ($1, $2, 'Feed the cat', $3, $4)`, id, hh, member, at)
		return id
	}
	chicagoHeld := newQuest(chicagoHH, &chicagoMember)
	aucklandHeld := newQuest(aucklandHH, &aucklandMember)
	unheld := newQuest(chicagoHH, nil)

	type before struct {
		name        string
		claimedBy   *uuid.UUID
		claimedAt   *time.Time
		available   *time.Time
		repeatDays  int
		createdAt   time.Time
		updatedAt   time.Time
		householdID uuid.UUID
	}
	read := func(id uuid.UUID) before {
		t.Helper()
		var b before
		if err := tx.QueryRow(ctx, `SELECT name, claimed_by, claimed_at, available_at, repeat_days, created_at, updated_at, household_id
			FROM household_quests WHERE id = $1`, id).
			Scan(&b.name, &b.claimedBy, &b.claimedAt, &b.available, &b.repeatDays, &b.createdAt, &b.updatedAt, &b.householdID); err != nil {
			t.Fatalf("read quest %s: %v", id, err)
		}
		return b
	}
	preserved := map[uuid.UUID]before{chicagoHeld: read(chicagoHeld), aucklandHeld: read(aucklandHeld), unheld: read(unheld)}

	mustExec(upSQL)

	// CLM-7: the function, with exact instants.
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatalf("load Pacific/Auckland: %v", err)
	}
	expiresAt := func(at time.Time, tz string) time.Time {
		t.Helper()
		var got time.Time
		if err := tx.QueryRow(ctx, "SELECT quest_claim_expires_at($1, $2)", at, tz).Scan(&got); err != nil {
			t.Fatalf("quest_claim_expires_at(%s, %s): %v", at, tz, err)
		}
		return got
	}
	for _, c := range []struct {
		name string
		at   time.Time
		tz   string
		want time.Time
	}{
		{"23:59 local ends one minute later", time.Date(2026, 6, 15, 4, 59, 0, 0, time.UTC), "America/Chicago", time.Date(2026, 6, 15, 5, 0, 0, 0, time.UTC)},
		{"same instant in Auckland is the next local day", time.Date(2026, 6, 15, 4, 59, 0, 0, time.UTC), "Pacific/Auckland", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)},
		{"exactly local midnight starts a new day", time.Date(2026, 6, 15, 5, 0, 0, 0, time.UTC), "America/Chicago", time.Date(2026, 6, 16, 5, 0, 0, 0, time.UTC)},
		{"Chicago fall-back day is 25 hours long", time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), "America/Chicago", time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)},
		{"Auckland summer time (UTC+13) day", time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC), "Pacific/Auckland", time.Date(2026, 3, 15, 11, 0, 0, 0, time.UTC)},
		{"UTC", time.Date(2026, 5, 5, 17, 45, 0, 0, time.UTC), "UTC", time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)},
	} {
		if got := expiresAt(c.at, c.tz); !got.Equal(c.want) {
			t.Errorf("%s: got %s, want %s", c.name, got.UTC(), c.want)
		}
	}

	// CLM-11: the backfill. NOW() is one instant for the transaction.
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT NOW()").Scan(&now); err != nil {
		t.Fatalf("now: %v", err)
	}
	localMidnightAfter := func(loc *time.Location) time.Time {
		l := now.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc)
	}
	deadline := func(id uuid.UUID) *time.Time {
		t.Helper()
		var d *time.Time
		if err := tx.QueryRow(ctx, "SELECT claim_expires_at FROM household_quests WHERE id = $1", id).Scan(&d); err != nil {
			t.Fatalf("read claim_expires_at %s: %v", id, err)
		}
		return d
	}
	for _, c := range []struct {
		name string
		id   uuid.UUID
		loc  *time.Location
	}{{"Chicago held", chicagoHeld, chicago}, {"Auckland held", aucklandHeld, auckland}} {
		got, want := deadline(c.id), localMidnightAfter(c.loc)
		if got == nil || !got.Equal(want) {
			t.Errorf("%s: claim_expires_at = %v, want %s (first local midnight after the migration)", c.name, got, want)
		}
	}
	if got := deadline(unheld); got != nil {
		t.Errorf("unheld quest: claim_expires_at = %s, want NULL", got)
	}
	for id, want := range preserved {
		got := read(id)
		if got.name != want.name || got.repeatDays != want.repeatDays || got.householdID != want.householdID ||
			!got.createdAt.Equal(want.createdAt) || !got.updatedAt.Equal(want.updatedAt) ||
			(got.claimedBy == nil) != (want.claimedBy == nil) || (got.claimedBy != nil && *got.claimedBy != *want.claimedBy) ||
			(got.claimedAt == nil) != (want.claimedAt == nil) || (got.claimedAt != nil && !got.claimedAt.Equal(*want.claimedAt)) ||
			(got.available == nil) != (want.available == nil) {
			t.Errorf("quest %s changed by the migration: got %+v, want %+v", id, got, want)
		}
	}

	// Down: the column and the function go; the held quest stays held.
	mustExec(downSQL)
	var cols, funcs int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'household_quests' AND column_name = 'claim_expires_at' AND table_schema = current_schema()`).Scan(&cols); err != nil {
		t.Fatalf("count columns: %v", err)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM pg_proc WHERE proname = 'quest_claim_expires_at'").Scan(&funcs); err != nil {
		t.Fatalf("count functions: %v", err)
	}
	if cols != 0 || funcs != 0 {
		t.Errorf("after down: %d claim_expires_at column(s), %d quest_claim_expires_at function(s), want none", cols, funcs)
	}
	if got := read(chicagoHeld); got.claimedBy == nil {
		t.Error("down released a held quest")
	}
	// And the migration applies again after a down.
	mustExec(upSQL)
}
