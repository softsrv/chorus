//go:build integration

package http_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type questScheduleWire struct {
	ID         uuid.UUID `json:"id"`
	QuestID    uuid.UUID `json:"quest_id"`
	QuestName  string    `json:"quest_name"`
	MemberID   uuid.UUID `json:"member_id"`
	MemberName string    `json:"member_name"`
	StartDate  string    `json:"start_date"`
}

// TestAPI_QuestSchedules drives the quest-schedule routes through the router
// against Postgres (CLM-6): 201 on a schedule, the list ({items: []} when
// empty), 204 on remove; 403 for a non-admin, a missing admin PIN and a wrong
// one; 404 for a non-member and for a quest, member or schedule of another
// household; 422 on fields.date for a missing, malformed, oversized or past
// date. Every refusal leaves quest_schedules as it was.
func TestAPI_QuestSchedules(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("schedparent")
	ctx := context.Background()

	newHousehold := func(name string) uuid.UUID {
		t.Helper()
		var h struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
			map[string]string{"name": name, "timezone": "America/Chicago"}, &h)
		s.deleteHouseholdAfter(h.ID)
		return h.ID
	}
	hid := newHousehold("Schedules over HTTP")
	otherID := newHousehold("Other schedules over HTTP")
	hp := "/api/v1/households/" + hid.String()
	op := "/api/v1/households/" + otherID.String()

	newQuest := func(p, name string) uuid.UUID {
		t.Helper()
		var q struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", p+"/quests", parent.AccessToken, map[string]any{"name": name, "points": 5}, &q)
		return q.ID
	}
	newMember := func(p, name string) uuid.UUID {
		t.Helper()
		var m struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", p+"/family-members", parent.AccessToken, map[string]string{"name": name}, &m)
		return m.ID
	}
	quest := newQuest(hp, "Feed the cat")
	sam := newMember(hp, "Sam")
	otherQuest := newQuest(op, "Other quest")
	otherMember := newMember(op, "Other kid")

	count := func(h uuid.UUID) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM quest_schedules WHERE household_id = $1`, h).Scan(&n); err != nil {
			t.Fatalf("count quest_schedules: %v", err)
		}
		return n
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	future := time.Now().In(loc).AddDate(0, 0, 30).Format(time.DateOnly)
	body := func(q, m uuid.UUID, date any) map[string]any {
		return map[string]any{"quest_id": q, "member_id": m, "date": date}
	}
	// refused sends one request that must fail with status/code, and checks
	// nothing was written in either household.
	refused := func(label string, status int, code, method, path, token string, headers map[string]string, b any) apiError {
		t.Helper()
		before, otherBefore := count(hid), count(otherID)
		var e apiError
		if got := s.doWithHeaders(method, path, token, headers, b, &e); got != status {
			t.Fatalf("%s: status %d, want %d (%+v)", label, got, status, e)
		}
		if code != "" && e.Error.Code != code {
			t.Errorf("%s: code %q, want %q", label, e.Error.Code, code)
		}
		if after, otherAfter := count(hid), count(otherID); after != before || otherAfter != otherBefore {
			t.Errorf("%s: quest_schedules changed %d→%d, other %d→%d", label, before, after, otherBefore, otherAfter)
		}
		return e
	}

	// An empty household lists {"items": []}, never null.
	var list struct {
		Items []questScheduleWire `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quest-schedules", parent.AccessToken, nil, &list)
	if list.Items == nil || len(list.Items) != 0 {
		t.Fatalf("empty list: %+v", list.Items)
	}

	// A missing, malformed, oversized or past date is 422 on fields.date.
	for _, c := range []struct {
		label string
		date  any
	}{
		{"missing date", nil},
		{"empty date", ""},
		{"blank date", "   "},
		{"malformed date", "21/11/2026"},
		{"impossible date", "2026-02-30"},
		{"oversized date", strings.Repeat("2026-11-21", 1000)},
		{"past date", "2000-01-01"},
		{"yesterday", time.Now().In(loc).AddDate(0, 0, -2).Format(time.DateOnly)},
	} {
		b := body(quest, sam, c.date)
		if c.date == nil {
			b = map[string]any{"quest_id": quest, "member_id": sam}
		}
		e := refused(c.label, http.StatusUnprocessableEntity, "", "POST", hp+"/quest-schedules", parent.AccessToken, nil, b)
		if e.Error.Fields["date"] == "" {
			t.Errorf("%s: no message on fields.date: %+v", c.label, e.Error.Fields)
		}
	}

	// A quest or member of another household is 404, as is a non-member.
	refused("other household's quest", http.StatusNotFound, "not_found", "POST", hp+"/quest-schedules", parent.AccessToken, nil, body(otherQuest, sam, future))
	refused("other household's member", http.StatusNotFound, "not_found", "POST", hp+"/quest-schedules", parent.AccessToken, nil, body(quest, otherMember, future))
	refused("unknown quest", http.StatusNotFound, "not_found", "POST", hp+"/quest-schedules", parent.AccessToken, nil, body(uuid.New(), sam, future))
	stranger := s.signUp("schedstranger")
	refused("non-member create", http.StatusNotFound, "not_found", "POST", hp+"/quest-schedules", stranger.AccessToken, nil, body(quest, sam, future))
	refused("non-member list", http.StatusNotFound, "not_found", "GET", hp+"/quest-schedules", stranger.AccessToken, nil, nil)

	// A household member who is not an admin is refused.
	member := s.signUp("schedmember")
	if _, err := s.pool.Exec(ctx, `INSERT INTO household_members (household_id, user_id, role, joined_at) VALUES ($1, $2, 'member', NOW())`,
		hid, member.User.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	refused("non-admin create", http.StatusForbidden, "", "POST", hp+"/quest-schedules", member.AccessToken, nil, body(quest, sam, future))
	refused("non-admin list", http.StatusForbidden, "", "GET", hp+"/quest-schedules", member.AccessToken, nil, nil)

	// 201: the admin schedules a quest for a member.
	var created questScheduleWire
	s.expect(http.StatusCreated, "POST", hp+"/quest-schedules", parent.AccessToken, body(quest, sam, future), &created)
	if created.ID == uuid.Nil || created.QuestID != quest || created.QuestName != "Feed the cat" ||
		created.MemberID != sam || created.MemberName != "Sam" || created.StartDate != future {
		t.Fatalf("created: %+v", created)
	}
	if n := count(hid); n != 1 {
		t.Fatalf("after create: %d schedules, want 1", n)
	}
	s.expect(http.StatusOK, "GET", hp+"/quest-schedules", parent.AccessToken, nil, &list)
	if len(list.Items) != 1 || list.Items[0] != created {
		t.Fatalf("list after create: %+v", list.Items)
	}
	// The other household's list never shows it.
	s.expect(http.StatusOK, "GET", op+"/quest-schedules", parent.AccessToken, nil, &list)
	if len(list.Items) != 0 {
		t.Fatalf("other household list: %+v", list.Items)
	}
	var otherSched questScheduleWire
	s.expect(http.StatusCreated, "POST", op+"/quest-schedules", parent.AccessToken, body(otherQuest, otherMember, future), &otherSched)

	// Removing an unknown schedule, or another household's, is 404.
	refused("delete unknown", http.StatusNotFound, "not_found", "DELETE", hp+"/quest-schedules/"+uuid.NewString(), parent.AccessToken, nil, nil)
	refused("delete other household's", http.StatusNotFound, "not_found", "DELETE", hp+"/quest-schedules/"+otherSched.ID.String(), parent.AccessToken, nil, nil)
	refused("non-admin delete", http.StatusForbidden, "", "DELETE", hp+"/quest-schedules/"+created.ID.String(), member.AccessToken, nil, nil)

	// Once an admin PIN is set, every route needs it.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", parent.AccessToken, map[string]string{"pin": "4321"}, nil)
	wrong := map[string]string{"X-Admin-PIN": "9999"}
	right := map[string]string{"X-Admin-PIN": "4321"}
	for _, c := range []struct {
		label, method, path string
		b                   any
	}{
		{"create", "POST", hp + "/quest-schedules", body(quest, sam, future)},
		{"list", "GET", hp + "/quest-schedules", nil},
		{"delete", "DELETE", hp + "/quest-schedules/" + created.ID.String(), nil},
	} {
		refused(c.label+" without PIN", http.StatusForbidden, "admin_pin_required", c.method, c.path, parent.AccessToken, nil, c.b)
		refused(c.label+" with wrong PIN", http.StatusForbidden, "wrong_pin", c.method, c.path, parent.AccessToken, wrong, c.b)
	}
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quest-schedules", parent.AccessToken, right, nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("list with PIN: %+v", list.Items)
	}

	// 204: removed; the list is empty again and a second remove is 404.
	s.expectWithHeaders(http.StatusNoContent, "DELETE", hp+"/quest-schedules/"+created.ID.String(), parent.AccessToken, right, nil, nil)
	if n := count(hid); n != 0 {
		t.Fatalf("after delete: %d schedules, want 0", n)
	}
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quest-schedules", parent.AccessToken, right, nil, &list)
	if list.Items == nil || len(list.Items) != 0 {
		t.Fatalf("list after delete: %+v", list.Items)
	}
	refused("delete twice", http.StatusNotFound, "not_found", "DELETE", hp+"/quest-schedules/"+created.ID.String(), parent.AccessToken, right, nil)
	if n := count(otherID); n != 1 {
		t.Fatalf("other household schedules: %d, want 1", n)
	}
}
