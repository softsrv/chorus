//go:build integration

package http_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
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
}
