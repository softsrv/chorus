//go:build integration

package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// scheduleWorld is one household (Pacific/Auckland, so local dates differ
// from UTC) with an admin, its services and helpers for the schedule tests.
type scheduleWorld struct {
	t         *testing.T
	ctx       context.Context
	q         *db.Queries
	pool      *pgxpool.Pool
	admin     uuid.UUID
	h         uuid.UUID
	loc       *time.Location
	quests    *app.QuestService
	family    *app.FamilyService
	schedules *app.QuestScheduleService
	// pin is the household's admin PIN once setAdminPin set one; the
	// helpers pass it on every admin-gated call.
	pin string
}

func newScheduleWorld(t *testing.T, prefix string) *scheduleWorld {
	t.Helper()
	return newScheduleWorldIn(t, prefix, "Pacific/Auckland")
}

// newScheduleWorldIn is newScheduleWorld for a household in timezone tz.
// The household's schedules are deleted when the test ends: the test
// database is shared, and a repeated schedule left behind would make its
// household due at every later test's rollover.
func newScheduleWorldIn(t *testing.T, prefix, tz string) *scheduleWorld {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	ctx := context.Background()
	admin := registerVerified(t, pool, authSvc, mailer, prefix).User.ID
	h, err := app.NewHouseholdService(q, pool).Create(ctx, admin, app.HouseholdInput{Name: "Schedulers", Timezone: tz})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM quest_schedules WHERE household_id = $1`, h.ID); err != nil {
			t.Errorf("delete schedules of %s: %v", h.ID, err)
		}
	})
	return &scheduleWorld{
		t: t, ctx: ctx, q: q, pool: pool, admin: admin, h: h.ID, loc: loc,
		quests:    app.NewQuestService(q, pool),
		family:    app.NewFamilyService(q, pool, 10),
		schedules: app.NewQuestScheduleService(q, pool),
	}
}

// setAdminPin sets the household's admin PIN; later helper calls pass it.
func (w *scheduleWorld) setAdminPin(pin string) {
	w.t.Helper()
	if err := w.family.SetAdminPin(w.ctx, w.admin, w.h, pin, w.pin); err != nil {
		w.t.Fatalf("set admin pin: %v", err)
	}
	w.pin = pin
}

func (w *scheduleWorld) member(name string) uuid.UUID {
	w.t.Helper()
	m, err := w.family.Create(w.ctx, w.admin, w.h, app.FamilyMemberInput{Name: name}, w.pin)
	if err != nil {
		w.t.Fatalf("create member %s: %v", name, err)
	}
	return m.ID
}

func (w *scheduleWorld) quest(in app.QuestInput) uuid.UUID {
	w.t.Helper()
	qv, err := w.quests.Create(w.ctx, w.admin, w.h, in, w.pin)
	if err != nil {
		w.t.Fatalf("create quest %s: %v", in.Name, err)
	}
	return qv.ID
}

// localMidnight is the start of the household-local day days after today.
func (w *scheduleWorld) localMidnight(days int) time.Time {
	n := time.Now().In(w.loc)
	return time.Date(n.Year(), n.Month(), n.Day()+days, 0, 0, 0, 0, w.loc)
}

func (w *scheduleWorld) date(days int) string {
	return w.localMidnight(days).Format(time.DateOnly)
}

func (w *scheduleWorld) schedule(quest, member uuid.UUID, days int) app.QuestSchedule {
	w.t.Helper()
	s, err := w.schedules.Schedule(w.ctx, w.admin, w.h, app.ScheduleInput{QuestID: quest, MemberID: member, Date: w.date(days)}, w.pin)
	if err != nil {
		w.t.Fatalf("schedule: %v", err)
	}
	return s
}

// scheduleOn schedules the quest for the member on a household-local date,
// repeated when repeat is set.
func (w *scheduleWorld) scheduleOn(quest, member uuid.UUID, date string, repeat bool) app.QuestSchedule {
	w.t.Helper()
	s, err := w.schedules.Schedule(w.ctx, w.admin, w.h, app.ScheduleInput{QuestID: quest, MemberID: member, Date: date, Repeat: repeat}, w.pin)
	if err != nil {
		w.t.Fatalf("schedule on %s (repeat %v): %v", date, repeat, err)
	}
	if s.Repeat != repeat {
		w.t.Fatalf("stored schedule repeat = %v, want %v", s.Repeat, repeat)
	}
	return s
}

// claim reads the quest's holder and claimed_at straight from the table.
func (w *scheduleWorld) claim(quest uuid.UUID) (*uuid.UUID, pgtype.Timestamptz) {
	w.t.Helper()
	got, err := w.q.GetHouseholdQuest(w.ctx, db.GetHouseholdQuestParams{HouseholdID: w.h, ID: quest})
	if err != nil {
		w.t.Fatalf("get quest: %v", err)
	}
	return got.ClaimedBy, got.ClaimedAt
}

func (w *scheduleWorld) hasSchedule(id uuid.UUID) bool {
	w.t.Helper()
	for _, got := range w.scheduleIDs() {
		if got == id {
			return true
		}
	}
	return false
}

func (w *scheduleWorld) markDone(quest, member uuid.UUID) {
	w.t.Helper()
	if _, err := w.quests.MarkDone(w.ctx, w.admin, w.h, quest, app.MemberAction{MemberID: member}); err != nil {
		w.t.Fatalf("mark done: %v", err)
	}
}

func (w *scheduleWorld) rollover(now time.Time) int {
	w.t.Helper()
	n, err := app.RolloverDueHouseholds(w.ctx, w.q, w.pool, now)
	if err != nil {
		w.t.Fatalf("RolloverDueHouseholds: %v", err)
	}
	return n
}

// holder reads the quest back through QuestService.List, as the member's
// Mine list does.
func (w *scheduleWorld) holder(quest uuid.UUID) *uuid.UUID {
	w.t.Helper()
	list, _, err := w.quests.List(w.ctx, w.admin, w.h, app.QuestPage{Limit: 100})
	if err != nil {
		w.t.Fatalf("list quests: %v", err)
	}
	for _, v := range list {
		if v.ID == quest {
			return v.ClaimedBy
		}
	}
	w.t.Fatalf("quest %s not in the list", quest)
	return nil
}

func (w *scheduleWorld) scheduleIDs() []uuid.UUID {
	w.t.Helper()
	list, err := w.schedules.List(w.ctx, w.admin, w.h, w.pin)
	if err != nil {
		w.t.Fatalf("list schedules: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	return ids
}

func isHeldBy(got *uuid.UUID, want uuid.UUID) bool { return got != nil && *got == want }

// TestIntegration_QuestSchedules proves CLM-1 against real Postgres: a quest
// scheduled with the admin PIN for a member on a household-local date is
// assigned by RolloverDueHouseholds once the household's local date reaches
// it, exactly as Accept leaves a quest, and not before.
func TestIntegration_QuestSchedules(t *testing.T) {
	t.Run("assigned_at_local_midnight", func(t *testing.T) {
		w := newScheduleWorld(t, "schedmidnight")
		sam := w.member("Sam")
		w.setAdminPin("4321")
		cat := w.quest(app.QuestInput{Name: "Feed the cat", Points: num(5)})
		s, err := w.schedules.Schedule(w.ctx, w.admin, w.h, app.ScheduleInput{QuestID: cat, MemberID: sam, Date: w.date(1)}, "4321")
		if err != nil {
			t.Fatalf("schedule with admin pin: %v", err)
		}
		if s.QuestID != cat || s.MemberID != sam || s.StartDate != w.date(1) || s.QuestName != "Feed the cat" || s.MemberName != "Sam" {
			t.Fatalf("stored schedule: %+v", s)
		}

		midnight := w.localMidnight(1)
		w.rollover(midnight.Add(-time.Minute))
		if got := w.holder(cat); got != nil {
			t.Fatalf("before local midnight the quest is held by %v, want nobody", got)
		}

		after := midnight.Add(time.Minute)
		w.rollover(after)
		if got := w.holder(cat); !isHeldBy(got, sam) {
			t.Fatalf("after local midnight claimed_by = %v, want Sam %v", got, sam)
		}
		got, err := w.q.GetHouseholdQuest(w.ctx, db.GetHouseholdQuestParams{HouseholdID: w.h, ID: cat})
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		// As Accept leaves it: claimed_at set, the deadline the local
		// midnight ending the day it was assigned, no done_at.
		if !got.ClaimedAt.Valid || !got.ClaimedAt.Time.Equal(after) {
			t.Errorf("claimed_at = %v, want %v", got.ClaimedAt, after)
		}
		if want := w.localMidnight(2); !got.ClaimExpiresAt.Valid || !got.ClaimExpiresAt.Time.Equal(want) {
			t.Errorf("claim_expires_at = %v, want %v", got.ClaimExpiresAt, want)
		}
		if got.DoneAt.Valid {
			t.Errorf("done_at = %v, want NULL", got.DoneAt)
		}
		// The one-off schedule ended with its day.
		if ids := w.scheduleIDs(); len(ids) != 0 {
			t.Errorf("schedules after their day: %v, want none", ids)
		}

		// Running again never assigns twice: Sam abandons, and another run
		// of the same day leaves the quest in the pool.
		if _, err := w.quests.Abandon(w.ctx, w.admin, w.h, cat, app.MemberAction{MemberID: sam}); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		w.rollover(after.Add(time.Minute))
		if got := w.holder(cat); got != nil {
			t.Errorf("second run of the day re-assigned the quest to %v", got)
		}
	})

	// CLM-3: before the rollover on a schedule's date the schedule is in the
	// member's Upcoming and the quest is not held; once that rollover
	// assigns it the quest is held and the one-off is gone from Upcoming.
	t.Run("upcoming_before_and_after_rollover", func(t *testing.T) {
		w := newScheduleWorld(t, "schedupcoming")
		sam, ann := w.member("Sam"), w.member("Ann")
		w.setAdminPin("4321")
		cat := w.quest(app.QuestInput{Name: "Feed the cat", Points: num(5)})
		room := w.quest(app.QuestInput{Name: "Clean your room", Points: num(5)})
		dishes := w.quest(app.QuestInput{Name: "Dishes", Points: num(5)})

		if got, err := w.schedules.Upcoming(w.ctx, w.admin, w.h, sam); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("Upcoming with nothing scheduled = %#v, %v; want an empty non-nil slice", got, err)
		}

		catS := w.schedule(cat, sam, 1)
		roomS := w.schedule(room, sam, 2)
		w.schedule(dishes, ann, 1) // another member's: never in Sam's list

		up, err := w.schedules.Upcoming(w.ctx, w.admin, w.h, sam)
		if err != nil {
			t.Fatalf("Upcoming before rollover: %v", err)
		}
		if len(up) != 2 || up[0].ID != catS.ID || up[1].ID != roomS.ID {
			t.Fatalf("Upcoming before rollover = %+v, want Feed the cat then Clean your room", up)
		}
		if up[0].QuestID != cat || up[0].QuestName != "Feed the cat" || up[0].StartDate != w.date(1) || up[0].NextDate != w.date(1) || up[0].Repeat {
			t.Errorf("Feed the cat upcoming = %+v, want one-off next_date %s", up[0], w.date(1))
		}
		if up[1].NextDate != w.date(2) {
			t.Errorf("Clean your room next_date = %s, want %s", up[1].NextDate, w.date(2))
		}
		if got := w.holder(cat); got != nil {
			t.Fatalf("before its day the quest is held by %v, want nobody", got)
		}

		w.rollover(w.localMidnight(1).Add(time.Minute))
		if got := w.holder(cat); !isHeldBy(got, sam) {
			t.Fatalf("after the rollover claimed_by = %v, want Sam %v", got, sam)
		}
		up, err = w.schedules.Upcoming(w.ctx, w.admin, w.h, sam)
		if err != nil {
			t.Fatalf("Upcoming after rollover: %v", err)
		}
		if len(up) != 1 || up[0].ID != roomS.ID {
			t.Errorf("Upcoming after rollover = %+v, want only Clean your room (the assigned one-off gone)", up)
		}
		// Leave nothing due behind: the test database is shared, and a later
		// subtest's rollover counts every due household.
		if err := w.schedules.Cancel(w.ctx, w.admin, w.h, roomS.ID, w.pin); err != nil {
			t.Fatalf("cancel leftover schedule: %v", err)
		}
	})

	t.Run("expiry_runs_before_assignment", func(t *testing.T) {
		w := newScheduleWorld(t, "schedexpiry")
		ann, ben := w.member("Ann"), w.member("Ben")
		q := w.quest(app.QuestInput{Name: "Dishes", Points: num(5)})
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, q, app.MemberAction{MemberID: ann}); err != nil {
			t.Fatalf("Ann accept: %v", err)
		}
		w.schedule(q, ben, 1)
		// Ann's hold ends at tonight's local midnight; one run just after it
		// puts the quest back and gives it to Ben.
		w.rollover(w.localMidnight(1).Add(time.Minute))
		if got := w.holder(q); !isHeldBy(got, ben) {
			t.Fatalf("after one run claimed_by = %v, want Ben %v", got, ben)
		}
	})

	t.Run("held_by_another_member_is_skipped", func(t *testing.T) {
		w := newScheduleWorld(t, "schedheld")
		ann, ben := w.member("Ann"), w.member("Ben")
		q := w.quest(app.QuestInput{Name: "Recycling", Points: num(5)})
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, q, app.MemberAction{MemberID: ann}); err != nil {
			t.Fatalf("Ann accept: %v", err)
		}
		w.schedule(q, ben, 0) // today; this household has never rolled over
		w.rollover(time.Now())
		if got := w.holder(q); !isHeldBy(got, ann) {
			t.Fatalf("held quest moved to %v, want Ann %v kept", got, ann)
		}
		if ids := w.scheduleIDs(); len(ids) != 0 {
			t.Errorf("one-off skipped for a held quest still listed: %v", ids)
		}
	})

	t.Run("resting_quest_is_skipped_and_completion_kept", func(t *testing.T) {
		w := newScheduleWorld(t, "schedrest")
		ann, ben := w.member("Ann"), w.member("Ben")
		q := w.quest(app.QuestInput{Name: "Mow", Points: num(5), RepeatDays: num(3)})
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, q, app.MemberAction{MemberID: ann}); err != nil {
			t.Fatalf("accept: %v", err)
		}
		done, err := w.quests.MarkDone(w.ctx, w.admin, w.h, q, app.MemberAction{MemberID: ann})
		if err != nil {
			t.Fatalf("mark done: %v", err)
		}
		w.schedule(q, ben, 1)
		w.rollover(w.localMidnight(1).Add(time.Minute))
		got, err := w.q.GetHouseholdQuest(w.ctx, db.GetHouseholdQuestParams{HouseholdID: w.h, ID: q})
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		if got.ClaimedBy != nil {
			t.Errorf("resting quest assigned to %v", got.ClaimedBy)
		}
		if !got.AvailableAt.Valid || !got.AvailableAt.Time.Equal(done.AvailableAt.Time) {
			t.Errorf("cooldown changed: %v, want %v", got.AvailableAt, done.AvailableAt)
		}
		pending, err := w.quests.ListPending(w.ctx, w.admin, w.h, "")
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		if len(pending) != 1 || pending[0].QuestID != q {
			t.Errorf("pending completions after rollover: %+v, want Ann's one", pending)
		}
		if ids := w.scheduleIDs(); len(ids) != 0 {
			t.Errorf("one-off skipped for a resting quest still listed: %v", ids)
		}
	})

	t.Run("age_skip_keeps_schedule", func(t *testing.T) {
		w := newScheduleWorld(t, "schedage")
		kid := w.member("Kid")
		n := time.Now().In(w.loc)
		bday := time.Date(n.Year()-5, n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		if _, err := w.family.SetBirthday(w.ctx, w.admin, w.h, kid, &bday, ""); err != nil {
			t.Fatalf("set birthday: %v", err)
		}
		q := w.quest(app.QuestInput{Name: "Drive to the shop", Points: num(5), MinAge: num(16)})
		s := w.schedule(q, kid, 1)
		w.rollover(w.localMidnight(1).Add(time.Minute))
		if got := w.holder(q); got != nil {
			t.Errorf("quest assigned to %v outside its age range", got)
		}
		if ids := w.scheduleIDs(); len(ids) != 1 || ids[0] != s.ID {
			t.Errorf("age-skipped schedule: %v, want %v kept", ids, s.ID)
		}
	})

	t.Run("created_for_today_after_rollover_assigns_at_once", func(t *testing.T) {
		w := newScheduleWorld(t, "schedtoday")
		ann := w.member("Ann")
		first := w.quest(app.QuestInput{Name: "First", Points: num(5)})
		w.schedule(first, ann, 0)
		w.rollover(time.Now()) // today's rollover has run
		if got := w.holder(first); !isHeldBy(got, ann) {
			t.Fatalf("first quest: %v, want Ann", got)
		}
		second := w.quest(app.QuestInput{Name: "Second", Points: num(5)})
		w.schedule(second, ann, 0)
		if got := w.holder(second); !isHeldBy(got, ann) {
			t.Fatalf("quest scheduled for today after the rollover: held by %v, want Ann at once", got)
		}
	})

	t.Run("refusals_write_nothing", func(t *testing.T) {
		w := newScheduleWorld(t, "schedrefuse")
		ann := w.member("Ann")
		q := w.quest(app.QuestInput{Name: "Dust", Points: num(5)})
		other := newScheduleWorld(t, "schedrefuseother")
		otherMember := other.member("Mal")
		otherQuest := other.quest(app.QuestInput{Name: "Theirs", Points: num(5)})

		cases := []struct {
			name string
			in   app.ScheduleInput
		}{
			{"missing date", app.ScheduleInput{QuestID: q, MemberID: ann}},
			{"bad date", app.ScheduleInput{QuestID: q, MemberID: ann, Date: "21/11/2026"}},
			{"past date", app.ScheduleInput{QuestID: q, MemberID: ann, Date: w.date(-1)}},
		}
		for _, c := range cases {
			_, err := w.schedules.Schedule(w.ctx, w.admin, w.h, c.in, "")
			var ve *app.ValidationError
			if !errors.As(err, &ve) || ve.Fields["date"] == "" {
				t.Errorf("%s: err = %v, want a ValidationError on date", c.name, err)
			}
		}
		for name, in := range map[string]app.ScheduleInput{
			"other household's quest":  {QuestID: otherQuest, MemberID: ann, Date: w.date(1)},
			"other household's member": {QuestID: q, MemberID: otherMember, Date: w.date(1)},
		} {
			if _, err := w.schedules.Schedule(w.ctx, w.admin, w.h, in, ""); !errors.Is(err, app.ErrNotFound) {
				t.Errorf("%s: err = %v, want ErrNotFound", name, err)
			}
		}
		// A non-member admin of another household gets ErrNotFound.
		if _, err := w.schedules.Schedule(w.ctx, other.admin, w.h, app.ScheduleInput{QuestID: q, MemberID: ann, Date: w.date(1)}, ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("non-member: err = %v, want ErrNotFound", err)
		}
		w.setAdminPin("4321")
		in := app.ScheduleInput{QuestID: q, MemberID: ann, Date: w.date(1)}
		if _, err := w.schedules.Schedule(w.ctx, w.admin, w.h, in, ""); !errors.Is(err, app.ErrAdminPinRequired) {
			t.Errorf("no admin pin: err = %v, want ErrAdminPinRequired", err)
		}
		if _, err := w.schedules.Schedule(w.ctx, w.admin, w.h, in, "0000"); !errors.Is(err, app.ErrWrongPIN) {
			t.Errorf("wrong admin pin: err = %v, want ErrWrongPIN", err)
		}
		list, err := w.schedules.List(w.ctx, w.admin, w.h, "4321")
		if err != nil || list == nil || len(list) != 0 {
			t.Errorf("after refusals: %v %v, want an empty non-nil list", list, err)
		}
	})

	t.Run("cancel_and_cascade", func(t *testing.T) {
		w := newScheduleWorld(t, "schedcancel")
		ann := w.member("Ann")
		a := w.quest(app.QuestInput{Name: "A", Points: num(5)})
		b := w.quest(app.QuestInput{Name: "B", Points: num(5)})
		sa := w.schedule(a, ann, 1)
		w.schedule(b, ann, 1)
		if err := w.schedules.Cancel(w.ctx, w.admin, w.h, sa.ID, ""); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if err := w.schedules.Cancel(w.ctx, w.admin, w.h, sa.ID, ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("cancel twice: %v, want ErrNotFound", err)
		}
		if err := w.quests.Delete(w.ctx, w.admin, w.h, b, ""); err != nil {
			t.Fatalf("delete quest: %v", err)
		}
		if ids := w.scheduleIDs(); len(ids) != 0 {
			t.Errorf("schedules after cancel and quest delete: %v", ids)
		}
		w.rollover(w.localMidnight(1).Add(time.Minute))
		if got := w.holder(a); got != nil {
			t.Errorf("cancelled schedule fired: %v", got)
		}
	})

	t.Run("missed_midnights_run_once", func(t *testing.T) {
		w := newScheduleWorld(t, "schedmissed")
		ann := w.member("Ann")
		a := w.quest(app.QuestInput{Name: "Day one", Points: num(5)})
		b := w.quest(app.QuestInput{Name: "Day two", Points: num(5)})
		w.schedule(a, ann, 1)
		w.schedule(b, ann, 2)
		// The server was down over both midnights: the first run after
		// them assigns both days' schedules in one pass.
		now := w.localMidnight(2).Add(time.Hour)
		if n := w.rollover(now); n != 2 {
			t.Fatalf("first run after two missed midnights assigned %d, want 2", n)
		}
		if !isHeldBy(w.holder(a), ann) || !isHeldBy(w.holder(b), ann) {
			t.Fatalf("missed schedules not assigned: a=%v b=%v", w.holder(a), w.holder(b))
		}
		if ids := w.scheduleIDs(); len(ids) != 0 {
			t.Errorf("one-off schedules after their pass: %v", ids)
		}
		// A second run of the same local date is a no-op.
		if _, err := w.quests.Abandon(w.ctx, w.admin, w.h, a, app.MemberAction{MemberID: ann}); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		if n := w.rollover(now.Add(time.Minute)); n != 0 {
			t.Errorf("second run of the day assigned %d, want 0", n)
		}
		if got := w.holder(a); got != nil {
			t.Errorf("second run re-assigned the abandoned quest to %v", got)
		}
	})

	t.Run("missed_midnights_same_quest_assigns_the_latest_day", func(t *testing.T) {
		w := newScheduleWorld(t, "schedmissedsame")
		ann := w.member("Ann")
		bob := w.member("Bob")
		q := w.quest(app.QuestInput{Name: "Shared chore", Points: num(5)})
		w.schedule(q, ann, 1) // Monday's one-off
		w.schedule(q, bob, 2) // Tuesday's one-off, the same quest
		// The server was down over both midnights: the claim is for
		// Tuesday, so Tuesday's member holds it and Monday's one-off ends
		// with its day.
		now := w.localMidnight(2).Add(time.Hour)
		if n := w.rollover(now); n != 1 {
			t.Fatalf("first run after two missed midnights assigned %d, want 1", n)
		}
		if got := w.holder(q); !isHeldBy(got, bob) {
			t.Fatalf("after missed midnights the quest is held by %v, want Bob %v (today's schedule)", got, bob)
		}
		if ids := w.scheduleIDs(); len(ids) != 0 {
			t.Errorf("one-off schedules after their pass: %v", ids)
		}
	})

	t.Run("concurrent_runs_assign_once", func(t *testing.T) {
		w := newScheduleWorld(t, "schedconc")
		ann := w.member("Ann")
		q := w.quest(app.QuestInput{Name: "Race", Points: num(5)})
		w.schedule(q, ann, 1)
		now := w.localMidnight(1).Add(time.Minute)
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = app.RolloverDueHouseholds(w.ctx, w.q, w.pool, now)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("run %d: %v", i, err)
			}
		}
		got, err := w.q.GetHouseholdQuest(w.ctx, db.GetHouseholdQuestParams{HouseholdID: w.h, ID: q})
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		if !isHeldBy(got.ClaimedBy, ann) || !got.ClaimedAt.Time.Equal(now) {
			t.Errorf("after concurrent runs: claimed_by=%v claimed_at=%v", got.ClaimedBy, got.ClaimedAt)
		}
	})

	t.Run("failing_household_is_logged_others_run", func(t *testing.T) {
		bad := newScheduleWorld(t, "schedbad")
		good := newScheduleWorld(t, "schedgood")
		badMember, goodMember := bad.member("Bad"), good.member("Good")
		badQuest := bad.quest(app.QuestInput{Name: "Bad", Points: num(5)})
		goodQuest := good.quest(app.QuestInput{Name: "Good", Points: num(5)})
		bad.schedule(badQuest, badMember, 1)
		good.schedule(goodQuest, goodMember, 1)

		// The bad household's rollover fails at its last step.
		ctx := context.Background()
		if _, err := bad.pool.Exec(ctx, `
			CREATE OR REPLACE FUNCTION test_fail_rollover() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'injected rollover failure'; END $$;
			CREATE TRIGGER test_fail_rollover BEFORE INSERT ON household_quest_rollovers
			    FOR EACH ROW WHEN (NEW.household_id = '`+bad.h.String()+`'::uuid)
			    EXECUTE FUNCTION test_fail_rollover();`); err != nil {
			t.Fatalf("install failing trigger: %v", err)
		}
		t.Cleanup(func() {
			cctx := context.Background()
			if _, err := bad.pool.Exec(cctx, `
				DROP TRIGGER IF EXISTS test_fail_rollover ON household_quest_rollovers;
				DROP FUNCTION IF EXISTS test_fail_rollover();`); err != nil {
				t.Errorf("drop failing trigger: %v", err)
			}
			// Its schedule is left due by the failure; it must not fire in
			// a later run.
			if _, err := bad.pool.Exec(cctx, `DELETE FROM quest_schedules WHERE household_id = $1`, bad.h); err != nil {
				t.Errorf("delete failing household's schedules: %v", err)
			}
		})

		var buf bytes.Buffer
		var mu sync.Mutex
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })

		good.rollover(good.localMidnight(1).Add(time.Minute))
		if got := good.holder(goodQuest); !isHeldBy(got, goodMember) {
			t.Errorf("good household not assigned: %v", got)
		}
		if got := bad.holder(badQuest); got != nil {
			t.Errorf("failing household's assignment committed: %v", got)
		}
		mu.Lock()
		logged := buf.String()
		mu.Unlock()
		if !strings.Contains(logged, bad.h.String()) || !strings.Contains(logged, "injected rollover failure") {
			t.Errorf("log does not name the household and its cause: %q", logged)
		}
		if ids := bad.scheduleIDs(); len(ids) != 1 {
			t.Errorf("failing household's schedule: %v, want kept for the next run", ids)
		}
	})
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestIntegration_QuestScheduleUpcoming proves CLM-1 and CLM-4 against real
// Postgres: Upcoming returns an empty non-nil slice for a member with
// nothing scheduled, lists the member's future one-offs with next_date the
// start date, ordered by next_date and then quest name, leaves out a
// one-off whose date has passed, drops a one-off once its day's rollover
// assigned it, and answers ErrNotFound for a member id not in the
// household.
func TestIntegration_QuestScheduleUpcoming(t *testing.T) {
	w := newScheduleWorld(t, "schedupnext")
	sam, ann := w.member("Sam"), w.member("Ann")
	w.setAdminPin("4321")

	got, err := w.schedules.Upcoming(w.ctx, w.admin, w.h, sam)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("Upcoming with nothing scheduled = %#v, %v; want an empty non-nil slice", got, err)
	}

	brush := w.quest(app.QuestInput{Name: "Brush the dog", Points: num(5)})
	apple := w.quest(app.QuestInput{Name: "Apple picking", Points: num(5)})
	laundry := w.quest(app.QuestInput{Name: "Laundry", Points: num(5)})
	old := w.quest(app.QuestInput{Name: "Old chore", Points: num(5)})
	other := w.quest(app.QuestInput{Name: "Ann's chore", Points: num(5)})

	// Scheduled out of order: Laundry (day 3), Brush and Apple (both day 2).
	laundryS := w.schedule(laundry, sam, 3)
	brushS := w.schedule(brush, sam, 2)
	appleS := w.schedule(apple, sam, 2)
	w.schedule(other, ann, 1) // another member's: never in Sam's list

	// A one-off whose date has passed, as one kept after an age skip is.
	// Schedule refuses a past date, so it is stored with the query itself.
	past := w.localMidnight(-2)
	if _, err := w.q.CreateQuestSchedule(w.ctx, db.CreateQuestScheduleParams{
		ID: uuid.Must(uuid.NewV7()), StartDate: pgtype.Date{Time: time.Date(past.Year(), past.Month(), past.Day(), 0, 0, 0, 0, time.UTC), Valid: true},
		HouseholdID: w.h, QuestID: old, MemberID: sam,
	}); err != nil {
		t.Fatalf("store passed schedule: %v", err)
	}

	up, err := w.schedules.Upcoming(w.ctx, w.admin, w.h, sam)
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	want := []struct {
		id    uuid.UUID
		quest uuid.UUID
		name  string
		date  string
	}{
		{appleS.ID, apple, "Apple picking", w.date(2)},
		{brushS.ID, brush, "Brush the dog", w.date(2)},
		{laundryS.ID, laundry, "Laundry", w.date(3)},
	}
	if len(up) != len(want) {
		t.Fatalf("Upcoming = %+v, want %d items (passed one-off and Ann's left out)", up, len(want))
	}
	for i, wnt := range want {
		u := up[i]
		if u.ID != wnt.id || u.QuestID != wnt.quest || u.QuestName != wnt.name || u.StartDate != wnt.date || u.NextDate != wnt.date || u.Repeat {
			t.Errorf("Upcoming[%d] = %+v, want %s one-off on %s", i, u, wnt.name, wnt.date)
		}
	}

	// Once the rollover on day 2 assigns Apple and Brush, only Laundry is
	// upcoming.
	w.rollover(w.localMidnight(2).Add(time.Minute))
	if h := w.holder(apple); !isHeldBy(h, sam) {
		t.Fatalf("after day 2's rollover Apple picking is held by %v, want Sam", h)
	}
	up, err = w.schedules.Upcoming(w.ctx, w.admin, w.h, sam)
	if err != nil {
		t.Fatalf("Upcoming after rollover: %v", err)
	}
	if len(up) != 1 || up[0].ID != laundryS.ID {
		t.Errorf("Upcoming after rollover = %+v, want only Laundry", up)
	}
	// Leave nothing due behind: the test database is shared, and another
	// test's rollover counts every due household.
	if err := w.schedules.Cancel(w.ctx, w.admin, w.h, laundryS.ID, w.pin); err != nil {
		t.Fatalf("cancel leftover schedule: %v", err)
	}

	if _, err := w.schedules.Upcoming(w.ctx, w.admin, w.h, uuid.New()); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Upcoming for a member not in the household: err = %v, want ErrNotFound", err)
	}

	// CLM-9: repeated schedules carry repeat, and their next date is the
	// local date the next midnight pass can assign them: the start date for
	// a free quest, the local date of available_at for a resting one.
	t.Run("repeat_flag_and_next_date", func(t *testing.T) {
		w := newScheduleWorld(t, "schedupreps")
		sam := w.member("Sam")
		resting := w.quest(app.QuestInput{Name: "Clean your room", Points: num(5), RepeatDays: num(7)})
		free := w.quest(app.QuestInput{Name: "Feed the cat", Points: num(5), RepeatDays: num(1)})
		oneOff := w.quest(app.QuestInput{Name: "Water the plants", Points: num(5)})

		// The 7-day quest is done today: it rests until a local midnight
		// well after tomorrow, the day its schedule starts.
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, resting, app.MemberAction{MemberID: sam}); err != nil {
			t.Fatalf("accept: %v", err)
		}
		w.markDone(resting, sam)
		got, err := w.q.GetHouseholdQuest(w.ctx, db.GetHouseholdQuestParams{HouseholdID: w.h, ID: resting})
		if err != nil || !got.AvailableAt.Valid {
			t.Fatalf("resting quest = %+v, %v; want available_at set", got, err)
		}
		restDate := got.AvailableAt.Time.In(w.loc).Format(time.DateOnly)
		if restDate <= w.date(3) {
			t.Fatalf("test setup: available_at date %s is not after %s", restDate, w.date(3))
		}

		restingS := w.scheduleOn(resting, sam, w.date(1), true)
		freeS := w.scheduleOn(free, sam, w.date(3), true)
		oneOffS := w.scheduleOn(oneOff, sam, w.date(5), false)

		up, err := w.schedules.Upcoming(w.ctx, w.admin, w.h, sam)
		if err != nil {
			t.Fatalf("Upcoming: %v", err)
		}
		want := []struct {
			id           uuid.UUID
			start, next  string
			repeat       bool
			questMessage string
		}{
			{freeS.ID, w.date(3), w.date(3), true, "free repeated quest"},
			{oneOffS.ID, w.date(5), w.date(5), false, "one-off"},
			{restingS.ID, w.date(1), restDate, true, "resting repeated quest"},
		}
		if len(up) != len(want) {
			t.Fatalf("Upcoming = %+v, want %d items", up, len(want))
		}
		for i, wnt := range want {
			u := up[i]
			if u.ID != wnt.id || u.StartDate != wnt.start || u.NextDate != wnt.next || u.Repeat != wnt.repeat {
				t.Errorf("Upcoming[%d] = %+v, want the %s: start %s, next %s, repeat %v",
					i, u, wnt.questMessage, wnt.start, wnt.next, wnt.repeat)
			}
		}
	})
}

// TestIntegration_QuestRolloverRepeat proves the repeated schedule against
// real Postgres, through RolloverDueHouseholds as the midnight pass runs it:
// a repeated schedule assigns its quest at every household-local midnight
// at which the quest is free, is never consumed, and its cadence is the
// quest's own cooldown; a one-off fires on its day only. MarkDone stamps
// the cooldown from the real clock (today), so each timeline starts today
// and the passes run at later local midnights.
func TestIntegration_QuestRolloverRepeat(t *testing.T) {
	// CLM-2, CLM-7: a 1-day quest is assigned at each of three midnights,
	// the member marking it done between; the row stays, and a second run
	// of the same local day writes nothing.
	t.Run("daily_assigned_every_midnight", func(t *testing.T) {
		w := newScheduleWorld(t, "repdaily")
		sam := w.member("Sam")
		cat := w.quest(app.QuestInput{Name: "Feed the cat", Points: num(5), RepeatDays: num(1)})
		s := w.scheduleOn(cat, sam, w.date(1), true)
		for day := 1; day <= 3; day++ {
			at := w.localMidnight(day).Add(time.Minute)
			w.rollover(at)
			holder, claimedAt := w.claim(cat)
			if !isHeldBy(holder, sam) || !claimedAt.Time.Equal(at) {
				t.Fatalf("day %d: claimed_by=%v claimed_at=%v, want Sam at %v", day, holder, claimedAt.Time, at)
			}
			if !w.hasSchedule(s.ID) {
				t.Fatalf("day %d: repeated schedule consumed", day)
			}
			// Same local day again (as after a restart): nothing new.
			w.rollover(at.Add(time.Minute))
			if holder, again := w.claim(cat); !isHeldBy(holder, sam) || !again.Time.Equal(at) {
				t.Fatalf("day %d second run: claimed_by=%v claimed_at=%v, want unchanged %v", day, holder, again.Time, at)
			}
			// Done today: the cooldown ends at tomorrow's real local
			// midnight, before the next pass.
			w.markDone(cat, sam)
		}
	})

	// CLM-1: a 7-day quest done today rests until the local midnight 7
	// days on; the six passes before it assign nothing, that one does.
	t.Run("weekly_waits_for_cooldown", func(t *testing.T) {
		w := newScheduleWorld(t, "repweekly")
		sam := w.member("Sam")
		room := w.quest(app.QuestInput{Name: "Clean your room", Points: num(5), RepeatDays: num(7)})
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, room, app.MemberAction{MemberID: sam}); err != nil {
			t.Fatalf("accept: %v", err)
		}
		w.markDone(room, sam)
		s := w.scheduleOn(room, sam, w.date(1), true)
		for day := 1; day <= 6; day++ {
			w.rollover(w.localMidnight(day).Add(time.Minute))
			if holder, _ := w.claim(room); holder != nil {
				t.Fatalf("day %d: resting quest assigned to %v", day, holder)
			}
			if !w.hasSchedule(s.ID) {
				t.Fatalf("day %d: repeated schedule consumed while the quest rested", day)
			}
		}
		w.rollover(w.localMidnight(7).Add(time.Minute))
		if holder, _ := w.claim(room); !isHeldBy(holder, sam) {
			t.Fatalf("day 7: claimed_by = %v, want Sam once the cooldown ended", holder)
		}
	})

	// CLM-1, CLM-7: two households in different timezones each assign a
	// repeated schedule at their own local midnight of the same date D.
	t.Run("each_timezone_at_its_own_midnight", func(t *testing.T) {
		akl := newScheduleWorld(t, "repakl")
		lax := newScheduleWorldIn(t, "replax", "America/Los_Angeles")
		aklKid, laxKid := akl.member("Kia"), lax.member("Lou")
		aklQuest := akl.quest(app.QuestInput{Name: "Auckland chore", Points: num(1)})
		laxQuest := lax.quest(app.QuestInput{Name: "LA chore", Points: num(1)})
		dayD := akl.date(2)
		akl.scheduleOn(aklQuest, aklKid, dayD, true)
		lax.scheduleOn(laxQuest, laxKid, dayD, true)
		aklAt := akl.localMidnight(2).Add(time.Hour)
		if got := aklAt.In(lax.loc).Format(time.DateOnly); got >= dayD {
			t.Fatalf("test setup: Los Angeles date at %v is %s, want before %s", aklAt, got, dayD)
		}
		akl.rollover(aklAt)
		if holder, _ := akl.claim(aklQuest); !isHeldBy(holder, aklKid) {
			t.Fatalf("Auckland after its midnight: %v, want %v", holder, aklKid)
		}
		if holder, _ := lax.claim(laxQuest); holder != nil {
			t.Fatalf("Los Angeles assigned before its own midnight: %v", holder)
		}
		d, err := time.Parse(time.DateOnly, dayD)
		if err != nil {
			t.Fatalf("parse D: %v", err)
		}
		laxAt := time.Date(d.Year(), d.Month(), d.Day(), 1, 0, 0, 0, lax.loc)
		lax.rollover(laxAt)
		if holder, _ := lax.claim(laxQuest); !isHeldBy(holder, laxKid) {
			t.Fatalf("Los Angeles after its midnight: %v, want %v", holder, laxKid)
		}
		// laxAt is already D+1 in Auckland, so that run is also Auckland's
		// next midnight pass: its quest is still Kia's, re-assigned there
		// after the end-of-day expiry, and never Lou's.
		if holder, _ := akl.claim(aklQuest); !isHeldBy(holder, aklKid) {
			t.Fatalf("Auckland after Los Angeles's pass: %v, want %v", holder, aklKid)
		}
	})

	// CLM-3 (a): abandoned, the quest is assigned again at the next pass.
	t.Run("abandoned_reassigned_next_midnight", func(t *testing.T) {
		w := newScheduleWorld(t, "repabandon")
		sam := w.member("Sam")
		q := w.quest(app.QuestInput{Name: "Dishes", Points: num(5)})
		w.scheduleOn(q, sam, w.date(1), true)
		day1 := w.localMidnight(1).Add(time.Minute)
		w.rollover(day1)
		if holder, _ := w.claim(q); !isHeldBy(holder, sam) {
			t.Fatalf("day 1: %v, want Sam", holder)
		}
		if _, err := w.quests.Abandon(w.ctx, w.admin, w.h, q, app.MemberAction{MemberID: sam}); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		w.rollover(day1.Add(time.Minute))
		if holder, _ := w.claim(q); holder != nil {
			t.Fatalf("second run of day 1 re-assigned the abandoned quest to %v", holder)
		}
		day2 := w.localMidnight(2).Add(time.Minute)
		w.rollover(day2)
		if holder, claimedAt := w.claim(q); !isHeldBy(holder, sam) || !claimedAt.Time.Equal(day2) {
			t.Fatalf("day 2: %v at %v, want Sam at %v", holder, claimedAt.Time, day2)
		}
	})

	// CLM-3 (b): left unfinished, the quest is returned by the end-of-day
	// expiry and assigned again in the same pass, with a fresh claimed_at.
	t.Run("expired_reassigned_same_pass", func(t *testing.T) {
		w := newScheduleWorld(t, "repexpire")
		sam := w.member("Sam")
		q := w.quest(app.QuestInput{Name: "Recycling", Points: num(5)})
		w.scheduleOn(q, sam, w.date(1), true)
		w.rollover(w.localMidnight(1).Add(time.Minute))
		day2 := w.localMidnight(2).Add(time.Minute)
		w.rollover(day2)
		got, err := w.q.GetHouseholdQuest(w.ctx, db.GetHouseholdQuestParams{HouseholdID: w.h, ID: q})
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		if !isHeldBy(got.ClaimedBy, sam) || !got.ClaimedAt.Time.Equal(day2) {
			t.Fatalf("day 2: %v at %v, want Sam at %v (expired first, then assigned)", got.ClaimedBy, got.ClaimedAt.Time, day2)
		}
		if want := w.localMidnight(3); !got.ClaimExpiresAt.Time.Equal(want) {
			t.Errorf("claim_expires_at = %v, want %v", got.ClaimExpiresAt.Time, want)
		}
	})

	// CLM-4: once cancelled, a repeated schedule never assigns again and
	// leaves the list and Upcoming; the quest already assigned is kept.
	t.Run("cancel_stops_future_assignments", func(t *testing.T) {
		w := newScheduleWorld(t, "repcancel")
		sam := w.member("Sam")
		w.setAdminPin("4321")
		q := w.quest(app.QuestInput{Name: "Walk the dog", Points: num(5)})
		s := w.scheduleOn(q, sam, w.date(1), true)
		w.rollover(w.localMidnight(1).Add(time.Minute))
		if holder, _ := w.claim(q); !isHeldBy(holder, sam) {
			t.Fatalf("day 1: %v, want Sam", holder)
		}
		w.markDone(q, sam)
		if err := w.schedules.Cancel(w.ctx, w.admin, w.h, s.ID, w.pin); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		for day := 2; day <= 4; day++ {
			w.rollover(w.localMidnight(day).Add(time.Minute))
			if holder, _ := w.claim(q); holder != nil {
				t.Fatalf("day %d: cancelled schedule assigned to %v", day, holder)
			}
		}
		if ids := w.scheduleIDs(); len(ids) != 0 {
			t.Errorf("schedules after cancel: %v, want none", ids)
		}
		up, err := w.schedules.Upcoming(w.ctx, w.admin, w.h, sam)
		if err != nil || len(up) != 0 {
			t.Errorf("Upcoming after cancel = %+v, %v; want empty", up, err)
		}
		pending, err := w.quests.ListPending(w.ctx, w.admin, w.h, w.pin)
		if err != nil || len(pending) != 1 || pending[0].QuestID != q {
			t.Errorf("completion of the assigned quest after cancel = %+v, %v; want Sam's one kept", pending, err)
		}
	})

	// CLM-5: a one-off fires on its day only and ends there, assigned or
	// skipped for a held quest; one skipped for age is kept.
	t.Run("one_off_ends_with_its_day", func(t *testing.T) {
		w := newScheduleWorld(t, "reponeoff")
		sam, ann, kid := w.member("Sam"), w.member("Ann"), w.member("Kid")
		n := time.Now().In(w.loc)
		bday := time.Date(n.Year()-5, n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		if _, err := w.family.SetBirthday(w.ctx, w.admin, w.h, kid, &bday, ""); err != nil {
			t.Fatalf("set birthday: %v", err)
		}
		free := w.quest(app.QuestInput{Name: "Free", Points: num(5)})
		held := w.quest(app.QuestInput{Name: "Held", Points: num(5)})
		adult := w.quest(app.QuestInput{Name: "Adult", Points: num(5), MinAge: num(16)})
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, held, app.MemberAction{MemberID: ann}); err != nil {
			t.Fatalf("Ann accept: %v", err)
		}
		// A repeated schedule of an unrelated quest keeps the household
		// due at the next midnight, so that pass runs for it: its
		// end-of-day expiry first, then the assign sweep.
		other := w.quest(app.QuestInput{Name: "Other", Points: num(5)})
		// Today's pass has not run for this household yet.
		freeS := w.scheduleOn(free, sam, w.date(0), false)
		heldS := w.scheduleOn(held, sam, w.date(0), false)
		ageS := w.scheduleOn(adult, kid, w.date(0), false)
		otherS := w.scheduleOn(other, ann, w.date(0), true)
		w.rollover(time.Now())
		if holder, _ := w.claim(free); !isHeldBy(holder, sam) {
			t.Fatalf("free one-off: %v, want Sam", holder)
		}
		if holder, _ := w.claim(held); !isHeldBy(holder, ann) {
			t.Fatalf("held quest moved to %v, want Ann kept", holder)
		}
		if holder, _ := w.claim(adult); holder != nil {
			t.Fatalf("out-of-age one-off assigned to %v", holder)
		}
		if w.hasSchedule(freeS.ID) || w.hasSchedule(heldS.ID) {
			t.Fatalf("one-offs after their day: %v, want the assigned and the held-skip ones gone", w.scheduleIDs())
		}
		if !w.hasSchedule(ageS.ID) {
			t.Fatalf("age-skipped one-off removed, want it kept")
		}
		// Next midnight: the end-of-day rule runs first, so Sam's unfinished
		// scheduled hold and Ann's own hold both go back to the pool; then
		// no one-off retries, so none of the three is assigned afresh.
		w.rollover(w.localMidnight(1).Add(time.Minute))
		for name, q := range map[string]uuid.UUID{"free": free, "held": held, "adult": adult} {
			if holder, at := w.claim(q); holder != nil || at.Valid {
				t.Errorf("%s quest the day after its one-off: held by %v at %v, want back in the pool", name, holder, at.Time)
			}
		}
		if ids := w.scheduleIDs(); len(ids) != 2 || !w.hasSchedule(ageS.ID) || !w.hasSchedule(otherS.ID) {
			t.Errorf("schedules the day after: %v, want only the age-skipped %v and the repeated %v", ids, ageS.ID, otherS.ID)
		}
	})

	// CLM-8: a repeated schedule that finds its quest held by another
	// member, or its member outside the age range, assigns nothing, keeps
	// the schedule, raises no error and tries again at the next pass.
	t.Run("held_or_age_blocked_kept_and_retried", func(t *testing.T) {
		w := newScheduleWorld(t, "repheld")
		sam, ann, kid := w.member("Sam"), w.member("Ann"), w.member("Kid")
		n := time.Now().In(w.loc)
		bday := time.Date(n.Year()-5, n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		if _, err := w.family.SetBirthday(w.ctx, w.admin, w.h, kid, &bday, ""); err != nil {
			t.Fatalf("set birthday: %v", err)
		}
		held := w.quest(app.QuestInput{Name: "Held", Points: num(5)})
		adult := w.quest(app.QuestInput{Name: "Adult", Points: num(5), MinAge: num(16)})
		if _, err := w.quests.Accept(w.ctx, w.admin, w.h, held, app.MemberAction{MemberID: ann}); err != nil {
			t.Fatalf("Ann accept: %v", err)
		}
		_, annAt := w.claim(held)
		heldS := w.scheduleOn(held, sam, w.date(0), true)
		ageS := w.scheduleOn(adult, kid, w.date(0), true)
		w.rollover(time.Now())
		if holder, at := w.claim(held); !isHeldBy(holder, ann) || !at.Time.Equal(annAt.Time) {
			t.Fatalf("held quest after the pass: %v at %v, want Ann's claim unchanged", holder, at.Time)
		}
		if !w.hasSchedule(heldS.ID) || !w.hasSchedule(ageS.ID) {
			t.Fatalf("skipped repeated schedules removed: %v", w.scheduleIDs())
		}
		// Ann's hold ends at midnight; the next pass gives it to Sam.
		w.rollover(w.localMidnight(1).Add(time.Minute))
		if holder, _ := w.claim(held); !isHeldBy(holder, sam) {
			t.Fatalf("day 1: held quest %v, want Sam once free", holder)
		}
		for day := 1; day <= 2; day++ {
			if day == 2 {
				w.rollover(w.localMidnight(2).Add(time.Minute))
			}
			if holder, _ := w.claim(adult); holder != nil {
				t.Fatalf("day %d: out-of-age member assigned: %v", day, holder)
			}
			if !w.hasSchedule(ageS.ID) || !w.hasSchedule(heldS.ID) {
				t.Fatalf("day %d: repeated schedules: %v, want both kept", day, w.scheduleIDs())
			}
		}
	})
}
