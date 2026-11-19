//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

func num(n int64) app.NumberField { return app.NumberField{Set: true, Value: n} }

// completionStatusBy reads a quest completion's stored status and status_by
// (nil when NULL) straight from the table.
func completionStatusBy(t *testing.T, pool *pgxpool.Pool, completionID uuid.UUID) (string, *uuid.UUID) {
	t.Helper()
	var status string
	var by *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT status, status_by FROM quest_completions WHERE id = $1`, completionID).Scan(&status, &by); err != nil {
		t.Fatalf("read completion status_by: %v", err)
	}
	return status, by
}

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

// TestIntegration_QuestWritesNeedAdminPin: once an admin PIN is set, quest
// writes need it; a missing or wrong PIN changes nothing.
func TestIntegration_QuestWritesNeedAdminPin(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 4)
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
	familySvc := app.NewFamilyService(q, pool, 4)
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
	familySvc := app.NewFamilyService(q, pool, 4)
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
