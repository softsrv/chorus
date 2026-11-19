//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_RewardRedemptionsStatusByMigration proves CLM-3 at the
// schema: 000023 adds status_by as a nullable UUID referencing users with ON
// DELETE SET NULL; a row decided before the migration keeps status_by NULL (no
// backfill) and a new pending row has it NULL; deleting the user named in
// status_by succeeds and leaves the decided row with its status and status_by
// NULL; down drops the column. Everything runs in one transaction that is
// rolled back.
func TestIntegration_RewardRedemptionsStatusByMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	upSQL := readMigration(t, "000023_reward_redemptions_status_by.up.sql")
	downSQL := readMigration(t, "000023_reward_redemptions_status_by.down.sql")

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
	hasColumn := func() bool {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'reward_redemptions' AND column_name = 'status_by'`).Scan(&n); err != nil {
			t.Fatalf("read columns: %v", err)
		}
		return n == 1
	}

	// Start from 000022.
	mustExec(downSQL)
	if hasColumn() {
		t.Fatal("status_by present after down")
	}

	// A request decided before 000023.
	hhID, memberID, rewardID := uuid.New(), uuid.New(), uuid.New()
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Status By Mig Fam', 'UTC')", hhID)
	mustExec("INSERT INTO family_members (id, household_id, name, points) VALUES ($1, $2, 'Sam', 120)", memberID, hhID)
	mustExec("INSERT INTO household_rewards (id, household_id, description, cost) VALUES ($1, $2, 'Movie night', 100)", rewardID, hhID)
	oldDecided := uuid.New()
	mustExec(`INSERT INTO reward_redemptions (id, household_id, member_id, reward_id, description, cost, status, decided_at)
		VALUES ($1, $2, $3, $4, 'Movie night', 100, 'approved', $5)`, oldDecided, hhID, memberID, rewardID, time.Now())

	mustExec(upSQL)

	statusBy := func(id uuid.UUID) (string, *uuid.UUID) {
		t.Helper()
		var status string
		var by *uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT status, status_by FROM reward_redemptions WHERE id = $1", id).Scan(&status, &by); err != nil {
			t.Fatalf("read redemption %s: %v", id, err)
		}
		return status, by
	}
	if status, by := statusBy(oldDecided); status != "approved" || by != nil {
		t.Errorf("pre-000023 decided row: status=%q status_by=%v, want approved and NULL (no backfill)", status, by)
	}
	pending := uuid.New()
	mustExec(`INSERT INTO reward_redemptions (id, household_id, member_id, reward_id, description, cost)
		VALUES ($1, $2, $3, $4, 'Movie night', 100)`, pending, hhID, memberID, rewardID)
	if status, by := statusBy(pending); status != "pending" || by != nil {
		t.Errorf("new pending row: status=%q status_by=%v, want pending and NULL", status, by)
	}

	// Column shape: nullable uuid with a FK to users(id) ON DELETE SET NULL.
	var dataType, nullable string
	if err := tx.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'reward_redemptions' AND column_name = 'status_by'`).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read status_by column: %v", err)
	}
	if dataType != "uuid" || nullable != "YES" {
		t.Errorf("status_by: type=%q nullable=%q, want uuid and YES", dataType, nullable)
	}
	var refTable, delType string
	if err := tx.QueryRow(ctx, `SELECT c.confrelid::regclass::text, c.confdeltype::text
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE c.conrelid = 'reward_redemptions'::regclass AND c.contype = 'f' AND a.attname = 'status_by'`).Scan(&refTable, &delType); err != nil {
		t.Fatalf("read status_by foreign key: %v", err)
	}
	if refTable != "users" || delType != "n" {
		t.Errorf("status_by FK: references %q on delete %q, want users and 'n' (SET NULL)", refTable, delType)
	}

	// Deleting the user who decided keeps the row, its status, and nulls status_by.
	deciderID := uuid.Must(uuid.NewV7())
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, email_verified)
		VALUES ($1, $2, 'hash', 'Decider', true)`, deciderID, "status-by-mig-"+deciderID.String()[:8]+"@example.com")
	decided := uuid.New()
	mustExec(`INSERT INTO reward_redemptions (id, household_id, member_id, reward_id, description, cost, status, decided_at, status_by)
		VALUES ($1, $2, $3, $4, 'Movie night', 100, 'rejected', $5, $6)`, decided, hhID, memberID, rewardID, time.Now(), deciderID)
	if _, by := statusBy(decided); by == nil || *by != deciderID {
		t.Fatalf("status_by = %v, want %s", by, deciderID)
	}
	mustExec("DELETE FROM users WHERE id = $1", deciderID)
	if status, by := statusBy(decided); status != "rejected" || by != nil {
		t.Errorf("after deleting the decider: status=%q status_by=%v, want rejected and NULL", status, by)
	}

	// Down drops the column; the rows stay.
	mustExec(downSQL)
	if hasColumn() {
		t.Error("status_by still present after down")
	}
	var kept int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM reward_redemptions WHERE household_id = $1", hhID).Scan(&kept); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if kept != 3 {
		t.Errorf("rows after down = %d, want 3", kept)
	}
	// And up applies again on top of it.
	mustExec(upSQL)
}

// TestIntegration_DeleteAccountOfDecidingAdmin proves the account deletion
// path of CLM-3: AuthService.DeleteAccount of the user named in a decided
// request's status_by succeeds, and the request stays with its status and
// status_by NULL. household_members_one_login allows one login per household,
// so the request sits in a household the deleted user is not a member of
// (deleting their own sole-login household would cascade it away).
func TestIntegration_DeleteAccountOfDecidingAdmin(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	ctx := context.Background()

	decider := registerVerified(t, pool, authSvc, mailer, "statusbydecider").User.ID

	hhID, memberID, redemptionID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, "INSERT INTO households (id, name, timezone) VALUES ($1, 'Status By Del Fam', 'UTC')", hhID); err != nil {
		t.Fatalf("insert household: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM households WHERE id = $1", hhID); err != nil {
			t.Errorf("delete household %s: %v", hhID, err)
		}
	})
	if _, err := pool.Exec(ctx, "INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, 'Sam')", memberID, hhID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reward_redemptions (id, household_id, member_id, description, cost, status, decided_at, status_by)
		VALUES ($1, $2, $3, 'Movie night', 0, 'approved', NOW(), $4)`, redemptionID, hhID, memberID, decider); err != nil {
		t.Fatalf("insert decided redemption: %v", err)
	}

	if err := authSvc.DeleteAccount(ctx, decider, uuid.Nil, "correct-password-123", ""); err != nil {
		t.Fatalf("DeleteAccount of the deciding admin: %v", err)
	}

	var status string
	var by *uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT status, status_by FROM reward_redemptions WHERE id = $1", redemptionID).Scan(&status, &by); err != nil {
		t.Fatalf("read redemption after account deletion: %v", err)
	}
	if status != "approved" || by != nil {
		t.Errorf("after DeleteAccount: status=%q status_by=%v, want approved and NULL", status, by)
	}
}
