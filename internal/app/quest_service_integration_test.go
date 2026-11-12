//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	rewards := app.NewRewardService(q)
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
	if !app.QuestAvailable(quest, time.Now()) {
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
		accepted.Notes != "Half a scoop" || app.QuestAvailable(accepted, time.Now()) {
		t.Errorf("accepted quest: %+v", accepted)
	}

	// The list marks it unavailable, so it is in no member's available set.
	list, _, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list[0].ClaimedBy == nil || *list[0].ClaimedBy != memberA.ID || app.QuestAvailable(list[0], time.Now()) {
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
		quest    db.HouseholdQuest
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

// TestIntegration_QuestApproval walks a quest through its lifecycle: a member
// accepts it, abandons it, another member takes it, marks it done, the
// administrator sends it back and then approves it. Points move only on
// approval, exactly once, in the same transaction as the quest update, and the
// quest comes back at household-local midnight, repeat_days after the day it
// was marked done.
func TestIntegration_QuestApproval(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questapprove").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Approval Family"})
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
	get := func() db.HouseholdQuest {
		t.Helper()
		got, err := quests.Get(ctx, alice, h.ID, quest.ID)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return got
	}
	heldBy := func(label string, want uuid.UUID, done bool) {
		t.Helper()
		got := get()
		if got.ClaimedBy == nil || *got.ClaimedBy != want || got.DoneAt.Valid != done {
			t.Errorf("%s: claimed_by %v done_at %v; want %v, done=%v", label, got.ClaimedBy, got.DoneAt, want, done)
		}
	}
	pending := func(adminPin string) []db.ListPendingQuestsRow {
		t.Helper()
		rows, err := quests.ListPending(ctx, alice, h.ID, adminPin)
		if err != nil || rows == nil {
			t.Fatalf("list pending: %v %v", rows, err)
		}
		return rows
	}

	// Declining is client-side: reading the quest writes nothing.
	if got := get(); got.ClaimedBy != nil || !app.QuestAvailable(got, time.Now()) {
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
	heldBy("after refused actions", ann.ID, false)

	// Ann abandons: the quest is back in the pool and Ben can take it.
	abandoned, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asAnn)
	if err != nil || abandoned.ClaimedBy != nil || abandoned.ClaimedAt.Valid || !app.QuestAvailable(abandoned, time.Now()) {
		t.Fatalf("Ann abandons: %+v %v", abandoned, err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("Ben accepts after abandon: %v", err)
	}

	// Ben marks it done: it waits for approval and no points move.
	done, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen)
	if err != nil || !done.DoneAt.Valid || done.ClaimedBy == nil || *done.ClaimedBy != ben.ID {
		t.Fatalf("Ben marks done: %+v %v", done, err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after mark done: %d, want 0", p)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mark done twice: %v, want ErrNotFound", err)
	}
	if rows := pending(""); len(rows) != 1 || rows[0].HouseholdQuest.ID != quest.ID || rows[0].MemberName != "Ben" ||
		rows[0].HouseholdQuest.Points != 7 || rows[0].HouseholdQuest.ClaimedBy == nil || *rows[0].HouseholdQuest.ClaimedBy != ben.ID {
		t.Errorf("pending: %+v", rows)
	}
	// A quest waiting for approval cannot be abandoned.
	if _, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("abandon while pending: %v, want ErrNotFound", err)
	}
	heldBy("after abandon while pending", ben.ID, true)

	// Sent back: still Ben's, not done, no points, nothing waiting.
	rejected, err := quests.Reject(ctx, alice, h.ID, quest.ID, "")
	if err != nil || rejected.DoneAt.Valid || rejected.ClaimedBy == nil || *rejected.ClaimedBy != ben.ID {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after reject: %d, want 0", p)
	}
	if len(pending("")) != 0 {
		t.Error("a rejected quest is still waiting")
	}
	if _, err := quests.Reject(ctx, alice, h.ID, quest.ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("reject a quest that is not pending: %v, want ErrNotFound", err)
	}
	if _, _, err := quests.Approve(ctx, alice, h.ID, quest.ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("approve a quest that is not pending: %v, want ErrNotFound", err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after approving a quest that was not pending: %d, want 0", p)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("mark done again after reject: %v", err)
	}

	// With an admin PIN set, a missing or wrong PIN changes nothing.
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "2468", ""); err != nil {
		t.Fatalf("set admin pin: %v", err)
	}
	if _, _, err := quests.Approve(ctx, alice, h.ID, quest.ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("approve without admin pin: %v, want ErrAdminPinRequired", err)
	}
	if _, _, err := quests.Approve(ctx, alice, h.ID, quest.ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("approve with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if _, err := quests.Reject(ctx, alice, h.ID, quest.ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("reject with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if _, err := quests.Reject(ctx, alice, h.ID, quest.ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("reject without admin pin: %v, want ErrAdminPinRequired", err)
	}
	if _, err := quests.ListPending(ctx, alice, h.ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("pending with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after refused approvals: %d, want 0", p)
	}
	heldBy("after refused approvals", ben.ID, true)

	// A failure after the quest update rolls back both: a trigger makes the
	// points credit fail, and the quest is still pending with no points.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION test_fail_points() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'points credit failed'; END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fail_points() CASCADE`) })
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_fail_points BEFORE UPDATE OF points ON family_members
		FOR EACH ROW WHEN (NEW.id = '`+ben.ID.String()+`') EXECUTE FUNCTION test_fail_points()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, _, err := quests.Approve(ctx, alice, h.ID, quest.ID, "2468"); err == nil {
		t.Fatal("approve succeeded although the points credit failed")
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION test_fail_points() CASCADE`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	heldBy("after a failed approval", ben.ID, true)
	if got := get(); got.CompletedAt.Valid || got.AvailableAt.Valid {
		t.Errorf("a failed approval left the quest completed: %+v", got)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after a failed approval: %d, want 0", p)
	}

	// Two approvals at once: exactly one credits the points.
	doneAt := get().DoneAt.Time
	start := make(chan struct{})
	type result struct {
		member db.FamilyMember
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			_, m, err := quests.Approve(ctx, alice, h.ID, quest.ID, "2468")
			results <- result{m, err}
		}()
	}
	close(start)
	var wins, notFound int
	for range 2 {
		r := <-results
		switch {
		case r.err == nil:
			wins++
			if r.member.ID != ben.ID || r.member.Points != 7 {
				t.Errorf("credited member: %+v, want Ben with 7 points", r.member)
			}
		case errors.Is(r.err, app.ErrNotFound):
			notFound++
		default:
			t.Errorf("concurrent approve: %v", r.err)
		}
	}
	if wins != 1 || notFound != 1 {
		t.Errorf("concurrent approvals: %d won, %d not found; want 1 and 1", wins, notFound)
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("Ben's points after approval: %d, want 7", p)
	}
	// Approving again credits nothing more.
	if _, _, err := quests.Approve(ctx, alice, h.ID, quest.ID, "2468"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("repeat approve: %v, want ErrNotFound", err)
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("Ben's points after repeat approve: %d, want 7", p)
	}

	// Approved: released, completed, and out of the pool until 00:00 (the
	// household's default UTC) three days after the day it was marked done.
	approved := get()
	y, m, d := doneAt.UTC().Date()
	wantBack := time.Date(y, m, d+3, 0, 0, 0, 0, time.UTC)
	if approved.ClaimedBy != nil || approved.ClaimedAt.Valid || approved.DoneAt.Valid || !approved.CompletedAt.Valid ||
		!approved.AvailableAt.Valid || !approved.AvailableAt.Time.Equal(wantBack) {
		t.Fatalf("approved quest: %+v, want available_at %v", approved, wantBack)
	}
	if app.QuestAvailable(approved, time.Now()) {
		t.Error("an approved quest is available before its repeat days pass")
	}
	if len(pending("2468")) != 0 {
		t.Error("an approved quest is still waiting")
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("accept during repeat days: %v, want ErrQuestTaken", err)
	}

	// Once its repeat days have passed it is back in the pool.
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, quest.ID); err != nil {
		t.Fatalf("age available_at: %v", err)
	}
	list, _, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10})
	if err != nil || len(list) != 1 || !app.QuestAvailable(list[0], time.Now()) {
		t.Fatalf("list after repeat days: %+v %v", list, err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("accept after repeat days: %v", err)
	}

	// With no admin PIN, being the administrator is enough to approve.
	if err := familySvc.ClearAdminPin(ctx, alice, h.ID, "2468"); err != nil {
		t.Fatalf("clear admin pin: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("Ann marks done: %v", err)
	}
	if _, m, err := quests.Approve(ctx, alice, h.ID, quest.ID, ""); err != nil || m.ID != ann.ID || m.Points != 7 {
		t.Errorf("approve with no admin pin set: %+v %v", m, err)
	}
	if p := points(ann.ID); p != 7 {
		t.Errorf("Ann's points: %d, want 7", p)
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("Ben's points changed on Ann's approval: %d, want 7", p)
	}
}

// TestIntegration_QuestResetsAtLocalMidnight pins the return rule: an approved
// quest comes back at 00:00 in its household's timezone on local day D+N,
// where D is the local day its holder marked it done and N its repeat_days.
// The approval time never enters it, and nothing but an approval sets it.
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
	get := func(f family, id uuid.UUID) db.HouseholdQuest {
		t.Helper()
		got, err := quests.Get(ctx, alice, f.id, id)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return got
	}
	// doneAt has Ann accept and mark the quest done, then moves done_at to the
	// given instant, as if she had marked it done then.
	doneAt := func(f family, id uuid.UUID, at time.Time) {
		t.Helper()
		if _, err := quests.Accept(ctx, alice, f.id, id, f.ann); err != nil {
			t.Fatalf("accept: %v", err)
		}
		if _, err := quests.MarkDone(ctx, alice, f.id, id, f.ann); err != nil {
			t.Fatalf("mark done: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET done_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatalf("age done_at: %v", err)
		}
	}
	approve := func(f family, id uuid.UUID) db.HouseholdQuest {
		t.Helper()
		got, _, err := quests.Approve(ctx, alice, f.id, id, "")
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		return got
	}
	// back marks a quest done at `at`, approves it now, and returns available_at.
	back := func(f family, days int64, at time.Time) time.Time {
		t.Helper()
		id := newQuest(f, days)
		doneAt(f, id, at)
		got := approve(f, id)
		if !got.AvailableAt.Valid || !got.CompletedAt.Valid || got.CompletedAt.Time.Before(time.Now().Add(-time.Minute)) {
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
		// Marked done at 20:00 yesterday (local): back at 00:00 today, so it
		// can be picked up this morning however late the approval.
		now := time.Now().In(chicagoTZ)
		yesterday20 := time.Date(now.Year(), now.Month(), now.Day()-1, 20, 0, 0, 0, chicagoTZ)
		todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, chicagoTZ)
		id := newQuest(chicago, 1)
		doneAt(chicago, id, yesterday20)
		if got := approve(chicago, id); !got.AvailableAt.Time.Equal(todayMidnight) || !app.QuestAvailable(got, time.Now()) {
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
		doneAt(chicago, id, time.Now())
		approve(chicago, id)
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
		if got := get(chicago, id); got.AvailableAt.Valid || got.CompletedAt.Valid || !app.QuestAvailable(got, time.Now()) {
			t.Fatalf("new quest: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Errorf("accept a new quest: %v", err)
		}
	})

	t.Run("no return time without approval", func(t *testing.T) {
		unearned := func(label string, id uuid.UUID) {
			t.Helper()
			if got := get(chicago, id); got.AvailableAt.Valid || got.CompletedAt.Valid {
				t.Errorf("%s: available_at %v completed_at %v, want both unset", label, got.AvailableAt, got.CompletedAt)
			}
		}
		// Done, sent back, done again, sent back, put down: another takes it.
		id := newQuest(chicago, 3)
		doneAt(chicago, id, time.Now().Add(-48*time.Hour))
		if _, err := quests.Reject(ctx, alice, chicago.id, id, ""); err != nil {
			t.Fatalf("reject: %v", err)
		}
		unearned("after reject", id)
		if _, err := quests.MarkDone(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("mark done again: %v", err)
		}
		if _, err := quests.Reject(ctx, alice, chicago.id, id, ""); err != nil {
			t.Fatalf("reject again: %v", err)
		}
		if _, err := quests.Abandon(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		unearned("after abandon", id)
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after reject and abandon: %v", err)
		}

		// Taken and put down.
		id = newQuest(chicago, 3)
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("accept: %v", err)
		}
		if _, err := quests.Abandon(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		unearned("after a plain abandon", id)
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after abandon: %v", err)
		}

		// Marked done, then its holder is removed from the household.
		f := newFamily("Release Family", "America/Chicago")
		id = newQuest(f, 3)
		doneAt(f, id, time.Now().Add(-48*time.Hour))
		if err := familySvc.Remove(ctx, alice, f.id, f.ann.MemberID, ""); err != nil {
			t.Fatalf("remove Ann: %v", err)
		}
		if got := get(f, id); got.AvailableAt.Valid || got.CompletedAt.Valid || got.ClaimedBy != nil {
			t.Errorf("after release: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, f.id, id, f.ben); err != nil {
			t.Errorf("accept after release: %v", err)
		}

		// Approved once and back: a rejection keeps its earned return time.
		id = newQuest(chicago, 1)
		doneAt(chicago, id, time.Now().Add(-72*time.Hour))
		earned := approve(chicago, id)
		doneAt(chicago, id, time.Now())
		if _, err := quests.Reject(ctx, alice, chicago.id, id, ""); err != nil {
			t.Fatalf("reject: %v", err)
		}
		if got := get(chicago, id); !got.AvailableAt.Time.Equal(earned.AvailableAt.Time) ||
			!got.CompletedAt.Time.Equal(earned.CompletedAt.Time) {
			t.Errorf("reject moved the return time: %v %v, want %v %v",
				got.AvailableAt.Time, got.CompletedAt.Time, earned.AvailableAt.Time, earned.CompletedAt.Time)
		}
	})
}
