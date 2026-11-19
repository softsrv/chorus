//go:build integration

package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_HouseholdIsolation_Birthdays extends the isolation guard to
// member birthdays (CLM-5, CLM-4): a user outside the household cannot read
// or write them (ErrNotFound, never ErrForbidden), a member id of another
// household is not found through the caller's own household, and neither
// changes the stored birthday. ListBirthdays lists only this household.
func TestIntegration_HouseholdIsolation_Birthdays(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "bdayalice").User.ID
	mallory := registerVerified(t, pool, authSvc, mailer, "bdaymallory").User.ID

	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Alice Birthdays", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("create alice household: %v", err)
	}
	mh, err := households.Create(ctx, mallory, app.HouseholdInput{Name: "Mallory Birthdays"})
	if err != nil {
		t.Fatalf("create mallory household: %v", err)
	}
	kid, err := familySvc.Create(ctx, alice, h.ID, app.FamilyMemberInput{Name: "Kid"}, "")
	if err != nil {
		t.Fatalf("create kid: %v", err)
	}
	malloryKid, err := familySvc.Create(ctx, mallory, mh.ID, app.FamilyMemberInput{Name: "Mallory Kid"}, "")
	if err != nil {
		t.Fatalf("create mallory kid: %v", err)
	}
	set := "2015-04-02"
	if _, err := familySvc.SetBirthday(ctx, alice, h.ID, kid.ID, &set, ""); err != nil {
		t.Fatalf("alice SetBirthday: %v", err)
	}
	stored := func(id uuid.UUID) string {
		t.Helper()
		var d pgtype.Date
		if err := pool.QueryRow(ctx, "SELECT birthday FROM family_members WHERE id = $1", id).Scan(&d); err != nil {
			t.Fatalf("read birthday: %v", err)
		}
		if !d.Valid {
			return ""
		}
		return d.Time.Format(time.DateOnly)
	}

	// Mallory reads and writes nothing of Alice's household.
	if _, err := familySvc.ListBirthdays(ctx, mallory, h.ID, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory ListBirthdays: %v, want ErrNotFound", err)
	}
	other := "2001-01-01"
	if _, err := familySvc.SetBirthday(ctx, mallory, h.ID, kid.ID, &other, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory SetBirthday: %v, want ErrNotFound", err)
	}
	if _, err := familySvc.SetBirthday(ctx, mallory, h.ID, kid.ID, nil, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory clear birthday: %v, want ErrNotFound", err)
	}
	// Alice's member id through Mallory's own household is not found either.
	if _, err := familySvc.SetBirthday(ctx, mallory, mh.ID, kid.ID, &other, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory SetBirthday on alice's member via own household: %v, want ErrNotFound", err)
	}
	if got := stored(kid.ID); got != set {
		t.Errorf("alice kid birthday after mallory's attempts = %q, want %q", got, set)
	}
	// And Mallory's member id through Alice's household is not found for Alice.
	if _, err := familySvc.SetBirthday(ctx, alice, h.ID, malloryKid.ID, &other, ""); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("alice SetBirthday on mallory's member: %v, want ErrNotFound", err)
	}
	if got := stored(malloryKid.ID); got != "" {
		t.Errorf("mallory kid birthday = %q, want unset", got)
	}

	// Each household's list holds only its own members.
	list, err := familySvc.ListBirthdays(ctx, alice, h.ID, "")
	if err != nil {
		t.Fatalf("alice ListBirthdays: %v", err)
	}
	if len(list) != 1 || list[0].MemberID != kid.ID {
		t.Errorf("alice ListBirthdays = %+v, want only Kid", list)
	}
	mlist, err := familySvc.ListBirthdays(ctx, mallory, mh.ID, "")
	if err != nil {
		t.Fatalf("mallory ListBirthdays own: %v", err)
	}
	if len(mlist) != 1 || mlist[0].MemberID != malloryKid.ID || mlist[0].Birthday != nil || mlist[0].Age != nil {
		t.Errorf("mallory ListBirthdays = %+v, want only Mallory Kid with no birthday", mlist)
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

// TestIntegration_HouseholdIsolation_QuestAges extends the isolation guard to
// quest age ranges (CLM-17): the age checks of ClaimQuest, ShareQuest and the
// ineligible-members read only use members of the quest's own household. A
// member of another household is not found and claims nothing, another
// household's out-of-range members are never listed, and a non-member user
// reads nothing.
func TestIntegration_HouseholdIsolation_QuestAges(t *testing.T) {
	f := newAgeRangeFixture(t, "ageiso")
	hb, err := app.NewHouseholdService(f.q, f.pool).Create(f.ctx, f.admin, app.HouseholdInput{Name: "Other ages", Timezone: "Asia/Tokyo"})
	if err != nil {
		t.Fatalf("create second household: %v", err)
	}
	holder := f.member(f.h, "Holder", 12)
	inRange := f.member(f.h, "InRange", 11)
	young := f.member(f.h, "Young", 7)
	// The other household's members, one far out of range and one in it.
	otherYoung := f.member(hb.ID, "OtherYoung", 3)
	otherInRange := f.member(hb.ID, "OtherInRange", 12)

	ranged := f.quest("Ranged", limit(10), limit(14), true)

	// Another household's member cannot accept it, even one inside the range,
	// and the refusal is not found (never age_restricted, which would tell the
	// caller something about a member they cannot see).
	for _, m := range []uuid.UUID{otherYoung, otherInRange} {
		if err := f.accept(ranged.ID, m); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("accept by %s of another household: %v, want ErrNotFound", f.nameByID[m], err)
		}
	}
	if got := f.stored(ranged.ID).ClaimedBy; got != nil {
		t.Fatalf("cross-household accept claimed the quest: %v", *got)
	}
	// Through the other household's id the quest does not exist.
	if _, err := f.quests.Accept(f.ctx, f.admin, hb.ID, ranged.ID, app.MemberAction{MemberID: otherInRange}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("accept through another household: %v, want ErrNotFound", err)
	}

	// ineligible_member_ids lists only this household's out-of-range member.
	view, err := f.quests.Get(f.ctx, f.admin, f.h, ranged.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(view.IneligibleMemberIDs) != 1 || view.IneligibleMemberIDs[0] != young {
		t.Errorf("ineligible = %v, want only Young", view.IneligibleMemberIDs)
	}
	list, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	for _, id := range list[0].IneligibleMemberIDs {
		if id == otherYoung || id == otherInRange {
			t.Errorf("list ineligible holds another household's member %s", f.nameByID[id])
		}
	}

	// Sharing with another household's member is not found and adds nobody;
	// sharing in-household still respects the range.
	if err := f.accept(ranged.ID, holder); err != nil {
		t.Fatalf("holder accept: %v", err)
	}
	holderAct := app.MemberAction{MemberID: holder}
	for _, m := range []uuid.UUID{otherYoung, otherInRange} {
		if _, err := f.quests.Share(f.ctx, f.admin, f.h, ranged.ID, holderAct, app.MemberAction{MemberID: m}); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("share with %s of another household: %v, want ErrNotFound", f.nameByID[m], err)
		}
	}
	if got, err := f.quests.Get(f.ctx, f.admin, f.h, ranged.ID); err != nil || len(got.Participants) != 0 {
		t.Errorf("participants after cross-household shares: %v %v", got.Participants, err)
	}
	if _, err := f.quests.Share(f.ctx, f.admin, f.h, ranged.ID, holderAct, app.MemberAction{MemberID: inRange}); err != nil {
		t.Errorf("share with an in-range member: %v", err)
	}

	// A user who is not a member of the household reads nothing of it.
	mailer := &captureMailer{}
	mallory := registerVerified(t, f.pool, testAuthService(t, f.pool, mailer), mailer, "agemallory").User.ID
	if _, err := f.quests.Get(f.ctx, mallory, f.h, ranged.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory get: %v, want ErrNotFound", err)
	}
	if _, _, err := f.quests.List(f.ctx, mallory, f.h, app.QuestPage{Limit: 10}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory list: %v, want ErrNotFound", err)
	}
	if _, err := f.quests.Accept(f.ctx, mallory, f.h, ranged.ID, app.MemberAction{MemberID: young}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("mallory accept: %v, want ErrNotFound", err)
	}
}

// isolationFixture is what TestIntegration_HouseholdIsolation sets up:
// Alice's household A, Bob's household B with its seeded state, and the
// outsider Mallory. Its section helpers below read from it.
type isolationFixture struct {
	pool       *pgxpool.Pool
	q          *db.Queries
	ctx        context.Context
	authSvc    *app.AuthService
	households *app.HouseholdService
	quests     *app.QuestService
	familySvc  *app.FamilyService
	rewards    *app.RewardService
	combos     *app.ComboService
	alice      uuid.UUID
	mallory    uuid.UUID
	bob        uuid.UUID
	bobRes     app.TokenResult
	h          app.Household
	bh         app.Household
	aliceQuest app.QuestView
	aliceCombo app.Combo
	asAlice    app.MemberAction
	asBob      app.MemberAction
	bobMember  app.Member
	bobAwardID uuid.UUID
	bobClaimed app.QuestView
	bobPending app.QuestView
	bobReward  db.HouseholdReward
}

// householdIsolationCombos: look-alike chores done the same day in another
// household never trigger or pay a household's combo.
func householdIsolationCombos(t *testing.T, f *isolationFixture) {
	alice, aliceCombo, aliceQuest, asAlice, asBob, bh, bob, bobMember, combos, ctx, h, pool, q, quests := f.alice, f.aliceCombo, f.aliceQuest, f.asAlice, f.asBob, f.bh, f.bob, f.bobMember, f.combos, f.ctx, f.h, f.pool, f.q, f.quests
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
}

// householdIsolationExpirySweep: the quest expiry sweep releases only the
// household of the quest it keys on.
func householdIsolationExpirySweep(t *testing.T, f *isolationFixture) {
	alice, asAlice, asBob, bh, bob, ctx, familySvc, h, pool, q, quests := f.alice, f.asAlice, f.asBob, f.bh, f.bob, f.ctx, f.familySvc, f.h, f.pool, f.q, f.quests
	// CLM-8: the quest expiry sweep keys every statement by the household of
	// the quest it releases. Household B holds a shared quest with a deadline
	// still ahead; household A holds an overdue one.
	bobKid, err := familySvc.Create(ctx, bob, bh.ID, app.FamilyMemberInput{Name: "Bob Kid"}, "")
	if err != nil {
		t.Fatalf("Create bob kid: %v", err)
	}
	bobTeam, err := quests.Create(ctx, bob, bh.ID, app.QuestInput{Name: "Bob Team", Points: num(10), Shareable: true}, "")
	if err != nil {
		t.Fatalf("Create bob team quest: %v", err)
	}
	if _, err := quests.Accept(ctx, bob, bh.ID, bobTeam.ID, asBob); err != nil {
		t.Fatalf("Accept bob team quest: %v", err)
	}
	if _, err := quests.Share(ctx, bob, bh.ID, bobTeam.ID, asBob, app.MemberAction{MemberID: bobKid.ID}); err != nil {
		t.Fatalf("Share bob team quest: %v", err)
	}
	bobTeamSnapshot := func() string {
		t.Helper()
		return rowJSON(t, pool, `SELECT to_jsonb(q)::text FROM household_quests q WHERE id = $1`, bobTeam.ID) +
			rowJSON(t, pool, `SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.member_id), '[]')::text
				FROM household_quest_participants p WHERE quest_id = $1`, bobTeam.ID)
	}
	bobTeamBefore := bobTeamSnapshot()
	if n := participantRows(t, pool, bobTeam.ID); n != 2 {
		t.Fatalf("bob team participants before expiry: %d, want 2", n)
	}
	bobTeamRow, err := q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: bh.ID, ID: bobTeam.ID})
	if err != nil || bobTeamRow.ClaimedBy == nil || !bobTeamRow.ClaimExpiresAt.Valid || !bobTeamRow.ClaimExpiresAt.Time.After(time.Now()) {
		t.Fatalf("bob team quest before expiry: %+v, %v; want held with a future deadline", bobTeamRow, err)
	}

	// Probe: releasing B's quest through household A's id matches no row even
	// when B's deadline has passed, and changes nothing.
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET claim_expires_at = NOW() - INTERVAL '1 minute' WHERE household_id = $1 AND id = $2`, bh.ID, bobTeam.ID); err != nil {
		t.Fatalf("back-date bob team deadline: %v", err)
	}
	if _, err := q.ExpireQuestClaim(ctx, db.ExpireQuestClaimParams{HouseholdID: h.ID, ID: bobTeam.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ExpireQuestClaim with household A and B's quest: %v, want pgx.ErrNoRows", err)
	}
	if n := participantRows(t, pool, bobTeam.ID); n != 2 {
		t.Errorf("bob team participants after cross-household ExpireQuestClaim: %d, want 2", n)
	}
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET claim_expires_at = $3 WHERE household_id = $1 AND id = $2`, bh.ID, bobTeam.ID, bobTeamRow.ClaimExpiresAt); err != nil {
		t.Fatalf("restore bob team deadline: %v", err)
	}
	if got := bobTeamSnapshot(); got != bobTeamBefore {
		t.Errorf("cross-household ExpireQuestClaim changed B's quest:\n got %s\nwant %s", got, bobTeamBefore)
	}

	aliceOverdue, err := quests.Create(ctx, alice, h.ID, app.QuestInput{Name: "Overdue in A", Points: num(3)}, "")
	if err != nil {
		t.Fatalf("Create alice overdue quest: %v", err)
	}
	if _, err := quests.Accept(ctx, alice, h.ID, aliceOverdue.ID, asAlice); err != nil {
		t.Fatalf("Accept alice overdue quest: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE household_quests SET claim_expires_at = NOW() - INTERVAL '1 minute' WHERE household_id = $1 AND id = $2`, h.ID, aliceOverdue.ID); err != nil {
		t.Fatalf("back-date alice overdue deadline: %v", err)
	}
	if _, err := quests.ExpireOverdueQuests(ctx); err != nil {
		t.Fatalf("ExpireOverdueQuests: %v", err)
	}
	gotAliceOverdue, err := q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: h.ID, ID: aliceOverdue.ID})
	if err != nil || gotAliceOverdue.ClaimedBy != nil || gotAliceOverdue.ClaimedAt.Valid || gotAliceOverdue.ClaimExpiresAt.Valid {
		t.Errorf("alice overdue quest after sweep: %+v, %v; want released", gotAliceOverdue, err)
	}
	if got := bobTeamSnapshot(); got != bobTeamBefore {
		t.Errorf("sweep of household A changed B's held quest:\n got %s\nwant %s", got, bobTeamBefore)
	}
	assertExpirySweepOnlyFromRunQuestExpiry(t)
}

// householdIsolationDelete: an outsider cannot delete household A, and Alice
// deleting it leaves household B and its data untouched.
func householdIsolationDelete(t *testing.T, f *isolationFixture) {
	alice, authSvc, bh, bob, bobAwardID, bobClaimed, bobMember, bobPending, bobRes, bobReward, ctx, familySvc, h, households, mallory, pool, quests, rewards := f.alice, f.authSvc, f.bh, f.bob, f.bobAwardID, f.bobClaimed, f.bobMember, f.bobPending, f.bobRes, f.bobReward, f.ctx, f.familySvc, f.h, f.households, f.mallory, f.pool, f.quests, f.rewards
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
