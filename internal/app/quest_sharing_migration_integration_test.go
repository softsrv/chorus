//go:build integration

package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_QuestSharingMigration proves CLM-15 at the schema:
// Up leaves every existing quest not shareable with a teamwork bonus of 0
// and changes no completion or point balance. Down refuses, changing
// nothing, while a pending completion covers more than one member; once it
// is resolved, down drops the participant tables and the two columns and
// keeps every completion and balance. Everything runs in one transaction
// that is rolled back, so the database is left at its current version.
func TestIntegration_QuestSharingMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	upSQL := readMigration(t, "000015_quest_sharing.up.sql")
	downSQL := readMigration(t, "000015_quest_sharing.down.sql")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	columnExists := func(table, column string) bool {
		t.Helper()
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`, table, column).Scan(&ok); err != nil {
			t.Fatalf("column %s.%s: %v", table, column, err)
		}
		return ok
	}
	tableExists := func(table string) bool {
		t.Helper()
		var ok bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&ok); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
		return ok
	}

	// 1. Back to version 14 if 15 is applied. Other tests may have left
	//    shared pending completions behind; inside this rolled-back
	//    transaction their extra members are set aside so down can run.
	if columnExists("household_quests", "shareable") {
		if _, err := tx.Exec(ctx, `DELETE FROM quest_completion_members m
			USING quest_completions c
			WHERE c.household_id = m.household_id AND c.id = m.completion_id AND c.status = 'pending'`); err != nil {
			t.Fatalf("set aside pending completion members: %v", err)
		}
		if _, err := tx.Exec(ctx, downSQL); err != nil {
			t.Fatalf("run down to version 14: %v", err)
		}
	}
	if columnExists("household_quests", "shareable") || tableExists("quest_completion_members") || tableExists("household_quest_participants") {
		t.Fatal("not at version 14 before seeding")
	}

	// 2. Seed at version 14: two members with points, a quest with an
	//    approved and a pending completion.
	hhID := uuid.New()
	// The quest and completion seeded in step 2 with no marker; completions()
	// leaves them out, since up cancels that completion.
	orphanQuestID, orphanID := uuid.New(), uuid.New()
	if _, err := tx.Exec(ctx, "INSERT INTO households (id, name, timezone) VALUES ($1, 'Sharing Fam', 'America/Chicago')", hhID); err != nil {
		t.Fatalf("insert household: %v", err)
	}
	newMember := func(name string, points int32) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := tx.Exec(ctx, "INSERT INTO family_members (id, household_id, name, points) VALUES ($1, $2, $3, $4)", id, hhID, name, points); err != nil {
			t.Fatalf("insert member %s: %v", name, err)
		}
		return id
	}
	annID := newMember("Ann", 17)
	bobID := newMember("Bob", 40)
	questID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO household_quests (id, household_id, name, points, repeat_days)
		VALUES ($1, $2, 'Clean the living room', 100, 7)`, questID, hhID); err != nil {
		t.Fatalf("insert quest: %v", err)
	}
	doneAt := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)
	approvedID, pendingID := uuid.New(), uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, approved_at)
		VALUES ($1, $3, $4, $5, $6, 'approved', $6), ($2, $3, $4, $5, $6 + interval '1 day', 'pending', NULL)`,
		approvedID, pendingID, hhID, questID, annID, doneAt); err != nil {
		t.Fatalf("insert completions: %v", err)
	}

	type completion struct {
		id       uuid.UUID
		memberID uuid.UUID
		doneAt   time.Time
		status   string
	}
	completions := func() []completion {
		t.Helper()
		rows, err := tx.Query(ctx, "SELECT id, member_id, done_at, status FROM quest_completions WHERE household_id = $1 AND quest_id <> $2 ORDER BY done_at, id", hhID, orphanQuestID)
		if err != nil {
			t.Fatalf("query completions: %v", err)
		}
		defer rows.Close()
		var out []completion
		for rows.Next() {
			var c completion
			if err := rows.Scan(&c.id, &c.memberID, &c.doneAt, &c.status); err != nil {
				t.Fatalf("scan completion: %v", err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("completions: %v", err)
		}
		return out
	}
	points := func(id uuid.UUID) int32 {
		t.Helper()
		var p int32
		if err := tx.QueryRow(ctx, "SELECT points FROM family_members WHERE id = $1", id).Scan(&p); err != nil {
			t.Fatalf("points: %v", err)
		}
		return p
	}
	// A pending completion whose marker was removed under 000014 (member_id
	// nulled by ON DELETE SET NULL), on a quest resting on the cooldown it
	// started. Up cancels it, as member removal would, and reopens that
	// cooldown; nobody's points move.
	orphanDoneAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, `INSERT INTO household_quests (id, household_id, name, points, repeat_days, completed_at, available_at)
		VALUES ($1, $2, 'Orphaned', 10, 7, $3::timestamptz, $3::timestamptz + interval '7 days')`, orphanQuestID, hhID, orphanDoneAt); err != nil {
		t.Fatalf("insert orphan quest: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status)
		VALUES ($1, $2, $3, NULL, $4, 'pending')`, orphanID, hhID, orphanQuestID, orphanDoneAt); err != nil {
		t.Fatalf("insert orphan completion: %v", err)
	}
	orphan := func() (string, time.Time) {
		t.Helper()
		var status string
		var availableAt time.Time
		if err := tx.QueryRow(ctx, `SELECT c.status, q.available_at FROM quest_completions c
			JOIN household_quests q ON q.id = c.quest_id WHERE c.id = $1`, orphanID).Scan(&status, &availableAt); err != nil {
			t.Fatalf("read orphan: %v", err)
		}
		return status, availableAt
	}
	before := completions()
	if len(before) != 2 {
		t.Fatalf("seeded completions = %+v, want 2", before)
	}
	assertUnchanged := func(stage string) {
		t.Helper()
		after := completions()
		if len(after) != len(before) {
			t.Fatalf("%s: completions %+v, want %+v", stage, after, before)
		}
		for i := range before {
			if after[i].id != before[i].id || after[i].memberID != before[i].memberID ||
				!after[i].doneAt.Equal(before[i].doneAt) || after[i].status != before[i].status {
				t.Fatalf("%s: completion %d = %+v, want %+v", stage, i, after[i], before[i])
			}
		}
		if a, b := points(annID), points(bobID); a != 17 || b != 40 {
			t.Fatalf("%s: points Ann %d Bob %d, want 17 and 40", stage, a, b)
		}
	}

	// 3. Up: every existing quest, in any household, gets the defaults; no
	//    completion or balance changes and no participant row appears.
	if _, err := tx.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run up migration: %v", err)
	}
	var shareable bool
	var bonus int32
	if err := tx.QueryRow(ctx, "SELECT shareable, teamwork_bonus FROM household_quests WHERE id = $1", questID).Scan(&shareable, &bonus); err != nil {
		t.Fatalf("read quest after up: %v", err)
	}
	if shareable || bonus != 0 {
		t.Fatalf("quest after up shareable=%v teamwork_bonus=%d, want false and 0", shareable, bonus)
	}
	var notDefault int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM household_quests WHERE shareable OR teamwork_bonus <> 0").Scan(&notDefault); err != nil || notDefault != 0 {
		t.Fatalf("quests not at the defaults after up = %d (err %v), want 0", notDefault, err)
	}
	var participantRows, memberRows int
	if err := tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM household_quest_participants), (SELECT count(*) FROM quest_completion_members)").
		Scan(&participantRows, &memberRows); err != nil || participantRows != 0 || memberRows != 0 {
		t.Fatalf("rows after up: participants %d, completion members %d (err %v), want 0 and 0", participantRows, memberRows, err)
	}
	assertUnchanged("after up")
	if status, availableAt := orphan(); status != "cancelled" || availableAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("pending completion with no member after up: status %s, quest available_at %v; want cancelled and available now", status, availableAt)
	}

	// 4. A shared pending completion covering Ann and Bob, plus a pending
	//    completion that records only one member (allowed to go down).
	if _, err := tx.Exec(ctx, "UPDATE household_quests SET shareable = true, teamwork_bonus = 20 WHERE id = $1", questID); err != nil {
		t.Fatalf("make quest shareable: %v", err)
	}
	sharedID := uuid.New()
	sharedDoneAt := doneAt.Add(48 * time.Hour)
	if _, err := tx.Exec(ctx, `INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status)
		VALUES ($1, $2, $3, $4, $5, 'pending')`, sharedID, hhID, questID, annID, sharedDoneAt); err != nil {
		t.Fatalf("insert shared completion: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO quest_completion_members (household_id, completion_id, member_id)
		VALUES ($1, $2, $3), ($1, $2, $4), ($1, $5, $3)`, hhID, sharedID, annID, bobID, pendingID); err != nil {
		t.Fatalf("insert completion members: %v", err)
	}
	before = completions()

	// Down refuses and changes nothing.
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, downErr := sp.Exec(ctx, downSQL)
	if downErr == nil || !strings.Contains(downErr.Error(), "cannot roll back quest sharing") {
		t.Fatalf("down with a shared pending completion: err = %v, want the refusal", downErr)
	}
	if err := sp.Rollback(ctx); err != nil {
		t.Fatalf("rollback savepoint: %v", err)
	}
	if !columnExists("household_quests", "shareable") || !columnExists("household_quests", "teamwork_bonus") ||
		!tableExists("quest_completion_members") || !tableExists("household_quest_participants") {
		t.Fatal("refused down dropped part of the schema")
	}
	var sharedMembers int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM quest_completion_members WHERE completion_id = $1", sharedID).Scan(&sharedMembers); err != nil || sharedMembers != 2 {
		t.Fatalf("shared completion members after refused down = %d (err %v), want 2", sharedMembers, err)
	}
	if err := tx.QueryRow(ctx, "SELECT shareable, teamwork_bonus FROM household_quests WHERE id = $1", questID).Scan(&shareable, &bonus); err != nil || !shareable || bonus != 20 {
		t.Fatalf("quest after refused down shareable=%v bonus=%d (err %v), want true and 20", shareable, bonus, err)
	}
	assertUnchanged("after refused down")

	// 5. Resolved (sent back), down runs: the tables and columns go, every
	//    completion and balance stays.
	if _, err := tx.Exec(ctx, "UPDATE quest_completions SET status = 'rejected' WHERE id = $1", sharedID); err != nil {
		t.Fatalf("resolve shared completion: %v", err)
	}
	before = completions()
	if _, err := tx.Exec(ctx, downSQL); err != nil {
		t.Fatalf("run down after resolving: %v", err)
	}
	if columnExists("household_quests", "shareable") || columnExists("household_quests", "teamwork_bonus") {
		t.Fatal("shareable/teamwork_bonus columns remain after down")
	}
	if tableExists("quest_completion_members") || tableExists("household_quest_participants") {
		t.Fatal("participant tables remain after down")
	}
	assertUnchanged("after down")
}
