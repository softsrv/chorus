//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_QuestCompletionStatusByMigration proves CLM-2 and CLM-6 at the schema:
// - Migrates to state 000024 by running 000025 down if status_by exists.
// - Seeds approved, pending, rejected, and cancelled quest_completions rows with NULL status_by.
// - Migrates up (000025 up SQL) and verifies all existing rows are preserved with status_by NULL.
// - Verifies the foreign key constraint on status_by references users(id) with delete rule SET NULL via pg_constraint (confdeltype = 'n').
// - Updates a row to reference user A via status_by, deletes user A, and verifies the deletion succeeds while the completion survives with status unchanged and status_by set to NULL.
// - Verifies that 000025 down and up round-trip cleanly.
func TestIntegration_QuestCompletionStatusByMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	upSQL := readMigration(t, "000025_quest_completion_status_by.up.sql")
	downSQL := readMigration(t, "000025_quest_completion_status_by.down.sql")

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

	// Start from 000024: if 000025 is already present, run downSQL to revert it.
	var hasStatusBy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'quest_completions' AND column_name = 'status_by'
	)`).Scan(&hasStatusBy); err != nil {
		t.Fatalf("check status_by: %v", err)
	}
	if hasStatusBy {
		mustExec(downSQL)
	}

	// Create test household, member, and quest before applying 000025.
	hhID := uuid.New()
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Mig 25 Fam', 'UTC')", hhID)
	memberID := uuid.New()
	mustExec("INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, 'Member 25')", memberID, hhID)
	questID := uuid.New()
	mustExec("INSERT INTO household_quests (id, household_id, name) VALUES ($1, $2, 'Quest 25')", questID, hhID)

	type compBefore struct {
		id         uuid.UUID
		status     string
		approvedAt *time.Time
		doneAt     time.Time
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	approvedID := uuid.New()
	pendingID := uuid.New()
	rejectedID := uuid.New()
	cancelledID := uuid.New()

	approvedAt := now.Add(-time.Hour)
	mustExec(`INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, approved_at)
		VALUES ($1, $2, $3, $4, $5, 'approved', $6)`, approvedID, hhID, questID, memberID, now.Add(-2*time.Hour), approvedAt)

	mustExec(`INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, approved_at)
		VALUES ($1, $2, $3, $4, $5, 'pending', NULL)`, pendingID, hhID, questID, memberID, now.Add(-90*time.Minute))

	mustExec(`INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, approved_at)
		VALUES ($1, $2, $3, $4, $5, 'rejected', NULL)`, rejectedID, hhID, questID, memberID, now.Add(-80*time.Minute))

	mustExec(`INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, approved_at)
		VALUES ($1, $2, $3, $4, $5, 'cancelled', NULL)`, cancelledID, hhID, questID, memberID, now.Add(-70*time.Minute))

	preserved := []compBefore{
		{id: approvedID, status: "approved", approvedAt: &approvedAt, doneAt: now.Add(-2 * time.Hour)},
		{id: pendingID, status: "pending", approvedAt: nil, doneAt: now.Add(-90 * time.Minute)},
		{id: rejectedID, status: "rejected", approvedAt: nil, doneAt: now.Add(-80 * time.Minute)},
		{id: cancelledID, status: "cancelled", approvedAt: nil, doneAt: now.Add(-70 * time.Minute)},
	}

	// Migrate up to 000025.
	mustExec(upSQL)

	// Assert every row unchanged with status_by NULL.
	for _, want := range preserved {
		var gotStatus string
		var gotApprovedAt *time.Time
		var gotDoneAt time.Time
		var gotStatusBy *uuid.UUID
		var gotHH, gotQ, gotM uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT household_id, quest_id, member_id, done_at, status, approved_at, status_by
			FROM quest_completions WHERE id = $1`, want.id).Scan(&gotHH, &gotQ, &gotM, &gotDoneAt, &gotStatus, &gotApprovedAt, &gotStatusBy); err != nil {
			t.Fatalf("read completion %s: %v", want.id, err)
		}
		if gotHH != hhID || gotQ != questID || gotM != memberID {
			t.Errorf("completion %s ids changed", want.id)
		}
		if gotStatus != want.status {
			t.Errorf("completion %s status: got %s, want %s", want.id, gotStatus, want.status)
		}
		if !gotDoneAt.Equal(want.doneAt) {
			t.Errorf("completion %s done_at: got %v, want %v", want.id, gotDoneAt, want.doneAt)
		}
		if (gotApprovedAt == nil) != (want.approvedAt == nil) || (gotApprovedAt != nil && !gotApprovedAt.Equal(*want.approvedAt)) {
			t.Errorf("completion %s approved_at: got %v, want %v", want.id, gotApprovedAt, want.approvedAt)
		}
		if gotStatusBy != nil {
			t.Errorf("completion %s status_by: got %v, want NULL", want.id, gotStatusBy)
		}
	}

	// Assert the FK status_by -> users(id) with delete rule SET NULL via pg_constraint (confdeltype = 'n').
	var fkCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
		WHERE c.conrelid = 'quest_completions'::regclass
		  AND c.confrelid = 'users'::regclass
		  AND c.contype = 'f'
		  AND c.confdeltype = 'n'
		  AND a.attname = 'status_by'
	`).Scan(&fkCount); err != nil {
		t.Fatalf("query pg_constraint for status_by FK: %v", err)
	}
	if fkCount != 1 {
		t.Fatalf("expected 1 FK constraint status_by -> users(id) with confdeltype='n', got %d", fkCount)
	}

	// Create user A, set status_by on a row, delete user A, assert user deletion succeeds and row survives with status_by NULL.
	userA := uuid.New()
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, email_verified)
		VALUES ($1, $2, 'hash', 'Actor A', true)`, userA, "actora-"+userA.String()[:8]+"@example.com")

	mustExec("UPDATE quest_completions SET status_by = $1 WHERE id = $2", userA, approvedID)

	var setStatusBy *uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT status_by FROM quest_completions WHERE id = $1", approvedID).Scan(&setStatusBy); err != nil {
		t.Fatalf("read status_by after update: %v", err)
	}
	if setStatusBy == nil || *setStatusBy != userA {
		t.Fatalf("status_by not updated to userA: got %v", setStatusBy)
	}

	// Delete user A.
	mustExec("DELETE FROM users WHERE id = $1", userA)

	// Assert the row survives with same status and status_by NULL.
	var afterDeleteStatus string
	var afterDeleteStatusBy *uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT status, status_by FROM quest_completions WHERE id = $1", approvedID).Scan(&afterDeleteStatus, &afterDeleteStatusBy); err != nil {
		t.Fatalf("read completion after user delete: %v", err)
	}
	if afterDeleteStatus != "approved" {
		t.Errorf("status changed after user delete: got %s, want approved", afterDeleteStatus)
	}
	if afterDeleteStatusBy != nil {
		t.Errorf("status_by after user delete: got %v, want NULL", afterDeleteStatusBy)
	}

	// Run down and up again.
	mustExec(downSQL)
	var colCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'quest_completions' AND column_name = 'status_by'`).Scan(&colCount); err != nil {
		t.Fatalf("count status_by columns after down: %v", err)
	}
	if colCount != 0 {
		t.Errorf("after down: %d status_by column(s), want 0", colCount)
	}

	mustExec(upSQL)
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'quest_completions' AND column_name = 'status_by'`).Scan(&colCount); err != nil {
		t.Fatalf("count status_by columns after re-up: %v", err)
	}
	if colCount != 1 {
		t.Errorf("after re-up: %d status_by column(s), want 1", colCount)
	}
}
