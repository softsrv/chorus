//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// TestIntegration_PointsHistory verifies CLM-4:
// - A new member with no awards gets an empty, non-nil slice (len 0), not an error.
// - Three sequential quest approvals produce history entries ordered newest first (created_at DESC, id DESC).
// - Each entry carries matching earned_at (date), points, and reason.
// - Limit and offset paging is honoured.
func TestIntegration_PointsHistory(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 4)
	ctx := context.Background()

	parent := registerVerified(t, pool, authSvc, mailer, "pointshistory").User.ID
	h, err := households.Create(ctx, parent, app.HouseholdInput{Name: "Points History Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	member, err := familySvc.Create(ctx, parent, h.ID, app.FamilyMemberInput{Name: "Sam", PIN: "5678"}, "")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	// 1. Member with no awards gets an empty non-nil slice, not an error.
	history, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 50, 0)
	if err != nil {
		t.Fatalf("PointsHistory for member with no awards returned error: %v", err)
	}
	if history == nil {
		t.Fatal("PointsHistory returned nil slice, want non-nil empty slice")
	}
	if len(history) != 0 {
		t.Fatalf("PointsHistory returned %d awards, want 0", len(history))
	}

	// 2. Approve three quests sequentially.
	var questNames = []string{"Tidy room", "Walk dog", "Do homework"}
	var questPoints = []int32{5, 10, 15}
	asSam := app.MemberAction{MemberID: member.ID, PIN: "5678"}

	var approvalTimes []time.Time
	for i := range 3 {
		questRow, err := quests.Create(ctx, parent, h.ID, app.QuestInput{
			Name:       questNames[i],
			Points:     num(int64(questPoints[i])),
			RepeatDays: num(1),
		}, "")
		if err != nil {
			t.Fatalf("create quest %d: %v", i, err)
		}

		if _, err := quests.Accept(ctx, parent, h.ID, questRow.ID, asSam); err != nil {
			t.Fatalf("accept quest %d: %v", i, err)
		}
		if _, err := quests.MarkDone(ctx, parent, h.ID, questRow.ID, asSam); err != nil {
			t.Fatalf("mark done quest %d: %v", i, err)
		}

		pendingRows, err := quests.ListPending(ctx, parent, h.ID, "")
		if err != nil || len(pendingRows) != 1 {
			t.Fatalf("pending for quest %d: %+v %v", i, pendingRows, err)
		}
		appr, _, _, _, err := quests.Approve(ctx, parent, h.ID, pendingRows[0].ID, "")
		if err != nil {
			t.Fatalf("approve quest %d: %v", i, err)
		}
		if appr.ApprovedAt == nil {
			t.Fatalf("quest %d approved_at is not set", i)
		}
		// History is earned at the completion's approval time (CLM-3).
		approvalTimes = append(approvalTimes, *appr.ApprovedAt)
		// Small pause to guarantee distinct created_at / completed_at timestamps if required
		time.Sleep(10 * time.Millisecond)
	}

	// 3. Query all awards (default limit/offset)
	allAwards, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 50, 0)
	if err != nil {
		t.Fatalf("PointsHistory query failed: %v", err)
	}
	if len(allAwards) != 3 {
		t.Fatalf("expected 3 awards, got %d: %+v", len(allAwards), allAwards)
	}

	// Assert newest first: index 0 should be quest 2 ("Do homework"), index 1 quest 1 ("Walk dog"), index 2 quest 0 ("Tidy room")
	for i := range 3 {
		expectedIdx := 2 - i
		got := allAwards[i]
		wantName := questNames[expectedIdx]
		wantPts := questPoints[expectedIdx]
		wantTime := approvalTimes[expectedIdx]

		if got.Reason != wantName {
			t.Errorf("award %d Reason mismatch: got %q, want %q", i, got.Reason, wantName)
		}
		if got.Points != wantPts {
			t.Errorf("award %d Points mismatch: got %d, want %d", i, got.Points, wantPts)
		}
		if got.Kind != "quest" {
			t.Errorf("award %d Kind mismatch: got %q, want 'quest'", i, got.Kind)
		}
		if !got.EarnedAt.Equal(wantTime) {
			t.Errorf("award %d EarnedAt mismatch: got %v, want %v", i, got.EarnedAt, wantTime)
		}
	}

	// Verify ordering by CreatedAt DESC
	if allAwards[0].CreatedAt.Before(allAwards[1].CreatedAt) || allAwards[1].CreatedAt.Before(allAwards[2].CreatedAt) {
		t.Errorf("expected awards ordered newest first: %v, %v, %v",
			allAwards[0].CreatedAt, allAwards[1].CreatedAt, allAwards[2].CreatedAt)
	}

	// 4. Test paging (limit / offset)
	// Page 1: limit 2, offset 0 -> returns first 2 awards (indices 0 and 1)
	page1, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 2, 0)
	if err != nil {
		t.Fatalf("page 1 failed: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1 len got %d, want 2", len(page1))
	}
	if page1[0].ID != allAwards[0].ID || page1[1].ID != allAwards[1].ID {
		t.Errorf("page 1 items do not match first 2 awards: %+v vs %+v", page1, allAwards[:2])
	}

	// Page 2: limit 2, offset 2 -> returns remaining 1 award (index 2)
	page2, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 2, 2)
	if err != nil {
		t.Fatalf("page 2 failed: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page 2 len got %d, want 1", len(page2))
	}
	if page2[0].ID != allAwards[2].ID {
		t.Errorf("page 2 item does not match third award: %+v vs %+v", page2[0], allAwards[2])
	}

	// Page 3: limit 2, offset 4 -> returns empty non-nil slice
	page3, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 2, 4)
	if err != nil {
		t.Fatalf("page 3 failed: %v", err)
	}
	if page3 == nil || len(page3) != 0 {
		t.Errorf("page 3 expected empty non-nil slice, got %+v", page3)
	}
}

// streakWorld is one household (America/Chicago) with an administrator and
// one family member (PIN 1234), driven through the real QuestService.
type streakWorld struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	q        *db.Queries
	quests   *app.QuestService
	admin    uuid.UUID
	hID      uuid.UUID
	memberID uuid.UUID
	loc      *time.Location
}

func newStreakWorld(t *testing.T, prefix string) *streakWorld {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	ctx := context.Background()
	admin := registerVerified(t, pool, authSvc, mailer, prefix).User.ID
	h, err := app.NewHouseholdService(q, pool).Create(ctx, admin, app.HouseholdInput{Name: "Streak Family", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	member, err := app.NewFamilyService(q, pool, 4).Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Maya", PIN: "1234"}, "")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	return &streakWorld{t: t, ctx: ctx, pool: pool, q: q, quests: app.NewQuestService(q, pool),
		admin: admin, hID: h.ID, memberID: member.ID, loc: loc}
}

// quest creates a household quest through the service.
func (w *streakWorld) quest(name string, points, repeatDays, streakBonus int64) uuid.UUID {
	w.t.Helper()
	v, err := w.quests.Create(w.ctx, w.admin, w.hID, app.QuestInput{
		Name: name, Points: num(points), RepeatDays: num(repeatDays), StreakBonus: num(streakBonus),
	}, "")
	if err != nil {
		w.t.Fatalf("create quest %s: %v", name, err)
	}
	return v.ID
}

// markDone makes the quest available (its cooldown is not under test here),
// has the member accept it and mark it done, and returns the new pending
// completion's id.
func (w *streakWorld) markDone(questID uuid.UUID) uuid.UUID {
	w.t.Helper()
	if _, err := w.pool.Exec(w.ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, questID); err != nil {
		w.t.Fatalf("make quest available: %v", err)
	}
	act := app.MemberAction{MemberID: w.memberID, PIN: "1234"}
	if _, err := w.quests.Accept(w.ctx, w.admin, w.hID, questID, act); err != nil {
		w.t.Fatalf("accept: %v", err)
	}
	if _, err := w.quests.MarkDone(w.ctx, w.admin, w.hID, questID, act); err != nil {
		w.t.Fatalf("mark done: %v", err)
	}
	var id uuid.UUID
	if err := w.pool.QueryRow(w.ctx, `SELECT id FROM quest_completions WHERE quest_id = $1 AND status = 'pending'
		ORDER BY done_at DESC, id DESC LIMIT 1`, questID).Scan(&id); err != nil {
		w.t.Fatalf("read new completion: %v", err)
	}
	return id
}

// streak reads the member's streak row for the quest; false when none.
func (w *streakWorld) streak(questID uuid.UUID) (db.MemberQuestStreak, bool) {
	w.t.Helper()
	s, err := w.q.GetMemberQuestStreak(w.ctx, db.GetMemberQuestStreakParams{HouseholdID: w.hID, MemberID: w.memberID, QuestID: questID})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false
	}
	if err != nil {
		w.t.Fatalf("read streak: %v", err)
	}
	return s, true
}

func (w *streakWorld) count(questID uuid.UUID) int32 {
	w.t.Helper()
	s, ok := w.streak(questID)
	if !ok {
		w.t.Fatalf("no streak row for quest %s", questID)
	}
	return s.Count
}

func (w *streakWorld) points() int32 {
	w.t.Helper()
	var p int32
	if err := w.pool.QueryRow(w.ctx, `SELECT points FROM family_members WHERE household_id = $1 AND id = $2`, w.hID, w.memberID).Scan(&p); err != nil {
		w.t.Fatalf("read points: %v", err)
	}
	return p
}

func (w *streakWorld) awards() []db.PointsAward {
	w.t.Helper()
	a, err := w.q.ListMemberPointsAwards(w.ctx, db.ListMemberPointsAwardsParams{HouseholdID: w.hID, MemberID: w.memberID, Limit: 1000})
	if err != nil {
		w.t.Fatalf("list awards: %v", err)
	}
	return a
}

// awardsWithReason counts the member's history entries of kind with reason.
func (w *streakWorld) awardsWithReason(kind, reason string) []db.PointsAward {
	w.t.Helper()
	var out []db.PointsAward
	for _, a := range w.awards() {
		if a.Kind == kind && a.Reason == reason {
			out = append(out, a)
		}
	}
	return out
}

// approve approves the completion and checks the member's points rose by
// wantDelta and the returned streak is want.
func (w *streakWorld) approve(label string, completionID uuid.UUID, wantDelta int32, want app.StreakBonus) {
	w.t.Helper()
	before := w.points()
	_, m, _, members, err := w.quests.Approve(w.ctx, w.admin, w.hID, completionID, "")
	if err != nil {
		w.t.Fatalf("%s: approve: %v", label, err)
	}
	// Only the marker's streak moves at mark-done, so it is on their entry.
	var streak app.StreakBonus
	for _, am := range members {
		if am.Member.ID == m.ID {
			streak = am.Streak
		}
	}
	if streak != want {
		w.t.Errorf("%s: streak %+v, want %+v", label, streak, want)
	}
	if got := w.points() - before; got != wantDelta || m.Points != before+wantDelta {
		w.t.Errorf("%s: points rose by %d (returned total %d), want +%d from %d", label, got, m.Points, wantDelta, before)
	}
}

// wantExpiry is local midnight in loc starting the day after 2*repeatDays
// days from the local date of doneAt.
func wantExpiry(doneAt time.Time, loc *time.Location, repeatDays int) time.Time {
	l := doneAt.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+2*repeatDays+1, 0, 0, 0, 0, loc)
}

// TestIntegration_QuestStreak drives quest streaks end to end against real
// Postgres: the expiry set at mark-done (CLM-6), counting up (CLM-7), the
// bonus from the 3rd in a row (CLM-9) with its own history entry (CLM-5),
// reset after expiry (CLM-8), and the streak SQL leaving the cooldown alone
// (CLM-17).
func TestIntegration_QuestStreak(t *testing.T) {
	w := newStreakWorld(t, "streak")
	dishes := w.quest("Dishes", 4, 1, 5)

	// CLM-6 / CLM-7: the first mark-done starts the streak at 1 and expires at
	// local midnight 3 days after the local mark-done date (any time the next
	// day or the day after still counts).
	c1 := w.markDone(dishes)
	s, ok := w.streak(dishes)
	if !ok || s.Count != 1 || s.PrevCount.Valid {
		t.Fatalf("after 1st mark-done: %+v (found %v), want count 1 and no previous step", s, ok)
	}
	if want := wantExpiry(s.LastDoneAt.Time, w.loc, 1); !s.ExpiresAt.Time.Equal(want) {
		t.Errorf("daily expires_at %v, want %v", s.ExpiresAt.Time.In(w.loc), want)
	}
	var completionDone time.Time
	if err := w.pool.QueryRow(w.ctx, `SELECT done_at FROM quest_completions WHERE id = $1`, c1).Scan(&completionDone); err != nil {
		t.Fatalf("read done_at: %v", err)
	}
	if !s.LastDoneAt.Time.Equal(completionDone) {
		t.Errorf("streak last_done_at %v, want the completion's done_at %v", s.LastDoneAt.Time, completionDone)
	}
	// A 7-day quest: 2*7+1 days.
	weekly := w.quest("Wash the car", 20, 7, 10)
	w.markDone(weekly)
	if ws, ok := w.streak(weekly); !ok || ws.Count != 1 || !ws.ExpiresAt.Time.Equal(wantExpiry(ws.LastDoneAt.Time, w.loc, 7)) {
		t.Errorf("weekly streak: %+v (found %v), want count 1 expiring %v", ws, ok, wantExpiry(ws.LastDoneAt.Time, w.loc, 7))
	}

	// CLM-17: mark-done's cooldown is exactly INT-0157's (local midnight
	// repeat_days days after the mark-done day), untouched by the streak
	// write, and the streak SQL never assigns available_at.
	var cooldownIntact bool
	if err := w.pool.QueryRow(w.ctx, `SELECT q.available_at = quest_next_available_at(q.completed_at, h.timezone, q.repeat_days)
		FROM household_quests q JOIN households h ON h.id = q.household_id WHERE q.id = $1`, dishes).Scan(&cooldownIntact); err != nil {
		t.Fatalf("read cooldown: %v", err)
	}
	if !cooldownIntact {
		t.Error("available_at after mark-done is not the mark-done cooldown")
	}
	sqlText, err := os.ReadFile("../../db/queries/quests.sql")
	if err != nil {
		t.Fatalf("read quests.sql: %v", err)
	}
	_, afterStreaked, found := strings.Cut(string(sqlText), "), streaked AS (")
	streakedCTE, _, foundEnd := strings.Cut(afterStreaked, "), recorded AS (")
	if !found || !foundEnd {
		t.Fatal("MarkQuestDone's streaked CTE not found in quests.sql")
	}
	if strings.Contains(streakedCTE, "available_at") || strings.Contains(streakedCTE, "household_quests") {
		t.Errorf("the streak upsert writes outside member_quest_streaks:\n%s", streakedCTE)
	}
	recomputeAt := strings.Index(string(sqlText), "-- name: RecomputeMemberQuestStreak")
	if recomputeAt < 0 {
		t.Fatal("RecomputeMemberQuestStreak not found in quests.sql")
	}
	recomputeText, _, _ := strings.Cut(string(sqlText)[recomputeAt+1:], "-- name: ")
	if strings.Contains(recomputeText, "available_at") || strings.Contains(recomputeText, "UPDATE household_quests") {
		t.Error("RecomputeMemberQuestStreak touches the quest or its cooldown")
	}

	// CLM-9 / CLM-7: deltas p, p, p+b, p+b over four approvals in a row.
	w.approve("1st", c1, 4, app.StreakBonus{Count: 1})
	c2 := w.markDone(dishes)
	if n := w.count(dishes); n != 2 {
		t.Fatalf("after 2nd mark-done: count %d, want 2", n)
	}
	w.approve("2nd", c2, 4, app.StreakBonus{Count: 2})
	c3 := w.markDone(dishes)
	s3, _ := w.streak(dishes)
	if s3.Count != 3 || s3.ExpiresAt.Time.Before(s.ExpiresAt.Time) {
		t.Fatalf("after 3rd mark-done: %+v, want count 3 and expiry not moved back", s3)
	}
	w.approve("3rd", c3, 9, app.StreakBonus{Points: 5, Count: 3})

	// CLM-5: the bonus has its own entry next to the quest's: kind streak,
	// reason "streak: Dishes 3", the bonus points, earned when marked done.
	streakRows := w.awardsWithReason("streak", "streak: Dishes 3")
	if len(streakRows) != 1 || streakRows[0].Points != 5 {
		t.Fatalf("streak history after 3rd approval: %+v, want one 5-point 'streak: Dishes 3'", streakRows)
	}
	var c3Approved time.Time
	if err := w.pool.QueryRow(w.ctx, `SELECT approved_at FROM quest_completions WHERE id = $1`, c3).Scan(&c3Approved); err != nil {
		t.Fatalf("read approved_at: %v", err)
	}
	questRows := 0
	for _, a := range w.awardsWithReason("quest", "Dishes") {
		if a.EarnedAt.Time.Equal(c3Approved) {
			questRows++
		}
	}
	if questRows != 1 || !streakRows[0].EarnedAt.Time.Equal(c3Approved) {
		t.Errorf("3rd approval: %d quest entries earned at %v, streak entry earned at %v; want 1 and the same time", questRows, c3Approved, streakRows[0].EarnedAt.Time)
	}

	c4 := w.markDone(dishes)
	w.approve("4th", c4, 9, app.StreakBonus{Points: 5, Count: 4})
	if got := len(w.awardsWithReason("streak", "streak: Dishes 4")); got != 1 {
		t.Errorf("'streak: Dishes 4' entries: %d, want 1", got)
	}

	// CLM-8: marked done after the expiry the count is back to 1, no bonus at
	// 1 or 2, and the bonus returns at 3.
	if _, err := w.pool.Exec(w.ctx, `UPDATE member_quest_streaks SET expires_at = NOW() - interval '1 minute'
		WHERE member_id = $1 AND quest_id = $2`, w.memberID, dishes); err != nil {
		t.Fatalf("expire streak: %v", err)
	}
	c5 := w.markDone(dishes)
	if n := w.count(dishes); n != 1 {
		t.Fatalf("mark-done after expiry: count %d, want 1", n)
	}
	w.approve("after expiry 1", c5, 4, app.StreakBonus{Count: 1})
	c6 := w.markDone(dishes)
	w.approve("after expiry 2", c6, 4, app.StreakBonus{Count: 2})
	if got := len(w.awardsWithReason("streak", "streak: Dishes 3")); got != 1 {
		t.Errorf("a bonus was paid before the new streak reached 3: %d 'streak: Dishes 3' entries", got)
	}
	c7 := w.markDone(dishes)
	w.approve("after expiry 3", c7, 9, app.StreakBonus{Points: 5, Count: 3})
	if got := len(w.awardsWithReason("streak", "streak: Dishes 3")); got != 2 {
		t.Errorf("'streak: Dishes 3' entries after the new streak's 3rd: %d, want 2", got)
	}
}

// TestIntegration_QuestStreakZeroBonus: CLM-2, a quest with streak_bonus 0
// pays only its points at count 3 and 4 and writes no streak entry.
func TestIntegration_QuestStreakZeroBonus(t *testing.T) {
	w := newStreakWorld(t, "streakzero")
	sweep := w.quest("Sweep", 3, 1, 0)
	for i := int32(1); i <= 4; i++ {
		w.approve(fmt.Sprintf("sweep %d", i), w.markDone(sweep), 3, app.StreakBonus{Count: i})
	}
	for _, a := range w.awards() {
		if a.Kind == "streak" {
			t.Errorf("streak entry for a 0-bonus quest: %+v", a)
		}
	}
	if got := len(w.awards()); got != 4 {
		t.Errorf("history entries: %d, want 4 quest entries", got)
	}
}

// TestIntegration_QuestStreakRollback: CLM-3, a failure writing the streak
// bonus's history entry rolls the whole approval back.
func TestIntegration_QuestStreakRollback(t *testing.T) {
	w := newStreakWorld(t, "streakfault")
	faulty := w.quest("Faulty", 4, 1, 5)
	w.approve("faulty 1", w.markDone(faulty), 4, app.StreakBonus{Count: 1})
	w.approve("faulty 2", w.markDone(faulty), 4, app.StreakBonus{Count: 2})
	c3 := w.markDone(faulty)

	// The trigger fails only this member's streak entries, so nothing else
	// sharing the database is affected.
	name := "streak_fault_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := w.pool.Exec(w.ctx, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.kind = 'streak' AND NEW.member_id = '%[2]s'::uuid THEN
				RAISE EXCEPTION 'injected streak award failure';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER %[1]s BEFORE INSERT ON points_awards FOR EACH ROW EXECUTE FUNCTION %[1]s();`,
		name, w.memberID.String())); err != nil {
		t.Fatalf("install fault trigger: %v", err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := w.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON points_awards; DROP FUNCTION IF EXISTS %[1]s();`, name)); err != nil {
			t.Errorf("drop fault trigger: %v", err)
		}
	}
	t.Cleanup(drop)

	pointsBefore, awardsBefore := w.points(), len(w.awards())
	if _, _, _, _, err := w.quests.Approve(w.ctx, w.admin, w.hID, c3, ""); err == nil {
		t.Fatal("approve with a failing streak entry succeeded")
	}
	var status string
	if err := w.pool.QueryRow(w.ctx, `SELECT status FROM quest_completions WHERE id = $1`, c3).Scan(&status); err != nil || status != "pending" {
		t.Errorf("completion after failed approval: %q %v, want pending", status, err)
	}
	if p := w.points(); p != pointsBefore {
		t.Errorf("points after failed approval: %d, want %d", p, pointsBefore)
	}
	if n := len(w.awards()); n != awardsBefore {
		t.Errorf("history entries after failed approval: %d, want %d (no quest or streak entry)", n, awardsBefore)
	}
	if n := w.count(faulty); n != 3 {
		t.Errorf("streak after failed approval: %d, want 3", n)
	}

	// With the fault gone the same completion approves once, in full.
	drop()
	w.approve("faulty 3 retried", c3, 9, app.StreakBonus{Points: 5, Count: 3})
}

// TestIntegration_QuestStreakConcurrentApprove: CLM-4, concurrent approvals
// of a count-3 completion pay the quest points and the bonus once.
func TestIntegration_QuestStreakConcurrentApprove(t *testing.T) {
	w := newStreakWorld(t, "streakrace")
	race := w.quest("Race", 4, 1, 5)
	w.approve("race 1", w.markDone(race), 4, app.StreakBonus{Count: 1})
	w.approve("race 2", w.markDone(race), 4, app.StreakBonus{Count: 2})
	c3 := w.markDone(race)
	before := w.points()

	const n = 5
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _, _, errs[i] = w.quests.Approve(w.ctx, w.admin, w.hID, c3, "")
		}()
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, app.ErrNotFound):
		default:
			t.Errorf("concurrent approve: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("concurrent approvals that succeeded: %d, want 1", wins)
	}
	if got := w.points() - before; got != 9 {
		t.Errorf("points after concurrent approvals: +%d, want +9", got)
	}
	if got := len(w.awardsWithReason("streak", "streak: Race 3")); got != 1 {
		t.Errorf("streak entries after concurrent approvals: %d, want 1", got)
	}
}

// TestIntegration_QuestStreakLateApproval: CLM-10, the streak is fixed at
// mark-done: completions marked done on days D, D+1, D+2 and approved long
// after their streak expired still pay the bonus at 3, and every entry is
// earned at its approval time.
func TestIntegration_QuestStreakLateApproval(t *testing.T) {
	w := newStreakWorld(t, "streaklate")
	cat := w.quest("Feed the cat", 3, 1, 5)
	now := time.Now().In(w.loc)
	var ids []uuid.UUID
	var marked []time.Time
	for i := range 3 {
		id := w.markDone(cat)
		doneAt := time.Date(now.Year(), now.Month(), now.Day()-10+i, 10, 0, 0, 0, w.loc)
		if _, err := w.pool.Exec(w.ctx, `UPDATE quest_completions SET done_at = $2 WHERE id = $1`, id, doneAt); err != nil {
			t.Fatalf("back-date completion: %v", err)
		}
		ids, marked = append(ids, id), append(marked, doneAt)
	}
	// Approval day D+10: the streak has long expired; approval must not
	// recompute it from now.
	if _, err := w.pool.Exec(w.ctx, `UPDATE member_quest_streaks SET expires_at = $3
		WHERE member_id = $1 AND quest_id = $2`, w.memberID, cat, marked[2].Add(-time.Hour)); err != nil {
		t.Fatalf("expire streak: %v", err)
	}
	w.approve("D", ids[0], 3, app.StreakBonus{Count: 1})
	w.approve("D+1", ids[1], 3, app.StreakBonus{Count: 2})
	w.approve("D+2", ids[2], 8, app.StreakBonus{Points: 5, Count: 3})

	var approvedTimes []time.Time
	for _, id := range ids {
		var appAt time.Time
		if err := w.pool.QueryRow(w.ctx, `SELECT approved_at FROM quest_completions WHERE id = $1`, id).Scan(&appAt); err != nil {
			t.Fatalf("read approved_at: %v", err)
		}
		approvedTimes = append(approvedTimes, appAt)
	}

	questRows := w.awardsWithReason("quest", "Feed the cat")
	if len(questRows) != 3 {
		t.Fatalf("quest entries: %d, want 3", len(questRows))
	}
	for _, want := range approvedTimes {
		hits := 0
		for _, a := range questRows {
			if a.EarnedAt.Time.Equal(want) {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("quest entries earned at %v: %d, want 1", want, hits)
		}
	}
	streakRows := w.awardsWithReason("streak", "streak: Feed the cat 3")
	if len(streakRows) != 1 || !streakRows[0].EarnedAt.Time.Equal(approvedTimes[2]) {
		t.Errorf("streak entry: %+v, want one earned at %v", streakRows, approvedTimes[2])
	}
}

// TestIntegration_QuestStreakReject: CLM-6, sending a completion back
// recomputes the member's streak for the quest in the same transaction:
// (a) rejecting the latest completion puts count and expiry back to their
// values before it; (c) rejecting a member's only completion removes the
// streak; (b) rejecting the first of three pending completions on
// consecutive days, with later mark-dones after it, leaves the streak at 2
// and renumbers the remaining pending completions 1 and 2, so approving the
// last pays no bonus; (d) rejecting a pending completion after an earlier
// one paid its bonus leaves that bonus and its history entry; (e) a reject
// through another household is ErrNotFound and changes nothing.
func TestIntegration_QuestStreakReject(t *testing.T) {
	w := newStreakWorld(t, "streakreject")
	streakCount := func(id uuid.UUID) int32 {
		t.Helper()
		var n int32
		if err := w.pool.QueryRow(w.ctx, `SELECT streak_count FROM quest_completions WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read streak_count: %v", err)
		}
		return n
	}
	status := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := w.pool.QueryRow(w.ctx, `SELECT status FROM quest_completions WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read status: %v", err)
		}
		return s
	}

	// (a)
	trash := w.quest("Trash", 2, 1, 5)
	w.approve("trash 1", w.markDone(trash), 2, app.StreakBonus{Count: 1})
	w.approve("trash 2", w.markDone(trash), 2, app.StreakBonus{Count: 2})
	before, _ := w.streak(trash)
	c3 := w.markDone(trash)
	if n := w.count(trash); n != 3 {
		t.Fatalf("after 3rd mark-done: count %d, want 3", n)
	}
	if _, err := w.quests.Reject(w.ctx, w.admin, w.hID, c3, ""); err != nil {
		t.Fatalf("reject: %v", err)
	}
	after, ok := w.streak(trash)
	if !ok || after.Count != 2 || !after.ExpiresAt.Time.Equal(before.ExpiresAt.Time) || !after.LastDoneAt.Time.Equal(before.LastDoneAt.Time) {
		t.Errorf("after reject: %+v (found %v), want count 2, expires %v, last done %v", after, ok, before.ExpiresAt.Time, before.LastDoneAt.Time)
	}
	// The next mark-done continues the restored streak: 3, and pays.
	w.approve("trash 3 again", w.markDone(trash), 7, app.StreakBonus{Points: 5, Count: 3})

	// (c) Rejecting the completion that started a streak removes the streak.
	fresh := w.quest("Fresh", 2, 1, 5)
	first := w.markDone(fresh)
	if _, ok := w.streak(fresh); !ok {
		t.Fatal("no streak after first mark-done")
	}
	if _, err := w.quests.Reject(w.ctx, w.admin, w.hID, first, ""); err != nil {
		t.Fatalf("reject first: %v", err)
	}
	if s, ok := w.streak(fresh); ok {
		t.Errorf("streak after rejecting its first completion: %+v, want none", s)
	}

	// (b) Three pending completions marked done on consecutive days D, D+1,
	// D+2; rejecting D's (a step later mark-dones already built on) leaves
	// D+1 at 1 and D+2 at 2, the streak at 2 as of D+2.
	cat := w.quest("Feed the cat", 3, 1, 5)
	now := time.Now().In(w.loc)
	var pend []uuid.UUID
	var marked []time.Time
	for i := range 3 {
		id := w.markDone(cat)
		doneAt := time.Date(now.Year(), now.Month(), now.Day()-3+i, 10, 0, 0, 0, w.loc)
		if _, err := w.pool.Exec(w.ctx, `UPDATE quest_completions SET done_at = $2 WHERE id = $1`, id, doneAt); err != nil {
			t.Fatalf("back-date completion: %v", err)
		}
		pend, marked = append(pend, id), append(marked, doneAt)
	}
	if n := streakCount(pend[2]); n != 3 {
		t.Fatalf("3rd pending completion's streak_count %d, want 3", n)
	}
	if _, err := w.quests.Reject(w.ctx, w.admin, w.hID, pend[0], ""); err != nil {
		t.Fatalf("reject D: %v", err)
	}
	cs, ok := w.streak(cat)
	if !ok || cs.Count != 2 || !cs.LastDoneAt.Time.Equal(marked[2]) || !cs.ExpiresAt.Time.Equal(wantExpiry(marked[2], w.loc, 1)) {
		t.Errorf("after rejecting D: %+v (found %v), want count 2, last done %v, expires %v", cs, ok, marked[2], wantExpiry(marked[2], w.loc, 1))
	}
	if a, b := streakCount(pend[1]), streakCount(pend[2]); a != 1 || b != 2 {
		t.Errorf("remaining completions' streak_count %d and %d, want 1 and 2", a, b)
	}
	w.approve("D+1", pend[1], 3, app.StreakBonus{Count: 1})
	w.approve("D+2", pend[2], 3, app.StreakBonus{Count: 2})
	if got := len(w.awardsWithReason("streak", "streak: Feed the cat 3")); got != 0 {
		t.Errorf("streak entries after the renumbered approvals: %d, want 0", got)
	}

	// (d) An approved completion that paid its bonus keeps it, its history
	// entry and its streak_count when a later pending one is rejected.
	mop := w.quest("Mop", 4, 1, 5)
	var paid []uuid.UUID
	for i := int32(1); i <= 3; i++ {
		id := w.markDone(mop)
		paid = append(paid, id)
		want := app.StreakBonus{Count: i}
		delta := int32(4)
		if i == 3 {
			want, delta = app.StreakBonus{Points: 5, Count: 3}, 9
		}
		w.approve(fmt.Sprintf("mop %d", i), id, delta, want)
	}
	c4, c5 := w.markDone(mop), w.markDone(mop)
	pointsBefore := w.points()
	if _, err := w.quests.Reject(w.ctx, w.admin, w.hID, c4, ""); err != nil {
		t.Fatalf("reject mop 4: %v", err)
	}
	if p := w.points(); p != pointsBefore {
		t.Errorf("points after reject: %d, want %d", p, pointsBefore)
	}
	if rows := w.awardsWithReason("streak", "streak: Mop 3"); len(rows) != 1 || rows[0].Points != 5 {
		t.Errorf("paid bonus entry after reject: %+v, want one 5-point 'streak: Mop 3'", rows)
	}
	if n := streakCount(paid[2]); n != 3 || status(paid[2]) != "approved" {
		t.Errorf("approved completion after reject: streak_count %d status %q, want 3 approved", n, status(paid[2]))
	}
	if n := streakCount(c5); n != 4 {
		t.Errorf("later pending completion's streak_count %d, want 4", n)
	}
	if n := w.count(mop); n != 4 {
		t.Errorf("streak after rejecting mop 4: count %d, want 4", n)
	}
	w.approve("mop 5", c5, 9, app.StreakBonus{Points: 5, Count: 4})

	// (e) Another household's admin rejecting this household's completion
	// through their own household id gets ErrNotFound; nothing changes.
	other := newStreakWorld(t, "streakrejectother")
	victim := w.markDone(mop)
	foreignBefore, _ := w.streak(mop)
	if _, err := w.quests.Reject(w.ctx, other.admin, other.hID, victim, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("reject through another household: %v, want ErrNotFound", err)
	}
	if _, err := w.quests.Reject(w.ctx, other.admin, w.hID, victim, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("reject by a non-member: %v, want ErrNotFound", err)
	}
	if s := status(victim); s != "pending" {
		t.Errorf("completion after foreign rejects: %q, want pending", s)
	}
	if after, ok := w.streak(mop); !ok || after.Count != foreignBefore.Count || !after.ExpiresAt.Time.Equal(foreignBefore.ExpiresAt.Time) {
		t.Errorf("streak after foreign rejects: %+v (found %v), want %+v", after, ok, foreignBefore)
	}
}

// TestIntegration_QuestStreakExistingQuests: CLM-15, a quest row written
// without streak_bonus (as every quest before the migration) gets 0 and
// works as before, and a completion marked done before streaks existed is
// approved with its points and no streak.
func TestIntegration_QuestStreakExistingQuests(t *testing.T) {
	w := newStreakWorld(t, "streaklegacy")
	legacy := uuid.New()
	if _, err := w.pool.Exec(w.ctx, `INSERT INTO household_quests (id, household_id, name, points, repeat_days, notes, created_at, updated_at)
		VALUES ($1, $2, 'Legacy', 6, 1, '', NOW(), NOW())`, legacy, w.hID); err != nil {
		t.Fatalf("seed legacy quest: %v", err)
	}
	var bonus int32
	if err := w.pool.QueryRow(w.ctx, `SELECT streak_bonus FROM household_quests WHERE id = $1`, legacy).Scan(&bonus); err != nil || bonus != 0 {
		t.Fatalf("legacy streak_bonus: %d %v, want 0", bonus, err)
	}
	// A completion from before streaks: no streak_count.
	old := uuid.New()
	if _, err := w.pool.Exec(w.ctx, `INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status)
		VALUES ($1, $2, $3, $4, NOW() - interval '1 hour', 'pending')`, old, w.hID, legacy, w.memberID); err != nil {
		t.Fatalf("seed legacy completion: %v", err)
	}
	w.approve("pre-streak completion", old, 6, app.StreakBonus{})
	for i := int32(1); i <= 3; i++ {
		w.approve(fmt.Sprintf("legacy %d", i), w.markDone(legacy), 6, app.StreakBonus{Count: i})
	}
	if got := len(w.awards()); got != 4 {
		t.Errorf("history entries: %d, want 4 quest entries and no streak entry", got)
	}
}
