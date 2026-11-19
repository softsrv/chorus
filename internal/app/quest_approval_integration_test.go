//go:build integration

package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_QuestApproval walks a quest through its new lifecycle:
// a member accepts, abandons, another takes it, marks it done (releasing the
// holder and starting cooldown), the admin rejects it (cancelling cooldown)
// and approves it. Points move only on approval.
func TestIntegration_QuestApproval(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 4)
	combos := app.NewComboService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "questapprove").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Approval Family", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	ann, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ann", PIN: "1111"}, "")
	if err != nil {
		t.Fatalf("create Ann: %v", err)
	}
	ben, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Ben"}, "")
	if err != nil {
		t.Fatalf("create Ben: %v", err)
	}
	asAnn := app.MemberAction{MemberID: ann.ID, PIN: "1111"}
	asBen := app.MemberAction{MemberID: ben.ID}
	quest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Mow the lawn", Points: num(7), RepeatDays: num(3)}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	points := func(memberID uuid.UUID) int32 {
		t.Helper()
		m, err := q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: h.ID, ID: memberID})
		if err != nil {
			t.Fatalf("get member: %v", err)
		}
		return m.Points
	}
	get := func() app.QuestView {
		t.Helper()
		got, err := quests.Get(ctx, alice, h.ID, quest.ID)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		return got
	}
	pending := func(adminPin string) []app.PendingCompletion {
		t.Helper()
		rows, err := quests.ListPending(ctx, alice, h.ID, adminPin)
		if err != nil || rows == nil {
			t.Fatalf("list pending: %v %v", rows, err)
		}
		return rows
	}
	awards := func(memberID uuid.UUID) []db.PointsAward {
		t.Helper()
		rows, err := q.ListMemberPointsAwards(ctx, db.ListMemberPointsAwardsParams{HouseholdID: h.ID, MemberID: memberID, Limit: 100, Offset: 0})
		if err != nil {
			t.Fatalf("list points awards: %v", err)
		}
		return rows
	}

	// Declining is client-side: reading the quest writes nothing.
	if got := get(); got.ClaimedBy != nil || !app.QuestAvailable(got.HouseholdQuest, time.Now()) || got.AwaitingApproval {
		t.Fatalf("a read changed the quest: %+v", got)
	}
	if len(pending("")) != 0 {
		t.Error("a new household has quests waiting")
	}
	// Nobody holds it, so nobody can mark it done.
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asAnn); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mark done unclaimed: %v, want ErrNotFound", err)
	}

	// Ann accepts. Ben can neither abandon nor finish Ann's quest.
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("Ann accepts: %v", err)
	}
	if _, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Ben abandons Ann's quest: %v, want ErrNotFound", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Ben marks Ann's quest done: %v, want ErrNotFound", err)
	}
	if _, err := quests.Abandon(ctx, alice, h.ID, quest.ID, app.MemberAction{MemberID: ann.ID, PIN: "9999"}); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Ann abandons with a wrong PIN: %v, want ErrWrongPIN", err)
	}
	if got := get(); got.ClaimedBy == nil || *got.ClaimedBy != ann.ID {
		t.Errorf("after refused actions: claimed_by %v, want Ann %v", got.ClaimedBy, ann.ID)
	}

	// Ann abandons: the quest is back in the pool and Ben can take it.
	abandoned, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asAnn)
	if err != nil || abandoned.ClaimedBy != nil || abandoned.ClaimedAt.Valid || !app.QuestAvailable(abandoned.HouseholdQuest, time.Now()) {
		t.Fatalf("Ann abandons: %+v %v", abandoned, err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("Ben accepts after abandon: %v", err)
	}

	// Ben marks it done: it releases holder, starts cooldown, and records a pending completion.
	// No points move yet.
	doneView, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen)
	if err != nil || doneView.ClaimedBy != nil || doneView.ClaimedAt.Valid || !doneView.CompletedAt.Valid ||
		!doneView.AvailableAt.Valid || !doneView.AwaitingApproval {
		t.Fatalf("Ben marks done: %+v %v", doneView, err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after mark done: %d, want 0", p)
	}
	// Ben no longer holds it, so marking done again is ErrNotFound.
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mark done twice: %v, want ErrNotFound", err)
	}
	rows := pending("")
	if len(rows) != 1 || rows[0].QuestID != quest.ID || rows[0].MemberName != "Ben" ||
		rows[0].Points != 7 || rows[0].MemberID != ben.ID {
		t.Errorf("pending: %+v", rows)
	}
	compID := rows[0].ID
	// Marking done changes no status by an admin: status_by stays NULL.
	if st, by := completionStatusBy(t, pool, compID); st != "pending" || by != nil {
		t.Errorf("after mark done: status %q status_by %v, want pending NULL", st, by)
	}
	// Once marked done, the member no longer holds it, so abandon is ErrNotFound.
	if _, err := quests.Abandon(ctx, alice, h.ID, quest.ID, asBen); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("abandon after mark done: %v, want ErrNotFound", err)
	}

	// Sent back (Reject): rejects only this pending completion, no points.
	// Since this completion started the running cooldown, cooldown is cancelled (available_at <= NOW()).
	rejected, err := quests.Reject(ctx, alice, h.ID, compID, "")
	if err != nil || rejected.ID != compID || rejected.Status != "rejected" || rejected.QuestID != quest.ID {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if st, by := completionStatusBy(t, pool, compID); st != "rejected" || by == nil || *by != alice {
		t.Errorf("after reject: status %q status_by %v, want rejected by %v", st, by, alice)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after reject: %d, want 0", p)
	}
	if got := awards(ben.ID); len(got) != 0 {
		t.Errorf("points awards after reject: %+v, want none", got)
	}
	if len(pending("")) != 0 {
		t.Error("a rejected completion is still waiting")
	}
	if got := get(); !app.QuestAvailable(got.HouseholdQuest, time.Now()) || got.ClaimedBy != nil {
		t.Errorf("after reject, quest should be available immediately: %+v", got)
	}
	// Re-rejecting or approving a non-pending completion is ErrNotFound.
	if _, err := quests.Reject(ctx, alice, h.ID, compID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("reject completion not pending: %v, want ErrNotFound", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, compID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("approve completion not pending: %v, want ErrNotFound", err)
	}

	// Ben accepts again and marks done again.
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("accept after reject: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("mark done again after reject: %v", err)
	}
	rows = pending("")
	if len(rows) != 1 {
		t.Fatalf("pending after second mark done: %d rows", len(rows))
	}
	comp2ID := rows[0].ID

	// Back-date done_at a day before approval to prove history entry earned_at uses approved_at, not done_at (CLM-3).
	backDatedDoneAt := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if _, err := pool.Exec(ctx, `UPDATE quest_completions SET done_at = $2 WHERE id = $1`, comp2ID, backDatedDoneAt); err != nil {
		t.Fatalf("back-date completion: %v", err)
	}

	// With an admin PIN set, a missing or wrong PIN changes nothing.
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "2468", ""); err != nil {
		t.Fatalf("set admin pin: %v", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("approve without admin pin: %v, want ErrAdminPinRequired", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("approve with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if _, err := quests.Reject(ctx, alice, h.ID, comp2ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("reject with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if _, err := quests.Reject(ctx, alice, h.ID, comp2ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("reject without admin pin: %v, want ErrAdminPinRequired", err)
	}
	if _, err := quests.ListPending(ctx, alice, h.ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("pending with wrong admin pin: %v, want ErrWrongPIN", err)
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after refused approvals: %d, want 0", p)
	}

	// Transaction atomicity: A failure after completion update rolls back both:
	// a trigger makes the points credit fail, so completion is still pending with no points.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION test_fail_points() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'points credit failed'; END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fail_points() CASCADE`) })
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_fail_points BEFORE UPDATE OF points ON family_members
		FOR EACH ROW WHEN (NEW.id = '`+ben.ID.String()+`') EXECUTE FUNCTION test_fail_points()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "2468"); err == nil {
		t.Fatal("approve succeeded although the points credit failed")
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION test_fail_points() CASCADE`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if len(pending("2468")) != 1 {
		t.Error("completion was not kept pending after failed approval")
	}
	if p := points(ben.ID); p != 0 {
		t.Errorf("points after a failed approval: %d, want 0", p)
	}
	if got := awards(ben.ID); len(got) != 0 {
		t.Errorf("a failed approval left history entries: %+v", got)
	}

	// CLM-1: N goroutines approve one completion at once: exactly one credits points,
	// N-1 get ErrNotFound.
	const N = 5
	start := make(chan struct{})
	type result struct {
		comp   app.Completion
		member db.FamilyMember
		err    error
	}
	results := make(chan result, N)
	for range N {
		go func() {
			<-start
			c, m, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "2468")
			results <- result{c, m, err}
		}()
	}
	close(start)
	var wins, notFound int
	for range N {
		r := <-results
		switch {
		case r.err == nil:
			wins++
			if r.member.ID != ben.ID || r.member.Points != 7 {
				t.Errorf("credited member: %+v, want Ben with 7 points", r.member)
			}
			if r.comp.ID != comp2ID || r.comp.Status != "approved" || r.comp.ApprovedAt == nil {
				t.Errorf("approved completion: %+v", r.comp)
			}
		case errors.Is(r.err, app.ErrNotFound):
			notFound++
		default:
			t.Errorf("concurrent approve: %v", r.err)
		}
	}
	if wins != 1 || notFound != N-1 {
		t.Errorf("concurrent approvals: %d won, %d not found; want 1 and %d", wins, notFound, N-1)
	}
	if st, by := completionStatusBy(t, pool, comp2ID); st != "approved" || by == nil || *by != alice {
		t.Errorf("after approve: status %q status_by %v, want approved by %v", st, by, alice)
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("Ben's points after approval: %d, want 7", p)
	}
	// Approving again credits nothing more.
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, comp2ID, "2468"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("repeat approve: %v, want ErrNotFound", err)
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("Ben's points after repeat approve: %d, want 7", p)
	}
	// The concurrent and repeated approvals wrote exactly one history entry,
	// for the credited points, earned at the completion's approval time.
	benAwards := awards(ben.ID)
	if len(benAwards) != 1 {
		t.Fatalf("Ben's history after approval: %d entries, want 1: %+v", len(benAwards), benAwards)
	}
	var markedDoneAt, approvedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT done_at, approved_at FROM quest_completions WHERE id = $1`, comp2ID).Scan(&markedDoneAt, &approvedAt); err != nil {
		t.Fatalf("read completion timestamps: %v", err)
	}
	if approvedAt.Equal(markedDoneAt) {
		t.Fatalf("approved_at %v should differ from done_at %v", approvedAt, markedDoneAt)
	}
	if a := benAwards[0]; a.HouseholdID != h.ID || a.MemberID != ben.ID || a.Kind != "quest" || a.Reason != "Mow the lawn" ||
		a.Points != 7 || !a.EarnedAt.Valid || !a.EarnedAt.Time.Equal(approvedAt) || a.EarnedAt.Time.Equal(markedDoneAt) {
		t.Errorf("Ben's history entry: %+v, want quest 'Mow the lawn' 7 points earned at approved_at %v (not done_at %v)", a, approvedAt, markedDoneAt)
	}
	if len(pending("2468")) != 0 {
		t.Error("approved completion is still waiting")
	}

	// Clear admin PIN for multi-day and combo tests.
	if err := familySvc.ClearAdminPin(ctx, alice, h.ID, "2468"); err != nil {
		t.Fatalf("clear admin pin: %v", err)
	}

	// CLM-3 & CLM-4: Four rounds of accept + mark done on days 1, 2, 3, 4 with no approval
	// in between; each mark done adds a distinct pending completion; approving all 4 credits 4x.
	chicagoTZ, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	nowChicago := time.Now().In(chicagoTZ)
	var markedTimes []time.Time
	for day := 1; day <= 4; day++ {
		// Ensure quest is available to accept
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, quest.ID); err != nil {
			t.Fatalf("make quest available: %v", err)
		}
		if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
			t.Fatalf("round %d accept: %v", day, err)
		}
		doneAtTime := time.Date(nowChicago.Year(), nowChicago.Month(), nowChicago.Day()-(5-day), 10, 0, 0, 0, chicagoTZ)
		markedTimes = append(markedTimes, doneAtTime)

		if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); err != nil {
			t.Fatalf("round %d mark done: %v", day, err)
		}
		// Back-date this completion's done_at to day N
		if _, err := pool.Exec(ctx, `
			UPDATE quest_completions
			SET done_at = $2
			WHERE id = (SELECT id FROM quest_completions WHERE quest_id = $1 AND status = 'pending' ORDER BY done_at DESC, id DESC LIMIT 1)
		`, quest.ID, doneAtTime); err != nil {
			t.Fatalf("back-date completion: %v", err)
		}
	}
	// Assert CLM-3: 4 pending completions, distinct done_at, no points moved (still 7 from earlier approval).
	rows4 := pending("")
	if len(rows4) != 4 {
		t.Fatalf("pending completions count = %d, want 4", len(rows4))
	}
	doneAtSeen := map[string]bool{}
	for i, r := range rows4 {
		if doneAtSeen[r.DoneAt.String()] {
			t.Errorf("duplicate done_at in pending: %v", r.DoneAt)
		}
		doneAtSeen[r.DoneAt.String()] = true
		if i > 0 && !r.DoneAt.After(rows4[i-1].DoneAt) && !r.DoneAt.Equal(rows4[i-1].DoneAt) {
			t.Errorf("pending not ordered by done_at ASC: %v before %v", rows4[i-1].DoneAt, r.DoneAt)
		}
	}
	if p := points(ben.ID); p != 7 {
		t.Errorf("points during 4 pending completions: %d, want 7", p)
	}

	// Assert CLM-4: Admin approves all four: member gains 4 * 7 = 28 points (total 35),
	// each completion's done_at equals marked time, quest available_at/completed_at unchanged.
	beforeQuest := get()
	for _, r := range rows4 {
		approvedComp, mem, _, _, err := quests.Approve(ctx, alice, h.ID, r.ID, "")
		if err != nil {
			t.Fatalf("approve completion %s: %v", r.ID, err)
		}
		if approvedComp.Status != "approved" || approvedComp.ApprovedAt == nil {
			t.Errorf("approved completion status: %+v", approvedComp)
		}
		if !approvedComp.DoneAt.Equal(r.DoneAt) {
			t.Errorf("completion done_at changed on approval: got %v, want %v", approvedComp.DoneAt, r.DoneAt)
		}
		var storedDoneAt time.Time
		if err := pool.QueryRow(ctx, `SELECT done_at FROM quest_completions WHERE id = $1`, r.ID).Scan(&storedDoneAt); err != nil || !storedDoneAt.Equal(r.DoneAt) {
			t.Errorf("stored done_at: %v %v, want %v", storedDoneAt, err, r.DoneAt)
		}
		_ = mem
	}
	if p := points(ben.ID); p != 35 {
		t.Errorf("points after approving all 4: %d, want 35", p)
	}
	afterQuest := get()
	if !afterQuest.AvailableAt.Time.Equal(beforeQuest.AvailableAt.Time) ||
		!afterQuest.CompletedAt.Time.Equal(beforeQuest.CompletedAt.Time) {
		t.Errorf("quest times changed by approval: before %v %v, after %v %v",
			beforeQuest.AvailableAt, beforeQuest.CompletedAt, afterQuest.AvailableAt, afterQuest.CompletedAt)
	}
	if len(pending("")) != 0 {
		t.Error("pending list not empty after all approved")
	}

	// CLM-16: a shareable quest with a teamwork bonus that was never shared
	// has one recorded member, who gets the full points and no teamwork bonus,
	// exactly like a quest that is not shareable.
	soloShareable, err := quests.Create(ctx, alice, h.ID, app.QuestInput{
		Name: "Wash the car", Points: num(100), Shareable: true, TeamworkBonus: num(20),
	}, "")
	if err != nil {
		t.Fatalf("create shareable quest: %v", err)
	}
	if !soloShareable.Shareable || soloShareable.TeamworkBonus != 20 {
		t.Fatalf("shareable quest stored as %+v", soloShareable.HouseholdQuest)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, soloShareable.ID, asBen); err != nil {
		t.Fatalf("accept shareable quest: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, soloShareable.ID, asBen); err != nil {
		t.Fatalf("mark shareable quest done: %v", err)
	}
	soloRows := pending("")
	if len(soloRows) != 1 || soloRows[0].QuestID != soloShareable.ID || soloRows[0].MemberID != ben.ID ||
		soloRows[0].MemberName != "Ben" || soloRows[0].Points != 100 {
		t.Fatalf("pending unshared shareable completion: %+v", soloRows)
	}
	if m := soloRows[0].Members; len(m) != 1 || m[0].MemberID != ben.ID || m[0].Name != "Ben" || m[0].Award != 100 ||
		soloRows[0].TeamworkBonus != 0 {
		t.Errorf("pending unshared shareable members: %+v bonus %d, want Ben alone with 100 and no bonus", m, soloRows[0].TeamworkBonus)
	}
	benBeforeSolo := points(ben.ID)
	annBeforeSolo := points(ann.ID)
	soloComp, soloMember, soloBonuses, soloApproved, err := quests.Approve(ctx, alice, h.ID, soloRows[0].ID, "")
	if err != nil {
		t.Fatalf("approve unshared shareable completion: %v", err)
	}
	if len(soloApproved) != 1 || soloApproved[0].Member.ID != ben.ID || soloApproved[0].PointsAwarded != 100 ||
		soloApproved[0].TeamworkBonus != 0 {
		t.Errorf("approved members of unshared shareable completion: %+v, want Ben alone with 100 and no bonus", soloApproved)
	}
	if soloComp.Status != "approved" || soloComp.MemberID == nil || *soloComp.MemberID != ben.ID {
		t.Errorf("approved unshared shareable completion: %+v", soloComp)
	}
	if len(soloBonuses) != 0 {
		t.Errorf("combo bonuses for unshared shareable completion: %+v, want none", soloBonuses)
	}
	// Full 100 points, not 100 + 20 and not a split.
	if soloMember.ID != ben.ID || soloMember.Points != benBeforeSolo+100 {
		t.Errorf("credited member: %+v, want Ben with %d points", soloMember, benBeforeSolo+100)
	}
	if p := points(ben.ID); p != benBeforeSolo+100 {
		t.Errorf("Ben's points after unshared shareable approval: %d, want %d", p, benBeforeSolo+100)
	}
	if p := points(ann.ID); p != annBeforeSolo {
		t.Errorf("Ann's points moved on Ben's approval: %d, want %d", p, annBeforeSolo)
	}
	if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, soloRows[0].ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("repeat approve unshared shareable: %v, want ErrNotFound", err)
	}
	if p := points(ben.ID); p != benBeforeSolo+100 {
		t.Errorf("Ben's points after repeat approve: %d, want %d", p, benBeforeSolo+100)
	}

	// The CLM-16 case above credited Ben 100 points; the reject checks below
	// compare against the balance the earlier approvals left (35).
	if _, err := pool.Exec(ctx, `UPDATE family_members SET points = 35 WHERE id = $1`, ben.ID); err != nil {
		t.Fatalf("reset Ben's balance after the unshared shareable case: %v", err)
	}

	// CLM-5: Reject latest vs earlier:
	// Two completions: reject earlier -> available_at unchanged.
	// Reject latest (quest.completed_at = done_at) -> available_at <= now, QuestAvailable true.
	// Make quest available first
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, quest.ID); err != nil {
		t.Fatalf("make quest available: %v", err)
	}
	// Completion 1 (earlier)
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("accept c1: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("mark done c1: %v", err)
	}
	c1Rows := pending("")
	c1ID := c1Rows[0].ID
	c1Time := time.Now().Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE quest_completions SET done_at = $2 WHERE id = $1`, c1ID, c1Time); err != nil {
		t.Fatalf("backdate c1: %v", err)
	}

	// Completion 2 (latest)
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, quest.ID); err != nil {
		t.Fatalf("make quest available for c2: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asBen); err != nil {
		t.Fatalf("accept c2: %v", err)
	}
	c2DoneView, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asBen)
	if err != nil {
		t.Fatalf("mark done c2: %v", err)
	}
	c2Rows := pending("")
	if len(c2Rows) != 2 {
		t.Fatalf("pending completions count = %d, want 2", len(c2Rows))
	}
	c2ID := c2Rows[1].ID // newer one
	futureAvail := c2DoneView.AvailableAt.Time
	if !futureAvail.After(time.Now()) {
		t.Fatalf("c2 available_at %v not in future", futureAvail)
	}

	// Reject earlier (c1): available_at unchanged!
	if _, err := quests.Reject(ctx, alice, h.ID, c1ID, ""); err != nil {
		t.Fatalf("reject c1: %v", err)
	}
	afterC1Reject := get()
	if !afterC1Reject.AvailableAt.Time.Equal(futureAvail) {
		t.Errorf("rejecting earlier completion changed available_at: got %v, want %v", afterC1Reject.AvailableAt.Time, futureAvail)
	}
	if app.QuestAvailable(afterC1Reject.HouseholdQuest, time.Now()) {
		t.Error("quest became available when earlier completion was rejected")
	}
	remPending := pending("")
	if len(remPending) != 1 || remPending[0].ID != c2ID {
		t.Errorf("remaining pending after c1 reject: %+v", remPending)
	}

	// Reject latest (c2): cancels running cooldown, available_at <= now, QuestAvailable true!
	if _, err := quests.Reject(ctx, alice, h.ID, c2ID, ""); err != nil {
		t.Fatalf("reject c2: %v", err)
	}
	afterC2Reject := get()
	if !app.QuestAvailable(afterC2Reject.HouseholdQuest, time.Now()) {
		t.Errorf("quest not available after latest completion rejected: available_at %v", afterC2Reject.AvailableAt)
	}
	if p := points(ben.ID); p != 35 {
		t.Errorf("points changed on reject: %d, want 35", p)
	}
	if len(pending("")) != 0 {
		t.Error("pending list not empty after c2 rejected")
	}
	if _, err := quests.Reject(ctx, alice, h.ID, c2ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("repeat reject c2: %v, want ErrNotFound", err)
	}

	// CLM-11: Combo bonus pays when chores completed on same household day are approved
	// (reading mark-done date from completion record).
	quest2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Dishes", Points: num(3), RepeatDays: num(1)}, "")
	if err != nil {
		t.Fatalf("create quest2: %v", err)
	}
	combo, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "Yard and Kitchen",
		BonusPoints: num(10),
		QuestIDs:    []uuid.UUID{quest.ID, quest2.ID},
	}, "")
	if err != nil {
		t.Fatalf("create combo: %v", err)
	}

	// Ann accepts and marks done both on same day
	if _, err := quests.Accept(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("Ann accept quest: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest.ID, asAnn); err != nil {
		t.Fatalf("Ann mark done quest: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, quest2.ID, asAnn); err != nil {
		t.Fatalf("Ann accept quest2: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, quest2.ID, asAnn); err != nil {
		t.Fatalf("Ann mark done quest2: %v", err)
	}
	annPending := pending("")
	if len(annPending) != 2 {
		t.Fatalf("Ann pending count = %d, want 2", len(annPending))
	}
	// Approve first completion: no combo bonus yet
	_, mem1, bonuses1, _, err := quests.Approve(ctx, alice, h.ID, annPending[0].ID, "")
	if err != nil || len(bonuses1) != 0 || mem1.Points != 7 {
		t.Errorf("first combo approval: mem points %d, bonuses %+v, err %v", mem1.Points, bonuses1, err)
	}
	// Approve second completion: pays combo bonus (10 points)!
	_, mem2, bonuses2, _, err := quests.Approve(ctx, alice, h.ID, annPending[1].ID, "")
	if err != nil || len(bonuses2) != 1 || bonuses2[0].ComboID != combo.ID || bonuses2[0].BonusPoints != 10 {
		t.Errorf("second combo approval: bonuses %+v, err %v", bonuses2, err)
	}
	if mem2.Points != 7+3+10 {
		t.Errorf("Ann's points after combo: %d, want 20", mem2.Points)
	}

	// Helper to accept, mark done, and back-date a completion to an explicit doneAt instant
	markDoneAt := func(questID uuid.UUID, action app.MemberAction, doneAt time.Time) uuid.UUID {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE household_quests SET available_at = NOW() - interval '1 minute' WHERE id = $1`, questID); err != nil {
			t.Fatalf("make quest available: %v", err)
		}
		if _, err := quests.Accept(ctx, alice, h.ID, questID, action); err != nil {
			t.Fatalf("accept quest %s: %v", questID, err)
		}
		if _, err := quests.MarkDone(ctx, alice, h.ID, questID, action); err != nil {
			t.Fatalf("mark done quest %s: %v", questID, err)
		}
		var compID uuid.UUID
		if err := pool.QueryRow(ctx, `
			UPDATE quest_completions
			SET done_at = $2
			WHERE id = (SELECT id FROM quest_completions WHERE quest_id = $1 AND status = 'pending' ORDER BY done_at DESC, id DESC LIMIT 1)
			RETURNING id
		`, questID, doneAt).Scan(&compID); err != nil {
			t.Fatalf("back-date completion %s: %v", compID, err)
		}
		return compID
	}

	// CLM-1: Two-chore combo, Ann marks both done, back-date both to same Chicago local day,
	// approve on different days: first approval pays chore points only, second pays points plus bonus once.
	t.Run("CLM-1: same local day approved on different days", func(t *testing.T) {
		annBase := points(ann.ID)
		q1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM1 Chores 1", Points: num(5), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q1: %v", err)
		}
		q2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM1 Chores 2", Points: num(8), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q2: %v", err)
		}
		c1, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
			Name:        "CLM1 Combo",
			BonusPoints: num(15),
			QuestIDs:    []uuid.UUID{q1.ID, q2.ID},
		}, "")
		if err != nil {
			t.Fatalf("create combo: %v", err)
		}

		jan15_10am := time.Date(2025, time.January, 15, 10, 0, 0, 0, chicagoTZ)
		jan15_2pm := time.Date(2025, time.January, 15, 14, 0, 0, 0, chicagoTZ)

		cID1 := markDoneAt(q1.ID, asAnn, jan15_10am)
		cID2 := markDoneAt(q2.ID, asAnn, jan15_2pm)

		// First approval (e.g. earlier): pays chore points only (5 pts), 0 bonus
		_, memFirst, bonusesFirst, _, err := quests.Approve(ctx, alice, h.ID, cID1, "")
		if err != nil {
			t.Fatalf("first approval: %v", err)
		}
		if len(bonusesFirst) != 0 {
			t.Errorf("first approval bonuses: got %+v, want none", bonusesFirst)
		}
		if memFirst.Points != annBase+5 || points(ann.ID) != annBase+5 {
			t.Errorf("ann points after first approval: got %d (stored %d), want %d", memFirst.Points, points(ann.ID), annBase+5)
		}

		// Second approval (e.g. later day): pays chore points (8 pts) + combo bonus (15 pts)
		_, memSecond, bonusesSecond, _, err := quests.Approve(ctx, alice, h.ID, cID2, "")
		if err != nil {
			t.Fatalf("second approval: %v", err)
		}
		if len(bonusesSecond) != 1 || bonusesSecond[0].ComboID != c1.ID || bonusesSecond[0].BonusPoints != 15 {
			t.Errorf("second approval bonuses: got %+v, want 1 bonus of 15", bonusesSecond)
		}
		wantPoints := annBase + 5 + 8 + 15
		if memSecond.Points != wantPoints || points(ann.ID) != wantPoints {
			t.Errorf("ann points after second approval: got %d (stored %d), want %d", memSecond.Points, points(ann.ID), wantPoints)
		}

		var awardCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, c1.ID).Scan(&awardCount); err != nil {
			t.Fatalf("query awards: %v", err)
		}
		if awardCount != 1 {
			t.Errorf("award count for c1 = %d, want 1", awardCount)
		}
	})

	// CLM-2: Chicago household timezone cases:
	// A) done on different local days, approved together -> no bonus
	// B) 1h apart across Chicago local midnight (same UTC date) -> no bonus
	// C) same Chicago local date but different UTC dates -> bonus
	t.Run("CLM-2: Chicago timezone boundaries", func(t *testing.T) {
		// Case A: done on different local days, approved together -> no bonus
		{
			annBase := points(ann.ID)
			qA1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2A Chores 1", Points: num(4), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qA1: %v", err)
			}
			qA2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2A Chores 2", Points: num(6), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qA2: %v", err)
			}
			cA, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
				Name:        "CLM2A Combo",
				BonusPoints: num(10),
				QuestIDs:    []uuid.UUID{qA1.ID, qA2.ID},
			}, "")
			if err != nil {
				t.Fatalf("create combo: %v", err)
			}

			day1 := time.Date(2025, time.January, 16, 12, 0, 0, 0, chicagoTZ)
			day2 := time.Date(2025, time.January, 17, 12, 0, 0, 0, chicagoTZ)
			cA_id1 := markDoneAt(qA1.ID, asAnn, day1)
			cA_id2 := markDoneAt(qA2.ID, asAnn, day2)

			_, _, b1, _, err := quests.Approve(ctx, alice, h.ID, cA_id1, "")
			if err != nil || len(b1) != 0 {
				t.Errorf("approve cA1: bonuses %+v, err %v", b1, err)
			}
			_, _, b2, _, err := quests.Approve(ctx, alice, h.ID, cA_id2, "")
			if err != nil || len(b2) != 0 {
				t.Errorf("approve cA2: bonuses %+v, err %v", b2, err)
			}
			if p := points(ann.ID); p != annBase+4+6 {
				t.Errorf("ann points after different local days: got %d, want %d", p, annBase+10)
			}
			var awardsA int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cA.ID).Scan(&awardsA); err != nil || awardsA != 0 {
				t.Errorf("awards for cA: got %d %v, want 0", awardsA, err)
			}
		}

		// Case B: 1h apart across Chicago local midnight (same UTC date) -> no bonus
		// 2025-01-11 05:30:00 UTC = 2025-01-10 23:30:00 Chicago (local Jan 10)
		// 2025-01-11 06:30:00 UTC = 2025-01-11 00:30:00 Chicago (local Jan 11)
		{
			annBase := points(ann.ID)
			qB1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2B Chores 1", Points: num(4), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qB1: %v", err)
			}
			qB2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2B Chores 2", Points: num(6), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qB2: %v", err)
			}
			cB, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
				Name:        "CLM2B Combo",
				BonusPoints: num(12),
				QuestIDs:    []uuid.UUID{qB1.ID, qB2.ID},
			}, "")
			if err != nil {
				t.Fatalf("create combo: %v", err)
			}

			t1 := time.Date(2025, time.January, 11, 5, 30, 0, 0, time.UTC)
			t2 := time.Date(2025, time.January, 11, 6, 30, 0, 0, time.UTC)
			if t1.UTC().Day() != t2.UTC().Day() {
				t.Fatalf("UTC days should match: %v vs %v", t1.UTC(), t2.UTC())
			}
			if t1.In(chicagoTZ).Day() == t2.In(chicagoTZ).Day() {
				t.Fatalf("Chicago local days should differ: %v vs %v", t1.In(chicagoTZ), t2.In(chicagoTZ))
			}

			cB_id1 := markDoneAt(qB1.ID, asAnn, t1)
			cB_id2 := markDoneAt(qB2.ID, asAnn, t2)

			_, _, b1, _, err := quests.Approve(ctx, alice, h.ID, cB_id1, "")
			if err != nil || len(b1) != 0 {
				t.Errorf("approve cB1: bonuses %+v, err %v", b1, err)
			}
			_, _, b2, _, err := quests.Approve(ctx, alice, h.ID, cB_id2, "")
			if err != nil || len(b2) != 0 {
				t.Errorf("approve cB2: bonuses %+v, err %v", b2, err)
			}
			if p := points(ann.ID); p != annBase+4+6 {
				t.Errorf("ann points after midnight straddle: got %d, want %d", p, annBase+10)
			}
			var awardsB int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cB.ID).Scan(&awardsB); err != nil || awardsB != 0 {
				t.Errorf("awards for cB: got %d %v, want 0", awardsB, err)
			}
		}

		// Case C: same Chicago local date but different UTC dates -> bonus
		// 2025-01-10 10:00:00 Chicago = 2025-01-10 16:00:00 UTC (Jan 10 UTC)
		// 2025-01-10 23:00:00 Chicago = 2025-01-11 05:00:00 UTC (Jan 11 UTC)
		{
			annBase := points(ann.ID)
			qC1, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2C Chores 1", Points: num(4), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qC1: %v", err)
			}
			qC2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM2C Chores 2", Points: num(6), RepeatDays: num(1)}, "")
			if err != nil {
				t.Fatalf("create qC2: %v", err)
			}
			cC, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
				Name:        "CLM2C Combo",
				BonusPoints: num(14),
				QuestIDs:    []uuid.UUID{qC1.ID, qC2.ID},
			}, "")
			if err != nil {
				t.Fatalf("create combo: %v", err)
			}

			tC1 := time.Date(2025, time.January, 10, 10, 0, 0, 0, chicagoTZ)
			tC2 := time.Date(2025, time.January, 10, 23, 0, 0, 0, chicagoTZ)
			if tC1.UTC().Day() == tC2.UTC().Day() {
				t.Fatalf("UTC days should differ: %v vs %v", tC1.UTC(), tC2.UTC())
			}
			if tC1.In(chicagoTZ).Day() != tC2.In(chicagoTZ).Day() {
				t.Fatalf("Chicago local days should match: %v vs %v", tC1.In(chicagoTZ), tC2.In(chicagoTZ))
			}

			cC_id1 := markDoneAt(qC1.ID, asAnn, tC1)
			cC_id2 := markDoneAt(qC2.ID, asAnn, tC2)

			_, _, b1, _, err := quests.Approve(ctx, alice, h.ID, cC_id1, "")
			if err != nil || len(b1) != 0 {
				t.Errorf("approve cC1: bonuses %+v, err %v", b1, err)
			}
			_, _, b2, _, err := quests.Approve(ctx, alice, h.ID, cC_id2, "")
			if err != nil || len(b2) != 1 || b2[0].ComboID != cC.ID || b2[0].BonusPoints != 14 {
				t.Errorf("approve cC2: bonuses %+v, err %v", b2, err)
			}
			wantPoints := annBase + 4 + 6 + 14
			if p := points(ann.ID); p != wantPoints {
				t.Errorf("ann points after same Chicago date / different UTC: got %d, want %d", p, wantPoints)
			}
			var awardsC int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cC.ID).Scan(&awardsC); err != nil || awardsC != 1 {
				t.Errorf("awards for cC: got %d %v, want 1", awardsC, err)
			}
		}
	})

	// CLM-3: Once-per-combo:
	// - After a combo pays, approving a later unrelated completion pays no second bonus
	// - Re-approving the paid completion returns app.ErrNotFound
	// - Two concurrent approvals of a full pending set pay exactly one bonus (sync.WaitGroup)
	// - A later new set completed later pays again
	t.Run("CLM-3: once per combo and concurrency", func(t *testing.T) {
		annBase := points(ann.ID)
		q3A, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Chores A", Points: num(10), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q3A: %v", err)
		}
		q3B, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Chores B", Points: num(15), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q3B: %v", err)
		}
		q3Solo, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Solo", Points: num(5), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create q3Solo: %v", err)
		}
		c3, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
			Name:        "CLM3 Combo",
			BonusPoints: num(20),
			QuestIDs:    []uuid.UUID{q3A.ID, q3B.ID},
		}, "")
		if err != nil {
			t.Fatalf("create combo: %v", err)
		}

		jan18 := time.Date(2025, time.January, 18, 10, 0, 0, 0, chicagoTZ)
		c3_idA := markDoneAt(q3A.ID, asAnn, jan18)
		c3_idB := markDoneAt(q3B.ID, asAnn, jan18.Add(time.Hour))

		// Approve A (no bonus), then B (pays combo bonus)
		_, _, bA, _, err := quests.Approve(ctx, alice, h.ID, c3_idA, "")
		if err != nil || len(bA) != 0 {
			t.Fatalf("approve c3A: bonuses %+v, err %v", bA, err)
		}
		_, _, bB, _, err := quests.Approve(ctx, alice, h.ID, c3_idB, "")
		if err != nil || len(bB) != 1 || bB[0].BonusPoints != 20 || bB[0].ComboID != c3.ID {
			t.Fatalf("approve c3B: bonuses %+v, err %v", bB, err)
		}
		if p := points(ann.ID); p != annBase+10+15+20 {
			t.Errorf("ann points after initial c3: got %d, want %d", p, annBase+45)
		}

		// 1. Approve a later unrelated completion: pays only its chore points, no bonus
		c3_idSolo := markDoneAt(q3Solo.ID, asAnn, jan18.Add(2*time.Hour))
		_, memSolo, bSolo, _, err := quests.Approve(ctx, alice, h.ID, c3_idSolo, "")
		if err != nil || len(bSolo) != 0 {
			t.Fatalf("approve solo: bonuses %+v, err %v", bSolo, err)
		}
		if memSolo.Points != annBase+45+5 || points(ann.ID) != annBase+50 {
			t.Errorf("ann points after solo: got %d (stored %d), want %d", memSolo.Points, points(ann.ID), annBase+50)
		}

		// 2. Re-approving the paid completion returns app.ErrNotFound
		if _, _, _, _, err := quests.Approve(ctx, alice, h.ID, c3_idB, ""); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("re-approve paid completion: got err %v, want app.ErrNotFound", err)
		}
		if p := points(ann.ID); p != annBase+50 {
			t.Errorf("points changed after re-approve attempt: got %d, want %d", p, annBase+50)
		}
		var awardsCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, c3.ID).Scan(&awardsCount); err != nil || awardsCount != 1 {
			t.Errorf("awards for c3: got %d %v, want 1", awardsCount, err)
		}

		// 3. Concurrent approvals of a full pending set pay exactly one bonus
		qConcA, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Conc A", Points: num(10), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create qConcA: %v", err)
		}
		qConcB, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "CLM3 Conc B", Points: num(12), RepeatDays: num(1)}, "")
		if err != nil {
			t.Fatalf("create qConcB: %v", err)
		}
		cConc, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
			Name:        "CLM3 Conc Combo",
			BonusPoints: num(25),
			QuestIDs:    []uuid.UUID{qConcA.ID, qConcB.ID},
		}, "")
		if err != nil {
			t.Fatalf("create cConc: %v", err)
		}

		jan19 := time.Date(2025, time.January, 19, 11, 0, 0, 0, chicagoTZ)
		cConcIDA := markDoneAt(qConcA.ID, asAnn, jan19)
		cConcIDB := markDoneAt(qConcB.ID, asAnn, jan19.Add(time.Minute))

		annBeforeConc := points(ann.ID)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		paidBonuses := make([][]app.ComboBonus, 2)
		start := make(chan struct{})
		for i, compID := range []uuid.UUID{cConcIDA, cConcIDB} {
			wg.Add(1)
			go func(idx int, cID uuid.UUID) {
				defer wg.Done()
				<-start
				_, _, b, _, err := quests.Approve(ctx, alice, h.ID, cID, "")
				errs[idx] = err
				paidBonuses[idx] = b
			}(i, compID)
		}
		close(start)
		wg.Wait()

		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("concurrent approvals failed: %v, %v", errs[0], errs[1])
		}
		totalBonuses := len(paidBonuses[0]) + len(paidBonuses[1])
		if totalBonuses != 1 {
			t.Fatalf("concurrent approvals total bonuses = %d, want 1", totalBonuses)
		}
		wantPointsConc := annBeforeConc + 10 + 12 + 25
		if p := points(ann.ID); p != wantPointsConc {
			t.Errorf("ann points after concurrent approvals: got %d, want %d", p, wantPointsConc)
		}
		var awardConcCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cConc.ID).Scan(&awardConcCount); err != nil {
			t.Fatalf("query cConc awards: %v", err)
		}
		if awardConcCount != 1 {
			t.Errorf("cConc award count = %d, want 1", awardConcCount)
		}

		// 4. A later new set pays again
		jan20 := time.Date(2025, time.January, 20, 10, 0, 0, 0, chicagoTZ)
		cNewIDA := markDoneAt(qConcA.ID, asAnn, jan20)
		cNewIDB := markDoneAt(qConcB.ID, asAnn, jan20.Add(time.Hour))

		annBeforeNew := points(ann.ID)
		_, _, bNewA, _, err := quests.Approve(ctx, alice, h.ID, cNewIDA, "")
		if err != nil || len(bNewA) != 0 {
			t.Fatalf("approve new set A: bonuses %+v, err %v", bNewA, err)
		}
		_, _, bNewB, _, err := quests.Approve(ctx, alice, h.ID, cNewIDB, "")
		if err != nil || len(bNewB) != 1 || bNewB[0].BonusPoints != 25 {
			t.Fatalf("approve new set B: bonuses %+v, err %v", bNewB, err)
		}
		wantPointsNew := annBeforeNew + 10 + 12 + 25
		if p := points(ann.ID); p != wantPointsNew {
			t.Errorf("ann points after later new set: got %d, want %d", p, wantPointsNew)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM combo_awards WHERE combo_id = $1`, cConc.ID).Scan(&awardConcCount); err != nil {
			t.Fatalf("query cConc awards round 2: %v", err)
		}
		if awardConcCount != 2 {
			t.Errorf("cConc award count after second set = %d, want 2", awardConcCount)
		}
	})
}
