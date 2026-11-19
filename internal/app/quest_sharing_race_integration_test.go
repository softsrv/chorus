//go:build integration

package app_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_QuestSharingAbandonRace: members leaving a shared quest at
// the same moment as each other, or as the holder shares it, never leave it
// in a state where the holder has no participant row or one member is left
// as a lone participant. After each round the quest has no participant rows
// or two or more, and when it has any, its holder is among them.
func TestIntegration_QuestSharingAbandonRace(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 4)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questsharerace").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Racers"})
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
	// reset puts the quest back in the pool, has Ann accept it and share it
	// with each of with.
	reset := func(with ...app.MemberAction) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM household_quest_participants WHERE quest_id = $1`, quest.ID); err != nil {
			t.Fatalf("clear participants: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET claimed_by = NULL, claimed_at = NULL, done_at = NULL, available_at = NULL WHERE id = $1`, quest.ID); err != nil {
			t.Fatalf("free quest: %v", err)
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
	consistent := func(round string) {
		t.Helper()
		v, err := quests.Get(ctx, admin, h.ID, quest.ID)
		if err != nil {
			t.Fatalf("%s: get quest: %v", round, err)
		}
		n := participantRows(t, pool, quest.ID)
		if n == 1 {
			t.Fatalf("%s: one lone participant row %v, holder %v", round, v.Participants, v.ClaimedBy)
		}
		if n >= 2 && (v.ClaimedBy == nil || !slices.Contains(v.Participants, *v.ClaimedBy)) {
			t.Fatalf("%s: holder %v not among participants %v", round, v.ClaimedBy, v.Participants)
		}
	}
	run := func(fns ...func() error) []error {
		errs := make([]error, len(fns))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, fn := range fns {
			wg.Add(1)
			go func(i int, fn func() error) {
				defer wg.Done()
				<-start
				errs[i] = fn()
			}(i, fn)
		}
		close(start)
		wg.Wait()
		return errs
	}
	abandon := func(m app.MemberAction) func() error {
		return func() error { _, err := quests.Abandon(ctx, admin, h.ID, quest.ID, m); return err }
	}
	share := func(m app.MemberAction) func() error {
		return func() error { _, err := quests.Share(ctx, admin, h.ID, quest.ID, ann, m); return err }
	}

	for i := range 20 {
		// Ben leaves while Ann shares with Cara: the finding's case.
		reset(ben)
		for _, err := range run(abandon(ben), share(cara)) {
			if err != nil {
				t.Fatalf("round %d abandon/share: %v", i, err)
			}
		}
		consistent(fmt.Sprintf("round %d abandon/share", i))
		if v, _ := quests.Get(ctx, admin, h.ID, quest.ID); len(v.Participants) != 2 {
			t.Fatalf("round %d abandon/share: participants %v, want Ann and Cara", i, v.Participants)
		}

		// Two of three members leave at once: one is left, not shared.
		reset(ben, cara)
		for _, err := range run(abandon(ben), abandon(cara)) {
			if err != nil {
				t.Fatalf("round %d abandon/abandon: %v", i, err)
			}
		}
		consistent(fmt.Sprintf("round %d abandon/abandon", i))
		if v, _ := quests.Get(ctx, admin, h.ID, quest.ID); v.ClaimedBy == nil || *v.ClaimedBy != ann.MemberID || v.Shared() {
			t.Fatalf("round %d abandon/abandon: holder %v participants %v, want Ann alone", i, v.ClaimedBy, v.Participants)
		}

		// The holder leaves while another member leaves too.
		reset(ben, cara, dan)
		for _, err := range run(abandon(ann), abandon(dan)) {
			if err != nil {
				t.Fatalf("round %d holder abandon/abandon: %v", i, err)
			}
		}
		consistent(fmt.Sprintf("round %d holder abandon/abandon", i))
	}
}
