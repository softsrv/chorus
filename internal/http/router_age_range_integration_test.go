//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ageErr is an API error body with its message, which apiError leaves out.
type ageErr struct {
	Error struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	} `json:"error"`
}

// noBirthdayKey fails the test when a decoded JSON object, or any object
// nested in it, carries a birthday key or a bare age key.
func noBirthdayKey(t *testing.T, label string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if strings.Contains(strings.ToLower(k), "birthday") || k == "age" {
				t.Errorf("%s carries %q", label, k)
			}
			noBirthdayKey(t, label, child)
		}
	case []any:
		for _, child := range x {
			noBirthdayKey(t, label, child)
		}
	}
}

// TestAPI_QuestAgeRange drives quest age ranges through the HTTP stack:
// min_age/max_age round-trip on POST/PUT/GET (CLM-2), bad limits are 422 per
// field and save nothing (CLM-3), an out-of-range accept is 409
// age_restricted and leaves the quest in the pool (CLM-12),
// ineligible_member_ids lists exactly the out-of-range members of this
// household (CLM-13), and neither quest nor member bodies ever carry a
// birthday (CLM-6).
func TestAPI_QuestAgeRange(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("ageparent")
	ctx := context.Background()

	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "Ages over HTTP", "timezone": "America/Chicago"}, &h)
	hp := "/api/v1/households/" + h.ID.String()
	var other struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "Other ages over HTTP", "timezone": "America/Chicago"}, &other)

	// Members with ages fixed whatever today's date: a birthday a month past
	// their last one, in the household's timezone.
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	newMember := func(hid uuid.UUID, name string, years int) uuid.UUID {
		t.Helper()
		var m struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", "/api/v1/households/"+hid.String()+"/family-members", parent.AccessToken,
			map[string]string{"name": name}, &m)
		if years >= 0 {
			bd := today.AddDate(-years, 0, -30).Format("2006-01-02")
			if _, err := s.pool.Exec(ctx, `UPDATE family_members SET birthday = $1::date WHERE id = $2`, bd, m.ID); err != nil {
				t.Fatalf("set birthday: %v", err)
			}
		}
		return m.ID
	}
	eight := newMember(h.ID, "Eight", 8)
	twelve := newMember(h.ID, "Twelve", 12)
	nobday := newMember(h.ID, "NoBirthday", -1)
	outsider := newMember(other.ID, "OutsiderFive", 5)

	type ageQuest struct {
		ID         uuid.UUID   `json:"id"`
		Name       string      `json:"name"`
		Available  bool        `json:"available"`
		ClaimedBy  *uuid.UUID  `json:"claimed_by"`
		MinAge     *int32      `json:"min_age"`
		MaxAge     *int32      `json:"max_age"`
		Ineligible []uuid.UUID `json:"ineligible_member_ids"`
	}
	limits := func(q ageQuest) [2]int32 {
		out := [2]int32{-1, -1}
		if q.MinAge != nil {
			out[0] = *q.MinAge
		}
		if q.MaxAge != nil {
			out[1] = *q.MaxAge
		}
		return out
	}

	// CLM-2: each range comes back as sent, null for no bound.
	cases := []struct {
		body map[string]any
		want [2]int32
	}{
		{map[string]any{"name": "Mow the lawn", "min_age": 10, "max_age": 14}, [2]int32{10, 14}},
		{map[string]any{"name": "Read a chapter", "min_age": 6, "max_age": nil}, [2]int32{6, -1}},
		{map[string]any{"name": "Bake", "max_age": 12}, [2]int32{-1, 12}},
		{map[string]any{"name": "Feed the cat"}, [2]int32{-1, -1}},
	}
	created := make([]ageQuest, len(cases))
	for i, c := range cases {
		s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, c.body, &created[i])
		if got := limits(created[i]); got != c.want {
			t.Errorf("POST %v: limits %v, want %v", c.body["name"], got, c.want)
		}
		var got ageQuest
		s.expect(http.StatusOK, "GET", hp+"/quests/"+created[i].ID.String(), parent.AccessToken, nil, &got)
		if limits(got) != c.want {
			t.Errorf("GET %v: limits %v, want %v", c.body["name"], limits(got), c.want)
		}
	}
	mow, open := created[0], created[3]

	// The raw body has the keys: null limits and [] when nobody is left out.
	var raw map[string]json.RawMessage
	s.expect(http.StatusOK, "GET", hp+"/quests/"+open.ID.String(), parent.AccessToken, nil, &raw)
	if string(raw["min_age"]) != "null" || string(raw["max_age"]) != "null" || string(raw["ineligible_member_ids"]) != "[]" {
		t.Errorf("all-ages quest keys: min_age %s max_age %s ineligible %s", raw["min_age"], raw["max_age"], raw["ineligible_member_ids"])
	}

	// PUT replaces the range; a PUT without the fields clears it.
	qp := hp + "/quests/" + created[1].ID.String()
	var put ageQuest
	s.expect(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]any{"name": "Read a chapter", "min_age": 7, "max_age": 9}, &put)
	if limits(put) != [2]int32{7, 9} {
		t.Errorf("PUT 7/9: %v", limits(put))
	}
	s.expect(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]any{"name": "Read a chapter"}, &put)
	if limits(put) != [2]int32{-1, -1} {
		t.Errorf("PUT without limits: %v, want cleared", limits(put))
	}
	s.expect(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]any{"name": "Read a chapter", "min_age": 6}, &put)

	// CLM-3: bad limits are 422 per field, on create and update, and save
	// nothing.
	countQuests := func() int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM household_quests WHERE household_id = $1`, h.ID).Scan(&n); err != nil {
			t.Fatalf("count quests: %v", err)
		}
		return n
	}
	before := countQuests()
	bad := []struct {
		body  map[string]any
		field string
		msg   string
	}{
		{map[string]any{"min_age": 14, "max_age": 10}, "max_age", "Minimum age can't be above the maximum age."},
		{map[string]any{"min_age": -1}, "min_age", ""},
		{map[string]any{"max_age": -1}, "max_age", ""},
		{map[string]any{"min_age": 2.5}, "min_age", ""},
		{map[string]any{"max_age": "ten"}, "max_age", ""},
	}
	for _, c := range bad {
		for _, method := range []string{"POST", "PUT"} {
			body := map[string]any{"name": "Refused", "points": -3}
			for k, v := range c.body {
				body[k] = v
			}
			path := hp + "/quests"
			if method == "PUT" {
				path = hp + "/quests/" + mow.ID.String()
			}
			var e ageErr
			if got := s.do(method, path, parent.AccessToken, body, &e); got != http.StatusUnprocessableEntity {
				t.Errorf("%s %v: status %d, want 422", method, c.body, got)
				continue
			}
			if e.Error.Fields[c.field] == "" || (c.msg != "" && e.Error.Fields[c.field] != c.msg) {
				t.Errorf("%s %v: %s = %q, want %q", method, c.body, c.field, e.Error.Fields[c.field], c.msg)
			}
			if e.Error.Fields["points"] == "" {
				t.Errorf("%s %v: points not reported with the age field: %v", method, c.body, e.Error.Fields)
			}
		}
	}
	if n := countQuests(); n != before {
		t.Errorf("refused creates saved quests: %d -> %d", before, n)
	}
	var unchanged ageQuest
	s.expect(http.StatusOK, "GET", hp+"/quests/"+mow.ID.String(), parent.AccessToken, nil, &unchanged)
	if unchanged.Name != "Mow the lawn" || limits(unchanged) != [2]int32{10, 14} {
		t.Errorf("refused updates changed the quest: %+v", unchanged)
	}

	// CLM-13: only this household's out-of-range member is listed.
	var list struct {
		Items []ageQuest `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &list)
	for _, q := range list.Items {
		switch q.ID {
		case mow.ID:
			if !slices.Equal(q.Ineligible, []uuid.UUID{eight}) {
				t.Errorf("10-14 ineligible = %v, want [Eight]", q.Ineligible)
			}
		case open.ID:
			if q.Ineligible == nil || len(q.Ineligible) != 0 {
				t.Errorf("all-ages ineligible = %v, want []", q.Ineligible)
			}
		}
		if slices.Contains(q.Ineligible, outsider) {
			t.Errorf("%s lists another household's member", q.Name)
		}
	}

	// CLM-12: an out-of-range accept is 409 age_restricted and the quest
	// stays available; the ineligible list matches who Accept refuses.
	ap := hp + "/quests/" + mow.ID.String() + "/accept"
	var refused ageErr
	s.expect(http.StatusConflict, "POST", ap, parent.AccessToken, map[string]any{"member_id": eight}, &refused)
	if refused.Error.Code != "age_restricted" || refused.Error.Message != "This quest is outside that member's age range." {
		t.Errorf("out-of-range accept body: %+v", refused.Error)
	}
	var still ageQuest
	s.expect(http.StatusOK, "GET", hp+"/quests/"+mow.ID.String(), parent.AccessToken, nil, &still)
	if !still.Available || still.ClaimedBy != nil {
		t.Errorf("quest after refused accept: %+v, want available", still)
	}
	// Another household's member is not found, never age_restricted.
	s.expect(http.StatusNotFound, "POST", ap, parent.AccessToken, map[string]any{"member_id": outsider}, nil)
	// A member with no birthday accepts it; the held quest is then taken.
	var accepted ageQuest
	s.expect(http.StatusOK, "POST", ap, parent.AccessToken, map[string]any{"member_id": nobday}, &accepted)
	if accepted.ClaimedBy == nil || *accepted.ClaimedBy != nobday {
		t.Errorf("no-birthday accept: %+v", accepted)
	}
	var taken ageErr
	s.expect(http.StatusConflict, "POST", ap, parent.AccessToken, map[string]any{"member_id": twelve}, &taken)
	if taken.Error.Code != "quest_taken" {
		t.Errorf("accept of a held quest: code %q, want quest_taken", taken.Error.Code)
	}

	// CLM-6: no body sent to a member carries a birthday: the member list
	// and single-member GET, and quest list and get.
	for label, path := range map[string]string{
		"member list": hp + "/family-members",
		"member get":  hp + "/family-members/" + eight.String(),
		"quest list":  hp + "/quests",
		"quest get":   hp + "/quests/" + mow.ID.String(),
	} {
		var body any
		s.expect(http.StatusOK, "GET", path, parent.AccessToken, nil, &body)
		noBirthdayKey(t, label, body)
	}
	var acceptedRaw any
	s.expect(http.StatusOK, "POST", hp+"/quests/"+open.ID.String()+"/accept", parent.AccessToken, map[string]any{"member_id": twelve}, &acceptedRaw)
	noBirthdayKey(t, "accept body", acceptedRaw)
}
