//go:build integration

package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// questShareFields reads a quest's stored shareable and teamwork_bonus.
func questShareFields(t *testing.T, pool *pgxpool.Pool, questID uuid.UUID) (bool, int32) {
	t.Helper()
	var shareable bool
	var bonus int32
	if err := pool.QueryRow(context.Background(),
		`SELECT shareable, teamwork_bonus FROM household_quests WHERE id = $1`, questID).Scan(&shareable, &bonus); err != nil {
		t.Fatalf("read share fields: %v", err)
	}
	return shareable, bonus
}

// TestIntegration_QuestSharingFields: an administrator creates a quest with
// shareable and a teamwork bonus, and Get and List return both; an edit
// replaces them, and turning shareable off with the bonus at 0 stores that.
// Quests created without the fields, and library copies, are not shareable
// with bonus 0. A teamwork_bonus that is negative, not a whole number, or
// above 0 on a quest that is not shareable is a 422 on teamwork_bonus and
// saves nothing; the database refuses a bonus without shareable too.
func TestIntegration_QuestSharingFields(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questsharefields").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Fields"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	created, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Clean the living room", Points: num(100), Shareable: true, TeamworkBonus: num(20)}, "")
	if err != nil {
		t.Fatalf("create shareable quest: %v", err)
	}
	if !created.Shareable || created.TeamworkBonus != 20 {
		t.Errorf("created: shareable=%v bonus=%d, want true 20", created.Shareable, created.TeamworkBonus)
	}
	got, err := quests.Get(ctx, admin, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Shareable || got.TeamworkBonus != 20 || got.Shared() || len(got.Participants) != 0 {
		t.Errorf("get: shareable=%v bonus=%d shared=%v participants=%v, want true 20 false []", got.Shareable, got.TeamworkBonus, got.Shared(), got.Participants)
	}
	plain, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Take out trash", Points: num(10)}, "")
	if err != nil {
		t.Fatalf("create plain quest: %v", err)
	}
	list, _, err := quests.List(ctx, admin, h.ID, app.QuestPage{Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := 0
	for _, v := range list {
		switch v.ID {
		case created.ID:
			seen++
			if !v.Shareable || v.TeamworkBonus != 20 {
				t.Errorf("listed shareable quest: shareable=%v bonus=%d, want true 20", v.Shareable, v.TeamworkBonus)
			}
		case plain.ID:
			seen++
			if v.Shareable || v.TeamworkBonus != 0 {
				t.Errorf("listed plain quest: shareable=%v bonus=%d, want false 0", v.Shareable, v.TeamworkBonus)
			}
		}
	}
	if seen != 2 {
		t.Errorf("list showed %d of the 2 quests", seen)
	}

	// Edit: a new bonus, then shareable off with the bonus back to 0.
	updated, err := quests.Update(ctx, admin, h.ID, created.ID, app.QuestInput{Name: "Clean the living room", Points: num(100), Shareable: true, TeamworkBonus: num(35)}, "")
	if err != nil {
		t.Fatalf("update bonus: %v", err)
	}
	if !updated.Shareable || updated.TeamworkBonus != 35 {
		t.Errorf("updated: shareable=%v bonus=%d, want true 35", updated.Shareable, updated.TeamworkBonus)
	}
	if s, b := questShareFields(t, pool, created.ID); !s || b != 35 {
		t.Errorf("stored after update: shareable=%v bonus=%d, want true 35", s, b)
	}
	if _, err := quests.Update(ctx, admin, h.ID, plain.ID, app.QuestInput{Name: "Take out trash", Points: num(10), Shareable: true}, ""); err != nil {
		t.Fatalf("make plain shareable: %v", err)
	}
	if s, b := questShareFields(t, pool, plain.ID); !s || b != 0 {
		t.Errorf("plain made shareable: shareable=%v bonus=%d, want true 0", s, b)
	}
	off, err := quests.Update(ctx, admin, h.ID, created.ID, app.QuestInput{Name: "Clean the living room", Points: num(100)}, "")
	if err != nil {
		t.Fatalf("update shareable off: %v", err)
	}
	if off.Shareable || off.TeamworkBonus != 0 {
		t.Errorf("shareable off: shareable=%v bonus=%d, want false 0", off.Shareable, off.TeamworkBonus)
	}

	// Refused bonuses: field error on teamwork_bonus, nothing saved.
	if _, err := quests.Update(ctx, admin, h.ID, created.ID, app.QuestInput{Name: "Clean the living room", Points: num(100), Shareable: true, TeamworkBonus: num(20)}, ""); err != nil {
		t.Fatalf("restore bonus: %v", err)
	}
	bad := []struct {
		name string
		in   app.QuestInput
	}{
		{"negative", app.QuestInput{Name: "Bad", Shareable: true, TeamworkBonus: num(-1)}},
		{"not a whole number", app.QuestInput{Name: "Bad", Shareable: true, TeamworkBonus: app.NumberField{Set: true, Invalid: true}}},
		{"bonus without shareable", app.QuestInput{Name: "Bad", TeamworkBonus: num(5)}},
	}
	for _, c := range bad {
		var ve *app.ValidationError
		if _, err := quests.Create(ctx, admin, h.ID, c.in, ""); !errors.As(err, &ve) || ve.Fields["teamwork_bonus"] == "" {
			t.Errorf("create %s: %v, want a teamwork_bonus field error", c.name, err)
		}
		if _, err := quests.Update(ctx, admin, h.ID, created.ID, c.in, ""); !errors.As(err, &ve) || ve.Fields["teamwork_bonus"] == "" {
			t.Errorf("update %s: %v, want a teamwork_bonus field error", c.name, err)
		}
	}
	var bads int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM household_quests WHERE household_id = $1 AND name = 'Bad'`, h.ID).Scan(&bads); err != nil {
		t.Fatalf("count bad quests: %v", err)
	}
	if bads != 0 {
		t.Errorf("refused creates saved %d quests", bads)
	}
	if s, b := questShareFields(t, pool, created.ID); !s || b != 20 {
		t.Errorf("after refused updates: shareable=%v bonus=%d, want true 20", s, b)
	}
	// The database holds the rule on its own as well.
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET shareable = false WHERE id = $1`, created.ID); err == nil {
		t.Error("database stored a teamwork bonus on a quest that is not shareable")
	}
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET teamwork_bonus = -1 WHERE id = $1`, created.ID); err == nil {
		t.Error("database stored a negative teamwork bonus")
	}

	// A library copy keeps the defaults.
	library, err := quests.ListLibrary(ctx)
	if err != nil || len(library) == 0 {
		t.Fatalf("list library: %d items, %v", len(library), err)
	}
	copied, err := quests.CopyFromLibrary(ctx, admin, h.ID, library[0].ID, "")
	if err != nil {
		t.Fatalf("copy from library: %v", err)
	}
	if copied.Shareable || copied.TeamworkBonus != 0 {
		t.Errorf("library copy: shareable=%v bonus=%d, want false 0", copied.Shareable, copied.TeamworkBonus)
	}
}

// participantRows counts the participant rows of a quest.
func participantRows(t *testing.T, pool *pgxpool.Pool, questID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM household_quest_participants WHERE quest_id = $1`, questID).Scan(&n); err != nil {
		t.Fatalf("count participants: %v", err)
	}
	return n
}

// TestIntegration_QuestSharing: the holder of a shareable quest shares it
// with another household member, each proving themselves with their own PIN.
// Afterwards Get and List show the quest shared with both member ids (the
// holder first) and it is still held. Every refused share (wrong PIN of
// either member, a quest that is not shareable or not held by the sharer,
// the sharer themselves, a member already on it, a member of another
// household or one that does not exist) changes nothing; a quest that is
// never shared has no participant rows.
func TestIntegration_QuestSharing(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questshare").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Sharers"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	newMember := func(hid uuid.UUID, name, pin string) uuid.UUID {
		t.Helper()
		m, err := familySvc.Create(ctx, admin, hid, app.FamilyMemberInput{Name: name, PIN: pin}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return m.ID
	}
	ann := newMember(h.ID, "Ann", "1111")
	ben := newMember(h.ID, "Ben", "2222")
	cara := newMember(h.ID, "Cara", "")
	annAct := app.MemberAction{MemberID: ann, PIN: "1111"}
	benAct := app.MemberAction{MemberID: ben, PIN: "2222"}
	caraAct := app.MemberAction{MemberID: cara}

	// Another household of the same user, with its own member.
	other, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Elsewhere"})
	if err != nil {
		t.Fatalf("create other household: %v", err)
	}
	outsider := newMember(other.ID, "Out", "")

	shared, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Clean the living room", Points: num(100), Shareable: true, TeamworkBonus: num(20)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	plain, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Take out trash", Points: num(10)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}

	// Sharing before anyone holds it: not found.
	if _, err := quests.Share(ctx, admin, h.ID, shared.ID, annAct, benAct); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("share unheld quest: %v, want ErrNotFound", err)
	}
	if _, err := quests.Accept(ctx, admin, h.ID, shared.ID, annAct); err != nil {
		t.Fatalf("Ann accept: %v", err)
	}
	if _, err := quests.Accept(ctx, admin, h.ID, plain.ID, annAct); err != nil {
		t.Fatalf("Ann accept plain: %v", err)
	}

	refused := []struct {
		name          string
		quest         uuid.UUID
		act, target   app.MemberAction
		want          error
		viaOtherHouse bool
	}{
		{"wrong target pin", shared.ID, annAct, app.MemberAction{MemberID: ben, PIN: "9999"}, app.ErrWrongPIN, false},
		{"missing target pin", shared.ID, annAct, app.MemberAction{MemberID: ben}, app.ErrWrongPIN, false},
		{"wrong sharer pin", shared.ID, app.MemberAction{MemberID: ann, PIN: "0000"}, benAct, app.ErrWrongPIN, false},
		{"not shareable", plain.ID, annAct, benAct, app.ErrNotFound, false},
		{"not the holder", shared.ID, benAct, caraAct, app.ErrNotFound, false},
		{"with themselves", shared.ID, annAct, annAct, app.ErrAlreadyParticipant, false},
		{"member of another household", shared.ID, annAct, app.MemberAction{MemberID: outsider}, app.ErrNotFound, false},
		{"unknown member", shared.ID, annAct, app.MemberAction{MemberID: uuid.New()}, app.ErrNotFound, false},
		{"unknown quest", uuid.New(), annAct, benAct, app.ErrNotFound, false},
		{"quest through another household", shared.ID, annAct, benAct, app.ErrNotFound, true},
	}
	for _, c := range refused {
		hid := h.ID
		if c.viaOtherHouse {
			hid = other.ID
		}
		if _, err := quests.Share(ctx, admin, hid, c.quest, c.act, c.target); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if n := participantRows(t, pool, shared.ID); n != 0 {
		t.Errorf("refused shares wrote %d participant rows", n)
	}
	if n := participantRows(t, pool, plain.ID); n != 0 {
		t.Errorf("plain quest has %d participant rows", n)
	}

	got, err := quests.Share(ctx, admin, h.ID, shared.ID, annAct, benAct)
	if err != nil {
		t.Fatalf("share with Ben: %v", err)
	}
	wantBoth := []uuid.UUID{ann, ben}
	if !got.Shared() || !slices.Equal(got.Participants, wantBoth) || got.ClaimedBy == nil || *got.ClaimedBy != ann {
		t.Errorf("shared quest: shared=%v participants=%v claimed_by=%v, want true %v %v", got.Shared(), got.Participants, got.ClaimedBy, wantBoth, ann)
	}
	view, err := quests.Get(ctx, admin, h.ID, shared.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !view.Shared() || !slices.Equal(view.Participants, wantBoth) {
		t.Errorf("get shared quest: participants %v, want %v", view.Participants, wantBoth)
	}
	list, _, err := quests.List(ctx, admin, h.ID, app.QuestPage{Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, v := range list {
		switch v.ID {
		case shared.ID:
			if !v.Shared() || !slices.Equal(v.Participants, wantBoth) {
				t.Errorf("listed shared quest: participants %v, want %v", v.Participants, wantBoth)
			}
		case plain.ID:
			if v.Shared() || len(v.Participants) != 0 {
				t.Errorf("listed plain quest: participants %v, want none", v.Participants)
			}
		}
	}

	// Ben is already on it; Ben cannot share it on (only the holder can).
	if _, err := quests.Share(ctx, admin, h.ID, shared.ID, annAct, benAct); !errors.Is(err, app.ErrAlreadyParticipant) {
		t.Errorf("share with Ben again: %v, want ErrAlreadyParticipant", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, shared.ID, benAct, caraAct); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Ben shares on: %v, want ErrNotFound", err)
	}
	if n := participantRows(t, pool, shared.ID); n != 2 {
		t.Errorf("after refused re-shares: %d participant rows, want 2", n)
	}

	// The holder adds a third member; Cara has no PIN, so hers is empty.
	got, err = quests.Share(ctx, admin, h.ID, shared.ID, annAct, caraAct)
	if err != nil {
		t.Fatalf("share with Cara: %v", err)
	}
	if want := []uuid.UUID{ann, ben, cara}; !slices.Equal(got.Participants, want) {
		t.Errorf("after third share: participants %v, want %v", got.Participants, want)
	}

	// Mark done (CLM-9). A household member who is not on the quest cannot
	// mark it done, and nothing changes.
	dan := newMember(h.ID, "Dan", "")
	if _, err := quests.MarkDone(ctx, admin, h.ID, shared.ID, app.MemberAction{MemberID: dan}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("non-participant marks done: %v, want ErrNotFound", err)
	}
	if _, err := quests.MarkDone(ctx, admin, other.ID, shared.ID, app.MemberAction{MemberID: outsider}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("outsider marks done via own household: %v, want ErrNotFound", err)
	}
	if n, p := completionRows(t, pool, shared.ID), participantRows(t, pool, shared.ID); n != 0 || p != 3 {
		t.Errorf("refused mark-done: %d completions, %d participants; want 0 and 3", n, p)
	}

	// Cara, a participant who is not the holder, marks it done: it completes
	// for all three at once as one pending completion.
	doneView, err := quests.MarkDone(ctx, admin, h.ID, shared.ID, caraAct)
	if err != nil || doneView.ClaimedBy != nil || !doneView.AwaitingApproval || doneView.Shared() {
		t.Fatalf("Cara marks done: %+v, %v", doneView, err)
	}
	if n := completionRows(t, pool, shared.ID); n != 1 {
		t.Errorf("shared mark-done: %d completions, want 1", n)
	}
	if n := participantRows(t, pool, shared.ID); n != 0 {
		t.Errorf("shared mark-done left %d participant rows, want 0", n)
	}
	if view, err := quests.Get(ctx, admin, h.ID, shared.ID); err != nil || view.ClaimedBy != nil || view.Shared() || len(view.Participants) != 0 {
		t.Errorf("after shared mark-done: %+v, %v; want released with no participants", view, err)
	}
	// Every member left the quest with it, so nobody can mark it done again.
	for _, act := range []app.MemberAction{annAct, benAct, caraAct} {
		if _, err := quests.MarkDone(ctx, admin, h.ID, shared.ID, act); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("mark done again as %v: %v, want ErrNotFound", act.MemberID, err)
		}
	}
	item := pendingFor(t, quests, admin, h.ID, shared.ID)
	wantThree := []app.PendingMember{{MemberID: ann, Name: "Ann", Award: 34}, {MemberID: ben, Name: "Ben", Award: 34}, {MemberID: cara, Name: "Cara", Award: 34}}
	if !slices.Equal(item.Members, wantThree) || item.TeamworkBonus != 20 || item.Points != 100 ||
		item.MemberID != cara || item.MemberName != "Cara" {
		t.Errorf("pending shared completion: %+v, want members %v, bonus 20, marked by Cara", item, wantThree)
	}

	// Sending it back (CLM-13) pays nobody and reopens the cooldown.
	pointsBefore := map[uuid.UUID]int32{}
	for _, id := range []uuid.UUID{ann, ben, cara} {
		pointsBefore[id] = memberPoints(t, q, h.ID, id)
	}
	rejected, err := quests.Reject(ctx, admin, h.ID, item.ID, "")
	if err != nil || rejected.Status != "rejected" || rejected.ID != item.ID {
		t.Fatalf("reject shared completion: %+v, %v", rejected, err)
	}
	for _, id := range []uuid.UUID{ann, ben, cara} {
		if p := memberPoints(t, q, h.ID, id); p != pointsBefore[id] {
			t.Errorf("member %v points after reject: %d, want %d", id, p, pointsBefore[id])
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM quest_completions WHERE id = $1`, item.ID).Scan(&status); err != nil || status != "rejected" {
		t.Errorf("stored status after reject: %q, %v; want rejected", status, err)
	}
	if view, err := quests.Get(ctx, admin, h.ID, shared.ID); err != nil || !app.QuestAvailable(view.HouseholdQuest, time.Now()) || view.AwaitingApproval {
		t.Errorf("after reject the shared quest should be available now: %+v, %v", view, err)
	}

	// Two members: Ann takes it again and shares with Ben; Ben marks done.
	if _, err := quests.Accept(ctx, admin, h.ID, shared.ID, annAct); err != nil {
		t.Fatalf("Ann accepts again: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, shared.ID, annAct, benAct); err != nil {
		t.Fatalf("share with Ben again: %v", err)
	}
	if _, err := quests.MarkDone(ctx, admin, h.ID, shared.ID, benAct); err != nil {
		t.Fatalf("Ben marks done: %v", err)
	}
	item = pendingFor(t, quests, admin, h.ID, shared.ID)
	wantTwo := []app.PendingMember{{MemberID: ann, Name: "Ann", Award: 50}, {MemberID: ben, Name: "Ben", Award: 50}}
	if !slices.Equal(item.Members, wantTwo) || item.TeamworkBonus != 20 || item.MemberID != ben {
		t.Errorf("pending two-member completion: %+v, want members %v, bonus 20, marked by Ben", item, wantTwo)
	}
	if n := participantRows(t, pool, shared.ID); n != 0 {
		t.Errorf("two-member mark-done left %d participant rows", n)
	}

	// A quest that is never shared is one member with the full points and no
	// teamwork bonus.
	if _, err := quests.MarkDone(ctx, admin, h.ID, plain.ID, annAct); err != nil {
		t.Fatalf("Ann marks plain done: %v", err)
	}
	item = pendingFor(t, quests, admin, h.ID, plain.ID)
	if want := []app.PendingMember{{MemberID: ann, Name: "Ann", Award: 10}}; !slices.Equal(item.Members, want) ||
		item.TeamworkBonus != 0 || item.MemberID != ann || item.Points != 10 {
		t.Errorf("pending plain completion: %+v, want only Ann with 10 and no bonus", item)
	}
	if rows, err := quests.ListPending(ctx, admin, other.ID, ""); err != nil || len(rows) != 0 {
		t.Errorf("other household's pending list: %+v, %v; want empty", rows, err)
	}
}

// completionRows counts the completions of a quest, whatever their status.
func completionRows(t *testing.T, pool *pgxpool.Pool, questID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM quest_completions WHERE quest_id = $1`, questID).Scan(&n); err != nil {
		t.Fatalf("count completions: %v", err)
	}
	return n
}

// pendingFor is the one pending item of a quest in the admin's list.
func pendingFor(t *testing.T, quests *app.QuestService, admin, householdID, questID uuid.UUID) app.PendingCompletion {
	t.Helper()
	rows, err := quests.ListPending(context.Background(), admin, householdID, "")
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	var found []app.PendingCompletion
	for _, r := range rows {
		if r.QuestID == questID {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("pending items for quest %v: %+v, want exactly 1", questID, found)
	}
	return found[0]
}

// memberPoints is a member's stored point balance.
func memberPoints(t *testing.T, q *db.Queries, householdID, memberID uuid.UUID) int32 {
	t.Helper()
	m, err := q.GetFamilyMember(context.Background(), db.GetFamilyMemberParams{HouseholdID: householdID, ID: memberID})
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	return m.Points
}
