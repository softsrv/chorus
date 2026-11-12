//go:build integration

package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

func TestIntegration_ComboBonus(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	combos := app.NewComboService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "comboadmin").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Combo Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	// 1. Empty list returns non-nil empty slice
	empty, err := combos.List(ctx, alice, h.ID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list: got %+v, err: %v (want non-nil empty slice)", empty, err)
	}

	// Create test quests
	q1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Chore 1", Points: num(10)}, "")
	if err != nil {
		t.Fatalf("create q1: %v", err)
	}
	q2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Chore 2", Points: num(15)}, "")
	if err != nil {
		t.Fatalf("create q2: %v", err)
	}
	q3, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Chore 3", Points: num(20)}, "")
	if err != nil {
		t.Fatalf("create q3: %v", err)
	}

	// 2. Validation (CLM-11)
	// Empty name + negative bonus + one chore -> all three field errors returned at once
	_, err = combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "   ",
		BonusPoints: num(-5),
		QuestIDs:    []uuid.UUID{q1.ID},
	}, "")
	var ve *app.ValidationError
	if !errors.As(err, &ve) || ve.Fields["name"] == "" || ve.Fields["bonus_points"] == "" || ve.Fields["quest_ids"] == "" {
		t.Fatalf("expected validation error with name, bonus_points, and quest_ids, got: %v", err)
	}

	// Duplicate-id pair that is really one chore is refused
	_, err = combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "Duplicate Quests",
		BonusPoints: num(10),
		QuestIDs:    []uuid.UUID{q1.ID, q1.ID},
	}, "")
	if !errors.As(err, &ve) || ve.Fields["quest_ids"] == "" {
		t.Fatalf("expected duplicate quest ids refused with quest_ids error, got: %v", err)
	}

	// Oversize name (>200 chars) is refused
	_, err = combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        strings.Repeat("a", 201),
		BonusPoints: num(10),
		QuestIDs:    []uuid.UUID{q1.ID, q2.ID},
	}, "")
	if !errors.As(err, &ve) || ve.Fields["name"] == "" {
		t.Fatalf("expected oversize name error, got: %v", err)
	}

	// List unchanged after rejected validations
	afterBad, err := combos.List(ctx, alice, h.ID)
	if err != nil || len(afterBad) != 0 {
		t.Fatalf("list after bad creates: got %+v (want empty)", afterBad)
	}

	// 3. Create (CLM-1)
	created, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "Morning Routine",
		BonusPoints: num(25),
		QuestIDs:    []uuid.UUID{q1.ID, q2.ID},
	}, "")
	if err != nil {
		t.Fatalf("create combo: %v", err)
	}
	if created.Name != "Morning Routine" || created.BonusPoints != 25 || len(created.QuestIDs) != 2 {
		t.Fatalf("unexpected created combo: %+v", created)
	}

	// 4. Get (CLM-1)
	got, err := combos.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get combo: %v", err)
	}
	if got.ID != created.ID || got.Name != "Morning Routine" || got.BonusPoints != 25 || len(got.QuestIDs) != 2 {
		t.Fatalf("unexpected get combo: %+v", got)
	}

	// 5. List (CLM-1)
	list, err := combos.List(ctx, alice, h.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list combos: got %d items, err: %v", len(list), err)
	}
	if list[0].ID != created.ID || len(list[0].QuestIDs) != 2 {
		t.Fatalf("unexpected combo in list: %+v", list[0])
	}

	// 6. PUT-replace (Update) (CLM-1)
	// Replace with different chores (q2, q3), new name, new bonus; old chore q1 gone
	updated, err := combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name:        "Evening Routine",
		BonusPoints: num(35),
		QuestIDs:    []uuid.UUID{q2.ID, q3.ID},
	}, "")
	if err != nil {
		t.Fatalf("update combo: %v", err)
	}
	if updated.Name != "Evening Routine" || updated.BonusPoints != 35 || len(updated.QuestIDs) != 2 {
		t.Fatalf("unexpected updated combo: %+v", updated)
	}
	for _, id := range updated.QuestIDs {
		if id == q1.ID {
			t.Fatalf("old chore q1 still present after update")
		}
	}

	// Check Get reflects the update
	gotUpdated, err := combos.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get updated combo: %v", err)
	}
	if gotUpdated.Name != "Evening Routine" || gotUpdated.BonusPoints != 35 {
		t.Fatalf("unexpected get after update: %+v", gotUpdated)
	}

	// 7. Admin PIN gating (CLM-9)
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "4321", ""); err != nil {
		t.Fatalf("set admin pin: %v", err)
	}
	// Non-admin user refused
	otherUser := registerVerified(t, pool, authSvc, mailer, "otheruser").User.ID
	if _, err := combos.Create(ctx, otherUser, h.ID, app.ComboInput{
		Name: "Other", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "4321"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("non-admin create: expected ErrNotFound, got %v", err)
	}
	if _, err := combos.Update(ctx, otherUser, h.ID, created.ID, app.ComboInput{
		Name: "Other", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "4321"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("non-admin update: expected ErrNotFound, got %v", err)
	}
	if err := combos.Delete(ctx, otherUser, h.ID, created.ID, "4321"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("non-admin delete: expected ErrNotFound, got %v", err)
	}

	// Missing PIN refused
	if _, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name: "Pin Missing", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Fatalf("missing pin create: expected ErrAdminPinRequired, got %v", err)
	}
	if _, err := combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name: "Pin Missing", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Fatalf("missing pin update: expected ErrAdminPinRequired, got %v", err)
	}
	if err := combos.Delete(ctx, alice, h.ID, created.ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Fatalf("missing pin delete: expected ErrAdminPinRequired, got %v", err)
	}

	// Wrong PIN refused
	if _, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name: "Pin Wrong", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "9999"); !errors.Is(err, app.ErrWrongPIN) {
		t.Fatalf("wrong pin create: expected ErrWrongPIN, got %v", err)
	}
	if _, err := combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name: "Pin Wrong", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "9999"); !errors.Is(err, app.ErrWrongPIN) {
		t.Fatalf("wrong pin update: expected ErrWrongPIN, got %v", err)
	}
	if err := combos.Delete(ctx, alice, h.ID, created.ID, "9999"); !errors.Is(err, app.ErrWrongPIN) {
		t.Fatalf("wrong pin delete: expected ErrWrongPIN, got %v", err)
	}

	// Verify combo is unchanged after all failed operations
	checkUnchanged, err := combos.Get(ctx, alice, h.ID, created.ID)
	if err != nil || checkUnchanged.Name != "Evening Routine" || checkUnchanged.BonusPoints != 35 {
		t.Fatalf("combo changed after failed pin calls: %+v", checkUnchanged)
	}

	// 8. Awards preserved on edit and delete (CLM-8)
	member, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Kid"}, "4321")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	initialPoints := member.Points

	awardID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new uuid: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO combo_awards (id, household_id, combo_id, member_id, combo_name, points, award_day, awarded_at)
		VALUES ($1, $2, $3, $4, $5, $6, CURRENT_DATE, NOW())
	`, awardID, h.ID, created.ID, member.ID, "Evening Routine Snapshot", 50)
	if err != nil {
		t.Fatalf("insert combo award: %v", err)
	}

	// Update the combo (change name and points again with valid PIN)
	_, err = combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name:        "Night Routine",
		BonusPoints: num(100),
		QuestIDs:    []uuid.UUID{q1.ID, q3.ID},
	}, "4321")
	if err != nil {
		t.Fatalf("update with pin: %v", err)
	}

	// Verify award row is untouched after update
	var awardName string
	var awardPoints int32
	var awardComboID *uuid.UUID
	err = pool.QueryRow(ctx, `SELECT combo_name, points, combo_id FROM combo_awards WHERE id = $1`, awardID).
		Scan(&awardName, &awardPoints, &awardComboID)
	if err != nil {
		t.Fatalf("query award after update: %v", err)
	}
	if awardName != "Evening Routine Snapshot" || awardPoints != 50 || awardComboID == nil || *awardComboID != created.ID {
		t.Fatalf("award changed after combo update: name=%s, points=%d, combo_id=%v", awardName, awardPoints, awardComboID)
	}

	// Delete the combo with valid PIN
	if err := combos.Delete(ctx, alice, h.ID, created.ID, "4321"); err != nil {
		t.Fatalf("delete combo with pin: %v", err)
	}

	// Get returns ErrNotFound
	if _, err := combos.Get(ctx, alice, h.ID, created.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("get deleted combo: expected ErrNotFound, got %v", err)
	}

	// Verify award row still exists, original snapshot preserved, combo_id is now NULL
	err = pool.QueryRow(ctx, `SELECT combo_name, points, combo_id FROM combo_awards WHERE id = $1`, awardID).
		Scan(&awardName, &awardPoints, &awardComboID)
	if err != nil {
		t.Fatalf("query award after delete: %v", err)
	}
	if awardName != "Evening Routine Snapshot" || awardPoints != 50 || awardComboID != nil {
		t.Fatalf("award after combo delete: name=%s, points=%d, combo_id=%v (want nil)", awardName, awardPoints, awardComboID)
	}

	// Verify member points are unchanged
	m, err := familySvc.Get(ctx, alice, h.ID, member.ID)
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if m.Points != initialPoints {
		t.Fatalf("member points changed: got %d, want %d", m.Points, initialPoints)
	}
}
