//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_QuestCompletionsMigration proves CLM-10:
// Seed pending quests at the prior version in a non-UTC household.
// Up: completions pending with same done_at/member, quests freed with local-midnight available_at, points unchanged.
// Down: unheld quest restored with oldest pending completion, quest taken by another member since keeps its holder and gains no done_at.
func TestIntegration_QuestCompletionsMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	upSQL := readMigration(t, "000014_quest_completions_pending.up.sql")
	downSQL := readMigration(t, "000014_quest_completions_pending.down.sql")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

	newMember := func(householdID uuid.UUID, name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := tx.Exec(ctx, "INSERT INTO family_members (id, household_id, name, points) VALUES ($1, $2, $3, 10)", id, householdID, name); err != nil {
			t.Fatalf("insert member %s: %v", name, err)
		}
		return id
	}

	// 1. Setup in migration 13 state (down 14 if 14 was applied)
	// We run downSQL first to ensure we are at state 13 in case the database was migrated to 14.
	// But let's check if 14 is already applied by checking if quest_next_available_at exists or status column exists.
	var hasStatus bool
	_ = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'quest_completions' AND column_name = 'status')").Scan(&hasStatus)
	if hasStatus {
		if _, err := tx.Exec(ctx, downSQL); err != nil {
			t.Fatalf("run down to state 13: %v", err)
		}
	}

	hhID := newHousehold("Chicago Fam", "America/Chicago")
	annID := newMember(hhID, "Ann")
	bobID := newMember(hhID, "Bob")

	// Seed pending quests at version 13 in a non-UTC household
	// Quest 1: pending approval held by Ann
	doneAt1 := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC) // 12:00 Chicago
	q1ID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_quests (id, household_id, name, repeat_days, points, claimed_by, claimed_at, done_at)
		VALUES ($1, $2, 'Dishes', 2, 5, $3, $4, $4)`,
		q1ID, hhID, annID, doneAt1); err != nil {
		t.Fatalf("insert q1: %v", err)
	}

	// Quest 2: another pending quest held by Ann
	doneAt2 := time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)
	q2ID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_quests (id, household_id, name, repeat_days, points, claimed_by, claimed_at, done_at)
		VALUES ($1, $2, 'Trash', 1, 3, $3, $4, $4)`,
		q2ID, hhID, annID, doneAt2); err != nil {
		t.Fatalf("insert q2: %v", err)
	}

	// Run up migration
	if _, err := tx.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run up migration: %v", err)
	}

	// Verify up migration results:
	// - completions pending with same done_at/member
	var compCount int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM quest_completions WHERE household_id = $1 AND status = 'pending'", hhID).Scan(&compCount); err != nil || compCount != 2 {
		t.Fatalf("pending completions count = %d (want 2), err = %v", compCount, err)
	}

	type compRow struct {
		questID  uuid.UUID
		memberID uuid.UUID
		doneAt   time.Time
		status   string
	}
	rows, err := tx.Query(ctx, "SELECT quest_id, member_id, done_at, status FROM quest_completions WHERE household_id = $1 ORDER BY done_at ASC", hhID)
	if err != nil {
		t.Fatalf("query completions: %v", err)
	}
	defer rows.Close()
	var comps []compRow
	for rows.Next() {
		var c compRow
		if err := rows.Scan(&c.questID, &c.memberID, &c.doneAt, &c.status); err != nil {
			t.Fatalf("scan comp: %v", err)
		}
		comps = append(comps, c)
	}
	if len(comps) != 2 || comps[0].questID != q1ID || comps[0].memberID != annID || !comps[0].doneAt.Equal(doneAt1) ||
		comps[1].questID != q2ID || comps[1].memberID != annID || !comps[1].doneAt.Equal(doneAt2) {
		t.Fatalf("unexpected completions after up: %+v", comps)
	}

	// - quests freed with local-midnight available_at, completed_at set, claimed_by NULL, done_at NULL
	var q1ClaimedBy *uuid.UUID
	var q1DoneAt, q1CompletedAt, q1AvailableAt *time.Time
	if err := tx.QueryRow(ctx, "SELECT claimed_by, done_at, completed_at, available_at FROM household_quests WHERE id = $1", q1ID).
		Scan(&q1ClaimedBy, &q1DoneAt, &q1CompletedAt, &q1AvailableAt); err != nil {
		t.Fatalf("query q1: %v", err)
	}
	if q1ClaimedBy != nil || q1DoneAt != nil {
		t.Errorf("q1 not freed: claimed_by %v, done_at %v", q1ClaimedBy, q1DoneAt)
	}
	if q1CompletedAt == nil || !q1CompletedAt.Equal(doneAt1) {
		t.Errorf("q1 completed_at = %v, want %v", q1CompletedAt, doneAt1)
	}
	// repeat_days = 2, doneAt1 = 2026-11-15 12:00 Chicago -> local date Nov 15 + 2 days = Nov 17 00:00 Chicago
	wantAvail1 := time.Date(2026, 11, 17, 0, 0, 0, 0, chicago)
	if q1AvailableAt == nil || !q1AvailableAt.Equal(wantAvail1) {
		t.Errorf("q1 available_at = %v, want %v", q1AvailableAt, wantAvail1)
	}

	// - points unchanged
	var annPoints int32
	if err := tx.QueryRow(ctx, "SELECT points FROM family_members WHERE id = $1", annID).Scan(&annPoints); err != nil || annPoints != 10 {
		t.Errorf("ann points = %d, want 10", annPoints)
	}

	// Now simulate before down migration:
	// q1 remains unheld.
	// q2 is taken by Bob since the up migration (claimed_by = Bob, claimed_at = NOW()).
	// We also insert a second pending completion for q1 to test that down restores the OLDEST pending completion.
	doneAt1Newer := doneAt1.Add(time.Hour)
	if _, err := tx.Exec(ctx, `
		INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status)
		VALUES ($1, $2, $3, $4, $5, 'pending')`,
		uuid.New(), hhID, q1ID, annID, doneAt1Newer); err != nil {
		t.Fatalf("insert second pending completion for q1: %v", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE household_quests
		SET claimed_by = $2, claimed_at = NOW(), updated_at = NOW()
		WHERE id = $1`, q2ID, bobID); err != nil {
		t.Fatalf("bob claims q2: %v", err)
	}

	// Run down migration
	if _, err := tx.Exec(ctx, downSQL); err != nil {
		t.Fatalf("run down migration: %v", err)
	}

	// Verify down migration results:
	// - unheld quest q1 restored with oldest pending completion (claimed_by = Ann, done_at = doneAt1)
	var q1DownClaimedBy *uuid.UUID
	var q1DownDoneAt *time.Time
	if err := tx.QueryRow(ctx, "SELECT claimed_by, done_at FROM household_quests WHERE id = $1", q1ID).
		Scan(&q1DownClaimedBy, &q1DownDoneAt); err != nil {
		t.Fatalf("query q1 after down: %v", err)
	}
	if q1DownClaimedBy == nil || *q1DownClaimedBy != annID {
		t.Errorf("q1 claimed_by after down = %v, want %v", q1DownClaimedBy, annID)
	}
	if q1DownDoneAt == nil || !q1DownDoneAt.Equal(doneAt1) {
		t.Errorf("q1 done_at after down = %v, want oldest %v", q1DownDoneAt, doneAt1)
	}

	// - quest q2 taken by Bob since keeps its holder (Bob) and gains no done_at
	var q2DownClaimedBy *uuid.UUID
	var q2DownDoneAt *time.Time
	if err := tx.QueryRow(ctx, "SELECT claimed_by, done_at FROM household_quests WHERE id = $1", q2ID).
		Scan(&q2DownClaimedBy, &q2DownDoneAt); err != nil {
		t.Fatalf("query q2 after down: %v", err)
	}
	if q2DownClaimedBy == nil || *q2DownClaimedBy != bobID {
		t.Errorf("q2 claimed_by after down = %v, want Bob %v", q2DownClaimedBy, bobID)
	}
	if q2DownDoneAt != nil {
		t.Errorf("q2 done_at after down = %v, want nil", q2DownDoneAt)
	}

	// - unrestored pending completions are dropped, status column dropped
	var statusColExists bool
	_ = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'quest_completions' AND column_name = 'status')").Scan(&statusColExists)
	if statusColExists {
		t.Errorf("status column still exists after down")
	}
}
