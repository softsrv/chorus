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

// redeemWorld is one household with an admin, its family and reward
// services, and a direct pool for reading what landed.
type redeemWorld struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	family  *app.FamilyService
	rewards *app.RewardService
	admin   uuid.UUID
	h       app.Household
}

func newRedeemWorld(t *testing.T, prefix string) *redeemWorld {
	t.Helper()
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	ctx := context.Background()
	admin := registerVerified(t, pool, authSvc, mailer, prefix).User.ID
	h, err := app.NewHouseholdService(q, pool).Create(ctx, admin, app.HouseholdInput{Name: "Redeem Home"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}
	return &redeemWorld{t: t, ctx: ctx, pool: pool, family: app.NewFamilyService(q, pool, 10),
		rewards: app.NewRewardService(q, pool), admin: admin, h: h}
}

// member creates a member with PIN 1234 holding points.
func (w *redeemWorld) member(name string, points int64) app.Member {
	w.t.Helper()
	m, err := w.family.Create(w.ctx, w.admin, w.h.ID, app.FamilyMemberInput{Name: name, PIN: "1234"}, "")
	if err != nil {
		w.t.Fatalf("Create member %s: %v", name, err)
	}
	if points > 0 {
		if m, err = w.family.AdjustPoints(w.ctx, w.admin, w.h.ID, m.ID, app.AdjustPointsInput{Points: num(points)}, ""); err != nil {
			w.t.Fatalf("AdjustPoints %s: %v", name, err)
		}
	}
	return m
}

func (w *redeemWorld) reward(desc string, cost int64) db.HouseholdReward {
	w.t.Helper()
	r, err := w.rewards.Create(w.ctx, w.admin, w.h.ID, app.RewardInput{Description: desc, Cost: num(cost)}, "")
	if err != nil {
		w.t.Fatalf("Create reward %s: %v", desc, err)
	}
	return r
}

func (w *redeemWorld) request(m app.Member, r db.HouseholdReward) db.RewardRedemption {
	w.t.Helper()
	red, created, err := w.rewards.RequestRedemption(w.ctx, w.admin, w.h.ID, r.ID, app.MemberAction{MemberID: m.ID, PIN: "1234"})
	if err != nil || !created {
		w.t.Fatalf("RequestRedemption(%s, %s): created %v, %v", m.Name, r.Description, created, err)
	}
	return red
}

// state reads the member's total, their 'reward' points awards (count and
// sum, and the reasons) and their reward_redemptions rows straight from the
// database.
func (w *redeemWorld) state(m app.Member) (total int32, rewardRows int, rewardSum int64, reasons string, requests int) {
	w.t.Helper()
	if err := w.pool.QueryRow(w.ctx, `SELECT points FROM family_members WHERE household_id = $1 AND id = $2`, w.h.ID, m.ID).Scan(&total); err != nil {
		w.t.Fatalf("read total: %v", err)
	}
	if err := w.pool.QueryRow(w.ctx, `SELECT count(*), COALESCE(sum(points), 0), COALESCE(string_agg(reason, '|'), '')
		FROM points_awards WHERE household_id = $1 AND member_id = $2 AND kind = 'reward'`, w.h.ID, m.ID).Scan(&rewardRows, &rewardSum, &reasons); err != nil {
		w.t.Fatalf("read awards: %v", err)
	}
	if err := w.pool.QueryRow(w.ctx, `SELECT count(*) FROM reward_redemptions WHERE household_id = $1 AND member_id = $2`, w.h.ID, m.ID).Scan(&requests); err != nil {
		w.t.Fatalf("read requests: %v", err)
	}
	return
}

func (w *redeemWorld) status(id uuid.UUID) string {
	w.t.Helper()
	var s string
	if err := w.pool.QueryRow(w.ctx, `SELECT status FROM reward_redemptions WHERE household_id = $1 AND id = $2`, w.h.ID, id).Scan(&s); err != nil {
		w.t.Fatalf("read status: %v", err)
	}
	return s
}

// TestIntegration_RewardRedemption covers the redemption flow at the service
// against Postgres: request (points untouched, snapshot, one pending per
// reward), the short request, the member and admin pending lists, approve
// (one debit of the snapshot, a 'Redeemed: <description>' history row, also
// under concurrent approvals), the short approval, reject, decided requests,
// member removal (alone and racing an approval) and the snapshot surviving an
// edited and deleted reward.
func TestIntegration_RewardRedemption(t *testing.T) {
	w := newRedeemWorld(t, "redeem")

	t.Run("request leaves points alone and a repeat writes no second row", func(t *testing.T) {
		sam := w.member("Sam", 120)
		movie := w.reward("Movie night", 100)
		red := w.request(sam, movie)
		if red.Status != "pending" || red.Description != "Movie night" || red.Cost != 100 || red.RewardID == nil || *red.RewardID != movie.ID || red.DecidedAt.Valid {
			t.Errorf("request = %+v, want pending Movie night at 100", red)
		}
		again, created, err := w.rewards.RequestRedemption(w.ctx, w.admin, w.h.ID, movie.ID, app.MemberAction{MemberID: sam.ID, PIN: "1234"})
		if err != nil || created || again.ID != red.ID {
			t.Errorf("repeat request: id %v created %v err %v, want the existing %v", again.ID, created, err, red.ID)
		}
		if total, rows, _, _, requests := w.state(sam); total != 120 || rows != 0 || requests != 1 {
			t.Errorf("after request: total %d, reward awards %d, requests %d; want 120, 0, 1", total, rows, requests)
		}

		mine, err := w.rewards.ListMemberPending(w.ctx, w.admin, w.h.ID, sam.ID, "1234", "")
		if err != nil || len(mine) != 1 || mine[0].ID != red.ID {
			t.Errorf("ListMemberPending with member PIN = %v, %v; want the request", mine, err)
		}
		if _, err := w.rewards.ListMemberPending(w.ctx, w.admin, w.h.ID, uuid.New(), "1234", ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("ListMemberPending unknown member: %v, want ErrNotFound", err)
		}
		pending, err := w.rewards.ListPending(w.ctx, w.admin, w.h.ID, "")
		if err != nil {
			t.Fatalf("ListPending: %v", err)
		}
		found := false
		for _, p := range pending {
			if p.ID == red.ID {
				found = p.MemberName == "Sam" && p.Description == "Movie night" && p.Cost == 100
			}
		}
		if !found {
			t.Errorf("ListPending = %+v, want the request with Sam, Movie night, 100", pending)
		}
	})

	t.Run("a short member's request is refused and nothing is written", func(t *testing.T) {
		ana := w.member("Ana", 120)
		pizza := w.reward("Pizza night", 150)
		_, _, err := w.rewards.RequestRedemption(w.ctx, w.admin, w.h.ID, pizza.ID, app.MemberAction{MemberID: ana.ID, PIN: "1234"})
		var vErr *app.ValidationError
		if !errors.As(err, &vErr) || vErr.Fields["points"] == "" {
			t.Fatalf("short request: %v, want ValidationError on points", err)
		}
		if _, _, _, _, requests := w.state(ana); requests != 0 {
			t.Errorf("short request wrote %d rows", requests)
		}
		if _, _, err := w.rewards.RequestRedemption(w.ctx, w.admin, w.h.ID, pizza.ID, app.MemberAction{MemberID: ana.ID, PIN: "9999"}); !errors.Is(err, app.ErrWrongPIN) {
			t.Errorf("wrong member PIN: %v, want ErrWrongPIN", err)
		}
		if _, _, err := w.rewards.RequestRedemption(w.ctx, w.admin, w.h.ID, uuid.New(), app.MemberAction{MemberID: ana.ID, PIN: "1234"}); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("unknown reward: %v, want ErrNotFound", err)
		}
	})

	t.Run("approve debits the snapshot once; decided requests are not found", func(t *testing.T) {
		ben := w.member("Ben", 120)
		movie := w.reward("Movie night", 100)
		red := w.request(ben, movie)
		approved, member, err := w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, "")
		if err != nil {
			t.Fatalf("Approve: %v", err)
		}
		if approved.Status != "approved" || !approved.DecidedAt.Valid || member.Points != 20 {
			t.Errorf("Approve = %+v, member points %d; want approved with decided_at, 20", approved, member.Points)
		}
		if total, rows, sum, reasons, _ := w.state(ben); total != 20 || rows != 1 || sum != -100 || reasons != "Redeemed: Movie night" {
			t.Errorf("after approve: total %d, reward awards %d sum %d %q; want 20, 1, -100, Redeemed: Movie night", total, rows, sum, reasons)
		}
		if _, _, err := w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("second approve: %v, want ErrNotFound", err)
		}
		if _, err := w.rewards.Reject(w.ctx, w.admin, w.h.ID, red.ID, ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("reject after approve: %v, want ErrNotFound", err)
		}
		if total, rows, _, _, _ := w.state(ben); total != 20 || rows != 1 {
			t.Errorf("after refused repeats: total %d, reward awards %d; want 20, 1", total, rows)
		}
	})

	t.Run("concurrent approvals debit once", func(t *testing.T) {
		cara := w.member("Cara", 120)
		movie := w.reward("Movie night", 100)
		red := w.request(cara, movie)
		const n = 4
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, "")
			}()
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, app.ErrNotFound):
				t.Errorf("concurrent approve: unexpected %v", err)
			}
		}
		if ok != 1 {
			t.Errorf("%d approvals succeeded, want 1", ok)
		}
		if total, rows, sum, _, _ := w.state(cara); total != 20 || rows != 1 || sum != -100 {
			t.Errorf("after concurrent approvals: total %d, reward awards %d sum %d; want 20, 1, -100", total, rows, sum)
		}
	})

	t.Run("approval refused when the member is now short; reject still works", func(t *testing.T) {
		dee := w.member("Dee", 120)
		movie := w.reward("Movie night", 100)
		red := w.request(dee, movie)
		if _, err := w.family.AdjustPoints(w.ctx, w.admin, w.h.ID, dee.ID, app.AdjustPointsInput{Points: num(-50)}, ""); err != nil {
			t.Fatalf("AdjustPoints: %v", err)
		}
		_, _, err := w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, "")
		var vErr *app.ValidationError
		if !errors.As(err, &vErr) {
			t.Fatalf("short approve: %v, want ValidationError", err)
		}
		msg := vErr.Fields["points"]
		for _, want := range []string{"Dee", "70", "100", "Movie night"} {
			if !strings.Contains(msg, want) {
				t.Errorf("short approve message %q does not name %q", msg, want)
			}
		}
		if total, rows, _, _, _ := w.state(dee); total != 70 || rows != 0 {
			t.Errorf("after short approve: total %d, reward awards %d; want 70, 0", total, rows)
		}
		if s := w.status(red.ID); s != "pending" {
			t.Errorf("status after short approve = %q, want pending", s)
		}
		rejected, err := w.rewards.Reject(w.ctx, w.admin, w.h.ID, red.ID, "")
		if err != nil || rejected.Status != "rejected" || !rejected.DecidedAt.Valid {
			t.Fatalf("Reject: %+v, %v; want rejected with decided_at", rejected, err)
		}
		if total, rows, _, _, _ := w.state(dee); total != 70 || rows != 0 {
			t.Errorf("after reject: total %d, reward awards %d; want 70, 0", total, rows)
		}
		if _, _, err := w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("approve after reject: %v, want ErrNotFound", err)
		}
	})

	t.Run("removing the member cancels the request", func(t *testing.T) {
		eli := w.member("Eli", 120)
		movie := w.reward("Movie night", 100)
		red := w.request(eli, movie)
		// The member delete cascades to the request, so record each status
		// change inside Remove's transaction to see the cancel itself.
		for _, stmt := range []string{
			`CREATE TABLE IF NOT EXISTS test_redemption_status_log (id UUID, status TEXT)`,
			`CREATE OR REPLACE FUNCTION test_log_redemption_status() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN INSERT INTO test_redemption_status_log VALUES (NEW.id, NEW.status); RETURN NEW; END $$`,
			`CREATE OR REPLACE TRIGGER test_redemption_status_log AFTER UPDATE ON reward_redemptions
			 FOR EACH ROW EXECUTE FUNCTION test_log_redemption_status()`,
		} {
			if _, err := w.pool.Exec(w.ctx, stmt); err != nil {
				t.Fatalf("install status log: %v", err)
			}
		}
		t.Cleanup(func() {
			for _, stmt := range []string{
				`DROP TRIGGER IF EXISTS test_redemption_status_log ON reward_redemptions`,
				`DROP FUNCTION IF EXISTS test_log_redemption_status()`,
				`DROP TABLE IF EXISTS test_redemption_status_log`,
			} {
				if _, err := w.pool.Exec(context.Background(), stmt); err != nil {
					t.Errorf("drop status log: %v", err)
				}
			}
		})
		if err := w.family.Remove(w.ctx, w.admin, w.h.ID, eli.ID, ""); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		var logged string
		if err := w.pool.QueryRow(w.ctx, `SELECT COALESCE(string_agg(status, ','), '') FROM test_redemption_status_log WHERE id = $1`, red.ID).Scan(&logged); err != nil {
			t.Fatalf("read status log: %v", err)
		}
		if logged != "cancelled" {
			t.Errorf("Remove moved the request through %q, want cancelled", logged)
		}
		if _, _, err := w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("approve after removal: %v, want ErrNotFound", err)
		}
		if _, err := w.rewards.Reject(w.ctx, w.admin, w.h.ID, red.ID, ""); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("reject after removal: %v, want ErrNotFound", err)
		}
	})

	t.Run("removal racing an approval never deadlocks or debits a removed member", func(t *testing.T) {
		for i := range 5 {
			m := w.member("Fay", 120)
			red := w.request(m, w.reward("Movie night", 100))
			var approveErr, removeErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _, approveErr = w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, "") }()
			go func() { defer wg.Done(); removeErr = w.family.Remove(w.ctx, w.admin, w.h.ID, m.ID, "") }()
			wg.Wait()
			if removeErr != nil {
				t.Errorf("round %d: Remove: %v", i, removeErr)
			}
			if approveErr != nil && !errors.Is(approveErr, app.ErrNotFound) {
				t.Errorf("round %d: Approve: %v, want success or ErrNotFound", i, approveErr)
			}
		}
	})

	t.Run("approval charges the snapshot after the reward is edited and deleted", func(t *testing.T) {
		gus := w.member("Gus", 120)
		movie := w.reward("Movie night", 100)
		red := w.request(gus, movie)
		if _, err := w.rewards.Update(w.ctx, w.admin, w.h.ID, movie.ID, app.RewardInput{Description: "Cinema trip", Cost: num(500)}, ""); err != nil {
			t.Fatalf("Update reward: %v", err)
		}
		if err := w.rewards.Delete(w.ctx, w.admin, w.h.ID, movie.ID, ""); err != nil {
			t.Fatalf("Delete reward: %v", err)
		}
		approved, member, err := w.rewards.Approve(w.ctx, w.admin, w.h.ID, red.ID, "")
		if err != nil {
			t.Fatalf("Approve after delete: %v", err)
		}
		if approved.RewardID != nil || approved.Cost != 100 || member.Points != 20 {
			t.Errorf("Approve after delete: %+v points %d; want reward_id nil, cost 100, 20 points", approved, member.Points)
		}
		if _, _, sum, reasons, _ := w.state(gus); sum != -100 || reasons != "Redeemed: Movie night" {
			t.Errorf("history after snapshot approve: %d %q; want -100 Redeemed: Movie night", sum, reasons)
		}
	})
}

// TestIntegration_RewardHistory covers RewardHistory at the service against
// Postgres: requests approved through Approve come back with the snapshotted
// description and cost and their decided_at, newest approval first (ties by
// id DESC); limit and offset page through them and are clamped; a member
// with none gets an empty, non-nil slice.
func TestIntegration_RewardHistory(t *testing.T) {
	w := newRedeemWorld(t, "rewardhistory")
	hal := w.member("Hal", 500)
	movie := w.reward("Movie night", 100)
	pizza := w.reward("Pizza night", 150)
	game := w.reward("Game time", 20)

	approve := func(r db.HouseholdReward, decidedAt time.Time) db.RewardRedemption {
		t.Helper()
		approved, _, err := w.rewards.Approve(w.ctx, w.admin, w.h.ID, w.request(hal, r).ID, "")
		if err != nil {
			t.Fatalf("Approve %s: %v", r.Description, err)
		}
		// Pin the approval time so the order is decided by the data, not by
		// how fast the approvals ran.
		if _, err := w.pool.Exec(w.ctx, `UPDATE reward_redemptions SET decided_at = $1 WHERE id = $2`, decidedAt, approved.ID); err != nil {
			t.Fatalf("pin decided_at: %v", err)
		}
		return approved
	}
	movieRed := approve(movie, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC))
	pizzaRed := approve(pizza, time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC))
	// The same approval time as pizza: id DESC puts the later request first.
	gameRed := approve(game, time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC))

	history, err := w.rewards.RewardHistory(w.ctx, w.admin, w.h.ID, hal.ID, "1234", "", 50, 0)
	if err != nil {
		t.Fatalf("RewardHistory: %v", err)
	}
	type item struct {
		id   uuid.UUID
		desc string
		cost int32
		at   string
	}
	got := make([]item, 0, len(history))
	for _, r := range history {
		if r.Status != "approved" || !r.DecidedAt.Valid || r.MemberID != hal.ID || r.HouseholdID != w.h.ID {
			t.Errorf("history row %+v: want an approved row of Hal with decided_at", r)
		}
		got = append(got, item{r.ID, r.Description, r.Cost, r.DecidedAt.Time.UTC().Format("2006-01-02T15:04:05Z")})
	}
	want := []item{
		{gameRed.ID, "Game time", 20, "2026-11-18T10:00:00Z"},
		{pizzaRed.ID, "Pizza night", 150, "2026-11-18T10:00:00Z"},
		{movieRed.ID, "Movie night", 100, "2026-11-16T10:00:00Z"},
	}
	if len(got) != len(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("history[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The admin reads it too (no admin PIN set, so no header is needed).
	if adminView, err := w.rewards.RewardHistory(w.ctx, w.admin, w.h.ID, hal.ID, "", "", 50, 0); err != nil || len(adminView) != 3 {
		t.Errorf("admin RewardHistory = %d rows, %v; want 3", len(adminView), err)
	}

	page := func(limit, offset int32) []uuid.UUID {
		t.Helper()
		rows, err := w.rewards.RewardHistory(w.ctx, w.admin, w.h.ID, hal.ID, "1234", "", limit, offset)
		if err != nil {
			t.Fatalf("RewardHistory(limit %d, offset %d): %v", limit, offset, err)
		}
		ids := make([]uuid.UUID, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if ids := page(1, 1); len(ids) != 1 || ids[0] != pizzaRed.ID {
		t.Errorf("limit 1 offset 1 = %v, want [%v]", ids, pizzaRed.ID)
	}
	if ids := page(2, 2); len(ids) != 1 || ids[0] != movieRed.ID {
		t.Errorf("limit 2 offset 2 = %v, want [%v]", ids, movieRed.ID)
	}
	if ids := page(0, -5); len(ids) != 3 || ids[0] != gameRed.ID {
		t.Errorf("limit 0 offset -5 (clamped to 50, 0) = %v, want all three from %v", ids, gameRed.ID)
	}
	if ids := page(1000, 0); len(ids) != 3 {
		t.Errorf("limit 1000 (clamped to 100) = %v, want all three", ids)
	}
	if ids := page(50, 3); ids == nil || len(ids) != 0 {
		t.Errorf("offset past the end = %#v, want empty", ids)
	}

	// A member with nothing approved: an empty, non-nil slice.
	ivy := w.member("Ivy", 0)
	none, err := w.rewards.RewardHistory(w.ctx, w.admin, w.h.ID, ivy.ID, "1234", "", 50, 0)
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("RewardHistory for a member with none = %#v, %v; want an empty, non-nil slice", none, err)
	}
}
