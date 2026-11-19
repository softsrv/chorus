//go:build integration

package app_test

import (
	"context"
	"errors"
	"slices"
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

// TestIntegration_RemoveMemberHoldingQuests: removing a member who holds an
// accepted quest or has a completion waiting for approval succeeds: the held
// quest returns to the pool, the waiting completion is cancelled with no
// points, and a quest resting on the cooldown that completion started (marked
// done just now, so the cooldown is still running) is available again at
// once, as on reject; other members' cooldowns, quests and approved history
// are untouched.
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
	waiting, err := quests.ListPending(ctx, admin, h.ID, "")
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	var warmupCompletion uuid.UUID
	for _, p := range waiting {
		if p.QuestID == warmup.ID {
			warmupCompletion = p.ID
		}
	}
	if warmupCompletion == uuid.Nil {
		t.Fatalf("Ben's warmup completion not pending: %+v", waiting)
	}
	if _, _, _, _, err := quests.Approve(ctx, admin, h.ID, warmupCompletion, ""); err != nil {
		t.Fatalf("approve warmup: %v", err)
	}
	if _, err := quests.Accept(ctx, admin, h.ID, others.ID, benAct); err != nil {
		t.Fatalf("Ben accept: %v", err)
	}
	// Ann's marked-done chore is resting on the cooldown her pending
	// completion started; the removal must free it. Ben's approved warmup
	// rests on his own cooldown, which the removal must not move.
	pendingBefore, err := quests.Get(ctx, admin, h.ID, pending.ID)
	if err != nil {
		t.Fatalf("Get pending quest before removal: %v", err)
	}
	if !pendingBefore.AvailableAt.Valid || !pendingBefore.AvailableAt.Time.After(time.Now()) || !pendingBefore.AwaitingApproval ||
		app.QuestAvailable(pendingBefore.HouseholdQuest, time.Now()) {
		t.Fatalf("marked-done quest should be resting in a running cooldown and awaiting approval: %+v", pendingBefore)
	}
	warmupBefore, err := quests.Get(ctx, admin, h.ID, warmup.ID)
	if err != nil {
		t.Fatalf("Get warmup before removal: %v", err)
	}
	if !warmupBefore.AvailableAt.Valid || !warmupBefore.AvailableAt.Time.After(time.Now()) {
		t.Fatalf("Ben's warmup should be resting in a running cooldown: %+v", warmupBefore)
	}
	completionStatuses := func(memberID, questID uuid.UUID) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT status FROM quest_completions WHERE household_id = $1 AND quest_id = $2 AND member_id = $3 ORDER BY done_at`, h.ID, questID, memberID)
		if err != nil {
			t.Fatalf("query completions: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan completion: %v", err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read completions: %v", err)
		}
		return out
	}
	if got := completionStatuses(ann.ID, pending.ID); len(got) != 1 || got[0] != "pending" {
		t.Fatalf("Ann's completions before removal = %v, want [pending]", got)
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
	gotAccepted, err := quests.Get(ctx, admin, h.ID, accepted.ID)
	if err != nil {
		t.Fatalf("Get accepted quest: %v", err)
	}
	if gotAccepted.ClaimedBy != nil || gotAccepted.ClaimedAt.Valid {
		t.Errorf("accepted quest not released: %+v", gotAccepted)
	}
	if !app.QuestAvailable(gotAccepted.HouseholdQuest, time.Now()) {
		t.Errorf("released quest is not available: %+v", gotAccepted)
	}
	// The cooldown Ann's cancelled completion started is cancelled with it,
	// so the chore is back in the pool now, not at its old Back date; its
	// waiting completion is cancelled, so nothing is left to approve.
	gotPending, err := quests.Get(ctx, admin, h.ID, pending.ID)
	if err != nil {
		t.Fatalf("Get pending quest: %v", err)
	}
	if gotPending.ClaimedBy != nil || !app.QuestAvailable(gotPending.HouseholdQuest, time.Now()) ||
		!gotPending.AvailableAt.Valid || !gotPending.AvailableAt.Time.Before(pendingBefore.AvailableAt.Time) {
		t.Errorf("removal left the chore resting on the cancelled completion's cooldown: before %+v after %+v", pendingBefore, gotPending)
	}
	if !gotPending.CompletedAt.Time.Equal(pendingBefore.CompletedAt.Time) {
		t.Errorf("removal changed completed_at: before %v after %v", pendingBefore.CompletedAt, gotPending.CompletedAt)
	}
	if _, err := quests.Accept(ctx, admin, h.ID, pending.ID, app.MemberAction{MemberID: ben.ID}); err != nil {
		t.Errorf("Ben cannot take the freed chore: %v", err)
	} else if _, err := quests.Abandon(ctx, admin, h.ID, pending.ID, app.MemberAction{MemberID: ben.ID}); err != nil {
		t.Fatalf("Ben abandon freed chore: %v", err)
	}
	warmupAfter, err := quests.Get(ctx, admin, h.ID, warmup.ID)
	if err != nil {
		t.Fatalf("Get warmup after removal: %v", err)
	}
	if !warmupAfter.AvailableAt.Time.Equal(warmupBefore.AvailableAt.Time) || !warmupAfter.CompletedAt.Time.Equal(warmupBefore.CompletedAt.Time) {
		t.Errorf("removal moved another member's cooldown: before %+v after %+v", warmupBefore, warmupAfter)
	}
	if gotPending.AwaitingApproval {
		t.Errorf("quest still awaiting approval after its member was removed: %+v", gotPending)
	}
	var cancelled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quest_completions WHERE household_id = $1 AND quest_id = $2 AND status = 'cancelled'`, h.ID, pending.ID).Scan(&cancelled); err != nil || cancelled != 1 {
		t.Errorf("cancelled completions of the removed member's quest = %d (%v), want 1", cancelled, err)
	}
	// The cancellation names the admin who removed the member.
	var cancelledBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status_by FROM quest_completions WHERE household_id = $1 AND quest_id = $2 AND status = 'cancelled'`, h.ID, pending.ID).Scan(&cancelledBy); err != nil ||
		cancelledBy == nil || *cancelledBy != admin {
		t.Errorf("cancelled completion status_by = %v (%v), want the removing admin %v", cancelledBy, err, admin)
	}
	// Ben's approved completion is history and survives, untouched.
	if got := completionStatuses(ben.ID, warmup.ID); len(got) != 1 || got[0] != "approved" {
		t.Errorf("Ben's completions = %v, want [approved]", got)
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

// TestIntegration_RemoveMemberOnSharedQuests: removing a member who is on a
// shared quest, held or waiting for approval, takes off only their part. A
// shared completion they marked done passes to the earliest remaining
// recorded member and stays listed; one they were merely on loses them; the
// rest are paid as the remaining n at approval (ceil(points/n) + bonus, no
// bonus once one member is left). A shared quest they held passes to the
// earliest remaining participant; one they were merely on stays with its
// holder. No balance moves on removal.
func TestIntegration_RemoveMemberOnSharedQuests(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "removeshared").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Sharers"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	member := func(name string) uuid.UUID {
		t.Helper()
		m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: name}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return m.ID
	}
	ann, ben, cal, dee := member("Ann"), member("Ben"), member("Cal"), member("Dee")
	act := func(id uuid.UUID) app.MemberAction { return app.MemberAction{MemberID: id} }
	shareable := func(name string) uuid.UUID {
		t.Helper()
		qv, err := quests.Create(ctx, admin, h.ID, app.QuestInput{
			Name: name, Points: app.NumberField{Set: true, Value: 100}, Shareable: true,
			TeamworkBonus: app.NumberField{Set: true, Value: 20},
		}, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return qv.ID
	}
	// takeAndShare: holder accepts the quest, then shares it with each of
	// with in turn, so they join in that order after the holder.
	takeAndShare := func(questID, holder uuid.UUID, with ...uuid.UUID) {
		t.Helper()
		if _, err := quests.Accept(ctx, admin, h.ID, questID, act(holder)); err != nil {
			t.Fatalf("accept: %v", err)
		}
		for _, w := range with {
			if _, err := quests.Share(ctx, admin, h.ID, questID, act(holder), act(w)); err != nil {
				t.Fatalf("share: %v", err)
			}
		}
	}

	// P1: Ann, Ben, Cal; Ben marks it done. P2: Dee, Cal, Ann; Cal marks it done.
	p1, p2 := shareable("P1"), shareable("P2")
	takeAndShare(p1, ann, ben, cal)
	takeAndShare(p2, dee, cal, ann)
	if _, err := quests.MarkDone(ctx, admin, h.ID, p1, act(ben)); err != nil {
		t.Fatalf("Ben mark P1 done: %v", err)
	}
	if _, err := quests.MarkDone(ctx, admin, h.ID, p2, act(cal)); err != nil {
		t.Fatalf("Cal mark P2 done: %v", err)
	}
	// H1: Ben holds it, shared with Ann then Dee. H2: Dee holds it, shared with Cal.
	h1, h2 := shareable("H1"), shareable("H2")
	takeAndShare(h1, ben, ann, dee)
	takeAndShare(h2, dee, cal)

	balances := func() map[uuid.UUID]int32 {
		t.Helper()
		ms, err := familySvc.List(ctx, admin, h.ID)
		if err != nil {
			t.Fatalf("list members: %v", err)
		}
		out := map[uuid.UUID]int32{}
		for _, m := range ms {
			out[m.ID] = m.Points
		}
		return out
	}
	// remove takes the member out and checks nobody else's balance moved.
	remove := func(id uuid.UUID) {
		t.Helper()
		before := balances()
		if err := familySvc.Remove(ctx, admin, h.ID, id, ""); err != nil {
			t.Fatalf("remove member: %v", err)
		}
		after := balances()
		if len(after) != len(before)-1 {
			t.Errorf("members after removal = %d, want %d", len(after), len(before)-1)
		}
		for mid, p := range after {
			if before[mid] != p {
				t.Errorf("member %s points moved on removal: %d -> %d", mid, before[mid], p)
			}
		}
	}
	// pendingItem is the one pending item of the quest, checked to have
	// marker, members (in order), each member's award and the bonus.
	pendingItem := func(questID, marker uuid.UUID, award, bonus int32, members ...uuid.UUID) app.PendingCompletion {
		t.Helper()
		items, err := quests.ListPending(ctx, admin, h.ID, "")
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		var found []app.PendingCompletion
		for _, it := range items {
			if it.QuestID == questID {
				found = append(found, it)
			}
		}
		if len(found) != 1 {
			t.Fatalf("pending items of quest %s = %+v, want one", questID, found)
		}
		it := found[0]
		got := make([]uuid.UUID, len(it.Members))
		for i, m := range it.Members {
			got[i] = m.MemberID
			if m.Award != award {
				t.Errorf("quest %s member %s award = %d, want %d", questID, m.MemberID, m.Award, award)
			}
		}
		if it.MemberID != marker || !slices.Equal(got, members) || it.TeamworkBonus != bonus {
			t.Errorf("quest %s pending item: marker %s members %v bonus %d, want marker %s members %v bonus %d",
				questID, it.MemberID, got, it.TeamworkBonus, marker, members, bonus)
		}
		return it
	}
	holding := func(questID uuid.UUID, holder *uuid.UUID, participants ...uuid.UUID) {
		t.Helper()
		qv, err := quests.Get(ctx, admin, h.ID, questID)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		if (holder == nil) != (qv.ClaimedBy == nil) || (holder != nil && *qv.ClaimedBy != *holder) ||
			!slices.Equal(qv.Participants, participants) {
			t.Errorf("quest %s held by %v with %v, want %v with %v", questID, qv.ClaimedBy, qv.Participants, holder, participants)
		}
		if holder != nil && !qv.ClaimedAt.Valid {
			t.Errorf("quest %s held with no claimed_at: %+v", questID, qv)
		}
	}
	pendingItem(p1, ben, 34, 20, ann, ben, cal)
	pendingItem(p2, cal, 34, 20, dee, cal, ann)
	holding(h1, &ben, ben, ann, dee)
	holding(h2, &dee, dee, cal)

	// Ben goes: the marker of P1 and the holder of H1.
	remove(ben)
	handed := pendingItem(p1, ann, 50, 20, ann, cal)
	// Handed over, not cancelled: still pending, and no status change to name.
	if st, by := completionStatusBy(t, pool, handed.ID); st != "pending" || by != nil {
		t.Errorf("handed-over completion: status %q status_by %v, want pending NULL", st, by)
	}
	pendingItem(p2, cal, 34, 20, dee, cal, ann)
	holding(h1, &ann, ann, dee)
	holding(h2, &dee, dee, cal)

	// Cal goes: merely on P1 and H2, the marker of P2.
	remove(cal)
	pendingItem(p1, ann, 100, 0, ann)
	item2 := pendingItem(p2, dee, 50, 20, dee, ann)
	holding(h1, &ann, ann, dee)
	holding(h2, &dee) // one member left: no longer shared

	var cancelled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quest_completions WHERE household_id = $1 AND status = 'cancelled'`, h.ID).Scan(&cancelled); err != nil || cancelled != 0 {
		t.Errorf("cancelled completions = %d (%v), want 0: shared completions with members left stay pending", cancelled, err)
	}

	// Approval pays the remaining n: P2's two get 50 + 20, P1's one gets 100.
	before := balances()
	if _, _, _, paid, err := quests.Approve(ctx, admin, h.ID, item2.ID, ""); err != nil {
		t.Fatalf("approve P2: %v", err)
	} else if len(paid) != 2 {
		t.Errorf("P2 approval credited %+v, want Dee and Ann", paid)
	}
	item1 := pendingItem(p1, ann, 100, 0, ann)
	if _, _, _, paid, err := quests.Approve(ctx, admin, h.ID, item1.ID, ""); err != nil {
		t.Fatalf("approve P1: %v", err)
	} else if len(paid) != 1 || paid[0].PointsAwarded != 100 || paid[0].TeamworkBonus != 0 {
		t.Errorf("P1 approval credited %+v, want Ann 100 and no bonus", paid)
	}
	after := balances()
	if after[dee] != before[dee]+70 || after[ann] != before[ann]+70+100 {
		t.Errorf("balances after approvals: Dee %d -> %d (want +70), Ann %d -> %d (want +170)",
			before[dee], after[dee], before[ann], after[ann])
	}
}

// TestIntegration_ApproveRacesRemoveMember: an admin approving a member's
// pending completion while another admin removes that member never fails
// with a database error (a deadlock), whichever lands first. Either the
// approval wins (points credited, then the member is removed) or the removal
// wins (the completion is cancelled and the approval is ErrNotFound).
func TestIntegration_ApproveRacesRemoveMember(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "approveremove").User.ID
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Race"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	for round := 0; round < 10; round++ {
		m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: "Racer"}, "")
		if err != nil {
			t.Fatalf("round %d: create member: %v", round, err)
		}
		chore, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Race chore", Points: num(3)}, "")
		if err != nil {
			t.Fatalf("round %d: create quest: %v", round, err)
		}
		act := app.MemberAction{MemberID: m.ID}
		if _, err := quests.Accept(ctx, admin, h.ID, chore.ID, act); err != nil {
			t.Fatalf("round %d: accept: %v", round, err)
		}
		if _, err := quests.MarkDone(ctx, admin, h.ID, chore.ID, act); err != nil {
			t.Fatalf("round %d: mark done: %v", round, err)
		}
		waiting, err := quests.ListPending(ctx, admin, h.ID, "")
		if err != nil || len(waiting) != 1 {
			t.Fatalf("round %d: pending %+v %v", round, waiting, err)
		}
		var wg sync.WaitGroup
		var approveErr, removeErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, _, _, approveErr = quests.Approve(ctx, admin, h.ID, waiting[0].ID, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			removeErr = familySvc.Remove(ctx, admin, h.ID, m.ID, "")
		}()
		close(start)
		wg.Wait()
		if removeErr != nil {
			t.Fatalf("round %d: remove failed: %v", round, removeErr)
		}
		if approveErr != nil && !errors.Is(approveErr, app.ErrNotFound) {
			t.Fatalf("round %d: approve failed with %v, want success or ErrNotFound", round, approveErr)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM quest_completions WHERE household_id = $1 AND id = $2`, h.ID, waiting[0].ID).Scan(&status); err != nil {
			t.Fatalf("round %d: read completion: %v", round, err)
		}
		if (approveErr == nil) != (status == "approved") || (approveErr != nil && status != "cancelled") {
			t.Fatalf("round %d: approve err %v but completion %q", round, approveErr, status)
		}
	}
}

// TestIntegration_RemoveMemberRollsBack: the release and the delete are one
// transaction. When the member delete fails after the release ran, the
// release is undone: the member is still there, still holds their accepted
// quest, and their completion is still waiting for approval; the caller gets
// an error.
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
	if !marked.AwaitingApproval {
		t.Fatalf("marked-done quest not awaiting approval: %+v", marked)
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
	if gotPending.ClaimedBy != nil || !gotPending.AwaitingApproval ||
		!gotPending.AvailableAt.Time.Equal(marked.AvailableAt.Time) {
		t.Errorf("pending quest changed by a failed removal: %+v", gotPending)
	}
	gotAccepted, err := quests.Get(ctx, admin, h.ID, accepted.ID)
	if err != nil {
		t.Fatalf("Get accepted: %v", err)
	}
	if gotAccepted.ClaimedBy == nil || *gotAccepted.ClaimedBy != ann.ID || !gotAccepted.ClaimedAt.Valid {
		t.Errorf("accepted quest changed by a failed removal: %+v", gotAccepted)
	}
	if pend, err := quests.ListPending(ctx, admin, h.ID, ""); err != nil || len(pend) != 1 || pend[0].QuestID != pending.ID || pend[0].MemberID != ann.ID {
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

	// A share that commits while the removal of another participant waits
	// on the quest must be counted by the removal: the holder keeps their
	// participant row, and the new member is on the quest with them.
	t.Run("a share committing during the removal of a participant keeps the holder and the new member", func(t *testing.T) {
		mk := func(name string) uuid.UUID {
			t.Helper()
			m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: name}, "")
			if err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
			return m.ID
		}
		holder, leaver, joiner := mk("Holder"), mk("Leaver"), mk("Joiner")
		sq, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Shared race", Points: num(10), Shareable: true, TeamworkBonus: num(2)}, "")
		if err != nil {
			t.Fatalf("create shareable quest: %v", err)
		}
		holderAct := app.MemberAction{MemberID: holder}
		if _, err := quests.Accept(ctx, admin, h.ID, sq.ID, holderAct); err != nil {
			t.Fatalf("holder accept: %v", err)
		}
		if _, err := quests.Share(ctx, admin, h.ID, sq.ID, holderAct, app.MemberAction{MemberID: leaver}); err != nil {
			t.Fatalf("share with leaver: %v", err)
		}

		// The holder's share to the joiner, written and held uncommitted with
		// the locks Share takes (both member rows, then the quest row).
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		qtx := q.WithTx(tx)
		for _, id := range []uuid.UUID{holder, joiner} {
			if _, err := qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: h.ID, ID: id}); err != nil {
				t.Fatalf("lock member: %v", err)
			}
		}
		added, err := qtx.ShareQuest(ctx, db.ShareQuestParams{HouseholdID: h.ID, ID: sq.ID, MemberID: holder, TargetMemberID: joiner})
		if err != nil || len(added) == 0 {
			t.Fatalf("share with joiner: %v %v", added, err)
		}

		removeErr := make(chan error, 1)
		go func() { removeErr <- familySvc.Remove(ctx, admin, h.ID, leaver, "") }()
		waitForLockWait(t, pool)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit share: %v", err)
		}
		if err := <-removeErr; err != nil {
			t.Fatalf("Remove: %v", err)
		}

		got, err := quests.Get(ctx, admin, h.ID, sq.ID)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		want := []uuid.UUID{holder, joiner}
		slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		gotIDs := slices.Clone(got.Participants)
		slices.SortFunc(gotIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		if !slices.Equal(gotIDs, want) || got.ClaimedBy == nil || *got.ClaimedBy != holder {
			t.Errorf("after the removal: participants %v claimed_by %v, want %v held by %v", got.Participants, got.ClaimedBy, want, holder)
		}
	})

	// Remove and Share run at once, many times: whichever lands first, the
	// quest stays held by the holder, the holder is on it whenever it is
	// shared, and a shared quest has at least two members.
	t.Run("remove of a participant against share", func(t *testing.T) {
		for round := range 10 {
			mk := func(name string) uuid.UUID {
				t.Helper()
				m, err := familySvc.Create(ctx, admin, h.ID, app.FamilyMemberInput{Name: name}, "")
				if err != nil {
					t.Fatalf("round %d: create %s: %v", round, name, err)
				}
				return m.ID
			}
			holder, leaver, joiner := mk("Holder"), mk("Leaver"), mk("Joiner")
			sq, err := quests.Create(ctx, admin, h.ID, app.QuestInput{Name: "Shared race", Points: num(10), Shareable: true}, "")
			if err != nil {
				t.Fatalf("round %d: create quest: %v", round, err)
			}
			holderAct := app.MemberAction{MemberID: holder}
			if _, err := quests.Accept(ctx, admin, h.ID, sq.ID, holderAct); err != nil {
				t.Fatalf("round %d: accept: %v", round, err)
			}
			if _, err := quests.Share(ctx, admin, h.ID, sq.ID, holderAct, app.MemberAction{MemberID: leaver}); err != nil {
				t.Fatalf("round %d: share with leaver: %v", round, err)
			}
			var wg sync.WaitGroup
			var shareErr, removeErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, shareErr = quests.Share(ctx, admin, h.ID, sq.ID, holderAct, app.MemberAction{MemberID: joiner})
			}()
			go func() {
				defer wg.Done()
				<-start
				removeErr = familySvc.Remove(ctx, admin, h.ID, leaver, "")
			}()
			close(start)
			wg.Wait()
			if removeErr != nil || shareErr != nil {
				t.Fatalf("round %d: remove %v, share %v", round, removeErr, shareErr)
			}
			got, err := quests.Get(ctx, admin, h.ID, sq.ID)
			if err != nil {
				t.Fatalf("round %d: get: %v", round, err)
			}
			if got.ClaimedBy == nil || *got.ClaimedBy != holder || len(got.Participants) != 2 ||
				!slices.Contains(got.Participants, holder) || !slices.Contains(got.Participants, joiner) {
				t.Fatalf("round %d: participants %v claimed_by %v, want holder %v and joiner %v", round, got.Participants, got.ClaimedBy, holder, joiner)
			}
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
