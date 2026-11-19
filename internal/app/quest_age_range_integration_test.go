//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// ageRangeFixture is a household in America/Chicago with members of known
// ages, set as birthdays a month past their last birthday so the age is fixed
// whatever today's date.
type ageRangeFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	q        *db.Queries
	quests   *app.QuestService
	family   *app.FamilyService
	admin    uuid.UUID
	h        uuid.UUID
	tz       string
	nameByID map[uuid.UUID]string
}

func newAgeRangeFixture(t *testing.T, prefix string) *ageRangeFixture {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	f := &ageRangeFixture{
		t: t, ctx: context.Background(), pool: pool, q: q,
		quests: app.NewQuestService(q, pool), family: app.NewFamilyService(q, pool, 10),
		tz: "America/Chicago", nameByID: map[uuid.UUID]string{},
	}
	f.admin = registerVerified(t, pool, authSvc, mailer, prefix).User.ID
	h, err := app.NewHouseholdService(q, pool).Create(f.ctx, f.admin, app.HouseholdInput{Name: "Ages", Timezone: f.tz})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	f.h = h.ID
	return f
}

// member adds a member to household hid; years < 0 leaves the birthday unset.
func (f *ageRangeFixture) member(hid uuid.UUID, name string, years int) uuid.UUID {
	f.t.Helper()
	m, err := f.family.Create(f.ctx, f.admin, hid, app.FamilyMemberInput{Name: name}, "")
	if err != nil {
		f.t.Fatalf("create %s: %v", name, err)
	}
	f.nameByID[m.ID] = name
	if years >= 0 {
		f.setAge(m.ID, years)
	}
	return m.ID
}

// setAge stores a birthday that makes the member years old today in the
// household's timezone (a month after their birthday).
func (f *ageRangeFixture) setAge(memberID uuid.UUID, years int) {
	f.t.Helper()
	d := localDate(f.t, f.tz).AddDate(-years, 0, -30)
	if _, err := f.pool.Exec(f.ctx, `UPDATE family_members SET birthday = $1 WHERE id = $2`, pgDate(d), memberID); err != nil {
		f.t.Fatalf("set birthday: %v", err)
	}
}

func ageField(n *int64) app.NumberField {
	if n == nil {
		return app.NumberField{}
	}
	return num(*n)
}

func limit(n int64) *int64 { return &n }

// quest creates a quest with the given limits (nil is no limit).
func (f *ageRangeFixture) quest(name string, minAge, maxAge *int64, shareable bool) app.QuestView {
	f.t.Helper()
	v, err := f.quests.Create(f.ctx, f.admin, f.h, app.QuestInput{
		Name: name, Points: num(5), Shareable: shareable, MinAge: ageField(minAge), MaxAge: ageField(maxAge),
	}, "")
	if err != nil {
		f.t.Fatalf("create quest %s: %v", name, err)
	}
	return v
}

func (f *ageRangeFixture) accept(questID, memberID uuid.UUID) error {
	f.t.Helper()
	_, err := f.quests.Accept(f.ctx, f.admin, f.h, questID, app.MemberAction{MemberID: memberID})
	return err
}

func (f *ageRangeFixture) stored(questID uuid.UUID) db.HouseholdQuest {
	f.t.Helper()
	row, err := f.q.GetHouseholdQuest(f.ctx, db.GetHouseholdQuestParams{HouseholdID: f.h, ID: questID})
	if err != nil {
		f.t.Fatalf("read quest: %v", err)
	}
	return row
}

func limitsOf(q db.HouseholdQuest) string {
	s := func(n pgtype.Int4) string {
		if !n.Valid {
			return "none"
		}
		return fmt.Sprint(n.Int32)
	}
	return s(q.MinAge) + "/" + s(q.MaxAge)
}

// TestIntegration_QuestAgeRangeStored proves CLM-2 and CLM-3 at the service:
// the range is stored and returned as sent, Update replaces it whole, and a
// bad limit is a per-field ValidationError that saves nothing.
func TestIntegration_QuestAgeRangeStored(t *testing.T) {
	f := newAgeRangeFixture(t, "agestore")

	cases := []struct {
		min, max *int64
		want     string
	}{
		{limit(10), limit(14), "10/14"},
		{limit(6), nil, "6/none"},
		{nil, limit(12), "none/12"},
		{nil, nil, "none/none"},
		{limit(0), limit(0), "0/0"},
	}
	var ids []uuid.UUID
	for _, c := range cases {
		v := f.quest("Ranged "+c.want, c.min, c.max, false)
		if got := limitsOf(v.HouseholdQuest); got != c.want {
			t.Errorf("created %s: view %s", c.want, got)
		}
		if got := limitsOf(f.stored(v.ID)); got != c.want {
			t.Errorf("created %s: stored %s", c.want, got)
		}
		got, err := f.quests.Get(f.ctx, f.admin, f.h, v.ID)
		if err != nil || limitsOf(got.HouseholdQuest) != c.want {
			t.Errorf("get %s: %s %v", c.want, limitsOf(got.HouseholdQuest), err)
		}
		ids = append(ids, v.ID)
	}

	// Update is a full replacement of the range: limits omitted are cleared,
	// and a new pair replaces the old.
	if v, err := f.quests.Update(f.ctx, f.admin, f.h, ids[0], app.QuestInput{Name: "Ranged cleared", Points: num(5)}, ""); err != nil || limitsOf(v.HouseholdQuest) != "none/none" {
		t.Errorf("update without limits: %s %v, want none/none", limitsOf(v.HouseholdQuest), err)
	}
	if got := limitsOf(f.stored(ids[0])); got != "none/none" {
		t.Errorf("stored after clearing update: %s", got)
	}
	if v, err := f.quests.Update(f.ctx, f.admin, f.h, ids[3], app.QuestInput{Name: "Now ranged", MinAge: num(8), MaxAge: num(12)}, ""); err != nil || limitsOf(v.HouseholdQuest) != "8/12" {
		t.Errorf("update to 8/12: %s %v", limitsOf(v.HouseholdQuest), err)
	}
	if got := limitsOf(f.stored(ids[3])); got != "8/12" {
		t.Errorf("stored after 8/12 update: %s", got)
	}

	// CLM-3: each bad limit is reported on its own field, with every other
	// bad field, and nothing is saved.
	invalid := app.NumberField{Set: true, Invalid: true}
	bad := []struct {
		name string
		in   app.QuestInput
		want map[string]string
	}{
		{"min above max", app.QuestInput{Name: "x", MinAge: num(14), MaxAge: num(10)},
			map[string]string{"max_age": "Minimum age can't be above the maximum age."}},
		{"negative min", app.QuestInput{Name: "x", MinAge: num(-1)}, map[string]string{"min_age": ""}},
		{"negative max", app.QuestInput{Name: "x", MaxAge: num(-1)}, map[string]string{"max_age": ""}},
		{"fractional or text min", app.QuestInput{Name: "x", MinAge: invalid}, map[string]string{"min_age": ""}},
		{"fractional or text max", app.QuestInput{Name: "x", MaxAge: invalid}, map[string]string{"max_age": ""}},
		{"with other fields", app.QuestInput{Name: "", Points: num(-1), MinAge: num(-2), MaxAge: invalid},
			map[string]string{"name": "", "points": "", "min_age": "", "max_age": ""}},
	}
	before, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range bad {
		for _, op := range []string{"create", "update"} {
			var err error
			if op == "create" {
				_, err = f.quests.Create(f.ctx, f.admin, f.h, c.in, "")
			} else {
				_, err = f.quests.Update(f.ctx, f.admin, f.h, ids[3], c.in, "")
			}
			var ve *app.ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("%s %s: %v, want ValidationError", op, c.name, err)
				continue
			}
			if len(ve.Fields) != len(c.want) {
				t.Errorf("%s %s: fields %v, want keys of %v", op, c.name, ve.Fields, c.want)
			}
			for field, msg := range c.want {
				if got := ve.Fields[field]; got == "" || (msg != "" && got != msg) {
					t.Errorf("%s %s: %s = %q, want %q", op, c.name, field, got, msg)
				}
			}
		}
	}
	after, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil || len(after) != len(before) {
		t.Errorf("refused creates saved quests: %d before, %d after (%v)", len(before), len(after), err)
	}
	if got := f.stored(ids[3]); limitsOf(got) != "8/12" || got.Name != "Now ranged" {
		t.Errorf("refused updates changed the quest: %s %q", limitsOf(got), got.Name)
	}
}

// TestIntegration_QuestAgeRangeAccept proves CLM-10, CLM-11, CLM-13 and
// CLM-15: ClaimQuest admits members inside the range (limits inclusive, one-
// sided ranges apply only their side), members with no birthday and anyone
// on a quest without limits; an available quest refuses an out-of-range
// member with ErrAgeRestricted and stays in the pool, while a held or resting
// quest is still ErrQuestTaken; ineligible_member_ids lists exactly the
// members Accept refuses; a library copy is all ages.
func TestIntegration_QuestAgeRangeAccept(t *testing.T) {
	f := newAgeRangeFixture(t, "ageaccept")
	kid5 := f.member(f.h, "Five", 5)
	kid6 := f.member(f.h, "Six", 6)
	kid8 := f.member(f.h, "Eight", 8)
	kid10 := f.member(f.h, "Ten", 10)
	kid12 := f.member(f.h, "Twelve", 12)
	kid13 := f.member(f.h, "Thirteen", 13)
	kid14 := f.member(f.h, "Fourteen", 14)
	kid15 := f.member(f.h, "Fifteen", 15)
	nobday := f.member(f.h, "NoBirthday", -1)
	everyone := []uuid.UUID{kid5, kid6, kid8, kid10, kid12, kid13, kid14, kid15, nobday}

	accepts := []struct {
		name     string
		min, max *int64
		member   uuid.UUID
		want     error
	}{
		{"10-14 at the min", limit(10), limit(14), kid10, nil},
		{"10-14 at the max", limit(10), limit(14), kid14, nil},
		{"10-14 inside", limit(10), limit(14), kid12, nil},
		{"10-14 no birthday", limit(10), limit(14), nobday, nil},
		{"10-14 below", limit(10), limit(14), kid8, app.ErrAgeRestricted},
		{"10-14 above", limit(10), limit(14), kid15, app.ErrAgeRestricted},
		{"6+ inside", limit(6), nil, kid15, nil},
		{"6+ at the min", limit(6), nil, kid6, nil},
		{"6+ below", limit(6), nil, kid5, app.ErrAgeRestricted},
		{"up to 12 inside", nil, limit(12), kid5, nil},
		{"up to 12 at the max", nil, limit(12), kid12, nil},
		{"up to 12 above", nil, limit(12), kid13, app.ErrAgeRestricted},
		{"no limits, youngest", nil, nil, kid5, nil},
		{"no limits, oldest", nil, nil, kid15, nil},
		{"no limits, no birthday", nil, nil, nobday, nil},
	}
	for _, c := range accepts {
		quest := f.quest(c.name, c.min, c.max, false)
		err := f.accept(quest.ID, c.member)
		if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: accept by %s = %v, want %v", c.name, f.nameByID[c.member], err, c.want)
			continue
		}
		held := f.stored(quest.ID).ClaimedBy
		if c.want == nil && (held == nil || *held != c.member) {
			t.Errorf("%s: claimed_by %v, want %s", c.name, held, f.nameByID[c.member])
		}
		if c.want != nil && held != nil {
			t.Errorf("%s: refused accept left claimed_by %v", c.name, *held)
		}
	}

	// CLM-11: the refused quest stays in the pool and an eligible member
	// takes it; a held quest is taken for everyone, in range or not.
	pool := f.quest("Mow the lawn", limit(10), limit(14), false)
	if err := f.accept(pool.ID, kid8); !errors.Is(err, app.ErrAgeRestricted) {
		t.Fatalf("out-of-range accept: %v, want ErrAgeRestricted", err)
	}
	if got, err := f.quests.Get(f.ctx, f.admin, f.h, pool.ID); err != nil || got.ClaimedBy != nil || !app.QuestAvailable(got.HouseholdQuest, time.Now()) {
		t.Fatalf("quest after refused accept: %+v %v, want available", got, err)
	}
	if err := f.accept(pool.ID, kid12); err != nil {
		t.Fatalf("eligible accept after refusal: %v", err)
	}
	if err := f.accept(pool.ID, kid8); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("out-of-range accept of a held quest: %v, want ErrQuestTaken", err)
	}
	if err := f.accept(pool.ID, kid10); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("in-range accept of a held quest: %v, want ErrQuestTaken", err)
	}
	// A resting quest (marked done, waiting for its midnight) is taken too.
	if _, err := f.quests.MarkDone(f.ctx, f.admin, f.h, pool.ID, app.MemberAction{MemberID: kid12}); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if err := f.accept(pool.ID, kid8); !errors.Is(err, app.ErrQuestTaken) {
		t.Errorf("out-of-range accept of a resting quest: %v, want ErrQuestTaken", err)
	}
	if err := f.accept(uuid.New(), kid8); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("accept of an unknown quest: %v, want ErrNotFound", err)
	}

	// CLM-13: ineligible_member_ids is exactly the members Accept refuses,
	// in list and get, and [] on a quest without limits.
	ranged := f.quest("Ranged list", limit(10), limit(14), false)
	open := f.quest("Open list", nil, nil, false)
	want := map[uuid.UUID]bool{kid5: true, kid6: true, kid8: true, kid15: true}
	list, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed, listedOpen *app.QuestView
	for i := range list {
		switch list[i].ID {
		case ranged.ID:
			listed = &list[i]
		case open.ID:
			listedOpen = &list[i]
		}
	}
	if listed == nil || listedOpen == nil {
		t.Fatalf("list misses the new quests")
	}
	got, err := f.quests.Get(f.ctx, f.admin, f.h, ranged.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for label, ids := range map[string][]uuid.UUID{"list": listed.IneligibleMemberIDs, "get": got.IneligibleMemberIDs, "create": ranged.IneligibleMemberIDs} {
		if len(ids) != len(want) {
			t.Errorf("%s ineligible = %v, want %d members", label, ids, len(want))
		}
		for _, id := range ids {
			if !want[id] {
				t.Errorf("%s ineligible lists %s", label, f.nameByID[id])
			}
		}
	}
	if len(listedOpen.IneligibleMemberIDs) != 0 || len(open.IneligibleMemberIDs) != 0 {
		t.Errorf("all-ages quest ineligible = %v / %v, want none", listedOpen.IneligibleMemberIDs, open.IneligibleMemberIDs)
	}
	for _, m := range everyone {
		err := f.accept(ranged.ID, m)
		if want[m] != errors.Is(err, app.ErrAgeRestricted) {
			t.Errorf("accept by %s: %v; listed ineligible %v", f.nameByID[m], err, want[m])
		}
		if err == nil {
			// Put it back in the pool for the next member.
			if _, err := f.quests.Abandon(f.ctx, f.admin, f.h, ranged.ID, app.MemberAction{MemberID: m}); err != nil {
				t.Fatalf("abandon: %v", err)
			}
		}
	}

	// CLM-15: a library copy is all ages, as returned and as stored, and a
	// member with a birthday accepts it.
	library, err := f.quests.ListLibrary(f.ctx)
	if err != nil || len(library) == 0 {
		t.Fatalf("library: %v", err)
	}
	copied, err := f.quests.CopyFromLibrary(f.ctx, f.admin, f.h, library[0].ID, "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if limitsOf(copied.HouseholdQuest) != "none/none" || limitsOf(f.stored(copied.ID)) != "none/none" || len(copied.IneligibleMemberIDs) != 0 {
		t.Errorf("library copy limits %s, stored %s, ineligible %v; want all ages", limitsOf(copied.HouseholdQuest), limitsOf(f.stored(copied.ID)), copied.IneligibleMemberIDs)
	}
	if err := f.accept(copied.ID, kid15); err != nil {
		t.Errorf("accept library copy: %v", err)
	}
}

// TestIntegration_QuestAgeRangeKeepsHeld proves CLM-16: narrowing a quest's
// range and setting a holder's birthday out of it releases nothing; the
// holder and participants keep it and can mark it done, and the list returns
// every quest of the household whatever its range.
func TestIntegration_QuestAgeRangeKeepsHeld(t *testing.T) {
	f := newAgeRangeFixture(t, "ageheld")
	holder := f.member(f.h, "Holder", -1)
	partner := f.member(f.h, "Partner", 11)

	solo := f.quest("Solo", limit(10), limit(14), false)
	shared := f.quest("Shared", limit(10), limit(14), true)
	if err := f.accept(solo.ID, partner); err != nil {
		t.Fatalf("accept solo: %v", err)
	}
	if err := f.accept(shared.ID, holder); err != nil {
		t.Fatalf("accept shared: %v", err)
	}
	if _, err := f.quests.Share(f.ctx, f.admin, f.h, shared.ID, app.MemberAction{MemberID: holder}, app.MemberAction{MemberID: partner}); err != nil {
		t.Fatalf("share: %v", err)
	}

	// The admin narrows both ranges to exclude them and sets the holder's
	// birthday out of range too.
	for _, q := range []app.QuestView{solo, shared} {
		if _, err := f.quests.Update(f.ctx, f.admin, f.h, q.ID, app.QuestInput{
			Name: q.Name, Points: num(5), Shareable: q.Shareable, MinAge: num(2), MaxAge: num(4),
		}, ""); err != nil {
			t.Fatalf("narrow %s: %v", q.Name, err)
		}
	}
	f.setAge(holder, 16)

	if got := f.stored(solo.ID).ClaimedBy; got == nil || *got != partner {
		t.Errorf("solo holder after narrowing: %v, want Partner", got)
	}
	sharedView, err := f.quests.Get(f.ctx, f.admin, f.h, shared.ID)
	if err != nil {
		t.Fatalf("get shared: %v", err)
	}
	if sharedView.ClaimedBy == nil || *sharedView.ClaimedBy != holder || len(sharedView.Participants) != 2 {
		t.Errorf("shared quest after narrowing: holder %v participants %v", sharedView.ClaimedBy, sharedView.Participants)
	}
	// Both are listed as out of range now, yet still theirs.
	if len(sharedView.IneligibleMemberIDs) != 2 {
		t.Errorf("shared quest ineligible %v, want both members", sharedView.IneligibleMemberIDs)
	}

	list, _, err := f.quests.List(f.ctx, f.admin, f.h, app.QuestPage{Limit: 100})
	if err != nil || len(list) != 2 {
		t.Fatalf("list returns %d quests (%v), want both ranged quests", len(list), err)
	}

	if _, err := f.quests.MarkDone(f.ctx, f.admin, f.h, solo.ID, app.MemberAction{MemberID: partner}); err != nil {
		t.Errorf("mark done solo after narrowing: %v", err)
	}
	if _, err := f.quests.MarkDone(f.ctx, f.admin, f.h, shared.ID, app.MemberAction{MemberID: holder}); err != nil {
		t.Errorf("mark done shared after narrowing: %v", err)
	}
}
