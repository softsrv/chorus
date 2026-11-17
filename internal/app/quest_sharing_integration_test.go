//go:build integration

package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// makeShareable marks a quest shareable directly in the database, the state
// an administrator's edit leaves it in.
func makeShareable(t *testing.T, pool *pgxpool.Pool, questID uuid.UUID, bonus int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE household_quests SET shareable = true, teamwork_bonus = $2 WHERE id = $1`, questID, bonus); err != nil {
		t.Fatalf("make quest shareable: %v", err)
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

	shared, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Clean the living room", Points: num(100)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	makeShareable(t, pool, shared.ID, 20)
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
}
