//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_QuestSchedulesMigration proves CLM-8 at the schema:
// deleting a quest or a family member removes its quest_schedules rows, and a
// later rollover assigns nothing for them (a control schedule on a surviving
// quest and member is still assigned, so the rollover is not simply idle);
// the composite foreign keys reject a quest or a member of another
// household; down drops quest_schedules and household_quest_rollovers and up
// applies again. The data is committed, because the rollover opens its own
// transactions, and is removed with its households when the test ends; the
// down and up steps run in one transaction that is rolled back.
func TestIntegration_QuestSchedulesMigration(t *testing.T) {
	pool := testDB(t)
	q := db.New(pool)
	ctx := context.Background()
	upSQL := readMigration(t, "000022_quest_schedules.up.sql")
	downSQL := readMigration(t, "000022_quest_schedules.down.sql")

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	hhID, otherHH := uuid.New(), uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM households WHERE id = ANY($1)", []uuid.UUID{hhID, otherHH})
	})
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Schedule Mig Fam', 'UTC')", hhID)
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Schedule Mig Other', 'UTC')", otherHH)

	newMember := func(hh uuid.UUID, name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec("INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, $3)", id, hh, name)
		return id
	}
	newQuest := func(hh uuid.UUID, name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec("INSERT INTO household_quests (id, household_id, name) VALUES ($1, $2, $3)", id, hh, name)
		return id
	}
	// A schedule dated yesterday is due at any rollover run today.
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	schedule := func(hh, quest, member uuid.UUID) error {
		_, err := pool.Exec(ctx, `INSERT INTO quest_schedules (id, household_id, quest_id, member_id, start_date)
			VALUES ($1, $2, $3, $4, $5::date)`, uuid.New(), hh, quest, member, yesterday)
		return err
	}
	count := func(where string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM quest_schedules WHERE "+where, args...).Scan(&n); err != nil {
			t.Fatalf("count schedules: %v", err)
		}
		return n
	}

	memberGone, memberSurvivor, memberControl := newMember(hhID, "Gone"), newMember(hhID, "Survivor"), newMember(hhID, "Control")
	questGone, questSurvivor, questControl := newQuest(hhID, "Quest gone"), newQuest(hhID, "Quest survivor"), newQuest(hhID, "Quest control")
	for _, s := range [][2]uuid.UUID{{questGone, memberSurvivor}, {questSurvivor, memberGone}, {questControl, memberControl}} {
		if err := schedule(hhID, s[0], s[1]); err != nil {
			t.Fatalf("insert schedule: %v", err)
		}
	}

	// A quest or a member of another household cannot be named.
	otherQuest, otherMember := newQuest(otherHH, "Other quest"), newMember(otherHH, "Other member")
	for label, args := range map[string][2]uuid.UUID{
		"quest of another household":  {otherQuest, memberSurvivor},
		"member of another household": {questSurvivor, otherMember},
	} {
		err := schedule(hhID, args[0], args[1])
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Errorf("%s: got %v, want a foreign key violation", label, err)
		}
	}
	if n := count("household_id = $1", hhID); n != 3 {
		t.Fatalf("schedules after rejected inserts = %d, want 3", n)
	}

	// Deleting the quest, then the member, removes their schedules only.
	mustExec("DELETE FROM household_quests WHERE id = $1", questGone)
	if n := count("quest_id = $1", questGone); n != 0 {
		t.Errorf("schedules of the deleted quest = %d, want 0", n)
	}
	if n := count("household_id = $1", hhID); n != 2 {
		t.Errorf("schedules after quest delete = %d, want 2", n)
	}
	mustExec("DELETE FROM family_members WHERE id = $1", memberGone)
	if n := count("member_id = $1", memberGone); n != 0 {
		t.Errorf("schedules of the deleted member = %d, want 0", n)
	}
	if n := count("household_id = $1", hhID); n != 1 {
		t.Errorf("schedules after member delete = %d, want 1 (the control)", n)
	}

	// The rollover assigns only the control; nothing fires for the deleted.
	assigned, err := app.RolloverDueHouseholds(ctx, q, pool, time.Now())
	if err != nil {
		t.Fatalf("rollover: %v", err)
	}
	if assigned < 1 {
		t.Errorf("rollover assigned %d, want the control quest at least", assigned)
	}
	claimedBy := func(quest uuid.UUID) *uuid.UUID {
		t.Helper()
		var by *uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT claimed_by FROM household_quests WHERE id = $1", quest).Scan(&by); err != nil {
			t.Fatalf("read quest %s: %v", quest, err)
		}
		return by
	}
	if by := claimedBy(questControl); by == nil || *by != memberControl {
		t.Errorf("control quest claimed_by = %v, want %s", by, memberControl)
	}
	if by := claimedBy(questSurvivor); by != nil {
		t.Errorf("quest of the deleted member claimed_by = %v, want nil", by)
	}

	// Down drops both tables and up applies again; rolled back after.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	registered := func(table string) *string {
		t.Helper()
		var reg *string
		if err := tx.QueryRow(ctx, "SELECT to_regclass($1)::text", table).Scan(&reg); err != nil {
			t.Fatalf("to_regclass %s: %v", table, err)
		}
		return reg
	}
	if _, err := tx.Exec(ctx, downSQL); err != nil {
		t.Fatalf("down: %v", err)
	}
	for _, table := range []string{"quest_schedules", "household_quest_rollovers"} {
		if registered(table) != nil {
			t.Errorf("%s still exists after down", table)
		}
	}
	if _, err := tx.Exec(ctx, upSQL); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	for _, table := range []string{"quest_schedules", "household_quest_rollovers"} {
		if registered(table) == nil {
			t.Errorf("%s missing after re-up", table)
		}
	}
}
