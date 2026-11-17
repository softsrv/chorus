//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_HouseholdIsolation is the core authorization guarantee: a
// user who isn't a member of a household can't see or touch anything in it,
// and gets ErrNotFound (not ErrForbidden) so household IDs can't be probed.
func TestIntegration_HouseholdIsolation(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	rewards := app.NewRewardService(q)
	combos := app.NewComboService(q, pool)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "alice").User.ID
	mallory := registerVerified(t, pool, authSvc, mailer, "mallory").User.ID
	bobRes := registerVerified(t, pool, authSvc, mailer, "bob")
	bob := bobRes.User.ID // member, no Google connection

	// Alice connects Google; the stand-in knows only her access token.
	const aliceAccess, aliceRefresh = "alice-google-access-secret", "alice-google-refresh-secret"
	google := newCalendarStandIn(t, aliceAccess)
	google.cals["primary-alice@example.com"] = "Alice private"
	google.cals["family-y@group.calendar.google.com"] = "Family Y"
	google.events["family-y@group.calendar.google.com"] = []map[string]any{{"id": "y1", "summary": "Y soccer",
		"start": map[string]any{"dateTime": "2026-11-05T18:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-05T19:00:00Z"}}}
	google.events["primary-alice@example.com"] = []map[string]any{{"id": "x1", "summary": "Alice private appt",
		"start": map[string]any{"dateTime": "2026-11-06T18:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-06T19:00:00Z"}}}
	family := connectCalendar(t, pool, alice, aliceAccess, aliceRefresh, google)

	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Alice Family", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}
	aliceQuest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{
		Name:       "Dishes",
		Points:     app.NumberField{Value: 10, Set: true},
		RepeatDays: app.NumberField{Value: 1, Set: true},
		Notes:      "Wash and dry",
	}, "")
	if err != nil {
		t.Fatalf("Create quest: %v", err)
	}
	libraryItems, err := quests.ListLibrary(ctx)
	if err != nil || len(libraryItems) == 0 {
		t.Fatalf("ListLibrary: %v", err)
	}
	libraryItemID := libraryItems[0].ID
	aliceReward, err := rewards.Create(ctx, alice, h.ID, app.RewardInput{Description: "Movie night", Cost: num(300)}, "")
	if err != nil {
		t.Fatalf("Create reward: %v", err)
	}
	aliceQuest2, err := quests.Create(ctx, alice, h.ID, app.QuestInput{
		Name:       "Wipe counters",
		Points:     app.NumberField{Value: 5, Set: true},
		RepeatDays: app.NumberField{Value: 1, Set: true},
	}, "")
	if err != nil {
		t.Fatalf("Create quest 2: %v", err)
	}
	aliceCombo, err := combos.Create(ctx, alice, h.ID, app.ComboInput{
		Name:        "Kitchen Crew",
		BonusPoints: num(20),
		QuestIDs:    []uuid.UUID{aliceQuest.ID, aliceQuest2.ID},
	}, "")
	if err != nil {
		t.Fatalf("Create combo: %v", err)
	}
	if _, err := family.Set(ctx, alice, h.ID, "family-y@group.calendar.google.com"); err != nil {
		t.Fatalf("Set family calendar: %v", err)
	}
	aliceMember, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Alice Member", PIN: "1234"}, "")
	if err != nil {
		t.Fatalf("Create alice member: %v", err)
	}
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	// Mallory has her own household; IDs from it must not unlock Alice's data.
	mh, err := households.Create(ctx, mallory, app.HouseholdInput{Name: "Mallory"})
	if err != nil {
		t.Fatalf("Create mallory household: %v", err)
	}

	checks := map[string]error{}
	_, checks["get household"] = households.Get(ctx, mallory, h.ID)
	_, checks["update household"] = households.Update(ctx, mallory, h.ID, app.HouseholdInput{Name: "pwned"})
	checks["delete household"] = households.Delete(ctx, mallory, h.ID)
	_, _, checks["list quests"] = quests.List(ctx, mallory, h.ID, app.QuestPage{Limit: 10})
	_, checks["get quest"] = quests.Get(ctx, mallory, h.ID, aliceQuest.ID)
	_, checks["create quest"] = quests.Create(ctx, mallory, h.ID, app.QuestInput{Name: "Evil Quest"}, "")
	_, checks["update quest"] = quests.Update(ctx, mallory, h.ID, aliceQuest.ID, app.QuestInput{Name: "Pwned"}, "")
	checks["delete quest"] = quests.Delete(ctx, mallory, h.ID, aliceQuest.ID, "")
	_, checks["copy quest from library"] = quests.CopyFromLibrary(ctx, mallory, h.ID, libraryItemID, "")
	requestsBefore := len(google.recorded())
	_, checks["family calendar status"] = family.Status(ctx, mallory, h.ID)
	_, checks["family calendar entries"] = family.ListEntries(ctx, mallory, h.ID, from, to)
	_, checks["set family calendar"] = family.Set(ctx, mallory, h.ID, "family-y@group.calendar.google.com")
	checks["clear family calendar"] = family.Clear(ctx, mallory, h.ID)
	_, checks["tag family calendar entry"] = family.SetEntryTags(ctx, mallory, h.ID, "y1", []uuid.UUID{alice})
	// Cross-household ID smuggling: Mallory's household ID + Alice's quest ID.
	_, checks["get quest via own household"] = quests.Get(ctx, mallory, mh.ID, aliceQuest.ID)
	_, checks["update quest via own household"] = quests.Update(ctx, mallory, mh.ID, aliceQuest.ID, app.QuestInput{Name: "Smuggled"}, "")
	checks["delete quest via own household"] = quests.Delete(ctx, mallory, mh.ID, aliceQuest.ID, "")
	// Quest actions: an outsider cannot accept Alice's quest, and smuggling
	// Alice's member or quest id through Mallory's own household fails too.
	malloryMember, err := familySvc.Create(ctx, mallory, mh.ID, app.FamilyMemberInput{Name: "Mallory Member"}, "")
	if err != nil {
		t.Fatalf("Create mallory member: %v", err)
	}
	_, checks["accept quest"] = quests.Accept(ctx, mallory, h.ID, aliceQuest.ID, app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"})
	_, checks["accept quest as own member"] = quests.Accept(ctx, mallory, h.ID, aliceQuest.ID, app.MemberAction{MemberID: malloryMember.ID})
	_, checks["accept quest via own household with alice's member"] = quests.Accept(ctx, mallory, mh.ID, aliceQuest.ID, app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"})
	_, checks["accept quest via own household"] = quests.Accept(ctx, mallory, mh.ID, aliceQuest.ID, app.MemberAction{MemberID: malloryMember.ID})
	// Alice's member takes a second quest and marks it done, so an outsider
	// has a pending quest to try to approve, send back, abandon or list.
	alicePending, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Laundry", Points: num(4)}, "")
	if err != nil {
		t.Fatalf("Create pending quest: %v", err)
	}
	asAlice := app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"}
	if _, err := quests.Accept(ctx, alice, h.ID, alicePending.ID, asAlice); err != nil {
		t.Fatalf("Accept pending quest: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, alicePending.ID, asAlice); err != nil {
		t.Fatalf("MarkDone pending quest: %v", err)
	}
	pendingBefore, err := quests.Get(ctx, alice, h.ID, alicePending.ID)
	if err != nil {
		t.Fatalf("Get pending quest before outsider attempts: %v", err)
	}
	aliceCompletions, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil || len(aliceCompletions) != 1 {
		t.Fatalf("ListPending for Alice before outsider attempts: %v, len=%d", err, len(aliceCompletions))
	}
	aliceCompletionID := aliceCompletions[0].ID
	foreignCompletionID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new uuid: %v", err)
	}
	memberBefore, err := familySvc.Get(ctx, alice, h.ID, aliceMember.ID)
	if err != nil {
		t.Fatalf("Get alice member before outsider attempts: %v", err)
	}
	var aliceStreakCountBefore, aliceStreakRowsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(MAX(count), 0) FROM member_quest_streaks WHERE household_id = $1 AND member_id = $2 AND quest_id = $3`, h.ID, aliceMember.ID, alicePending.ID).Scan(&aliceStreakRowsBefore, &aliceStreakCountBefore); err != nil {
		t.Fatalf("get alice streak before outsider attempts: %v", err)
	}
	// Alice's own mark-done above started her streak, so the after-check below
	// compares against a real row and fails if an outsider moves it.
	if aliceStreakRowsBefore != 1 || aliceStreakCountBefore < 1 {
		t.Fatalf("alice streak before outsider attempts = %d rows, count %d; want 1 row with count >= 1", aliceStreakRowsBefore, aliceStreakCountBefore)
	}
	var malloryStreakRowsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM member_quest_streaks WHERE household_id = $1`, mh.ID).Scan(&malloryStreakRowsBefore); err != nil || malloryStreakRowsBefore != 0 {
		t.Fatalf("mallory streak rows before outsider attempts = %d, want 0: %v", malloryStreakRowsBefore, err)
	}
	_, checks["abandon quest"] = quests.Abandon(ctx, mallory, h.ID, alicePending.ID, asAlice)
	_, checks["mark quest done"] = quests.MarkDone(ctx, mallory, h.ID, alicePending.ID, asAlice)
	_, checks["mark quest done as own member"] = quests.MarkDone(ctx, mallory, h.ID, alicePending.ID, app.MemberAction{MemberID: malloryMember.ID})
	_, checks["list pending quests"] = quests.ListPending(ctx, mallory, h.ID, "")
	_, _, _, _, checks["approve quest"] = quests.Approve(ctx, mallory, h.ID, aliceCompletionID, "")
	_, checks["reject quest"] = quests.Reject(ctx, mallory, h.ID, aliceCompletionID, "")
	_, checks["abandon quest via own household"] = quests.Abandon(ctx, mallory, mh.ID, alicePending.ID, asAlice)
	_, checks["abandon quest via own household as own member"] = quests.Abandon(ctx, mallory, mh.ID, alicePending.ID, app.MemberAction{MemberID: malloryMember.ID})
	_, checks["mark quest done via own household"] = quests.MarkDone(ctx, mallory, mh.ID, alicePending.ID, asAlice)
	_, checks["mark quest done via own household as own member"] = quests.MarkDone(ctx, mallory, mh.ID, alicePending.ID, app.MemberAction{MemberID: malloryMember.ID})
	_, _, _, _, checks["approve quest via own household"] = quests.Approve(ctx, mallory, mh.ID, aliceCompletionID, "")
	_, checks["reject quest via own household"] = quests.Reject(ctx, mallory, mh.ID, aliceCompletionID, "")
	_, _, _, _, checks["approve foreign completion via own household"] = quests.Approve(ctx, mallory, mh.ID, foreignCompletionID, "")
	_, checks["reject foreign completion via own household"] = quests.Reject(ctx, mallory, mh.ID, foreignCompletionID, "")
	_, _, _, _, checks["approve foreign completion via alice household"] = quests.Approve(ctx, mallory, h.ID, foreignCompletionID, "")
	_, checks["reject foreign completion via alice household"] = quests.Reject(ctx, mallory, h.ID, foreignCompletionID, "")

	// CLM-9: Mallory's cross-household Approve/Reject/MarkDone attempts on Alice's pending quest
	// must not approve it, alter its completion, change available_at, or award points.
	pendingAfter, err := quests.Get(ctx, alice, h.ID, alicePending.ID)
	if err != nil {
		t.Fatalf("get pending quest after outsider approve attempts: %v", err)
	}
	if pendingAfter.ClaimedBy != nil {
		t.Errorf("alice pending quest was claimed: claimed_by %v", pendingAfter.ClaimedBy)
	}
	if !pendingAfter.AwaitingApproval {
		t.Errorf("alice pending quest not awaiting approval: %+v", pendingAfter)
	}
	if pendingAfter.AvailableAt.Valid != pendingBefore.AvailableAt.Valid ||
		(pendingBefore.AvailableAt.Valid && !pendingAfter.AvailableAt.Time.Equal(pendingBefore.AvailableAt.Time)) {
		t.Errorf("alice pending quest available_at changed: before %v, after %v", pendingBefore.AvailableAt, pendingAfter.AvailableAt)
	}
	// completed_at is the cooldown anchor (it equals the pending completion's
	// done_at); no outsider MarkDone, Approve or Reject may re-stamp it.
	if !pendingBefore.CompletedAt.Valid || pendingAfter.CompletedAt.Valid != pendingBefore.CompletedAt.Valid ||
		!pendingAfter.CompletedAt.Time.Equal(pendingBefore.CompletedAt.Time) {
		t.Errorf("alice pending quest completed_at changed: before %v, after %v", pendingBefore.CompletedAt, pendingAfter.CompletedAt)
	}
	memberAfter, err := familySvc.Get(ctx, alice, h.ID, aliceMember.ID)
	if err != nil {
		t.Fatalf("get alice member after outsider approve attempts: %v", err)
	}
	if memberAfter.Points != memberBefore.Points {
		t.Errorf("alice member points changed: before %d, after %d", memberBefore.Points, memberAfter.Points)
	}
	aliceCompletionsAfter, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil || len(aliceCompletionsAfter) != 1 || aliceCompletionsAfter[0].ID != aliceCompletionID {
		t.Errorf("alice pending completion touched by outsider: %+v, %v", aliceCompletionsAfter, err)
	}
	var compStatus string
	var compApprovedAt pgtype.Timestamptz
	var compDoneAt time.Time
	if err := pool.QueryRow(ctx, `SELECT status, approved_at, done_at FROM quest_completions WHERE id = $1 AND household_id = $2`, aliceCompletionID, h.ID).Scan(&compStatus, &compApprovedAt, &compDoneAt); err != nil || compStatus != "pending" || compApprovedAt.Valid {
		t.Errorf("alice completion in db = status %q approved_at %v, %v; want pending, no approved_at", compStatus, compApprovedAt, err)
	}
	if !compDoneAt.Equal(pendingBefore.CompletedAt.Time) {
		t.Errorf("alice completion done_at %v no longer anchors the quest's completed_at %v", compDoneAt, pendingBefore.CompletedAt.Time)
	}
	var aliceCompletionRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quest_completions WHERE household_id = $1 AND quest_id = $2`, h.ID, alicePending.ID).Scan(&aliceCompletionRows); err != nil || aliceCompletionRows != 1 {
		t.Errorf("alice quest completion rows after outsider MarkDone = %d, %v; want 1", aliceCompletionRows, err)
	}
	if rows, err := quests.ListPending(ctx, mallory, mh.ID, ""); err != nil || len(rows) != 0 {
		t.Errorf("mallory's own pending list shows other households' quests: %+v, %v", rows, err)
	}
	// CLM-14: no outsider MarkDone, Approve or Reject moved Alice's streak,
	// and no streak row landed in (or crossed into) Mallory's household.
	var aliceStreakRowsAfter, aliceStreakCountAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(MAX(count), 0) FROM member_quest_streaks WHERE household_id = $1 AND member_id = $2 AND quest_id = $3`, h.ID, aliceMember.ID, alicePending.ID).Scan(&aliceStreakRowsAfter, &aliceStreakCountAfter); err != nil {
		t.Fatalf("get alice streak after outsider attempts: %v", err)
	}
	if aliceStreakRowsAfter != aliceStreakRowsBefore {
		t.Errorf("alice streak rows after outsider attempts = %d, want %d (unchanged)", aliceStreakRowsAfter, aliceStreakRowsBefore)
	}
	if aliceStreakCountAfter != aliceStreakCountBefore {
		t.Errorf("alice streak count after outsider attempts = %d, want %d (unchanged)", aliceStreakCountAfter, aliceStreakCountBefore)
	}
	var malloryStreakRowsAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM member_quest_streaks WHERE household_id = $1`, mh.ID).Scan(&malloryStreakRowsAfter); err != nil {
		t.Fatalf("get mallory streak rows after outsider attempts: %v", err)
	}
	if malloryStreakRowsAfter != 0 {
		t.Errorf("mallory household streak rows after outsider attempts = %d, want 0", malloryStreakRowsAfter)
	}
	var crossedStreakRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM member_quest_streaks WHERE household_id <> $1 AND (member_id = $2 OR quest_id = $3)`, h.ID, aliceMember.ID, alicePending.ID).Scan(&crossedStreakRows); err != nil {
		t.Fatalf("get cross-household streak rows: %v", err)
	}
	if crossedStreakRows != 0 {
		t.Errorf("streak rows for alice's member or quest outside her household = %d, want 0", crossedStreakRows)
	}
	// Family member and admin-pin isolation checks
	_, checks["list family members"] = familySvc.List(ctx, mallory, h.ID)
	_, checks["get family member"] = familySvc.Get(ctx, mallory, h.ID, aliceMember.ID)
	_, checks["create family member"] = familySvc.Create(ctx, mallory, h.ID, app.FamilyMemberInput{Name: "Evil"}, "")
	_, checks["rename family member"] = familySvc.Rename(ctx, mallory, h.ID, aliceMember.ID, app.RenameMemberInput{Name: "Pwned"}, "")
	checks["remove family member"] = familySvc.Remove(ctx, mallory, h.ID, aliceMember.ID, "")
	_, checks["set member pin"] = familySvc.SetPin(ctx, mallory, h.ID, aliceMember.ID, "9999", "")
	_, checks["clear member pin"] = familySvc.ClearPin(ctx, mallory, h.ID, aliceMember.ID, "")
	_, checks["verify member pin"] = familySvc.Verify(ctx, mallory, h.ID, aliceMember.ID, "1234")
	_, checks["admin pin set"] = familySvc.AdminPinSet(ctx, mallory, h.ID)
	checks["set admin pin"] = familySvc.SetAdminPin(ctx, mallory, h.ID, "1111", "")
	checks["clear admin pin"] = familySvc.ClearAdminPin(ctx, mallory, h.ID, "")
	checks["verify admin pin"] = familySvc.VerifyAdminPin(ctx, mallory, h.ID, "1111")
	// Cross-household ID smuggling with family members:
	_, checks["get family member via own household"] = familySvc.Get(ctx, mallory, mh.ID, aliceMember.ID)
	checks["remove family member via own household"] = familySvc.Remove(ctx, mallory, mh.ID, aliceMember.ID, "")
	// Points history isolation checks:
	_, checks["points history outsider user"] = familySvc.PointsHistory(ctx, mallory, h.ID, aliceMember.ID, "1234", "", 10, 0)
	_, checks["points history own household foreign member"] = familySvc.PointsHistory(ctx, alice, h.ID, malloryMember.ID, "", "", 10, 0)
	_, checks["points history foreign household own member"] = familySvc.PointsHistory(ctx, alice, mh.ID, aliceMember.ID, "1234", "", 10, 0)
	_, checks["points history nonexistent member"] = familySvc.PointsHistory(ctx, alice, h.ID, uuid.New(), "1234", "", 10, 0)
	// Reward isolation: every reward operation by an outsider is ErrNotFound,
	// and Alice's reward id smuggled under Mallory's own household fails too.
	rewardLibrary, err := rewards.ListLibrary(ctx)
	if err != nil || len(rewardLibrary) == 0 {
		t.Fatalf("ListLibrary rewards: %v", err)
	}
	_, checks["list rewards"] = rewards.List(ctx, mallory, h.ID)
	_, checks["get reward"] = rewards.Get(ctx, mallory, h.ID, aliceReward.ID)
	_, checks["create reward"] = rewards.Create(ctx, mallory, h.ID, app.RewardInput{Description: "Evil", Cost: num(1)}, "")
	_, checks["update reward"] = rewards.Update(ctx, mallory, h.ID, aliceReward.ID, app.RewardInput{Description: "Pwned", Cost: num(0)}, "")
	checks["delete reward"] = rewards.Delete(ctx, mallory, h.ID, aliceReward.ID, "")
	_, checks["adopt reward from library"] = rewards.CopyFromLibrary(ctx, mallory, h.ID, rewardLibrary[0].ID, "")
	_, checks["get reward via own household"] = rewards.Get(ctx, mallory, mh.ID, aliceReward.ID)
	_, checks["update reward via own household"] = rewards.Update(ctx, mallory, mh.ID, aliceReward.ID, app.RewardInput{Description: "Smuggled", Cost: num(0)}, "")
	checks["delete reward via own household"] = rewards.Delete(ctx, mallory, mh.ID, aliceReward.ID, "")
	if own, err := rewards.List(ctx, mallory, mh.ID); err != nil || len(own) != 0 {
		t.Errorf("mallory's own reward list shows other households' rewards: %+v, %v", own, err)
	}

	// Combo isolation: every combo operation by an outsider is ErrNotFound,
	// smuggling Alice's combo id via Mallory's own household fails, and
	// creating a combo with Alice's chore ids fails with ErrNotFound.
	_, checks["list combos"] = combos.List(ctx, mallory, h.ID)
	_, checks["get combo"] = combos.Get(ctx, mallory, h.ID, aliceCombo.ID)
	_, checks["create combo"] = combos.Create(ctx, mallory, h.ID, app.ComboInput{Name: "Evil", BonusPoints: num(10), QuestIDs: []uuid.UUID{aliceQuest.ID, aliceQuest2.ID}}, "")
	_, checks["update combo"] = combos.Update(ctx, mallory, h.ID, aliceCombo.ID, app.ComboInput{Name: "Pwned", BonusPoints: num(0), QuestIDs: []uuid.UUID{aliceQuest.ID, aliceQuest2.ID}}, "")
	checks["delete combo"] = combos.Delete(ctx, mallory, h.ID, aliceCombo.ID, "")
	_, checks["get combo via own household"] = combos.Get(ctx, mallory, mh.ID, aliceCombo.ID)
	_, checks["update combo via own household"] = combos.Update(ctx, mallory, mh.ID, aliceCombo.ID, app.ComboInput{Name: "Smuggled", BonusPoints: num(0), QuestIDs: []uuid.UUID{aliceQuest.ID, aliceQuest2.ID}}, "")
	checks["delete combo via own household"] = combos.Delete(ctx, mallory, mh.ID, aliceCombo.ID, "")
	_, checks["create combo with foreign quests"] = combos.Create(ctx, mallory, mh.ID, app.ComboInput{Name: "Foreign Chores", BonusPoints: num(10), QuestIDs: []uuid.UUID{aliceQuest.ID, aliceQuest2.ID}}, "")
	if own, err := combos.List(ctx, mallory, mh.ID); err != nil || len(own) != 0 {
		t.Errorf("mallory's own combo list shows other households' combos: %+v, %v", own, err)
	}
	if got, err := combos.Get(ctx, alice, h.ID, aliceCombo.ID); err != nil || got.ID != aliceCombo.ID || got.Name != "Kitchen Crew" {
		t.Errorf("alice's combo was modified by outsider: %+v, %v", got, err)
	}

	for name, err := range checks {
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s: expected ErrNotFound, got %v", name, err)
		}
	}
	// The outsider's attempts never reach Google.
	if reqs := google.recorded()[requestsBefore:]; len(reqs) != 0 {
		t.Errorf("outsider's family calendar calls reached Google: %+v", reqs)
	}

	// Alice sees the family calendar's entries, fetched server-side with
	// Alice's token from calendar Y only.
	requestsBefore = len(google.recorded())
	entries, err := family.ListEntries(ctx, alice, h.ID, from, to)
	if err != nil || entries.Status != app.FamilyCalendarOK || len(entries.Entries) != 1 || entries.Entries[0].ID != "y1" {
		t.Fatalf("alice's entries = %+v, %v; want only y1", entries, err)
	}
	reqs := google.recorded()[requestsBefore:]
	if len(reqs) != 1 || reqs[0].Auth != aliceAccess || reqs[0].Path != "/calendars/family-y@group.calendar.google.com/events" {
		t.Errorf("alice's fetch requests = %+v, want one events request to Y with alice's token", reqs)
	}
	status, err := family.Status(ctx, alice, h.ID)
	if err != nil || status.Status != app.FamilyCalendarOK || status.SetByID != alice {
		t.Fatalf("alice's status = %+v, %v", status, err)
	}
	// Nothing Alice receives carries her tokens, her calendar list or another calendar's entries.
	bodies, _ := json.Marshal(map[string]any{"status": status, "entries": entries})
	for _, leak := range []string{aliceAccess, aliceRefresh, "primary-alice@example.com", "Alice private", "x1"} {
		if strings.Contains(string(bodies), leak) {
			t.Errorf("alice's results contain %q: %s", leak, bodies)
		}
	}
	// Bob is an outsider to Alice's household and cannot list her entries.
	if _, err := family.ListEntries(ctx, bob, h.ID, from, to); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("bob list entries: expected ErrNotFound, got %v", err)
	}
	if _, err := family.Status(ctx, bob, h.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("bob status: expected ErrNotFound, got %v", err)
	}
	// Bob may not list Alice's calendars: his own list is "not connected".
	mine, err := family.ListMyCalendars(ctx, bob)
	if err != nil || mine.Status != app.CalendarNotConnected || len(mine.Calendars) != 0 {
		t.Errorf("bob's calendar list = %+v, %v; want not_connected and empty", mine, err)
	}

	// Alice's data is intact.
	if got, err := quests.Get(ctx, alice, h.ID, aliceQuest.ID); err != nil || got.Name != "Dishes" || got.Points != 10 ||
		got.RepeatDays != 1 || got.Notes != "Wash and dry" || got.ClaimedBy != nil || got.ClaimedAt.Valid {
		t.Errorf("alice's quest was modified: %+v, %v", got, err)
	}
	if list, _, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10}); err != nil || len(list) != 3 {
		t.Errorf("alice's quests after outsider attempts: %d, %v; want exactly 3 (no smuggled create or copy)", len(list), err)
	}
	if rows, err := quests.ListPending(ctx, alice, h.ID, ""); err != nil || len(rows) != 1 || rows[0].QuestID != alicePending.ID ||
		rows[0].MemberID != aliceMember.ID {
		t.Errorf("alice's pending quest was changed by the outsider: %+v, %v", rows, err)
	}
	if m, err := familySvc.Get(ctx, alice, h.ID, aliceMember.ID); err != nil || m.Points != 0 {
		t.Errorf("alice's member points changed by the outsider: %+v, %v", m, err)
	}
	if list, err := rewards.List(ctx, alice, h.ID); err != nil || len(list) != 1 || list[0].ID != aliceReward.ID ||
		list[0].Description != "Movie night" || list[0].Cost != 300 || list[0].LibraryID != nil {
		t.Errorf("alice's rewards after outsider attempts = %+v, %v; want only Movie night / 300", list, err)
	}
	if st, err := family.Status(ctx, alice, h.ID); err != nil || st.CalendarID != "family-y@group.calendar.google.com" || st.SetByID != alice {
		t.Errorf("alice's family calendar was modified: %+v, %v", st, err)
	}

	// Positive control for the tag check: Alice tags the entry with her family member.
	requestsBefore = len(google.recorded())
	tagged, err := family.SetEntryTags(ctx, alice, h.ID, "y1", []uuid.UUID{aliceMember.ID})
	if err != nil || tagged.Title != "Y soccer [Alice Member]" {
		t.Fatalf("alice tags y1: %+v, %v; want title 'Y soccer [Alice Member]'", tagged, err)
	}
	if reqs := google.recorded()[requestsBefore:]; patches(reqs) != 1 || reqs[len(reqs)-1].Auth != aliceAccess {
		t.Errorf("alice's tag requests = %+v, want one PATCH with alice's token", reqs)
	}

	// CLM-3: Household A (Alice) and Household B (Bob), each with family members,
	// quests (one claimed, one pending approval), rewards, and points.
	// Alice also adds a claimed quest and points to Household A.
	aliceClaimed, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Dishes Claimed", Points: num(10), RepeatDays: num(1)}, "")
	if err != nil {
		t.Fatalf("Create alice claimed quest: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, aliceClaimed.ID, asAlice); err != nil {
		t.Fatalf("Accept alice claimed quest: %v", err)
	}
	if _, err := q.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: h.ID, ID: aliceMember.ID, Delta: 10}); err != nil {
		t.Fatalf("Add points to alice member: %v", err)
	}

	// Seed Household B for Bob
	bh, err := households.Create(ctx, bob, app.HouseholdInput{Name: "Bob Family", Timezone: "America/New_York"})
	if err != nil {
		t.Fatalf("Create household B: %v", err)
	}
	bobMember, err := familySvc.Create(ctx, bob, bh.ID, app.FamilyMemberInput{Name: "Bob Member", PIN: "4321"}, "")
	if err != nil {
		t.Fatalf("Create bob member: %v", err)
	}
	if _, err := q.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: bh.ID, ID: bobMember.ID, Delta: 25}); err != nil {
		t.Fatalf("Add points to bob member: %v", err)
	}
	bobClaimed, err := quests.Create(ctx, bob, bh.ID, app.QuestInput{Name: "Bob Claimed", Points: num(15), RepeatDays: num(1)}, "")
	if err != nil {
		t.Fatalf("Create bob claimed quest: %v", err)
	}
	asBob := app.MemberAction{MemberID: bobMember.ID, PIN: "4321"}
	if _, err := quests.Accept(ctx, bob, bh.ID, bobClaimed.ID, asBob); err != nil {
		t.Fatalf("Accept bob claimed quest: %v", err)
	}
	bobPending, err := quests.Create(ctx, bob, bh.ID, app.QuestInput{Name: "Bob Pending", Points: num(20), RepeatDays: num(1)}, "")
	if err != nil {
		t.Fatalf("Create bob pending quest: %v", err)
	}
	if _, err := quests.Accept(ctx, bob, bh.ID, bobPending.ID, asBob); err != nil {
		t.Fatalf("Accept bob pending quest: %v", err)
	}
	if _, err := quests.MarkDone(ctx, bob, bh.ID, bobPending.ID, asBob); err != nil {
		t.Fatalf("MarkDone bob pending quest: %v", err)
	}
	bobReward, err := rewards.Create(ctx, bob, bh.ID, app.RewardInput{Description: "Ice cream", Cost: num(100)}, "")
	if err != nil {
		t.Fatalf("Create bob reward: %v", err)
	}

	// Points history isolation and PIN checks:
	// Insert awards for Alice (Household A) and Bob (Household B).
	aliceAwardID := uuid.New()
	_, err = q.InsertPointsAward(ctx, db.InsertPointsAwardParams{
		ID:          aliceAwardID,
		HouseholdID: h.ID,
		MemberID:    aliceMember.ID,
		Kind:        "quest",
		Reason:      "Alice quest",
		Points:      10,
		EarnedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertPointsAward for alice: %v", err)
	}

	bobAwardID := uuid.New()
	_, err = q.InsertPointsAward(ctx, db.InsertPointsAwardParams{
		ID:          bobAwardID,
		HouseholdID: bh.ID,
		MemberID:    bobMember.ID,
		Kind:        "quest",
		Reason:      "Bob quest",
		Points:      25,
		EarnedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertPointsAward for bob: %v", err)
	}

	// Right member PIN reads history; Bob's award is not in Alice's history.
	aliceHist, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "1234", "", 50, 0)
	if err != nil {
		t.Fatalf("PointsHistory with member PIN: %v", err)
	}
	if len(aliceHist) != 1 || aliceHist[0].ID != aliceAwardID {
		t.Errorf("alice points history mismatch: got %+v, want award %v", aliceHist, aliceAwardID)
	}

	// Admin PIN reads history (SetAdminPin first).
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "5555", ""); err != nil {
		t.Fatalf("SetAdminPin for alice: %v", err)
	}
	aliceHistAdmin, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "", "5555", 50, 0)
	if err != nil {
		t.Fatalf("PointsHistory with admin PIN: %v", err)
	}
	if len(aliceHistAdmin) != 1 || aliceHistAdmin[0].ID != aliceAwardID {
		t.Errorf("alice admin PIN points history mismatch: got %+v, want award %v", aliceHistAdmin, aliceAwardID)
	}
	if err := familySvc.ClearAdminPin(ctx, alice, h.ID, "5555"); err != nil {
		t.Fatalf("ClearAdminPin for alice: %v", err)
	}

	// Wrong PIN -> ErrWrongPIN and no rows returned.
	wrongHist, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "0000", "", 50, 0)
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("wrong member PIN points history: expected ErrWrongPIN, got %v", err)
	}
	if len(wrongHist) != 0 {
		t.Errorf("wrong member PIN points history returned rows: %+v", wrongHist)
	}
	wrongHist2, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "0000", "0000", 50, 0)
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("wrong member and admin PIN points history: expected ErrWrongPIN, got %v", err)
	}
	if len(wrongHist2) != 0 {
		t.Errorf("wrong member and admin PIN points history returned rows: %+v", wrongHist2)
	}

	// A DB fault inside PIN verification (after getHousehold and the member
	// lookup succeed) is returned wrapped, not reported as ErrWrongPIN.
	// (a) Member path: PointsHistory reads GetFamilyMember once for the
	// existence check; the 2nd read is VerifyMemberPin's pin_hash read.
	memberFault := &faultDBTX{inner: pool, query: "GetFamilyMember", failOnCall: 2}
	memberFaultSvc := app.NewFamilyService(db.New(memberFault), pool, 10)
	nonPinHist, err := memberFaultSvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "1234", "", 50, 0)
	if memberFault.calls < 2 {
		t.Fatalf("member-path fault never reached VerifyMemberPin: %d GetFamilyMember calls, err %v", memberFault.calls, err)
	}
	if err == nil {
		t.Errorf("PointsHistory with member verify fault: expected error, got nil")
	} else if errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("PointsHistory with member verify fault returned ErrWrongPIN: %v", err)
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("PointsHistory with member verify fault: want wrapped injected error, got %v", err)
	}
	if len(nonPinHist) != 0 {
		t.Errorf("PointsHistory with member verify fault returned rows: %+v", nonPinHist)
	}

	// (b) Admin path: wrong member PIN, then the admin PIN check hits a fault
	// reading the household's admin PIN.
	if err := familySvc.SetAdminPin(ctx, alice, h.ID, "5555", ""); err != nil {
		t.Fatalf("SetAdminPin for admin-path fault: %v", err)
	}
	adminFault := &faultDBTX{inner: pool, query: "GetAdminPin", failOnCall: 1}
	adminFaultSvc := app.NewFamilyService(db.New(adminFault), pool, 10)
	nonPinAdminHist, err := adminFaultSvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "0000", "5555", 50, 0)
	if adminFault.calls < 1 {
		t.Fatalf("admin-path fault never reached VerifyAdminPin: err %v", err)
	}
	if err == nil {
		t.Errorf("PointsHistory with admin verify fault: expected error, got nil")
	} else if errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("PointsHistory with admin verify fault returned ErrWrongPIN: %v", err)
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("PointsHistory with admin verify fault: want wrapped injected error, got %v", err)
	}
	if len(nonPinAdminHist) != 0 {
		t.Errorf("PointsHistory with admin verify fault returned rows: %+v", nonPinAdminHist)
	}
	if err := familySvc.ClearAdminPin(ctx, alice, h.ID, "5555"); err != nil {
		t.Fatalf("ClearAdminPin after admin-path fault: %v", err)
	}

	// Bob's history in household B is separate.
	bobHist, err := familySvc.PointsHistory(ctx, bob, bh.ID, bobMember.ID, "4321", "", 50, 0)
	if err != nil {
		t.Fatalf("bob PointsHistory with member PIN: %v", err)
	}
	if len(bobHist) != 1 || bobHist[0].ID != bobAwardID {
		t.Errorf("bob points history mismatch: got %+v, want award %v", bobHist, bobAwardID)
	}

	// CLM-4: Cross-household combo isolation. Look-alike chores done the same
	// day in another household never trigger or pay a household's combo.
	bobDishes, err := quests.Create(ctx, bob, bh.ID, app.QuestInput{
		Name:       "Dishes",
		Points:     num(10),
		RepeatDays: num(1),
	}, "")
	if err != nil {
		t.Fatalf("Create bob dishes: %v", err)
	}
	bobWipe, err := quests.Create(ctx, bob, bh.ID, app.QuestInput{
		Name:       "Wipe counters",
		Points:     num(5),
		RepeatDays: num(1),
	}, "")
	if err != nil {
		t.Fatalf("Create bob wipe counters: %v", err)
	}
	_, err = combos.Create(ctx, bob, bh.ID, app.ComboInput{
		Name:        "Kitchen Crew",
		BonusPoints: num(20),
		QuestIDs:    []uuid.UUID{bobDishes.ID, bobWipe.ID},
	}, "")
	if err != nil {
		t.Fatalf("Create bob combo: %v", err)
	}

	// Bob's member accepts and marks done both look-alike chores.
	if _, err := quests.Accept(ctx, bob, bh.ID, bobDishes.ID, asBob); err != nil {
		t.Fatalf("Accept bob dishes: %v", err)
	}
	if _, err := quests.MarkDone(ctx, bob, bh.ID, bobDishes.ID, asBob); err != nil {
		t.Fatalf("MarkDone bob dishes: %v", err)
	}
	if _, err := quests.Accept(ctx, bob, bh.ID, bobWipe.ID, asBob); err != nil {
		t.Fatalf("Accept bob wipe counters: %v", err)
	}
	if _, err := quests.MarkDone(ctx, bob, bh.ID, bobWipe.ID, asBob); err != nil {
		t.Fatalf("MarkDone bob wipe counters: %v", err)
	}

	// Approve Bob's look-alike completions (leave bobPending awaiting approval).
	bobPendingList, err := quests.ListPending(ctx, bob, bh.ID, "")
	if err != nil {
		t.Fatalf("ListPending bob: %v", err)
	}
	var bobBonusesTotal int
	for _, p := range bobPendingList {
		if p.QuestID == bobDishes.ID || p.QuestID == bobWipe.ID {
			_, _, bonuses, _, err := quests.Approve(ctx, bob, bh.ID, p.ID, "")
			if err != nil {
				t.Fatalf("Approve bob completion %s: %v", p.ID, err)
			}
			bobBonusesTotal += len(bonuses)
		}
	}
	if bobBonusesTotal != 1 {
		t.Fatalf("bob combo bonuses = %d, want 1", bobBonusesTotal)
	}

	// Alice's member accepts and marks done only aliceQuest ("Dishes").
	// Even though Bob marked done "Wipe counters" in household B on the same day,
	// Bob's completion must not count toward Alice's "Kitchen Crew" combo.
	if _, err := quests.Accept(ctx, alice, h.ID, aliceQuest.ID, asAlice); err != nil {
		t.Fatalf("Accept aliceQuest: %v", err)
	}
	if _, err := quests.MarkDone(ctx, alice, h.ID, aliceQuest.ID, asAlice); err != nil {
		t.Fatalf("MarkDone aliceQuest: %v", err)
	}

	alicePendingList, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil {
		t.Fatalf("ListPending alice: %v", err)
	}
	var aliceDishesCompID uuid.UUID
	for _, p := range alicePendingList {
		if p.QuestID == aliceQuest.ID {
			aliceDishesCompID = p.ID
			break
		}
	}
	if aliceDishesCompID == uuid.Nil {
		t.Fatal("alice dishes completion not found in pending list")
	}

	_, _, aliceBonuses, _, err := quests.Approve(ctx, alice, h.ID, aliceDishesCompID, "")
	if err != nil {
		t.Fatalf("Approve alice dishes completion: %v", err)
	}
	if len(aliceBonuses) != 0 {
		t.Fatalf("alice combo bonuses = %d, want 0", len(aliceBonuses))
	}

	// Verify combo_awards in the database: Bob has 1, Alice has 0.
	var aliceComboAwardsCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM combo_awards WHERE household_id = $1", h.ID).Scan(&aliceComboAwardsCount); err != nil {
		t.Fatalf("count alice combo_awards: %v", err)
	}
	if aliceComboAwardsCount != 0 {
		t.Errorf("alice combo_awards count = %d, want 0", aliceComboAwardsCount)
	}

	var bobComboAwardsCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM combo_awards WHERE household_id = $1", bh.ID).Scan(&bobComboAwardsCount); err != nil {
		t.Fatalf("count bob combo_awards: %v", err)
	}
	if bobComboAwardsCount != 1 {
		t.Errorf("bob combo_awards count = %d, want 1", bobComboAwardsCount)
	}

	var aliceComboByComboID int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM combo_awards WHERE combo_id = $1", aliceCombo.ID).Scan(&aliceComboByComboID); err != nil {
		t.Fatalf("count aliceCombo by id: %v", err)
	}
	if aliceComboByComboID != 0 {
		t.Errorf("aliceCombo awards count = %d, want 0", aliceComboByComboID)
	}

	// Restore Bob's member points to baseline 25 for downstream assertions.
	if _, err := q.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: bh.ID, ID: bobMember.ID, Delta: -35}); err != nil {
		t.Fatalf("restore bob member points: %v", err)
	}

	// Non-member (Mallory) gets ErrNotFound deleting household A, and A stays intact.
	if err := households.Delete(ctx, mallory, h.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory delete household A: expected ErrNotFound, got %v", err)
	}
	if _, err := households.Get(ctx, alice, h.ID); err != nil {
		t.Errorf("household A missing after mallory delete attempt: %v", err)
	}

	// Capture Bob's household_provisioned_at before
	var bobProvBefore pgtype.Timestamptz
	if err := pool.QueryRow(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1", bob).Scan(&bobProvBefore); err != nil {
		t.Fatalf("query bob household_provisioned_at before: %v", err)
	}

	// Lock Alice's user row in a manual tx (mirroring an in-flight delete on Alice's row),
	// and assert Bob can sign in without blocking.
	aliceDelTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin alice delete tx: %v", err)
	}
	defer aliceDelTx.Rollback(ctx) //nolint:errcheck
	if _, err := aliceDelTx.Exec(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1 FOR UPDATE", alice); err != nil {
		t.Fatalf("lock alice row: %v", err)
	}

	bobLoginDone := make(chan error, 1)
	go func() {
		_, lErr := authSvc.Login(ctx, bobRes.User.Email, "correct-password-123", app.DeviceMeta{})
		bobLoginDone <- lErr
	}()
	select {
	case lErr := <-bobLoginDone:
		if lErr != nil {
			t.Fatalf("bob sign-in failed while alice delete tx open: %v", lErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bob sign-in blocked by alice delete tx lock")
	}
	_ = aliceDelTx.Rollback(ctx)

	// Capture Bob's PointsHistory right before Alice deletes household A.
	bobHistBefore, err := familySvc.PointsHistory(ctx, bob, bh.ID, bobMember.ID, "4321", "", 50, 0)
	if err != nil {
		t.Fatalf("bob PointsHistory before alice delete: %v", err)
	}
	var foundBobAward bool
	for _, a := range bobHistBefore {
		if a.ID == bobAwardID {
			foundBobAward = true
			break
		}
	}
	if !foundBobAward {
		t.Fatalf("bob points history before alice delete missing award %v: %+v", bobAwardID, bobHistBefore)
	}

	// Alice deletes household A via HouseholdService.Delete
	if err := households.Delete(ctx, alice, h.ID); err != nil {
		t.Fatalf("alice Delete household A: %v", err)
	}

	// Assert A's household, family_members, household_quests and household_rewards are gone.
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM households WHERE id = $1", h.ID).Scan(&count); err != nil || count != 0 {
		t.Errorf("household A row count = %d, want 0", count)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM family_members WHERE household_id = $1", h.ID).Scan(&count); err != nil || count != 0 {
		t.Errorf("household A family_members count = %d, want 0", count)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_quests WHERE household_id = $1", h.ID).Scan(&count); err != nil || count != 0 {
		t.Errorf("household A household_quests count = %d, want 0", count)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM household_rewards WHERE household_id = $1", h.ID).Scan(&count); err != nil || count != 0 {
		t.Errorf("household A household_rewards count = %d, want 0", count)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM points_awards WHERE household_id = $1", h.ID).Scan(&count); err != nil || count != 0 {
		t.Errorf("household A points_awards count = %d, want 0", count)
	}

	// Assert B's household, members, quests (including claimed and pending states), rewards and points are unchanged.
	gotBH, err := households.Get(ctx, bob, bh.ID)
	if err != nil || gotBH.ID != bh.ID || gotBH.Name != "Bob Family" {
		t.Errorf("household B changed or missing: %+v, %v", gotBH, err)
	}
	gotBobMember, err := familySvc.Get(ctx, bob, bh.ID, bobMember.ID)
	if err != nil || gotBobMember.ID != bobMember.ID || gotBobMember.Name != "Bob Member" || gotBobMember.Points != 25 {
		t.Errorf("bob member changed or points modified: %+v, %v", gotBobMember, err)
	}
	gotBobClaimed, err := quests.Get(ctx, bob, bh.ID, bobClaimed.ID)
	if err != nil || gotBobClaimed.ClaimedBy == nil || *gotBobClaimed.ClaimedBy != bobMember.ID || gotBobClaimed.DoneAt.Valid {
		t.Errorf("bob claimed quest changed: %+v, %v", gotBobClaimed, err)
	}
	gotBobPending, err := quests.Get(ctx, bob, bh.ID, bobPending.ID)
	if err != nil || gotBobPending.ClaimedBy != nil || !gotBobPending.AwaitingApproval {
		t.Errorf("bob pending quest changed: %+v, %v", gotBobPending, err)
	}
	gotBobReward, err := rewards.Get(ctx, bob, bh.ID, bobReward.ID)
	if err != nil || gotBobReward.ID != bobReward.ID || gotBobReward.Description != "Ice cream" || gotBobReward.Cost != 100 {
		t.Errorf("bob reward changed: %+v, %v", gotBobReward, err)
	}
	gotBobHist, err := familySvc.PointsHistory(ctx, bob, bh.ID, bobMember.ID, "4321", "", 50, 0)
	if err != nil {
		t.Errorf("bob points history changed or missing after alice delete: %v", err)
	} else if len(gotBobHist) != len(bobHistBefore) {
		t.Errorf("bob points history length changed after alice delete: got %d, want %d", len(gotBobHist), len(bobHistBefore))
	} else {
		for i := range bobHistBefore {
			if gotBobHist[i].ID != bobHistBefore[i].ID {
				t.Errorf("bob points history award %d id changed: got %v, want %v", i, gotBobHist[i].ID, bobHistBefore[i].ID)
			}
		}
	}

	// Assert Bob's household_provisioned_at is unchanged.
	var bobProvAfter pgtype.Timestamptz
	if err := pool.QueryRow(ctx, "SELECT household_provisioned_at FROM users WHERE id = $1", bob).Scan(&bobProvAfter); err != nil {
		t.Fatalf("query bob household_provisioned_at after: %v", err)
	}
	if bobProvBefore.Time.Unix() != bobProvAfter.Time.Unix() || bobProvBefore.Valid != bobProvAfter.Valid {
		t.Errorf("bob household_provisioned_at changed: before %v, after %v", bobProvBefore, bobProvAfter)
	}
}

func TestIntegration_HouseholdMembersOneLoginConstraint(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)

	user1 := registerVerified(t, pool, authSvc, mailer, "user1").User.ID
	user2 := registerVerified(t, pool, authSvc, mailer, "user2").User.ID

	h, err := households.Create(ctx, user1, app.HouseholdInput{Name: "One Login Household"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Attempting to insert a second login member for the same household must fail on the unique index.
	err = q.AddHouseholdMember(ctx, db.AddHouseholdMemberParams{
		HouseholdID: h.ID,
		UserID:      user2,
		Role:        app.RoleAdmin,
	})
	if err == nil {
		t.Fatal("expected error on inserting second household_members row, got nil")
	}
}

func TestIntegration_DeleteAccountDeletesHousehold(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	ctx := context.Background()

	parent := registerVerified(t, pool, authSvc, mailer, "delparent").User.ID

	h, err := households.Create(ctx, parent, app.HouseholdInput{Name: "Home"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	familyMemberID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, $3)",
		familyMemberID, h.ID, "Kid"); err != nil {
		t.Fatalf("insert family member: %v", err)
	}

	if err := authSvc.DeleteAccount(ctx, parent, uuid.Nil, "correct-password-123", ""); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	var householdCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM households WHERE id = $1", h.ID).Scan(&householdCount); err != nil {
		t.Fatalf("count households: %v", err)
	}
	if householdCount != 0 {
		t.Errorf("household still exists: count = %d, want 0", householdCount)
	}

	var memberCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM family_members WHERE household_id = $1", h.ID).Scan(&memberCount); err != nil {
		t.Fatalf("count family members: %v", err)
	}
	if memberCount != 0 {
		t.Errorf("family members still exist: count = %d, want 0", memberCount)
	}

	var userCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users WHERE id = $1", parent).Scan(&userCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 0 {
		t.Errorf("user still exists: count = %d, want 0", userCount)
	}
}

// errInjected is the fault faultDBTX returns for its targeted query.
var errInjected = errors.New("injected db fault")

// faultDBTX delegates every call to the real pool, except the failOnCall-th
// run of the sqlc query named query, which fails with errInjected. It injects
// a DB fault at one point; it mocks nothing about the service under test.
type faultDBTX struct {
	inner      db.DBTX
	query      string // sqlc query name, e.g. "GetAdminPin"
	failOnCall int
	calls      int
}

func (f *faultDBTX) hit(sql string) bool {
	if !strings.HasPrefix(sql, "-- name: "+f.query+" :") {
		return false
	}
	f.calls++
	return f.calls == f.failOnCall
}

func (f *faultDBTX) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	if f.hit(sql) {
		return pgconn.CommandTag{}, errInjected
	}
	return f.inner.Exec(ctx, sql, args...)
}

func (f *faultDBTX) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if f.hit(sql) {
		return nil, errInjected
	}
	return f.inner.Query(ctx, sql, args...)
}

func (f *faultDBTX) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	if f.hit(sql) {
		return errRow{}
	}
	return f.inner.QueryRow(ctx, sql, args...)
}

// errRow is a pgx.Row whose Scan fails with errInjected.
type errRow struct{}

func (errRow) Scan(...any) error { return errInjected }
