//go:build integration

package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

func TestIntegration_FamilyMemberLifecycle(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "familyadmin").User.ID

	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Family Home", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}

	// Initial list is empty (non-nil)
	initialList, err := familySvc.List(ctx, admin, h.ID)
	if err != nil {
		t.Fatalf("List initial members: %v", err)
	}
	if initialList == nil || len(initialList) != 0 {
		t.Fatalf("expected empty non-nil slice, got: %+v", initialList)
	}

	// 1. Create family member without PIN
	m1, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Alice"}, "")
	if err != nil {
		t.Fatalf("Create Alice: %v", err)
	}
	if m1.Name != "Alice" {
		t.Errorf("Alice Name: got %q, want %q", m1.Name, "Alice")
	}
	if m1.Points != 0 {
		t.Errorf("Alice Points: got %d, want 0", m1.Points)
	}
	if m1.HasPin {
		t.Errorf("Alice HasPin: got true, want false")
	}

	// 2. Create family member with 4-digit PIN
	m2, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Bob", PIN: "1234"}, "")
	if err != nil {
		t.Fatalf("Create Bob: %v", err)
	}
	if m2.Name != "Bob" {
		t.Errorf("Bob Name: got %q, want %q", m2.Name, "Bob")
	}
	if m2.Points != 0 {
		t.Errorf("Bob Points: got %d, want 0", m2.Points)
	}
	if !m2.HasPin {
		t.Errorf("Bob HasPin: got false, want true")
	}

	// 3. List members
	members, err := familySvc.List(ctx, admin, h.ID)
	if err != nil {
		t.Fatalf("List members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("List count: got %d, want 2", len(members))
	}
	if members[0].Name != "Alice" || members[1].Name != "Bob" {
		t.Errorf("List order: got [%s, %s], want [Alice, Bob]", members[0].Name, members[1].Name)
	}

	// 4. Get member
	gotM1, err := familySvc.Get(ctx, admin, h.ID, m1.ID)
	if err != nil {
		t.Fatalf("Get m1: %v", err)
	}
	if gotM1.ID != m1.ID || gotM1.Name != "Alice" {
		t.Errorf("Get m1: got %+v", gotM1)
	}

	// 5. Rename member
	renamed, err := familySvc.Rename(ctx, admin, h.ID, m1.ID, app.RenameMemberInput{Name: "Alicia"}, "")
	if err != nil {
		t.Fatalf("Rename Alice: %v", err)
	}
	if renamed.Name != "Alicia" {
		t.Errorf("Renamed Name: got %q, want Alicia", renamed.Name)
	}

	// 6. Set PIN
	withPin, err := familySvc.SetPin(ctx, admin, h.ID, m1.ID, "4321", "")
	if err != nil {
		t.Fatalf("SetPin m1: %v", err)
	}
	if !withPin.HasPin {
		t.Errorf("SetPin HasPin: got false, want true")
	}

	// 7. Clear PIN
	cleared, err := familySvc.ClearPin(ctx, admin, h.ID, m1.ID, "")
	if err != nil {
		t.Fatalf("ClearPin m1: %v", err)
	}
	if cleared.HasPin {
		t.Errorf("ClearPin HasPin: got true, want false")
	}

	// 8. Removal deletes no users row
	var usersBefore int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&usersBefore); err != nil {
		t.Fatalf("count users before: %v", err)
	}

	if err := familySvc.Remove(ctx, admin, h.ID, m1.ID, ""); err != nil {
		t.Fatalf("Remove m1: %v", err)
	}

	var usersAfter int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&usersAfter); err != nil {
		t.Fatalf("count users after: %v", err)
	}
	if usersBefore != usersAfter {
		t.Errorf("users row count changed on family member delete: before %d, after %d", usersBefore, usersAfter)
	}

	// Verify member is gone
	if _, err := familySvc.Get(ctx, admin, h.ID, m1.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Get deleted member: expected ErrNotFound, got %v", err)
	}
}

func TestIntegration_FamilyMemberValidation(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "valadmin").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Validation Home"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}

	// Empty name
	var vErr *app.ValidationError
	_, err = familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: ""}, "")
	if !errors.As(err, &vErr) || vErr.Fields["name"] == "" {
		t.Errorf("empty name: expected ValidationError on name, got %v", err)
	}

	// Name too long (>50 chars)
	tooLong := strings.Repeat("a", 51)
	_, err = familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: tooLong}, "")
	if !errors.As(err, &vErr) || vErr.Fields["name"] == "" {
		t.Errorf("too long name: expected ValidationError on name, got %v", err)
	}

	// Invalid PINs
	for _, badPin := range []string{"123", "12345", "abcd", "12a4"} {
		_, err = familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid", PIN: badPin}, "")
		if !errors.As(err, &vErr) || vErr.Fields["pin"] == "" {
			t.Errorf("bad pin %q: expected ValidationError on pin, got %v", badPin, err)
		}
	}

	// Rename with empty name
	m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "ValidKid"}, "")
	if err != nil {
		t.Fatalf("create valid kid: %v", err)
	}
	_, err = familySvc.Rename(ctx, admin, h.ID, m.ID, app.RenameMemberInput{Name: ""}, "")
	if !errors.As(err, &vErr) || vErr.Fields["name"] == "" {
		t.Errorf("empty rename: expected ValidationError on name, got %v", err)
	}

	// SetPin with bad pin
	_, err = familySvc.SetPin(ctx, admin, h.ID, m.ID, "999", "")
	if !errors.As(err, &vErr) || vErr.Fields["pin"] == "" {
		t.Errorf("bad setpin: expected ValidationError on pin, got %v", err)
	}
}

func TestIntegration_FamilyMemberIsolation(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "aliceadmin").User.ID
	mallory := registerVerified(t, pool, authSvc, mailer, "mallory").User.ID

	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Alice Home"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}

	m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid Alice"}, "")
	if err != nil {
		t.Fatalf("Create member: %v", err)
	}

	checks := map[string]error{}
	_, checks["list members"] = familySvc.List(ctx, mallory, h.ID)
	_, checks["get member"] = familySvc.Get(ctx, mallory, h.ID, m.ID)
	_, checks["create member"] = familySvc.Create(ctx, mallory, h.ID, app.FamilyMemberInput{Name: "Mallory Plant"}, "")
	_, checks["rename member"] = familySvc.Rename(ctx, mallory, h.ID, m.ID, app.RenameMemberInput{Name: "Hacked"}, "")
	checks["remove member"] = familySvc.Remove(ctx, mallory, h.ID, m.ID, "")
	_, checks["set pin"] = familySvc.SetPin(ctx, mallory, h.ID, m.ID, "1234", "")
	_, checks["clear pin"] = familySvc.ClearPin(ctx, mallory, h.ID, m.ID, "")

	for name, err := range checks {
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s: expected ErrNotFound, got %v", name, err)
		}
	}

	// Nonexistent member ID from same household
	fakeID := uuid.New()
	if _, err := familySvc.Get(ctx, admin, h.ID, fakeID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("get nonexistent: expected ErrNotFound, got %v", err)
	}
	if err := familySvc.Remove(ctx, admin, h.ID, fakeID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("remove nonexistent: expected ErrNotFound, got %v", err)
	}
}

func TestIntegration_VerifyMemberPin(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "pinadmin").User.ID
	otherAdmin := registerVerified(t, pool, authSvc, mailer, "otherpinadmin").User.ID

	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Pin House"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 1. Member with PIN "1234"
	mWithPin, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid1", PIN: "1234"}, "")
	if err != nil {
		t.Fatalf("Create Kid1: %v", err)
	}

	// 2. Member without PIN
	mNoPin, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid2"}, "")
	if err != nil {
		t.Fatalf("Create Kid2: %v", err)
	}

	// Verify Kid1: correct pin
	res1, err := familySvc.Verify(ctx, admin, h.ID, mWithPin.ID, "1234")
	if err != nil {
		t.Fatalf("Verify Kid1 correct pin: %v", err)
	}
	if res1.ID != mWithPin.ID || res1.Name != "Kid1" || !res1.HasPin {
		t.Errorf("Verify Kid1 got %+v", res1)
	}

	// Verify Kid1: wrong pin
	_, err = familySvc.Verify(ctx, admin, h.ID, mWithPin.ID, "0000")
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Verify Kid1 wrong pin: expected ErrWrongPIN, got %v", err)
	}

	// Verify Kid1: empty pin
	_, err = familySvc.Verify(ctx, admin, h.ID, mWithPin.ID, "")
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Verify Kid1 empty pin: expected ErrWrongPIN, got %v", err)
	}

	// Verify Kid2: empty pin succeeds
	res2, err := familySvc.Verify(ctx, admin, h.ID, mNoPin.ID, "")
	if err != nil {
		t.Fatalf("Verify Kid2 empty pin: %v", err)
	}
	if res2.ID != mNoPin.ID || res2.Name != "Kid2" || res2.HasPin {
		t.Errorf("Verify Kid2 got %+v", res2)
	}

	// Verify Kid2: non-empty pin fails with ErrWrongPIN
	_, err = familySvc.Verify(ctx, admin, h.ID, mNoPin.ID, "1234")
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Verify Kid2 with pin: expected ErrWrongPIN, got %v", err)
	}

	// Verify with outsider user: ErrNotFound
	_, err = familySvc.Verify(ctx, otherAdmin, h.ID, mWithPin.ID, "1234")
	if !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Verify outsider: expected ErrNotFound, got %v", err)
	}

	// Verify non-existent member
	_, err = familySvc.Verify(ctx, admin, h.ID, uuid.New(), "1234")
	if !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Verify nonexistent member: expected ErrNotFound, got %v", err)
	}

	// Standalone VerifyMemberPin helper directly
	dbM, err := app.VerifyMemberPin(ctx, q, h.ID, mWithPin.ID, "1234")
	if err != nil {
		t.Fatalf("VerifyMemberPin direct: %v", err)
	}
	if dbM.ID != mWithPin.ID {
		t.Errorf("VerifyMemberPin returned member id %v, want %v", dbM.ID, mWithPin.ID)
	}
}

func TestIntegration_AdminPin(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "adminpinuser").User.ID
	mallory := registerVerified(t, pool, authSvc, mailer, "adminpinmallory").User.ID

	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Admin Pin House"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Initially admin PIN is not set
	set, err := familySvc.AdminPinSet(ctx, admin, h.ID)
	if err != nil {
		t.Fatalf("AdminPinSet: %v", err)
	}
	if set {
		t.Errorf("expected AdminPinSet false initially, got true")
	}

	// While not set, management writes succeed without adminPin header
	m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid"}, "")
	if err != nil {
		t.Fatalf("Create without admin pin when none set: %v", err)
	}

	// Set admin PIN "5678"
	if err := familySvc.SetAdminPin(ctx, admin, h.ID, "5678", ""); err != nil {
		t.Fatalf("SetAdminPin: %v", err)
	}

	// AdminPinSet is now true
	set, err = familySvc.AdminPinSet(ctx, admin, h.ID)
	if err != nil || !set {
		t.Fatalf("AdminPinSet: got %v, err %v", set, err)
	}

	// VerifyAdminPin
	if err := familySvc.VerifyAdminPin(ctx, admin, h.ID, "5678"); err != nil {
		t.Errorf("VerifyAdminPin correct: %v", err)
	}
	if err := familySvc.VerifyAdminPin(ctx, admin, h.ID, "0000"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("VerifyAdminPin wrong: expected ErrWrongPIN, got %v", err)
	}

	// Now management writes require admin PIN:
	// Missing PIN -> ErrAdminPinRequired
	_, err = familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid2"}, "")
	if !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("Create without pin when set: expected ErrAdminPinRequired, got %v", err)
	}

	// Wrong PIN -> ErrWrongPIN
	_, err = familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid2"}, "9999")
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Create with wrong pin: expected ErrWrongPIN, got %v", err)
	}

	// Correct PIN -> Success
	m2, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Kid2"}, "5678")
	if err != nil {
		t.Fatalf("Create with correct pin: %v", err)
	}

	// Rename with wrong PIN -> ErrWrongPIN
	_, err = familySvc.Rename(ctx, admin, h.ID, m2.ID, app.RenameMemberInput{Name: "NewName"}, "1111")
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Rename with wrong pin: expected ErrWrongPIN, got %v", err)
	}

	// Rename with right PIN -> Success
	if _, err := familySvc.Rename(ctx, admin, h.ID, m2.ID, app.RenameMemberInput{Name: "NewName"}, "5678"); err != nil {
		t.Fatalf("Rename with right pin: %v", err)
	}

	// Set member PIN with wrong PIN -> ErrWrongPIN
	_, err = familySvc.SetPin(ctx, admin, h.ID, m2.ID, "1234", "1111")
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("SetPin with wrong admin pin: expected ErrWrongPIN, got %v", err)
	}

	// Set member PIN with right PIN -> Success
	if _, err := familySvc.SetPin(ctx, admin, h.ID, m2.ID, "1234", "5678"); err != nil {
		t.Fatalf("SetPin with right admin pin: %v", err)
	}

	// Clear member PIN with wrong PIN -> ErrWrongPIN
	_, err = familySvc.ClearPin(ctx, admin, h.ID, m2.ID, "1111")
	if !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("ClearPin with wrong admin pin: expected ErrWrongPIN, got %v", err)
	}

	// Clear member PIN with right PIN -> Success
	if _, err := familySvc.ClearPin(ctx, admin, h.ID, m2.ID, "5678"); err != nil {
		t.Fatalf("ClearPin with right admin pin: %v", err)
	}

	// Change admin PIN requires current admin PIN:
	// Wrong current PIN -> ErrWrongPIN
	if err := familySvc.SetAdminPin(ctx, admin, h.ID, "9999", "wrong"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Change admin pin with wrong current: expected ErrWrongPIN, got %v", err)
	}
	// Missing current PIN -> ErrAdminPinRequired
	if err := familySvc.SetAdminPin(ctx, admin, h.ID, "9999", ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("Change admin pin with missing current: expected ErrAdminPinRequired, got %v", err)
	}
	// Right current PIN -> Success
	if err := familySvc.SetAdminPin(ctx, admin, h.ID, "9999", "5678"); err != nil {
		t.Fatalf("Change admin pin with right current: %v", err)
	}

	// Clear admin PIN:
	// Wrong current PIN -> ErrWrongPIN
	if err := familySvc.ClearAdminPin(ctx, admin, h.ID, "5678"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("Clear admin pin with old pin: expected ErrWrongPIN, got %v", err)
	}
	// Right current PIN ("9999") -> Success
	if err := familySvc.ClearAdminPin(ctx, admin, h.ID, "9999"); err != nil {
		t.Fatalf("Clear admin pin with right pin: %v", err)
	}

	// AdminPinSet is false again
	set, err = familySvc.AdminPinSet(ctx, admin, h.ID)
	if err != nil || set {
		t.Fatalf("AdminPinSet after clear: got %v, err %v", set, err)
	}

	// Gate reopened: Remove member without admin PIN header succeeds
	if err := familySvc.Remove(ctx, admin, h.ID, m.ID, ""); err != nil {
		t.Fatalf("Remove member after admin pin cleared: %v", err)
	}

	// Non-member isolation
	_, err = familySvc.AdminPinSet(ctx, mallory, h.ID)
	if !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory AdminPinSet: expected ErrNotFound, got %v", err)
	}
	err = familySvc.SetAdminPin(ctx, mallory, h.ID, "1234", "")
	if !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory SetAdminPin: expected ErrNotFound, got %v", err)
	}
	err = familySvc.ClearAdminPin(ctx, mallory, h.ID, "")
	if !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory ClearAdminPin: expected ErrNotFound, got %v", err)
	}
	err = familySvc.VerifyAdminPin(ctx, mallory, h.ID, "1234")
	if !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory VerifyAdminPin: expected ErrNotFound, got %v", err)
	}
}

// TestIntegration_RemoveMemberHoldingQuests: removing a member who holds a
// quest, accepted or waiting for approval, succeeds and returns the quest to
// the pool; other members' quests are untouched.
func TestIntegration_RemoveMemberHoldingQuests(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "removeholder").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Holders"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	ann, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Ann"}, "")
	if err != nil {
		t.Fatalf("create Ann: %v", err)
	}
	ben, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Ben"}, "")
	if err != nil {
		t.Fatalf("create Ben: %v", err)
	}

	pending, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Pending", Points: app.NumberField{Set: true, Value: 5}}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	accepted, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Accepted"}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	others, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Bens"}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	annAct := app.MemberAction{MemberID: ann.ID}
	for _, id := range []uuid.UUID{pending.ID, accepted.ID} {
		if _, err := quests.Accept(ctx, admin, h.ID, id, annAct); err != nil {
			t.Fatalf("Ann accept: %v", err)
		}
	}
	if _, err := quests.MarkDone(ctx, admin, h.ID, pending.ID, annAct); err != nil {
		t.Fatalf("Ann mark done: %v", err)
	}
	// Ben earns points first, so a stray credit to him would show.
	warmup, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Warmup", Points: app.NumberField{Set: true, Value: 3}}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	benAct := app.MemberAction{MemberID: ben.ID}
	if _, err := quests.Accept(ctx, admin, h.ID, warmup.ID, benAct); err != nil {
		t.Fatalf("Ben accept warmup: %v", err)
	}
	if _, err := quests.MarkDone(ctx, admin, h.ID, warmup.ID, benAct); err != nil {
		t.Fatalf("Ben mark warmup done: %v", err)
	}
	if _, _, err := quests.Approve(ctx, admin, h.ID, warmup.ID, ""); err != nil {
		t.Fatalf("approve warmup: %v", err)
	}
	if _, err := quests.Accept(ctx, admin, h.ID, others.ID, benAct); err != nil {
		t.Fatalf("Ben accept: %v", err)
	}
	before, err := familySvc.List(ctx, admin, h.ID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	pointsBefore := map[uuid.UUID]int32{}
	for _, m := range before {
		pointsBefore[m.ID] = m.Points
	}
	if pointsBefore[ben.ID] != 3 {
		t.Fatalf("Ben's points before removal = %d, want 3", pointsBefore[ben.ID])
	}

	if err := familySvc.Remove(ctx, admin, h.ID, ann.ID, ""); err != nil {
		t.Fatalf("Remove member holding quests: %v", err)
	}

	if _, err := familySvc.Get(ctx, admin, h.ID, ann.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("removed member still readable: %v", err)
	}
	for _, id := range []uuid.UUID{pending.ID, accepted.ID} {
		got, err := quests.Get(ctx, admin, h.ID, id)
		if err != nil {
			t.Fatalf("Get quest: %v", err)
		}
		if got.ClaimedBy != nil || got.DoneAt.Valid || got.ClaimedAt.Valid {
			t.Errorf("quest %s not released: %+v", got.Name, got)
		}
		if !app.QuestAvailable(got, time.Now()) {
			t.Errorf("released quest %s is not available: %+v", got.Name, got)
		}
	}
	after, err := familySvc.List(ctx, admin, h.ID)
	if err != nil {
		t.Fatalf("list members after: %v", err)
	}
	if len(after) != len(before)-1 {
		t.Errorf("members after removal = %d, want %d", len(after), len(before)-1)
	}
	for _, m := range after {
		if want, ok := pointsBefore[m.ID]; !ok || m.Points != want {
			t.Errorf("member %s points = %d, want unchanged %d", m.Name, m.Points, want)
		}
	}
	if pend, err := quests.ListPending(ctx, admin, h.ID, ""); err != nil || len(pend) != 0 {
		t.Errorf("pending after removal: %v %v", pend, err)
	}
	got, err := quests.Get(ctx, admin, h.ID, others.ID)
	if err != nil || got.ClaimedBy == nil || *got.ClaimedBy != ben.ID {
		t.Errorf("Ben's quest changed: %+v %v", got, err)
	}
}

// TestIntegration_RemoveMemberRollsBack: the release and the delete are one
// transaction. When the member delete fails after the release ran, the
// release is undone: the member is still there and still holds their
// accepted and pending quests, and the caller gets an error.
func TestIntegration_RemoveMemberRollsBack(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "removerollback").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Rollback"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	ann, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Ann"}, "")
	if err != nil {
		t.Fatalf("create Ann: %v", err)
	}
	pending, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Pending", Points: app.NumberField{Set: true, Value: 4}}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	accepted, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Accepted"}, "")
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	annAct := app.MemberAction{MemberID: ann.ID}
	for _, id := range []uuid.UUID{pending.ID, accepted.ID} {
		if _, err := quests.Accept(ctx, admin, h.ID, id, annAct); err != nil {
			t.Fatalf("Ann accept: %v", err)
		}
	}
	marked, err := quests.MarkDone(ctx, admin, h.ID, pending.ID, annAct)
	if err != nil {
		t.Fatalf("Ann mark done: %v", err)
	}

	// The delete of this one member row fails; every other row is untouched.
	fn := "fail_member_delete_" + strings.ReplaceAll(ann.ID.String(), "-", "")
	ddl := `CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF OLD.id = '` + ann.ID.String() + `'::uuid THEN
		RAISE EXCEPTION 'member delete refused for the test';
	END IF;
	RETURN OLD;
END $$;
CREATE TRIGGER ` + fn + ` BEFORE DELETE ON family_members FOR EACH ROW EXECUTE FUNCTION ` + fn + `();`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON family_members; DROP FUNCTION IF EXISTS `+fn+`();`); err != nil {
			t.Errorf("drop failing trigger: %v", err)
		}
	})

	if err := familySvc.Remove(ctx, admin, h.ID, ann.ID, ""); err == nil {
		t.Fatal("Remove succeeded although the member delete failed")
	}

	if _, err := familySvc.Get(ctx, admin, h.ID, ann.ID); err != nil {
		t.Errorf("member gone after a failed removal: %v", err)
	}
	gotPending, err := quests.Get(ctx, admin, h.ID, pending.ID)
	if err != nil {
		t.Fatalf("Get pending: %v", err)
	}
	if gotPending.ClaimedBy == nil || *gotPending.ClaimedBy != ann.ID || !gotPending.DoneAt.Valid ||
		!gotPending.DoneAt.Time.Equal(marked.DoneAt.Time) {
		t.Errorf("pending quest changed by a failed removal: %+v", gotPending)
	}
	gotAccepted, err := quests.Get(ctx, admin, h.ID, accepted.ID)
	if err != nil {
		t.Fatalf("Get accepted: %v", err)
	}
	if gotAccepted.ClaimedBy == nil || *gotAccepted.ClaimedBy != ann.ID || gotAccepted.DoneAt.Valid || !gotAccepted.ClaimedAt.Valid {
		t.Errorf("accepted quest changed by a failed removal: %+v", gotAccepted)
	}
	if pend, err := quests.ListPending(ctx, admin, h.ID, ""); err != nil || len(pend) != 1 || pend[0].HouseholdQuest.ID != pending.ID {
		t.Errorf("pending list after a failed removal: %+v %v", pend, err)
	}
}

// TestIntegration_RemoveMemberRacesQuestActions: an Accept or MarkDone by a
// member who is being removed either lands first and is released by the
// removal, or fails with ErrNotFound after it. No quest is left held by, or
// waiting on, a removed member, and no other error surfaces.
func TestIntegration_RemoveMemberRacesQuestActions(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "removerace").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Race"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	newQuest := func(t *testing.T, name string) uuid.UUID {
		t.Helper()
		quest, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: name, Points: app.NumberField{Set: true, Value: 2}}, "")
		if err != nil {
			t.Fatalf("create quest %s: %v", name, err)
		}
		return quest.ID
	}
	// heldBy counts the household's quests that still name memberID.
	heldBy := func(t *testing.T, memberID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM household_quests WHERE household_id = $1 AND claimed_by = $2`, h.ID, memberID).Scan(&n); err != nil {
			t.Fatalf("count held quests: %v", err)
		}
		return n
	}
	okAfterRemoval := func(t *testing.T, what string, err error) {
		t.Helper()
		if err != nil && !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s racing the removal: %v, want success or ErrNotFound", what, err)
		}
	}

	t.Run("the member row lock holds back a concurrent accept", func(t *testing.T) {
		ann, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Ann"}, "")
		if err != nil {
			t.Fatalf("create Ann: %v", err)
		}
		held, free := newQuest(t, "Held"), newQuest(t, "Free")
		annAct := app.MemberAction{MemberID: ann.ID}
		if _, err := quests.Accept(ctx, admin, h.ID, held, annAct); err != nil {
			t.Fatalf("Ann accept: %v", err)
		}

		// Take the lock Remove takes, and hold it.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := q.WithTx(tx).LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: h.ID, ID: ann.ID}); err != nil {
			t.Fatalf("lock member: %v", err)
		}

		acceptErr, markErr := make(chan error, 1), make(chan error, 1)
		go func() { _, err := quests.Accept(ctx, admin, h.ID, free, annAct); acceptErr <- err }()
		go func() { _, err := quests.MarkDone(ctx, admin, h.ID, held, annAct); markErr <- err }()

		waitForLockWait(t, pool)
		select {
		case err := <-acceptErr:
			t.Fatalf("Accept finished while the member row was locked: %v", err)
		default:
		}

		removeErr := make(chan error, 1)
		go func() { removeErr <- familySvc.Remove(ctx, admin, h.ID, ann.ID, "") }()
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("release lock: %v", err)
		}

		if err := <-removeErr; err != nil {
			t.Fatalf("Remove: %v", err)
		}
		okAfterRemoval(t, "Accept", <-acceptErr)
		okAfterRemoval(t, "MarkDone", <-markErr)
		if n := heldBy(t, ann.ID); n != 0 {
			t.Errorf("%d quests still held by the removed member", n)
		}
		if pend, err := quests.ListPending(ctx, admin, h.ID, ""); err != nil || len(pend) != 0 {
			t.Errorf("pending after removal: %+v %v", pend, err)
		}
	})

	t.Run("concurrent accepts and mark-done against remove", func(t *testing.T) {
		const rounds, accepts = 10, 4
		for round := range rounds {
			member, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Racer"}, "")
			if err != nil {
				t.Fatalf("round %d: create member: %v", round, err)
			}
			act := app.MemberAction{MemberID: member.ID}
			held := newQuest(t, "Held")
			if _, err := quests.Accept(ctx, admin, h.ID, held, act); err != nil {
				t.Fatalf("round %d: accept held: %v", round, err)
			}
			free := make([]uuid.UUID, accepts)
			for i := range free {
				free[i] = newQuest(t, "Free")
			}

			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, accepts+1)
			for _, id := range free {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := quests.Accept(ctx, admin, h.ID, id, act)
					errs <- err
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := quests.MarkDone(ctx, admin, h.ID, held, act)
				errs <- err
			}()
			var removeErr error
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				removeErr = familySvc.Remove(ctx, admin, h.ID, member.ID, "")
			}()
			close(start)
			wg.Wait()
			close(errs)

			if removeErr != nil {
				t.Fatalf("round %d: Remove: %v", round, removeErr)
			}
			for err := range errs {
				okAfterRemoval(t, "quest action", err)
			}
			if n := heldBy(t, member.ID); n != 0 {
				t.Errorf("round %d: %d quests still held by the removed member", round, n)
			}
		}
		if pend, err := quests.ListPending(ctx, admin, h.ID, ""); err != nil || len(pend) != 0 {
			t.Errorf("pending after removals: %+v %v", pend, err)
		}
	})
}

// waitForLockWait waits, up to five seconds, until a session of this database
// is waiting on a lock.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		if err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session waited on the member row lock within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
