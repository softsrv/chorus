//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

// redemptionJSON is the wire shape of a reward redemption.
type redemptionJSON struct {
	ID          uuid.UUID  `json:"id"`
	MemberID    uuid.UUID  `json:"member_id"`
	RewardID    *uuid.UUID `json:"reward_id"`
	Description string     `json:"description"`
	Cost        int32      `json:"cost"`
	Status      string     `json:"status"`
	DecidedAt   *string    `json:"decided_at"`
	MemberName  string     `json:"member_name"`
}

// TestAPI_RewardRedemption drives redemption through the router against
// Postgres: redeem (201, points untouched, a repeat answers the same request),
// the short request (422), the member and admin pending lists, approve (the
// member at the new total), reject, decided requests (404), another
// household's request (404), and the admin PIN (403 admin_pin_required and
// wrong_pin, nothing changed).
func TestAPI_RewardRedemption(t *testing.T) {
	s := newTestServer(t)
	admin := s.signUp("redeemadmin")
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", admin.AccessToken,
		map[string]string{"name": "Redeem Home", "timezone": "UTC"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	type memberJSON struct {
		ID     uuid.UUID `json:"id"`
		Points int32     `json:"points"`
	}
	var sam memberJSON
	s.expect(http.StatusCreated, "POST", hp+"/family-members", admin.AccessToken,
		map[string]string{"name": "Sam", "pin": "1234"}, &sam)
	s.expect(http.StatusOK, "POST", hp+"/family-members/"+sam.ID.String()+"/points-adjustments", admin.AccessToken,
		map[string]any{"points": 120}, nil)
	points := func() int32 {
		t.Helper()
		var p int32
		if err := s.pool.QueryRow(t.Context(), "SELECT points FROM family_members WHERE id = $1", sam.ID).Scan(&p); err != nil {
			t.Fatalf("read points: %v", err)
		}
		return p
	}
	var movie, pizza, game struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", hp+"/rewards", admin.AccessToken, map[string]any{"description": "Movie night", "cost": 100}, &movie)
	s.expect(http.StatusCreated, "POST", hp+"/rewards", admin.AccessToken, map[string]any{"description": "Pizza night", "cost": 150}, &pizza)
	s.expect(http.StatusCreated, "POST", hp+"/rewards", admin.AccessToken, map[string]any{"description": "Game time", "cost": 20}, &game)
	redeem := func(rewardID uuid.UUID) string { return hp + "/rewards/" + rewardID.String() + "/redeem" }
	samBody := map[string]any{"member_id": sam.ID, "pin": "1234"}

	// Nothing pending: the admin list is {items: []}.
	var empty struct{ Items []redemptionJSON }
	s.expect(http.StatusOK, "GET", hp+"/reward-redemptions/pending", admin.AccessToken, nil, &empty)
	if empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty pending list = %#v, want []", empty.Items)
	}

	// Redeem: 201 pending, points unchanged; a repeat answers the same request.
	var red redemptionJSON
	s.expect(http.StatusCreated, "POST", redeem(movie.ID), admin.AccessToken, samBody, &red)
	if red.Status != "pending" || red.Description != "Movie night" || red.Cost != 100 || red.MemberID != sam.ID || red.DecidedAt != nil {
		t.Errorf("redeem = %+v", red)
	}
	if p := points(); p != 120 {
		t.Errorf("points after redeem = %d, want 120", p)
	}
	var again redemptionJSON
	s.expect(http.StatusOK, "POST", redeem(movie.ID), admin.AccessToken, samBody, &again)
	var rows int
	if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM reward_redemptions WHERE member_id = $1", sam.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if again.ID != red.ID || rows != 1 {
		t.Errorf("repeat redeem: id %v rows %d, want %v and 1", again.ID, rows, red.ID)
	}

	// Short of points: 422 on points, nothing written.
	var short apiError
	s.expect(http.StatusUnprocessableEntity, "POST", redeem(pizza.ID), admin.AccessToken, samBody, &short)
	if short.Error.Fields["points"] == "" {
		t.Errorf("short redeem: %+v, want a points field error", short)
	}
	var wrong apiError
	s.expect(http.StatusForbidden, "POST", redeem(game.ID), admin.AccessToken, map[string]any{"member_id": sam.ID, "pin": "9999"}, &wrong)
	if wrong.Error.Code != "wrong_pin" {
		t.Errorf("wrong member PIN code = %q", wrong.Error.Code)
	}

	// The member's pending list, by member PIN.
	var mine struct{ Items []redemptionJSON }
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/family-members/"+sam.ID.String()+"/reward-redemptions/pending",
		admin.AccessToken, map[string]string{"X-Member-PIN": "1234"}, nil, &mine)
	if len(mine.Items) != 1 || mine.Items[0].ID != red.ID {
		t.Errorf("member pending = %+v, want the Movie night request", mine.Items)
	}

	// The admin list carries member name, description and cost.
	var pending struct{ Items []redemptionJSON }
	s.expect(http.StatusOK, "GET", hp+"/reward-redemptions/pending", admin.AccessToken, nil, &pending)
	if len(pending.Items) != 1 || pending.Items[0].MemberName != "Sam" || pending.Items[0].Description != "Movie night" || pending.Items[0].Cost != 100 {
		t.Errorf("admin pending = %+v", pending.Items)
	}

	// Set an admin PIN: missing and wrong PINs are refused and change nothing.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", admin.AccessToken, map[string]string{"pin": "4321"}, nil)
	approvePath := hp + "/reward-redemptions/" + red.ID.String() + "/approve"
	rejectPath := hp + "/reward-redemptions/" + red.ID.String() + "/reject"
	var noPin, badPin apiError
	s.expect(http.StatusForbidden, "POST", approvePath, admin.AccessToken, nil, &noPin)
	s.expectWithHeaders(http.StatusForbidden, "POST", rejectPath, admin.AccessToken, map[string]string{"X-Admin-PIN": "0000"}, nil, &badPin)
	if noPin.Error.Code != "admin_pin_required" || badPin.Error.Code != "wrong_pin" {
		t.Errorf("PIN refusals = %q, %q; want admin_pin_required, wrong_pin", noPin.Error.Code, badPin.Error.Code)
	}
	var status string
	if err := s.pool.QueryRow(t.Context(), "SELECT status FROM reward_redemptions WHERE id = $1", red.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "pending" || points() != 120 {
		t.Errorf("after refused PINs: status %q points %d, want pending, 120", status, points())
	}
	adminPin := map[string]string{"X-Admin-PIN": "4321"}

	// Approve: 200 with the member at the new total; a repeat is 404.
	var approved struct {
		Redemption redemptionJSON `json:"redemption"`
		Member     memberJSON     `json:"member"`
	}
	s.expectWithHeaders(http.StatusOK, "POST", approvePath, admin.AccessToken, adminPin, nil, &approved)
	if approved.Redemption.Status != "approved" || approved.Member.Points != 20 || points() != 20 {
		t.Errorf("approve = %+v, stored points %d; want approved, 20", approved, points())
	}
	s.expectWithHeaders(http.StatusNotFound, "POST", approvePath, admin.AccessToken, adminPin, nil, nil)
	s.expectWithHeaders(http.StatusNotFound, "POST", rejectPath, admin.AccessToken, adminPin, nil, nil)

	// Reject: 200 rejected, no points move; approve after it is 404.
	var gameRed redemptionJSON
	s.expect(http.StatusCreated, "POST", redeem(game.ID), admin.AccessToken, samBody, &gameRed)
	gp := hp + "/reward-redemptions/" + gameRed.ID.String()
	var rejected struct {
		Redemption redemptionJSON `json:"redemption"`
	}
	s.expectWithHeaders(http.StatusOK, "POST", gp+"/reject", admin.AccessToken, adminPin, map[string]any{}, &rejected)
	if rejected.Redemption.Status != "rejected" || points() != 20 {
		t.Errorf("reject = %+v, points %d; want rejected, 20", rejected, points())
	}
	s.expectWithHeaders(http.StatusNotFound, "POST", gp+"/approve", admin.AccessToken, adminPin, nil, nil)

	// Another household's admin cannot see or act on this household's request.
	other := s.signUp("redeemother")
	var oh struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", other.AccessToken,
		map[string]string{"name": "Other Home", "timezone": "UTC"}, &oh)
	var pend redemptionJSON
	s.expect(http.StatusCreated, "POST", redeem(game.ID), admin.AccessToken, samBody, &pend)
	ohp := "/api/v1/households/" + oh.ID.String()
	s.expect(http.StatusNotFound, "POST", ohp+"/reward-redemptions/"+pend.ID.String()+"/approve", other.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "POST", hp+"/reward-redemptions/"+pend.ID.String()+"/approve", other.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", hp+"/reward-redemptions/pending", other.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "POST", redeem(game.ID), other.AccessToken, samBody, nil)
	if points() != 20 {
		t.Errorf("points after outsider attempts = %d, want 20", points())
	}
}

// TestAPI_RewardHistory drives GET .../family-members/{id}/reward-history
// through the router against Postgres: approved requests come back as
// {items: [redemption]} with description, cost and decided_at, newest
// approval first, read with the member's PIN or as the admin; ?limit= and
// ?offset= page and clamp as points-history does, and a non-integer is 422.
func TestAPI_RewardHistory(t *testing.T) {
	s := newTestServer(t)
	admin := s.signUp("historyadmin")
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", admin.AccessToken,
		map[string]string{"name": "History Home", "timezone": "UTC"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	var sam struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", hp+"/family-members", admin.AccessToken,
		map[string]string{"name": "Sam", "pin": "1234"}, &sam)
	s.expect(http.StatusOK, "POST", hp+"/family-members/"+sam.ID.String()+"/points-adjustments", admin.AccessToken,
		map[string]any{"points": 300}, nil)
	samBody := map[string]any{"member_id": sam.ID, "pin": "1234"}

	approveReward := func(desc string, cost int, decidedAt time.Time) redemptionJSON {
		t.Helper()
		var reward struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", hp+"/rewards", admin.AccessToken, map[string]any{"description": desc, "cost": cost}, &reward)
		var red redemptionJSON
		s.expect(http.StatusCreated, "POST", hp+"/rewards/"+reward.ID.String()+"/redeem", admin.AccessToken, samBody, &red)
		s.expect(http.StatusOK, "POST", hp+"/reward-redemptions/"+red.ID.String()+"/approve", admin.AccessToken, nil, nil)
		// Pin the approval time so the order is decided by the data.
		if _, err := s.pool.Exec(t.Context(), "UPDATE reward_redemptions SET decided_at = $1 WHERE id = $2", decidedAt, red.ID); err != nil {
			t.Fatalf("pin decided_at: %v", err)
		}
		return red
	}
	movie := approveReward("Movie night", 100, time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC))
	pizza := approveReward("Pizza night", 150, time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))

	histPath := hp + "/family-members/" + sam.ID.String() + "/reward-history"
	memberPin := map[string]string{"X-Member-PIN": "1234"}
	var hist struct{ Items []redemptionJSON }
	s.expectWithHeaders(http.StatusOK, "GET", histPath, admin.AccessToken, memberPin, nil, &hist)
	if len(hist.Items) != 2 {
		t.Fatalf("history = %+v, want two items", hist.Items)
	}
	wants := []struct {
		id   uuid.UUID
		desc string
		cost int32
		at   string
	}{{pizza.ID, "Pizza night", 150, "2026-11-18T09:00:00Z"}, {movie.ID, "Movie night", 100, "2026-11-17T09:00:00Z"}}
	for i, want := range wants {
		got := hist.Items[i]
		if got.ID != want.id || got.Description != want.desc || got.Cost != want.cost || got.Status != "approved" ||
			got.MemberID != sam.ID || got.DecidedAt == nil || *got.DecidedAt != want.at {
			t.Errorf("history[%d] = %+v (decided_at %v), want %s %d decided %s", i, got, got.DecidedAt, want.desc, want.cost, want.at)
		}
	}

	// The admin reads it without a member PIN (no admin PIN set).
	var adminHist struct{ Items []redemptionJSON }
	s.expect(http.StatusOK, "GET", histPath, admin.AccessToken, nil, &adminHist)
	if len(adminHist.Items) != 2 || adminHist.Items[0].ID != pizza.ID {
		t.Errorf("admin history = %+v, want pizza then movie", adminHist.Items)
	}

	ids := func(query string) []uuid.UUID {
		t.Helper()
		var page struct{ Items []redemptionJSON }
		s.expectWithHeaders(http.StatusOK, "GET", histPath+query, admin.AccessToken, memberPin, nil, &page)
		out := make([]uuid.UUID, 0, len(page.Items))
		for _, it := range page.Items {
			out = append(out, it.ID)
		}
		return out
	}
	if got := ids("?limit=1"); len(got) != 1 || got[0] != pizza.ID {
		t.Errorf("?limit=1 = %v, want [%v]", got, pizza.ID)
	}
	if got := ids("?limit=1&offset=1"); len(got) != 1 || got[0] != movie.ID {
		t.Errorf("?limit=1&offset=1 = %v, want [%v]", got, movie.ID)
	}
	if got := ids("?limit=0"); len(got) != 1 {
		t.Errorf("?limit=0 (clamped to 1) = %v, want one item", got)
	}
	if got := ids("?limit=1000&offset=-3"); len(got) != 2 {
		t.Errorf("?limit=1000&offset=-3 (clamped to 100, 0) = %v, want both", got)
	}
	for _, off := range []string{"2147483648", "4294967296"} {
		if got := ids("?offset=" + off); len(got) != 0 {
			t.Errorf("?offset=%s = %v, want an empty page (no wrap)", off, got)
		}
	}
	s.expectWithHeaders(http.StatusUnprocessableEntity, "GET", histPath+"?limit=abc", admin.AccessToken, memberPin, nil, nil)

	// Only approved requests are history over HTTP too: a pending and a
	// rejected request of Sam stay out until the pending one is approved.
	var game, snack struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", hp+"/rewards", admin.AccessToken, map[string]any{"description": "Game time", "cost": 20}, &game)
	s.expect(http.StatusCreated, "POST", hp+"/rewards", admin.AccessToken, map[string]any{"description": "Snack", "cost": 30}, &snack)
	var waiting, refused redemptionJSON
	s.expect(http.StatusCreated, "POST", hp+"/rewards/"+game.ID.String()+"/redeem", admin.AccessToken, samBody, &waiting)
	s.expect(http.StatusCreated, "POST", hp+"/rewards/"+snack.ID.String()+"/redeem", admin.AccessToken, samBody, &refused)
	s.expect(http.StatusOK, "POST", hp+"/reward-redemptions/"+refused.ID.String()+"/reject", admin.AccessToken, nil, nil)
	if got := ids(""); len(got) != 2 || got[0] != pizza.ID || got[1] != movie.ID {
		t.Errorf("history with a pending and a rejected request = %v, want only [%v %v]", got, pizza.ID, movie.ID)
	}
	s.expect(http.StatusOK, "POST", hp+"/reward-redemptions/"+waiting.ID.String()+"/approve", admin.AccessToken, nil, nil)
	got := ids("")
	inHistory := map[uuid.UUID]bool{}
	for _, id := range got {
		inHistory[id] = true
	}
	if len(got) != 3 || !inHistory[waiting.ID] || !inHistory[pizza.ID] || !inHistory[movie.ID] || inHistory[refused.ID] {
		t.Errorf("history after approving the pending request = %v, want %v, %v and %v", got, waiting.ID, pizza.ID, movie.ID)
	}

	// A member with nothing approved: 200 {"items": []}, never null.
	var ivy struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", hp+"/family-members", admin.AccessToken,
		map[string]string{"name": "Ivy", "pin": "5678"}, &ivy)
	var raw struct {
		Items json.RawMessage `json:"items"`
	}
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/family-members/"+ivy.ID.String()+"/reward-history", admin.AccessToken,
		map[string]string{"X-Member-PIN": "5678"}, nil, &raw)
	if string(raw.Items) != "[]" {
		t.Errorf("empty history items = %s, want []", raw.Items)
	}

	// Access is that of points-history. With an admin PIN set the admin
	// needs it; a wrong member PIN, or no PIN at all, is 403 wrong_pin.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", admin.AccessToken, map[string]string{"pin": "4321"}, nil)
	var viaAdminPin struct{ Items []redemptionJSON }
	s.expectWithHeaders(http.StatusOK, "GET", histPath, admin.AccessToken, map[string]string{"X-Admin-PIN": "4321"}, nil, &viaAdminPin)
	if len(viaAdminPin.Items) != 3 {
		t.Errorf("history with X-Admin-PIN = %d items, want 3", len(viaAdminPin.Items))
	}
	if got := ids(""); len(got) != 3 {
		t.Errorf("history with the member PIN after an admin PIN is set = %v, want three", got)
	}
	for name, hdr := range map[string]map[string]string{
		"wrong member PIN": {"X-Member-PIN": "0000"},
		"wrong admin PIN":  {"X-Admin-PIN": "0000"},
		"no PIN":           nil,
	} {
		var refusal apiError
		s.expectWithHeaders(http.StatusForbidden, "GET", histPath, admin.AccessToken, hdr, nil, &refusal)
		if refusal.Error.Code != "wrong_pin" {
			t.Errorf("%s: code %q, want wrong_pin", name, refusal.Error.Code)
		}
	}

	// A caller outside the household is 404, even with the right PIN.
	outsider := s.signUp("historyoutsider")
	var notFound apiError
	s.expectWithHeaders(http.StatusNotFound, "GET", histPath, outsider.AccessToken, memberPin, nil, &notFound)
	if notFound.Error.Code != "not_found" {
		t.Errorf("outsider: code %q, want not_found", notFound.Error.Code)
	}

	// A member of another household, asked for under this one, is 404.
	var oh struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", outsider.AccessToken,
		map[string]string{"name": "Other Home", "timezone": "UTC"}, &oh)
	var foreign struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+oh.ID.String()+"/family-members", outsider.AccessToken,
		map[string]string{"name": "Zed", "pin": "1234"}, &foreign)
	var foreignErr apiError
	s.expectWithHeaders(http.StatusNotFound, "GET", hp+"/family-members/"+foreign.ID.String()+"/reward-history", admin.AccessToken,
		map[string]string{"X-Member-PIN": "1234", "X-Admin-PIN": "4321"}, nil, &foreignErr)
	if foreignErr.Error.Code != "not_found" {
		t.Errorf("foreign member: code %q, want not_found", foreignErr.Error.Code)
	}
}

// TestAPI_RewardHistoryHasItsOwnPINBudget proves the reward-history read is
// rate limited like points-history: correct reads are counted on the history
// budget only, so they never spend the PIN budget a member action (redeem)
// needs, while a wrong PIN on it is charged to the shared PIN budget.
func TestAPI_RewardHistoryHasItsOwnPINBudget(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	q := db.New(pool)
	mail := &mailbox{text: map[string]string{}}
	authSvc := app.NewAuthService(q, pool, mail, app.AuthServiceConfig{
		JWTSecret: secret, AccessExpiry: 15 * time.Minute, RefreshExpiry: 24 * time.Hour,
		BCryptCost: 10, PasswordMinLen: 8, AppBaseURL: "http://chorus.test", AppName: "Chorus",
	})
	t.Cleanup(authSvc.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// PIN budget: member create + reward create + admin PIN set + one redeem
	// + one wrong history PIN. History budget: four correct reads + the wrong one.
	const pinLimit = 5
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		FamilySvc:    app.NewFamilyService(q, pool, 10),
		JWTSecret:    secret,
		PINRateLimit: pinLimit,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ts := &testServer{t: t, srv: srv, mail: mail, pool: pool}

	u := ts.signUp("rewardhistrl")
	var h struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "Reward History RL Home", "timezone": "UTC"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	var m struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", hp+"/family-members", u.AccessToken,
		map[string]string{"name": "Kid", "pin": "1234"}, &m)
	// A free reward, so the member can redeem it without points.
	var reward struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", hp+"/rewards", u.AccessToken,
		map[string]any{"description": "Sticker", "cost": 0}, &reward)
	// An admin PIN is set so a wrong member PIN is refused: with none set,
	// the household admin needs no PIN at all.
	ts.expect(http.StatusOK, "PUT", hp+"/admin-pin", u.AccessToken,
		map[string]string{"pin": "4321"}, nil)

	histPath := hp + "/family-members/" + m.ID.String() + "/reward-history"
	pinHdr := map[string]string{"X-Member-PIN": "1234"}
	for i := 0; i < pinLimit-1; i++ {
		ts.expectWithHeaders(http.StatusOK, "GET", histPath, u.AccessToken, pinHdr, nil, nil)
	}

	// The history reads spent none of the action budget: redeem still runs.
	redeemPath := hp + "/rewards/" + reward.ID.String() + "/redeem"
	ts.expect(http.StatusCreated, "POST", redeemPath, u.AccessToken,
		map[string]any{"member_id": m.ID, "pin": "1234"}, nil)

	// A wrong PIN on the history read is refused and charged to the shared
	// PIN budget, which it now fills: the next member action is limited.
	var wrongPin apiError
	ts.expectWithHeaders(http.StatusForbidden, "GET", histPath, u.AccessToken,
		map[string]string{"X-Member-PIN": "0000"}, nil, &wrongPin)
	if wrongPin.Error.Code != "wrong_pin" {
		t.Errorf("expected wrong_pin, got %q", wrongPin.Error.Code)
	}
	var actionRL apiError
	ts.expect(http.StatusTooManyRequests, "POST", redeemPath, u.AccessToken,
		map[string]any{"member_id": m.ID, "pin": "1234"}, &actionRL)
	if actionRL.Error.Code != "rate_limited" {
		t.Errorf("redeem after wrong history PIN: expected rate_limited, got %q", actionRL.Error.Code)
	}

	// The history read is still limited on its own budget.
	var rlErr apiError
	ts.expectWithHeaders(http.StatusTooManyRequests, "GET", histPath, u.AccessToken, pinHdr, nil, &rlErr)
	if rlErr.Error.Code != "rate_limited" {
		t.Errorf("expected rate_limited, got %q", rlErr.Error.Code)
	}
}
