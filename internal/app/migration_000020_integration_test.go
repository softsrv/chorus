//go:build integration

package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_RewardRedemptionsMigration proves CLM-10 at the schema:
// with 000020 applied a 'reward' points award must be negative, admin rows
// stay any non-zero amount and the others >= 0; reward_redemptions takes only
// a known status and one pending request per member and reward; down refuses
// while a 'reward' award exists and succeeds once none do, restoring the
// 000019 constraints and dropping the table. Everything runs in one
// transaction that is rolled back, with a savepoint around each statement
// expected to fail.
func TestIntegration_RewardRedemptionsMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	upSQL := readMigration(t, "000020_reward_redemptions.up.sql")
	downSQL := readMigration(t, "000020_reward_redemptions.down.sql")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	try := func(sql string, args ...any) error {
		t.Helper()
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		if _, err := sp.Exec(ctx, sql, args...); err != nil {
			_ = sp.Rollback(ctx)
			return err
		}
		return sp.Commit(ctx)
	}
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	hhID, memberID, rewardID := uuid.New(), uuid.New(), uuid.New()
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Redeem Mig Fam', 'UTC')", hhID)
	mustExec("INSERT INTO family_members (id, household_id, name, points) VALUES ($1, $2, 'Sam', 120)", memberID, hhID)
	mustExec("INSERT INTO household_rewards (id, household_id, description, cost) VALUES ($1, $2, 'Movie night', 100)", rewardID, hhID)
	award := func(kind string, points int) error {
		return try(`INSERT INTO points_awards (id, household_id, member_id, kind, reason, points, earned_at)
			VALUES ($1, $2, $3, $4, 'r', $5, $6)`, uuid.New(), hhID, memberID, kind, points, time.Now())
	}
	request := func(status string) error {
		decided := any(nil)
		if status != "pending" {
			decided = time.Now()
		}
		return try(`INSERT INTO reward_redemptions (id, household_id, member_id, reward_id, description, cost, status, decided_at)
			VALUES ($1, $2, $3, $4, 'Movie night', 100, $5, $6)`, uuid.New(), hhID, memberID, rewardID, status, decided)
	}

	// Other tests may have left reward rows; set them aside in this
	// transaction only, so the down step can be exercised below.
	mustExec("DELETE FROM points_awards WHERE kind = 'reward'")

	// points_awards: reward rows only negative; admin and others unchanged.
	if err := award("reward", -100); err != nil {
		t.Fatalf("negative reward row rejected: %v", err)
	}
	if err := award("reward", 0); err == nil {
		t.Error("zero reward row accepted")
	}
	if err := award("reward", 5); err == nil {
		t.Error("positive reward row accepted")
	}
	if err := award("admin", -5); err != nil {
		t.Errorf("negative admin row rejected: %v", err)
	}
	if err := award("admin", 0); err == nil {
		t.Error("zero admin row accepted")
	}
	for _, kind := range []string{"quest", "combo", "streak"} {
		if err := award(kind, -1); err == nil {
			t.Errorf("negative %s row accepted", kind)
		}
	}

	// reward_redemptions: known statuses, one pending per member and reward.
	if err := request("pending"); err != nil {
		t.Fatalf("pending request rejected: %v", err)
	}
	if err := request("pending"); err == nil {
		t.Error("second pending request for the same member and reward accepted")
	}
	for _, status := range []string{"approved", "rejected", "cancelled"} {
		if err := request(status); err != nil {
			t.Errorf("%s request rejected: %v", status, err)
		}
	}
	if err := request("bogus"); err == nil {
		t.Error("unknown status accepted")
	}
	// Deleting the reward keeps its requests with reward_id NULL.
	mustExec("DELETE FROM household_rewards WHERE id = $1", rewardID)
	var kept int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM reward_redemptions WHERE household_id = $1 AND reward_id IS NULL", hhID).Scan(&kept); err != nil {
		t.Fatalf("count kept requests: %v", err)
	}
	if kept != 4 {
		t.Errorf("requests kept after reward delete = %d, want 4", kept)
	}

	// Down is refused while a reward award exists and changes nothing.
	err = try(downSQL)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back reward redemptions") {
		t.Fatalf("down with reward rows: got %v, want refusal", err)
	}
	if err := award("reward", -2); err != nil {
		t.Fatalf("reward rows no longer accepted after refused down: %v", err)
	}

	// Once no reward row exists, down succeeds and restores 000019.
	mustExec("DELETE FROM points_awards WHERE kind = 'reward'")
	mustExec(downSQL)
	if err := award("reward", -5); err == nil {
		t.Error("reward kind accepted after down")
	}
	if err := award("admin", -3); err != nil {
		t.Errorf("negative admin row rejected after down: %v", err)
	}
	var reg *string
	if err := tx.QueryRow(ctx, "SELECT to_regclass('reward_redemptions')::text").Scan(&reg); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	if reg != nil {
		t.Error("reward_redemptions still exists after down")
	}

	// And up applies again on top of it.
	mustExec(upSQL)
	if err := award("reward", -5); err != nil {
		t.Errorf("negative reward row rejected after re-up: %v", err)
	}
}
