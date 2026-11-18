//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

func num(n int64) app.NumberField { return app.NumberField{Set: true, Value: n} }

// TestIntegration_QuestCRUD: the administrator's quest is stored with exactly
// the given name, points, repeat_days and notes, and List and Get return them.
func TestIntegration_QuestCRUD(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questadmin").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Quest Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	// A new household has no quests: an empty, non-nil list.
	empty, more, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10})
	if err != nil || empty == nil || len(empty) != 0 || more {
		t.Fatalf("empty list: %v %v %v", empty, more, err)
	}

	created, err := quests.Create(ctx, alice, h.ID, app.QuestInput{
		Name: "Mow the lawn", Points: num(25), RepeatDays: num(7), Notes: "Front and back",
	}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	want := func(label string, got db.HouseholdQuest, name string, points, days int32, notes string) {
		t.Helper()
		if got.ID != created.ID || got.HouseholdID != h.ID || got.Name != name || got.Points != points ||
			got.RepeatDays != days || got.Notes != notes || got.LibraryID != nil {
			t.Errorf("%s: got %+v", label, got)
		}
	}
	want("created", created.HouseholdQuest, "Mow the lawn", 25, 7, "Front and back")

	got, err := quests.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want("get", got.HouseholdQuest, "Mow the lawn", 25, 7, "Front and back")

	list, more, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10})
	if err != nil || len(list) != 1 || more {
		t.Fatalf("list: %d items, more=%v, err=%v", len(list), more, err)
	}
	want("list", list[0].HouseholdQuest, "Mow the lawn", 25, 7, "Front and back")

	updated, err := quests.Update(ctx, alice, h.ID, created.ID, app.QuestInput{
		Name: "Mow the lawn (front)", Points: num(30), RepeatDays: num(14), Notes: "",
	}, "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	want("updated", updated.HouseholdQuest, "Mow the lawn (front)", 30, 14, "")

	// CLM-1: an Update that omits streak_bonus keeps the stored one; an
	// explicit value (0 included) is written; another household's quest id is
	// ErrNotFound and leaves the quest as it was.
	bonusQuest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Feed the cat", Points: num(3), StreakBonus: num(7)}, "")
	if err != nil || bonusQuest.StreakBonus != 7 {
		t.Fatalf("create with streak_bonus 7: %+v %v", bonusQuest, err)
	}
	kept, err := quests.Update(ctx, alice, h.ID, bonusQuest.ID, app.QuestInput{Name: "Feed the cat", Points: num(4)}, "")
	if err != nil || kept.StreakBonus != 7 || kept.Points != 4 {
		t.Fatalf("update without streak_bonus: got bonus %d points %d, %v; want 7, 4", kept.StreakBonus, kept.Points, err)
	}
	if stored, err := quests.Get(ctx, alice, h.ID, bonusQuest.ID); err != nil || stored.StreakBonus != 7 {
		t.Fatalf("stored streak_bonus after omitted update: %d %v, want 7", stored.StreakBonus, err)
	}
	otherH, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Other Quest Family"})
	if err != nil {
		t.Fatalf("create other household: %v", err)
	}
	if _, err := quests.Update(ctx, alice, otherH.ID, bonusQuest.ID, app.QuestInput{Name: "Smuggled"}, ""); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("update via another household without streak_bonus: %v, want ErrNotFound", err)
	}
	if _, err := quests.Update(ctx, alice, otherH.ID, bonusQuest.ID, app.QuestInput{Name: "Smuggled", StreakBonus: num(1)}, ""); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("update via another household with streak_bonus: %v, want ErrNotFound", err)
	}
	if stored, err := quests.Get(ctx, alice, h.ID, bonusQuest.ID); err != nil || stored.Name != "Feed the cat" || stored.StreakBonus != 7 {
		t.Fatalf("quest changed by another household's update: %+v %v", stored, err)
	}
	cleared, err := quests.Update(ctx, alice, h.ID, bonusQuest.ID, app.QuestInput{Name: "Feed the cat", Points: num(4), StreakBonus: num(0)}, "")
	if err != nil || cleared.StreakBonus != 0 {
		t.Fatalf("update with streak_bonus 0: got %d %v, want 0", cleared.StreakBonus, err)
	}
	// An invalid streak_bonus is still a 422 naming the field.
	var bonusVE *app.ValidationError
	if _, err := quests.Update(ctx, alice, h.ID, bonusQuest.ID, app.QuestInput{Name: "Feed the cat", StreakBonus: num(-1)}, ""); !errors.As(err, &bonusVE) || bonusVE.Fields["streak_bonus"] == "" {
		t.Fatalf("negative streak_bonus on update: %v", err)
	}
	if err := quests.Delete(ctx, alice, h.ID, bonusQuest.ID, ""); err != nil {
		t.Fatalf("delete bonus quest: %v", err)
	}

	// Omitted numbers default to 0 points, every 1 day.
	defaulted, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Water plants"}, "")
	if err != nil || defaulted.Points != 0 || defaulted.RepeatDays != 1 {
		t.Fatalf("defaults: %+v %v", defaulted, err)
	}

	// Every bad field is reported at once, and nothing is saved.
	_, err = quests.Create(ctx, alice, h.ID, app.QuestInput{
		Name: "  ", Points: num(-1), RepeatDays: num(0),
	}, "")
	var ve *app.ValidationError
	if !errors.As(err, &ve) || ve.Fields["name"] == "" || ve.Fields["points"] == "" || ve.Fields["repeat_days"] == "" {
		t.Fatalf("invalid create: %v", err)
	}
	_, err = quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "x", Points: app.NumberField{Set: true, Invalid: true}}, "")
	if !errors.As(err, &ve) || ve.Fields["points"] == "" {
		t.Fatalf("non-numeric points: %v", err)
	}
	// notes > 2000 chars rejected
	tooLongNotes := strings.Repeat("n", 2001)
	_, err = quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Valid Name", Notes: tooLongNotes}, "")
	if !errors.As(err, &ve) || ve.Fields["notes"] == "" {
		t.Fatalf("notes > 2000 chars create not rejected: %v", err)
	}
	if list, _, _ := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10}); len(list) != 2 {
		t.Fatalf("rejected creates saved rows: %d quests", len(list))
	}

	// Update with empty name, points -1, repeat_days 0 rejected together, and row is unchanged
	beforeInvalidUpdate, err := quests.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get before invalid update: %v", err)
	}
	_, err = quests.Update(ctx, alice, h.ID, created.ID, app.QuestInput{
		Name: " ", Points: num(-1), RepeatDays: num(0),
	}, "")
	if !errors.As(err, &ve) || ve.Fields["name"] == "" || ve.Fields["points"] == "" || ve.Fields["repeat_days"] == "" {
		t.Fatalf("invalid update: %v", err)
	}
	// notes > 2000 chars on update rejected
	_, err = quests.Update(ctx, alice, h.ID, created.ID, app.QuestInput{
		Name: "Still Valid", Notes: tooLongNotes,
	}, "")
	if !errors.As(err, &ve) || ve.Fields["notes"] == "" {
		t.Fatalf("notes > 2000 chars update not rejected: %v", err)
	}
	afterInvalidUpdate, err := quests.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get after invalid update: %v", err)
	}
	if !reflect.DeepEqual(beforeInvalidUpdate, afterInvalidUpdate) {
		t.Fatalf("row changed after rejected update: %+v -> %+v", beforeInvalidUpdate, afterInvalidUpdate)
	}

	if err := quests.Delete(ctx, alice, h.ID, created.ID, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := quests.Get(ctx, alice, h.ID, created.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := quests.Delete(ctx, alice, h.ID, created.ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// TestIntegration_QuestCopyFromLibrary: a copy arrives with the library's
// values and its library_id; editing it leaves the library unchanged.
func TestIntegration_QuestCopyFromLibrary(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questcopy").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Copy Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	library, err := quests.ListLibrary(ctx)
	if err != nil || len(library) == 0 {
		t.Fatalf("library: %d items, %v", len(library), err)
	}

	// CLM-2: ensure a test copies the seeded 'Wash the car' (expect points 40, repeat_days 30),
	// updates the copy, then ListLibrary still returns original values.
	var washCar *db.QuestLibrary
	for i := range library {
		if library[i].Name == "Wash the car" {
			washCar = &library[i]
			break
		}
	}
	if washCar == nil {
		t.Fatal("seeded 'Wash the car' not found in library")
	}
	if washCar.Points != 40 || washCar.RepeatDays != 30 {
		t.Fatalf("'Wash the car' expected points 40, repeat_days 30, got %d, %d", washCar.Points, washCar.RepeatDays)
	}

	copied, err := quests.CopyFromLibrary(ctx, alice, h.ID, washCar.ID, "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if copied.LibraryID == nil || *copied.LibraryID != washCar.ID || copied.Name != washCar.Name ||
		copied.Points != 40 || copied.RepeatDays != 30 || copied.Notes != washCar.Notes {
		t.Fatalf("copy %+v does not match library %+v", copied, washCar)
	}
	// CLM-15: a library copy is not shareable and has no teamwork bonus,
	// as stored, not only as returned.
	if copied.Shareable || copied.TeamworkBonus != 0 {
		t.Fatalf("copy shareable=%v teamwork_bonus=%d, want false and 0", copied.Shareable, copied.TeamworkBonus)
	}
	if stored, err := quests.Get(ctx, alice, h.ID, copied.ID); err != nil || stored.Shareable || stored.TeamworkBonus != 0 {
		t.Fatalf("stored copy shareable=%v teamwork_bonus=%d (err %v), want false and 0", stored.Shareable, stored.TeamworkBonus, err)
	}

	// CLM-3: CopyFromLibrary-edit path invalid inputs are rejected and leave row unchanged
	var ve *app.ValidationError
	_, err = quests.Update(ctx, alice, h.ID, copied.ID, app.QuestInput{
		Name: "", Points: num(-1), RepeatDays: num(0),
	}, "")
	if !errors.As(err, &ve) || ve.Fields["name"] == "" || ve.Fields["points"] == "" || ve.Fields["repeat_days"] == "" {
		t.Fatalf("invalid update on copy: %v", err)
	}
	unchangedCopy, err := quests.Get(ctx, alice, h.ID, copied.ID)
	if err != nil {
		t.Fatalf("get copy after rejected update: %v", err)
	}
	if unchangedCopy.Name != washCar.Name || unchangedCopy.Points != 40 || unchangedCopy.RepeatDays != 30 {
		t.Fatalf("copy changed after rejected update: %+v", unchangedCopy)
	}

	if _, err := quests.Update(ctx, alice, h.ID, copied.ID, app.QuestInput{
		Name: "Our own version", Points: num(int64(washCar.Points) + 100), RepeatDays: num(14), Notes: "edited",
	}, ""); err != nil {
		t.Fatalf("edit copy: %v", err)
	}
	after, err := q.GetQuestLibraryItem(ctx, washCar.ID)
	if err != nil {
		t.Fatalf("reload library item: %v", err)
	}
	if after.Name != washCar.Name || after.Points != washCar.Points || after.RepeatDays != washCar.RepeatDays || after.Notes != washCar.Notes {
		t.Fatalf("library changed by editing a copy: %+v -> %+v", washCar, after)
	}

	libraryAfter, err := quests.ListLibrary(ctx)
	if err != nil {
		t.Fatalf("list library after edit: %v", err)
	}
	var washCarAfter *db.QuestLibrary
	for i := range libraryAfter {
		if libraryAfter[i].ID == washCar.ID {
			washCarAfter = &libraryAfter[i]
			break
		}
	}
	if washCarAfter == nil || washCarAfter.Name != "Wash the car" || washCarAfter.Points != 40 || washCarAfter.RepeatDays != 30 {
		t.Fatalf("ListLibrary returned modified values for 'Wash the car': %+v", washCarAfter)
	}

	if _, err := quests.CopyFromLibrary(ctx, alice, h.ID, h.ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("copy of unknown library id: %v", err)
	}
}

// TestIntegration_QuestLibraryGlobalAndReadOnly proves CLM-4:
// - The quest library is served from a global table with no household_id.
// - It is identical for every household.
// - No route or service method writes to it.
// - After one household copies and edits, the other still sees the original.
func TestIntegration_QuestLibraryGlobalAndReadOnly(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	// 1. Verify quest_library table has no household_id column (query information_schema)
	var colCount int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'quest_library' AND column_name = 'household_id'
	`).Scan(&colCount)
	if err != nil {
		t.Fatalf("query information_schema for quest_library: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("quest_library table has household_id column; must have none")
	}

	alice := registerVerified(t, pool, authSvc, mailer, "libalice").User.ID
	bob := registerVerified(t, pool, authSvc, mailer, "libbob").User.ID

	hAlice, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Alice House"})
	if err != nil {
		t.Fatalf("create alice household: %v", err)
	}
	hBob, err := households.Create(ctx, bob, app.HouseholdInput{Name: "Bob House"})
	if err != nil {
		t.Fatalf("create bob household: %v", err)
	}

	// Two households list identical library rows
	aliceLib, err := quests.ListLibrary(ctx)
	if err != nil {
		t.Fatalf("alice list library: %v", err)
	}
	bobLib, err := quests.ListLibrary(ctx)
	if err != nil {
		t.Fatalf("bob list library: %v", err)
	}
	if len(aliceLib) != len(bobLib) {
		t.Fatalf("library mismatch: alice %d, bob %d", len(aliceLib), len(bobLib))
	}
	for i := range aliceLib {
		if aliceLib[i] != bobLib[i] {
			t.Fatalf("item %d mismatch: alice %+v, bob %+v", i, aliceLib[i], bobLib[i])
		}
	}

	// After Alice copies and edits, Bob still sees the original
	target := aliceLib[0]
	copyAlice, err := quests.CopyFromLibrary(ctx, alice, hAlice.ID, target.ID, "")
	if err != nil {
		t.Fatalf("alice copy: %v", err)
	}
	if _, err := quests.Update(ctx, alice, hAlice.ID, copyAlice.ID, app.QuestInput{
		Name: "Pwned Library", Points: num(999), RepeatDays: num(99), Notes: "Hacked",
	}, ""); err != nil {
		t.Fatalf("alice update copy: %v", err)
	}

	bobLibAfter, err := quests.ListLibrary(ctx)
	if err != nil {
		t.Fatalf("bob list library after edit: %v", err)
	}
	// Bob's own copy starts from the library's values, not Alice's edit.
	bobCopy, err := quests.CopyFromLibrary(ctx, bob, hBob.ID, target.ID, "")
	if err != nil {
		t.Fatalf("bob copy: %v", err)
	}
	if bobCopy.Name != target.Name || bobCopy.Points != target.Points || bobCopy.RepeatDays != target.RepeatDays {
		t.Fatalf("bob's copy = %+v, want library values %+v", bobCopy, target)
	}
	for _, item := range bobLibAfter {
		if item.ID == target.ID {
			if item.Name != target.Name || item.Points != target.Points || item.RepeatDays != target.RepeatDays || item.Notes != target.Notes {
				t.Fatalf("bob sees modified library item: %+v vs original %+v", item, target)
			}
		}
	}
}

// TestIntegration_RewardLibraryAdoptAndGlobal: the reward library is one
// global, read-only table (no household_id) that every household reads the
// same, seeded with 6 rewards. Adopting copies its description, cost and
// library_id; editing the copy leaves the library and other households'
// adoptions untouched; an unknown library id is ErrNotFound.
func TestIntegration_RewardLibraryAdoptAndGlobal(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	rewards := app.NewRewardService(q, pool)
	ctx := context.Background()

	var colCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'reward_library' AND column_name = 'household_id'
	`).Scan(&colCount); err != nil {
		t.Fatalf("query information_schema for reward_library: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("reward_library table has a household_id column; must have none")
	}

	alice := registerVerified(t, pool, authSvc, mailer, "rewardlibalice").User.ID
	bob := registerVerified(t, pool, authSvc, mailer, "rewardlibbob").User.ID
	hAlice, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Alice Rewards"})
	if err != nil {
		t.Fatalf("create alice household: %v", err)
	}
	hBob, err := households.Create(ctx, bob, app.HouseholdInput{Name: "Bob Rewards"})
	if err != nil {
		t.Fatalf("create bob household: %v", err)
	}

	before, err := rewards.ListLibrary(ctx)
	if err != nil {
		t.Fatalf("list reward library: %v", err)
	}
	if len(before) != 6 {
		t.Fatalf("reward library count: got %d, want 6", len(before))
	}
	var ten *db.RewardLibrary
	for i := range before {
		if i > 0 && before[i-1].Cost > before[i].Cost {
			t.Errorf("reward library not ordered by cost: %+v before %+v", before[i-1], before[i])
		}
		if before[i].Description == "$10" {
			ten = &before[i]
		}
	}
	if ten == nil || ten.Cost != 1000 {
		t.Fatalf("seeded '$10' reward = %+v, want cost 1000", ten)
	}

	adopted, err := rewards.CopyFromLibrary(ctx, alice, hAlice.ID, ten.ID, "")
	if err != nil {
		t.Fatalf("adopt '$10': %v", err)
	}
	if adopted.HouseholdID != hAlice.ID || adopted.LibraryID == nil || *adopted.LibraryID != ten.ID ||
		adopted.Description != "$10" || adopted.Cost != 1000 {
		t.Fatalf("adopted reward = %+v, want '$10' / 1000 from library %s", adopted, ten.ID)
	}
	edited, err := rewards.Update(ctx, alice, hAlice.ID, adopted.ID, app.RewardInput{Description: "Twenty dollars", Cost: num(2000)}, "")
	if err != nil {
		t.Fatalf("edit adopted reward: %v", err)
	}
	if edited.Description != "Twenty dollars" || edited.Cost != 2000 || edited.LibraryID == nil || *edited.LibraryID != ten.ID {
		t.Fatalf("edited reward = %+v", edited)
	}

	// Bob's adoption starts from the library's values, not Alice's edit.
	bobCopy, err := rewards.CopyFromLibrary(ctx, bob, hBob.ID, ten.ID, "")
	if err != nil {
		t.Fatalf("bob adopt '$10': %v", err)
	}
	if bobCopy.Description != "$10" || bobCopy.Cost != 1000 {
		t.Fatalf("bob's copy = %+v, want '$10' / 1000", bobCopy)
	}

	after, err := rewards.ListLibrary(ctx)
	if err != nil {
		t.Fatalf("list reward library after edits: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("library size changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("library row %d changed: %+v -> %+v", i, before[i], after[i])
		}
	}

	if _, err := rewards.CopyFromLibrary(ctx, alice, hAlice.ID, uuid.New(), ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("adopt unknown library id: got %v, want ErrNotFound", err)
	}
	if list, err := rewards.List(ctx, alice, hAlice.ID); err != nil || len(list) != 1 {
		t.Errorf("alice's rewards = %+v, %v; want exactly the one adopted", list, err)
	}
}

// TestIntegration_LibrarySeeded proves CLM-9:
// - ListLibrary has exactly 14 quests including 'Wash the car' (40 points, 30 days), 'Make your bed'.
// - reward_library table has exactly 6 rows (query via pool SQL) including '$10' cost 1000 and 'Choose the game on family game night' 200.
// - Copying every seeded quest into a household succeeds.
func TestIntegration_LibrarySeeded(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "libraryseeded").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Library Seeded Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	library, err := quests.ListLibrary(ctx)
	if err != nil {
		t.Fatalf("list library: %v", err)
	}
	if len(library) != 14 {
		t.Fatalf("quest library count: got %d, want 14", len(library))
	}

	var foundWashCar, foundMakeBed bool
	for _, item := range library {
		if item.Name == "Wash the car" {
			foundWashCar = true
			if item.Points != 40 || item.RepeatDays != 30 {
				t.Errorf("'Wash the car' expected points 40, repeat_days 30, got %d, %d", item.Points, item.RepeatDays)
			}
		}
		if item.Name == "Make your bed" {
			foundMakeBed = true
		}
	}
	if !foundWashCar {
		t.Errorf("seeded quest 'Wash the car' not found")
	}
	if !foundMakeBed {
		t.Errorf("seeded quest 'Make your bed' not found")
	}

	// Reward library has exactly 6 rows
	var rewardCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM reward_library").Scan(&rewardCount); err != nil {
		t.Fatalf("query reward_library count: %v", err)
	}
	if rewardCount != 6 {
		t.Fatalf("reward library count: got %d, want 6", rewardCount)
	}

	var cost10 int
	if err := pool.QueryRow(ctx, "SELECT cost FROM reward_library WHERE description = '$10'").Scan(&cost10); err != nil {
		t.Fatalf("query '$10' reward: %v", err)
	}
	if cost10 != 1000 {
		t.Errorf("'$10' reward cost: got %d, want 1000", cost10)
	}

	var costGame int
	if err := pool.QueryRow(ctx, "SELECT cost FROM reward_library WHERE description = 'Choose the game on family game night'").Scan(&costGame); err != nil {
		t.Fatalf("query 'Choose the game on family game night' reward: %v", err)
	}
	if costGame != 200 {
		t.Errorf("'Choose the game on family game night' reward cost: got %d, want 200", costGame)
	}

	// Copying every seeded quest into a household succeeds
	for _, item := range library {
		copied, err := quests.CopyFromLibrary(ctx, alice, h.ID, item.ID, "")
		if err != nil {
			t.Fatalf("copy quest %q (%s): %v", item.Name, item.ID, err)
		}
		if copied.LibraryID == nil || *copied.LibraryID != item.ID || copied.Name != item.Name ||
			copied.Points != item.Points || copied.RepeatDays != item.RepeatDays || copied.Notes != item.Notes {
			t.Fatalf("copied quest %+v mismatch with library %+v", copied, item)
		}
	}
}

// TestIntegration_QuestWritesNeedAdminPin: once an admin PIN is set, quest
// writes need it; a missing or wrong PIN changes nothing.
func TestIntegration_QuestWritesNeedAdminPin(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questpin").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Pin Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	quest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Dust", Points: num(3)}, "")
	if err != nil {
		t.Fatalf("create before pin: %v", err)
	}
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "2468", ""); err != nil {
		t.Fatalf("set admin pin: %v", err)
	}

	in := app.QuestInput{Name: "Changed", Points: num(99)}
	if _, err := quests.Create(ctx, alice, h.ID, in, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("create without pin: %v", err)
	}
	if _, err := quests.Update(ctx, alice, h.ID, quest.ID, in, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("update with wrong pin: %v", err)
	}
	if err := quests.Delete(ctx, alice, h.ID, quest.ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("delete with wrong pin: %v", err)
	}
	if got, err := quests.Get(ctx, alice, h.ID, quest.ID); err != nil || got.Name != "Dust" || got.Points != 3 {
		t.Fatalf("quest changed without the pin: %+v %v", got, err)
	}
	if _, err := quests.Update(ctx, alice, h.ID, quest.ID, in, "2468"); err != nil {
		t.Fatalf("update with right pin: %v", err)
	}
}

// TestIntegration_QuestAccept: a verified acting member accepts an available
// quest; it is then theirs, carries its notes, and no other member sees it as
// available. A wrong PIN or a member from another household changes nothing.
func TestIntegration_QuestAccept(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questaccept").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Accept Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	other, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Other Family"})
	if err != nil {
		t.Fatalf("create other household: %v", err)
	}
	memberA, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ann", PIN: "1111"}, "")
	if err != nil {
		t.Fatalf("create member A: %v", err)
	}
	memberB, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ben"}, "")
	if err != nil {
		t.Fatalf("create member B: %v", err)
	}
	outsider, err := familySvc.Create(ctx, alice, other.ID, app.FamilyMemberInput{Name: "Otto"}, "")
	if err != nil {
		t.Fatalf("create outsider member: %v", err)
	}
	quest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Feed the cat", Points: num(5), Notes: "Half a scoop"}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	if !app.QuestAvailable(quest.HouseholdQuest, time.Now()) {
		t.Fatalf("a new quest is not available: %+v", quest)
	}

	// A wrong PIN, or a member of another household, cannot accept it.
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, app.MemberAction{MemberID: memberA.ID, PIN: "9999"}); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("accept with wrong pin: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, app.MemberAction{MemberID: outsider.ID}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("accept as another household's member: %v", err)
	}
	if got, err := quests.Get(ctx, alice, h.ID, quest.ID); err != nil || got.ClaimedBy != nil {
		t.Fatalf("quest claimed by a refused accept: %+v %v", got, err)
	}

	accepted, err := quests.Accept(ctx, alice, h.ID, quest.ID, app.MemberAction{MemberID: memberA.ID, PIN: "1111"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.ClaimedBy == nil || *accepted.ClaimedBy != memberA.ID || !accepted.ClaimedAt.Valid ||
		accepted.Notes != "Half a scoop" || app.QuestAvailable(accepted.HouseholdQuest, time.Now()) {
		t.Errorf("accepted quest: %+v", accepted)
	}

	// The list marks it unavailable, so it is in no member's available set.
	list, _, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list[0].ClaimedBy == nil || *list[0].ClaimedBy != memberA.ID || app.QuestAvailable(list[0].HouseholdQuest, time.Now()) {
		t.Errorf("listed quest still available: %+v", list[0])
	}

	// Another member accepting it is told it is taken, and A keeps it.
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, app.MemberAction{MemberID: memberB.ID}); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("second accept: %v, want ErrQuestTaken", err)
	}
	if got, err := quests.Get(ctx, alice, h.ID, quest.ID); err != nil || got.ClaimedBy == nil || *got.ClaimedBy != memberA.ID {
		t.Errorf("quest after second accept: %+v %v", got, err)
	}
}

// TestIntegration_QuestAcceptConcurrent: concurrent accepts of one available
// quest by different members result in exactly one success (nil error) and N-1
// ErrQuestTaken errors, and the quest is held by the winner.
func TestIntegration_QuestAcceptConcurrent(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questacceptconc").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Accept Conc Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	const n = 8
	members := make([]app.Member, n)
	for i := range n {
		m, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: fmt.Sprintf("Member %d", i)}, "")
		if err != nil {
			t.Fatalf("create member %d: %v", i, err)
		}
		members[i] = m
	}

	quest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Feed the cat", Points: num(5), Notes: "Half a scoop"}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}

	start := make(chan struct{})
	type result struct {
		memberID uuid.UUID
		quest    app.QuestView
		err      error
	}
	results := make(chan result, n)
	for i := range n {
		go func(m app.Member) {
			<-start
			qRow, err := quests.Accept(ctx, alice, h.ID, quest.ID, app.MemberAction{MemberID: m.ID})
			results <- result{memberID: m.ID, quest: qRow, err: err}
		}(members[i])
	}
	close(start)

	var wins, taken int
	var winnerID uuid.UUID
	for range n {
		r := <-results
		switch {
		case r.err == nil:
			wins++
			winnerID = r.memberID
		case errors.Is(r.err, app.ErrQuestTaken):
			taken++
		default:
			t.Errorf("concurrent accept: unexpected error %v", r.err)
		}
	}
	if wins != 1 || taken != n-1 {
		t.Fatalf("concurrent accepts: %d won, %d taken; want 1 and %d", wins, taken, n-1)
	}

	got, err := quests.Get(ctx, alice, h.ID, quest.ID)
	if err != nil {
		t.Fatalf("get quest: %v", err)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != winnerID {
		t.Errorf("quest claimed_by: got %v, want winner %v", got.ClaimedBy, winnerID)
	}
}

// TestIntegration_QuestApproval walks a quest through its new lifecycle:
// a member accepts, abandons, another takes it, marks it done (releasing the
// holder and starting cooldown), the admin rejects it (cancelling cooldown)
// and approves it. Points move only on approval.
func TestIntegration_QuestApproval(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	combos := app.NewComboService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questapprove").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Approval Family", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	ann, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ann", PIN: "1111"}, "")
	if err != nil {
		t.Fatalf("create Ann: %v", err)
	}
	ben, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ben"}, "")
	if err != nil {
		t.Fatalf("create Ben: %v", err)
	}
	asAnn := app.MemberAction{MemberID: ann.ID, PIN: "1111"}
	asBen := app.MemberAction{MemberID: ben.ID}
	quest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Mow the lawn", Points: num(7), RepeatDays: num(3)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	points := func(memberID uuid.UUID) int32 {
		t.Helper()
		m, err := q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: h.ID, ID: memberID})
		if err != nil {
			t.Fatalf("get member: %v", err)
		}
		return m.Points
	}
	get := func() app.QuestView {
		t.Helper()
		got, err := quests.Get(ctx, alice, h.ID, quest.ID)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return got
	}
	pending := func(adminPin string) []app.PendingCompletion {
		t.Helper()
		rows, err := quests.ListPending(ctx, alice, h.ID, adminPin)
		if err != nil || rows == nil {
			t.Fatalf("list pending: %v %v", rows, err)
		}
		return rows
	}
	awards := func(memberID uuid.UUID) []db.PointsAward {
		t.Helper()
		rows, err := q.ListMemberPointsAwards(ctx, db.ListMemberPointsAwardsParams{HouseholdID: h.ID, MemberID: memberID, Limit: 100, Offset: 0})
		if err != nil {
			t.Fatalf("list points awards: %v", err)
		}
		return rows
	}

	// Declining is client-side: reading the quest writes nothing.
	if got := get(); got.ClaimedBy != nil || !app.QuestAvailable(got.HouseholdQuest, time.Now()) || got.AwaitingApproval {
		t.Fatalf("a read changed the quest: %+v", got)
	}
	if len(pending("")) != 0 {
		t.Error("a new household has quests waiting")
	}
	// Nobody holds it, so nobody can mark it done.
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asAnn); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mark done unclaimed: %v, want ErrNotFound", err)
	}

	// Ann accepts. Ben can neither abandon nor finish Ann's quest.
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("Ann accepts: %v", err)
	}
	if _, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Ben abandons Ann's quest: %v, want ErrNotFound", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Ben marks Ann's quest done: %v, want ErrNotFound", err)
	}
	if _, err := quests.Abandon(ctx, alice, h.ID, quest.ID, app.MemberAction{MemberID: ann.ID, PIN: "9999"}); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Ann abandons with a wrong PIN: %v, want ErrWrongPIN", err)
	}
	if got := get(); got.ClaimedBy == nil || *got.ClaimedBy != ann.ID {
		t.Errorf("after refused actions: claimed_by %v, want Ann %v", got.ClaimedBy, ann.ID)
	}

	// Ann abandons: the quest is back in the pool and Ben can take it.
	abandoned, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asAnn)
	if err != nil || abandoned.ClaimedBy != nil || abandoned.ClaimedAt.Valid || !app.QuestAvailable(abandoned.HouseholdQuest, time.Now()) {
		t.Fatalf("Ann abandons: %+v %v", abandoned, err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("Ben accepts after abandon: %v", err)
	}

	// Ben marks it done: it releases holder, starts cooldown, and records a pending completion.
	// No points move yet.
	doneView, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen)
	if err != nil || doneView.ClaimedBy != nil || doneView.ClaimedAt.Valid || !doneView.CompletedAt.Valid ||
		!doneView.AvailableAt.Valid || !doneView.AwaitingApproval {
		t.Fatalf("Ben marks done: %+v %v", doneView, err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after mark done: %d, want 0", p)
	}
	// Ben no longer holds it, so marking done again is ErrNotFound.
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mark done twice: %v, want ErrNotFound", err)
	}
	rows := pending("")
	if len(rows) != 1 || rows[0].QuestID != quest.ID || rows[0].MemberName != "Ben" ||
		rows[0].Points != 7 || rows[0].MemberID != ben.ID {
		t.Errorf("pending: %+v", rows)
	}
	compID := rows[0].ID
	// Once marked done, the member no longer holds it, so abandon is ErrNotFound.
	if _, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("abandon after mark done: %v, want ErrNotFound", err)
	}

	// Sent back (Reject): rejects only this pending completion, no points.
	// Since this completion started the running cooldown, cooldown is cancelled (available_at <= NOW()).
	rejected, err := quests.Reject(ctx, alice, h.ID, compID, "")
	if err != nil || rejected.ID != compID || rejected.Status != "rejected" || rejected.QuestID != quest.ID {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after reject: %d, want 0", p)
	}
	if got := awards(ben.ID); len(got) != 0 {
		t.Errorf("points awards after reject: %+v, want none", got)
	}
	if len(pending("")) != 0 {
		t.Error("a rejected completion is still waiting")
	}
	if got := get(); !app.QuestAvailable(got.HouseholdQuest, time.Now()) || got.ClaimedBy != nil {
		t.Errorf("after reject, quest should be available immediately: %+v", got)
	}
	// Re-rejecting or approving a non-pending completion is ErrNotFound.
	if _, err := quests.Reject(ctx, alice, h.ID, compID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("reject completion not pending: %v, want ErrNotFound", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, compID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("approve completion not pending: %v, want ErrNotFound", err)
	}

	// Ben accepts again and marks done again.
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("accept after reject: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("mark done again after reject: %v", err)
	}
	rows = pending("")
	if len(rows) != 1 {
		t.Fatalf("pending after second mark done: %d rows", len(rows))
	}
	comp2ID := rows[0].ID

	// Back-date done_at a day before approval to prove history entry earned_at uses approved_at, not done_at (CLM-3).
	backDatedDoneAt := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if _, err := pool.Exec(ctx, `UPDATE quest_completions SET done_at = $2 WHERE id = $1`, comp2ID, backDatedDoneAt); err != nil {
		t.Fatalf("back-date completion: %v", err)
	}

	// With an admin PIN set, a missing or wrong PIN changes nothing.
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "2468", ""); err != nil {
		t.Fatalf("set admin pin: %v", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("approve without admin pin: %v, want ErrAdminPinRequired", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("approve with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if _, err := quests.Reject(ctx, alice, h.ID, comp2ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("reject with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if _, err := quests.Reject(ctx, alice, h.ID, comp2ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("reject without admin pin: %v, want ErrAdminPinRequired", err)
	}
	if _, err := quests.ListPending(ctx, alice, h.ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("pending with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after refused approvals: %d, want 0", p)
	}

	// Transaction atomicity: A failure after completion update rolls back both:
	// a trigger makes the points credit fail, so completion is still pending with no points.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION test_fail_points() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'points credit failed'; END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fail_points() CASCADE`) })
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_fail_points BEFORE UPDATE OF points ON family_members
		FOR EACH ROW WHEN (NEW.id = '`+ben.ID.String()+`') EXECUTE FUNCTION test_fail_points()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "2468"); err == nil {
		t.Fatal("approve succeeded although the points credit failed")
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION test_fail_points() CASCADE`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if len(pending("2468")) != 1 {
		t.Error("completion was not kept pending after failed approval")
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after a failed approval: %d, want 0", p)
	}
	if got := awards(ben.ID); len(got) != 0 {
		t.Errorf("a failed approval left history entries: %+v", got)
	}

	// CLM-1: N goroutines approve one completion at once: exactly one credits points,
	// N-1 get ErrNotFound.
	const N = 5
	start := make(chan struct{})
	type result struct {
		comp   app.Completion
		member db.FamilyMember
		err    error
	}
	results := make(chan result, N)
	for range N {
		go func() {
			<-start
			c, m, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "2468")
			results <- result{c, m, err}
		}()
	}
	close(start)
	var wins, notFound int
	for range N {
		r := <-results
		switch {
		case r.err == nil:
			wins++
			if r.member.ID != ben.ID || r.member.Points != 7 {
				t.Errorf("credited member: %+v, want Ben with 7 points", r.member)
			}
			if r.comp.ID != comp2ID || r.comp.Status != "approved" || r.comp.ApprovedAt == nil {
				t.Errorf("approved completion: %+v", r.comp)
			}
		case errors.Is(r.err, app.ErrNotFound):
			notFound++
		default:
			t.Errorf("concurrent approve: %v", r.err)
		}
	}
	if wins != 1 || notFound != N-1 {
		t.Errorf("concurrent approvals: %d won, %d not found; want 1 and %d", wins, notFound, N-1)
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("Ben's points after approval: %d, want 7", p)
	}
	// Approving again credits nothing more.
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "2468"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("repeat approve: %v, want ErrNotFound", err)
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("Ben's points after repeat approve: %d, want 7", p)
	}
	// The concurrent and repeated approvals wrote exactly one history entry,
	// for the credited points, earned at the completion's approval time.
	benAwards := awards(ben.ID)
	if len(benAwards) != 1 {
		t.Fatalf("Ben's history after approval: %d entries, want 1: %+v", len(benAwards), benAwards)
	}
	var markedDoneAt, approvedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT done_at, approved_at FROM quest_completions WHERE id = $1`, comp2ID).Scan(&markedDoneAt, &approvedAt); err != nil {
		t.Fatalf("read completion timestamps: %v", err)
	}
	if approvedAt.Equal(markedDoneAt) {
		t.Fatalf("approved_at %v should differ from done_at %v", approvedAt, markedDoneAt)
	}
	if a := benAwards[0]; a.HouseholdID != h.ID || a.MemberID != ben.ID || a.Kind != "quest" || a.Reason != "Mow the lawn" ||
		a.Points != 7 || !a.EarnedAt.Valid || !a.EarnedAt.Time.Equal(approvedAt) || a.EarnedAt.Time.Equal(markedDoneAt) {
		t.Errorf("Ben's history entry: %+v, want quest 'Mow the lawn' 7 points earned at approved_at %v (not done_at %v)", a, approvedAt, markedDoneAt)
	}
	if len(pending("2468")) != 0 {
		t.Error("approved completion is still waiting")
	}

	// Clear admin PIN for multi-day and combo tests.
	if err := familySvc.ClearAdminPin(ctx, alice, h.ID, "2468"); err != nil {
		t.Fatalf("clear admin pin: %v", err)
	}

	// CLM-3 & CLM-4: Four rounds of accept + mark done on days 1, 2, 3, 4 with no approval
	// in between; each mark done adds a distinct pending completion; approving all 4 credits 4x.
	chicagoTZ, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	nowChicago := time.Now().In(chicagoTZ)
	var markedTimes []time.Time
	for day := 1; day <= 4; day++ {
		// Ensure quest is available to accept
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, quest.ID); err != nil {
			t.Fatalf("make quest available: %v", err)
		}
		if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
			t.Fatalf("round %d accept: %v", day, err)
		}
		doneAtTime := time.Date(nowChicago.Year(), nowChicago.Month(), nowChicago.Day()-(5-day), 10, 0, 0, 0, chicagoTZ)
		markedTimes = append(markedTimes, doneAtTime)

		if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); err != nil {
			t.Fatalf("round %d mark done: %v", day, err)
		}
		// Back-date this completion's done_at to day N
		if _, err := pool.Exec(ctx, `
			UPDATE quest_completions
			SET done_at = $2
			WHERE id = (SELECT id FROM quest_completions WHERE quest_id = $1 AND status = 'pending' ORDER BY done_at DESC, id DESC LIMIT 1)
		`, quest.ID, doneAtTime); err != nil {
			t.Fatalf("back-date completion: %v", err)
		}
	}
	// Assert CLM-3: 4 pending completions, distinct done_at, no points moved (still 7 from earlier approval).
	rows4 := pending("")
	if len(rows4) != 4 {
		t.Fatalf("pending completions count = %d, want 4", len(rows4))
	}
	doneAtSeen := map[string]bool{}
	for i, r := range rows4 {
		if doneAtSeen[r.DoneAt.String()] {
			t.Errorf("duplicate done_at in pending: %v", r.DoneAt)
		}
		doneAtSeen[r.DoneAt.String()] = true
		if i > 0 && !r.DoneAt.After(rows4[i-1].DoneAt) && !r.DoneAt.Equal(rows4[i-1].DoneAt) {
			t.Errorf("pending not ordered by done_at ASC: %v before %v", rows4[i-1].DoneAt, r.DoneAt)
		}
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("points during 4 pending completions: %d, want 7", p)
	}

	// Assert CLM-4: Admin approves all four: member gains 4 * 7 = 28 points (total 35),
	// each completion's done_at equals marked time, quest available_at/completed_at unchanged.
	beforeQuest := get()
	for _, r := range rows4 {
		approvedComp, mem, _, _, err := quests.Approve(ctx, alice, h.ID, r.ID, "")
		if err != nil {
			t.Fatalf("approve completion %s: %v", r.ID, err)
		}
		if approvedComp.Status != "approved" || approvedComp.ApprovedAt == nil {
			t.Errorf("approved completion status: %+v", approvedComp)
		}
		if !approvedComp.DoneAt.Equal(r.DoneAt) {
			t.Errorf("completion done_at changed on approval: got %v, want %v", approvedComp.DoneAt, r.DoneAt)
		}
		var storedDoneAt time.Time
		if err := pool.QueryRow(ctx, `SELECT done_at FROM quest_completions WHERE id = $1`, r.ID).Scan(&storedDoneAt); err != nil || !storedDoneAt.Equal(r.DoneAt) {
			t.Errorf("stored done_at: %v %v, want %v", storedDoneAt, err, r.DoneAt)
		}
		_ = mem
	}
	if p := points(ben.ID); p != 35 {
		t.Errorf("points after approving all 4: %d, want 35", p)
	}
	afterQuest := get()
	if !afterQuest.AvailableAt.Time.Equal(beforeQuest.AvailableAt.Time) ||
		!afterQuest.CompletedAt.Time.Equal(beforeQuest.CompletedAt.Time) {
		t.Errorf("quest times changed by approval: before %v %v, after %v %v",
			beforeQuest.AvailableAt, beforeQuest.CompletedAt, afterQuest.AvailableAt, afterQuest.CompletedAt)
	}
	if len(pending("")) != 0 {
		t.Error("pending list not empty after all approved")
	}

	// CLM-16: a shareable quest with a teamwork bonus that was never shared
	// has one recorded member, who gets the full points and no teamwork bonus,
	// exactly like a quest that is not shareable.
	soloShareable, err := quests.Create(ctx, alice, h.ID, app.QuestInput{
		Name: "Wash the car", Points: num(100), Shareable: true, TeamworkBonus: num(20),
	}, "")
	if err != nil {
		t.Fatalf("create shareable quest: %v", err)
	}
	if !soloShareable.Shareable || soloShareable.TeamworkBonus != 20 {
		t.Fatalf("shareable quest stored as %+v", soloShareable.HouseholdQuest)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, soloShareable.ID, asBen); err != nil {
		t.Fatalf("accept shareable quest: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, soloShareable.ID, asBen); err != nil {
		t.Fatalf("mark shareable quest done: %v", err)
	}
	soloRows := pending("")
	if len(soloRows) != 1 || soloRows[0].QuestID != soloShareable.ID || soloRows[0].MemberID != ben.ID ||
		soloRows[0].MemberName != "Ben" || soloRows[0].Points != 100 {
		t.Fatalf("pending unshared shareable completion: %+v", soloRows)
	}
	if m := soloRows[0].Members; len(m) != 1 || m[0].MemberID != ben.ID || m[0].Name != "Ben" || m[0].Award != 100 ||
		soloRows[0].TeamworkBonus != 0 {
		t.Errorf("pending unshared shareable members: %+v bonus %d, want Ben alone with 100 and no bonus", m, soloRows[0].TeamworkBonus)
	}
	benBeforeSolo := points(ben.ID)
	annBeforeSolo := points(ann.ID)
	soloComp, soloMember, soloBonuses, soloApproved, err := quests.Approve(ctx, alice, h.ID, soloRows[0].ID, "")
	if err != nil {
		t.Fatalf("approve unshared shareable completion: %v", err)
	}
	if len(soloApproved) != 1 || soloApproved[0].Member.ID != ben.ID || soloApproved[0].PointsAwarded != 100 ||
		soloApproved[0].TeamworkBonus != 0 {
		t.Errorf("approved members of unshared shareable completion: %+v, want Ben alone with 100 and no bonus", soloApproved)
	}
	if soloComp.Status != "approved" || soloComp.MemberID == nil || *soloComp.MemberID != ben.ID {
		t.Errorf("approved unshared shareable completion: %+v", soloComp)
	}
	if len(soloBonuses) != 0 {
		t.Errorf("combo bonuses for unshared shareable completion: %+v, want none", soloBonuses)
	}
	// Full 100 points, not 100 + 20 and not a split.
	if soloMember.ID != ben.ID || soloMember.Points != benBeforeSolo+100 {
		t.Errorf("credited member: %+v, want Ben with %d points", soloMember, benBeforeSolo+100)
	}
	if p := points(ben.ID); p != benBeforeSolo+100 {
		t.Errorf("Ben's points after unshared shareable approval: %d, want %d", p, benBeforeSolo+100)
	}
	if p := points(ann.ID); p != annBeforeSolo {
		t.Errorf("Ann's points moved on Ben's approval: %d, want %d", p, annBeforeSolo)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, soloRows[0].ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("repeat approve unshared shareable: %v, want ErrNotFound", err)
	}
	if p := points(ben.ID); p != benBeforeSolo+100 {
		t.Errorf("Ben's points after repeat approve: %d, want %d", p, benBeforeSolo+100)
	}

	// The CLM-16 case above credited Ben 100 points; the reject checks below
	// compare against the balance the earlier approvals left (35).
	if _, err := pool.Exec(ctx, `UPDATE family_members SET points = 35 WHERE id = $1`, ben.ID); err != nil {
		t.Fatalf("reset Ben's balance after the unshared shareable case: %v", err)
	}

	// CLM-5: Reject latest vs earlier:
	// Two completions: reject earlier -> available_at unchanged.
	// Reject latest (quest.completed_at = done_at) -> available_at <= now, QuestAvailable true.
	// Make quest available first
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, quest.ID); err != nil {
		t.Fatalf("make quest available: %v", err)
	}
	// Completion 1 (earlier)
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("accept c1: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("mark done c1: %v", err)
	}
	c1Rows := pending("")
	c1ID := c1Rows[0].ID
	c1Time := time.Now().Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE quest_completions SET done_at = $2 WHERE id = $1`, c1ID, c1Time); err != nil {
		t.Fatalf("backdate c1: %v", err)
	}

	// Completion 2 (latest)
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, quest.ID); err != nil {
		t.Fatalf("make quest available for c2: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("accept c2: %v", err)
	}
	c2DoneView, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen)
	if err != nil {
		t.Fatalf("mark done c2: %v", err)
	}
	c2Rows := pending("")
	if len(c2Rows) != 2 {
		t.Fatalf("pending completions count = %d, want 2", len(c2Rows))
	}
	c2ID := c2Rows[1].ID // newer one
	futureAvail := c2DoneView.AvailableAt.Time
	if !futureAvail.After(time.Now()) {
		t.Fatalf("c2 available_at %v not in future", futureAvail)
	}

	// Reject earlier (c1): available_at unchanged!
	if _, err := quests.Reject(ctx, alice, h.ID, c1ID, ""); err != nil {
		t.Fatalf("reject c1: %v", err)
	}
	afterC1Reject := get()
	if !afterC1Reject.AvailableAt.Time.Equal(futureAvail) {
		t.Errorf("rejecting earlier completion changed available_at: got %v, want %v", afterC1Reject.AvailableAt.Time, futureAvail)
	}
	if app.QuestAvailable(afterC1Reject.HouseholdQuest, time.Now()) {
		t.Error("quest became available when earlier completion was rejected")
	}
	remPending := pending("")
	if len(remPending) != 1 || remPending[0].ID != c2ID {
		t.Errorf("remaining pending after c1 reject: %+v", remPending)
	}

	// Reject latest (c2): cancels running cooldown, available_at <= now, QuestAvailable true!
	if _, err := quests.Reject(ctx, alice, h.ID, c2ID, ""); err != nil {
		t.Fatalf("reject c2: %v", err)
	}
	afterC2Reject := get()
	if !app.QuestAvailable(afterC2Reject.HouseholdQuest, time.Now()) {
		t.Errorf("quest not available after latest completion rejected: available_at %v", afterC2Reject.AvailableAt)
	}
	if p := points(ben.ID); p != 35 {
		t.Errorf("points changed on reject: %d, want 35", p)
	}
	if len(pending("")) != 0 {
		t.Error("pending list not empty after c2 rejected")
	}
	if _, err := quests.Reject(ctx, alice, h.ID, c2ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("repeat reject c2: %v, want ErrNotFound", err)
	}

	// CLM-11: Combo bonus pays when chores completed on same household day are approved
	// (reading mark-done date from completion record).
	quest2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Dishes", Points: num(3), RepeatDays: num(1)}, "")
	if err != nil {
		t.Fatalf("create quest2: %v", err)
	}
	combo, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "Yard and Kitchen",
		BonusPoints: num(10),
		QuestIDs:    []uuid.UUID{quest.ID, quest2.ID},
	}, "")
	if err != nil {
		t.Fatalf("create combo: %v", err)
	}

	// Ann accepts and marks done both on same day
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("Ann accept quest: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("Ann mark done quest: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest2.ID, asAnn); err != nil {
		t.Fatalf("Ann accept quest2: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest2.ID, asAnn); err != nil {
		t.Fatalf("Ann mark done quest2: %v", err)
	}
	annPending := pending("")
	if len(annPending) != 2 {
		t.Fatalf("Ann pending count = %d, want 2", len(annPending))
	}
	// Approve first completion: no combo bonus yet
	_, mem1, bonuses1, _, err := quests.Approve(ctx, alice, h.ID, annPending[0].ID, "")
	if err != nil || len(bonuses1) != 0 || mem1.Points != 7 {
		t.Errorf("first combo approval: mem points %d, bonuses %+v, err %v", mem1.Points, bonuses1, err)
	}
	// Approve second completion: pays combo bonus (10 points)!
	_, mem2, bonuses2, _, err := quests.Approve(ctx, alice, h.ID, annPending[1].ID, "")
	if err != nil || len(bonuses2) != 1 || bonuses2[0].ComboID != combo.ID || bonuses2[0].BonusPoints != 10 {
		t.Errorf("second combo approval: bonuses %+v, err %v", bonuses2, err)
	}
	if mem2.Points != 7+3+10 {
		t.Errorf("Ann's points after combo: %d, want 20", mem2.Points)
	}

	// Helper to accept, mark done, and back-date a completion to an explicit doneAt instant
	markDoneAt := func(questID uuid.UUID, action app.MemberAction, doneAt time.Time) uuid.UUID {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, questID); err != nil {
			t.Fatalf("make quest available: %v", err)
		}
		if _, err := quests.Accept(ctx, alice, h.ID, questID, action); err != nil {
			t.Fatalf("accept quest %s: %v", questID, err)
		}
		if _, err := quests.MarkDone(ctx, alice, h.ID, questID, action); err != nil {
			t.Fatalf("mark done quest %s: %v", questID, err)
		}
		var compID uuid.UUID
		if err := pool.QueryRow(ctx, `
			UPDATE quest_completions
			SET done_at = $2
			WHERE id = (SELECT id FROM quest_completions WHERE quest_id = $1 AND status = 'pending' ORDER BY done_at DESC, id DESC LIMIT 1)
			RETURNING id
		`, questID, doneAt).Scan(&compID); err != nil {
			t.Fatalf("back-date completion %s: %v", compID, err)
		}
		return compID
	}

	// CLM-1: Two-chore combo, Ann marks both done, back-date both to same Chicago local day,
	// approve on different days: first approval pays chore points only, second pays points plus bonus once.
	t.Run("CLM-1: same local day approved on different days", func(t *testing.T) {
		annBase := points(ann.ID)
		q1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM1 Chores 1", Points: num(5), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q1: %v", err)
		}
		q2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM1 Chores 2", Points: num(8), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q2: %v", err)
		}
		c1, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
			Name:        "CLM1 Combo",
			BonusPoints: num(15),
			QuestIDs:    []uuid.UUID{q1.ID, q2.ID},
		}, "")
		if err != nil {
			t.Fatalf("create combo: %v", err)
		}

		jan15_10am := time.Date(2025, time.January, 15, 10, 0, 0, 0, chicagoTZ)
		jan15_2pm := time.Date(2025, time.January, 15, 14, 0, 0, 0, chicagoTZ)

		cID1 := markDoneAt(q1.ID, asAnn, jan15_10am)
		cID2 := markDoneAt(q2.ID, asAnn, jan15_2pm)

		// First approval (e.g. earlier): pays chore points only (5 pts), 0 bonus
		_, memFirst, bonusesFirst, _, err := quests.Approve(ctx, alice, h.ID, cID1, "")
		if err != nil {
			t.Fatalf("first approval: %v", err)
		}
		if len(bonusesFirst) != 0 {
			t.Errorf("first approval bonuses: got %+v, want none", bonusesFirst)
		}
		if memFirst.Points != annBase+5 || points(ann.ID) != annBase+5 {
			t.Errorf("ann points after first approval: got %d (stored %d), want %d", memFirst.Points, points(ann.ID), annBase+5)
		}

		// Second approval (e.g. later day): pays chore points (8 pts) + combo bonus (15 pts)
		_, memSecond, bonusesSecond, _, err := quests.Approve(ctx, alice, h.ID, cID2, "")
		if err != nil {
			t.Fatalf("second approval: %v", err)
		}
		if len(bonusesSecond) != 1 || bonusesSecond[0].ComboID != c1.ID || bonusesSecond[0].BonusPoints != 15 {
			t.Errorf("second approval bonuses: got %+v, want 1 bonus of 15", bonusesSecond)
		}
		wantPoints := annBase + 5 + 8 + 15
		if memSecond.Points != wantPoints || points(ann.ID) != wantPoints {
			t.Errorf("ann points after second approval: got %d (stored %d), want %d", memSecond.Points, points(ann.ID), wantPoints)
		}

		var awardCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, c1.ID).Scan(&awardCount); err != nil {
			t.Fatalf("query awards: %v", err)
		}
		if awardCount != 1 {
			t.Errorf("award count for c1 = %d, want 1", awardCount)
		}
	})

	// CLM-2: Chicago household timezone cases:
	// A) done on different local days, approved together -> no bonus
	// B) 1h apart across Chicago local midnight (same UTC date) -> no bonus
	// C) same Chicago local date but different UTC dates -> bonus
	t.Run("CLM-2: Chicago timezone boundaries", func(t *testing.T) {
		// Case A: done on different local days, approved together -> no bonus
		{
			annBase := points(ann.ID)
			qA1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2A Chores 1", Points: num(4), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qA1: %v", err)
			}
			qA2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2A Chores 2", Points: num(6), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qA2: %v", err)
			}
			cA, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
				Name:        "CLM2A Combo",
				BonusPoints: num(10),
				QuestIDs:    []uuid.UUID{qA1.ID, qA2.ID},
			}, "")
			if err != nil {
				t.Fatalf("create combo: %v", err)
			}

			day1 := time.Date(2025, time.January, 16, 12, 0, 0, 0, chicagoTZ)
			day2 := time.Date(2025, time.January, 17, 12, 0, 0, 0, chicagoTZ)
			cA_id1 := markDoneAt(qA1.ID, asAnn, day1)
			cA_id2 := markDoneAt(qA2.ID, asAnn, day2)

			_, _, b1, _, err := quests.Approve(ctx, alice, h.ID, cA_id1, "")
			if err != nil || len(b1) != 0 {
				t.Errorf("approve cA1: bonuses %+v, err %v", b1, err)
			}
			_, _, b2, _, err := quests.Approve(ctx, alice, h.ID, cA_id2, "")
			if err != nil || len(b2) != 0 {
				t.Errorf("approve cA2: bonuses %+v, err %v", b2, err)
			}
			if p := points(ann.ID); p != annBase+4+6 {
				t.Errorf("ann points after different local days: got %d, want %d", p, annBase+10)
			}
			var awardsA int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cA.ID).Scan(&awardsA); err != nil || awardsA != 0 {
				t.Errorf("awards for cA: got %d %v, want 0", awardsA, err)
			}
		}

		// Case B: 1h apart across Chicago local midnight (same UTC date) -> no bonus
		// 2025-01-11 05:30:00 UTC = 2025-01-10 23:30:00 Chicago (local Jan 10)
		// 2025-01-11 06:30:00 UTC = 2025-01-11 00:30:00 Chicago (local Jan 11)
		{
			annBase := points(ann.ID)
			qB1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2B Chores 1", Points: num(4), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qB1: %v", err)
			}
			qB2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2B Chores 2", Points: num(6), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qB2: %v", err)
			}
			cB, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
				Name:        "CLM2B Combo",
				BonusPoints: num(12),
				QuestIDs:    []uuid.UUID{qB1.ID, qB2.ID},
			}, "")
			if err != nil {
				t.Fatalf("create combo: %v", err)
			}

			t1 := time.Date(2025, time.January, 11, 5, 30, 0, 0, time.UTC)
			t2 := time.Date(2025, time.January, 11, 6, 30, 0, 0, time.UTC)
			if t1.UTC().Day() != t2.UTC().Day() {
				t.Fatalf("UTC days should match: %v vs %v", t1.UTC(), t2.UTC())
			}
			if t1.In(chicagoTZ).Day() == t2.In(chicagoTZ).Day() {
				t.Fatalf("Chicago local days should differ: %v vs %v", t1.In(chicagoTZ), t2.In(chicagoTZ))
			}

			cB_id1 := markDoneAt(qB1.ID, asAnn, t1)
			cB_id2 := markDoneAt(qB2.ID, asAnn, t2)

			_, _, b1, _, err := quests.Approve(ctx, alice, h.ID, cB_id1, "")
			if err != nil || len(b1) != 0 {
				t.Errorf("approve cB1: bonuses %+v, err %v", b1, err)
			}
			_, _, b2, _, err := quests.Approve(ctx, alice, h.ID, cB_id2, "")
			if err != nil || len(b2) != 0 {
				t.Errorf("approve cB2: bonuses %+v, err %v", b2, err)
			}
			if p := points(ann.ID); p != annBase+4+6 {
				t.Errorf("ann points after midnight straddle: got %d, want %d", p, annBase+10)
			}
			var awardsB int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cB.ID).Scan(&awardsB); err != nil || awardsB != 0 {
				t.Errorf("awards for cB: got %d %v, want 0", awardsB, err)
			}
		}

		// Case C: same Chicago local date but different UTC dates -> bonus
		// 2025-01-10 10:00:00 Chicago = 2025-01-10 16:00:00 UTC (Jan 10 UTC)
		// 2025-01-10 23:00:00 Chicago = 2025-01-11 05:00:00 UTC (Jan 11 UTC)
		{
			annBase := points(ann.ID)
			qC1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2C Chores 1", Points: num(4), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qC1: %v", err)
			}
			qC2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2C Chores 2", Points: num(6), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qC2: %v", err)
			}
			cC, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
				Name:        "CLM2C Combo",
				BonusPoints: num(14),
				QuestIDs:    []uuid.UUID{qC1.ID, qC2.ID},
			}, "")
			if err != nil {
				t.Fatalf("create combo: %v", err)
			}

			tC1 := time.Date(2025, time.January, 10, 10, 0, 0, 0, chicagoTZ)
			tC2 := time.Date(2025, time.January, 10, 23, 0, 0, 0, chicagoTZ)
			if tC1.UTC().Day() == tC2.UTC().Day() {
				t.Fatalf("UTC days should differ: %v vs %v", tC1.UTC(), tC2.UTC())
			}
			if tC1.In(chicagoTZ).Day() != tC2.In(chicagoTZ).Day() {
				t.Fatalf("Chicago local days should match: %v vs %v", tC1.In(chicagoTZ), tC2.In(chicagoTZ))
			}

			cC_id1 := markDoneAt(qC1.ID, asAnn, tC1)
			cC_id2 := markDoneAt(qC2.ID, asAnn, tC2)

			_, _, b1, _, err := quests.Approve(ctx, alice, h.ID, cC_id1, "")
			if err != nil || len(b1) != 0 {
				t.Errorf("approve cC1: bonuses %+v, err %v", b1, err)
			}
			_, _, b2, _, err := quests.Approve(ctx, alice, h.ID, cC_id2, "")
			if err != nil || len(b2) != 1 || b2[0].ComboID != cC.ID || b2[0].BonusPoints != 14 {
				t.Errorf("approve cC2: bonuses %+v, err %v", b2, err)
			}
			wantPoints := annBase + 4 + 6 + 14
			if p := points(ann.ID); p != wantPoints {
				t.Errorf("ann points after same Chicago date / different UTC: got %d, want %d", p, wantPoints)
			}
			var awardsC int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cC.ID).Scan(&awardsC); err != nil || awardsC != 1 {
				t.Errorf("awards for cC: got %d %v, want 1", awardsC, err)
			}
		}
	})

	// CLM-3: Once-per-combo:
	// - After a combo pays, approving a later unrelated completion pays no second bonus
	// - Re-approving the paid completion returns app.ErrNotFound
	// - Two concurrent approvals of a full pending set pay exactly one bonus (sync.WaitGroup)
	// - A later new set completed later pays again
	t.Run("CLM-3: once per combo and concurrency", func(t *testing.T) {
		annBase := points(ann.ID)
		q3A, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Chores A", Points: num(10), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q3A: %v", err)
		}
		q3B, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Chores B", Points: num(15), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q3B: %v", err)
		}
		q3Solo, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Solo", Points: num(5), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q3Solo: %v", err)
		}
		c3, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
			Name:        "CLM3 Combo",
			BonusPoints: num(20),
			QuestIDs:    []uuid.UUID{q3A.ID, q3B.ID},
		}, "")
		if err != nil {
			t.Fatalf("create combo: %v", err)
		}

		jan18 := time.Date(2025, time.January, 18, 10, 0, 0, 0, chicagoTZ)
		c3_idA := markDoneAt(q3A.ID, asAnn, jan18)
		c3_idB := markDoneAt(q3B.ID, asAnn, jan18.Add(time.Hour))

		// Approve A (no bonus), then B (pays combo bonus)
		_, _, bA, _, err := quests.Approve(ctx, alice, h.ID, c3_idA, "")
		if err != nil || len(bA) != 0 {
			t.Fatalf("approve c3A: bonuses %+v, err %v", bA, err)
		}
		_, _, bB, _, err := quests.Approve(ctx, alice, h.ID, c3_idB, "")
		if err != nil || len(bB) != 1 || bB[0].BonusPoints != 20 || bB[0].ComboID != c3.ID {
			t.Fatalf("approve c3B: bonuses %+v, err %v", bB, err)
		}
		if p := points(ann.ID); p != annBase+10+15+20 {
			t.Errorf("ann points after initial c3: got %d, want %d", p, annBase+45)
		}

		// 1. Approve a later unrelated completion: pays only its chore points, no bonus
		c3_idSolo := markDoneAt(q3Solo.ID, asAnn, jan18.Add(2*time.Hour))
		_, memSolo, bSolo, _, err := quests.Approve(ctx, alice, h.ID, c3_idSolo, "")
		if err != nil || len(bSolo) != 0 {
			t.Fatalf("approve solo: bonuses %+v, err %v", bSolo, err)
		}
		if memSolo.Points != annBase+45+5 || points(ann.ID) != annBase+50 {
			t.Errorf("ann points after solo: got %d (stored %d), want %d", memSolo.Points, points(ann.ID), annBase+50)
		}

		// 2. Re-approving the paid completion returns app.ErrNotFound
		if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, c3_idB, ""); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("re-approve paid completion: got err %v, want app.ErrNotFound", err)
		}
		if p := points(ann.ID); p != annBase+50 {
			t.Errorf("points changed after re-approve attempt: got %d, want %d", p, annBase+50)
		}
		var awardsCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, c3.ID).Scan(&awardsCount); err != nil || awardsCount != 1 {
			t.Errorf("awards for c3: got %d %v, want 1", awardsCount, err)
		}

		// 3. Concurrent approvals of a full pending set pay exactly one bonus
		qConcA, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Conc A", Points: num(10), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create qConcA: %v", err)
		}
		qConcB, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Conc B", Points: num(12), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create qConcB: %v", err)
		}
		cConc, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
			Name:        "CLM3 Conc Combo",
			BonusPoints: num(25),
			QuestIDs:    []uuid.UUID{qConcA.ID, qConcB.ID},
		}, "")
		if err != nil {
			t.Fatalf("create cConc: %v", err)
		}

		jan19 := time.Date(2025, time.January, 19, 11, 0, 0, 0, chicagoTZ)
		cConcIDA := markDoneAt(qConcA.ID, asAnn, jan19)
		cConcIDB := markDoneAt(qConcB.ID, asAnn, jan19.Add(time.Minute))

		annBeforeConc := points(ann.ID)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		paidBonuses := make([][]app.ComboBonus, 2)
		start := make(chan struct{})
		for i, compID := range []uuid.UUID{cConcIDA, cConcIDB} {
			wg.Add(1)
			go func(idx int, cID uuid.UUID) {
				defer wg.Done()
				<-start
				_, _, b, _, err := quests.Approve(ctx, alice, h.ID, cID, "")
				errs[idx] = err
				paidBonuses[idx] = b
			}(i, compID)
		}
		close(start)
		wg.Wait()

		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("concurrent approvals failed: %v, %v", errs[0], errs[1])
		}
		totalBonuses := len(paidBonuses[0]) + len(paidBonuses[1])
		if totalBonuses != 1 {
			t.Fatalf("concurrent approvals total bonuses = %d, want 1", totalBonuses)
		}
		wantPointsConc := annBeforeConc + 10 + 12 + 25
		if p := points(ann.ID); p != wantPointsConc {
			t.Errorf("ann points after concurrent approvals: got %d, want %d", p, wantPointsConc)
		}
		var awardConcCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cConc.ID).Scan(&awardConcCount); err != nil {
			t.Fatalf("query cConc awards: %v", err)
		}
		if awardConcCount != 1 {
			t.Errorf("cConc award count = %d, want 1", awardConcCount)
		}

		// 4. A later new set pays again
		jan20 := time.Date(2025, time.January, 20, 10, 0, 0, 0, chicagoTZ)
		cNewIDA := markDoneAt(qConcA.ID, asAnn, jan20)
		cNewIDB := markDoneAt(qConcB.ID, asAnn, jan20.Add(time.Hour))

		annBeforeNew := points(ann.ID)
		_, _, bNewA, _, err := quests.Approve(ctx, alice, h.ID, cNewIDA, "")
		if err != nil || len(bNewA) != 0 {
			t.Fatalf("approve new set A: bonuses %+v, err %v", bNewA, err)
		}
		_, _, bNewB, _, err := quests.Approve(ctx, alice, h.ID, cNewIDB, "")
		if err != nil || len(bNewB) != 1 || bNewB[0].BonusPoints != 25 {
			t.Fatalf("approve new set B: bonuses %+v, err %v", bNewB, err)
		}
		wantPointsNew := annBeforeNew + 10 + 12 + 25
		if p := points(ann.ID); p != wantPointsNew {
			t.Errorf("ann points after later new set: got %d, want %d", p, wantPointsNew)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cConc.ID).Scan(&awardConcCount); err != nil {
			t.Fatalf("query cConc awards round 2: %v", err)
		}
		if awardConcCount != 2 {
			t.Errorf("cConc award count after second set = %d, want 2", awardConcCount)
		}
	})
}

// TestIntegration_QuestMarkDoneCooldown_CLM2 directly proves CLM-2:
// MarkDone, in one transaction, inserts a pending completion (done_at=NOW()),
// sets completed_at=NOW() and available_at to local midnight in households.timezone
// starting the local date of NOW() plus repeat_days days, computed by quest_next_available_at.
// It releases the holder (claimed_by/claimed_at NULL), requires claimed_by = acting member
// and household_id, and moves no points. A never-completed quest has available_at NULL and is available.
func TestIntegration_QuestMarkDoneCooldown_CLM2(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questclm2").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "CLM2 Family", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	member, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Sam", PIN: "1234"}, "")
	if err != nil {
		t.Fatalf("create Sam: %v", err)
	}
	asSam := app.MemberAction{MemberID: member.ID, PIN: "1234"}

	// 1. A never-completed quest has available_at NULL and is available.
	neverDone, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Rake leaves", Points: num(5), RepeatDays: num(2)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	gotNever, err := quests.Get(ctx, alice, h.ID, neverDone.ID)
	if err != nil {
		t.Fatalf("get never-done: %v", err)
	}
	if gotNever.AvailableAt.Valid || gotNever.CompletedAt.Valid || !app.QuestAvailable(gotNever.HouseholdQuest, time.Now()) {
		t.Errorf("never-done quest should have NULL available_at and be available: %+v", gotNever)
	}

	// 2. Accept, then mark done.
	if _, err := quests.Accept(ctx, alice, h.ID, neverDone.ID, asSam); err != nil {
		t.Fatalf("Sam accept: %v", err)
	}
	markTime := time.Now()
	doneView, err := quests.MarkDone(ctx, alice, h.ID, neverDone.ID, asSam)
	if err != nil {
		t.Fatalf("Sam mark done: %v", err)
	}

	// Assert: claimed_by is NULL, claimed_at is NULL
	if doneView.ClaimedBy != nil || doneView.ClaimedAt.Valid {
		t.Errorf("mark done did not release holder: claimed_by=%v", doneView.ClaimedBy)
	}

	// Assert: points unchanged
	samMember, err := familySvc.Get(ctx, alice, h.ID, member.ID)
	if err != nil || samMember.Points != 0 {
		t.Errorf("Sam's points changed on MarkDone: %d %v, want 0", samMember.Points, err)
	}

	// Assert: exactly one pending completion
	pending, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil || len(pending) != 1 || pending[0].QuestID != neverDone.ID || pending[0].MemberID != member.ID {
		t.Fatalf("pending completions: %+v, %v", pending, err)
	}

	// Assert: available_at equals local midnight of (local date + repeat_days) in America/Chicago
	chicagoLoc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	localMark := markTime.In(chicagoLoc)
	wantMidnight := time.Date(localMark.Year(), localMark.Month(), localMark.Day()+2, 0, 0, 0, 0, chicagoLoc)
	if !doneView.AvailableAt.Valid || !doneView.AvailableAt.Time.Equal(wantMidnight) {
		t.Errorf("available_at %v, want local midnight %v", doneView.AvailableAt.Time, wantMidnight)
	}
	if app.QuestAvailable(doneView.HouseholdQuest, time.Now()) {
		t.Error("quest with future available_at should not be available")
	}

	// 3. Back-date available_at by SQL and assert QuestAvailable
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 second' WHERE id = $1`, neverDone.ID); err != nil {
		t.Fatalf("back-date available_at: %v", err)
	}
	gotAged, err := quests.Get(ctx, alice, h.ID, neverDone.ID)
	if err != nil {
		t.Fatalf("get aged: %v", err)
	}
	if !app.QuestAvailable(gotAged.HouseholdQuest, time.Now()) {
		t.Errorf("aged quest not available: %+v", gotAged)
	}
}

// TestIntegration_QuestApproval_RollbackOnPointsAwardFailure proves CLM-2:
// when the points_awards insert fails during Approve, the whole transaction rolls back:
// the completion is still pending, the member's points
// are unchanged, zero history rows exist, and Approve returns an error.
// Dropping the fault trigger allows a retry to succeed.
func TestIntegration_QuestApproval_RollbackOnPointsAwardFailure(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "approvefault").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Fault Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	ann, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ann"}, "")
	if err != nil {
		t.Fatalf("create Ann: %v", err)
	}
	asAnn := app.MemberAction{MemberID: ann.ID}
	quest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Mow lawn", Points: num(10), RepeatDays: num(2)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}

	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	pendingRows, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil || len(pendingRows) != 1 || pendingRows[0].MemberID != ann.ID {
		t.Fatalf("pending after mark done: %+v %v", pendingRows, err)
	}
	compID := pendingRows[0].ID

	// Fault injection: trigger on points_awards fails the insert.
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION test_fail_points_award() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'points award insert failed'; END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fail_points_award() CASCADE`)
	})
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_fail_points_award BEFORE INSERT ON points_awards
		FOR EACH ROW EXECUTE FUNCTION test_fail_points_award()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Attempt approve: must fail because points_awards insert fails.
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, compID, ""); err == nil {
		t.Fatal("approve succeeded despite points_awards trigger failure")
	}

	// Assert whole transaction rolled back:
	// 1. The completion is still pending.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM quest_completions WHERE household_id = $1 AND id = $2`, h.ID, compID).Scan(&status); err != nil || status != "pending" {
		t.Errorf("completion after failed approve: status %q %v, want pending", status, err)
	}

	// 2. Member points unchanged (0).
	mAfter, err := q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: h.ID, ID: ann.ID})
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if mAfter.Points != 0 {
		t.Errorf("member points after failed approve: got %d, want 0", mAfter.Points)
	}

	// 3. Zero points_awards rows.
	awards, err := q.ListMemberPointsAwards(ctx, db.ListMemberPointsAwardsParams{
		HouseholdID: h.ID,
		MemberID:    ann.ID,
		Limit:       100,
		Offset:      0,
	})
	if err != nil {
		t.Fatalf("list points awards: %v", err)
	}
	if len(awards) != 0 {
		t.Errorf("points awards count after failed approve: got %d, want 0", len(awards))
	}

	// 4. Drop trigger and assert retry succeeds.
	if _, err := pool.Exec(ctx, `DROP TRIGGER test_fail_points_award ON points_awards`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	approved, creditedMember, _, _, err := quests.Approve(ctx, alice, h.ID, compID, "")
	if err != nil {
		t.Fatalf("retry approve after dropping trigger: %v", err)
	}
	if creditedMember.ID != ann.ID || creditedMember.Points != 10 {
		t.Errorf("credited member after retry: %+v", creditedMember)
	}
	if approved.ID != compID || approved.Status != "approved" || approved.ApprovedAt == nil {
		t.Errorf("approved completion: %+v", approved)
	}

	// Member points credited
	mFinal, err := q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: h.ID, ID: ann.ID})
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if mFinal.Points != 10 {
		t.Errorf("member points after successful retry: got %d, want 10", mFinal.Points)
	}

	// Exactly 1 points award row now exists
	awardsFinal, err := q.ListMemberPointsAwards(ctx, db.ListMemberPointsAwardsParams{
		HouseholdID: h.ID,
		MemberID:    ann.ID,
		Limit:       100,
		Offset:      0,
	})
	if err != nil {
		t.Fatalf("list points awards final: %v", err)
	}
	if len(awardsFinal) != 1 || awardsFinal[0].Points != 10 || awardsFinal[0].Reason != "Mow lawn" {
		t.Errorf("points awards final: %+v, want 1 award with 10 pts", awardsFinal)
	}
}

// TestIntegration_QuestResetsAtLocalMidnight pins the return rule:
// MarkDone sets available_at to local midnight in the household's timezone,
// repeat_days days after the local day it was marked done, computed by quest_next_available_at.
// Approvals do not alter available_at.
func TestIntegration_QuestResetsAtLocalMidnight(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()
	alice := registerVerified(t, pool, authSvc, mailer, "questmidnight").User.ID

	type family struct {
		id       uuid.UUID
		ann, ben app.MemberAction
	}
	newFamily := func(name, tz string) family {
		t.Helper()
		h, err := households.Create(ctx, alice, app.HouseholdInput{Name: name, Timezone: tz})
		if err != nil {
			t.Fatalf("create household %s: %v", name, err)
		}
		ann, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ann"}, "")
		if err != nil {
			t.Fatalf("create Ann: %v", err)
		}
		ben, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ben"}, "")
		if err != nil {
			t.Fatalf("create Ben: %v", err)
		}
		return family{h.ID, app.MemberAction{MemberID: ann.ID}, app.MemberAction{MemberID: ben.ID}}
	}
	newQuest := func(f family, days int64) uuid.UUID {
		t.Helper()
		quest, err := quests.Create(ctx, alice, f.id, app.QuestInput{Name: "Feed the cat", Points: num(1), RepeatDays: num(days)}, "")
		if err != nil {
			t.Fatalf("create quest: %v", err)
		}
		return quest.ID
	}
	get := func(f family, id uuid.UUID) app.QuestView {
		t.Helper()
		got, err := quests.Get(ctx, alice, f.id, id)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return got
	}
	doneAt := func(f family, id uuid.UUID, at time.Time) uuid.UUID {
		t.Helper()
		if _, err := quests.Accept(ctx, alice, f.id, id, f.ann); err != nil {
			t.Fatalf("accept: %v", err)
		}
		if _, err := quests.MarkDone(ctx, alice, f.id, id, f.ann); err != nil {
			t.Fatalf("mark done: %v", err)
		}
		var compID uuid.UUID
		if err := pool.QueryRow(ctx, `
			UPDATE quest_completions
			SET done_at = $2
			WHERE id = (SELECT id FROM quest_completions WHERE quest_id = $1 AND status = 'pending' ORDER BY done_at DESC, id DESC LIMIT 1)
			RETURNING id
		`, id, at).Scan(&compID); err != nil {
			t.Fatalf("update completion done_at: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE household_quests
			SET completed_at = $2,
			    available_at = quest_next_available_at($2, (SELECT timezone FROM households WHERE id = $3), repeat_days)
			WHERE id = $1
		`, id, at, f.id); err != nil {
			t.Fatalf("age available_at: %v", err)
		}
		return compID
	}
	approve := func(f family, compID uuid.UUID) app.Completion {
		t.Helper()
		comp, _, _, _, err := quests.Approve(ctx, alice, f.id, compID, "")
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		return comp
	}
	back := func(f family, days int64, at time.Time) time.Time {
		t.Helper()
		id := newQuest(f, days)
		compID := doneAt(f, id, at)
		approve(f, compID)
		got := get(f, id)
		if !got.AvailableAt.Valid || !got.CompletedAt.Valid {
			t.Fatalf("approved quest: %+v", got)
		}
		return got.AvailableAt.Time
	}
	chicagoTZ, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	chicago := newFamily("Chicago Family", "America/Chicago")

	t.Run("late approval keeps the done-day midnight", func(t *testing.T) {
		now := time.Now().In(chicagoTZ)
		yesterday20 := time.Date(now.Year(), now.Month(), now.Day()-1, 20, 0, 0, 0, chicagoTZ)
		todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, chicagoTZ)
		id := newQuest(chicago, 1)
		compID := doneAt(chicago, id, yesterday20)
		approve(chicago, compID)
		got := get(chicago, id)
		if !got.AvailableAt.Time.Equal(todayMidnight) || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("available_at %v, want %v and available", got.AvailableAt.Time, todayMidnight)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after the midnight: %v", err)
		}
		// Approved months after it was done: the done day still decides.
		at := time.Date(2025, 6, 2, 20, 0, 0, 0, chicagoTZ)
		want := time.Date(2025, 6, 3, 0, 0, 0, 0, chicagoTZ)
		if got := back(chicago, 1, at); !got.Equal(want) {
			t.Errorf("late approval: available_at %v, want %v", got, want)
		}
	})

	t.Run("N days from any time on the done day", func(t *testing.T) {
		// 2025-06-02 is a Monday.
		for _, days := range []int64{1, 3, 7} {
			want := time.Date(2025, 6, 2+int(days), 0, 0, 0, 0, chicagoTZ)
			for _, at := range []time.Time{
				time.Date(2025, 6, 2, 0, 0, 30, 0, chicagoTZ),
				time.Date(2025, 6, 2, 23, 0, 0, 0, chicagoTZ),
				time.Date(2025, 6, 2, 23, 59, 59, 0, chicagoTZ),
			} {
				got := back(chicago, days, at)
				if !got.Equal(want) {
					t.Errorf("repeat %d, done %v: available_at %v, want %v", days, at, got.In(chicagoTZ), want)
				}
				if wait := got.Sub(at); wait <= time.Duration(days-1)*24*time.Hour || wait > time.Duration(days)*24*time.Hour {
					t.Errorf("repeat %d, done %v: wait %v", days, at, wait)
				}
			}
		}
	})

	t.Run("each household uses its own midnight", func(t *testing.T) {
		utc := newFamily("UTC Family", "UTC")
		plain := newFamily("Default Family", "")
		if h, err := households.Get(ctx, alice, plain.id); err != nil || h.Timezone != "UTC" {
			t.Fatalf("default timezone: %q %v", h.Timezone, err)
		}
		// 03:00 UTC on June 3 is still 22:00 on June 2 in Chicago.
		at := time.Date(2025, 6, 3, 3, 0, 0, 0, time.UTC)
		cases := []struct {
			name string
			f    family
			want time.Time
		}{
			{"UTC", utc, time.Date(2025, 6, 4, 0, 0, 0, 0, time.UTC)},
			{"America/Chicago", chicago, time.Date(2025, 6, 3, 0, 0, 0, 0, chicagoTZ)},
			{"default", plain, time.Date(2025, 6, 4, 0, 0, 0, 0, time.UTC)},
		}
		for _, c := range cases {
			if got := back(c.f, 1, at); !got.Equal(c.want) {
				t.Errorf("%s: available_at %v, want %v", c.name, got, c.want)
			}
		}
	})

	t.Run("daylight-saving days", func(t *testing.T) {
		havana := newFamily("Havana Family", "America/Havana")
		cases := []struct {
			name string
			f    family
			at   time.Time
			want time.Time
		}{
			// Spring forward (02:00 is skipped): 00:00 CST.
			{"spring forward", chicago, time.Date(2025, 3, 8, 18, 0, 0, 0, time.UTC), time.Date(2025, 3, 9, 6, 0, 0, 0, time.UTC)},
			// Fall back (01:00 happens twice): the one 00:00, CDT.
			{"fall back", chicago, time.Date(2025, 11, 1, 17, 0, 0, 0, time.UTC), time.Date(2025, 11, 2, 5, 0, 0, 0, time.UTC)},
			// Havana skips 00:00 on 2025-03-09: the day starts at 01:00 CDT.
			{"skipped midnight", havana, time.Date(2025, 3, 8, 17, 0, 0, 0, time.UTC), time.Date(2025, 3, 9, 5, 0, 0, 0, time.UTC)},
		}
		for _, c := range cases {
			if got := back(c.f, 1, c.at); !got.Equal(c.want) {
				t.Errorf("%s: available_at %v, want %v", c.name, got.UTC(), c.want)
			}
		}
	})

	t.Run("accept is refused until the midnight", func(t *testing.T) {
		id := newQuest(chicago, 1)
		compID := doneAt(chicago, id, time.Now())
		approve(chicago, compID)
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() + interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatalf("set available_at: %v", err)
		}
		before := get(chicago, id)
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); !errors.Is(err, app.ErrQuestTaken) {
			t.Errorf("accept before the midnight: %v, want ErrQuestTaken", err)
		}
		if after := get(chicago, id); after.ClaimedBy != nil || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) ||
			!after.AvailableAt.Time.Equal(before.AvailableAt.Time) {
			t.Errorf("a refused accept changed the quest: %+v, was %+v", after, before)
		}
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatalf("set available_at: %v", err)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after the midnight: %v", err)
		}
	})

	t.Run("a quest never done is available", func(t *testing.T) {
		id := newQuest(chicago, 7)
		if got := get(chicago, id); got.AvailableAt.Valid || got.CompletedAt.Valid || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Fatalf("new quest: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Errorf("accept a new quest: %v", err)
		}
	})

	t.Run("cooldown starts at mark-done and cancels on rejection", func(t *testing.T) {
		// Done, sent back: cooldown cancelled, chore available immediately.
		id := newQuest(chicago, 3)
		compID := doneAt(chicago, id, time.Now())
		if _, err := quests.Reject(ctx, alice, chicago.id, compID, ""); err != nil {
			t.Fatalf("reject: %v", err)
		}
		got := get(chicago, id)
		if !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("after reject, quest should be available: %+v", got)
		}
		compID2 := doneAt(chicago, id, time.Now())
		if _, err := quests.Reject(ctx, alice, chicago.id, compID2, ""); err != nil {
			t.Fatalf("reject again: %v", err)
		}
		// Another member can accept right away
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after second reject: %v", err)
		}

		// Taken and put down (abandon before marking done).
		id = newQuest(chicago, 3)
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("accept: %v", err)
		}
		if _, err := quests.Abandon(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		got = get(chicago, id)
		if got.AvailableAt.Valid || got.CompletedAt.Valid || got.ClaimedBy != nil || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("after a plain abandon: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after abandon: %v", err)
		}

		// Marked done, then its holder is removed from the household.
		// A 1-day repeat marked done 48h ago has finished its cooldown, so the
		// released chore is back in the pool for the other member.
		f := newFamily("Release Family", "America/Chicago")
		id = newQuest(f, 1)
		_ = doneAt(f, id, time.Now().Add(-48*time.Hour))
		if err := familySvc.Remove(ctx, alice, f.id, f.ann.MemberID, ""); err != nil {
			t.Fatalf("remove Ann: %v", err)
		}
		got = get(f, id)
		if got.ClaimedBy != nil {
			t.Errorf("after release: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, f.id, id, f.ben); err != nil {
			t.Errorf("accept after release: %v", err)
		}

		// Approved once and back: a rejection of a second completion keeps the quest available or cancels its cooldown
		id = newQuest(chicago, 1)
		cEarliest := doneAt(chicago, id, time.Now().Add(-72*time.Hour))
		approve(chicago, cEarliest)
		earned := get(chicago, id)
		cLatest := doneAt(chicago, id, time.Now())
		if _, err := quests.Reject(ctx, alice, chicago.id, cLatest, ""); err != nil {
			t.Fatalf("reject: %v", err)
		}
		got = get(chicago, id)
		if !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("reject latest completion should make quest available: %+v, was %+v", got, earned)
		}
	})
}

// ageRangeFixture is a household in America/Chicago with members of known
// ages, set as birthdays a month past their last birthday so the age is fixed
// whatever today's date.
type ageRangeFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	q        *db.Queries
	quests   *app.QuestService
	family   *app.FamilyService
	admin    uuid.UUID
	h        uuid.UUID
	tz       string
	nameByID map[uuid.UUID]string
}

func newAgeRangeFixture(t *testing.T, prefix string) *ageRangeFixture {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	f := &ageRangeFixture{
		t: t, ctx: context.Background(), pool: pool, q: q,
		quests: app.NewQuestService(q, pool), family: app.NewFamilyService(q, pool, 10),
		tz: "America/Chicago", nameByID: map[uuid.UUID]string{},
	}
	f.admin = registerVerified(t, pool, authSvc, mailer, prefix).User.ID
	h, err := app.NewHouseholdService(q, pool).Create(f.ctx, f.admin, app.HouseholdInput{Name: "Ages", Timezone: f.tz})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	f.h = h.ID
	return f
}

// member adds a member to household hid; years < 0 leaves the birthday unset.
func (f *ageRangeFixture) member(hid uuid.UUID, name string, years int) uuid.UUID {
	f.t.Helper()
	m, err := f.family.Create(f.ctx, f.admin, hid, app.FamilyMemberInput{Name: name}, "")
	if err != nil {
		f.t.Fatalf("create %s: %v", name, err)
	}
	f.nameByID[m.ID] = name
	if years >= 0 {
		f.setAge(m.ID, years)
	}
	return m.ID
}

// setAge stores a birthday that makes the member years old today in the
// household's timezone (a month after their birthday).
func (f *ageRangeFixture) setAge(memberID uuid.UUID, years int) {
	f.t.Helper()
	d := localDate(f.t, f.tz).AddDate(-years, 0, -30)
	if _, err := f.pool.Exec(f.ctx, `UPDATE family_members SET birthday = $1 WHERE id = $2`, pgDate(d), memberID); err != nil {
		f.t.Fatalf("set birthday: %v", err)
	}
}

func ageField(n *int64) app.NumberField {
	if n == nil {
		return app.NumberField{}
	}
	return num(*n)
}

func limit(n int64) *int64 { return &n }

// quest creates a quest with the given limits (nil is no limit).
func (f *ageRangeFixture) quest(name string, minAge, maxAge *int64, shareable bool) app.QuestView {
	f.t.Helper()
	v, err := f.quests.Create(f.ctx, f.admin, f.h, app.QuestInput{
		Name: name, Points: num(5), Shareable: shareable, MinAge: ageField(minAge), MaxAge: ageField(maxAge),
	}, "")
	if err != nil {
		f.t.Fatalf("create quest %s: %v", name, err)
	}
	return v
}

func (f *ageRangeFixture) accept(questID, memberID uuid.UUID) error {
	f.t.Helper()
	_, err := f.quests.Accept(f.ctx, f.admin, f.h, questID, app.MemberAction{MemberID: memberID})
	return err
}

func (f *ageRangeFixture) stored(questID uuid.UUID) db.HouseholdQuest {
	f.t.Helper()
	row, err := f.q.GetHouseholdQuest(f.ctx, db.GetHouseholdQuestParams{HouseholdID: f.h, ID: questID})
	if err != nil {
		f.t.Fatalf("read quest: %v", err)
	}
	return row
}

func limitsOf(q db.HouseholdQuest) string {
	s := func(n pgtype.Int4) string {
		if !n.Valid {
			return "none"
		}
		return fmt.Sprint(n.Int32)
	}
	return s(q.MinAge) + "/" + s(q.MaxAge)
}

// TestIntegration_QuestAgeRangeStored proves CLM-2 and CLM-3 at the service:
// the range is stored and returned as sent, Update replaces it whole, and a
// bad limit is a per-field ValidationError that saves nothing.
func TestIntegration_QuestAgeRangeStored(t *testing.T) {
	f := newAgeRangeFixture(t, "agestore")

	cases := []struct {
		min, max *int64
		want     string
	}{
		{limit(10), limit(14), "10/14"},
		{limit(6), nil, "6/none"},
		{nil, limit(12), "none/12"},
		{nil, nil, "none/none"},
		{limit(0), limit(0), "0/0"},
	}
	var ids []uuid.UUID
	for _, c := range cases {
		v := f.quest("Ranged "+c.want, c.min, c.max, false)
		if got := limitsOf(v.HouseholdQuest); got != c.want {
			t.Errorf("created %s: view %s", c.want, got)
		}
		if got := limitsOf(f.stored(v.ID)); got != c.want {
			t.Errorf("created %s: stored %s", c.want, got)
		}
		got, err := f.quests.Get(f.ctx, f.admin, f.h, v.ID)
		if err != nil || limitsOf(got.HouseholdQuest) != c.want {
			t.Errorf("get %s: %s %v", c.want, limitsOf(got.HouseholdQuest), err)
		}
		ids = append(ids, v.ID)
	}

	// Update is a full replacement of the range: limits omitted are cleared,
	// and a new pair replaces the old.
	if v, err := f.quests.Update(f.ctx, f.admin, f.h, ids[0], app.QuestInput{Name: "Ranged cleared", Points: num(5)}, ""); err != nil || limitsOf(v.HouseholdQuest) != "none/none" {
		t.Errorf("update without limits: %s %v, want none/none", limitsOf(v.HouseholdQuest), err)
	}
	if got := limitsOf(f.stored(ids[0])); got != "none/none" {
		t.Errorf("stored after clearing update: %s", got)
	}
	if v, err := f.quests.Update(f.ctx, f.admin, f.h, ids[3], app.QuestInput{Name: "Now ranged", MinAge: num(8), MaxAge: num(12)}, ""); err != nil || limitsOf(v.HouseholdQuest) != "8/12" {
		t.Errorf("update to 8/12: %s %v", limitsOf(v.HouseholdQuest), err)
	}
	if got := limitsOf(f.stored(ids[3])); got != "8/12" {
		t.Errorf("stored after 8/12 update: %s", got)
	}

	// CLM-3: each bad limit is reported on its own field, with every other
	// bad field, and nothing is saved.
	invalid := app.NumberField{Set: true, Invalid: true}
	bad := []struct {
		name string
		in   app.QuestInput
		want map[string]string
	}{
		{"min above max", app.QuestInput{Name: "x", MinAge: num(14), MaxAge: num(10)},
			map[string]string{"max_age": "Minimum age can't be above the maximum age."}},
		{"negative min", app.QuestInput{Name: "x", MinAge: num(-1)}, map[string]string{"min_age": ""}},
		{"negative max", app.QuestInput{Name: "x", MaxAge: num(-1)}, map[string]string{"max_age": ""}},
		{"fractional or text min", app.QuestInput{Name: "x", MinAge: invalid}, map[string]string{"min_age": ""}},
		{"fractional or text max", app.QuestInput{Name: "x", MaxAge: invalid}, map[string]string{"max_age": ""}},
		{"with other fields", app.QuestInput{Name: "", Points: num(-1), MinAge: num(-2), MaxAge: invalid},
			map[string]string{"name": "", "points": "", "min_age": "", "max_age": ""}},
	}
	before, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range bad {
		for _, op := range []string{"create", "update"} {
			var err error
			if op == "create" {
				_, err = f.quests.Create(f.ctx, f.admin, f.h, c.in, "")
			} else {
				_, err = f.quests.Update(f.ctx, f.admin, f.h, ids[3], c.in, "")
			}
			var ve *app.ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("%s %s: %v, want ValidationError", op, c.name, err)
				continue
			}
			if len(ve.Fields) != len(c.want) {
				t.Errorf("%s %s: fields %v, want keys of %v", op, c.name, ve.Fields, c.want)
			}
			for field, msg := range c.want {
				if got := ve.Fields[field]; got == "" || (msg != "" && got != msg) {
					t.Errorf("%s %s: %s = %q, want %q", op, c.name, field, got, msg)
				}
			}
		}
	}
	after, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil || len(after) != len(before) {
		t.Errorf("refused creates saved quests: %d before, %d after (%v)", len(before), len(after), err)
	}
	if got := f.stored(ids[3]); limitsOf(got) != "8/12" || got.Name != "Now ranged" {
		t.Errorf("refused updates changed the quest: %s %q", limitsOf(got), got.Name)
	}
}

// TestIntegration_QuestAgeRangeAccept proves CLM-10, CLM-11, CLM-13 and
// CLM-15: ClaimQuest admits members inside the range (limits inclusive, one-
// sided ranges apply only their side), members with no birthday and anyone
// on a quest without limits; an available quest refuses an out-of-range
// member with ErrAgeRestricted and stays in the pool, while a held or resting
// quest is still ErrQuestTaken; ineligible_member_ids lists exactly the
// members Accept refuses; a library copy is all ages.
func TestIntegration_QuestAgeRangeAccept(t *testing.T) {
	f := newAgeRangeFixture(t, "ageaccept")
	kid5 := f.member(f.h, "Five", 5)
	kid6 := f.member(f.h, "Six", 6)
	kid8 := f.member(f.h, "Eight", 8)
	kid10 := f.member(f.h, "Ten", 10)
	kid12 := f.member(f.h, "Twelve", 12)
	kid13 := f.member(f.h, "Thirteen", 13)
	kid14 := f.member(f.h, "Fourteen", 14)
	kid15 := f.member(f.h, "Fifteen", 15)
	nobday := f.member(f.h, "NoBirthday", -1)
	everyone := []uuid.UUID{kid5, kid6, kid8, kid10, kid12, kid13, kid14, kid15, nobday}

	accepts := []struct {
		name     string
		min, max *int64
		member   uuid.UUID
		want     error
	}{
		{"10-14 at the min", limit(10), limit(14), kid10, nil},
		{"10-14 at the max", limit(10), limit(14), kid14, nil},
		{"10-14 inside", limit(10), limit(14), kid12, nil},
		{"10-14 no birthday", limit(10), limit(14), nobday, nil},
		{"10-14 below", limit(10), limit(14), kid8, app.ErrAgeRestricted},
		{"10-14 above", limit(10), limit(14), kid15, app.ErrAgeRestricted},
		{"6+ inside", limit(6), nil, kid15, nil},
		{"6+ at the min", limit(6), nil, kid6, nil},
		{"6+ below", limit(6), nil, kid5, app.ErrAgeRestricted},
		{"up to 12 inside", nil, limit(12), kid5, nil},
		{"up to 12 at the max", nil, limit(12), kid12, nil},
		{"up to 12 above", nil, limit(12), kid13, app.ErrAgeRestricted},
		{"no limits, youngest", nil, nil, kid5, nil},
		{"no limits, oldest", nil, nil, kid15, nil},
		{"no limits, no birthday", nil, nil, nobday, nil},
	}
	for _, c := range accepts {
		quest := f.quest(c.name, c.min, c.max, false)
		err := f.accept(quest.ID, c.member)
		if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: accept by %s = %v, want %v", c.name, f.nameByID[c.member], err, c.want)
			continue
		}
		held := f.stored(quest.ID).ClaimedBy
		if c.want == nil && (held == nil || *held != c.member) {
			t.Errorf("%s: claimed_by %v, want %s", c.name, held, f.nameByID[c.member])
		}
		if c.want != nil && held != nil {
			t.Errorf("%s: refused accept left claimed_by %v", c.name, *held)
		}
	}

	// CLM-11: the refused quest stays in the pool and an eligible member
	// takes it; a held quest is taken for everyone, in range or not.
	pool := f.quest("Mow the lawn", limit(10), limit(14), false)
	if err := f.accept(pool.ID, kid8); !errors.Is(err, app.ErrAgeRestricted) {
		t.Fatalf("out-of-range accept: %v, want ErrAgeRestricted", err)
	}
	if got, err := f.quests.Get(f.ctx, f.admin, f.h, pool.ID); err != nil || got.ClaimedBy != nil || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
		t.Fatalf("quest after refused accept: %+v %v, want available", got, err)
	}
	if err := f.accept(pool.ID, kid12); err != nil {
		t.Fatalf("eligible accept after refusal: %v", err)
	}
	if err := f.accept(pool.ID, kid8); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("out-of-range accept of a held quest: %v, want ErrQuestTaken", err)
	}
	if err := f.accept(pool.ID, kid10); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("in-range accept of a held quest: %v, want ErrQuestTaken", err)
	}
	// A resting quest (marked done, waiting for its midnight) is taken too.
	if _, err := f.quests.MarkDone(f.ctx, f.admin, f.h, pool.ID, app.MemberAction{MemberID: kid12}); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if err := f.accept(pool.ID, kid8); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("out-of-range accept of a resting quest: %v, want ErrQuestTaken", err)
	}
	if err := f.accept(uuid.New(), kid8); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("accept of an unknown quest: %v, want ErrNotFound", err)
	}

	// CLM-13: ineligible_member_ids is exactly the members Accept refuses,
	// in list and get, and [] on a quest without limits.
	ranged := f.quest("Ranged list", limit(10), limit(14), false)
	open := f.quest("Open list", nil, nil, false)
	want := map[uuid.UUID]bool{kid5: true, kid6: true, kid8: true, kid15: true}
	list, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed, listedOpen *app.QuestView
	for i := range list {
		switch list[i].ID {
		case ranged.ID:
			listed = &list[i]
		case open.ID:
			listedOpen = &list[i]
		}
	}
	if listed == nil || listedOpen == nil {
		t.Fatalf("list misses the new quests")
	}
	got, err := f.quests.Get(f.ctx, f.admin, f.h, ranged.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for label, ids := range map[string][]uuid.UUID{"list": listed.IneligibleMemberIDs, "get": got.IneligibleMemberIDs, "create": ranged.IneligibleMemberIDs} {
		if len(ids) != len(want) {
			t.Errorf("%s ineligible = %v, want %d members", label, ids, len(want))
		}
		for _, id := range ids {
			if !want[id] {
				t.Errorf("%s ineligible lists %s", label, f.nameByID[id])
			}
		}
	}
	if len(listedOpen.IneligibleMemberIDs) != 0 || len(open.IneligibleMemberIDs) != 0 {
		t.Errorf("all-ages quest ineligible = %v / %v, want none", listedOpen.IneligibleMemberIDs, open.IneligibleMemberIDs)
	}
	for _, m := range everyone {
		err := f.accept(ranged.ID, m)
		if want[m] != errors.Is(err, app.ErrAgeRestricted) {
			t.Errorf("accept by %s: %v; listed ineligible %v", f.nameByID[m], err, want[m])
		}
		if err == nil {
			// Put it back in the pool for the next member.
			if _, err := f.quests.Abandon(f.ctx, f.admin, f.h, ranged.ID, app.MemberAction{MemberID: m}); err != nil {
				t.Fatalf("abandon: %v", err)
			}
		}
	}

	// CLM-15: a library copy is all ages, as returned and as stored, and a
	// member with a birthday accepts it.
	library, err := f.quests.ListLibrary(f.ctx)
	if err != nil || len(library) == 0 {
		t.Fatalf("library: %v", err)
	}
	copied, err := f.quests.CopyFromLibrary(f.ctx, f.admin, f.h, library[0].ID, "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if limitsOf(copied.HouseholdQuest) != "none/none" || limitsOf(f.stored(copied.ID)) != "none/none" || len(copied.IneligibleMemberIDs) != 0 {
		t.Errorf("library copy limits %s, stored %s, ineligible %v; want all ages", limitsOf(copied.HouseholdQuest), limitsOf(f.stored(copied.ID)), copied.IneligibleMemberIDs)
	}
	if err := f.accept(copied.ID, kid15); err != nil {
		t.Errorf("accept library copy: %v", err)
	}
}

// TestIntegration_QuestAgeRangeKeepsHeld proves CLM-16: narrowing a quest's
// range and setting a holder's birthday out of it releases nothing; the
// holder and participants keep it and can mark it done, and the list returns
// every quest of the household whatever its range.
func TestIntegration_QuestAgeRangeKeepsHeld(t *testing.T) {
	f := newAgeRangeFixture(t, "ageheld")
	holder := f.member(f.h, "Holder", -1)
	partner := f.member(f.h, "Partner", 11)

	solo := f.quest("Solo", limit(10), limit(14), false)
	shared := f.quest("Shared", limit(10), limit(14), true)
	if err := f.accept(solo.ID, partner); err != nil {
		t.Fatalf("accept solo: %v", err)
	}
	if err := f.accept(shared.ID, holder); err != nil {
		t.Fatalf("accept shared: %v", err)
	}
	if _, err := f.quests.Share(f.ctx, f.admin, f.h, shared.ID, app.MemberAction{MemberID: holder}, app.MemberAction{MemberID: partner}); err != nil {
		t.Fatalf("share: %v", err)
	}

	// The admin narrows both ranges to exclude them and sets the holder's
	// birthday out of range too.
	for _, q := range []app.QuestView{solo, shared} {
		if _, err := f.quests.Update(f.ctx, f.admin, f.h, q.ID, app.QuestInput{
			Name: q.Name, Points: num(5), Shareable: q.Shareable, MinAge: num(2), MaxAge: num(4),
		}, ""); err != nil {
			t.Fatalf("narrow %s: %v", q.Name, err)
		}
	}
	f.setAge(holder, 16)

	if got := f.stored(solo.ID).ClaimedBy; got == nil || *got != partner {
		t.Errorf("solo holder after narrowing: %v, want Partner", got)
	}
	sharedView, err := f.quests.Get(f.ctx, f.admin, f.h, shared.ID)
	if err != nil {
		t.Fatalf("get shared: %v", err)
	}
	if sharedView.ClaimedBy == nil || *sharedView.ClaimedBy != holder || len(sharedView.Participants) != 2 {
		t.Errorf("shared quest after narrowing: holder %v participants %v", sharedView.ClaimedBy, sharedView.Participants)
	}
	// Both are listed as out of range now, yet still theirs.
	if len(sharedView.IneligibleMemberIDs) != 2 {
		t.Errorf("shared quest ineligible %v, want both members", sharedView.IneligibleMemberIDs)
	}

	list, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil || len(list) != 2 {
		t.Fatalf("list returns %d quests (%v), want both ranged quests", len(list), err)
	}

	if _, err := f.quests.MarkDone(f.ctx, f.admin, f.h, solo.ID, app.MemberAction{MemberID: partner}); err != nil {
		t.Errorf("mark done solo after narrowing: %v", err)
	}
	if _, err := f.quests.MarkDone(f.ctx, f.admin, f.h, shared.ID, app.MemberAction{MemberID: holder}); err != nil {
		t.Errorf("mark done shared after narrowing: %v", err)
	}
}
