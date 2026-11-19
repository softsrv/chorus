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

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// newComboHistoryBase builds the shared comboWorld (services and an admin)
// for the combo points-history tests.
func newComboHistoryBase(t *testing.T, prefix string) *comboWorld {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	return &comboWorld{
		ctx: context.Background(), pool: pool, q: q,
		quests: app.NewQuestService(q, pool), family: app.NewFamilyService(q, pool, 4), combos: app.NewComboService(q, pool),
		admin: registerVerified(t, pool, authSvc, mailer, prefix).User.ID,
	}
}

// history returns the member's points_awards rows, newest first.
func (w *comboWorld) history(memberID uuid.UUID) []db.PointsAward {
	w.t.Helper()
	rows, err := w.q.ListMemberPointsAwards(w.ctx, db.ListMemberPointsAwardsParams{
		HouseholdID: w.h.ID, MemberID: memberID, Limit: 100, Offset: 0,
	})
	if err != nil {
		w.t.Fatalf("list points awards: %v", err)
	}
	return rows
}

func (w *comboWorld) kindCount(kind string) int {
	return w.count(`SELECT count(*) FROM points_awards WHERE household_id = $1 AND kind = $2`, w.h.ID, kind)
}

// TestIntegration_ComboPointsHistory proves combo entry written: each combo bonus paid by
// Approve writes exactly one points_awards row (kind combo, the combo's name,
// its bonus points) for the holder, separate from the quest's row, in the
// same approval; the points total rises by exactly the sum of the rows the
// approval wrote, and the member's points history lists the entry newest
// first. Two combos finished by one approval write two combo rows.
func TestIntegration_ComboPointsHistory(t *testing.T) {
	base := newComboHistoryBase(t, "combohistory")

	t.Run("one combo writes one combo entry", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo History One")
		ann := w.member("Ann")
		a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
		w.combo("Kitchen Crew", 20, a, b)

		w.markDone(a, ann, utc(10, 14, 0))
		w.approve(a)
		before, pointsBefore := w.history(ann), w.points(ann)
		if len(before) != 1 || before[0].Kind != "quest" || w.kindCount("combo") != 0 {
			t.Fatalf("after first chore: history %+v, want one quest entry and no combo entry", before)
		}

		w.markDone(b, ann, utc(10, 15, 0))
		m, bonuses := w.approve(b)
		if len(bonuses) != 1 || bonuses[0].Name != "Kitchen Crew" || bonuses[0].BonusPoints != 20 {
			t.Fatalf("bonuses %+v, want Kitchen Crew +20", bonuses)
		}
		after := w.history(ann)
		added := after[:len(after)-len(before)]
		if len(added) != 2 {
			t.Fatalf("approval wrote %d history rows, want 2 (quest + combo): %+v", len(added), added)
		}
		var questRow, comboRow *db.PointsAward
		sum := int32(0)
		for i := range added {
			r := &added[i]
			sum += r.Points
			if r.HouseholdID != w.h.ID || r.MemberID != ann {
				t.Fatalf("row for wrong household/member: %+v", r)
			}
			switch r.Kind {
			case "quest":
				questRow = r
			case "combo":
				comboRow = r
			}
		}
		if questRow == nil || comboRow == nil {
			t.Fatalf("want one quest and one combo row, got %+v", added)
		}
		if questRow.Reason != "Wipe counters" || questRow.Points != 15 {
			t.Fatalf("quest row %+v, want Wipe counters 15", questRow)
		}
		if comboRow.Reason != "Kitchen Crew" || comboRow.Points != 20 {
			t.Fatalf("combo row %+v, want Kitchen Crew 20", comboRow)
		}
		// Both credits of one approval are earned at the same moment (approval time, CLM-3).
		if !comboRow.EarnedAt.Valid || !comboRow.EarnedAt.Time.Equal(questRow.EarnedAt.Time) {
			t.Fatalf("combo earned_at %v, want the quest entry's %v", comboRow.EarnedAt, questRow.EarnedAt)
		}
		var bDoneAt, bApprovedAt time.Time
		if err := w.pool.QueryRow(w.ctx, `SELECT done_at, approved_at FROM quest_completions WHERE id = $1`, w.last[b]).Scan(&bDoneAt, &bApprovedAt); err != nil {
			t.Fatalf("read completion timestamps: %v", err)
		}
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatalf("load location America/New_York: %v", err)
		}
		doneY, doneM, doneD := bDoneAt.In(loc).Date()
		apprY, apprM, apprD := bApprovedAt.In(loc).Date()
		doneDate := time.Date(doneY, doneM, doneD, 0, 0, 0, 0, loc)
		apprDate := time.Date(apprY, apprM, apprD, 0, 0, 0, 0, loc)
		if !doneDate.Before(apprDate) {
			t.Fatalf("done_at date %v not strictly before approved_at date %v in %s", doneDate, apprDate, loc)
		}
		if !comboRow.EarnedAt.Time.Equal(bApprovedAt) {
			t.Fatalf("combo earned_at %v, want approved_at %v", comboRow.EarnedAt.Time, bApprovedAt)
		}
		if comboRow.EarnedAt.Time.Equal(bDoneAt) {
			t.Fatalf("combo earned_at %v equals done_at %v, want approval-dated", comboRow.EarnedAt.Time, bDoneAt)
		}
		if m.Points != pointsBefore+sum || w.points(ann) != pointsBefore+15+20 {
			t.Fatalf("points %d (stored %d), want %d + rows %d = 45", m.Points, w.points(ann), pointsBefore, sum)
		}
		if w.kindCount("combo") != 1 {
			t.Fatalf("combo rows in household: %d, want 1", w.kindCount("combo"))
		}

		hist, err := w.family.PointsHistory(w.ctx, w.admin, w.h.ID, ann, "", "", 50, 0)
		if err != nil {
			t.Fatalf("points history: %v", err)
		}
		if len(hist) != 3 || hist[0].ID != comboRow.ID || hist[0].Kind != "combo" ||
			hist[0].Reason != "Kitchen Crew" || hist[0].Points != 20 {
			t.Fatalf("points history %+v, want the combo entry first of 3", hist)
		}
		for i := 1; i < len(hist); i++ {
			if hist[i].CreatedAt.After(hist[i-1].CreatedAt) {
				t.Fatalf("points history not newest first at %d: %+v", i, hist)
			}
		}
	})

	t.Run("two combos write two combo entries", func(t *testing.T) {
		w := newComboWorld(t, base, "Combo History Two")
		ann := w.member("Ann")
		x, y, z := w.chore("X", 10), w.chore("Y", 1), w.chore("Z", 2)
		w.combo("XY", 20, x, y)
		w.combo("XZ", 30, x, z)
		w.markDone(y, ann, utc(10, 14, 0))
		w.approve(y)
		w.markDone(z, ann, utc(10, 15, 0))
		w.approve(z)
		before, pointsBefore := w.history(ann), w.points(ann)

		w.markDone(x, ann, utc(10, 16, 0))
		m, bonuses := w.approve(x)
		if len(bonuses) != 2 {
			t.Fatalf("bonuses %+v, want two", bonuses)
		}
		after := w.history(ann)
		added := after[:len(after)-len(before)]
		combos := map[string]int32{}
		sum := int32(0)
		quests := 0
		for _, r := range added {
			sum += r.Points
			switch r.Kind {
			case "combo":
				if _, dup := combos[r.Reason]; dup {
					t.Fatalf("duplicate combo row %q: %+v", r.Reason, added)
				}
				combos[r.Reason] = r.Points
			case "quest":
				quests++
			}
		}
		if len(added) != 3 || quests != 1 || len(combos) != 2 || combos["XY"] != 20 || combos["XZ"] != 30 {
			t.Fatalf("approval rows %+v, want quest X + combo XY 20 + combo XZ 30", added)
		}
		if sum != 10+20+30 || m.Points != pointsBefore+sum || w.points(ann) != pointsBefore+sum {
			t.Fatalf("points %d, want %d + %d", m.Points, pointsBefore, sum)
		}
	})
}

// TestIntegration_ComboHistoryFailureRollsBack proves failed combo entry undoes the approval: when only the
// combo history insert fails (the quest's own credit and history succeed
// first), Approve returns an error and the whole approval is undone; once
// the fault is removed a retry pays exactly once.
func TestIntegration_ComboHistoryFailureRollsBack(t *testing.T) {
	base := newComboHistoryBase(t, "combohistfault")
	w := newComboWorld(t, base, "Combo History Fault")
	ann := w.member("Ann")
	a, b := w.chore("Load dishwasher", 10), w.chore("Wipe counters", 15)
	w.combo("Kitchen Crew", 20, a, b)
	w.markDone(a, ann, utc(10, 14, 0))
	w.approve(a)
	w.markDone(b, ann, utc(10, 15, 0))
	pointsBefore, rowsBefore := w.points(ann), len(w.history(ann))

	// A test-only trigger fails combo history inserts for this household only.
	suffix := strings.ReplaceAll(w.h.ID.String(), "-", "")
	fn, trg := "test_fail_combo_hist_"+suffix, "test_fail_combo_hist_trg_"+suffix
	if _, err := w.pool.Exec(w.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.household_id = '%s' AND NEW.kind = 'combo' THEN RAISE EXCEPTION 'injected combo history failure'; END IF;
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
		if _, err := w.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON points_awards; DROP FUNCTION IF EXISTS %s()`, trg, fn)); err != nil {
			t.Errorf("drop trigger: %v", err)
		}
	}
	t.Cleanup(drop)
	if _, err := w.pool.Exec(w.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON points_awards FOR EACH ROW EXECUTE FUNCTION %s()`, trg, fn)); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, _, _, _, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, w.last[b], ""); err == nil {
		t.Fatal("approve with failing combo history insert succeeded")
	}
	if n := w.count(`SELECT count(*) FROM quest_completions WHERE household_id = $1 AND id = $2 AND status = 'pending'`, w.h.ID, w.last[b]); n != 1 {
		t.Fatalf("completion not pending after failed approval: %d pending rows", n)
	}
	if w.points(ann) != pointsBefore || len(w.history(ann)) != rowsBefore || w.awards() != 0 {
		t.Fatalf("failed approval left points %d (want %d), history rows %d (want %d), awards %d",
			w.points(ann), pointsBefore, len(w.history(ann)), rowsBefore, w.awards())
	}

	drop()
	m, bonuses := w.approve(b)
	if len(bonuses) != 1 || m.Points != pointsBefore+15+20 {
		t.Fatalf("retry: points %d bonuses %+v, want %d and one", m.Points, bonuses, pointsBefore+35)
	}
	if w.kindCount("combo") != 1 || len(w.history(ann)) != rowsBefore+2 {
		t.Fatalf("retry: combo rows %d, history rows %d, want 1 and %d", w.kindCount("combo"), len(w.history(ann)), rowsBefore+2)
	}
}

// TestIntegration_NoComboApprovalUnchanged proves that approving a completion in a household
// with no combo credits the quest's points once and writes no combo history entry.
func TestIntegration_NoComboApprovalUnchanged(t *testing.T) {
	base := newComboHistoryBase(t, "nocombo")
	w := newComboWorld(t, base, "No Combo Family")
	ann := w.member("Ann")
	chore := w.chore("Feed cat", 10)

	doneAt := utc(10, 14, 0)
	w.markDone(chore, ann, doneAt)
	first := w.last[chore]
	approved, m, bonuses, _, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, first, "")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if bonuses == nil || len(bonuses) != 0 {
		t.Fatalf("bonuses %+v, want empty non-nil", bonuses)
	}
	if m.ID != ann || m.Points != 10 {
		t.Fatalf("credited member %+v, want Ann with 10", m)
	}
	// The completion is approved and keeps its mark-done time.
	if approved.ID != first || approved.Status != "approved" || approved.ApprovedAt == nil || !approved.DoneAt.Equal(doneAt) {
		t.Fatalf("approved completion: %+v, want approved with done_at %v", approved, doneAt)
	}
	hist := w.history(ann)
	if len(hist) != 1 || hist[0].Kind != "quest" || hist[0].Reason != "Feed cat" || hist[0].Points != 10 {
		t.Fatalf("history %+v, want only the quest entry", hist)
	}

	if _, _, _, _, err := w.quests.Approve(w.ctx, w.admin, w.h.ID, first, ""); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("repeat approval: %v, want ErrNotFound", err)
	}
	if w.points(ann) != 10 || len(w.history(ann)) != 1 {
		t.Fatalf("repeat approval paid again: points %d, history %d", w.points(ann), len(w.history(ann)))
	}

	// Concurrent approvals of one completion: one wins, the other is ErrNotFound.
	w.markDone(chore, ann, utc(12, 14, 0))
	second := w.last[chore]
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, _, _, errs[i] = w.quests.Approve(w.ctx, w.admin, w.h.ID, second, "")
		}(i)
	}
	close(start)
	wg.Wait()
	wins, losses := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			wins++
		case errors.Is(e, app.ErrNotFound):
			losses++
		default:
			t.Fatalf("concurrent approval failed: %v", e)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("concurrent approvals: %v, want one success and one ErrNotFound", errs)
	}
	if w.points(ann) != 20 || w.kindCount("quest") != 2 || w.kindCount("combo") != 0 {
		t.Fatalf("after concurrent approvals: points %d, quest rows %d, combo rows %d; want 20, 2, 0",
			w.points(ann), w.kindCount("quest"), w.kindCount("combo"))
	}
}
