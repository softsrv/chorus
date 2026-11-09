//go:build integration

package app_test

import (
	"context"
	"crypto/md5"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

func readMigration(t *testing.T, filename string) string {
	t.Helper()
	paths := []string{
		filepath.Join("..", "..", "db", "migrations", filename),
		filepath.Join("db", "migrations", filename),
	}
	for _, p := range paths {
		if content, err := os.ReadFile(p); err == nil {
			return string(content)
		}
	}
	t.Fatalf("could not find migration file %s", filename)
	return ""
}

// TestIntegration_SingleLoginMigration proves CLM-15, CLM-16, and CLM-18:
//   - CLM-15: Migration 000011 converts every login member into a family member
//     (display name, 0 points), keeps the earliest admin as the one login,
//     copies other members to household_members_retired, preserves chores.assignee_id,
//     deletes no users, and is idempotent on re-run.
//   - CLM-16: Resets household_provisioned_at to NULL for users left with no
//     household_members row; next verified sign-in auto-provisions a household.
//   - CLM-18: Row counts of chores, users, and households survive migrations.
func TestIntegration_SingleLoginMigration(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	ctx := context.Background()

	upSQL := readMigration(t, "000011_single_login_households.up.sql")
	downSQL := readMigration(t, "000011_single_login_households.down.sql")

	// Ensure clean pre-000011 state before seeding multi-login household
	if _, err := pool.Exec(ctx, downSQL); err != nil {
		t.Logf("pre-migration down (may fail if 000011 was not yet applied): %v", err)
	}

	// Register 3 users: 2 admins and 1 member
	admin1Prefix := "sl-admin1"
	admin2Prefix := "sl-admin2"
	memberPrefix := "sl-member"

	admin1 := registerVerified(t, pool, authSvc, mailer, admin1Prefix).User
	admin2 := registerVerified(t, pool, authSvc, mailer, admin2Prefix).User
	member1 := registerVerified(t, pool, authSvc, mailer, memberPrefix).User

	// Clean up any auto-provisioned memberships from registerVerified
	if _, err := pool.Exec(ctx, "DELETE FROM household_members WHERE user_id IN ($1, $2, $3)", admin1.ID, admin2.ID, member1.ID); err != nil {
		t.Fatalf("clean auto-provisioned memberships: %v", err)
	}

	// Create test multi-login household
	householdID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO households (id, name, timezone) VALUES ($1, $2, $3)", householdID, "Shared Family", "UTC"); err != nil {
		t.Fatalf("create test household: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM households WHERE id = $1", householdID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM users WHERE id IN ($1, $2, $3)", admin1.ID, admin2.ID, member1.ID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM households WHERE id NOT IN (SELECT household_id FROM household_members)")
	})

	now := time.Now().UTC()
	tAdmin1Joined := now.Add(-2 * time.Hour)
	tAdmin2Joined := now.Add(-1 * time.Hour)
	tMemberJoined := now

	// Seed memberships: Admin 1 (earlier), Admin 2 (later), Member
	if _, err := pool.Exec(ctx, `
		INSERT INTO household_members (household_id, user_id, role, joined_at) VALUES
		($1, $2, 'admin', $3),
		($1, $4, 'admin', $5),
		($1, $6, 'member', $7)
	`, householdID, admin1.ID, tAdmin1Joined, admin2.ID, tAdmin2Joined, member1.ID, tMemberJoined); err != nil {
		t.Fatalf("seed household_members: %v", err)
	}

	// Simulate 000009 backfill: all 3 users belong to a household, so provisioned_at is set
	if _, err := pool.Exec(ctx, "UPDATE users SET household_provisioned_at = $1 WHERE id IN ($2, $3, $4)", now, admin1.ID, admin2.ID, member1.ID); err != nil {
		t.Fatalf("set household_provisioned_at: %v", err)
	}

	// Seed chores assigned to non-kept users (admin2 and member1)
	chore1ID := uuid.Must(uuid.NewV7())
	chore2ID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateChore(ctx, db.CreateChoreParams{
		ID:          chore1ID,
		HouseholdID: householdID,
		Title:       "Chore for Admin 2",
		AssigneeID:  &admin2.ID,
		CreatedBy:   &admin1.ID,
	}); err != nil {
		t.Fatalf("create chore 1: %v", err)
	}
	if _, err := q.CreateChore(ctx, db.CreateChoreParams{
		ID:          chore2ID,
		HouseholdID: householdID,
		Title:       "Chore for Member",
		AssigneeID:  &member1.ID,
		CreatedBy:   &admin1.ID,
	}); err != nil {
		t.Fatalf("create chore 2: %v", err)
	}

	// Capture initial row counts to prove CLM-18 (counts survive across migration)
	var initialChores, initialUsers, initialHouseholds int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM chores").Scan(&initialChores); err != nil {
		t.Fatalf("count chores: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&initialUsers); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM households").Scan(&initialHouseholds); err != nil {
		t.Fatalf("count households: %v", err)
	}

	// Run migration 000011 UP twice (testing idempotency)
	for i := 1; i <= 2; i++ {
		if _, err := pool.Exec(ctx, upSQL); err != nil {
			t.Fatalf("apply 000011 up (run %d): %v", i, err)
		}
	}

	// ── CLM-15 assertions ────────────────────────────────────────────────────────

	// 1. Exactly one household_members row remains (the earliest admin: admin1)
	type memberRow struct {
		UserID uuid.UUID
		Role   string
	}
	rows, err := pool.Query(ctx, "SELECT user_id, role FROM household_members WHERE household_id = $1", householdID)
	if err != nil {
		t.Fatalf("query household_members: %v", err)
	}
	defer rows.Close()
	var keptMembers []memberRow
	for rows.Next() {
		var mr memberRow
		if err := rows.Scan(&mr.UserID, &mr.Role); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		keptMembers = append(keptMembers, mr)
	}
	if len(keptMembers) != 1 {
		t.Fatalf("expected exactly 1 kept member in household_members, got %d: %+v", len(keptMembers), keptMembers)
	}
	if keptMembers[0].UserID != admin1.ID || keptMembers[0].Role != "admin" {
		t.Errorf("expected kept member to be earliest admin (%s), got %+v", admin1.ID, keptMembers[0])
	}

	// 2. Three family_members created with display names and 0 points
	type famRow struct {
		ID     uuid.UUID
		Name   string
		Points int32
	}
	fRows, err := pool.Query(ctx, "SELECT id, name, points FROM family_members WHERE household_id = $1", householdID)
	if err != nil {
		t.Fatalf("query family_members: %v", err)
	}
	defer fRows.Close()
	famByUserID := make(map[uuid.UUID]famRow)
	for fRows.Next() {
		var fr famRow
		if err := fRows.Scan(&fr.ID, &fr.Name, &fr.Points); err != nil {
			t.Fatalf("scan family_member: %v", err)
		}
		for _, u := range []db.User{admin1, admin2, member1} {
			md5Hash := md5.Sum([]byte(householdID.String() + ":" + u.ID.String()))
			expectedID := uuid.Must(uuid.FromBytes(md5Hash[:]))
			if fr.ID == expectedID {
				famByUserID[u.ID] = fr
			}
		}
	}
	if len(famByUserID) != 3 {
		t.Fatalf("expected 3 family members matching md5 IDs, got %d", len(famByUserID))
	}
	for _, u := range []db.User{admin1, admin2, member1} {
		fr, ok := famByUserID[u.ID]
		if !ok {
			t.Errorf("family member missing for user %s (%s)", u.ID, u.DisplayName)
			continue
		}
		if fr.Name != u.DisplayName {
			t.Errorf("user %s: expected family member name %q, got %q", u.ID, u.DisplayName, fr.Name)
		}
		if fr.Points != 0 {
			t.Errorf("user %s: expected 0 points, got %d", u.ID, fr.Points)
		}
	}

	// 3. Retired members exist in household_members_retired (admin2 and member1)
	rRows, err := pool.Query(ctx, "SELECT user_id, role FROM household_members_retired WHERE household_id = $1", householdID)
	if err != nil {
		t.Fatalf("query household_members_retired: %v", err)
	}
	defer rRows.Close()
	retired := make(map[uuid.UUID]string)
	for rRows.Next() {
		var uid uuid.UUID
		var role string
		if err := rRows.Scan(&uid, &role); err != nil {
			t.Fatalf("scan retired member: %v", err)
		}
		retired[uid] = role
	}
	if len(retired) != 2 {
		t.Errorf("expected 2 retired members, got %d", len(retired))
	}
	if retired[admin2.ID] != "admin" {
		t.Errorf("expected admin2 to be retired with role 'admin', got %q", retired[admin2.ID])
	}
	if retired[member1.ID] != "member" {
		t.Errorf("expected member1 to be retired with role 'member', got %q", retired[member1.ID])
	}

	// 4. Assignee IDs on chores remain intact (assignee_id unchanged)
	c1, err := q.GetChore(ctx, db.GetChoreParams{HouseholdID: householdID, ID: chore1ID})
	if err != nil {
		t.Fatalf("get chore 1: %v", err)
	}
	if c1.AssigneeID == nil || *c1.AssigneeID != admin2.ID {
		t.Errorf("chore 1: expected assignee %s, got %v", admin2.ID, c1.AssigneeID)
	}
	c2, err := q.GetChore(ctx, db.GetChoreParams{HouseholdID: householdID, ID: chore2ID})
	if err != nil {
		t.Fatalf("get chore 2: %v", err)
	}
	if c2.AssigneeID == nil || *c2.AssigneeID != member1.ID {
		t.Errorf("chore 2: expected assignee %s, got %v", member1.ID, c2.AssigneeID)
	}

	// 5. All users still exist (none deleted)
	for _, u := range []db.User{admin1, admin2, member1} {
		if _, err := q.GetUserByID(ctx, u.ID); err != nil {
			t.Errorf("user %s should still exist: %v", u.ID, err)
		}
	}

	// 6. Unique index rejects a second login insert for the household
	if _, err := pool.Exec(ctx, "INSERT INTO household_members (household_id, user_id, role) VALUES ($1, $2, 'member')", householdID, admin2.ID); err == nil {
		t.Error("expected unique index on household_members(household_id) to reject second member, but insert succeeded")
	}

	// ── CLM-18 assertions ────────────────────────────────────────────────────────
	// Row counts of chores, users, households survived 000011 without corruption or deletion
	var postChores, postUsers, postHouseholds int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM chores").Scan(&postChores); err != nil {
		t.Fatalf("count post chores: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&postUsers); err != nil {
		t.Fatalf("count post users: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM households").Scan(&postHouseholds); err != nil {
		t.Fatalf("count post households: %v", err)
	}
	if postChores != initialChores {
		t.Errorf("chores count changed: was %d, now %d", initialChores, postChores)
	}
	if postUsers != initialUsers {
		t.Errorf("users count changed: was %d, now %d", initialUsers, postUsers)
	}
	if postHouseholds != initialHouseholds {
		t.Errorf("households count changed: was %d, now %d", initialHouseholds, postHouseholds)
	}

	// ── CLM-16 assertions ────────────────────────────────────────────────────────
	// Kept admin keeps their household_provisioned_at value unchanged
	var admin1Prov, admin2Prov, member1Prov pgtype.Timestamptz
	if err := pool.QueryRow(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1", admin1.ID).Scan(&admin1Prov); err != nil {
		t.Fatalf("query admin1 provisioned_at: %v", err)
	}
	if !admin1Prov.Valid {
		t.Errorf("kept admin should keep household_provisioned_at, got NULL")
	}

	// Retired co-admin and member have household_provisioned_at reset to NULL
	if err := pool.QueryRow(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1", admin2.ID).Scan(&admin2Prov); err != nil {
		t.Fatalf("query admin2 provisioned_at: %v", err)
	}
	if admin2Prov.Valid {
		t.Errorf("retired co-admin should have household_provisioned_at reset to NULL, got %v", admin2Prov.Time)
	}

	if err := pool.QueryRow(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1", member1.ID).Scan(&member1Prov); err != nil {
		t.Fatalf("query member1 provisioned_at: %v", err)
	}
	if member1Prov.Valid {
		t.Errorf("retired member should have household_provisioned_at reset to NULL, got %v", member1Prov.Time)
	}

	// Retired co-admin logs in, which auto-provisions their own household
	loginRes, err := authSvc.Login(ctx, admin2.Email, "correct-password-123", app.DeviceMeta{})
	if err != nil {
		t.Fatalf("login retired co-admin: %v", err)
	}

	// Assert admin2 now belongs to exactly one household (their own auto-created household)
	var newHHCount int
	var newHHName string
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(h.name), '')
		FROM household_members m
		JOIN households h ON h.id = m.household_id
		WHERE m.user_id = $1
	`, admin2.ID).Scan(&newHHCount, &newHHName); err != nil {
		t.Fatalf("query admin2 households: %v", err)
	}
	if newHHCount != 1 {
		t.Fatalf("expected admin2 to have 1 newly provisioned household, got %d", newHHCount)
	}
	if newHHName == "Shared Family" {
		t.Errorf("admin2 should have their own household, not Shared Family")
	}
	if !loginRes.User.HouseholdProvisionedAt.Valid {
		var refreshedProv pgtype.Timestamptz
		_ = pool.QueryRow(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1", admin2.ID).Scan(&refreshedProv)
		if !refreshedProv.Valid {
			t.Errorf("admin2 household_provisioned_at should be set after provisioning")
		}
	}

	// ── Down migration test ───────────────────────────────────────────────────────
	// Verify down migration rolls back single-login conversion cleanly
	if _, err := pool.Exec(ctx, downSQL); err != nil {
		t.Fatalf("apply 000011 down: %v", err)
	}

	// After down, retired table is dropped
	var tableExists bool
	_ = pool.QueryRow(ctx, "SELECT to_regclass('household_members_retired') IS NOT NULL").Scan(&tableExists)
	if tableExists {
		t.Errorf("household_members_retired table should be dropped after down migration")
	}

	// Re-apply up so database is left in updated state
	if _, err := pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("re-apply 000011 up after down test: %v", err)
	}
}
