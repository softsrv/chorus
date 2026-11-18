//go:build integration

package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration_AdminPointsMigration proves CLM-2 at the schema: with 000018
// applied an admin row may be negative, a negative quest row is rejected, and
// down refuses while an admin row exists and succeeds once none do. Everything
// runs in one transaction that is rolled back, with a savepoint around each
// statement expected to fail.
func TestIntegration_AdminPointsMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	upSQL := readMigration(t, "000018_admin_points_adjustments.up.sql")
	downSQL := readMigration(t, "000018_admin_points_adjustments.down.sql")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// try runs sql in a savepoint and rolls it back when it fails, so a
	// refusal leaves the outer transaction usable.
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

	hhID, memberID := uuid.New(), uuid.New()
	mustExec("INSERT INTO households (id, name, timezone) VALUES ($1, 'Admin Pts Fam', 'UTC')", hhID)
	mustExec("INSERT INTO family_members (id, household_id, name, points) VALUES ($1, $2, 'Maya', 10)", memberID, hhID)
	insert := func(kind string, points int) error {
		return try(`INSERT INTO points_awards (id, household_id, member_id, kind, reason, points, earned_at)
			VALUES ($1, $2, $3, $4, 'r', $5, $6)`, uuid.New(), hhID, memberID, kind, points, time.Now())
	}

	// Other tests may have left admin rows; set them aside in this
	// transaction only, so the down step can be exercised below.
	mustExec("DELETE FROM points_awards WHERE kind = 'admin'")

	// Up applied: signed non-zero admin rows, non-negative others.
	if err := insert("admin", -5); err != nil {
		t.Fatalf("negative admin row rejected: %v", err)
	}
	if err := insert("admin", 7); err != nil {
		t.Fatalf("positive admin row rejected: %v", err)
	}
	if err := insert("admin", 0); err == nil {
		t.Error("zero admin row accepted")
	}
	for _, kind := range []string{"quest", "combo", "streak"} {
		if err := insert(kind, -1); err == nil {
			t.Errorf("negative %s row accepted", kind)
		}
		if err := insert(kind, 0); err != nil {
			t.Errorf("zero %s row rejected: %v", kind, err)
		}
	}
	if err := insert("bogus", 1); err == nil {
		t.Error("unknown kind accepted")
	}
	if err := try("UPDATE family_members SET points = -1 WHERE id = $1", memberID); err == nil {
		t.Error("family_members.points below 0 accepted")
	}

	// Down is refused while an admin row exists and changes nothing.
	err = try(downSQL)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back admin points adjustments") {
		t.Fatalf("down with admin rows: got %v, want refusal", err)
	}
	if err := insert("admin", -2); err != nil {
		t.Fatalf("admin rows no longer accepted after refused down: %v", err)
	}

	// Once no admin row exists, down succeeds and restores 000016.
	mustExec("DELETE FROM points_awards WHERE kind = 'admin'")
	mustExec(downSQL)
	if err := insert("admin", 5); err == nil {
		t.Error("admin kind accepted after down")
	}
	if err := insert("quest", 3); err != nil {
		t.Errorf("quest row rejected after down: %v", err)
	}
	if err := insert("quest", -1); err == nil {
		t.Error("negative quest row accepted after down")
	}

	// And up applies again on top of it.
	mustExec(upSQL)
	if err := insert("admin", -5); err != nil {
		t.Errorf("negative admin row rejected after re-up: %v", err)
	}
}
