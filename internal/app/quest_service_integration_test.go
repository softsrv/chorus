//go:build integration

package app_test

import (
	"context"
	"errors"
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
	familySvc := app.NewFamilyService(q, 10)
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

// TestIntegration_QuestApproval walks a quest through its lifecycle: a member
// accepts it, abandons it, another member takes it, marks it done, the
// administrator sends it back and then approves it. Points move only on
// approval, exactly once, in the same transaction as the quest update, and the
// repeat countdown starts at approval.
func TestIntegration_QuestApproval(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, 10)
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

	// Approved: released, completed, and out of the pool for its 3 days.
	approved := get()
	if approved.ClaimedBy != nil || approved.ClaimedAt.Valid || approved.DoneAt.Valid || !approved.CompletedAt.Valid ||
		!approved.AvailableAt.Valid || approved.AvailableAt.Time.Sub(approved.CompletedAt.Time) != 72*time.Hour {
		t.Fatalf("approved quest: %+v", approved)
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
