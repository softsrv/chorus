//go:build integration

package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// TestIntegration_FamilyMemberBirthday covers SetBirthday and ListBirthdays
// (CLM-4, CLM-5): set and clear round-trip, the age is quest_member_age in
// the household's timezone, bad dates are a 422 on birthday that saves
// nothing, and both are gated by the admin PIN.
func TestIntegration_FamilyMemberBirthday(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "bdayadmin").User.ID
	const tz = "Pacific/Kiritimati" // UTC+14: its today is often not UTC's
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Birthday Home", Timezone: tz})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}
	sam, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Sam"}, "")
	if err != nil {
		t.Fatalf("create Sam: %v", err)
	}
	ava, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Ava"}, "")
	if err != nil {
		t.Fatalf("create Ava: %v", err)
	}

	stored := func(id uuid.UUID) string {
		t.Helper()
		var s *string
		if err := pool.QueryRow(ctx, "SELECT to_char(birthday, 'YYYY-MM-DD') FROM family_members WHERE id = $1", id).Scan(&s); err != nil {
			t.Fatalf("read birthday: %v", err)
		}
		if s == nil {
			return ""
		}
		return *s
	}
	// The age the database's one rule gives, in the household's timezone.
	sqlAge := func(birthday string) int32 {
		t.Helper()
		var age int32
		if err := pool.QueryRow(ctx, "SELECT quest_member_age($1::date, $2)", birthday, tz).Scan(&age); err != nil {
			t.Fatalf("quest_member_age: %v", err)
		}
		return age
	}

	// Set: stored, returned, with the SQL age.
	bd := "2015-04-02"
	got, err := familySvc.SetBirthday(ctx, admin, h.ID, sam.ID, &bd, "")
	if err != nil {
		t.Fatalf("SetBirthday: %v", err)
	}
	if got.MemberID != sam.ID || got.Birthday == nil || *got.Birthday != bd {
		t.Fatalf("SetBirthday = %+v, want Sam %s", got, bd)
	}
	if got.Age == nil || *got.Age != sqlAge(bd) {
		t.Errorf("SetBirthday age = %v, want %d", got.Age, sqlAge(bd))
	}
	if s := stored(sam.ID); s != bd {
		t.Errorf("stored birthday = %q, want %q", s, bd)
	}

	// Today in the household's timezone is allowed (age 0); tomorrow is not.
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayS := today.Format(time.DateOnly)
	if b, err := familySvc.SetBirthday(ctx, admin, h.ID, ava.ID, &todayS, ""); err != nil {
		t.Errorf("birthday today in %s: %v", tz, err)
	} else if b.Age == nil || *b.Age != 0 {
		t.Errorf("birthday today age = %v, want 0", b.Age)
	}

	// List: one item per member, the cleared one with null birthday and age.
	if _, err := familySvc.SetBirthday(ctx, admin, h.ID, ava.ID, nil, ""); err != nil {
		t.Fatalf("clear Ava: %v", err)
	}
	if s := stored(ava.ID); s != "" {
		t.Errorf("Ava birthday after clear = %q, want unset", s)
	}
	list, err := familySvc.ListBirthdays(ctx, admin, h.ID, "")
	if err != nil {
		t.Fatalf("ListBirthdays: %v", err)
	}
	byID := map[uuid.UUID]app.MemberBirthday{}
	for _, b := range list {
		byID[b.MemberID] = b
	}
	if len(list) != 2 {
		t.Fatalf("ListBirthdays = %+v, want 2 items", list)
	}
	if b := byID[ava.ID]; b.Birthday != nil || b.Age != nil {
		t.Errorf("Ava = %+v, want null birthday and age", b)
	}
	if b := byID[sam.ID]; b.Birthday == nil || *b.Birthday != bd || b.Age == nil || *b.Age != sqlAge(bd) {
		t.Errorf("Sam = %+v, want %s age %d", b, bd, sqlAge(bd))
	}

	// Bad dates: 422 on birthday, nothing saved.
	tomorrow := today.AddDate(0, 0, 1).Format(time.DateOnly)
	for _, bad := range []string{tomorrow, "", "2015-02-30", "2015-13-01", "02/04/2015", "2015-4-2", "not a date"} {
		var vErr *app.ValidationError
		_, err := familySvc.SetBirthday(ctx, admin, h.ID, sam.ID, &bad, "")
		if !errors.As(err, &vErr) || vErr.Fields["birthday"] == "" {
			t.Errorf("birthday %q: expected ValidationError on birthday, got %v", bad, err)
		}
	}
	if s := stored(sam.ID); s != bd {
		t.Errorf("birthday after refused sets = %q, want %q", s, bd)
	}

	// An unknown member is not found.
	if _, err := familySvc.SetBirthday(ctx, admin, h.ID, uuid.New(), &bd, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("unknown member: %v, want ErrNotFound", err)
	}

	// With an admin PIN set: missing is ErrAdminPinRequired, wrong ErrWrongPIN,
	// and neither reads or changes anything.
	if err := familySvc.SetAdminPin(ctx, admin, h.ID, "2468", ""); err != nil {
		t.Fatalf("SetAdminPin: %v", err)
	}
	other := "2010-01-01"
	if _, err := familySvc.SetBirthday(ctx, admin, h.ID, sam.ID, &other, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("SetBirthday no PIN: %v, want ErrAdminPinRequired", err)
	}
	if _, err := familySvc.SetBirthday(ctx, admin, h.ID, sam.ID, nil, "1111"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("SetBirthday wrong PIN: %v, want ErrWrongPIN", err)
	}
	if s := stored(sam.ID); s != bd {
		t.Errorf("birthday after PIN refusals = %q, want %q", s, bd)
	}
	if _, err := familySvc.ListBirthdays(ctx, admin, h.ID, ""); !errors.Is(err, app.ErrAdminPinRequired) {
		t.Errorf("ListBirthdays no PIN: %v, want ErrAdminPinRequired", err)
	}
	if _, err := familySvc.ListBirthdays(ctx, admin, h.ID, "1111"); !errors.Is(err, app.ErrWrongPIN) {
		t.Errorf("ListBirthdays wrong PIN: %v, want ErrWrongPIN", err)
	}
	if b, err := familySvc.SetBirthday(ctx, admin, h.ID, sam.ID, &other, "2468"); err != nil || b.Birthday == nil || *b.Birthday != other {
		t.Errorf("SetBirthday right PIN: %+v %v", b, err)
	}
	if l, err := familySvc.ListBirthdays(ctx, admin, h.ID, "2468"); err != nil || len(l) != 2 {
		t.Errorf("ListBirthdays right PIN: %+v %v", l, err)
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
