//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_QuestMarkDoneCooldown_CLM2 directly proves CLM-2:
// MarkDone, in one transaction, inserts a pending completion (done_at=NOW()),
// sets completed_at=NOW() and available_at to local midnight in households.timezone
// starting the local date of NOW() plus repeat_days days, computed by quest_next_available_at.
// It releases the holder (claimed_by/claimed_at NULL), requires claimed_by = acting member
// and household_id, and moves no points. A never-completed quest has available_at NULL and is available.
func TestIntegration_QuestMarkDoneCooldown_CLM2(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 4)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questclm2").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "CLM2 Family", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	member, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Sam", PIN: "1234"}, "")
	if err != nil {
		t.Fatalf("create Sam: %v", err)
	}
	asSam := app.MemberAction{MemberID: member.ID, PIN: "1234"}

	// 1. A never-completed quest has available_at NULL and is available.
	neverDone, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Rake leaves", Points: num(5), RepeatDays: num(2)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	gotNever, err := quests.Get(ctx, alice, h.ID, neverDone.ID)
	if err != nil {
		t.Fatalf("get never-done: %v", err)
	}
	if gotNever.AvailableAt.Valid || gotNever.CompletedAt.Valid || !app.QuestAvailable(gotNever.HouseholdQuest, time.Now()) {
		t.Errorf("never-done quest should have NULL available_at and be available: %+v", gotNever)
	}

	// 2. Accept, then mark done.
	if _, err := quests.Accept(ctx, alice, h.ID, neverDone.ID, asSam); err != nil {
		t.Fatalf("Sam accept: %v", err)
	}
	markTime := time.Now()
	doneView, err := quests.MarkDone(ctx, alice, h.ID, neverDone.ID, asSam)
	if err != nil {
		t.Fatalf("Sam mark done: %v", err)
	}

	// Assert: claimed_by is NULL, claimed_at is NULL
	if doneView.ClaimedBy != nil || doneView.ClaimedAt.Valid {
		t.Errorf("mark done did not release holder: claimed_by=%v", doneView.ClaimedBy)
	}

	// Assert: points unchanged
	samMember, err := familySvc.Get(ctx, alice, h.ID, member.ID)
	if err != nil || samMember.Points != 0 {
		t.Errorf("Sam's points changed on MarkDone: %d %v, want 0", samMember.Points, err)
	}

	// Assert: exactly one pending completion
	pending, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil || len(pending) != 1 || pending[0].QuestID != neverDone.ID || pending[0].MemberID != member.ID {
		t.Fatalf("pending completions: %+v, %v", pending, err)
	}

	// Assert: available_at equals local midnight of (local date + repeat_days) in America/Chicago
	chicagoLoc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	localMark := markTime.In(chicagoLoc)
	wantMidnight := time.Date(localMark.Year(), localMark.Month(), localMark.Day()+2, 0, 0, 0, 0, chicagoLoc)
	if !doneView.AvailableAt.Valid || !doneView.AvailableAt.Time.Equal(wantMidnight) {
		t.Errorf("available_at %v, want local midnight %v", doneView.AvailableAt.Time, wantMidnight)
	}
	if app.QuestAvailable(doneView.HouseholdQuest, time.Now()) {
		t.Error("quest with future available_at should not be available")
	}

	// 3. Back-date available_at by SQL and assert QuestAvailable
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 second' WHERE id = $1`, neverDone.ID); err != nil {
		t.Fatalf("back-date available_at: %v", err)
	}
	gotAged, err := quests.Get(ctx, alice, h.ID, neverDone.ID)
	if err != nil {
		t.Fatalf("get aged: %v", err)
	}
	if !app.QuestAvailable(gotAged.HouseholdQuest, time.Now()) {
		t.Errorf("aged quest not available: %+v", gotAged)
	}
}

// TestIntegration_QuestApproval_RollbackOnPointsAwardFailure proves CLM-2:
// when the points_awards insert fails during Approve, the whole transaction rolls back:
// the completion is still pending, the member's points
// are unchanged, zero history rows exist, and Approve returns an error.
// Dropping the fault trigger allows a retry to succeed.
func TestIntegration_QuestApproval_RollbackOnPointsAwardFailure(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 4)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "approvefault").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Fault Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	ann, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ann"}, "")
	if err != nil {
		t.Fatalf("create Ann: %v", err)
	}
	asAnn := app.MemberAction{MemberID: ann.ID}
	quest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Mow lawn", Points: num(10), RepeatDays: num(2)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}

	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	pendingRows, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil || len(pendingRows) != 1 || pendingRows[0].MemberID != ann.ID {
		t.Fatalf("pending after mark done: %+v %v", pendingRows, err)
	}
	compID := pendingRows[0].ID

	// Fault injection: trigger on points_awards fails the insert.
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION test_fail_points_award() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'points award insert failed'; END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fail_points_award() CASCADE`)
	})
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_fail_points_award BEFORE INSERT ON points_awards
		FOR EACH ROW EXECUTE FUNCTION test_fail_points_award()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Attempt approve: must fail because points_awards insert fails.
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, compID, ""); err == nil {
		t.Fatal("approve succeeded despite points_awards trigger failure")
	}

	// Assert whole transaction rolled back:
	// 1. The completion is still pending.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM quest_completions WHERE household_id = $1 AND id = $2`, h.ID, compID).Scan(&status); err != nil || status != "pending" {
		t.Errorf("completion after failed approve: status %q %v, want pending", status, err)
	}

	// 2. Member points unchanged (0).
	mAfter, err := q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: h.ID, ID: ann.ID})
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if mAfter.Points != 0 {
		t.Errorf("member points after failed approve: got %d, want 0", mAfter.Points)
	}

	// 3. Zero points_awards rows.
	awards, err := q.ListMemberPointsAwards(ctx, db.ListMemberPointsAwardsParams{
		HouseholdID: h.ID,
		MemberID:    ann.ID,
		Limit:       100,
		Offset:      0,
	})
	if err != nil {
		t.Fatalf("list points awards: %v", err)
	}
	if len(awards) != 0 {
		t.Errorf("points awards count after failed approve: got %d, want 0", len(awards))
	}

	// 4. Drop trigger and assert retry succeeds.
	if _, err := pool.Exec(ctx, `DROP TRIGGER test_fail_points_award ON points_awards`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	approved, creditedMember, _, _, err := quests.Approve(ctx, alice, h.ID, compID, "")
	if err != nil {
		t.Fatalf("retry approve after dropping trigger: %v", err)
	}
	if creditedMember.ID != ann.ID || creditedMember.Points != 10 {
		t.Errorf("credited member after retry: %+v", creditedMember)
	}
	if approved.ID != compID || approved.Status != "approved" || approved.ApprovedAt == nil {
		t.Errorf("approved completion: %+v", approved)
	}

	// Member points credited
	mFinal, err := q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: h.ID, ID: ann.ID})
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if mFinal.Points != 10 {
		t.Errorf("member points after successful retry: got %d, want 10", mFinal.Points)
	}

	// Exactly 1 points award row now exists
	awardsFinal, err := q.ListMemberPointsAwards(ctx, db.ListMemberPointsAwardsParams{
		HouseholdID: h.ID,
		MemberID:    ann.ID,
		Limit:       100,
		Offset:      0,
	})
	if err != nil {
		t.Fatalf("list points awards final: %v", err)
	}
	if len(awardsFinal) != 1 || awardsFinal[0].Points != 10 || awardsFinal[0].Reason != "Mow lawn" {
		t.Errorf("points awards final: %+v, want 1 award with 10 pts", awardsFinal)
	}
}

// TestIntegration_QuestResetsAtLocalMidnight pins the return rule:
// MarkDone sets available_at to local midnight in the household's timezone,
// repeat_days days after the local day it was marked done, computed by quest_next_available_at.
// Approvals do not alter available_at.
func TestIntegration_QuestResetsAtLocalMidnight(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 4)
	ctx := context.Background()
	alice := registerVerified(t, pool, authSvc, mailer, "questmidnight").User.ID

	type family struct {
		id       uuid.UUID
		ann, ben app.MemberAction
	}
	newFamily := func(name, tz string) family {
		t.Helper()
		h, err := households.Create(ctx, alice, app.HouseholdInput{Name: name, Timezone: tz})
		if err != nil {
			t.Fatalf("create household %s: %v", name, err)
		}
		ann, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ann"}, "")
		if err != nil {
			t.Fatalf("create Ann: %v", err)
		}
		ben, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ben"}, "")
		if err != nil {
			t.Fatalf("create Ben: %v", err)
		}
		return family{h.ID, app.MemberAction{MemberID: ann.ID}, app.MemberAction{MemberID: ben.ID}}
	}
	newQuest := func(f family, days int64) uuid.UUID {
		t.Helper()
		quest, err := quests.Create(ctx, alice, f.id, app.QuestInput{Name: "Feed the cat", Points: num(1), RepeatDays: num(days)}, "")
		if err != nil {
			t.Fatalf("create quest: %v", err)
		}
		return quest.ID
	}
	get := func(f family, id uuid.UUID) app.QuestView {
		t.Helper()
		got, err := quests.Get(ctx, alice, f.id, id)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return got
	}
	doneAt := func(f family, id uuid.UUID, at time.Time) uuid.UUID {
		t.Helper()
		if _, err := quests.Accept(ctx, alice, f.id, id, f.ann); err != nil {
			t.Fatalf("accept: %v", err)
		}
		if _, err := quests.MarkDone(ctx, alice, f.id, id, f.ann); err != nil {
			t.Fatalf("mark done: %v", err)
		}
		var compID uuid.UUID
		if err := pool.QueryRow(ctx, `
			UPDATE quest_completions
			SET done_at = $2
			WHERE id = (SELECT id FROM quest_completions WHERE quest_id = $1 AND status = 'pending' ORDER BY done_at DESC, id DESC LIMIT 1)
			RETURNING id
		`, id, at).Scan(&compID); err != nil {
			t.Fatalf("update completion done_at: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE household_quests
			SET completed_at = $2,
			    available_at = quest_next_available_at($2, (SELECT timezone FROM households WHERE id = $3), repeat_days)
			WHERE id = $1
		`, id, at, f.id); err != nil {
			t.Fatalf("age available_at: %v", err)
		}
		return compID
	}
	approve := func(f family, compID uuid.UUID) app.Completion {
		t.Helper()
		comp, _, _, _, err := quests.Approve(ctx, alice, f.id, compID, "")
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		return comp
	}
	back := func(f family, days int64, at time.Time) time.Time {
		t.Helper()
		id := newQuest(f, days)
		compID := doneAt(f, id, at)
		approve(f, compID)
		got := get(f, id)
		if !got.AvailableAt.Valid || !got.CompletedAt.Valid {
			t.Fatalf("approved quest: %+v", got)
		}
		return got.AvailableAt.Time
	}
	chicagoTZ, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	chicago := newFamily("Chicago Family", "America/Chicago")

	t.Run("late approval keeps the done-day midnight", func(t *testing.T) {
		now := time.Now().In(chicagoTZ)
		yesterday20 := time.Date(now.Year(), now.Month(), now.Day()-1, 20, 0, 0, 0, chicagoTZ)
		todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, chicagoTZ)
		id := newQuest(chicago, 1)
		compID := doneAt(chicago, id, yesterday20)
		approve(chicago, compID)
		got := get(chicago, id)
		if !got.AvailableAt.Time.Equal(todayMidnight) || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("available_at %v, want %v and available", got.AvailableAt.Time, todayMidnight)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after the midnight: %v", err)
		}
		// Approved months after it was done: the done day still decides.
		at := time.Date(2025, 6, 2, 20, 0, 0, 0, chicagoTZ)
		want := time.Date(2025, 6, 3, 0, 0, 0, 0, chicagoTZ)
		if got := back(chicago, 1, at); !got.Equal(want) {
			t.Errorf("late approval: available_at %v, want %v", got, want)
		}
	})

	t.Run("N days from any time on the done day", func(t *testing.T) {
		// 2025-06-02 is a Monday.
		for _, days := range []int64{1, 3, 7} {
			want := time.Date(2025, 6, 2+int(days), 0, 0, 0, 0, chicagoTZ)
			for _, at := range []time.Time{
				time.Date(2025, 6, 2, 0, 0, 30, 0, chicagoTZ),
				time.Date(2025, 6, 2, 23, 0, 0, 0, chicagoTZ),
				time.Date(2025, 6, 2, 23, 59, 59, 0, chicagoTZ),
			} {
				got := back(chicago, days, at)
				if !got.Equal(want) {
					t.Errorf("repeat %d, done %v: available_at %v, want %v", days, at, got.In(chicagoTZ), want)
				}
				if wait := got.Sub(at); wait <= time.Duration(days-1)*24*time.Hour || wait > time.Duration(days)*24*time.Hour {
					t.Errorf("repeat %d, done %v: wait %v", days, at, wait)
				}
			}
		}
	})

	t.Run("each household uses its own midnight", func(t *testing.T) {
		utc := newFamily("UTC Family", "UTC")
		plain := newFamily("Default Family", "")
		if h, err := households.Get(ctx, alice, plain.id); err != nil || h.Timezone != "UTC" {
			t.Fatalf("default timezone: %q %v", h.Timezone, err)
		}
		// 03:00 UTC on June 3 is still 22:00 on June 2 in Chicago.
		at := time.Date(2025, 6, 3, 3, 0, 0, 0, time.UTC)
		cases := []struct {
			name string
			f    family
			want time.Time
		}{
			{"UTC", utc, time.Date(2025, 6, 4, 0, 0, 0, 0, time.UTC)},
			{"America/Chicago", chicago, time.Date(2025, 6, 3, 0, 0, 0, 0, chicagoTZ)},
			{"default", plain, time.Date(2025, 6, 4, 0, 0, 0, 0, time.UTC)},
		}
		for _, c := range cases {
			if got := back(c.f, 1, at); !got.Equal(c.want) {
				t.Errorf("%s: available_at %v, want %v", c.name, got, c.want)
			}
		}
	})

	t.Run("daylight-saving days", func(t *testing.T) {
		havana := newFamily("Havana Family", "America/Havana")
		cases := []struct {
			name string
			f    family
			at   time.Time
			want time.Time
		}{
			// Spring forward (02:00 is skipped): 00:00 CST.
			{"spring forward", chicago, time.Date(2025, 3, 8, 18, 0, 0, 0, time.UTC), time.Date(2025, 3, 9, 6, 0, 0, 0, time.UTC)},
			// Fall back (01:00 happens twice): the one 00:00, CDT.
			{"fall back", chicago, time.Date(2025, 11, 1, 17, 0, 0, 0, time.UTC), time.Date(2025, 11, 2, 5, 0, 0, 0, time.UTC)},
			// Havana skips 00:00 on 2025-03-09: the day starts at 01:00 CDT.
			{"skipped midnight", havana, time.Date(2025, 3, 8, 17, 0, 0, 0, time.UTC), time.Date(2025, 3, 9, 5, 0, 0, 0, time.UTC)},
		}
		for _, c := range cases {
			if got := back(c.f, 1, c.at); !got.Equal(c.want) {
				t.Errorf("%s: available_at %v, want %v", c.name, got.UTC(), c.want)
			}
		}
	})

	t.Run("accept is refused until the midnight", func(t *testing.T) {
		id := newQuest(chicago, 1)
		compID := doneAt(chicago, id, time.Now())
		approve(chicago, compID)
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() + interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatalf("set available_at: %v", err)
		}
		before := get(chicago, id)
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); !errors.Is(err, app.ErrQuestTaken) {
			t.Errorf("accept before the midnight: %v, want ErrQuestTaken", err)
		}
		if after := get(chicago, id); after.ClaimedBy != nil || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) ||
			!after.AvailableAt.Time.Equal(before.AvailableAt.Time) {
			t.Errorf("a refused accept changed the quest: %+v, was %+v", after, before)
		}
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatalf("set available_at: %v", err)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after the midnight: %v", err)
		}
	})

	t.Run("a quest never done is available", func(t *testing.T) {
		id := newQuest(chicago, 7)
		if got := get(chicago, id); got.AvailableAt.Valid || got.CompletedAt.Valid || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Fatalf("new quest: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Errorf("accept a new quest: %v", err)
		}
	})

	t.Run("cooldown starts at mark-done and cancels on rejection", func(t *testing.T) {
		// Done, sent back: cooldown cancelled, chore available immediately.
		id := newQuest(chicago, 3)
		compID := doneAt(chicago, id, time.Now())
		if _, err := quests.Reject(ctx, alice, chicago.id, compID, ""); err != nil {
			t.Fatalf("reject: %v", err)
		}
		got := get(chicago, id)
		if !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("after reject, quest should be available: %+v", got)
		}
		compID2 := doneAt(chicago, id, time.Now())
		if _, err := quests.Reject(ctx, alice, chicago.id, compID2, ""); err != nil {
			t.Fatalf("reject again: %v", err)
		}
		// Another member can accept right away
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after second reject: %v", err)
		}

		// Taken and put down (abandon before marking done).
		id = newQuest(chicago, 3)
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("accept: %v", err)
		}
		if _, err := quests.Abandon(ctx, alice, chicago.id, id, chicago.ann); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		got = get(chicago, id)
		if got.AvailableAt.Valid || got.CompletedAt.Valid || got.ClaimedBy != nil || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("after a plain abandon: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, chicago.id, id, chicago.ben); err != nil {
			t.Errorf("accept after abandon: %v", err)
		}

		// Marked done, then its holder is removed from the household.
		// A 1-day repeat marked done 48h ago has finished its cooldown, so the
		// released chore is back in the pool for the other member.
		f := newFamily("Release Family", "America/Chicago")
		id = newQuest(f, 1)
		_ = doneAt(f, id, time.Now().Add(-48*time.Hour))
		if err := familySvc.Remove(ctx, alice, f.id, f.ann.MemberID, ""); err != nil {
			t.Fatalf("remove Ann: %v", err)
		}
		got = get(f, id)
		if got.ClaimedBy != nil {
			t.Errorf("after release: %+v", got)
		}
		if _, err := quests.Accept(ctx, alice, f.id, id, f.ben); err != nil {
			t.Errorf("accept after release: %v", err)
		}

		// Approved once and back: a rejection of a second completion keeps the quest available or cancels its cooldown
		id = newQuest(chicago, 1)
		cEarliest := doneAt(chicago, id, time.Now().Add(-72*time.Hour))
		approve(chicago, cEarliest)
		earned := get(chicago, id)
		cLatest := doneAt(chicago, id, time.Now())
		if _, err := quests.Reject(ctx, alice, chicago.id, cLatest, ""); err != nil {
			t.Fatalf("reject: %v", err)
		}
		got = get(chicago, id)
		if !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
			t.Errorf("reject latest completion should make quest available: %+v, was %+v", got, earned)
		}
	})
}
