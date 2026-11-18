//go:build integration

package http_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// birthdayItem is one member's birthday as the two admin-PIN-gated birthday
// routes return it.
type birthdayItem struct {
	MemberID uuid.UUID `json:"member_id"`
	Birthday *string   `json:"birthday"`
	Age      *int32    `json:"age"`
}

// TestAPI_MemberBirthdays drives the birthday routes through the full HTTP
// stack: PUT .../{memberID}/birthday sets and clears a birthday and answers
// {member_id, birthday, age} with the age from quest_member_age in the
// household's timezone (CLM-4); GET .../family-members/birthdays lists this
// household's members only, and is never read as a member id (CLM-5). With
// an admin PIN set both answer 403 admin_pin_required without it and 403
// wrong_pin with a wrong one; a bad date is 422 on birthday; a user outside
// the household gets 404; and the member list and get bodies carry no
// birthday.
func TestAPI_MemberBirthdays(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("bdayparent")
	outsider := s.signUp("bdayoutsider")
	ctx := context.Background()

	const tz = "America/Chicago"
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "Birthdays over HTTP", "timezone": tz}, &h)
	hp := "/api/v1/households/" + h.ID.String()
	var other struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "Other birthdays", "timezone": tz}, &other)

	var sam, ava, otherKid struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken, map[string]string{"name": "Sam"}, &sam)
	s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken, map[string]string{"name": "Ava"}, &ava)
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+other.ID.String()+"/family-members", parent.AccessToken,
		map[string]string{"name": "OtherKid"}, &otherKid)

	sqlAge := func(birthday string) int32 {
		t.Helper()
		var age int32
		if err := s.pool.QueryRow(ctx, "SELECT quest_member_age($1::date, $2)", birthday, tz).Scan(&age); err != nil {
			t.Fatalf("quest_member_age: %v", err)
		}
		return age
	}
	stored := func(id uuid.UUID) string {
		t.Helper()
		var v *string
		if err := s.pool.QueryRow(ctx, "SELECT to_char(birthday, 'YYYY-MM-DD') FROM family_members WHERE id = $1", id).Scan(&v); err != nil {
			t.Fatalf("read birthday: %v", err)
		}
		if v == nil {
			return ""
		}
		return *v
	}

	// Set the admin PIN; from here on both routes need it.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", parent.AccessToken, map[string]string{"pin": "2468"}, nil)
	good := map[string]string{"X-Admin-PIN": "2468"}
	wrong := map[string]string{"X-Admin-PIN": "1111"}

	// Set: 200 {member_id, birthday, age} with the SQL age.
	const bd = "2015-04-02"
	var set birthdayItem
	s.expectWithHeaders(http.StatusOK, "PUT", hp+"/family-members/"+sam.ID.String()+"/birthday", parent.AccessToken, good,
		map[string]any{"birthday": bd}, &set)
	if set.MemberID != sam.ID || set.Birthday == nil || *set.Birthday != bd || set.Age == nil || *set.Age != sqlAge(bd) {
		t.Errorf("PUT birthday = %+v, want Sam %s age %d", set, bd, sqlAge(bd))
	}
	if got := stored(sam.ID); got != bd {
		t.Errorf("stored = %q, want %q", got, bd)
	}

	// Clear with null: 200 with null birthday and age.
	var cleared map[string]any
	s.expectWithHeaders(http.StatusOK, "PUT", hp+"/family-members/"+ava.ID.String()+"/birthday", parent.AccessToken, good,
		map[string]any{"birthday": "2016-06-06"}, nil)
	s.expectWithHeaders(http.StatusOK, "PUT", hp+"/family-members/"+ava.ID.String()+"/birthday", parent.AccessToken, good,
		map[string]any{"birthday": nil}, &cleared)
	if v, ok := cleared["birthday"]; !ok || v != nil {
		t.Errorf("cleared birthday = %v (present %v), want null", v, ok)
	}
	if v, ok := cleared["age"]; !ok || v != nil {
		t.Errorf("cleared age = %v (present %v), want null", v, ok)
	}
	if got := stored(ava.ID); got != "" {
		t.Errorf("Ava stored after clear = %q, want unset", got)
	}

	// GET birthdays: this household's members only, Ava with nulls. The
	// literal path is served by the list route, not read as a member id.
	var list struct{ Items []birthdayItem }
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/family-members/birthdays", parent.AccessToken, good, nil, &list)
	if len(list.Items) != 2 {
		t.Fatalf("GET birthdays = %+v, want 2 items", list.Items)
	}
	for _, it := range list.Items {
		switch it.MemberID {
		case sam.ID:
			if it.Birthday == nil || *it.Birthday != bd || it.Age == nil || *it.Age != sqlAge(bd) {
				t.Errorf("Sam = %+v", it)
			}
		case ava.ID:
			if it.Birthday != nil || it.Age != nil {
				t.Errorf("Ava = %+v, want null birthday and age", it)
			}
		default:
			t.Errorf("GET birthdays lists %s, not a member of this household", it.MemberID)
		}
	}

	// Admin PIN gate on both routes; refusals change nothing.
	samBday := hp + "/family-members/" + sam.ID.String() + "/birthday"
	for _, c := range []struct {
		name    string
		headers map[string]string
		code    string
	}{{"no PIN", nil, "admin_pin_required"}, {"wrong PIN", wrong, "wrong_pin"}} {
		var e apiError
		if got := s.doWithHeaders("PUT", samBday, parent.AccessToken, c.headers, map[string]any{"birthday": "2010-01-01"}, &e); got != http.StatusForbidden || e.Error.Code != c.code {
			t.Errorf("PUT %s: %d %q, want 403 %s", c.name, got, e.Error.Code, c.code)
		}
		e = apiError{}
		if got := s.doWithHeaders("GET", hp+"/family-members/birthdays", parent.AccessToken, c.headers, nil, &e); got != http.StatusForbidden || e.Error.Code != c.code {
			t.Errorf("GET %s: %d %q, want 403 %s", c.name, got, e.Error.Code, c.code)
		}
	}
	if got := stored(sam.ID); got != bd {
		t.Errorf("stored after PIN refusals = %q, want %q", got, bd)
	}

	// Bad dates: 422 naming birthday, nothing saved.
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Now().In(loc)
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	for _, bad := range []string{tomorrow, "", "2015-02-30", "04/02/2015"} {
		var e apiError
		if got := s.doWithHeaders("PUT", samBday, parent.AccessToken, good, map[string]any{"birthday": bad}, &e); got != http.StatusUnprocessableEntity || e.Error.Fields["birthday"] == "" {
			t.Errorf("PUT birthday %q: %d %+v, want 422 on birthday", bad, got, e)
		}
	}
	if got := stored(sam.ID); got != bd {
		t.Errorf("stored after 422s = %q, want %q", got, bd)
	}

	// Another household's member through this household: 404, unchanged.
	var e apiError
	if got := s.doWithHeaders("PUT", hp+"/family-members/"+otherKid.ID.String()+"/birthday", parent.AccessToken, good,
		map[string]any{"birthday": bd}, &e); got != http.StatusNotFound {
		t.Errorf("PUT another household's member: %d, want 404", got)
	}
	if got := stored(otherKid.ID); got != "" {
		t.Errorf("other household's member birthday = %q, want unset", got)
	}

	// A user outside the household: 404 on both, never 403.
	if got := s.doWithHeaders("GET", hp+"/family-members/birthdays", outsider.AccessToken, good, nil, nil); got != http.StatusNotFound {
		t.Errorf("outsider GET birthdays: %d, want 404", got)
	}
	if got := s.doWithHeaders("PUT", samBday, outsider.AccessToken, good, map[string]any{"birthday": "2001-01-01"}, nil); got != http.StatusNotFound {
		t.Errorf("outsider PUT birthday: %d, want 404", got)
	}
	if got := stored(sam.ID); got != bd {
		t.Errorf("stored after outsider = %q, want %q", got, bd)
	}

	// Member reads never carry a birthday, with or without the admin PIN.
	for _, hdr := range []map[string]string{nil, good} {
		var members any
		s.expectWithHeaders(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, hdr, nil, &members)
		noBirthdayKey(t, "family member list", members)
		var one any
		s.expectWithHeaders(http.StatusOK, "GET", hp+"/family-members/"+sam.ID.String(), parent.AccessToken, hdr, nil, &one)
		noBirthdayKey(t, "family member get", one)
	}
}
