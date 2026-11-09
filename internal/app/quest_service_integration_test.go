//go:build integration

package app_test

import (
	"context"
	"errors"
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
	if list, _, _ := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10}); len(list) != 2 {
		t.Fatalf("rejected creates saved rows: %d quests", len(list))
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
	item := library[0]

	copied, err := quests.CopyFromLibrary(ctx, alice, h.ID, item.ID, "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if copied.LibraryID == nil || *copied.LibraryID != item.ID || copied.Name != item.Name ||
		copied.Points != item.Points || copied.RepeatDays != item.RepeatDays || copied.Notes != item.Notes {
		t.Fatalf("copy %+v does not match library %+v", copied, item)
	}
	if _, err := quests.Update(ctx, alice, h.ID, copied.ID, app.QuestInput{
		Name: "Our own version", Points: num(int64(item.Points) + 100), RepeatDays: num(30), Notes: "edited",
	}, ""); err != nil {
		t.Fatalf("edit copy: %v", err)
	}
	after, err := q.GetQuestLibraryItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("reload library item: %v", err)
	}
	if after.Name != item.Name || after.Points != item.Points || after.RepeatDays != item.RepeatDays || after.Notes != item.Notes {
		t.Fatalf("library changed by editing a copy: %+v -> %+v", item, after)
	}

	if _, err := quests.CopyFromLibrary(ctx, alice, h.ID, h.ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("copy of unknown library id: %v", err)
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
