//go:build integration

package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
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

// expiryWorld is one household with members and shareable quests for the
// expiry tests below.
type expiryWorld struct {
	t      *testing.T
	pool   *pgxpool.Pool
	q      *db.Queries
	quests *app.QuestService
	ctx    context.Context
	admin  uuid.UUID
	h      uuid.UUID
}

func newExpiryWorld(t *testing.T, user, name string) *expiryWorld {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	ctx := context.Background()
	admin := registerVerified(t, pool, authSvc, mailer, user).User.ID
	h, err := app.NewHouseholdService(q, pool).Create(ctx, admin, app.HouseholdInput{Name: name, Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	return &expiryWorld{t: t, pool: pool, q: q, quests: app.NewQuestService(q, pool), ctx: ctx, admin: admin, h: h.ID}
}

func (w *expiryWorld) member(name string) app.MemberAction {
	w.t.Helper()
	m, err := app.NewFamilyService(w.q, w.pool, 10).Create(w.ctx, w.admin, w.h, app.FamilyMemberInput{Name: name}, "")
	if err != nil {
		w.t.Fatalf("create %s: %v", name, err)
	}
	return app.MemberAction{MemberID: m.ID}
}

func (w *expiryWorld) quest(in app.QuestInput) uuid.UUID {
	w.t.Helper()
	v, err := w.quests.Create(w.ctx, w.admin, w.h, in, "")
	if err != nil {
		w.t.Fatalf("create %s: %v", in.Name, err)
	}
	return v.ID
}

// backDate puts a quest's claim deadline a minute in the past.
func (w *expiryWorld) backDate(questID uuid.UUID) {
	w.t.Helper()
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE household_quests SET claim_expires_at = NOW() - INTERVAL '1 minute' WHERE household_id = $1 AND id = $2`,
		w.h, questID); err != nil {
		w.t.Fatalf("back-date deadline: %v", err)
	}
}

// snapshot is the quest row and its participant rows as JSON text.
func (w *expiryWorld) snapshot(questID uuid.UUID) string {
	w.t.Helper()
	return rowJSON(w.t, w.pool, `SELECT to_jsonb(q)::text FROM household_quests q WHERE id = $1`, questID) +
		rowJSON(w.t, w.pool, `SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.member_id), '[]')::text
			FROM household_quest_participants p WHERE quest_id = $1`, questID)
}

func (w *expiryWorld) get(questID uuid.UUID) db.HouseholdQuest {
	w.t.Helper()
	got, err := w.q.GetHouseholdQuest(w.ctx, db.GetHouseholdQuestParams{HouseholdID: w.h, ID: questID})
	if err != nil {
		w.t.Fatalf("get quest: %v", err)
	}
	return got
}

// TestIntegration_QuestExpiry_Share: sharing a quest whose claim day has
// ended, before the sweep has released it, is refused as for a quest the
// member does not hold (ErrNotFound) whoever the target is: a new member, a
// member already on the quest, the actor themselves, or a member outside the
// quest's age range. Each refusal writes no participant row and leaves the
// quest as it was. The same share before the deadline succeeds.
func TestIntegration_QuestExpiry_Share(t *testing.T) {
	w := newExpiryWorld(t, "questexpiryshare", "Late sharers")
	ann, ben, cara, tot := w.member("Ann"), w.member("Ben"), w.member("Cara"), w.member("Tot")
	if _, err := w.pool.Exec(w.ctx, `UPDATE family_members SET birthday = CURRENT_DATE - INTERVAL '2 years' WHERE id = $1`, tot.MemberID); err != nil {
		t.Fatalf("set Tot's birthday: %v", err)
	}
	solo := w.quest(app.QuestInput{Name: "Wash the car", Points: num(30), Shareable: true, MinAge: num(6)})
	shared := w.quest(app.QuestInput{Name: "Rake leaves", Points: num(30), Shareable: true, MinAge: num(6)})

	if _, err := w.quests.Accept(w.ctx, w.admin, w.h, solo, ann); err != nil {
		t.Fatalf("Ann accepts solo: %v", err)
	}
	if _, err := w.quests.Accept(w.ctx, w.admin, w.h, shared, ann); err != nil {
		t.Fatalf("Ann accepts shared: %v", err)
	}
	// Before the deadline: Tot is refused for age, and Ben is added, so the
	// refusals below come from the expiry, not from these cases.
	if _, err := w.quests.Share(w.ctx, w.admin, w.h, solo, ann, tot); !errors.Is(err, app.ErrAgeRestricted) {
		t.Fatalf("share with Tot before deadline: %v, want ErrAgeRestricted", err)
	}
	if _, err := w.quests.Share(w.ctx, w.admin, w.h, shared, ann, ben); err != nil {
		t.Fatalf("share before deadline: %v", err)
	}
	w.backDate(solo)
	w.backDate(shared)

	cases := []struct {
		name          string
		quest         uuid.UUID
		target        app.MemberAction
		wouldBeOnTime error
	}{
		{"a new member", solo, cara, nil},
		{"the actor themselves", solo, ann, app.ErrAlreadyParticipant},
		{"a member outside the age range", solo, tot, app.ErrAgeRestricted},
		{"a member already on it", shared, ben, app.ErrAlreadyParticipant},
		{"a new member of a shared quest", shared, cara, nil},
	}
	for _, c := range cases {
		before := w.snapshot(c.quest)
		_, err := w.quests.Share(w.ctx, w.admin, w.h, c.quest, ann, c.target)
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("share expired quest with %s: %v, want ErrNotFound", c.name, err)
		}
		if c.wouldBeOnTime != nil && errors.Is(err, c.wouldBeOnTime) {
			t.Errorf("share expired quest with %s fell through to %v", c.name, c.wouldBeOnTime)
		}
		if after := w.snapshot(c.quest); after != before {
			t.Errorf("refused share with %s changed the quest:\n got %s\nwant %s", c.name, after, before)
		}
	}
	if n := participantRows(t, w.pool, solo); n != 0 {
		t.Errorf("participant rows of the expired solo quest: %d, want 0", n)
	}
}

// failQuestRelease installs a trigger that makes any update clearing the
// holder of questID raise, so ExpireQuestClaim fails after its participant
// delete has run in the same statement. The returned func drops it.
func failQuestRelease(t *testing.T, pool *pgxpool.Pool, questID uuid.UUID) func() {
	t.Helper()
	fn := "test_fail_release_" + strings.ReplaceAll(questID.String(), "-", "")
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id = '%[2]s' AND NEW.claimed_by IS NULL THEN RAISE EXCEPTION 'injected release failure'; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER %[1]s BEFORE UPDATE ON household_quests FOR EACH ROW EXECUTE FUNCTION %[1]s();`, fn, questID)); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON household_quests; DROP FUNCTION IF EXISTS %[1]s()`, fn)); err != nil {
			t.Errorf("drop trigger: %v", err)
		}
	}
	t.Cleanup(drop)
	return drop
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog routes the default slog logger to a buffer until the test ends.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestIntegration_QuestExpiry_FailedRelease: when releasing one overdue
// shared quest fails after its participant rows were deleted in the same
// statement, that quest's transaction rolls back whole: it keeps its holder,
// claimed_at, deadline and participants. The failure is logged with
// slog.Error naming the quest, another overdue quest in the same run is
// still released, and once the fault is gone the next run releases the
// first. A run with a cancelled context changes nothing and returns its
// error.
func TestIntegration_QuestExpiry_FailedRelease(t *testing.T) {
	w := newExpiryWorld(t, "questexpiryfault", "Faulty")
	ann, ben, cara := w.member("Ann"), w.member("Ben"), w.member("Cara")
	stuck := w.quest(app.QuestInput{Name: "Sweep the porch", Points: num(20), Shareable: true})
	other := w.quest(app.QuestInput{Name: "Water plants", Points: num(5)})

	if _, err := w.quests.Accept(w.ctx, w.admin, w.h, stuck, ann); err != nil {
		t.Fatalf("Ann accepts: %v", err)
	}
	for _, m := range []app.MemberAction{ben, cara} {
		if _, err := w.quests.Share(w.ctx, w.admin, w.h, stuck, ann, m); err != nil {
			t.Fatalf("share: %v", err)
		}
	}
	if _, err := w.quests.Accept(w.ctx, w.admin, w.h, other, ben); err != nil {
		t.Fatalf("Ben accepts other: %v", err)
	}
	w.backDate(stuck)
	w.backDate(other)
	stuckBefore := w.snapshot(stuck)
	if n := participantRows(t, w.pool, stuck); n != 3 {
		t.Fatalf("participants before: %d, want 3", n)
	}

	// A cancelled context stops the run before it writes anything.
	cancelled, cancel := context.WithCancel(w.ctx)
	cancel()
	if _, err := w.quests.ExpireOverdueQuests(cancelled); err == nil {
		t.Errorf("ExpireOverdueQuests with a cancelled context: nil error, want one")
	}
	if got := w.snapshot(stuck); got != stuckBefore {
		t.Errorf("cancelled run changed the quest:\n got %s\nwant %s", got, stuckBefore)
	}
	if w.get(other).ClaimedBy == nil {
		t.Errorf("cancelled run released the other quest")
	}

	logs := captureSlog(t)
	drop := failQuestRelease(t, w.pool, stuck)
	if _, err := w.quests.ExpireOverdueQuests(w.ctx); err != nil {
		t.Fatalf("ExpireOverdueQuests with one failing quest: %v, want the run to go on", err)
	}
	if got := w.snapshot(stuck); got != stuckBefore {
		t.Errorf("failed release left the quest half released:\n got %s\nwant %s", got, stuckBefore)
	}
	if got := w.get(other); got.ClaimedBy != nil || got.ClaimExpiresAt.Valid {
		t.Errorf("other quest in the same run: claimed_by=%v claim_expires_at=%v, want released", got.ClaimedBy, got.ClaimExpiresAt)
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, stuck.String()) || !strings.Contains(out, "injected release failure") {
		t.Errorf("failure not logged as an error naming the quest; log:\n%s", out)
	}

	drop()
	if _, err := w.quests.ExpireOverdueQuests(w.ctx); err != nil {
		t.Fatalf("ExpireOverdueQuests after the fault: %v", err)
	}
	if got := w.get(stuck); got.ClaimedBy != nil || got.ClaimedAt.Valid || got.ClaimExpiresAt.Valid {
		t.Errorf("later run: claimed_by=%v claimed_at=%v claim_expires_at=%v, want released", got.ClaimedBy, got.ClaimedAt, got.ClaimExpiresAt)
	}
	if n := participantRows(t, w.pool, stuck); n != 0 {
		t.Errorf("participants after the later run: %d, want 0", n)
	}
	if n := completionRows(t, w.pool, stuck); n != 0 {
		t.Errorf("completions of the expired quest: %d, want 0", n)
	}
}

// TestIntegration_QuestExpiry_Race: the sweep racing a participant's abandon
// and the holder's share of the same overdue shared quest never leaves a
// half state. The share is refused (ErrNotFound: the day has ended), the
// abandon either goes first or finds the quest gone (ErrNotFound), and after
// the sweep the quest is back in the pool with no participant rows and no
// completion.
func TestIntegration_QuestExpiry_Race(t *testing.T) {
	w := newExpiryWorld(t, "questexpiryrace", "Racing the clock")
	ann, ben, cara, dan := w.member("Ann"), w.member("Ben"), w.member("Cara"), w.member("Dan")
	quest := w.quest(app.QuestInput{Name: "Tidy the garage", Points: num(60), Shareable: true, TeamworkBonus: num(10)})

	for round := range 15 {
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, quest, ann); err != nil {
			t.Fatalf("round %d: Ann accepts: %v", round, err)
		}
		for _, m := range []app.MemberAction{ben, cara} {
			if _, err := w.quests.Share(w.ctx, w.admin, w.h, quest, ann, m); err != nil {
				t.Fatalf("round %d: share: %v", round, err)
			}
		}
		w.backDate(quest)

		var wg sync.WaitGroup
		var sweepErr, abandonErr, shareErr error
		start := make(chan struct{})
		wg.Add(3)
		go func() { defer wg.Done(); <-start; _, sweepErr = w.quests.ExpireOverdueQuests(w.ctx) }()
		go func() { defer wg.Done(); <-start; _, abandonErr = w.quests.Abandon(w.ctx, w.admin, w.h, quest, ben) }()
		go func() { defer wg.Done(); <-start; _, shareErr = w.quests.Share(w.ctx, w.admin, w.h, quest, ann, dan) }()
		close(start)
		wg.Wait()

		if sweepErr != nil {
			t.Fatalf("round %d: sweep: %v", round, sweepErr)
		}
		if abandonErr != nil && !errors.Is(abandonErr, app.ErrNotFound) {
			t.Errorf("round %d: abandon: %v, want nil or ErrNotFound", round, abandonErr)
		}
		if !errors.Is(shareErr, app.ErrNotFound) {
			t.Errorf("round %d: share of an overdue quest: %v, want ErrNotFound", round, shareErr)
		}
		got := w.get(quest)
		if got.ClaimedBy != nil || got.ClaimedAt.Valid || got.ClaimExpiresAt.Valid {
			t.Errorf("round %d: claimed_by=%v claimed_at=%v claim_expires_at=%v, want back in the pool",
				round, got.ClaimedBy, got.ClaimedAt, got.ClaimExpiresAt)
		}
		if n := participantRows(t, w.pool, quest); n != 0 {
			t.Errorf("round %d: participant rows %d, want 0", round, n)
		}
		if n := completionRows(t, w.pool, quest); n != 0 {
			t.Errorf("round %d: completions %d, want 0", round, n)
		}
		if t.Failed() {
			return
		}
	}
}
