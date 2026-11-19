//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests drive the full HTTP stack (middleware, routing, handlers,
// services) against a real database, the same way a mobile client would.

func TestAPI_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("parent")

	// Verify auto-provisioning created an admin household on first sign-in.
	var initialHouseholds struct {
		Items []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
			Role string    `json:"role"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households", parent.AccessToken, nil, &initialHouseholds)
	if len(initialHouseholds.Items) != 1 || initialHouseholds.Items[0].Role != "admin" {
		t.Errorf("expected 1 admin household after sign-up, got %+v", initialHouseholds.Items)
	}

	// Household + invite + join.
	var h struct {
		ID   uuid.UUID `json:"id"`
		Role string    `json:"role"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "The Smiths", "timezone": "America/Chicago"}, &h)
	if h.Role != "admin" {
		t.Errorf("creator role: got %q", h.Role)
	}
	hp := "/api/v1/households/" + h.ID.String()

	// The old chore routes are gone: the API answers its JSON 404.
	var gone apiError
	s.expect(http.StatusNotFound, "GET", hp+"/chores", parent.AccessToken, nil, &gone)
	if gone.Error.Code != "not_found" {
		t.Errorf("GET chores: code %q, want not_found", gone.Error.Code)
	}
	gone = apiError{}
	s.expect(http.StatusNotFound, "PUT", hp+"/chores/"+uuid.NewString()+"/completion", parent.AccessToken, nil, &gone)
	if gone.Error.Code != "not_found" {
		t.Errorf("PUT chore completion: code %q, want not_found", gone.Error.Code)
	}

	// Quest list pagination; the documented bounds are inclusive.
	for i := range 3 {
		s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": fmt.Sprintf("quest %d", i)}, nil)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextOffset *int             `json:"next_offset"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests?limit=2", parent.AccessToken, nil, &page)
	if len(page.Items) != 2 || page.NextOffset == nil || *page.NextOffset != 2 {
		t.Fatalf("page 1: %d items, next %v", len(page.Items), page.NextOffset)
	}
	s.expect(http.StatusOK, "GET", hp+"/quests?limit=2&offset=2", parent.AccessToken, nil, &page)
	if len(page.Items) != 1 || page.NextOffset != nil {
		t.Fatalf("page 2: %d items, next %v", len(page.Items), page.NextOffset)
	}
	s.expect(http.StatusOK, "GET", hp+"/quests?limit=100", parent.AccessToken, nil, nil)
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/quests?limit=101", parent.AccessToken, nil, nil)
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/quests?limit=0", parent.AccessToken, nil, nil)
	// Clear them so the CRUD checks below start from an empty list.
	var cleanup struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &cleanup)
	for _, item := range cleanup.Items {
		s.expect(http.StatusNoContent, "DELETE", hp+"/quests/"+item.ID.String(), parent.AccessToken, nil, nil)
	}

	// Quest CRUD: what is posted is what GET returns.
	type questBody struct {
		ID         uuid.UUID  `json:"id"`
		LibraryID  *uuid.UUID `json:"library_id"`
		Name       string     `json:"name"`
		Points     int        `json:"points"`
		RepeatDays int        `json:"repeat_days"`
		Notes      string     `json:"notes"`
	}
	var quests struct {
		Items      []questBody `json:"items"`
		NextOffset *int        `json:"next_offset"`
	}
	var rawList map[string]json.RawMessage
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &rawList)
	if string(rawList["items"]) != "[]" {
		t.Errorf("empty quest list: items = %s, want []", rawList["items"])
	}
	// CLM-5: fresh household GET /quests returns 200 with items [] (not null) — and the library still lists.
	var freshLib struct {
		Items []questBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/quest-library", parent.AccessToken, nil, &freshLib)
	if len(freshLib.Items) == 0 {
		t.Errorf("fresh household cannot list quest library: 0 items")
	}
	var quest questBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "Walk the dog", "points": 15, "repeat_days": 2, "notes": "Around the block",
	}, &quest)
	wantQuest := questBody{ID: quest.ID, Name: "Walk the dog", Points: 15, RepeatDays: 2, Notes: "Around the block"}
	if quest != wantQuest {
		t.Errorf("created quest: %+v", quest)
	}
	qp := hp + "/quests/" + quest.ID.String()
	var gotQuest questBody
	s.expect(http.StatusOK, "GET", qp, parent.AccessToken, nil, &gotQuest)
	if gotQuest != wantQuest {
		t.Errorf("get quest: %+v", gotQuest)
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &quests)
	if len(quests.Items) != 1 || quests.Items[0] != wantQuest || quests.NextOffset != nil {
		t.Errorf("list quests: %+v", quests)
	}
	s.expect(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "notes": "",
	}, &gotQuest)
	if gotQuest != (questBody{ID: quest.ID, Name: "Walk the dog twice", Points: 20, RepeatDays: 1}) {
		t.Errorf("updated quest: %+v", gotQuest)
	}
	// One 422 lists every bad field, including a wrong-typed number.
	var qerr apiError
	if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "", "points": "lots", "repeat_days": 0,
	}, &qerr); got != http.StatusUnprocessableEntity ||
		qerr.Error.Fields["name"] == "" || qerr.Error.Fields["points"] == "" || qerr.Error.Fields["repeat_days"] == "" {
		t.Errorf("invalid quest: status %d, %+v", got, qerr)
	}
	// Non-numeric points value (e.g. "abc") is rejected with 422 and saves nothing.
	if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "Bad Points Quest", "points": "abc",
	}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["points"] == "" {
		t.Errorf("non-numeric points quest: status %d, %+v", got, qerr)
	}
	// notes > 2000 chars rejected
	if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "Long Notes", "notes": strings.Repeat("x", 2001),
	}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["notes"] == "" {
		t.Errorf("too long notes quest: status %d, %+v", got, qerr)
	}
	// Rejected creates saved nothing: list count is still 1.
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &quests)
	if len(quests.Items) != 1 {
		t.Errorf("rejected creates saved rows: %d quests", len(quests.Items))
	}
	// PUT with invalid fields rejected with 422, row unchanged.
	if got := s.do("PUT", qp, parent.AccessToken, map[string]any{
		"name": "", "points": -1, "repeat_days": 0,
	}, &qerr); got != http.StatusUnprocessableEntity ||
		qerr.Error.Fields["name"] == "" || qerr.Error.Fields["points"] == "" || qerr.Error.Fields["repeat_days"] == "" {
		t.Errorf("invalid quest update: status %d, %+v", got, qerr)
	}
	var unchangedAfterPut questBody
	s.expect(http.StatusOK, "GET", qp, parent.AccessToken, nil, &unchangedAfterPut)
	if unchangedAfterPut != gotQuest {
		t.Errorf("quest row changed after rejected update: %+v vs %+v", unchangedAfterPut, gotQuest)
	}
	// CLM-12: a negative or non-whole streak_bonus is a 422 naming
	// streak_bonus, on create and on update, and saves nothing.
	for _, bad := range []any{-1, 1.5, "five"} {
		qerr = apiError{}
		if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
			"name": "Streaky", "streak_bonus": bad,
		}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["streak_bonus"] == "" {
			t.Errorf("create with streak_bonus %v: status %d, %+v", bad, got, qerr)
		}
		qerr = apiError{}
		if got := s.do("PUT", qp, parent.AccessToken, map[string]any{
			"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": bad,
		}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["streak_bonus"] == "" {
			t.Errorf("update with streak_bonus %v: status %d, %+v", bad, got, qerr)
		}
	}
	readStreakBonus := func() int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(context.Background(), `SELECT streak_bonus FROM household_quests WHERE id = $1`, quest.ID).Scan(&n); err != nil {
			t.Fatalf("read streak_bonus: %v", err)
		}
		return n
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &quests)
	if len(quests.Items) != 1 {
		t.Errorf("rejected streak_bonus creates saved rows: %d quests", len(quests.Items))
	}
	s.expect(http.StatusOK, "GET", qp, parent.AccessToken, nil, &unchangedAfterPut)
	if unchangedAfterPut != gotQuest {
		t.Errorf("quest row changed after rejected streak_bonus update: %+v vs %+v", unchangedAfterPut, gotQuest)
	}
	if n := readStreakBonus(); n != 0 {
		t.Errorf("streak_bonus after rejected updates: %d, want 0", n)
	}
	// Changing streak_bonus needs the admin PIN like other quest changes.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", parent.AccessToken, map[string]string{"pin": "1357"}, nil)
	qerr = apiError{}
	s.expect(http.StatusForbidden, "PUT", qp, parent.AccessToken, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": 5,
	}, &qerr)
	if qerr.Error.Code != "admin_pin_required" || readStreakBonus() != 0 {
		t.Errorf("streak_bonus update without admin pin: code %q, streak_bonus %d", qerr.Error.Code, readStreakBonus())
	}
	// A valid streak_bonus is saved with the PIN.
	s.expectWithHeaders(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": 5,
	}, nil)
	if n := readStreakBonus(); n != 5 {
		t.Errorf("streak_bonus after valid update: %d, want 5", n)
	}
	// A PUT that omits streak_bonus keeps the stored one (an older client or a
	// blank edit field never wipes it).
	s.expectWithHeaders(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1,
	}, nil)
	if n := readStreakBonus(); n != 5 {
		t.Errorf("omitted streak_bonus on PUT: %d, want 5 kept", n)
	}
	// Back to the state the rest of this test expects: no streak bonus, no PIN.
	s.expectWithHeaders(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": 0,
	}, nil)
	s.expectWithHeaders(http.StatusNoContent, "DELETE", hp+"/admin-pin", parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, nil, nil)
	if n := readStreakBonus(); n != 0 {
		t.Errorf("explicit streak_bonus 0 on PUT: %d, want 0", n)
	}

	// CLM-4: POST/PUT/DELETE on /api/v1/quest-library return 404 or 405.
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		st := s.do(m, "/api/v1/quest-library", parent.AccessToken, map[string]any{"name": "test"}, nil)
		if st != http.StatusNotFound && st != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/quest-library returned %d, want 404 or 405", m, st)
		}
	}

	// Accept: a verified acting member takes an available quest; the 200
	// body carries its notes, claimed_by and available=false, and a second
	// member is told it is taken.
	type acceptedBody struct {
		ID        uuid.UUID  `json:"id"`
		Notes     string     `json:"notes"`
		Available bool       `json:"available"`
		ClaimedBy *uuid.UUID `json:"claimed_by"`
		ClaimedAt *time.Time `json:"claimed_at"`
	}
	var member, member2 struct {
		ID uuid.UUID `json:"id"`
	}
	s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken, map[string]string{"name": "Kid", "pin": "4321"}, &member)
	s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken, map[string]string{"name": "Kid two"}, &member2)
	var acceptQuest acceptedBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Take out trash", "notes": "Blue bin too"}, &acceptQuest)
	if !acceptQuest.Available || acceptQuest.ClaimedBy != nil {
		t.Errorf("new quest: %+v, want available and unclaimed", acceptQuest)
	}
	ap := hp + "/quests/" + acceptQuest.ID.String() + "/accept"
	var wrongPin apiError
	s.expect(http.StatusForbidden, "POST", ap, parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "0000"}, &wrongPin)
	if wrongPin.Error.Code != "wrong_pin" {
		t.Errorf("accept with wrong pin: code %q", wrongPin.Error.Code)
	}
	s.expect(http.StatusUnprocessableEntity, "POST", ap, parent.AccessToken, map[string]any{"pin": "4321"}, nil)
	var accepted acceptedBody
	s.expect(http.StatusOK, "POST", ap, parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, &accepted)
	if accepted.ID != acceptQuest.ID || accepted.Notes != "Blue bin too" || accepted.Available ||
		accepted.ClaimedBy == nil || *accepted.ClaimedBy != member.ID || accepted.ClaimedAt == nil {
		t.Errorf("accepted quest: %+v", accepted)
	}
	var afterAccept struct {
		Items []acceptedBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &afterAccept)
	for _, it := range afterAccept.Items {
		if it.ID == acceptQuest.ID && (it.Available || it.ClaimedBy == nil || *it.ClaimedBy != member.ID) {
			t.Errorf("listed accepted quest: %+v", it)
		}
	}
	var taken apiError
	s.expect(http.StatusConflict, "POST", ap, parent.AccessToken, map[string]any{"member_id": member2.ID}, &taken)
	if taken.Error.Code != "quest_taken" {
		t.Errorf("second accept: code %q, want quest_taken", taken.Error.Code)
	}

	// Share: the holder of a shareable quest adds another member, each with
	// their own PIN (Kid two has none, so target_pin is empty). The quest
	// is then shared, lists both members and stays held; the share route is
	// a PIN-limited member action like accept.
	type sharedBody struct {
		ID            uuid.UUID   `json:"id"`
		ClaimedBy     *uuid.UUID  `json:"claimed_by"`
		Shareable     bool        `json:"shareable"`
		TeamworkBonus int32       `json:"teamwork_bonus"`
		Shared        bool        `json:"shared"`
		Participants  []uuid.UUID `json:"participants"`
	}
	var shareQuest sharedBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Clean the living room", "points": 100}, &shareQuest)
	if shareQuest.Shared || shareQuest.Shareable || shareQuest.TeamworkBonus != 0 || shareQuest.Participants == nil || len(shareQuest.Participants) != 0 {
		t.Errorf("new quest: %+v, want not shareable, bonus 0, not shared, participants []", shareQuest)
	}
	sq := hp + "/quests/" + shareQuest.ID.String()
	// The administrator makes it shareable with a teamwork bonus by editing it;
	// a bad bonus is a 422 on teamwork_bonus and changes nothing.
	for _, bad := range []map[string]any{
		{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": -1},
		{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": 2.5},
		{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": "abc"},
		{"name": "Clean the living room", "points": 100, "shareable": false, "teamwork_bonus": 20},
	} {
		var badBonus apiError
		s.expect(http.StatusUnprocessableEntity, "PUT", sq, parent.AccessToken, bad, &badBonus)
		if badBonus.Error.Fields["teamwork_bonus"] == "" {
			t.Errorf("PUT %v: %+v, want a teamwork_bonus field error", bad, badBonus)
		}
	}
	var badCreate apiError
	s.expect(http.StatusUnprocessableEntity, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Bonus only", "teamwork_bonus": 5}, &badCreate)
	if badCreate.Error.Fields["teamwork_bonus"] == "" {
		t.Errorf("create with bonus but not shareable: %+v, want a teamwork_bonus field error", badCreate)
	}
	var madeShareable sharedBody
	s.expect(http.StatusOK, "PUT", sq, parent.AccessToken, map[string]any{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": 20}, &madeShareable)
	if !madeShareable.Shareable || madeShareable.TeamworkBonus != 20 {
		t.Errorf("edited quest: %+v, want shareable, bonus 20", madeShareable)
	}
	var createdShareable sharedBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Wash the car", "points": 30, "shareable": true, "teamwork_bonus": 5}, &createdShareable)
	if !createdShareable.Shareable || createdShareable.TeamworkBonus != 5 {
		t.Errorf("created shareable quest: %+v, want shareable, bonus 5", createdShareable)
	}
	var gotShareable sharedBody
	s.expect(http.StatusOK, "GET", hp+"/quests/"+createdShareable.ID.String(), parent.AccessToken, nil, &gotShareable)
	if !gotShareable.Shareable || gotShareable.TeamworkBonus != 5 {
		t.Errorf("get shareable quest: %+v, want shareable, bonus 5", gotShareable)
	}
	s.expect(http.StatusNoContent, "DELETE", hp+"/quests/"+createdShareable.ID.String(), parent.AccessToken, nil, nil)
	s.expect(http.StatusOK, "POST", sq+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	var shareErr apiError
	s.expect(http.StatusUnprocessableEntity, "POST", sq+"/share", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, &shareErr)
	if shareErr.Error.Fields["target_member_id"] == "" {
		t.Errorf("share without target: %+v, want a target_member_id field error", shareErr)
	}
	shareErr = apiError{}
	s.expect(http.StatusForbidden, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID, "target_pin": "0000"}, &shareErr)
	if shareErr.Error.Code != "wrong_pin" || shareErr.Error.Fields["target_pin"] == "" {
		t.Errorf("share with wrong target pin: %+v, want wrong_pin with a target_pin field", shareErr)
	}
	shareErr = apiError{}
	s.expect(http.StatusForbidden, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "0000", "target_member_id": member2.ID, "target_pin": ""}, &shareErr)
	if shareErr.Error.Code != "wrong_pin" || len(shareErr.Error.Fields) != 0 {
		t.Errorf("share with wrong sharer pin: %+v, want wrong_pin with no fields", shareErr)
	}
	s.expect(http.StatusNotFound, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": uuid.New()}, nil)
	var sharedQuest sharedBody
	s.expect(http.StatusOK, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID, "target_pin": ""}, &sharedQuest)
	wantParticipants := []uuid.UUID{member.ID, member2.ID}
	if !sharedQuest.Shared || !slices.Equal(sharedQuest.Participants, wantParticipants) ||
		sharedQuest.ClaimedBy == nil || *sharedQuest.ClaimedBy != member.ID {
		t.Errorf("shared quest: %+v, want shared, participants %v, held by %v", sharedQuest, wantParticipants, member.ID)
	}
	var gotShared sharedBody
	s.expect(http.StatusOK, "GET", sq, parent.AccessToken, nil, &gotShared)
	if !gotShared.Shared || !slices.Equal(gotShared.Participants, wantParticipants) {
		t.Errorf("get shared quest: %+v", gotShared)
	}
	var sharedList struct {
		Items []sharedBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &sharedList)
	for _, it := range sharedList.Items {
		if it.ID == shareQuest.ID && (!it.Shared || !slices.Equal(it.Participants, wantParticipants)) {
			t.Errorf("listed shared quest: %+v", it)
		}
		if it.ID != shareQuest.ID && (it.Shared || len(it.Participants) != 0) {
			t.Errorf("listed unshared quest %s: shared=%v participants=%v", it.ID, it.Shared, it.Participants)
		}
	}
	shareErr = apiError{}
	s.expect(http.StatusConflict, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID}, &shareErr)
	if shareErr.Error.Code != "already_participant" {
		t.Errorf("share again: code %q, want already_participant", shareErr.Error.Code)
	}
	// Clear it so the quest lists below hold only the trash chore.
	s.expect(http.StatusNoContent, "DELETE", sq, parent.AccessToken, nil, nil)

	// Approval is per completion: nothing waits yet ({items: []}, not null);
	// the holder marks the chore done, which releases it into its cooldown and
	// records a pending completion; the administrator sends that completion
	// back, which reopens the chore; the holder does it again and approval
	// credits the points, visible in the family-member list.
	qbase := hp + "/quests/" + acceptQuest.ID.String()
	var rawPending map[string]json.RawMessage
	s.expect(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, nil, &rawPending)
	if string(rawPending["items"]) != "[]" {
		t.Errorf("empty pending list: items = %s, want []", rawPending["items"])
	}
	s.expect(http.StatusNotFound, "POST", qbase+"/abandon", parent.AccessToken, map[string]any{"member_id": member2.ID}, nil)
	s.expect(http.StatusNotFound, "POST", qbase+"/done", parent.AccessToken, map[string]any{"member_id": member2.ID}, nil)
	var doneBody map[string]json.RawMessage
	s.expect(http.StatusOK, "POST", qbase+"/done", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, &doneBody)
	if string(doneBody["awaiting_approval"]) != "true" || string(doneBody["claimed_by"]) != "null" || string(doneBody["available"]) != "false" ||
		string(doneBody["available_at"]) == "null" || string(doneBody["completed_at"]) == "null" {
		t.Errorf("marked done: %s", doneBody)
	}
	if _, has := doneBody["done_at"]; has {
		t.Errorf("marked done still carries done_at: %s", doneBody)
	}
	// The holder let go at mark-done: it can no longer abandon the chore.
	s.expect(http.StatusNotFound, "POST", qbase+"/abandon", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	listedQuest := func() (awaiting, available bool, hasDoneAt bool) {
		t.Helper()
		var list struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &list)
		for _, it := range list.Items {
			if string(it["id"]) == `"`+acceptQuest.ID.String()+`"` {
				_, has := it["done_at"]
				return string(it["awaiting_approval"]) == "true", string(it["available"]) == "true", has
			}
		}
		t.Fatalf("quest %s not listed", acceptQuest.ID)
		return false, false, false
	}
	if awaiting, available, hasDoneAt := listedQuest(); !awaiting || available || hasDoneAt {
		t.Errorf("listed after done: awaiting_approval=%v available=%v done_at present=%v, want true false false", awaiting, available, hasDoneAt)
	}
	type pendingRow struct {
		CompletionID uuid.UUID  `json:"completion_id"`
		QuestID      uuid.UUID  `json:"quest_id"`
		Name         string     `json:"name"`
		Points       int        `json:"points"`
		MemberName   string     `json:"member_name"`
		DoneAt       *time.Time `json:"done_at"`
	}
	var pendingList struct {
		Items []pendingRow `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, nil, &pendingList)
	if len(pendingList.Items) != 1 || pendingList.Items[0].QuestID != acceptQuest.ID || pendingList.Items[0].MemberName != "Kid" ||
		pendingList.Items[0].Name != "Take out trash" || pendingList.Items[0].Points != 0 ||
		pendingList.Items[0].CompletionID == uuid.Nil || pendingList.Items[0].DoneAt == nil {
		t.Errorf("pending list: %+v", pendingList.Items)
	}
	firstCompletion := hp + "/quest-completions/" + pendingList.Items[0].CompletionID.String()
	var rejectedBody struct {
		Completion struct {
			ID     uuid.UUID `json:"id"`
			Status string    `json:"status"`
		} `json:"completion"`
	}
	s.expect(http.StatusOK, "POST", firstCompletion+"/reject", parent.AccessToken, map[string]any{}, &rejectedBody)
	if rejectedBody.Completion.ID != pendingList.Items[0].CompletionID || rejectedBody.Completion.Status != "rejected" {
		t.Errorf("rejected: %+v", rejectedBody)
	}
	s.expect(http.StatusNotFound, "POST", firstCompletion+"/reject", parent.AccessToken, map[string]any{}, nil)
	s.expect(http.StatusNotFound, "POST", firstCompletion+"/approve", parent.AccessToken, map[string]any{}, nil)
	s.expect(http.StatusBadRequest, "POST", firstCompletion+"/approve", parent.AccessToken, map[string]any{"admin_pin": "1234"}, nil)
	if awaiting, available, _ := listedQuest(); awaiting || !available {
		t.Errorf("listed after reject: awaiting_approval=%v available=%v, want false true", awaiting, available)
	}
	// The old per-quest approve and reject routes are gone.
	s.expect(http.StatusNotFound, "POST", qbase+"/approve", parent.AccessToken, map[string]any{}, nil)
	s.expect(http.StatusNotFound, "POST", qbase+"/reject", parent.AccessToken, map[string]any{}, nil)
	// Give the quest points so the credit is visible, then do it again.
	s.expect(http.StatusOK, "PUT", qbase, parent.AccessToken, map[string]any{"name": "Take out trash", "points": 9, "repeat_days": 2, "notes": "Blue bin too"}, nil)
	s.expect(http.StatusOK, "POST", qbase+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expect(http.StatusOK, "POST", qbase+"/done", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	pendingList.Items = nil
	s.expect(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, nil, &pendingList)
	if len(pendingList.Items) != 1 || pendingList.Items[0].Points != 9 {
		t.Fatalf("pending after second mark-done: %+v", pendingList.Items)
	}
	cbase := hp + "/quest-completions/" + pendingList.Items[0].CompletionID.String()

	// With an admin PIN set: a missing or wrong header is a 403, never a
	// sign-out 401, and changes nothing.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", parent.AccessToken, map[string]string{"pin": "8642"}, nil)
	var pinErr apiError
	s.expect(http.StatusForbidden, "POST", cbase+"/approve", parent.AccessToken, map[string]any{}, &pinErr)
	if pinErr.Error.Code != "admin_pin_required" {
		t.Errorf("approve without admin pin: code %q", pinErr.Error.Code)
	}
	pinErr = apiError{}
	s.expectWithHeaders(http.StatusForbidden, "POST", cbase+"/approve", parent.AccessToken, map[string]string{"X-Admin-PIN": "0000"}, map[string]any{}, &pinErr)
	if pinErr.Error.Code != "wrong_pin" {
		t.Errorf("approve with wrong admin pin: code %q", pinErr.Error.Code)
	}
	pinErr = apiError{}
	s.expectWithHeaders(http.StatusForbidden, "GET", hp+"/quests/pending", parent.AccessToken, map[string]string{"X-Admin-PIN": "0000"}, nil, &pinErr)
	if pinErr.Error.Code != "wrong_pin" {
		t.Errorf("pending with wrong admin pin: code %q", pinErr.Error.Code)
	}
	s.expectWithHeaders(http.StatusForbidden, "POST", cbase+"/reject", parent.AccessToken, map[string]string{"X-Admin-PIN": "0000"}, map[string]any{}, nil)
	adminHdr := map[string]string{"X-Admin-PIN": "8642"}
	pendingList.Items = nil
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &pendingList)
	if len(pendingList.Items) != 1 || pendingList.Items[0].Points != 9 {
		t.Errorf("pending after refused approvals: %+v", pendingList.Items)
	}

	var approvedBody struct {
		Completion struct {
			ID         uuid.UUID   `json:"id"`
			QuestID    uuid.UUID   `json:"quest_id"`
			DoneAt     *time.Time  `json:"done_at"`
			ApprovedAt *time.Time  `json:"approved_at"`
			Status     string      `json:"status"`
			MemberIDs  []uuid.UUID `json:"member_ids"`
		} `json:"completion"`
		Member struct {
			ID     uuid.UUID `json:"id"`
			Name   string    `json:"name"`
			HasPIN bool      `json:"has_pin"`
			Points int       `json:"points"`
		} `json:"member"`
		ComboBonuses []json.RawMessage `json:"combo_bonuses"`
		StreakBonus  *int              `json:"streak_bonus"`
		StreakCount  *int              `json:"streak_count"`
		Members      []struct {
			Member struct {
				ID     uuid.UUID `json:"id"`
				Points int       `json:"points"`
			} `json:"member"`
			PointsAwarded int `json:"points_awarded"`
			TeamworkBonus int `json:"teamwork_bonus"`
		} `json:"members"`
	}
	s.expectWithHeaders(http.StatusOK, "POST", cbase+"/approve", parent.AccessToken, adminHdr, map[string]any{}, &approvedBody)
	// CLM-13 / CLM-16: the first mark-done was sent back, which undid its
	// streak step, so this approval is the 1st in a row: count 1, no bonus.
	if approvedBody.StreakBonus == nil || *approvedBody.StreakBonus != 0 ||
		approvedBody.StreakCount == nil || *approvedBody.StreakCount != 1 {
		t.Errorf("approve streak fields: streak_bonus=%v streak_count=%v, want 0 and 1", approvedBody.StreakBonus, approvedBody.StreakCount)
	}
	if approvedBody.Completion.ID != pendingList.Items[0].CompletionID || approvedBody.Completion.QuestID != acceptQuest.ID ||
		approvedBody.Completion.Status != "approved" || approvedBody.Completion.ApprovedAt == nil ||
		approvedBody.Completion.DoneAt == nil || !approvedBody.Completion.DoneAt.Equal(*pendingList.Items[0].DoneAt) ||
		approvedBody.Member.ID != member.ID || approvedBody.Member.Name != "Kid" || !approvedBody.Member.HasPIN || approvedBody.Member.Points != 9 ||
		approvedBody.ComboBonuses == nil || len(approvedBody.ComboBonuses) != 0 {
		t.Errorf("approved: %+v", approvedBody)
	}
	// A quest that is not shared credits its one member, with no teamwork
	// bonus, in members as well as member.
	if len(approvedBody.Members) != 1 || approvedBody.Members[0].Member.ID != member.ID ||
		approvedBody.Members[0].Member.Points != 9 || approvedBody.Members[0].TeamworkBonus != 0 ||
		len(approvedBody.Completion.MemberIDs) != 1 || approvedBody.Completion.MemberIDs[0] != member.ID {
		t.Errorf("approved members: %+v, completion member_ids %v; want only Kid, no bonus", approvedBody.Members, approvedBody.Completion.MemberIDs)
	}
	s.expectWithHeaders(http.StatusNotFound, "POST", cbase+"/approve", parent.AccessToken, adminHdr, map[string]any{}, nil)
	if awaiting, _, _ := listedQuest(); awaiting {
		t.Errorf("listed after approval: awaiting_approval = true, want false")
	}
	var familyList struct {
		Items []struct {
			ID     uuid.UUID `json:"id"`
			Points int       `json:"points"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, nil, &familyList)
	for _, m := range familyList.Items {
		want := 0
		if m.ID == member.ID {
			want = 9
		}
		if m.Points != want {
			t.Errorf("family member %s points = %d, want %d", m.ID, m.Points, want)
		}
	}
	rawPending = nil
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &rawPending)
	if string(rawPending["items"]) != "[]" {
		t.Errorf("pending after approval: items = %s, want []", rawPending["items"])
	}

	// Points history (CLM-6):
	// A second quest approval for member so two awards are present.
	var quest2 acceptedBody
	s.expectWithHeaders(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, adminHdr, map[string]any{"name": "Dishes", "points": 5}, &quest2)
	q2base := hp + "/quests/" + quest2.ID.String()
	s.expect(http.StatusOK, "POST", q2base+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expect(http.StatusOK, "POST", q2base+"/done", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	pendingList.Items = nil
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &pendingList)
	if len(pendingList.Items) != 1 {
		t.Fatalf("pending after quest2 mark-done: %+v", pendingList.Items)
	}
	c2base := hp + "/quest-completions/" + pendingList.Items[0].CompletionID.String()
	s.expectWithHeaders(http.StatusOK, "POST", c2base+"/approve", parent.AccessToken, adminHdr, map[string]any{}, nil)
	s.expectWithHeaders(http.StatusNoContent, "DELETE", q2base, parent.AccessToken, adminHdr, nil, nil)

	histPath := hp + "/family-members/" + member.ID.String() + "/points-history"

	// Wrong X-Member-PIN returns 403 wrong_pin.
	var histPinErr apiError
	s.expectWithHeaders(http.StatusForbidden, "GET", histPath, parent.AccessToken, map[string]string{"X-Member-PIN": "0000"}, nil, &histPinErr)
	if histPinErr.Error.Code != "wrong_pin" {
		t.Errorf("history with wrong pin: code %q", histPinErr.Error.Code)
	}

	// GET after two approvals returns 200 with items newest first and all award fields.
	var hist struct {
		Items []struct {
			ID       uuid.UUID `json:"id"`
			Kind     string    `json:"kind"`
			Reason   string    `json:"reason"`
			Points   int       `json:"points"`
			EarnedAt time.Time `json:"earned_at"`
		} `json:"items"`
	}
	s.expectWithHeaders(http.StatusOK, "GET", histPath, parent.AccessToken, map[string]string{"X-Member-PIN": "4321"}, nil, &hist)
	if len(hist.Items) != 2 {
		t.Fatalf("points history count: got %d, want 2", len(hist.Items))
	}
	if hist.Items[0].Reason != "Dishes" || hist.Items[0].Points != 5 || hist.Items[0].Kind != "quest" {
		t.Errorf("newest award mismatch: %+v", hist.Items[0])
	}
	if hist.Items[1].Reason != "Take out trash" || hist.Items[1].Points != 9 || hist.Items[1].Kind != "quest" {
		t.Errorf("older award mismatch: %+v", hist.Items[1])
	}
	if hist.Items[0].ID == uuid.Nil || hist.Items[0].EarnedAt.IsZero() {
		t.Errorf("expected populated award fields: %+v", hist.Items[0])
	}

	// GET for a member with no awards returns {items: []}.
	var emptyHist struct {
		Items []map[string]any `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/family-members/"+member2.ID.String()+"/points-history", parent.AccessToken, nil, &emptyHist)
	if emptyHist.Items == nil || len(emptyHist.Items) != 0 {
		t.Errorf("empty member history: %+v, want []", emptyHist.Items)
	}

	// GET for unknown member in household returns 404 (identical to random uuid).
	var randHistErr apiError
	s.expect(http.StatusNotFound, "GET", hp+"/family-members/"+uuid.NewString()+"/points-history", parent.AccessToken, nil, &randHistErr)
	if randHistErr.Error.Code != "not_found" {
		t.Errorf("random member points-history: code %q, want not_found", randHistErr.Error.Code)
	}

	// A shared quest completes for everyone on it: Kid accepts a shareable
	// 100-point quest with a teamwork bonus of 20 and shares it with Kid two,
	// who marks it done. One item waits, naming the marker in the old
	// member_id/member_name and both members with 50 each in members;
	// approving pays each 100/2 + 20 = 70 once, keeping completion, member
	// (the marker) and combo_bonuses beside the new members and member_ids.
	var teamQuest sharedBody
	s.expectWithHeaders(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, adminHdr,
		map[string]any{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": 20}, &teamQuest)
	tq := hp + "/quests/" + teamQuest.ID.String()
	s.expect(http.StatusOK, "POST", tq+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expect(http.StatusOK, "POST", tq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID, "target_pin": ""}, nil)
	s.expect(http.StatusOK, "POST", tq+"/done", parent.AccessToken, map[string]any{"member_id": member2.ID}, nil)
	var teamPending struct {
		Items []struct {
			CompletionID  uuid.UUID `json:"completion_id"`
			QuestID       uuid.UUID `json:"quest_id"`
			Name          string    `json:"name"`
			Points        int       `json:"points"`
			TeamworkBonus int       `json:"teamwork_bonus"`
			MemberID      uuid.UUID `json:"member_id"`
			MemberName    string    `json:"member_name"`
			Members       []struct {
				MemberID   uuid.UUID `json:"member_id"`
				MemberName string    `json:"member_name"`
				Points     int       `json:"points"`
			} `json:"members"`
		} `json:"items"`
	}
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &teamPending)
	if len(teamPending.Items) != 1 {
		t.Fatalf("pending after shared mark-done: %+v, want one item", teamPending.Items)
	}
	tp := teamPending.Items[0]
	if tp.QuestID != teamQuest.ID || tp.Name != "Clean the living room" || tp.Points != 100 || tp.TeamworkBonus != 20 ||
		tp.MemberID != member2.ID || tp.MemberName != "Kid two" || len(tp.Members) != 2 {
		t.Errorf("shared pending item: %+v, want marker Kid two, bonus 20, two members", tp)
	}
	wantNames := map[uuid.UUID]string{member.ID: "Kid", member2.ID: "Kid two"}
	for _, m := range tp.Members {
		if wantNames[m.MemberID] != m.MemberName || m.Points != 50 {
			t.Errorf("shared pending member: %+v, want one of %v with 50 points", m, wantNames)
		}
	}
	if len(tp.Members) == 2 && tp.Members[0].MemberID == tp.Members[1].MemberID {
		t.Errorf("shared pending members repeat one member: %+v", tp.Members)
	}
	tc := hp + "/quest-completions/" + tp.CompletionID.String()
	var teamApproved struct {
		Completion struct {
			ID        uuid.UUID   `json:"id"`
			QuestID   uuid.UUID   `json:"quest_id"`
			MemberID  *uuid.UUID  `json:"member_id"`
			MemberIDs []uuid.UUID `json:"member_ids"`
			Status    string      `json:"status"`
		} `json:"completion"`
		Member struct {
			ID     uuid.UUID `json:"id"`
			Name   string    `json:"name"`
			Points int       `json:"points"`
		} `json:"member"`
		ComboBonuses []json.RawMessage `json:"combo_bonuses"`
		Members      []struct {
			Member struct {
				ID     uuid.UUID `json:"id"`
				Points int       `json:"points"`
			} `json:"member"`
			PointsAwarded int `json:"points_awarded"`
			TeamworkBonus int `json:"teamwork_bonus"`
		} `json:"members"`
	}
	s.expectWithHeaders(http.StatusOK, "POST", tc+"/approve", parent.AccessToken, adminHdr, map[string]any{}, &teamApproved)
	// Kid already had 9 from the trash chore and 5 from Dishes; Kid two had nothing.
	wantTotals := map[uuid.UUID]int{member.ID: 84, member2.ID: 70}
	if teamApproved.Completion.ID != tp.CompletionID || teamApproved.Completion.QuestID != teamQuest.ID ||
		teamApproved.Completion.Status != "approved" || teamApproved.Completion.MemberID == nil || *teamApproved.Completion.MemberID != member2.ID ||
		len(teamApproved.Completion.MemberIDs) != 2 ||
		teamApproved.Member.ID != member2.ID || teamApproved.Member.Name != "Kid two" || teamApproved.Member.Points != 70 ||
		teamApproved.ComboBonuses == nil || len(teamApproved.ComboBonuses) != 0 || len(teamApproved.Members) != 2 {
		t.Errorf("shared approve: %+v", teamApproved)
	}
	for _, id := range teamApproved.Completion.MemberIDs {
		if _, ok := wantTotals[id]; !ok {
			t.Errorf("shared approve member_ids: %v, want Kid and Kid two", teamApproved.Completion.MemberIDs)
		}
	}
	for _, m := range teamApproved.Members {
		if want, ok := wantTotals[m.Member.ID]; !ok || m.Member.Points != want || m.PointsAwarded != 50 || m.TeamworkBonus != 20 {
			t.Errorf("shared approve member: %+v, want 50 + 20 bonus, total %d", m, want)
		}
	}
	s.expectWithHeaders(http.StatusNotFound, "POST", tc+"/approve", parent.AccessToken, adminHdr, map[string]any{}, nil)
	familyList.Items = nil
	s.expect(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, nil, &familyList)
	for _, m := range familyList.Items {
		if want := wantTotals[m.ID]; m.Points != want {
			t.Errorf("after shared approve and repeat: member %s points = %d, want %d", m.ID, m.Points, want)
		}
	}
	// CLM-1: every member of the shared completion gets a history entry for
	// their split and one for the teamwork bonus, kind quest, reason the
	// quest name, earned at the approval.
	var teamHist struct {
		Items []struct {
			Kind     string    `json:"kind"`
			Reason   string    `json:"reason"`
			Points   int       `json:"points"`
			EarnedAt time.Time `json:"earned_at"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/family-members/"+member2.ID.String()+"/points-history", parent.AccessToken, nil, &teamHist)
	if len(teamHist.Items) != 2 {
		t.Fatalf("shared quest history for Kid two: %+v, want split and teamwork bonus entries", teamHist.Items)
	}
	gotPts := map[int]bool{}
	for _, it := range teamHist.Items {
		if it.Kind != "quest" || it.Reason != "Clean the living room" || it.EarnedAt.IsZero() {
			t.Errorf("shared quest history entry: %+v", it)
		}
		gotPts[it.Points] = true
	}
	if !gotPts[50] || !gotPts[20] {
		t.Errorf("shared quest history points: %+v, want 50 and 20", teamHist.Items)
	}
	teamHist.Items = nil
	s.expectWithHeaders(http.StatusOK, "GET", histPath, parent.AccessToken, map[string]string{"X-Member-PIN": "4321"}, nil, &teamHist)
	if len(teamHist.Items) != 4 || teamHist.Items[0].Reason != "Clean the living room" || teamHist.Items[1].Reason != "Clean the living room" ||
		teamHist.Items[0].Points+teamHist.Items[1].Points != 70 {
		t.Errorf("shared quest history for Kid: %+v, want two newest entries of 50 and 20", teamHist.Items)
	}
	s.expectWithHeaders(http.StatusNoContent, "DELETE", tq, parent.AccessToken, adminHdr, nil, nil)
	// No self-complete route exists.
	s.expect(http.StatusNotFound, "POST", qbase+"/complete", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expectWithHeaders(http.StatusNoContent, "DELETE", hp+"/admin-pin", parent.AccessToken, adminHdr, nil, nil)
	s.expect(http.StatusNoContent, "DELETE", hp+"/quests/"+acceptQuest.ID.String(), parent.AccessToken, nil, nil)

	// Library copy keeps the library's values and its id.
	var library struct {
		Items []questBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/quest-library", parent.AccessToken, nil, &library)
	if len(library.Items) == 0 {
		t.Fatal("quest library is empty")
	}
	var copied questBody
	s.expect(http.StatusCreated, "POST", hp+"/quests/from-library", parent.AccessToken,
		map[string]any{"library_id": library.Items[0].ID}, &copied)
	if copied.LibraryID == nil || *copied.LibraryID != library.Items[0].ID || copied.Name != library.Items[0].Name ||
		copied.Points != library.Items[0].Points || copied.RepeatDays != library.Items[0].RepeatDays {
		t.Errorf("copied quest %+v vs library %+v", copied, library.Items[0])
	}

	s.expect(http.StatusNoContent, "DELETE", qp, parent.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", qp, parent.AccessToken, nil, nil)
	endToEndRewards(t, endToEndFixture{
		s: s, parent: parent, hp: hp, autoHouseholdID: initialHouseholds.Items[0].ID,
		memberID: member.ID, copiedID: copied.ID, histPath: histPath,
	})
}
