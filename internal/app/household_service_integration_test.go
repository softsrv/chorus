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
	quests := app.NewQuestService(q)
	familySvc := app.NewFamilyService(q, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "alice").User.ID
	mallory := registerVerified(t, pool, authSvc, mailer, "mallory").User.ID
	bob := registerVerified(t, pool, authSvc, mailer, "bob").User.ID // member, no Google connection

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

	// Assigning a chore to a non-member is rejected.
	_, err = chores.Create(ctx, alice, h.ID, app.ChoreInput{Title: "Mow", AssigneeID: &mallory})
	var vErr *app.ValidationError
	if !errors.As(err, &vErr) || vErr.Fields["assignee_id"] == "" {
		t.Errorf("assign non-member: expected assignee_id validation error, got %v", err)
	}

	// Alice's data is intact.
	if got, err := chores.Get(ctx, alice, h.ID, chore.ID); err != nil || got.Title != "Dishes" || got.CompletedAt.Valid {
		t.Errorf("alice's chore was modified: %+v, %v", got, err)
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
