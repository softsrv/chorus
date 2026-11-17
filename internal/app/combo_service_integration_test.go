//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

func TestIntegration_ComboBonus(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	combos := app.NewComboService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "comboadmin").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Combo Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	// 1. Empty list returns non-nil empty slice
	empty, err := combos.List(ctx, alice, h.ID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list: got %+v, err: %v (want non-nil empty slice)", empty, err)
	}

	// Create test quests
	q1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Chore 1", Points: num(10)}, "")
	if err != nil {
		t.Fatalf("create q1: %v", err)
	}
	q2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Chore 2", Points: num(15)}, "")
	if err != nil {
		t.Fatalf("create q2: %v", err)
	}
	q3, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Chore 3", Points: num(20)}, "")
	if err != nil {
		t.Fatalf("create q3: %v", err)
	}

	// 2. Validation (CLM-11)
	// Empty name + negative bonus + one chore -> all three field errors returned at once
	_, err = combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "   ",
		BonusPoints: num(-5),
		QuestIDs:    []uuid.UUID{q1.ID},
	}, "")
	var ve *app.ValidationError
	if !errors.As(err, &ve) || ve.Fields["name"] == "" || ve.Fields["bonus_points"] == "" || ve.Fields["quest_ids"] == "" {
		t.Fatalf("expected validation error with name, bonus_points, and quest_ids, got: %v", err)
	}

	// Duplicate-id pair that is really one chore is refused
	_, err = combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "Duplicate Quests",
		BonusPoints: num(10),
		QuestIDs:    []uuid.UUID{q1.ID, q1.ID},
	}, "")
	if !errors.As(err, &ve) || ve.Fields["quest_ids"] == "" {
		t.Fatalf("expected duplicate quest ids refused with quest_ids error, got: %v", err)
	}

	// Oversize name (>200 chars) is refused
	_, err = combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        strings.Repeat("a", 201),
		BonusPoints: num(10),
		QuestIDs:    []uuid.UUID{q1.ID, q2.ID},
	}, "")
	if !errors.As(err, &ve) || ve.Fields["name"] == "" {
		t.Fatalf("expected oversize name error, got: %v", err)
	}

	// List unchanged after rejected validations
	afterBad, err := combos.List(ctx, alice, h.ID)
	if err != nil || len(afterBad) != 0 {
		t.Fatalf("list after bad creates: got %+v (want empty)", afterBad)
	}

	// 3. Create (CLM-1)
	created, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "Morning Routine",
		BonusPoints: num(25),
		QuestIDs:    []uuid.UUID{q1.ID, q2.ID},
	}, "")
	if err != nil {
		t.Fatalf("create combo: %v", err)
	}
	if created.Name != "Morning Routine" || created.BonusPoints != 25 || len(created.QuestIDs) != 2 {
		t.Fatalf("unexpected created combo: %+v", created)
	}

	// 4. Get (CLM-1)
	got, err := combos.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get combo: %v", err)
	}
	if got.ID != created.ID || got.Name != "Morning Routine" || got.BonusPoints != 25 || len(got.QuestIDs) != 2 {
		t.Fatalf("unexpected get combo: %+v", got)
	}

	// 5. List (CLM-1)
	list, err := combos.List(ctx, alice, h.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list combos: got %d items, err: %v", len(list), err)
	}
	if list[0].ID != created.ID || len(list[0].QuestIDs) != 2 {
		t.Fatalf("unexpected combo in list: %+v", list[0])
	}

	// 6. PUT-replace (Update) (CLM-1)
	// Replace with different chores (q2, q3), new name, new bonus; old chore q1 gone
	updated, err := combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name:        "Evening Routine",
		BonusPoints: num(35),
		QuestIDs:    []uuid.UUID{q2.ID, q3.ID},
	}, "")
	if err != nil {
		t.Fatalf("update combo: %v", err)
	}
	if updated.Name != "Evening Routine" || updated.BonusPoints != 35 || len(updated.QuestIDs) != 2 {
		t.Fatalf("unexpected updated combo: %+v", updated)
	}
	for _, id := range updated.QuestIDs {
		if id == q1.ID {
			t.Fatalf("old chore q1 still present after update")
		}
	}

	// Check Get reflects the update
	gotUpdated, err := combos.Get(ctx, alice, h.ID, created.ID)
	if err != nil {
		t.Fatalf("get updated combo: %v", err)
	}
	if gotUpdated.Name != "Evening Routine" || gotUpdated.BonusPoints != 35 {
		t.Fatalf("unexpected get after update: %+v", gotUpdated)
	}

	// 7. Admin PIN gating (CLM-9)
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "4321", ""); err != nil {
		t.Fatalf("set admin pin: %v", err)
	}
	// Non-admin user refused
	otherUser := registerVerified(t, pool, authSvc, mailer, "otheruser").User.ID
	if _, err := combos.Create(ctx, otherUser, h.ID, app.ComboInput{
		Name: "Other", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "4321"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("non-admin create: expected ErrNotFound, got %v", err)
	}
	if _, err := combos.Update(ctx, otherUser, h.ID, created.ID, app.ComboInput{
		Name: "Other", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "4321"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("non-admin update: expected ErrNotFound, got %v", err)
	}
	if err := combos.Delete(ctx, otherUser, h.ID, created.ID, "4321"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("non-admin delete: expected ErrNotFound, got %v", err)
	}

	// Missing PIN refused
	if _, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name: "Pin Missing", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Fatalf("missing pin create: expected ErrAdminPinRequired, got %v", err)
	}
	if _, err := combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name: "Pin Missing", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Fatalf("missing pin update: expected ErrAdminPinRequired, got %v", err)
	}
	if err := combos.Delete(ctx, alice, h.ID, created.ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Fatalf("missing pin delete: expected ErrAdminPinRequired, got %v", err)
	}

	// Wrong PIN refused
	if _, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name: "Pin Wrong", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "9999"); !errors.Is(err, app.ErrWrongPIN) {
		t.Fatalf("wrong pin create: expected ErrWrongPIN, got %v", err)
	}
	if _, err := combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name: "Pin Wrong", BonusPoints: num(10), QuestIDs: []uuid.UUID{q1.ID, q2.ID},
	}, "9999"); !errors.Is(err, app.ErrWrongPIN) {
		t.Fatalf("wrong pin update: expected ErrWrongPIN, got %v", err)
	}
	if err := combos.Delete(ctx, alice, h.ID, created.ID, "9999"); !errors.Is(err, app.ErrWrongPIN) {
		t.Fatalf("wrong pin delete: expected ErrWrongPIN, got %v", err)
	}

	// Verify combo is unchanged after all failed operations
	checkUnchanged, err := combos.Get(ctx, alice, h.ID, created.ID)
	if err != nil || checkUnchanged.Name != "Evening Routine" || checkUnchanged.BonusPoints != 35 {
		t.Fatalf("combo changed after failed pin calls: %+v", checkUnchanged)
	}

	// 8. Awards preserved on edit and delete (CLM-8)
	member, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Kid"}, "4321")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	initialPoints := member.Points

	awardID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new uuid: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO combo_awards (id, household_id, combo_id, member_id, combo_name, points, award_day, awarded_at)
		VALUES ($1, $2, $3, $4, $5, $6, CURRENT_DATE, NOW())
	`, awardID, h.ID, created.ID, member.ID, "Evening Routine Snapshot", 50)
	if err != nil {
		t.Fatalf("insert combo award: %v", err)
	}

	// Update the combo (change name and points again with valid PIN)
	_, err = combos.Update(ctx, alice, h.ID, created.ID, app.ComboInput{
		Name:        "Night Routine",
		BonusPoints: num(100),
		QuestIDs:    []uuid.UUID{q1.ID, q3.ID},
	}, "4321")
	if err != nil {
		t.Fatalf("update with pin: %v", err)
	}

	// Verify award row is untouched after update
	var awardName string
	var awardPoints int32
	var awardComboID *uuid.UUID
	err = pool.QueryRow(ctx, `SELECT combo_name, points, combo_id FROM combo_awards WHERE id = $1`, awardID).
		Scan(&awardName, &awardPoints, &awardComboID)
	if err != nil {
		t.Fatalf("query award after update: %v", err)
	}
	if awardName != "Evening Routine Snapshot" || awardPoints != 50 || awardComboID == nil || *awardComboID != created.ID {
		t.Fatalf("award changed after combo update: name=%s, points=%d, combo_id=%v", awardName, awardPoints, awardComboID)
	}

	// Delete the combo with valid PIN
	if err := combos.Delete(ctx, alice, h.ID, created.ID, "4321"); err != nil {
		t.Fatalf("delete combo with pin: %v", err)
	}

	// Get returns ErrNotFound
	if _, err := combos.Get(ctx, alice, h.ID, created.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("get deleted combo: expected ErrNotFound, got %v", err)
	}

	// Verify award row still exists, original snapshot preserved, combo_id is now NULL
	err = pool.QueryRow(ctx, `SELECT combo_name, points, combo_id FROM combo_awards WHERE id = $1`, awardID).
		Scan(&awardName, &awardPoints, &awardComboID)
	if err != nil {
		t.Fatalf("query award after delete: %v", err)
	}
	if awardName != "Evening Routine Snapshot" || awardPoints != 50 || awardComboID != nil {
		t.Fatalf("award after combo delete: name=%s, points=%d, combo_id=%v (want nil)", awardName, awardPoints, awardComboID)
	}

	// Verify member points are unchanged
	m, err := familySvc.Get(ctx, alice, h.ID, member.ID)
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if m.Points != initialPoints {
		t.Fatalf("member points changed: got %d, want %d", m.Points, initialPoints)
	}
}

// comboWorld is one household (America/New_York, UTC-5 in January) with an
// admin, used by the bonus-payment tests. Times below are written in UTC with
// their New York wall-clock time beside them.
type comboWorld struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *db.Queries
	quests *app.QuestService
	family *app.FamilyService
	combos *app.ComboService
	admin  uuid.UUID
	h      app.Household
	// last maps a chore to the completion its latest markDone recorded.
	last map[uuid.UUID]uuid.UUID
}

func newComboWorld(t *testing.T, w *comboWorld, name string) *comboWorld {
	t.Helper()
	h, err := app.NewHouseholdService(w.q, w.pool).Create(w.ctx, w.admin, app.HouseholdInput{Name: name, Timezone: "America/New_York"})
	if err != nil {
		t.Fatalf("create household %s: %v", name, err)
	}
	c := *w
	c.t = t
	c.h = h
	c.last = map[uuid.UUID]uuid.UUID{}
	return &c
}

func (w *comboWorld) member(name string) uuid.UUID {
	w.t.Helper()
	m, err := w.family.Create(w.ctx, w.admin, w.h.ID, app.FamilyMemberInput{Name: name}, "")
	if err != nil {
		w.t.Fatalf("create member %s: %v", name, err)
	}
	return m.ID
}

func (w *comboWorld) chore(name string, points int64) uuid.UUID {
	w.t.Helper()
	c, err := w.quests.Create(w.ctx, w.admin, w.h.ID, app.QuestInput{Name: name, Points: num(points)}, "")
	if err != nil {
		w.t.Fatalf("create chore %s: %v", name, err)
	}
	return c.ID
}

func (w *comboWorld) combo(name string, bonus int64, ids ...uuid.UUID) uuid.UUID {
	w.t.Helper()
	c, err := w.combos.Create(w.ctx, w.admin, w.h.ID, app.ComboInput{Name: name, BonusPoints: num(bonus), QuestIDs: ids}, "")
	if err != nil {
		w.t.Fatalf("create combo %s: %v", name, err)
	}
	return c.ID
}

// markDone has the member accept and mark the chore done through the service,
// then sets the new pending completion's done_at to at (test setup: it fixes
// the mark-done time) and remembers it in w.last. available_at is reset first
// so a chore marked done earlier in the test can be taken again.
func (w *comboWorld) markDone(questID, memberID uuid.UUID, at time.Time) {
	w.t.Helper()
	if _, err := w.pool.Exec(w.ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 second' WHERE household_id = $1 AND id = $2`, w.h.ID, questID); err != nil {
		w.t.Fatalf("reset available_at: %v", err)
	}
	act := app.MemberAction{MemberID: memberID}
	if _, err := w.quests.Accept(w.ctx, w.admin, w.h.ID, questID, act); err != nil {
		w.t.Fatalf("accept: %v", err)
	}
	if _, err := w.quests.MarkDone(w.ctx, w.admin, w.h.ID, questID, act); err != nil {
		w.t.Fatalf("mark done: %v", err)
	}
	// The completion just recorded has done_at NOW(), later than every
	// back-dated one in these tests.
	var completionID uuid.UUID
	if err := w.pool.QueryRow(w.ctx, `
		UPDATE quest_completions SET done_at = $4
		WHERE id = (SELECT id FROM quest_completions
		            WHERE household_id = $1 AND quest_id = $2 AND member_id = $3 AND status = 'pending'
		            ORDER BY done_at DESC, id DESC LIMIT 1)
		RETURNING id`, w.h.ID, questID, memberID, at).Scan(&completionID); err != nil {
		w.t.Fatalf("set completion done_at: %v", err)
	}
	w.last[questID] = completionID
}

// approve approves the completion the chore's latest markDone recorded.
func (w *comboWorld) approve(questID uuid.UUID) (db.FamilyMember, []app.ComboBonus) {
	w.t.Helper()
	_, m, bonuses, _, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, w.last[questID], "")
	if err != nil {
		w.t.Fatalf("approve: %v", err)
	}
	if bonuses == nil {
		w.t.Fatalf("approve returned nil bonuses, want non-nil")
	}
	return m, bonuses
}

func (w *comboWorld) points(memberID uuid.UUID) int32 {
	w.t.Helper()
	m, err := w.q.GetFamilyMember(w.ctx, db.GetFamilyMemberParams{HouseholdID: w.h.ID, ID: memberID})
	if err != nil {
		w.t.Fatalf("get member: %v", err)
	}
	return m.Points
}

func (w *comboWorld) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(w.ctx, query, args...).Scan(&n); err != nil {
		w.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (w *comboWorld) awards() int {
	return w.count(`SELECT count(*) FROM combo_awards WHERE household_id = $1`, w.h.ID)
}

func utc(day, hour, minute int) time.Time {
	return time.Date(2025, time.January, day, hour, minute, 0, 0, time.UTC)
}

// TestIntegration_ComboBonusPayment: approving the chore that completes a
// combo pays its bonus, judged by mark-done time on the household's clock,
// once per set of completions, atomically with the approval.
func TestIntegration_ComboBonusPayment(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	base := &comboWorld{
		ctx: context.Background(), pool: pool, q: q,
		quests: app.NewQuestService(q, pool), family: app.NewFamilyService(q, pool, 10), combos: app.NewComboService(q, pool),
		admin: registerVerified(t, pool, authSvc, mailer, "combopay").User.ID,
	}

	// CLM-2, CLM-4: both chores marked done on household day Jan 10 (NY), which
	// are different UTC days, approved long after: the first approval pays
	// chore points only, the last pays chore points plus the bonus. A chore in
	// no combo pays only its points.
	t.Run("same household day pays on the last approval", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Day")
		ann := w.member("Ann")
		a, b, solo := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15), w.chore("Feed cat", 5)
		kitchen := w.combo("Kitchen Crew", 20, a, b)

		w.markDone(solo, ann, utc(10, 12, 0))
		if m, bonuses := w.approve(solo); len(bonuses) != 0 || m.Points != 5 {
			t.Fatalf("chore in no combo: points %d bonuses %+v", m.Points, bonuses)
		}
		w.markDone(a, ann, utc(10, 23, 0)) // Jan 10 18:00 NY, Jan 10 UTC
		w.markDone(b, ann, utc(11, 1, 0))  // Jan 10 20:00 NY, Jan 11 UTC
		if m, bonuses := w.approve(a); len(bonuses) != 0 || m.Points != 15 {
			t.Fatalf("first approval: points %d bonuses %+v, want 15 and none", m.Points, bonuses)
		}
		m, bonuses := w.approve(b)
		if len(bonuses) != 1 || bonuses[0].ComboID != kitchen || bonuses[0].Name != "Kitchen Crew" || bonuses[0].BonusPoints != 20 {
			t.Fatalf("last approval bonuses: %+v", bonuses)
		}
		if m.ID != ann || m.Points != 5+10+15+20 || w.points(ann) != 50 {
			t.Fatalf("last approval member %+v, stored points %d, want 50", m, w.points(ann))
		}
		var day time.Time
		var pts int32
		var name string
		if err := pool.QueryRow(w.ctx, `SELECT award_day, points, combo_name FROM combo_awards WHERE household_id = $1`, w.h.ID).Scan(&day, &pts, &name); err != nil {
			t.Fatalf("read award: %v", err)
		}
		if day.Format("2006-01-02") != "2025-01-10" || pts != 20 || name != "Kitchen Crew" {
			t.Fatalf("award: day %s points %d name %q, want 2025-01-10 20 Kitchen Crew", day.Format("2006-01-02"), pts, name)
		}
		if n := w.count(`SELECT count(*) FROM quest_completions WHERE household_id = $1 AND member_id = $2`, w.h.ID, ann); n != 3 {
			t.Fatalf("quest_completions: %d, want 3", n)
		}
	})

	// CLM-3: different household days (straddling NY midnight, same UTC day)
	// and different members pay nothing.
	t.Run("different days or members pay nothing", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Apart")
		ann, ben := w.member("Ann"), w.member("Ben")
		a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
		w.combo("Kitchen Crew", 20, a, b)

		w.markDone(a, ann, utc(11, 4, 30)) // Jan 10 23:30 NY
		w.markDone(b, ann, utc(11, 5, 30)) // Jan 11 00:30 NY, same UTC day
		for _, id := range []uuid.UUID{a, b} {
			if _, bonuses := w.approve(id); len(bonuses) != 0 {
				t.Fatalf("different days paid %+v", bonuses)
			}
		}
		w.markDone(a, ann, utc(12, 15, 0))
		w.markDone(b, ben, utc(12, 15, 0))
		for _, id := range []uuid.UUID{a, b} {
			if _, bonuses := w.approve(id); len(bonuses) != 0 {
				t.Fatalf("different members paid %+v", bonuses)
			}
		}
		if w.awards() != 0 || w.points(ann) != 10+15+10 || w.points(ben) != 15 {
			t.Fatalf("awards %d, Ann %d (want 35), Ben %d (want 15)", w.awards(), w.points(ann), w.points(ben))
		}
	})

	// CLM-4: approval day never decides the combo day.
	// Case A: a member marks two combo chores done on the same household-local
	// day; approve the first, back-date its approved_at by direct SQL to the
	// previous local day, then approve the second; the combo bonus is paid
	// exactly once.
	// Case B (mirror): two completions marked done on different local days,
	// both approved with approved_at on the same local day; pays no bonus.
	t.Run("approved_at never decides combo day", func(t *testing.T) {
		// Case A: same done_at day, different approved_at days -> pays bonus.
		w := newComboWorld(t, base, "Combo App Day Same")
		ann := w.member("Ann")
		a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
		kitchen := w.combo("Kitchen Crew", 20, a, b)

		w.markDone(a, ann, utc(10, 14, 0)) // Jan 10 NY
		w.markDone(b, ann, utc(10, 15, 0)) // Jan 10 NY
		if m, bonuses := w.approve(a); len(bonuses) != 0 || m.Points != 10 {
			t.Fatalf("case A first approval: points %d bonuses %+v, want 10 and none", m.Points, bonuses)
		}
		if _, err := w.pool.Exec(w.ctx, `UPDATE quest_completions SET approved_at = $1 WHERE id = $2`, utc(9, 14, 0), w.last[a]); err != nil {
			t.Fatalf("case A back-date approved_at: %v", err)
		}
		m, bonuses := w.approve(b)
		if len(bonuses) != 1 || bonuses[0].ComboID != kitchen || bonuses[0].Name != "Kitchen Crew" || bonuses[0].BonusPoints != 20 {
			t.Fatalf("case A last approval bonuses: %+v", bonuses)
		}
		if m.Points != 10+15+20 || w.points(ann) != 45 {
			t.Fatalf("case A points: member %d, stored %d, want 45", m.Points, w.points(ann))
		}
		if w.awards() != 1 {
			t.Fatalf("case A awards: %d, want 1", w.awards())
		}

		// Case B (mirror): different done_at days, same approved_at day -> no bonus.
		wB := newComboWorld(t, base, "Combo App Day Diff")
		ben := wB.member("Ben")
		c, d := wB.chore("Sweep floor", 10), wB.chore("Mop floor", 15)
		wB.combo("Floor Duty", 20, c, d)

		wB.markDone(c, ben, utc(11, 14, 0)) // Jan 11 NY
		wB.markDone(d, ben, utc(12, 14, 0)) // Jan 12 NY
		if _, bonuses := wB.approve(c); len(bonuses) != 0 {
			t.Fatalf("case B first approval paid %+v", bonuses)
		}
		if _, bonuses := wB.approve(d); len(bonuses) != 0 {
			t.Fatalf("case B second approval paid %+v", bonuses)
		}
		var cAppDay, dAppDay time.Time
		if err := wB.pool.QueryRow(wB.ctx, `SELECT (approved_at AT TIME ZONE 'America/New_York')::date FROM quest_completions WHERE id = $1`, wB.last[c]).Scan(&cAppDay); err != nil {
			t.Fatalf("case B query c approved_at day: %v", err)
		}
		if err := wB.pool.QueryRow(wB.ctx, `SELECT (approved_at AT TIME ZONE 'America/New_York')::date FROM quest_completions WHERE id = $1`, wB.last[d]).Scan(&dAppDay); err != nil {
			t.Fatalf("case B query d approved_at day: %v", err)
		}
		if cAppDay != dAppDay {
			t.Fatalf("case B approved_at days differ: %v vs %v", cAppDay, dAppDay)
		}
		if wB.awards() != 0 || wB.points(ben) != 10+15 {
			t.Fatalf("case B awards %d, Ben points %d, want 0 and 25", wB.awards(), wB.points(ben))
		}
	})

	// CLM-5, criterion 2.1/3: a combo pays once per member and household day.
	// A second full set the same day (a chore redone after a reject, say)
	// pays chore points only; a full set on a later day pays again. A repeat
	// approval is ErrNotFound and pays nothing.
	t.Run("every full set pays once", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Again")
		ann := w.member("Ann")
		a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
		w.combo("Kitchen Crew", 20, a, b)

		sets := []struct {
			a, b   time.Time
			awards int
		}{
			{utc(10, 14, 0), utc(10, 15, 0), 1}, // Jan 10 morning NY
			{utc(10, 20, 0), utc(10, 21, 0), 1}, // Jan 10 afternoon NY: same day again, no second bonus
			{utc(12, 14, 0), utc(12, 15, 0), 2}, // a later day pays again
		}
		for i, s := range sets {
			w.markDone(a, ann, s.a)
			if _, bonuses := w.approve(a); len(bonuses) != 0 {
				t.Fatalf("set %d: first chore paid %+v", i, bonuses)
			}
			w.markDone(b, ann, s.b)
			wantPaid := s.awards - w.awards()
			if _, bonuses := w.approve(b); len(bonuses) != wantPaid {
				t.Fatalf("set %d: completing chore paid %+v, want %d bonus", i, bonuses, wantPaid)
			}
			if w.awards() != s.awards {
				t.Fatalf("set %d: %d awards, want %d", i, w.awards(), s.awards)
			}
		}
		if _, _, _, _, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, w.last[b], ""); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("repeat approval: %v, want ErrNotFound", err)
		}
		if w.awards() != 2 || w.points(ann) != 3*(10+15)+2*20 {
			t.Fatalf("after repeat: awards %d points %d, want 2 and %d", w.awards(), w.points(ann), 3*25+40)
		}
		if n := w.count(`SELECT count(*) FROM combo_award_completions cac JOIN combo_awards ca ON ca.id = cac.combo_award_id WHERE ca.household_id = $1`, w.h.ID); n != 4 {
			t.Fatalf("claimed completions: %d, want 4", n)
		}
	})

	// CLM-6: both chores pending, approved at the same moment: exactly one
	// award and one bonus, every round.
	t.Run("concurrent approvals pay once", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Race")
		ann := w.member("Ann")
		a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
		w.combo("Kitchen Crew", 20, a, b)
		for round := 0; round < 3; round++ {
			w.markDone(a, ann, utc(10+round, 15, 0))
			w.markDone(b, ann, utc(10+round, 16, 0))
			var wg sync.WaitGroup
			errs := make([]error, 2)
			paid := make([]int, 2)
			start := make(chan struct{})
			for i, id := range []uuid.UUID{w.last[a], w.last[b]} {
				wg.Add(1)
				go func(i int, id uuid.UUID) {
					defer wg.Done()
					<-start
					_, _, bonuses, _, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, id, "")
					errs[i], paid[i] = err, len(bonuses)
				}(i, id)
			}
			close(start)
			wg.Wait()
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("round %d: approvals failed: %v, %v", round, errs[0], errs[1])
			}
			if paid[0]+paid[1] != 1 || w.awards() != round+1 || w.points(ann) != int32((round+1)*45) {
				t.Fatalf("round %d: bonuses reported %v, awards %d, points %d", round, paid, w.awards(), w.points(ann))
			}
		}
	})

	// CLM-7: one completion finishing two combos pays both; the unfinished one
	// pays nothing.
	t.Run("one completion pays every combo it finishes", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Many")
		ann := w.member("Ann")
		x, y, z, v := w.chore("X", 10), w.chore("Y", 1), w.chore("Z", 2), w.chore("V", 3)
		c1, c2 := w.combo("XY", 20, x, y), w.combo("XZ", 30, x, z)
		w.combo("XV", 99, x, v)
		w.markDone(y, ann, utc(10, 14, 0))
		w.approve(y)
		w.markDone(z, ann, utc(10, 15, 0))
		w.approve(z)
		w.markDone(x, ann, utc(10, 16, 0))
		m, bonuses := w.approve(x)
		got := map[uuid.UUID]int32{}
		for _, b := range bonuses {
			got[b.ComboID] = b.BonusPoints
		}
		if len(bonuses) != 2 || got[c1] != 20 || got[c2] != 30 {
			t.Fatalf("bonuses %+v, want XY+20 and XZ+30", bonuses)
		}
		if m.Points != 1+2+10+20+30 || w.awards() != 2 {
			t.Fatalf("points %d awards %d, want 63 and 2", m.Points, w.awards())
		}
	})

	// CLM-12: if paying the bonus fails, the whole approval is undone.
	t.Run("failed bonus rolls back the approval", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Fault")
		ann := w.member("Ann")
		a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
		w.combo("Kitchen Crew", 20, a, b)
		w.markDone(a, ann, utc(10, 14, 0))
		w.approve(a)
		w.markDone(b, ann, utc(10, 15, 0))

		// A test-only trigger fails every award insert for this household.
		suffix := strings.ReplaceAll(w.h.ID.String(), "-", "")
		fn, trg := "test_fail_award_"+suffix, "test_fail_award_trg_"+suffix
		if _, err := pool.Exec(w.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.household_id = '%s' THEN RAISE EXCEPTION 'injected award failure'; END IF;
  RETURN NEW;
END $$`, fn, w.h.ID)); err != nil {
			t.Fatalf("create trigger function: %v", err)
		}
		dropped := false
		drop := func() {
			if dropped {
				return
			}
			dropped = true
			if _, err := pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON combo_awards; DROP FUNCTION IF EXISTS %s()`, trg, fn)); err != nil {
				t.Errorf("drop trigger: %v", err)
			}
		}
		t.Cleanup(drop)
		if _, err := pool.Exec(w.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON combo_awards FOR EACH ROW EXECUTE FUNCTION %s()`, trg, fn)); err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		if _, _, _, _, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, w.last[b], ""); err == nil {
			t.Fatal("approve with failing award insert succeeded")
		}
		if n := w.count(`SELECT count(*) FROM quest_completions WHERE household_id = $1 AND id = $2 AND status = 'pending' AND approved_at IS NULL`, w.h.ID, w.last[b]); n != 1 {
			t.Fatalf("completion not pending after failed approval")
		}
		if w.points(ann) != 10 || w.awards() != 0 ||
			w.count(`SELECT count(*) FROM quest_completions WHERE household_id = $1 AND quest_id = $2 AND status = 'approved'`, w.h.ID, b) != 0 {
			t.Fatalf("failed approval left points %d, awards %d or an approved completion", w.points(ann), w.awards())
		}

		drop()
		if m, bonuses := w.approve(b); len(bonuses) != 1 || m.Points != 45 {
			t.Fatalf("retry: points %d bonuses %+v, want 45 and one", m.Points, bonuses)
		}
	})

	// CLM-13: deleting a chore of a paid combo keeps the combo (one chore,
	// no bonus) and past awards and points; editing it back to two chores
	// pays again. Removing a member removes their rows only.
	t.Run("deleting a chore or a member keeps the rest", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Delete")
		ann, ben := w.member("Ann"), w.member("Ben")
		a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
		combo := w.combo("Kitchen Crew", 20, a, b)
		for _, who := range []uuid.UUID{ann, ben} {
			w.markDone(a, who, utc(10, 14, 0))
			w.approve(a)
			w.markDone(b, who, utc(10, 15, 0))
			w.approve(b)
		}
		if w.awards() != 2 {
			t.Fatalf("setup awards: %d, want 2", w.awards())
		}

		if err := w.quests.Delete(w.ctx, w.admin, w.h.ID, b, ""); err != nil {
			t.Fatalf("delete chore: %v", err)
		}
		c, err := w.combos.Get(w.ctx, w.admin, w.h.ID, combo)
		if err != nil || len(c.QuestIDs) != 1 || c.QuestIDs[0] != a {
			t.Fatalf("combo after chore delete: %+v %v", c, err)
		}
		if w.awards() != 2 || w.points(ann) != 45 || w.points(ben) != 45 {
			t.Fatalf("after chore delete: awards %d, Ann %d, Ben %d", w.awards(), w.points(ann), w.points(ben))
		}
		w.markDone(a, ann, utc(11, 14, 0))
		if _, bonuses := w.approve(a); len(bonuses) != 0 {
			t.Fatalf("one-chore combo paid %+v", bonuses)
		}

		cc := w.chore("Sweep floor", 5)
		if _, err := w.combos.Update(w.ctx, w.admin, w.h.ID, combo, app.ComboInput{Name: "Kitchen Crew", BonusPoints: num(20), QuestIDs: []uuid.UUID{a, cc}}, ""); err != nil {
			t.Fatalf("edit combo back to two chores: %v", err)
		}
		w.markDone(cc, ann, utc(12, 14, 0))
		w.approve(cc)
		w.markDone(a, ann, utc(12, 15, 0))
		if _, bonuses := w.approve(a); len(bonuses) != 1 {
			t.Fatalf("edited combo bonuses %+v, want one", bonuses)
		}
		if w.points(ann) != 45+10+5+10+20 {
			t.Fatalf("Ann points %d, want 90", w.points(ann))
		}

		if err := w.family.Remove(w.ctx, w.admin, w.h.ID, ann, ""); err != nil {
			t.Fatalf("remove Ann: %v", err)
		}
		if n := w.count(`SELECT count(*) FROM combo_awards WHERE household_id = $1 AND member_id = $2`, w.h.ID, ann) +
			w.count(`SELECT count(*) FROM quest_completions WHERE household_id = $1 AND member_id = $2`, w.h.ID, ann); n != 0 {
			t.Fatalf("Ann's rows remain: %d", n)
		}
		if w.count(`SELECT count(*) FROM combo_awards WHERE household_id = $1 AND member_id = $2`, w.h.ID, ben) != 1 || w.points(ben) != 45 {
			t.Fatalf("Ben changed by Ann's removal: points %d", w.points(ben))
		}
	})

	// Quest sharing CLM-17: a shared completion counts toward the combos of
	// every member it covers. Approving it pays each of them the combo bonus
	// once for the day; a later completion the same day pays it to nobody
	// again.
	t.Run("shared quest counts for every participant", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo Shared")
		ann, ben := w.member("Ann"), w.member("Ben")
		sharedQuest, err := w.quests.Create(w.ctx, w.admin, w.h.ID, app.QuestInput{Name: "Clean the living room", Points: num(30), Shareable: true, TeamworkBonus: num(10)}, "")
		if err != nil {
			t.Fatalf("create shareable chore: %v", err)
		}
		a, b := sharedQuest.ID, w.chore("Wipe counters", 15)
		kitchen := w.combo("Kitchen Crew", 20, a, b)

		// Each does the plain chore on Jan 10 (NY) on their own.
		w.markDone(b, ann, utc(10, 14, 0))
		if _, bonuses := w.approve(b); len(bonuses) != 0 {
			t.Fatalf("Ann's plain chore paid %+v", bonuses)
		}
		w.markDone(b, ben, utc(10, 15, 0))
		if _, bonuses := w.approve(b); len(bonuses) != 0 {
			t.Fatalf("Ben's plain chore paid %+v", bonuses)
		}

		// Ann takes the shared chore and shares it with Ben; Ben marks it
		// done the same household day.
		annAct, benAct := app.MemberAction{MemberID: ann}, app.MemberAction{MemberID: ben}
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h.ID, a, annAct); err != nil {
			t.Fatalf("Ann accepts: %v", err)
		}
		if _, err := w.quests.Share(w.ctx, w.admin, w.h.ID, a, annAct, benAct); err != nil {
			t.Fatalf("share: %v", err)
		}
		if _, err := w.quests.MarkDone(w.ctx, w.admin, w.h.ID, a, benAct); err != nil {
			t.Fatalf("Ben marks done: %v", err)
		}
		var sharedCompletion uuid.UUID
		if err := pool.QueryRow(w.ctx, `
			UPDATE quest_completions SET done_at = $3
			WHERE household_id = $1 AND quest_id = $2 AND status = 'pending'
			RETURNING id`, w.h.ID, a, utc(10, 16, 0)).Scan(&sharedCompletion); err != nil {
			t.Fatalf("set shared completion done_at: %v", err)
		}
		_, marker, bonuses, credited, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, sharedCompletion, "")
		if err != nil {
			t.Fatalf("approve shared completion: %v", err)
		}
		if marker.ID != ben || len(bonuses) != 1 || bonuses[0].ComboID != kitchen || bonuses[0].BonusPoints != 20 {
			t.Fatalf("marker %v bonuses %+v, want Ben paid Kitchen Crew 20", marker.ID, bonuses)
		}
		if len(credited) != 2 {
			t.Fatalf("credited %+v, want Ann and Ben", credited)
		}
		// 15 (plain) + 15 split + 10 teamwork bonus + 20 combo each.
		for _, c := range credited {
			if c.Member.Points != 60 {
				t.Errorf("member %v returned points %d, want 60", c.Member.ID, c.Member.Points)
			}
		}
		if w.points(ann) != 60 || w.points(ben) != 60 {
			t.Fatalf("points Ann %d Ben %d, want 60 each", w.points(ann), w.points(ben))
		}
		for _, m := range []uuid.UUID{ann, ben} {
			if n := w.count(`SELECT count(*) FROM combo_awards WHERE household_id = $1 AND member_id = $2`, w.h.ID, m); n != 1 {
				t.Fatalf("member %v has %d combo awards, want 1", m, n)
			}
		}
		if n := w.count(`SELECT count(*) FROM combo_award_completions WHERE quest_completion_id = $1`, sharedCompletion); n != 2 {
			t.Fatalf("shared completion counted %d times, want once per member (2)", n)
		}

		// The same day again: Ann does both chores alone and is paid only the
		// chore points; the combo was already paid to her for Jan 10.
		w.markDone(b, ann, utc(10, 17, 0))
		w.approve(b)
		w.markDone(a, ann, utc(10, 18, 0))
		if m, again := w.approve(a); len(again) != 0 || m.Points != 60+15+30 {
			t.Fatalf("second set same day: points %d bonuses %+v, want 105 and none", m.Points, again)
		}
		if w.awards() != 2 || w.points(ben) != 60 {
			t.Fatalf("awards %d, Ben %d after Ann's second set; want 2 and 60", w.awards(), w.points(ben))
		}
	})

	// CLM-10: another household's approvals never touch this one's awards or
	// points, even with identical chores and combos.
	t.Run("households are isolated", func(t *testing.T) {
		one, two := newComboWorld(t, base, "Combo One"), newComboWorld(t, base, "Combo Two")
		ann1, ann2 := one.member("Ann"), two.member("Ann")
		a1, b1 := one.chore("Load dishwasher", 10), one.chore("Wipe counters", 15)
		one.combo("Kitchen Crew", 20, a1, b1)
		a2, b2 := two.chore("Load dishwasher", 10), two.chore("Wipe counters", 15)
		two.combo("Kitchen Crew", 20, a2, b2)

		one.markDone(a1, ann1, utc(10, 14, 0))
		one.approve(a1)
		two.markDone(a2, ann2, utc(10, 14, 0))
		two.approve(a2)
		two.markDone(b2, ann2, utc(10, 15, 0))
		if _, bonuses := two.approve(b2); len(bonuses) != 1 {
			t.Fatalf("household two bonuses %+v", bonuses)
		}
		if one.awards() != 0 || one.points(ann1) != 10 {
			t.Fatalf("household one touched: awards %d points %d", one.awards(), one.points(ann1))
		}
		// Approving household two's completion through household one is not found.
		two.markDone(a2, ann2, utc(11, 14, 0))
		if _, _, _, _, err := one.quests.Approve(one.ctx, one.admin, one.h.ID, two.last[a2], ""); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("cross-household approve: %v, want ErrNotFound", err)
		}
	})
}
