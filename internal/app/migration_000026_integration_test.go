//go:build integration

package app_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestIntegration_DropUnusedTablesMigration proves CLM-1 and CLM-3:
// - CLM-1: In a scratch schema migrated to version 25, apply 000026 up and assert
//   to_regclass is NULL for household_invites, chores, and household_members_retired,
//   and no leftover idx_household_invites_* or idx_chores_* indexes exist.
//   DROP TABLE IF EXISTS makes a second run of 000026 up change nothing and error free.
// - CLM-3: Applying 000026 up removes only the three named tables: every other table,
//   and seeded rows (including households, household_members, and household_quests
//   migrated from chores with matching IDs) survives unchanged with identical counts.
func TestIntegration_DropUnusedTablesMigration(t *testing.T) {
	admin := testDB(t)
	ctx := context.Background()

	pairs := loadMigrationPairs(t)

	// Find migration 000026 index
	idx26 := -1
	for i, p := range pairs {
		if p.version == 26 {
			idx26 = i
			break
		}
	}
	if idx26 == -1 {
		t.Fatalf("migration 000026 not found in migration pairs")
	}

	schema := fmt.Sprintf("%sv000026_test", scratchSchemaPrefix)
	pool := scratchSchema(t, admin, schema)

	// Apply migrations up through version 25
	applyUps(t, pool, pairs[:idx26])

	// Seed rows in households, users, household_members, family_members, household_quests
	uID := uuid.New()
	hhID := uuid.New()
	fmID := uuid.New()
	questID := uuid.New()
	choreID := questID // same id as quest to test quest carried over from chores

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, email_verified)
		VALUES ($1, 'mig26@example.com', 'hash', 'Mig26 User', true)
	`, uID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO households (id, name, timezone)
		VALUES ($1, 'Mig26 Household', 'UTC')
	`, hhID); err != nil {
		t.Fatalf("seed household: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO household_members (household_id, user_id, role)
		VALUES ($1, $2, 'admin')
	`, hhID, uID); err != nil {
		t.Fatalf("seed household_member: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO family_members (id, household_id, name, points)
		VALUES ($1, $2, 'Mig26 Member', 10)
	`, fmID, hhID); err != nil {
		t.Fatalf("seed family_member: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	// Seed a chore in chores table (which exists at v25)
	if _, err := pool.Exec(ctx, `
		INSERT INTO chores (id, household_id, title, description, created_at, updated_at)
		VALUES ($1, $2, 'Carried Chore', 'Desc', $3, $3)
	`, choreID, hhID, now); err != nil {
		t.Fatalf("seed chore: %v", err)
	}

	// Seed household_quests with same id
	if _, err := pool.Exec(ctx, `
		INSERT INTO household_quests (id, household_id, name, points, repeat_days, notes, created_at, updated_at)
		VALUES ($1, $2, 'Carried Chore', 0, 1, 'Desc', $3, $3)
	`, choreID, hhID, now); err != nil {
		t.Fatalf("seed quest: %v", err)
	}

	// Seed invite and retired member in tables to be dropped
	inviteID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO household_invites (id, household_id, token_hash, expires_at)
		VALUES ($1, $2, 'hash123', $3)
	`, inviteID, hhID, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("seed invite: %v", err)
	}

	retiredUID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, email_verified)
		VALUES ($1, 'retired26@example.com', 'hash', 'Retired User', true)
	`, retiredUID); err != nil {
		t.Fatalf("seed retired user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO household_members_retired (household_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', $3)
	`, hhID, retiredUID, now); err != nil {
		t.Fatalf("seed retired member: %v", err)
	}

	// Snapshot all tables in schema at v25
	tablesAt25 := listTables(t, pool, schema)

	// Assert the 3 tables exist at v25
	for _, expected := range []string{"household_invites", "chores", "household_members_retired"} {
		if !slices.Contains(tablesAt25, expected) {
			t.Fatalf("table %s expected to exist at v25", expected)
		}
	}

	// Snapshot row counts of other tables
	countRows := func(tbl string) int {
		var cnt int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s."%s"`, schema, tbl)).Scan(&cnt); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		return cnt
	}

	hhCountBefore := countRows("households")
	hmCountBefore := countRows("household_members")
	hqCountBefore := countRows("household_quests")
	uCountBefore := countRows("users")
	fmCountBefore := countRows("family_members")

	// Apply 000026 up
	p26 := pairs[idx26]
	execMigration(t, pool, "up "+p26.name, p26.up)

	// CLM-1: Verify the 3 tables are gone (to_regclass is NULL)
	for _, tbl := range []string{"household_invites", "chores", "household_members_retired"} {
		var reg *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, fmt.Sprintf(`%s."%s"`, schema, tbl)).Scan(&reg); err != nil {
			t.Fatalf("to_regclass %s: %v", tbl, err)
		}
		if reg != nil {
			t.Errorf("table %s still exists after 000026 up: %s", tbl, *reg)
		}
	}

	// CLM-1: Verify no leftover idx_household_invites_* or idx_chores_* indexes exist
	var indexCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_indexes
		WHERE schemaname = $1
		  AND (indexname LIKE 'idx_household_invites_%' OR indexname LIKE 'idx_chores_%')
	`, schema).Scan(&indexCount); err != nil {
		t.Fatalf("check indexes: %v", err)
	}
	if indexCount != 0 {
		t.Errorf("found %d leftover indexes matching idx_household_invites_%% or idx_chores_%%", indexCount)
	}

	// CLM-3: Assert table set differs ONLY by the 3 dropped tables
	tablesAt26 := listTables(t, pool, schema)
	var dropped []string
	for _, tbl := range tablesAt25 {
		if !slices.Contains(tablesAt26, tbl) {
			dropped = append(dropped, tbl)
		}
	}
	slices.Sort(dropped)
	wantDropped := []string{"chores", "household_invites", "household_members_retired"}
	if !slices.Equal(dropped, wantDropped) {
		t.Errorf("dropped tables = %v, want %v", dropped, wantDropped)
	}

	var added []string
	for _, tbl := range tablesAt26 {
		if !slices.Contains(tablesAt25, tbl) {
			added = append(added, tbl)
		}
	}
	if len(added) != 0 {
		t.Errorf("unexpected tables added by 000026 up: %v", added)
	}

	// CLM-3: Assert surviving row counts are unchanged
	if got := countRows("households"); got != hhCountBefore {
		t.Errorf("households row count changed: %d -> %d", hhCountBefore, got)
	}
	if got := countRows("household_members"); got != hmCountBefore {
		t.Errorf("household_members row count changed: %d -> %d", hmCountBefore, got)
	}
	if got := countRows("household_quests"); got != hqCountBefore {
		t.Errorf("household_quests row count changed: %d -> %d", hqCountBefore, got)
	}
	if got := countRows("users"); got != uCountBefore {
		t.Errorf("users row count changed: %d -> %d", uCountBefore, got)
	}
	if got := countRows("family_members"); got != fmCountBefore {
		t.Errorf("family_members row count changed: %d -> %d", fmCountBefore, got)
	}

	// Verify the quest carried over from chores with same id survives intact
	var qName string
	var qNotes string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT name, notes FROM %s.household_quests WHERE id = $1`, schema), questID).Scan(&qName, &qNotes); err != nil {
		t.Fatalf("query quest with choreID: %v", err)
	}
	if qName != "Carried Chore" || qNotes != "Desc" {
		t.Errorf("quest fields mismatch: got (%s, %s), want ('Carried Chore', 'Desc')", qName, qNotes)
	}

	// CLM-1: Apply 000026 up again (idempotent, DROP TABLE IF EXISTS makes second run change nothing)
	execMigration(t, pool, "re-up "+p26.name, p26.up)

	// Verify again all 3 tables still do not exist and counts remain unchanged
	for _, tbl := range []string{"household_invites", "chores", "household_members_retired"} {
		var reg *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, fmt.Sprintf(`%s."%s"`, schema, tbl)).Scan(&reg); err != nil {
			t.Fatalf("to_regclass %s: %v", tbl, err)
		}
		if reg != nil {
			t.Errorf("table %s exists after re-applying 000026 up: %s", tbl, *reg)
		}
	}
	if got := countRows("household_quests"); got != hqCountBefore {
		t.Errorf("household_quests row count changed after re-up: %d -> %d", hqCountBefore, got)
	}
}

func listTables(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`, schema)
	if err != nil {
		t.Fatalf("listTables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listTables rows error: %v", err)
	}
	return tables
}
