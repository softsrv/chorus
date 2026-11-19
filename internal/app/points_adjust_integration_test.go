//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// adjustWorld is one household with an admin, the family service and a
// direct pool for reading what landed.
type adjustWorld struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	family *app.FamilyService
	admin  uuid.UUID
	h      app.Household
}

func newAdjustWorld(t *testing.T, prefix string) *adjustWorld {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	ctx := context.Background()
	admin := registerVerified(t, pool, authSvc, mailer, prefix).User.ID
	h, err := app.NewHouseholdService(q, pool).Create(ctx, admin, app.HouseholdInput{Name: "Adjust Home"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}
	return &adjustWorld{t: t, ctx: ctx, pool: pool, family: app.NewFamilyService(q, pool, 4), admin: admin, h: h}
}

func (w *adjustWorld) member(name string) app.Member {
	w.t.Helper()
	m, err := w.family.Create(w.ctx, w.admin, w.h.ID, app.FamilyMemberInput{Name: name}, "")
	if err != nil {
		w.t.Fatalf("Create member %s: %v", name, err)
	}
	return m
}

func (w *adjustWorld) adjust(m app.Member, points int64, reason, adminPin string) (app.Member, error) {
	return w.family.AdjustPoints(w.ctx, w.admin, w.h.ID, m.ID, app.AdjustPointsInput{Points: num(points), Reason: reason}, adminPin)
}

func (w *adjustWorld) mustAdjust(m app.Member, points int64, reason string) app.Member {
	w.t.Helper()
	got, err := w.adjust(m, points, reason, "")
	if err != nil {
		w.t.Fatalf("AdjustPoints(%s, %d, %q): %v", m.Name, points, reason, err)
	}
	return got
}

// state reads the member's stored total and every points_awards row of theirs
// straight from the database.
func (w *adjustWorld) state(m app.Member) (total int32, rows int, sum int64) {
	w.t.Helper()
	if err := w.pool.QueryRow(w.ctx, `SELECT points FROM family_members WHERE household_id = $1 AND id = $2`, w.h.ID, m.ID).Scan(&total); err != nil {
		w.t.Fatalf("read total: %v", err)
	}
	if err := w.pool.QueryRow(w.ctx, `SELECT count(*), COALESCE(sum(points), 0) FROM points_awards WHERE household_id = $1 AND member_id = $2`, w.h.ID, m.ID).Scan(&rows, &sum); err != nil {
		w.t.Fatalf("read awards: %v", err)
	}
	return total, rows, sum
}

func (w *adjustWorld) history(m app.Member) []app.PointsAward {
	w.t.Helper()
	awards, err := w.family.PointsHistory(w.ctx, w.admin, w.h.ID, m.ID, "", "", 100, 0)
	if err != nil {
		w.t.Fatalf("PointsHistory: %v", err)
	}
	return awards
}

// TestIntegration_AdjustPoints covers the admin points adjustment end to end
// against Postgres: credit, debit, blank reason, the below-zero refusal, field
// validation, the admin PIN, rollback on a failed history insert, and
// concurrent adjustments.
func TestIntegration_AdjustPoints(t *testing.T) {
	t.Run("credit, debit and blank reason land with their history", func(t *testing.T) {
		w := newAdjustWorld(t, "adjcredit")
		maya := w.member("Maya")

		got := w.mustAdjust(maya, 10, "  helped grandma  ")
		if got.Points != 10 || got.ID != maya.ID {
			t.Fatalf("after +10: got %+v, want points 10", got)
		}
		hist := w.history(maya)
		if len(hist) != 1 || hist[0].Kind != "admin" || hist[0].Points != 10 || hist[0].Reason != "helped grandma" {
			t.Fatalf("history after +10: %+v", hist)
		}

		got = w.mustAdjust(maya, -5, "left toys out")
		if got.Points != 5 {
			t.Fatalf("after -5: points %d, want 5", got.Points)
		}
		got = w.mustAdjust(maya, 2, "   ")
		if got.Points != 7 {
			t.Fatalf("after +2: points %d, want 7", got.Points)
		}

		hist = w.history(maya)
		if len(hist) != 3 {
			t.Fatalf("history rows: %d, want 3: %+v", len(hist), hist)
		}
		want := []struct {
			points int32
			reason string
		}{{2, "Adjusted by admin"}, {-5, "left toys out"}, {10, "helped grandma"}}
		for i, wnt := range want {
			if hist[i].Kind != "admin" || hist[i].Points != wnt.points || hist[i].Reason != wnt.reason {
				t.Errorf("history[%d] = %+v, want admin %d %q", i, hist[i], wnt.points, wnt.reason)
			}
		}

		total, rows, sum := w.state(maya)
		if total != 7 || rows != 3 || sum != int64(total) {
			t.Fatalf("stored total %d, rows %d, sum %d; want 7, 3, sum == total", total, rows, sum)
		}
	})

	t.Run("removal below zero is refused and changes nothing; removing exactly the total succeeds", func(t *testing.T) {
		w := newAdjustWorld(t, "adjfloor")
		leo := w.member("Leo")
		w.mustAdjust(leo, 3, "Quest: Make bed")

		_, err := w.adjust(leo, -5, "", "")
		var vErr *app.ValidationError
		if !errors.As(err, &vErr) {
			t.Fatalf("remove 5 of 3: got %v, want ValidationError", err)
		}
		if msg := vErr.Fields["points"]; msg != "Leo only has 3 points — you can't remove 5." {
			t.Fatalf("points message = %q", msg)
		}
		if total, rows, _ := w.state(leo); total != 3 || rows != 1 {
			t.Fatalf("after refused removal: total %d rows %d, want 3 and 1", total, rows)
		}

		got := w.mustAdjust(leo, -3, "spent")
		if got.Points != 0 {
			t.Fatalf("remove exactly 3: points %d, want 0", got.Points)
		}
		if total, rows, sum := w.state(leo); total != 0 || rows != 2 || sum != 0 {
			t.Fatalf("after removing all: total %d rows %d sum %d", total, rows, sum)
		}
	})

	t.Run("invalid points or reason are field errors and change nothing", func(t *testing.T) {
		w := newAdjustWorld(t, "adjvalid")
		sam := w.member("Sam")
		w.mustAdjust(sam, 4, "start")

		cases := []struct {
			name  string
			in    app.AdjustPointsInput
			field string
		}{
			{"absent", app.AdjustPointsInput{}, "points"},
			{"zero", app.AdjustPointsInput{Points: num(0)}, "points"},
			{"not whole", app.AdjustPointsInput{Points: app.NumberField{Set: true, Invalid: true}}, "points"},
			{"too large", app.AdjustPointsInput{Points: num(1_000_001)}, "points"},
			{"too small", app.AdjustPointsInput{Points: num(-1_000_001)}, "points"},
			{"int32 overflow", app.AdjustPointsInput{Points: num(1 << 40)}, "points"},
			{"reason too long", app.AdjustPointsInput{Points: num(1), Reason: strings.Repeat("r", 201)}, "reason"},
			{"reason control char", app.AdjustPointsInput{Points: num(1), Reason: "a\nb"}, "reason"},
		}
		for _, c := range cases {
			_, err := w.family.AdjustPoints(w.ctx, w.admin, w.h.ID, sam.ID, c.in, "")
			var vErr *app.ValidationError
			if !errors.As(err, &vErr) || vErr.Fields[c.field] == "" {
				t.Errorf("%s: got %v, want ValidationError on %s", c.name, err, c.field)
			}
		}
		// The longest allowed reason and the largest allowed amount pass.
		w.mustAdjust(sam, 1_000_000, strings.Repeat("r", 200))
		w.mustAdjust(sam, -1_000_000, "")
		if total, rows, sum := w.state(sam); total != 4 || rows != 3 || sum != 4 {
			t.Fatalf("after invalid inputs: total %d rows %d sum %d; want 4, 3, 4", total, rows, sum)
		}
	})

	t.Run("admin PIN, outsiders and foreign members are refused and change nothing", func(t *testing.T) {
		w := newAdjustWorld(t, "adjauth")
		ana := w.member("Ana")
		if err := w.family.SetAdminPin(w.ctx, w.admin, w.h.ID, "4321", ""); err != nil {
			t.Fatalf("SetAdminPin: %v", err)
		}
		if _, err := w.adjust(ana, 5, "", ""); !errors.Is(err, app.ErrAdminPinRequired) {
			t.Errorf("missing admin PIN: got %v, want ErrAdminPinRequired", err)
		}
		if _, err := w.adjust(ana, 5, "", "0000"); !errors.Is(err, app.ErrWrongPIN) {
			t.Errorf("wrong admin PIN: got %v, want ErrWrongPIN", err)
		}

		other := newAdjustWorld(t, "adjauthother")
		otherMember := other.member("Zed")
		// Outsider: the other household's admin names this household.
		if _, err := other.family.AdjustPoints(w.ctx, other.admin, w.h.ID, ana.ID, app.AdjustPointsInput{Points: num(5)}, "4321"); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("outsider caller: got %v, want ErrNotFound", err)
		}
		// Foreign member smuggled under this household's ID.
		if _, err := w.family.AdjustPoints(w.ctx, w.admin, w.h.ID, otherMember.ID, app.AdjustPointsInput{Points: num(5)}, "4321"); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("foreign member: got %v, want ErrNotFound", err)
		}
		if _, err := w.family.AdjustPoints(w.ctx, w.admin, w.h.ID, uuid.New(), app.AdjustPointsInput{Points: num(5)}, "4321"); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("nonexistent member: got %v, want ErrNotFound", err)
		}
		if total, rows, _ := w.state(ana); total != 0 || rows != 0 {
			t.Fatalf("refused adjustments changed Ana: total %d rows %d", total, rows)
		}
		if total, rows, _ := other.state(otherMember); total != 0 || rows != 0 {
			t.Fatalf("refused adjustment changed Zed: total %d rows %d", total, rows)
		}

		got, err := w.adjust(ana, 5, "with PIN", "4321")
		if err != nil || got.Points != 5 {
			t.Fatalf("right admin PIN: got %+v, %v", got, err)
		}
	})

	t.Run("a failed history insert rolls back the total", func(t *testing.T) {
		w := newAdjustWorld(t, "adjfault")
		kim := w.member("Kim")
		w.mustAdjust(kim, 6, "start")

		suffix := strings.ReplaceAll(w.h.ID.String(), "-", "")
		fn, trg := "test_fail_admin_award_"+suffix, "test_fail_admin_award_trg_"+suffix
		if _, err := w.pool.Exec(w.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.household_id = '%s' AND NEW.kind = 'admin' THEN RAISE EXCEPTION 'injected admin award failure'; END IF;
  RETURN NEW;
END $$`, fn, w.h.ID)); err != nil {
			t.Fatalf("create trigger function: %v", err)
		}
		dropped := false
		drop := func() {
			if dropped {
				return
			}
			dropped = true
			if _, err := w.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON points_awards; DROP FUNCTION IF EXISTS %s()`, trg, fn)); err != nil {
				t.Errorf("drop trigger: %v", err)
			}
		}
		t.Cleanup(drop)
		if _, err := w.pool.Exec(w.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON points_awards FOR EACH ROW EXECUTE FUNCTION %s()`, trg, fn)); err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		if _, err := w.adjust(kim, 4, "boom", ""); err == nil {
			t.Fatal("adjust with failing history insert succeeded")
		}
		if total, rows, _ := w.state(kim); total != 6 || rows != 1 {
			t.Fatalf("failed adjust left total %d rows %d; want 6 and 1", total, rows)
		}

		drop()
		if got := w.mustAdjust(kim, 4, "retry"); got.Points != 10 {
			t.Fatalf("retry after fault: points %d, want 10", got.Points)
		}
	})

	t.Run("concurrent adjustments to one member all land", func(t *testing.T) {
		w := newAdjustWorld(t, "adjconc")
		ben := w.member("Ben")
		w.mustAdjust(ben, 20, "start")

		// Removals sized so every ordering stays at or above 0.
		deltas := []int64{7, -2, 5, -3, 11, -1, 4, 9}
		var want int64 = 20
		for _, d := range deltas {
			want += d
		}
		start := make(chan struct{})
		errs := make([]error, len(deltas))
		var wg sync.WaitGroup
		for i, d := range deltas {
			wg.Add(1)
			go func(i int, d int64) {
				defer wg.Done()
				<-start
				_, errs[i] = w.adjust(ben, d, fmt.Sprintf("concurrent %d", i), "")
			}(i, d)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("adjustment %d (%+d): %v", i, deltas[i], err)
			}
		}
		total, rows, sum := w.state(ben)
		if int64(total) != want || rows != len(deltas)+1 || sum != want {
			t.Fatalf("after concurrent adjustments: total %d rows %d sum %d; want %d, %d, %d", total, rows, sum, want, len(deltas)+1, want)
		}
	})
}
