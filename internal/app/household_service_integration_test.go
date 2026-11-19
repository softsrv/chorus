//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	familySvc := app.NewFamilyService(q, pool, 4)
	rewards := app.NewRewardService(q, pool)
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
	// Sharing: Alice's member holds a shareable quest. An outsider cannot share
	// it, it cannot be shared to another household's member or an unknown id,
	// and it cannot be reached through Mallory's own household.
	aliceShareable, err := quests.Create(ctx, alice, h.ID, app.QuestInput{
		Name: "Sweep", Points: num(100), Shareable: true, TeamworkBonus: num(20),
	}, "")
	if err != nil {
		t.Fatalf("Create shareable quest: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, aliceShareable.ID, app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"}); err != nil {
		t.Fatalf("Accept shareable quest: %v", err)
	}
	aliceKid, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Alice Kid"}, "")
	if err != nil {
		t.Fatalf("Create alice kid: %v", err)
	}
	malloryMember2, err := familySvc.Create(ctx, mallory, mh.ID, app.FamilyMemberInput{Name: "Mallory Member 2"}, "")
	if err != nil {
		t.Fatalf("Create mallory member 2: %v", err)
	}
	// Mallory member with an out-of-range birthday for aliceShareable:
	malloryOutOfRange, err := familySvc.Create(ctx, mallory, mh.ID, app.FamilyMemberInput{Name: "Mallory Out Of Range"}, "")
	if err != nil {
		t.Fatalf("Create mallory out of range member: %v", err)
	}
	malloryBday := "2000-01-01"
	if _, err := familySvc.SetBirthday(ctx, mallory, mh.ID, malloryOutOfRange.ID, &malloryBday, ""); err != nil {
		t.Fatalf("Set mallory out of range birthday: %v", err)
	}
	unknownMemberID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new uuid: %v", err)
	}
	shareActor := app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"}
	_, checks["share quest"] = quests.Share(ctx, mallory, h.ID, aliceShareable.ID, shareActor, app.MemberAction{MemberID: aliceKid.ID})
	_, checks["share quest as own member"] = quests.Share(ctx, mallory, h.ID, aliceShareable.ID, app.MemberAction{MemberID: malloryMember.ID}, app.MemberAction{MemberID: aliceKid.ID})
	_, checks["share quest to another household's member"] = quests.Share(ctx, alice, h.ID, aliceShareable.ID, shareActor, app.MemberAction{MemberID: malloryMember.ID})
	_, checks["share quest to another household's member with out of range birthday"] = quests.Share(ctx, alice, h.ID, aliceShareable.ID, shareActor, app.MemberAction{MemberID: malloryOutOfRange.ID})
	_, checks["share quest to unknown member"] = quests.Share(ctx, alice, h.ID, aliceShareable.ID, shareActor, app.MemberAction{MemberID: unknownMemberID})
	_, checks["share quest via own household"] = quests.Share(ctx, mallory, mh.ID, aliceShareable.ID, app.MemberAction{MemberID: malloryMember.ID}, app.MemberAction{MemberID: malloryMember2.ID})
	_, checks["share quest via own household with alice's members"] = quests.Share(ctx, mallory, mh.ID, aliceShareable.ID, shareActor, app.MemberAction{MemberID: aliceKid.ID})
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
	// The same four with no admin PIN set on either household and no PIN at
	// all, so the no-PIN admin fallback is reachable: every caller below is
	// the admin of some household, and none may reach the member's history.
	for hh, owner := range map[uuid.UUID]uuid.UUID{h.ID: alice, mh.ID: mallory} {
		if set, err := familySvc.AdminPinSet(ctx, owner, hh); err != nil || set {
			t.Fatalf("admin PIN set on %v before no-PIN isolation checks = %v, %v; want false", hh, set, err)
		}
	}
	_, checks["points history outsider user, no PINs"] = familySvc.PointsHistory(ctx, mallory, h.ID, aliceMember.ID, "", "", 10, 0)
	_, checks["points history own household foreign member, no PINs"] = familySvc.PointsHistory(ctx, alice, h.ID, malloryMember.ID, "", "", 10, 0)
	_, checks["points history foreign household own member, no PINs"] = familySvc.PointsHistory(ctx, alice, mh.ID, aliceMember.ID, "", "", 10, 0)
	_, checks["points history nonexistent member, no PINs"] = familySvc.PointsHistory(ctx, alice, h.ID, uuid.New(), "", "", 10, 0)
	_, checks["points history foreign member via own household as admin, no PINs"] = familySvc.PointsHistory(ctx, mallory, mh.ID, aliceMember.ID, "", "", 10, 0)
	// Admin points adjustment isolation: a valid adjustment by an outsider, on
	// a foreign member, under a foreign household or for an unknown member is
	// ErrNotFound, and nothing is written for either household's members.
	countAwards := func(memberID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM points_awards WHERE member_id = $1`, memberID).Scan(&n); err != nil {
			t.Fatalf("count points_awards for %s: %v", memberID, err)
		}
		return n
	}
	adjustMembers := []uuid.UUID{aliceMember.ID, malloryMember.ID}
	pointsBefore := map[uuid.UUID]int32{}
	awardsBefore := map[uuid.UUID]int{}
	for _, id := range adjustMembers {
		owner, hh := alice, h.ID
		if id == malloryMember.ID {
			owner, hh = mallory, mh.ID
		}
		m, err := familySvc.Get(ctx, owner, hh, id)
		if err != nil {
			t.Fatalf("get member %s before adjustments: %v", id, err)
		}
		pointsBefore[id], awardsBefore[id] = m.Points, countAwards(id)
	}
	adjust := app.AdjustPointsInput{Points: num(5), Reason: "smuggled"}
	_, checks["adjust points outsider user"] = familySvc.AdjustPoints(ctx, mallory, h.ID, aliceMember.ID, adjust, "")
	_, checks["adjust points own household foreign member"] = familySvc.AdjustPoints(ctx, alice, h.ID, malloryMember.ID, adjust, "")
	_, checks["adjust points foreign household own member"] = familySvc.AdjustPoints(ctx, alice, mh.ID, aliceMember.ID, adjust, "")
	_, checks["adjust points nonexistent member"] = familySvc.AdjustPoints(ctx, alice, h.ID, uuid.New(), adjust, "")
	_, checks["adjust points foreign member via own household as admin"] = familySvc.AdjustPoints(ctx, mallory, mh.ID, aliceMember.ID, adjust, "")
	_, checks["adjust points outsider user, removal"] = familySvc.AdjustPoints(ctx, mallory, h.ID, aliceMember.ID, app.AdjustPointsInput{Points: num(-5)}, "")
	for _, id := range adjustMembers {
		owner, hh := alice, h.ID
		if id == malloryMember.ID {
			owner, hh = mallory, mh.ID
		}
		m, err := familySvc.Get(ctx, owner, hh, id)
		if err != nil || m.Points != pointsBefore[id] {
			t.Errorf("member %s points after refused adjustments = %+v, %v; want %d", id, m, err, pointsBefore[id])
		}
		if n := countAwards(id); n != awardsBefore[id] {
			t.Errorf("member %s points_awards after refused adjustments = %d; want %d", id, n, awardsBefore[id])
		}
	}
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

	// Redemption isolation: Alice's member has a pending request for Alice's
	// reward. Every redemption operation by an outsider is ErrNotFound, and
	// Alice's reward, member or request id smuggled through Mallory's own
	// household fails too. The points are lent only for the request (nothing
	// is taken until approval) and handed back, so the checks below still see
	// Alice's member at 0.
	if _, err := q.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: h.ID, ID: aliceMember.ID, Delta: aliceReward.Cost}); err != nil {
		t.Fatalf("lend points to alice member: %v", err)
	}
	aliceRedemption, created, err := rewards.RequestRedemption(ctx, alice, h.ID, aliceReward.ID, app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"})
	if err != nil || !created {
		t.Fatalf("alice member requests redemption: created=%v, %v", created, err)
	}
	if _, err := q.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: h.ID, ID: aliceMember.ID, Delta: -aliceReward.Cost}); err != nil {
		t.Fatalf("hand back lent points: %v", err)
	}
	_, _, checks["request redemption"] = rewards.RequestRedemption(ctx, mallory, h.ID, aliceReward.ID, app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"})
	_, _, checks["request redemption as own member"] = rewards.RequestRedemption(ctx, mallory, h.ID, aliceReward.ID, app.MemberAction{MemberID: malloryMember.ID})
	_, checks["list member pending redemptions"] = rewards.ListMemberPending(ctx, mallory, h.ID, aliceMember.ID, "1234", "")
	_, checks["list pending redemptions"] = rewards.ListPending(ctx, mallory, h.ID, "")
	_, checks["reward history outsider user"] = rewards.RewardHistory(ctx, mallory, h.ID, aliceMember.ID, "1234", "", 10, 0)
	_, checks["reward history own household foreign member"] = rewards.RewardHistory(ctx, alice, h.ID, malloryMember.ID, "", "", 10, 0)
	_, checks["reward history foreign household own member"] = rewards.RewardHistory(ctx, alice, mh.ID, aliceMember.ID, "1234", "", 10, 0)
	_, _, checks["approve redemption"] = rewards.Approve(ctx, mallory, h.ID, aliceRedemption.ID, "")
	_, checks["reject redemption"] = rewards.Reject(ctx, mallory, h.ID, aliceRedemption.ID, "")
	_, _, checks["request redemption via own household with alice's reward"] = rewards.RequestRedemption(ctx, mallory, mh.ID, aliceReward.ID, app.MemberAction{MemberID: malloryMember.ID})
	_, _, checks["request redemption via own household with alice's member"] = rewards.RequestRedemption(ctx, mallory, mh.ID, aliceReward.ID, app.MemberAction{MemberID: aliceMember.ID, PIN: "1234"})
	_, checks["list member pending redemptions via own household"] = rewards.ListMemberPending(ctx, mallory, mh.ID, aliceMember.ID, "1234", "")
	_, checks["reward history via own household"] = rewards.RewardHistory(ctx, mallory, mh.ID, aliceMember.ID, "1234", "", 10, 0)
	_, checks["reward history foreign member via own household as admin"] = rewards.RewardHistory(ctx, mallory, mh.ID, aliceMember.ID, "", "", 10, 0)
	_, _, checks["approve redemption via own household"] = rewards.Approve(ctx, mallory, mh.ID, aliceRedemption.ID, "")
	_, checks["reject redemption via own household"] = rewards.Reject(ctx, mallory, mh.ID, aliceRedemption.ID, "")
	_, _, checks["approve redemption from own household as alice"] = rewards.Approve(ctx, alice, mh.ID, aliceRedemption.ID, "")
	if own, err := rewards.ListPending(ctx, mallory, mh.ID, ""); err != nil || len(own) != 0 {
		t.Errorf("mallory's own pending redemptions show other households' requests: %+v, %v", own, err)
	}
	// Alice's request is untouched: still pending, and no points moved.
	if pending, err := rewards.ListPending(ctx, alice, h.ID, ""); err != nil || len(pending) != 1 || pending[0].ID != aliceRedemption.ID {
		t.Errorf("alice's pending redemptions after outsider attempts = %+v, %v; want only %v", pending, err, aliceRedemption.ID)
	}
	if m, err := familySvc.Get(ctx, alice, h.ID, aliceMember.ID); err != nil || m.Points != 0 {
		t.Errorf("alice's member points changed by outsider redemption attempts: %+v, %v", m, err)
	}
	var redemptionRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reward_redemptions WHERE household_id = $1`, mh.ID).Scan(&redemptionRows); err != nil || redemptionRows != 0 {
		t.Errorf("reward_redemptions rows in mallory's household = %d, %v; want 0", redemptionRows, err)
	}
	// Put the request away so the checks below start from the same state.
	if _, err := rewards.Reject(ctx, alice, h.ID, aliceRedemption.ID, ""); err != nil {
		t.Fatalf("alice rejects her member's redemption: %v", err)
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

	// Quest schedule isolation (CLM-9): every schedule call by an outsider is
	// ErrNotFound, even with the outsider's own correct admin PIN; another
	// household's quest, member or schedule id is not found through the
	// caller's own household, and nothing is written or removed.
	schedules := app.NewQuestScheduleService(q, pool)
	later := time.Now().AddDate(0, 0, 30).Format(time.DateOnly)
	aliceSchedule, err := schedules.Schedule(ctx, alice, h.ID, app.ScheduleInput{QuestID: aliceQuest.ID, MemberID: aliceMember.ID, Date: later}, "")
	if err != nil {
		t.Fatalf("alice Schedule: %v", err)
	}
	const malloryPin = "9999"
	if err := familySvc.SetAdminPin(ctx, mallory, mh.ID, malloryPin, ""); err != nil {
		t.Fatalf("mallory SetAdminPin: %v", err)
	}
	malloryQuest, err := quests.Create(ctx, mallory, mh.ID, app.QuestInput{Name: "Mallory chore", Points: num(1)}, malloryPin)
	if err != nil {
		t.Fatalf("Create mallory quest: %v", err)
	}
	mallorySchedule, err := schedules.Schedule(ctx, mallory, mh.ID, app.ScheduleInput{QuestID: malloryQuest.ID, MemberID: malloryMember.ID, Date: later}, malloryPin)
	if err != nil {
		t.Fatalf("mallory Schedule in own household: %v", err)
	}
	scheduleRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM quest_schedules").Scan(&n); err != nil {
			t.Fatalf("count quest_schedules: %v", err)
		}
		return n
	}
	rowsBefore := scheduleRows()
	_, checks["schedule quest"] = schedules.Schedule(ctx, mallory, h.ID, app.ScheduleInput{QuestID: aliceQuest.ID, MemberID: aliceMember.ID, Date: later}, malloryPin)
	_, checks["schedule quest as own member"] = schedules.Schedule(ctx, mallory, h.ID, app.ScheduleInput{QuestID: aliceQuest.ID, MemberID: malloryMember.ID, Date: later}, malloryPin)
	_, checks["list schedules"] = schedules.List(ctx, mallory, h.ID, malloryPin)
	checks["cancel schedule"] = schedules.Cancel(ctx, mallory, h.ID, aliceSchedule.ID, malloryPin)
	checks["cancel schedule via own household"] = schedules.Cancel(ctx, mallory, mh.ID, aliceSchedule.ID, malloryPin)
	_, checks["schedule alice's quest via own household"] = schedules.Schedule(ctx, mallory, mh.ID, app.ScheduleInput{QuestID: aliceQuest.ID, MemberID: malloryMember.ID, Date: later}, malloryPin)
	_, checks["schedule alice's member via own household"] = schedules.Schedule(ctx, mallory, mh.ID, app.ScheduleInput{QuestID: malloryQuest.ID, MemberID: aliceMember.ID, Date: later}, malloryPin)
	// Alice, a real admin of her own household, cannot reach the other
	// household's quest, member or schedule through it.
	_, checks["alice schedules other household's quest"] = schedules.Schedule(ctx, alice, h.ID, app.ScheduleInput{QuestID: malloryQuest.ID, MemberID: aliceMember.ID, Date: later}, "")
	_, checks["alice schedules other household's member"] = schedules.Schedule(ctx, alice, h.ID, app.ScheduleInput{QuestID: aliceQuest.ID, MemberID: malloryMember.ID, Date: later}, "")
	checks["alice cancels other household's schedule"] = schedules.Cancel(ctx, alice, h.ID, mallorySchedule.ID, "")
	if got := scheduleRows(); got != rowsBefore {
		t.Errorf("quest_schedules rows after cross-household attempts = %d, want %d", got, rowsBefore)
	}
	if got, err := schedules.List(ctx, alice, h.ID, ""); err != nil || len(got) != 1 || got[0].ID != aliceSchedule.ID || got[0].HouseholdID != h.ID {
		t.Errorf("alice's schedules after outsider attempts = %+v, %v; want only %v", got, err, aliceSchedule.ID)
	}
	if got, err := schedules.List(ctx, mallory, mh.ID, malloryPin); err != nil || len(got) != 1 || got[0].ID != mallorySchedule.ID {
		t.Errorf("mallory's own schedules = %+v, %v; want only %v", got, err, mallorySchedule.ID)
	}
	// Upcoming isolation (CLM-2): an outsider, or a member id smuggled from
	// another household, gets ErrNotFound and no rows.
	upcomingRefused := map[string]func() ([]app.UpcomingQuest, error){
		"upcoming outsider user": func() ([]app.UpcomingQuest, error) {
			return schedules.Upcoming(ctx, mallory, h.ID, aliceMember.ID)
		},
		"upcoming outsider user as own member": func() ([]app.UpcomingQuest, error) {
			return schedules.Upcoming(ctx, mallory, h.ID, malloryMember.ID)
		},
		"upcoming alice's member via own household": func() ([]app.UpcomingQuest, error) {
			return schedules.Upcoming(ctx, mallory, mh.ID, aliceMember.ID)
		},
		"upcoming own household foreign member": func() ([]app.UpcomingQuest, error) {
			return schedules.Upcoming(ctx, alice, h.ID, malloryMember.ID)
		},
		"upcoming nonexistent member": func() ([]app.UpcomingQuest, error) {
			return schedules.Upcoming(ctx, alice, h.ID, uuid.New())
		},
	}
	for name, call := range upcomingRefused {
		got, err := call()
		checks[name] = err
		if len(got) != 0 {
			t.Errorf("%s returned data: %+v; want none", name, got)
		}
	}
	// Positive controls: each admin's own member sees only their own
	// household's schedule.
	if got, err := schedules.Upcoming(ctx, alice, h.ID, aliceMember.ID); err != nil || len(got) != 1 || got[0].ID != aliceSchedule.ID {
		t.Errorf("alice's member upcoming = %+v, %v; want only %v", got, err, aliceSchedule.ID)
	}
	if got, err := schedules.Upcoming(ctx, mallory, mh.ID, malloryMember.ID); err != nil || len(got) != 1 || got[0].ID != mallorySchedule.ID {
		t.Errorf("mallory's member upcoming = %+v, %v; want only %v", got, err, mallorySchedule.ID)
	}
	if err := familySvc.ClearAdminPin(ctx, mallory, mh.ID, malloryPin); err != nil {
		t.Fatalf("mallory ClearAdminPin: %v", err)
	}

	// Each household rolls over at its own local midnight (CLM-9): Auckland
	// and Los Angeles each have a schedule for the same calendar date D. At an
	// instant past Auckland's midnight of D but still D-1 in Los Angeles, only
	// Auckland's quest is assigned; Los Angeles's quest and schedule are left
	// alone until its own midnight, which in turn leaves Auckland's untouched.
	akl, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatalf("load Pacific/Auckland: %v", err)
	}
	lax, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	aklToday := time.Now().In(akl)
	dDate := time.Date(aklToday.Year(), aklToday.Month(), aklToday.Day()+2, 0, 0, 0, 0, time.UTC)
	dayD := dDate.Format(time.DateOnly)
	aklMidnight := time.Date(dDate.Year(), dDate.Month(), dDate.Day(), 0, 0, 0, 0, akl)
	laxMidnight := time.Date(dDate.Year(), dDate.Month(), dDate.Day(), 0, 0, 0, 0, lax)
	aklInstant := aklMidnight.Add(time.Hour)
	if got := aklInstant.In(lax).Format(time.DateOnly); got >= dayD {
		t.Fatalf("test setup: Los Angeles date at %v is %s, want before %s", aklInstant, got, dayD)
	}
	type tzHousehold struct {
		id, member, quest, schedule uuid.UUID
	}
	newTZHousehold := func(name, tz string) tzHousehold {
		t.Helper()
		hh, err := households.Create(ctx, alice, app.HouseholdInput{Name: name, Timezone: tz})
		if err != nil {
			t.Fatalf("create %s household: %v", name, err)
		}
		m, err := familySvc.Create(ctx, alice, hh.ID, app.FamilyMemberInput{Name: name + " Kid"}, "")
		if err != nil {
			t.Fatalf("create %s member: %v", name, err)
		}
		qq, err := quests.Create(ctx, alice, hh.ID, app.QuestInput{Name: name + " chore", Points: num(1)}, "")
		if err != nil {
			t.Fatalf("create %s quest: %v", name, err)
		}
		s, err := schedules.Schedule(ctx, alice, hh.ID, app.ScheduleInput{QuestID: qq.ID, MemberID: m.ID, Date: dayD}, "")
		if err != nil {
			t.Fatalf("schedule %s quest: %v", name, err)
		}
		return tzHousehold{id: hh.ID, member: m.ID, quest: qq.ID, schedule: s.ID}
	}
	aklH := newTZHousehold("Auckland", "Pacific/Auckland")
	laxH := newTZHousehold("Los Angeles", "America/Los_Angeles")
	holderOf := func(hh tzHousehold) *uuid.UUID {
		t.Helper()
		var holder *uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT claimed_by FROM household_quests WHERE household_id = $1 AND id = $2", hh.id, hh.quest).Scan(&holder); err != nil {
			t.Fatalf("read quest holder: %v", err)
		}
		return holder
	}
	scheduleExists := func(hh tzHousehold) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM quest_schedules WHERE household_id = $1 AND id = $2", hh.id, hh.schedule).Scan(&n); err != nil {
			t.Fatalf("read schedule: %v", err)
		}
		return n == 1
	}
	if n, err := app.RolloverDueHouseholds(ctx, q, pool, aklInstant); err != nil || n != 1 {
		t.Errorf("rollover at Auckland's midnight assigned %d, %v; want 1", n, err)
	}
	if got := holderOf(aklH); got == nil || *got != aklH.member {
		t.Errorf("Auckland quest holder after its midnight = %v, want %v", got, aklH.member)
	}
	if scheduleExists(aklH) {
		t.Errorf("Auckland one-off schedule still present after its day was assigned")
	}
	if got := holderOf(laxH); got != nil {
		t.Errorf("Los Angeles quest assigned before its own midnight: holder %v", got)
	}
	if !scheduleExists(laxH) {
		t.Errorf("Los Angeles schedule removed by Auckland's rollover")
	}
	if n, err := app.RolloverDueHouseholds(ctx, q, pool, laxMidnight.Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("rollover at Los Angeles's midnight assigned %d, %v; want 1", n, err)
	}
	if got := holderOf(laxH); got == nil || *got != laxH.member {
		t.Errorf("Los Angeles quest holder after its midnight = %v, want %v", got, laxH.member)
	}
	if got := holderOf(aklH); got == nil || *got != aklH.member {
		t.Errorf("Auckland quest touched by Los Angeles's rollover: holder %v, want %v", got, aklH.member)
	}
	if got, err := schedules.List(ctx, alice, h.ID, ""); err != nil || len(got) != 1 || got[0].ID != aliceSchedule.ID {
		t.Errorf("alice's household schedules touched by other households' rollovers: %+v, %v", got, err)
	}

	// Repeated schedule isolation (INT-0177 CLM-7): with a repeated schedule
	// due today in alice's household, every outsider call on it is
	// ErrNotFound, and the pass that assigns it leaves mallory's quests and
	// schedules exactly as they were.
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	aliceToday := time.Now().In(chicago).Format(time.DateOnly)
	repeatQuest, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Feed the fish", Points: num(1)}, "")
	if err != nil {
		t.Fatalf("Create repeat quest: %v", err)
	}
	repeatSchedule, err := schedules.Schedule(ctx, alice, h.ID, app.ScheduleInput{QuestID: repeatQuest.ID, MemberID: aliceMember.ID, Date: aliceToday, Repeat: true}, "")
	if err != nil || !repeatSchedule.Repeat {
		t.Fatalf("alice repeated Schedule = %+v, %v", repeatSchedule, err)
	}
	t.Cleanup(func() {
		// Never leave a repeated schedule due in the shared database.
		if _, err := pool.Exec(context.Background(), "DELETE FROM quest_schedules WHERE household_id = $1 AND id = $2", h.ID, repeatSchedule.ID); err != nil {
			t.Errorf("delete repeated schedule: %v", err)
		}
	})
	malloryRepeat, err := schedules.Schedule(ctx, mallory, mh.ID, app.ScheduleInput{QuestID: malloryQuest.ID, MemberID: malloryMember.ID, Date: later, Repeat: true}, "")
	if err != nil {
		t.Fatalf("mallory repeated Schedule in own household: %v", err)
	}
	_, checks["schedule repeated quest"] = schedules.Schedule(ctx, mallory, h.ID, app.ScheduleInput{QuestID: repeatQuest.ID, MemberID: aliceMember.ID, Date: aliceToday, Repeat: true}, "")
	_, checks["list schedules with a repeated one"] = schedules.List(ctx, mallory, h.ID, "")
	checks["cancel repeated schedule"] = schedules.Cancel(ctx, mallory, h.ID, repeatSchedule.ID, "")
	checks["cancel repeated schedule via own household"] = schedules.Cancel(ctx, mallory, mh.ID, repeatSchedule.ID, "")
	upRepeat, upRepeatErr := schedules.Upcoming(ctx, mallory, h.ID, aliceMember.ID)
	checks["upcoming with a repeated schedule"] = upRepeatErr
	if len(upRepeat) != 0 {
		t.Errorf("outsider upcoming with a repeated schedule returned data: %+v", upRepeat)
	}
	householdRows := func(hid uuid.UUID) string {
		t.Helper()
		var qs, ss string
		if err := pool.QueryRow(ctx, "SELECT COALESCE(json_agg(q ORDER BY q.id)::text, '') FROM household_quests q WHERE q.household_id = $1", hid).Scan(&qs); err != nil {
			t.Fatalf("snapshot quests: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT COALESCE(json_agg(s ORDER BY s.id)::text, '') FROM quest_schedules s WHERE s.household_id = $1", hid).Scan(&ss); err != nil {
			t.Fatalf("snapshot schedules: %v", err)
		}
		return qs + "\n" + ss
	}
	malloryBefore := householdRows(mh.ID)
	if _, err := app.RolloverDueHouseholds(ctx, q, pool, time.Now()); err != nil {
		t.Fatalf("rollover with a repeated schedule: %v", err)
	}
	var repeatHolder *uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT claimed_by FROM household_quests WHERE household_id = $1 AND id = $2", h.ID, repeatQuest.ID).Scan(&repeatHolder); err != nil {
		t.Fatalf("read repeat quest holder: %v", err)
	}
	if repeatHolder == nil || *repeatHolder != aliceMember.ID {
		t.Errorf("alice's repeated schedule not assigned by the pass: holder %v, want %v", repeatHolder, aliceMember.ID)
	}
	if got := householdRows(mh.ID); got != malloryBefore {
		t.Errorf("mallory's household changed by alice's rollover:\nbefore %s\nafter  %s", malloryBefore, got)
	}
	if err := schedules.Cancel(ctx, mallory, mh.ID, malloryRepeat.ID, ""); err != nil {
		t.Errorf("mallory cancels own repeated schedule: %v", err)
	}
	if err := schedules.Cancel(ctx, alice, h.ID, repeatSchedule.ID, ""); err != nil {
		t.Errorf("alice cancels own repeated schedule: %v", err)
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
	if list, _, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10}); err != nil || len(list) != 5 {
		t.Errorf("alice's quests after outsider attempts: %d, %v; want exactly 5 (her four and the repeated-schedule quest; no smuggled create or copy)", len(list), err)
	}
	// Every refused share left Alice's shareable quest unshared and still hers.
	if got, err := quests.Get(ctx, alice, h.ID, aliceShareable.ID); err != nil || got.Shared() || len(got.Participants) > 1 ||
		got.ClaimedBy == nil || *got.ClaimedBy != aliceMember.ID {
		t.Errorf("alice's shareable quest changed by refused shares: %+v, %v; want unshared, held by alice's member", got, err)
	}
	var shareRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM household_quest_participants WHERE quest_id = $1`, aliceShareable.ID).Scan(&shareRows); err != nil || shareRows != 0 {
		t.Errorf("participant rows after refused shares = %d, %v; want 0", shareRows, err)
	}
	// Positive control: the same share inside Alice's household succeeds, and
	// the quest's participants are Alice's members only.
	shared, err := quests.Share(ctx, alice, h.ID, aliceShareable.ID, shareActor, app.MemberAction{MemberID: aliceKid.ID})
	if err != nil || !shared.Shared() || len(shared.Participants) != 2 ||
		!slices.Contains(shared.Participants, aliceMember.ID) || !slices.Contains(shared.Participants, aliceKid.ID) {
		t.Fatalf("alice shares with her kid = %+v, %v; want participants alice member and alice kid", shared, err)
	}
	if list, _, err := quests.List(ctx, alice, h.ID, app.QuestPage{Limit: 10}); err == nil {
		for _, q := range list {
			for _, p := range q.Participants {
				if p == malloryMember.ID || p == malloryMember2.ID {
					t.Errorf("alice's quest %s lists mallory's member %s as a participant", q.ID, p)
				}
			}
		}
	} else {
		t.Errorf("alice's quest list after share: %v", err)
	}
	// Mallory's own household now holds her quest; her list must show only
	// quests of her household, never Alice's or Bob's.
	if rows, _, err := quests.List(ctx, mallory, mh.ID, app.QuestPage{Limit: 100}); err != nil {
		t.Errorf("mallory's own quest list: %v", err)
	} else {
		sawOwn := false
		for _, r := range rows {
			if r.HouseholdID != mh.ID || r.ID == aliceQuest.ID || r.ID == aliceQuest2.ID {
				t.Errorf("mallory's own quest list shows another household's quest: %+v", r.HouseholdQuest)
			}
			if r.ID == malloryQuest.ID {
				sawOwn = true
			}
		}
		if !sawOwn {
			t.Errorf("mallory's own quest list misses her quest %v: %+v", malloryQuest.ID, rows)
		}
	}
	if rows, err := quests.ListPending(ctx, alice, h.ID, ""); err != nil || len(rows) != 1 || rows[0].QuestID != alicePending.ID ||
		rows[0].MemberID != aliceMember.ID {
		t.Errorf("alice's pending quest was changed by the outsider: %+v, %v", rows, err)
	}
	// CLM-8: a shared pending completion lists only Alice's household members,
	// and no outsider sees it through either household's pending list.
	if _, err := quests.MarkDone(ctx, alice, h.ID, aliceShareable.ID, app.MemberAction{MemberID: aliceKid.ID}); err != nil {
		t.Fatalf("alice's kid marks the shared quest done: %v", err)
	}
	sharedPending, err := quests.ListPending(ctx, alice, h.ID, "")
	if err != nil {
		t.Fatalf("alice's pending list with a shared completion: %v", err)
	}
	var sharedCompletionID uuid.UUID
	for _, row := range sharedPending {
		if row.QuestID != aliceShareable.ID {
			continue
		}
		sharedCompletionID = row.ID
		if len(row.Members) != 2 {
			t.Errorf("shared pending item members = %+v; want exactly alice's two members", row.Members)
		}
		for _, m := range row.Members {
			if m.MemberID != aliceMember.ID && m.MemberID != aliceKid.ID {
				t.Errorf("shared pending item lists %s, who is not one of alice's members", m.MemberID)
			}
		}
	}
	if sharedCompletionID == uuid.Nil {
		t.Fatalf("shared completion missing from alice's pending list: %+v", sharedPending)
	}
	if rows, err := quests.ListPending(ctx, mallory, mh.ID, ""); err != nil || len(rows) != 0 {
		t.Errorf("mallory's pending list shows alice's shared completion: %+v, %v", rows, err)
	}
	if _, err := quests.ListPending(ctx, mallory, h.ID, ""); err == nil {
		t.Error("mallory listed alice's household pending completions")
	}
	// Put the quest back so the checks below start from the same state.
	if _, err := quests.Reject(ctx, alice, h.ID, sharedCompletionID, ""); err != nil {
		t.Fatalf("alice rejects the shared completion: %v", err)
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
	// With an admin PIN set, a wrong member PIN plus a missing or wrong admin
	// PIN -> ErrWrongPIN and no rows returned.
	wrongHist, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "0000", "", 50, 0)
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("wrong member PIN, missing admin PIN points history: expected ErrWrongPIN, got %v", err)
	}
	if len(wrongHist) != 0 {
		t.Errorf("wrong member PIN, missing admin PIN points history returned rows: %+v", wrongHist)
	}
	wrongHist2, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "0000", "0000", 50, 0)
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("wrong member and admin PIN points history: expected ErrWrongPIN, got %v", err)
	}
	if len(wrongHist2) != 0 {
		t.Errorf("wrong member and admin PIN points history returned rows: %+v", wrongHist2)
	}
	if err := familySvc.ClearAdminPin(ctx, alice, h.ID, "5555"); err != nil {
		t.Fatalf("ClearAdminPin for alice: %v", err)
	}

	// No admin PIN set: the household admin reads a PIN'd member's history
	// with no member PIN, and with a wrong one, whatever the member's PIN.
	if set, err := familySvc.AdminPinSet(ctx, alice, h.ID); err != nil || set {
		t.Fatalf("admin PIN set before no-PIN admin read = %v, %v; want false", set, err)
	}
	for _, memberPin := range []string{"", "0000"} {
		adminNoPinHist, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, memberPin, "", 50, 0)
		if err != nil {
			t.Fatalf("admin PointsHistory with no admin PIN set (member PIN %q): %v", memberPin, err)
		}
		if len(adminNoPinHist) != 1 || adminNoPinHist[0].ID != aliceAwardID {
			t.Errorf("admin no-PIN points history (member PIN %q) mismatch: got %+v, want award %v", memberPin, adminNoPinHist, aliceAwardID)
		}
	}

	// A non-admin caller with a wrong member PIN gets ErrWrongPIN, never
	// ErrForbidden; the right member PIN still reads. The service never makes
	// a non-admin login, so the role is flipped in the database and restored.
	if _, err := pool.Exec(ctx, "UPDATE household_members SET role = 'member' WHERE household_id = $1 AND user_id = $2", h.ID, alice); err != nil {
		t.Fatalf("demote alice: %v", err)
	}
	nonAdminHist, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "0000", "", 50, 0)
	if !errors.Is(err, app.ErrWrongPIN) || errors.Is(err, app.ErrForbidden) {
		t.Errorf("non-admin wrong member PIN points history: expected ErrWrongPIN, got %v", err)
	}
	if len(nonAdminHist) != 0 {
		t.Errorf("non-admin wrong member PIN points history returned rows: %+v", nonAdminHist)
	}
	if got, err := familySvc.PointsHistory(ctx, alice, h.ID, aliceMember.ID, "1234", "", 50, 0); err != nil || len(got) != 1 {
		t.Errorf("non-admin right member PIN points history = %+v, %v; want 1 award", got, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE household_members SET role = 'admin' WHERE household_id = $1 AND user_id = $2", h.ID, alice); err != nil {
		t.Fatalf("restore alice admin: %v", err)
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
		t.Fatalf("admin-path fault never reached the admin PIN check: err %v", err)
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

	f := &isolationFixture{
		pool:       pool,
		q:          q,
		ctx:        ctx,
		authSvc:    authSvc,
		households: households,
		quests:     quests,
		familySvc:  familySvc,
		rewards:    rewards,
		combos:     combos,
		alice:      alice,
		mallory:    mallory,
		bob:        bob,
		bobRes:     bobRes,
		h:          h,
		bh:         bh,
		aliceQuest: aliceQuest,
		aliceCombo: aliceCombo,
		asAlice:    asAlice,
		asBob:      asBob,
		bobMember:  bobMember,
		bobAwardID: bobAwardID,
		bobClaimed: bobClaimed,
		bobPending: bobPending,
		bobReward:  bobReward,
	}
	householdIsolationCombos(t, f)
	householdIsolationExpirySweep(t, f)
	householdIsolationDelete(t, f)
}
