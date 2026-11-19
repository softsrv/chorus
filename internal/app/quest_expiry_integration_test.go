//go:build integration

package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// rowJSON is one row, or the rows of one query, as JSON text, so that two
// snapshots differ when any column of any row does.
func rowJSON(t *testing.T, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&out); err != nil {
		t.Fatalf("snapshot %q: %v", query, err)
	}
	return out
}

// TestIntegration_QuestExpiry_MarkDone: a quest marked done before its
// deadline completes as before (one pending completion, cooldown started,
// holder and deadline cleared) and a later sweep leaves it and the
// completion untouched. A quest whose deadline has passed, with no sweep yet,
// is refused as for a member not holding it (ErrNotFound): no completion, no
// streak step, the quest row as it was.
func TestIntegration_QuestExpiry_MarkDone(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questexpirydone").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Finishers", Timezone: "America/Chicago"})
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
	ann, ben := newMember("Ann"), newMember("Ben")
	newQuest := func(name string) uuid.UUID {
		t.Helper()
		v, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: name, Points: num(10), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return v.ID
	}
	onTime, late := newQuest("On time"), newQuest("Too late")
	getQuest := func(id uuid.UUID) db.HouseholdQuest {
		t.Helper()
		got, err := q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: h.ID, ID: id})
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return got
	}

	// Marked done before the deadline.
	if _, err := quests.Accept(ctx, admin, h.ID, onTime, ann); err != nil {
		t.Fatalf("Ann accept: %v", err)
	}
	done, err := quests.MarkDone(ctx, admin, h.ID, onTime, ann)
	if err != nil {
		t.Fatalf("MarkDone before deadline: %v", err)
	}
	if done.ClaimedBy != nil || done.ClaimExpiresAt.Valid {
		t.Errorf("done quest holder=%v claim_expires_at=%v, want both cleared", done.ClaimedBy, done.ClaimExpiresAt)
	}
	if !done.AvailableAt.Valid || !done.AvailableAt.Time.After(time.Now()) {
		t.Errorf("done quest available_at: %v, want a future cooldown end", done.AvailableAt)
	}
	if n := completionRows(t, pool, onTime); n != 1 {
		t.Fatalf("completions after on-time mark done: %d, want 1", n)
	}
	questBefore := rowJSON(t, pool, `SELECT to_jsonb(q)::text FROM household_quests q WHERE id = $1`, onTime)
	completionsBefore := rowJSON(t, pool,
		`SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id), '[]')::text FROM quest_completions c WHERE quest_id = $1`, onTime)
	if _, err := quests.ExpireOverdueQuests(ctx); err != nil {
		t.Fatalf("ExpireOverdueQuests: %v", err)
	}
	if got := rowJSON(t, pool, `SELECT to_jsonb(q)::text FROM household_quests q WHERE id = $1`, onTime); got != questBefore {
		t.Errorf("sweep changed the done quest:\n got %s\nwant %s", got, questBefore)
	}
	if got := rowJSON(t, pool,
		`SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id), '[]')::text FROM quest_completions c WHERE quest_id = $1`, onTime); got != completionsBefore {
		t.Errorf("sweep changed the completion:\n got %s\nwant %s", got, completionsBefore)
	}

	// Deadline passed, no sweep yet: refused like a member not holding it.
	if _, err := quests.Accept(ctx, admin, h.ID, late, ann); err != nil {
		t.Fatalf("Ann accept late: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE household_quests SET claim_expires_at = NOW() - INTERVAL '1 minute' WHERE household_id = $1 AND id = $2`,
		h.ID, late); err != nil {
		t.Fatalf("back-date deadline: %v", err)
	}
	lateBefore := getQuest(late)
	_, nonHolderErr := quests.MarkDone(ctx, admin, h.ID, late, ben)
	if !errors.Is(nonHolderErr, app.ErrNotFound) {
		t.Fatalf("MarkDone by a member not on it: %v, want ErrNotFound", nonHolderErr)
	}
	_, err = quests.MarkDone(ctx, admin, h.ID, late, ann)
	if !errors.Is(err, app.ErrNotFound) || err.Error() != nonHolderErr.Error() {
		t.Errorf("MarkDone after the day ended: %v, want the error of a non-holder (%v)", err, nonHolderErr)
	}
	if n := completionRows(t, pool, late); n != 0 {
		t.Errorf("completions after refused mark done: %d, want 0", n)
	}
	if _, err := q.GetMemberQuestStreak(ctx, db.GetMemberQuestStreakParams{HouseholdID: h.ID, MemberID: ann.MemberID, QuestID: late}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("streak after refused mark done: %v, want no row", err)
	}
	if after := getQuest(late); !reflect.DeepEqual(after, lateBefore) {
		t.Errorf("refused mark done changed the quest:\n got %+v\nwant %+v", after, lateBefore)
	}
}

// TestIntegration_QuestExpiry_SweepScope: a sweep that releases an overdue
// quest leaves everything else byte for byte as it was: a quest resting on
// its cooldown with a pending completion, a quest nobody holds, and a
// household with nothing held; the expired quest's available_at is not
// modified.
func TestIntegration_QuestExpiry_SweepScope(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questexpiryscope").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Busy", Timezone: "Pacific/Auckland"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	empty, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Quiet", Timezone: "Europe/London"})
	if err != nil {
		t.Fatalf("create empty household: %v", err)
	}
	var members []app.MemberAction
	for _, name := range []string{"Ann", "Ben"} {
		m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: name}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		members = append(members, app.MemberAction{MemberID: m.ID})
	}
	ann, ben := members[0], members[1]
	newQuest := func(name string) uuid.UUID {
		t.Helper()
		v, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: name, Points: num(10), RepeatDays: num(3)}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return v.ID
	}
	resting, unheld, overdue := newQuest("Resting"), newQuest("Unheld"), newQuest("Overdue")

	if _, err := quests.Accept(ctx, admin, h.ID, resting, ann); err != nil {
		t.Fatalf("accept resting: %v", err)
	}
	if _, err := quests.MarkDone(ctx, admin, h.ID, resting, ann); err != nil {
		t.Fatalf("mark resting done: %v", err)
	}
	if _, err := quests.Accept(ctx, admin, h.ID, overdue, ben); err != nil {
		t.Fatalf("accept overdue: %v", err)
	}
	// A cooldown end the sweep must not touch, on the expired quest too.
	overdueAvailable := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx,
		`UPDATE household_quests SET claim_expires_at = NOW() - INTERVAL '1 minute', available_at = $3
		 WHERE household_id = $1 AND id = $2`, h.ID, overdue, overdueAvailable); err != nil {
		t.Fatalf("back-date deadline: %v", err)
	}

	const questSnap = `SELECT to_jsonb(q)::text FROM household_quests q WHERE id = $1`
	const completionSnap = `SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id), '[]')::text FROM quest_completions c WHERE household_id = $1`
	const householdQuestsSnap = `SELECT coalesce(jsonb_agg(to_jsonb(q) ORDER BY q.id), '[]')::text FROM household_quests q WHERE household_id = $1`
	const streakSnap = `SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY s.member_id, s.quest_id), '[]')::text FROM member_quest_streaks s WHERE household_id = $1`
	snap := func() map[string]string {
		return map[string]string{
			"resting quest":       rowJSON(t, pool, questSnap, resting),
			"unheld quest":        rowJSON(t, pool, questSnap, unheld),
			"completions":         rowJSON(t, pool, completionSnap, h.ID),
			"streaks":             rowJSON(t, pool, streakSnap, h.ID),
			"empty household":     rowJSON(t, pool, householdQuestsSnap, empty.ID),
			"empty completions":   rowJSON(t, pool, completionSnap, empty.ID),
			"empty household row": rowJSON(t, pool, `SELECT to_jsonb(h)::text FROM households h WHERE id = $1`, empty.ID),
		}
	}
	before := snap()

	if _, err := quests.ExpireOverdueQuests(ctx); err != nil {
		t.Fatalf("ExpireOverdueQuests: %v", err)
	}

	for name, was := range before {
		if got := snap()[name]; got != was {
			t.Errorf("sweep changed %s:\n got %s\nwant %s", name, got, was)
		}
	}
	got, err := q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: h.ID, ID: overdue})
	if err != nil {
		t.Fatalf("get overdue quest: %v", err)
	}
	if got.ClaimedBy != nil || got.ClaimExpiresAt.Valid {
		t.Errorf("overdue quest still held: claimed_by=%v claim_expires_at=%v", got.ClaimedBy, got.ClaimExpiresAt)
	}
	if !got.AvailableAt.Valid || !got.AvailableAt.Time.Equal(overdueAvailable) {
		t.Errorf("expired quest available_at: %v, want %v unchanged", got.AvailableAt, overdueAvailable)
	}
	if n := completionRows(t, pool, overdue); n != 0 {
		t.Errorf("completions of the expired quest: %d, want 0", n)
	}
}
