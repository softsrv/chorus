//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// pointsAwardRows counts the points history entries of a household.
func pointsAwardRows(t *testing.T, pool *pgxpool.Pool, householdID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM points_awards WHERE household_id = $1`, householdID).Scan(&n); err != nil {
		t.Fatalf("count points awards: %v", err)
	}
	return n
}

// TestIntegration_QuestExpiry_ReleasesOverdueClaim: a held, shared quest
// whose claim_expires_at has passed is released by ExpireOverdueQuests:
// claimed_by, claimed_at, done_at and claim_expires_at become NULL, every
// participant row goes, available_at keeps its value, no completion or
// points history entry is written and no member's points move. The quest is
// then available, and another member's accept stamps a fresh deadline in the
// future.
func TestIntegration_QuestExpiry_ReleasesOverdueClaim(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questexpiry").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Expirers", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	newMember := func(name string) uuid.UUID {
		t.Helper()
		m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: name}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return m.ID
	}
	ann, ben, cara := newMember("Ann"), newMember("Ben"), newMember("Cara")
	annAct, benAct, caraAct := app.MemberAction{MemberID: ann}, app.MemberAction{MemberID: ben}, app.MemberAction{MemberID: cara}

	quest, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Fold laundry", Points: num(40), Shareable: true, TeamworkBonus: num(5)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	held, err := quests.Accept(ctx, admin, h.ID, quest.ID, annAct)
	if err != nil {
		t.Fatalf("Ann accept: %v", err)
	}
	if !held.ClaimExpiresAt.Valid || !held.ClaimExpiresAt.Time.After(time.Now()) {
		t.Fatalf("accepted quest deadline: %+v, want a future instant", held.ClaimExpiresAt)
	}
	if _, err := quests.Share(ctx, admin, h.ID, quest.ID, annAct, benAct); err != nil {
		t.Fatalf("share with Ben: %v", err)
	}

	// The day has ended: the deadline is back-dated, and available_at holds a
	// past cooldown end the sweep must leave as it is.
	pastAvailable := time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx,
		`UPDATE household_quests SET claim_expires_at = NOW() - INTERVAL '1 minute', available_at = $3
		 WHERE household_id = $1 AND id = $2`, h.ID, quest.ID, pastAvailable); err != nil {
		t.Fatalf("back-date deadline: %v", err)
	}

	completionsBefore := completionRows(t, pool, quest.ID)
	awardsBefore := pointsAwardRows(t, pool, h.ID)
	pointsBefore := map[uuid.UUID]int32{}
	for _, id := range []uuid.UUID{ann, ben, cara} {
		pointsBefore[id] = memberPoints(t, q, h.ID, id)
	}

	released, err := quests.ExpireOverdueQuests(ctx)
	if err != nil {
		t.Fatalf("ExpireOverdueQuests: %v", err)
	}
	if released < 1 {
		t.Errorf("released %d quests, want at least 1", released)
	}

	got, err := q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: h.ID, ID: quest.ID})
	if err != nil {
		t.Fatalf("get quest: %v", err)
	}
	if got.ClaimedBy != nil || got.ClaimedAt.Valid || got.DoneAt.Valid || got.ClaimExpiresAt.Valid {
		t.Errorf("after expiry claimed_by=%v claimed_at=%v done_at=%v claim_expires_at=%v, want all NULL",
			got.ClaimedBy, got.ClaimedAt, got.DoneAt, got.ClaimExpiresAt)
	}
	if !got.AvailableAt.Valid || !got.AvailableAt.Time.Equal(pastAvailable) {
		t.Errorf("available_at after expiry: %v, want %v untouched", got.AvailableAt, pastAvailable)
	}
	if !app.QuestAvailable(got, time.Now()) {
		t.Errorf("expired quest is not available: %+v", got)
	}
	participants, err := q.ListQuestParticipants(ctx, db.ListQuestParticipantsParams{HouseholdID: h.ID, QuestID: quest.ID})
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	if len(participants) != 0 {
		t.Errorf("participants after expiry: %v, want none", participants)
	}
	if n := completionRows(t, pool, quest.ID); n != completionsBefore {
		t.Errorf("completions after expiry: %d, want %d", n, completionsBefore)
	}
	if n := pointsAwardRows(t, pool, h.ID); n != awardsBefore {
		t.Errorf("points awards after expiry: %d, want %d", n, awardsBefore)
	}
	for id, before := range pointsBefore {
		if p := memberPoints(t, q, h.ID, id); p != before {
			t.Errorf("member %v points after expiry: %d, want %d", id, p, before)
		}
	}

	// A second sweep finds nothing more to do for this quest.
	if _, err := quests.ExpireOverdueQuests(ctx); err != nil {
		t.Fatalf("second ExpireOverdueQuests: %v", err)
	}

	// Back in the pool: another member accepts it and gets a fresh deadline.
	again, err := quests.Accept(ctx, admin, h.ID, quest.ID, caraAct)
	if err != nil {
		t.Fatalf("Cara accept after expiry: %v", err)
	}
	if again.ClaimedBy == nil || *again.ClaimedBy != cara {
		t.Errorf("holder after re-accept: %v, want %v", again.ClaimedBy, cara)
	}
	if !again.ClaimExpiresAt.Valid || !again.ClaimExpiresAt.Time.After(time.Now()) {
		t.Errorf("re-accepted quest deadline: %+v, want a future instant", again.ClaimExpiresAt)
	}
}
