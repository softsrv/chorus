//go:build integration

package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	quests := app.NewQuestService(q)
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
	want("created", created, "Mow the lawn", 25, 7, "Front and back")

	got, err := quests.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want("get", got, "Mow the lawn", 25, 7, "Front and back")

	list, more, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10})
	if err != nil || len(list) != 1 || more {
		t.Fatalf("list: %d items, more=%v, err=%v", len(list), more, err)
	}
	want("list", list[0], "Mow the lawn", 25, 7, "Front and back")

	updated, err := quests.Update(ctx, alice, h.ID, created.ID, app.QuestInput{
		Name: "Mow the lawn (front)", Points: num(30), RepeatDays: num(14), Notes: "",
	}, "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	want("updated", updated, "Mow the lawn (front)", 30, 14, "")

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
	if beforeInvalidUpdate != afterInvalidUpdate {
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
	quests := app.NewQuestService(q)
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
	quests := app.NewQuestService(q)
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
	quests := app.NewQuestService(q)
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
	quests := app.NewQuestService(q)
	familySvc := app.NewFamilyService(q, 10)
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
