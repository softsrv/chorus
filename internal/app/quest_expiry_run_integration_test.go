//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_RunQuestExpiry_SweepsAtStartupAndStopsOnCancel: an overdue
// held quest is released by the startup sweep of RunQuestExpiry, without
// waiting for an interval tick, and the function returns promptly once its
// context is cancelled.
func TestIntegration_RunQuestExpiry_SweepsAtStartupAndStopsOnCancel(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questexpiryrun").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Runners", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	member, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Dee"}, "")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	quest, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Sweep porch", Points: num(10)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	if _, err := quests.Accept(ctx, admin, h.ID, quest.ID, app.MemberAction{MemberID: member.ID}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE household_quests SET claim_expires_at = NOW() - INTERVAL '1 minute' WHERE household_id = $1 AND id = $2`,
		h.ID, quest.ID); err != nil {
		t.Fatalf("back-date deadline: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.RunQuestExpiry(runCtx, quests)
	}()

	released := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		got, err := q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: h.ID, ID: quest.ID})
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		if got.ClaimedBy == nil && !got.ClaimExpiresAt.Valid {
			released = true
			break
		}
	}
	if !released {
		t.Fatal("the startup sweep did not release the overdue quest")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunQuestExpiry did not return after its context was cancelled")
	}
}
