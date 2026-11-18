//go:build integration

package http_test

import (
	"net/http"
	"testing"

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
	s.expect(http.StatusCreated, "POST", hp+"/rewards", admin.AccessToken, map[string]any{"description": "Game time", "cost": 30}, &game)
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
