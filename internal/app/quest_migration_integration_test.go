//go:build integration

package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type choreSnapshot struct {
	id          uuid.UUID
	householdID uuid.UUID
	title       string
	description string
	assigneeID  *uuid.UUID
	dueOn       *time.Time
	completedAt *time.Time
	completedBy *uuid.UUID
	createdBy   *uuid.UUID
	createdAt   time.Time
	updatedAt   time.Time
}

type questSnapshot struct {
	id          uuid.UUID
	householdID uuid.UUID
	libraryID   *uuid.UUID
	name        string
	points      int
	repeatDays  int
	notes       string
	claimedBy   *uuid.UUID
	claimedAt   *time.Time
	doneAt      *time.Time
	completedAt *time.Time
	availableAt *time.Time
	createdAt   time.Time
	updatedAt   time.Time
}

// TestIntegration_ChoresMigrateToQuests proves CLM-7:
//   - Migration 000010 copies every chores row into household_quests with the
//     same id and household, name=title and notes=description.
//   - Points are 0, repeat_days 1, nothing is claimed, and completed state is kept
//     (available_at = completed_at + 1 day).
//   - It is idempotent (running twice changes nothing and creates no duplicates).
//   - The chores table is left unchanged as the read-only archive of assignee,
//     due date, completed_by, and creator.
func TestIntegration_ChoresMigrateToQuests(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	upSQL := readMigration(t, "000010_quests_from_chores.up.sql")
	downSQL := readMigration(t, "000010_quests_from_chores.down.sql")

	// Create test user and household
	userID := uuid.Must(uuid.NewV7())
	email := "migration-test-" + userID.String()[:8] + "@example.com"
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, email_verified)
		VALUES ($1, $2, 'hash', 'Migration User', true)
	`, userID, email); err != nil {
		t.Fatalf("create test user: %v", err)
	}

	householdID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO households (id, name, timezone)
		VALUES ($1, 'Migration Test Household', 'UTC')
	`, householdID); err != nil {
		t.Fatalf("create test household: %v", err)
	}

	// Add membership so chores FK (household_id, assignee_id) is satisfied if present
	if _, err := pool.Exec(ctx, `
		INSERT INTO household_members (household_id, user_id, role)
		VALUES ($1, $2, 'admin')
		ON CONFLICT (household_id, user_id) DO NOTHING
	`, householdID, userID); err != nil {
		t.Fatalf("create household membership: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM households WHERE id = $1", householdID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", userID)
	})

	now := time.Now().UTC().Truncate(time.Microsecond)
	completedTime := now.Add(-3 * time.Hour)
	dueTime := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
	longDesc := strings.Repeat("A", 2000)

	// Chores to seed:
	// 1. open chore
	// 2. completed chore
	// 3. assigned chore
	// 4. due-dated chore
	// 5. 2000-char notes chore
	choreIDs := []uuid.UUID{
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
	}

	choresToInsert := []struct {
		id          uuid.UUID
		title       string
		description string
		assigneeID  *uuid.UUID
		dueOn       *time.Time
		completedAt *time.Time
		completedBy *uuid.UUID
		createdBy   *uuid.UUID
	}{
		{
			id:          choreIDs[0],
			title:       "Open Chore",
			description: "Open chore description",
			assigneeID:  nil,
			dueOn:       nil,
			completedAt: nil,
			completedBy: nil,
			createdBy:   &userID,
		},
		{
			id:          choreIDs[1],
			title:       "Completed Chore",
			description: "Completed chore description",
			assigneeID:  nil,
			dueOn:       nil,
			completedAt: &completedTime,
			completedBy: &userID,
			createdBy:   &userID,
		},
		{
			id:          choreIDs[2],
			title:       "Assigned Chore",
			description: "Assigned chore description",
			assigneeID:  &userID,
			dueOn:       nil,
			completedAt: nil,
			completedBy: nil,
			createdBy:   &userID,
		},
		{
			id:          choreIDs[3],
			title:       "Due Dated Chore",
			description: "Due dated chore description",
			assigneeID:  nil,
			dueOn:       &dueTime,
			completedAt: nil,
			completedBy: nil,
			createdBy:   &userID,
		},
		{
			id:          choreIDs[4],
			title:       "Chore With Max Notes",
			description: longDesc,
			assigneeID:  nil,
			dueOn:       nil,
			completedAt: nil,
			completedBy: nil,
			createdBy:   &userID,
		},
	}

	for _, c := range choresToInsert {
		if _, err := pool.Exec(ctx, `
			INSERT INTO chores (id, household_id, title, description, assignee_id, due_on, completed_at, completed_by, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		`, c.id, householdID, c.title, c.description, c.assigneeID, c.dueOn, c.completedAt, c.completedBy, c.createdBy, now); err != nil {
			t.Fatalf("insert chore %s (%s): %v", c.id, c.title, err)
		}
	}

	// Capture snapshot of chores before migration
	queryChores := func() map[uuid.UUID]choreSnapshot {
		rows, err := pool.Query(ctx, `
			SELECT id, household_id, title, description, assignee_id, due_on, completed_at, completed_by, created_by, created_at, updated_at
			FROM chores
			WHERE household_id = $1
		`, householdID)
		if err != nil {
			t.Fatalf("query chores: %v", err)
		}
		defer rows.Close()

		res := make(map[uuid.UUID]choreSnapshot)
		for rows.Next() {
			var cs choreSnapshot
			if err := rows.Scan(
				&cs.id, &cs.householdID, &cs.title, &cs.description,
				&cs.assigneeID, &cs.dueOn, &cs.completedAt, &cs.completedBy,
				&cs.createdBy, &cs.createdAt, &cs.updatedAt,
			); err != nil {
				t.Fatalf("scan chore: %v", err)
			}
			res[cs.id] = cs
		}
		return res
	}

	beforeChores := queryChores()
	if len(beforeChores) != len(choresToInsert) {
		t.Fatalf("expected %d chores inserted, got %d", len(choresToInsert), len(beforeChores))
	}

	// Run migration 000010 up (first time)
	if _, err := pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run 000010 up (first time): %v", err)
	}

	// Run migration 000010 up (second time — testing idempotency)
	if _, err := pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run 000010 up (second time): %v", err)
	}

	// Query household_quests
	queryQuests := func() map[uuid.UUID]questSnapshot {
		rows, err := pool.Query(ctx, `
			SELECT id, household_id, library_id, name, points, repeat_days, notes,
			       claimed_by, claimed_at, done_at, completed_at, available_at, created_at, updated_at
			FROM household_quests
			WHERE household_id = $1
		`, householdID)
		if err != nil {
			t.Fatalf("query quests: %v", err)
		}
		defer rows.Close()

		res := make(map[uuid.UUID]questSnapshot)
		for rows.Next() {
			var qs questSnapshot
			if err := rows.Scan(
				&qs.id, &qs.householdID, &qs.libraryID, &qs.name, &qs.points, &qs.repeatDays, &qs.notes,
				&qs.claimedBy, &qs.claimedAt, &qs.doneAt, &qs.completedAt, &qs.availableAt, &qs.createdAt, &qs.updatedAt,
			); err != nil {
				t.Fatalf("scan quest: %v", err)
			}
			res[qs.id] = qs
		}
		return res
	}

	quests := queryQuests()

	// Assert exactly one quest per chore (5 quests)
	if len(quests) != len(choresToInsert) {
		t.Fatalf("expected exactly %d quests for household, got %d", len(choresToInsert), len(quests))
	}

	// Assert field mappings for each chore
	for _, c := range choresToInsert {
		q, ok := quests[c.id]
		if !ok {
			t.Errorf("missing quest for chore %s (%s)", c.id, c.title)
			continue
		}

		if q.householdID != householdID {
			t.Errorf("quest %s household_id = %s, want %s", c.id, q.householdID, householdID)
		}
		if q.libraryID != nil {
			t.Errorf("quest %s library_id = %v, want nil", c.id, q.libraryID)
		}
		if q.name != c.title {
			t.Errorf("quest %s name = %q, want %q", c.id, q.name, c.title)
		}
		if q.notes != c.description {
			t.Errorf("quest %s notes length = %d, want %d", c.id, len(q.notes), len(c.description))
		}
		if q.points != 0 {
			t.Errorf("quest %s points = %d, want 0", c.id, q.points)
		}
		if q.repeatDays != 1 {
			t.Errorf("quest %s repeat_days = %d, want 1", c.id, q.repeatDays)
		}
		if q.claimedBy != nil {
			t.Errorf("quest %s claimed_by = %v, want nil", c.id, q.claimedBy)
		}
		if q.claimedAt != nil {
			t.Errorf("quest %s claimed_at = %v, want nil", c.id, q.claimedAt)
		}
		if q.doneAt != nil {
			t.Errorf("quest %s done_at = %v, want nil", c.id, q.doneAt)
		}

		if c.completedAt == nil {
			if q.completedAt != nil {
				t.Errorf("quest %s completed_at = %v, want nil", c.id, q.completedAt)
			}
			if q.availableAt != nil {
				t.Errorf("quest %s available_at = %v, want nil", c.id, q.availableAt)
			}
		} else {
			if q.completedAt == nil {
				t.Fatalf("quest %s completed_at is nil, want %v", c.id, *c.completedAt)
			}
			if !q.completedAt.Equal(*c.completedAt) {
				t.Errorf("quest %s completed_at = %v, want %v", c.id, *q.completedAt, *c.completedAt)
			}
			expectedAvailable := c.completedAt.Add(24 * time.Hour)
			if q.availableAt == nil {
				t.Fatalf("quest %s available_at is nil, want %v", c.id, expectedAvailable)
			}
			if !q.availableAt.Equal(expectedAvailable) {
				t.Errorf("quest %s available_at = %v, want %v", c.id, *q.availableAt, expectedAvailable)
			}
		}

		if !q.createdAt.Equal(now) {
			t.Errorf("quest %s created_at = %v, want %v", c.id, q.createdAt, now)
		}
		if !q.updatedAt.Equal(now) {
			t.Errorf("quest %s updated_at = %v, want %v", c.id, q.updatedAt, now)
		}
	}

	// Assert unchanged chores rows (read-only archive)
	afterChores := queryChores()
	if len(afterChores) != len(beforeChores) {
		t.Fatalf("chores count changed: before %d, after %d", len(beforeChores), len(afterChores))
	}
	for id, before := range beforeChores {
		after, ok := afterChores[id]
		if !ok {
			t.Errorf("chore %s vanished after migration", id)
			continue
		}
		if before.householdID != after.householdID ||
			before.title != after.title ||
			before.description != after.description ||
			!equalUUIDPtr(before.assigneeID, after.assigneeID) ||
			!equalTimePtr(before.dueOn, after.dueOn) ||
			!equalTimePtr(before.completedAt, after.completedAt) ||
			!equalUUIDPtr(before.completedBy, after.completedBy) ||
			!equalUUIDPtr(before.createdBy, after.createdBy) ||
			!before.createdAt.Equal(after.createdAt) ||
			!before.updatedAt.Equal(after.updatedAt) {
			t.Errorf("chore %s modified after migration: before %+v, after %+v", id, before, after)
		}
	}

	// Assert down migration reverts quest rows
	if _, err := pool.Exec(ctx, downSQL); err != nil {
		t.Fatalf("run 000010 down: %v", err)
	}
	questsAfterDown := queryQuests()
	if len(questsAfterDown) != 0 {
		t.Fatalf("expected 0 quests after down migration, got %d", len(questsAfterDown))
	}

	// Chores still intact after down migration
	afterDownChores := queryChores()
	if len(afterDownChores) != len(beforeChores) {
		t.Fatalf("chores count changed after down migration: before %d, after %d", len(beforeChores), len(afterDownChores))
	}
}

func equalUUIDPtr(a, b *uuid.UUID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
}
