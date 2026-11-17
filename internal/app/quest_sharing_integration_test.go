//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
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
		} else if c.want == app.ErrWrongPIN && errors.Is(err, app.ErrWrongTargetPIN) != (c.act.MemberID == ann && c.act.PIN == annAct.PIN) {
			// Whose PIN failed is told apart: the target's is ErrWrongTargetPIN,
			// the sharer's is not.
			t.Errorf("%s: %v, ErrWrongTargetPIN only for the target's PIN", c.name, err)
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

// TestIntegration_QuestSharingApprove (CLM-11, CLM-12): approving a shared
// completion credits each recorded member ceil(points/n) plus the quest's
// teamwork bonus as it stands at approval, all in one transaction. Two
// members of a 100-point quest with bonus 20 get 70 each, three get 34 + 20;
// a quest edited to not shareable after mark-done still splits among the
// recorded members but pays no bonus. A repeated or concurrent approval
// credits once, and a failure crediting any member pays nobody.
func TestIntegration_QuestSharingApprove(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questshareapprove").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Payers"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	newMember := func(name string) app.MemberAction {
		t.Helper()
		m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: name}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return app.MemberAction{MemberID: m.ID}
	}
	ann, ben, cara := newMember("Ann"), newMember("Ben"), newMember("Cara")
	quest, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Clean the living room", Points: num(100), Shareable: true, TeamworkBonus: num(20)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	// sharedDone has Ann accept the quest, share it with the others, and the
	// last of them mark it done; it returns the pending completion's id.
	sharedDone := func(with ...app.MemberAction) uuid.UUID {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NULL WHERE id = $1`, quest.ID); err != nil {
			t.Fatalf("end cooldown: %v", err)
		}
		if _, err := quests.Accept(ctx, admin, h.ID, quest.ID, ann); err != nil {
			t.Fatalf("Ann accepts: %v", err)
		}
		for _, m := range with {
			if _, err := quests.Share(ctx, admin, h.ID, quest.ID, ann, m); err != nil {
				t.Fatalf("share: %v", err)
			}
		}
		if _, err := quests.MarkDone(ctx, admin, h.ID, quest.ID, with[len(with)-1]); err != nil {
			t.Fatalf("mark done: %v", err)
		}
		return pendingFor(t, quests, admin, h.ID, quest.ID).ID
	}
	balances := func(ms ...app.MemberAction) []int32 {
		t.Helper()
		out := make([]int32, len(ms))
		for i, m := range ms {
			out[i] = memberPoints(t, q, h.ID, m.MemberID)
		}
		return out
	}
	storedAwards := func(completionID uuid.UUID) map[uuid.UUID]int32 {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT member_id, points_awarded FROM quest_completion_members WHERE completion_id = $1`, completionID)
		if err != nil {
			t.Fatalf("read awards: %v", err)
		}
		defer rows.Close()
		out := map[uuid.UUID]int32{}
		for rows.Next() {
			var id uuid.UUID
			var award *int32
			if err := rows.Scan(&id, &award); err != nil {
				t.Fatalf("scan award: %v", err)
			}
			if award == nil {
				out[id] = -1
			} else {
				out[id] = *award
			}
		}
		return out
	}

	// Two members: 50 + 20 each.
	cid := sharedDone(ben)
	before := balances(ann, ben, cara)
	comp, marker, bonuses, members, err := quests.Approve(ctx, admin, h.ID, cid, "")
	if err != nil {
		t.Fatalf("approve two-member completion: %v", err)
	}
	after := balances(ann, ben, cara)
	if after[0]-before[0] != 70 || after[1]-before[1] != 70 || after[2] != before[2] {
		t.Errorf("two members: balances %v -> %v, want +70 +70 +0", before, after)
	}
	if comp.Status != "approved" || comp.MemberID == nil || *comp.MemberID != ben.MemberID ||
		!slices.Equal(comp.MemberIDs, []uuid.UUID{ann.MemberID, ben.MemberID}) {
		t.Errorf("approved completion: %+v, want marked by Ben covering Ann and Ben", comp)
	}
	if marker.ID != ben.MemberID || marker.Points != after[1] || bonuses == nil || len(bonuses) != 0 {
		t.Errorf("approve marker %+v bonuses %v, want Ben at %d, no combos", marker, bonuses, after[1])
	}
	if len(members) != 2 || members[0].Member.ID != ann.MemberID || members[1].Member.ID != ben.MemberID {
		t.Fatalf("approve members: %+v, want Ann then Ben", members)
	}
	for i, m := range members {
		if m.PointsAwarded != 50 || m.TeamworkBonus != 20 || m.Member.Points != after[i] {
			t.Errorf("approved member %d: %+v, want 50 + 20 at balance %d", i, m, after[i])
		}
	}
	if got := storedAwards(cid); len(got) != 2 || got[ann.MemberID] != 70 || got[ben.MemberID] != 70 {
		t.Errorf("stored awards: %v, want 70 each", got)
	}
	// Approving again pays nothing.
	if _, _, _, _, err := quests.Approve(ctx, admin, h.ID, cid, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("approve again: %v, want ErrNotFound", err)
	}
	if again := balances(ann, ben, cara); !slices.Equal(again, after) {
		t.Errorf("approve again moved points: %v -> %v", after, again)
	}

	// Three members: ceil(100/3) = 34, + 20 = 54 each.
	cid = sharedDone(ben, cara)
	before = balances(ann, ben, cara)
	_, _, _, members, err = quests.Approve(ctx, admin, h.ID, cid, "")
	if err != nil {
		t.Fatalf("approve three-member completion: %v", err)
	}
	after = balances(ann, ben, cara)
	for i := range after {
		if after[i]-before[i] != 54 {
			t.Errorf("three members: member %d %d -> %d, want +54", i, before[i], after[i])
		}
	}
	if len(members) != 3 {
		t.Fatalf("three-member approve: %+v", members)
	}
	for _, m := range members {
		if m.PointsAwarded != 34 || m.TeamworkBonus != 20 {
			t.Errorf("three-member award: %+v, want 34 + 20", m)
		}
	}

	// Shareable turned off after mark-done: the split follows the recorded
	// members, the bonus is 0.
	cid = sharedDone(ben)
	if _, err := quests.Update(ctx, admin, h.ID, quest.ID, app.QuestInput{Name: "Clean the living room", Points: num(100)}, ""); err != nil {
		t.Fatalf("turn shareable off: %v", err)
	}
	before = balances(ann, ben, cara)
	_, _, _, members, err = quests.Approve(ctx, admin, h.ID, cid, "")
	if err != nil {
		t.Fatalf("approve after shareable off: %v", err)
	}
	after = balances(ann, ben, cara)
	if after[0]-before[0] != 50 || after[1]-before[1] != 50 || after[2] != before[2] {
		t.Errorf("shareable off: balances %v -> %v, want +50 +50 +0", before, after)
	}
	for _, m := range members {
		if m.PointsAwarded != 50 || m.TeamworkBonus != 0 {
			t.Errorf("shareable off award: %+v, want 50 and no bonus", m)
		}
	}
	if _, err := quests.Update(ctx, admin, h.ID, quest.ID, app.QuestInput{Name: "Clean the living room", Points: num(100), Shareable: true, TeamworkBonus: num(20)}, ""); err != nil {
		t.Fatalf("turn shareable back on: %v", err)
	}

	// Two concurrent approvals of one shared completion credit it once.
	cid = sharedDone(ben)
	before = balances(ann, ben)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, _, errs[i] = quests.Approve(ctx, admin, h.ID, cid, "")
		}(i)
	}
	wg.Wait()
	ok, notFound := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, app.ErrNotFound):
			notFound++
		default:
			t.Errorf("concurrent approve: %v", err)
		}
	}
	after = balances(ann, ben)
	if ok != 1 || notFound != 1 || after[0]-before[0] != 70 || after[1]-before[1] != 70 {
		t.Errorf("concurrent approvals: %d ok, %d not found, balances %v -> %v; want one credit of 70 each", ok, notFound, before, after)
	}

	// A failure crediting the second member rolls back the whole approval:
	// nobody is paid and the completion stays pending.
	cid = sharedDone(ben)
	failOn := ben.MemberID
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION fail_credit_%[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id = '%[2]s' AND NEW.points <> OLD.points THEN
    RAISE EXCEPTION 'injected credit failure';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER fail_credit_%[1]s BEFORE UPDATE ON family_members
  FOR EACH ROW EXECUTE FUNCTION fail_credit_%[1]s();`, failOn.String()[:8], failOn)); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS fail_credit_%[1]s ON family_members; DROP FUNCTION IF EXISTS fail_credit_%[1]s();`, failOn.String()[:8])) //nolint:errcheck
	})
	before = balances(ann, ben)
	if _, _, _, _, err := quests.Approve(ctx, admin, h.ID, cid, ""); err == nil {
		t.Fatalf("approve with failing credit succeeded")
	}
	if after = balances(ann, ben); !slices.Equal(after, before) {
		t.Errorf("failed approve moved points: %v -> %v", before, after)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM quest_completions WHERE id = $1`, cid).Scan(&status); err != nil || status != "pending" {
		t.Errorf("after failed approve: status %q, %v; want pending", status, err)
	}
	if got := storedAwards(cid); got[ann.MemberID] != -1 || got[ben.MemberID] != -1 {
		t.Errorf("failed approve stored awards: %v, want none", got)
	}
}

// TestIntegration_QuestSharingAbandon (CLM-18): abandoning a shared quest
// takes only the acting member off it. When the holder leaves, the holder of
// record passes to the earliest remaining participant; once one member is
// left the quest is no longer shared, and approving it pays that member the
// full points and no teamwork bonus. A household member not on the quest
// gets ErrNotFound and nothing changes; the last member abandoning returns
// the quest to the pool, where anyone can take it.
func TestIntegration_QuestSharingAbandon(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questshareabandon").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Leavers"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	newMember := func(name string) app.MemberAction {
		t.Helper()
		m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: name}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return app.MemberAction{MemberID: m.ID}
	}
	ann, ben, cara, dan := newMember("Ann"), newMember("Ben"), newMember("Cara"), newMember("Dan")
	quest, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Clean the living room", Points: num(100), Shareable: true, TeamworkBonus: num(20)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	// acceptAndShare ends any cooldown, has Ann accept the quest and share it
	// with each of with, in order.
	acceptAndShare := func(with ...app.MemberAction) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NULL WHERE id = $1`, quest.ID); err != nil {
			t.Fatalf("end cooldown: %v", err)
		}
		if _, err := quests.Accept(ctx, admin, h.ID, quest.ID, ann); err != nil {
			t.Fatalf("Ann accepts: %v", err)
		}
		for _, m := range with {
			if _, err := quests.Share(ctx, admin, h.ID, quest.ID, ann, m); err != nil {
				t.Fatalf("share with %v: %v", m.MemberID, err)
			}
		}
	}
	// state is the quest's stored holder and its participants.
	state := func() (*uuid.UUID, []uuid.UUID) {
		t.Helper()
		v, err := quests.Get(ctx, admin, h.ID, quest.ID)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return v.ClaimedBy, v.Participants
	}
	is := func(got *uuid.UUID, want app.MemberAction) bool { return got != nil && *got == want.MemberID }

	// 1. Ann, the holder, leaves a quest shared with Ben: Ben holds it
	//    alone and it is no longer shared.
	acceptAndShare(ben)
	left, err := quests.Abandon(ctx, admin, h.ID, quest.ID, ann)
	if err != nil {
		t.Fatalf("Ann abandons: %v", err)
	}
	if !is(left.ClaimedBy, ben) || left.Shared() || len(left.Participants) != 0 {
		t.Errorf("after Ann leaves: claimed_by=%v participants=%v, want Ben alone, not shared", left.ClaimedBy, left.Participants)
	}
	if holder, parts := state(); !is(holder, ben) || len(parts) != 0 || participantRows(t, pool, quest.ID) != 0 {
		t.Errorf("stored after Ann leaves: claimed_by=%v participants=%v", holder, parts)
	}
	// Neither Ann (no longer on it) nor Dan (never on it) can abandon it,
	// and nothing changes.
	for name, act := range map[string]app.MemberAction{"Ann again": ann, "Dan": dan} {
		if _, err := quests.Abandon(ctx, admin, h.ID, quest.ID, act); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s abandons: %v, want ErrNotFound", name, err)
		}
	}
	if holder, parts := state(); !is(holder, ben) || len(parts) != 0 {
		t.Errorf("refused abandons changed the quest: claimed_by=%v participants=%v", holder, parts)
	}

	// 2. Ben marks it done alone: one member, full points, no bonus.
	if _, err := quests.MarkDone(ctx, admin, h.ID, quest.ID, ben); err != nil {
		t.Fatalf("Ben marks done: %v", err)
	}
	item := pendingFor(t, quests, admin, h.ID, quest.ID)
	if want := []app.PendingMember{{MemberID: ben.MemberID, Name: "Ben", Award: 100}}; !slices.Equal(item.Members, want) {
		t.Errorf("pending after abandon: members %+v, want %+v", item.Members, want)
	}
	annBefore, benBefore := memberPoints(t, q, h.ID, ann.MemberID), memberPoints(t, q, h.ID, ben.MemberID)
	_, _, _, credited, err := quests.Approve(ctx, admin, h.ID, item.ID, "")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(credited) != 1 || credited[0].Member.ID != ben.MemberID || credited[0].PointsAwarded != 100 || credited[0].TeamworkBonus != 0 {
		t.Errorf("approve credited %+v, want Ben alone with 100 and no bonus", credited)
	}
	if got := memberPoints(t, q, h.ID, ben.MemberID) - benBefore; got != 100 {
		t.Errorf("Ben gained %d, want 100", got)
	}
	if got := memberPoints(t, q, h.ID, ann.MemberID); got != annBefore {
		t.Errorf("Ann's points moved %d -> %d after she left", annBefore, got)
	}

	// 3. A participant who is not the holder leaves: Ann keeps holding it and
	//    Cara stays on it; then Cara leaves and Ann is alone (not shared);
	//    then Ann, the last member, leaves and the quest is back in the pool.
	acceptAndShare(ben, cara)
	if _, err := quests.Abandon(ctx, admin, h.ID, quest.ID, ben); err != nil {
		t.Fatalf("Ben abandons: %v", err)
	}
	if holder, parts := state(); !is(holder, ann) || !slices.Equal(parts, []uuid.UUID{ann.MemberID, cara.MemberID}) {
		t.Errorf("after Ben leaves: claimed_by=%v participants=%v, want Ann holding with Ann and Cara", holder, parts)
	}
	if _, err := quests.Abandon(ctx, admin, h.ID, quest.ID, cara); err != nil {
		t.Fatalf("Cara abandons: %v", err)
	}
	if holder, parts := state(); !is(holder, ann) || len(parts) != 0 {
		t.Errorf("after Cara leaves: claimed_by=%v participants=%v, want Ann alone, not shared", holder, parts)
	}
	back, err := quests.Abandon(ctx, admin, h.ID, quest.ID, ann)
	if err != nil {
		t.Fatalf("Ann, the last member, abandons: %v", err)
	}
	if back.ClaimedBy != nil || back.ClaimedAt.Valid || len(back.Participants) != 0 {
		t.Errorf("last abandon: claimed_by=%v participants=%v, want back in the pool", back.ClaimedBy, back.Participants)
	}
	if v, err := quests.Accept(ctx, admin, h.ID, quest.ID, dan); err != nil || !is(v.ClaimedBy, dan) {
		t.Errorf("Dan takes the pooled quest: %+v, %v", v.ClaimedBy, err)
	}
	if _, err := quests.Abandon(ctx, admin, h.ID, quest.ID, dan); err != nil {
		t.Fatalf("Dan abandons: %v", err)
	}

	// 4. The holder leaves a quest shared with two others: the holder of
	//    record passes to the earliest of them, and it stays shared.
	acceptAndShare(ben, cara)
	if _, err := quests.Abandon(ctx, admin, h.ID, quest.ID, ann); err != nil {
		t.Fatalf("Ann abandons three-way quest: %v", err)
	}
	if holder, parts := state(); !is(holder, ben) || !slices.Equal(parts, []uuid.UUID{ben.MemberID, cara.MemberID}) {
		t.Errorf("after the holder leaves three: claimed_by=%v participants=%v, want Ben holding with Ben and Cara", holder, parts)
	}
}
