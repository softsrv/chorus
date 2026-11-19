//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ── Entry tags ────────────────────────────────────────────────────────────────

// tagRoute is the tag endpoint: outside /households/{id}/events.
func tagRoute(hp, eventID string) string {
	return hp + "/calendar/entries/" + url.PathEscape(eventID) + "/tags"
}

func (s *calendarServer) tag(hp, token, eventID string, ids ...uuid.UUID) (int, string) {
	s.t.Helper()
	return s.doRaw("PUT", tagRoute(hp, eventID), token, map[string]any{"tags": append([]uuid.UUID{}, ids...)})
}

type rawResponse struct {
	status     int
	body       string
	retryAfter string
}

// tagResp is tag that also returns the Retry-After header.
func (s *calendarServer) tagResp(hp, token, eventID string, ids ...uuid.UUID) rawResponse {
	s.t.Helper()
	return s.doResp("PUT", tagRoute(hp, eventID), token, map[string]any{"tags": append([]uuid.UUID{}, ids...)})
}

// doResp is doRaw that also returns the Retry-After header.
func (s *calendarServer) doResp(method, path, token string, body any) rawResponse {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return rawResponse{status: resp.StatusCode, body: string(b), retryAfter: resp.Header.Get("Retry-After")}
}

// tagMemberOut is one tag choice as the listing returns it.
type tagMemberOut struct {
	MemberID uuid.UUID `json:"member_id"`
	Name     string    `json:"name"`
	Taggable bool      `json:"taggable"`
	Reason   string    `json:"reason"`
}

func (s *calendarServer) addFamilyMember(householdID uuid.UUID, name string) uuid.UUID {
	s.t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		s.t.Fatalf("uuid: %v", err)
	}
	_, err = s.pool.Exec(context.Background(),
		"INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, $3)",
		id, householdID, name)
	if err != nil {
		s.t.Fatalf("insert family_member: %v", err)
	}
	return id
}

type taggedEntries struct {
	Status  string         `json:"status"`
	Items   []taggedEntry  `json:"items"`
	Members []tagMemberOut `json:"members"`
}

func (s *calendarServer) taggedEntries(hp, token string) taggedEntries {
	s.t.Helper()
	var out taggedEntries
	s.expect(http.StatusOK, "GET", hp+"/events?"+rangeQuery(novFrom, novTo), token, nil, &out)
	return out
}

func (e taggedEntries) find(id string) taggedEntry {
	for _, it := range e.Items {
		if it.ID == id {
			return it
		}
	}
	return taggedEntry{}
}

// mutating returns the Calendar API requests that are not GETs.
func mutating(reqs []googleRequest) []googleRequest {
	var out []googleRequest
	for _, r := range reqs {
		if strings.HasPrefix(r.Path, "/calendar/v3/") && r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func dentist() map[string]any {
	return map[string]any{
		"id": "E", "summary": "Dentist", "location": "Main St 4", "description": "Bring the forms",
		"start":     map[string]any{"dateTime": "2026-11-12T09:00:00Z"},
		"end":       map[string]any{"dateTime": "2026-11-12T10:00:00Z"},
		"attendees": []any{map[string]any{"email": "mom@example.com"}, map[string]any{"email": "sam@example.com"}},
	}
}

// preservedFields are the entry fields a tag change must leave as they were.
var preservedFields = []string{"start", "end", "location", "description", "attendees"}

// tagChangeProblems is CLM-2's check: the only mutating request is one
// PATCH whose JSON key set is exactly {summary}, and the preserved fields
// deep-equal their prior values.
func tagChangeProblems(reqs []googleRequest, before, after map[string]any) []string {
	var problems []string
	mut := mutating(reqs)
	if len(mut) != 1 || mut[0].Method != http.MethodPatch {
		problems = append(problems, fmt.Sprintf("mutating requests = %+v, want exactly one PATCH", mut))
	}
	for _, r := range mut {
		var body map[string]any
		_ = json.Unmarshal([]byte(r.Body), &body)
		if keys := slices.Sorted(maps.Keys(body)); !slices.Equal(keys, []string{"summary"}) {
			problems = append(problems, fmt.Sprintf("%s body keys = %v, want [summary]", r.Method, keys))
		}
	}
	for _, k := range preservedFields {
		if !reflect.DeepEqual(before[k], after[k]) {
			problems = append(problems, fmt.Sprintf("%s changed: %v -> %v", k, before[k], after[k]))
		}
	}
	return problems
}

// tagFixture is household H whose one login is Mom (calendar Y, read_write),
// with family members Mom and Sam, Y as the family calendar and entry E
// "Dentist" on it. outsider is a login with its own household, outside H.
func tagFixture(t *testing.T) (*calendarServer, *fakeFamilyGoogle, familyMember, familyMember, string, uuid.UUID, uuid.UUID) {
	t.Helper()
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	mom := s.member(fake, "tagmom", calY)
	outsider := s.member(fake, "tagoutsider")
	hp := s.household(mom)
	hid, err := uuid.Parse(strings.TrimPrefix(hp, "/api/v1/households/"))
	if err != nil {
		t.Fatalf("parse household id: %v", err)
	}
	momMemberID := s.addFamilyMember(hid, "Mom")
	samMemberID := s.addFamilyMember(hid, "Sam")
	fake.set(func(f *fakeFamilyGoogle) { f.events[calY.ID] = []map[string]any{dentist()} })
	if code, _ := s.setFamily(hp, mom.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}
	return s, fake, mom, outsider, hp, momMemberID, samMemberID
}

// CLM-1, CLM-2, CLM-3, CLM-4: the household's login tags E with family
// members through the tag route; Chorus PATCHes only the summary
// 'Dentist [Mom, Sam]' with the connector's token and answers with Google's
// title; the listing reads tags from Google's title; untagging ends at
// 'Dentist'; a title changed at Google shows as is. An outsider login gets
// 404 with no Google request.
func TestFamilyCalendar_TagEntry(t *testing.T) {
	s, fake, mom, outsider, hp, momID, samID := tagFixture(t)
	before := fake.entry(calY.ID, "E")

	n := len(fake.recorded())
	s.expectOutsider("PUT", tagRoute(hp, "E"), outsider.AccessToken, map[string]any{"tags": []uuid.UUID{momID}})
	if reqs := fake.since(n); len(reqs) != 0 {
		t.Errorf("outsider's tag reached Google: %+v", reqs)
	}

	n = len(fake.recorded())
	code, raw := s.tag(hp, mom.AccessToken, "E", momID, samID)
	if code != http.StatusOK {
		t.Fatalf("tag: %d %s", code, raw)
	}
	var res struct {
		ID     string      `json:"id"`
		Title  string      `json:"title"`
		Tags   []uuid.UUID `json:"tags"`
		CanTag bool        `json:"can_tag"`
	}
	_ = json.Unmarshal([]byte(raw), &res)
	after := fake.entry(calY.ID, "E")
	reqs := fake.since(n)
	patches := mutating(reqs)
	wantPath := "/calendar/v3/calendars/" + url.PathEscape(calY.ID) + "/events/E"
	if len(patches) != 1 || patches[0].Path != wantPath || patches[0].Auth != mom.googleAccess {
		t.Fatalf("patches = %+v, want one PATCH to %s with Mom's token", patches, wantPath)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(patches[0].Body), &sent)
	if sent["summary"] != "Dentist [Mom, Sam]" {
		t.Errorf("PATCH summary = %v, want 'Dentist [Mom, Sam]'", sent["summary"])
	}
	if res.ID != "E" || res.Title != after["summary"] || res.Title != "Dentist [Mom, Sam]" ||
		!slices.Equal(res.Tags, []uuid.UUID{momID, samID}) || !res.CanTag {
		t.Errorf("response = %+v, want E with Google's title 'Dentist [Mom, Sam]' and both tags", res)
	}
	// CLM-2: only the summary went to Google; the other fields are intact.
	if problems := tagChangeProblems(reqs, before, after); len(problems) > 0 {
		t.Errorf("tag change touched more than the title: %v", problems)
	}

	// CLM-3: the listing shows Google's title with the parsed tags.
	list := s.taggedEntries(hp, mom.AccessToken)
	e := list.find("E")
	if e.Title != "Dentist [Mom, Sam]" || !slices.Equal(e.Tags, []uuid.UUID{momID, samID}) || !e.CanTag {
		t.Errorf("listed E = %+v, want Google's title, tags [Mom Sam], can_tag", e)
	}
	if len(list.Members) != 2 || !list.Members[0].Taggable || !list.Members[1].Taggable {
		t.Errorf("members = %+v, want Mom and Sam taggable", list.Members)
	}
	if list.Members[0].MemberID != momID || list.Members[0].Name != "Mom" ||
		list.Members[1].MemberID != samID || list.Members[1].Name != "Sam" {
		t.Errorf("members content = %+v, want Mom(%s) and Sam(%s)", list.Members, momID, samID)
	}

	// CLM-4: untagging leaves the bare base title.
	n = len(fake.recorded())
	if code, raw := s.tag(hp, mom.AccessToken, "E"); code != http.StatusOK {
		t.Fatalf("untag: %d %s", code, raw)
	}
	patches = mutating(fake.since(n))
	if len(patches) != 1 || !strings.Contains(patches[0].Body, `"summary":"Dentist"`) {
		t.Errorf("untag patches = %+v, want one PATCH with summary 'Dentist'", patches)
	}
	if got := fake.entry(calY.ID, "E")["summary"]; got != "Dentist" {
		t.Errorf("summary after untag = %v, want Dentist", got)
	}

	// CLM-3: Chorus keeps no title of its own; a change at Google shows.
	fake.set(func(f *fakeFamilyGoogle) { f.item(calY.ID, "E")["summary"] = "Dentist moved [sam]" })
	e = s.taggedEntries(hp, mom.AccessToken).find("E")
	if e.Title != "Dentist moved [sam]" || !slices.Equal(e.Tags, []uuid.UUID{samID}) {
		t.Errorf("listed E after Google-side change = %+v, want 'Dentist moved [sam]' tagged Sam", e)
	}
	// An outsider cannot read the tagged listing either.
	s.expectOutsider("GET", hp+"/events?"+rangeQuery(novFrom, novTo), outsider.AccessToken, nil)

	// The tag route is not under the retired /events editing routes.
	if code := s.do("PUT", hp+"/events/E/tags", mom.AccessToken, map[string]any{"tags": []uuid.UUID{}}, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /events/E/tags: %d, want 404/405", code)
	}
}

// CLM-2 positive control: the check above turns red on a full-body PUT that
// replaces the entry and drops an attendee.
func TestFamilyCalendar_TagCheckCatchesFullUpdate(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	fake.set(func(f *fakeFamilyGoogle) {
		f.calendars["tok"] = []fakeGoogleCal{calY}
		f.events[calY.ID] = []map[string]any{dentist()}
	})
	before := fake.entry(calY.ID, "E")
	full := dentist()
	full["summary"] = "Dentist [Mom]"
	full["attendees"] = []any{map[string]any{"email": "mom@example.com"}}
	b, _ := json.Marshal(full)
	req, _ := http.NewRequest(http.MethodPut, fake.srv.URL+"/calendar/v3/calendars/"+url.PathEscape(calY.ID)+"/events/E", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if problems := tagChangeProblems(fake.recorded(), before, fake.entry(calY.ID, "E")); len(problems) == 0 {
		t.Fatal("a full-body PUT changing attendees passed the summary-only check")
	}
}

// CLM-5: Google refusing the PATCH maps to 404 / 403 / 502 with a fixed body
// and none of Google's text; a rate limit or a Google outage is a temporary
// 503 with Retry-After, and a 401 is a 403 like the listing's broken. The
// title at Google and in the listing stays.
func TestFamilyCalendar_TagGoogleRefusal(t *testing.T) {
	s, fake, mom, _, hp, momID, _ := tagFixture(t)
	const rateLimit403 = `{"error":{"code":403,"message":"INTERNAL-SENTINEL","errors":[{"domain":"usageLimits","reason":"rateLimitExceeded"}]}}`
	const dailyLimit403 = `{"error":{"code":403,"message":"INTERNAL-SENTINEL","errors":[{"domain":"usageLimits","reason":"dailyLimitExceeded"}]}}`
	const forbidden403 = `{"error":{"code":403,"message":"INTERNAL-SENTINEL","errors":[{"domain":"global","reason":"insufficientPermissions"}]}}`
	for _, tc := range []struct {
		googleStatus, want int
		code               string
		body               string
	}{
		{http.StatusNotFound, http.StatusNotFound, "entry_gone", ""},
		{http.StatusGone, http.StatusNotFound, "entry_gone", ""},
		{http.StatusForbidden, http.StatusForbidden, "not_editable", ""},
		{http.StatusForbidden, http.StatusServiceUnavailable, "google_unavailable", rateLimit403},
		{http.StatusForbidden, http.StatusServiceUnavailable, "google_unavailable", dailyLimit403},
		{http.StatusForbidden, http.StatusForbidden, "not_editable", forbidden403},
		{http.StatusUnauthorized, http.StatusForbidden, "forbidden", ""},
		{http.StatusTooManyRequests, http.StatusServiceUnavailable, "google_unavailable", ""},
		{http.StatusInternalServerError, http.StatusServiceUnavailable, "google_unavailable", ""},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable, "google_unavailable", ""},
		{http.StatusBadRequest, http.StatusBadGateway, "google_rejected", ""},
	} {
		body := tc.body
		if body == "" {
			body = "INTERNAL-SENTINEL"
		}
		fake.set(func(f *fakeFamilyGoogle) { f.patchStatus, f.patchBody = tc.googleStatus, body })
		n := len(fake.recorded())
		resp := s.tagResp(hp, mom.AccessToken, "E", momID)
		status, raw := resp.status, resp.body
		var e apiError
		_ = json.Unmarshal([]byte(raw), &e)
		if status != tc.want || e.Error.Code != tc.code {
			t.Errorf("Google %d: %d %s, want %d %s", tc.googleStatus, status, raw, tc.want, tc.code)
		}
		if (tc.want == http.StatusServiceUnavailable) != (resp.retryAfter != "") {
			t.Errorf("Google %d: Retry-After %q on %d", tc.googleStatus, resp.retryAfter, status)
		}
		for _, leak := range []string{"INTERNAL-SENTINEL", "google calendar api", "status " + strconv.Itoa(tc.googleStatus), "Dentist [Mom]"} {
			if strings.Contains(raw, leak) {
				t.Errorf("Google %d: response %s leaks %q", tc.googleStatus, raw, leak)
			}
		}
		if len(mutating(fake.since(n))) != 1 {
			t.Errorf("Google %d: want the one refused PATCH", tc.googleStatus)
		}
		if got := fake.entry(calY.ID, "E")["summary"]; got != "Dentist" {
			t.Errorf("Google %d: summary at Google = %v, want Dentist", tc.googleStatus, got)
		}
		fake.set(func(f *fakeFamilyGoogle) { f.patchStatus = 0 })
		if e := s.taggedEntries(hp, mom.AccessToken).find("E"); e.Title != "Dentist" || len(e.Tags) != 0 {
			t.Errorf("Google %d: listed E = %+v, want Google's unchanged 'Dentist'", tc.googleStatus, e)
		}
	}
}

// CLM-6: can_tag needs a read_write connection AND an entry Google reports
// editable; otherwise every entry is can_tag=false and a tag is 403 with no
// PATCH. A writable calendar is the positive control.
func TestFamilyCalendar_CanTag(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	for _, tc := range []struct {
		name, level, role string
		want              bool
	}{
		{"read_only connection", "read_only", "owner", false},
		{"reader calendar", "read_write", "reader", false},
		{"writable calendar", "read_write", "writer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cal := fakeGoogleCal{ID: strings.ReplaceAll(tc.name, " ", "-") + "@group.calendar.google.com", Summary: "Family"}
			m := s.member(fake, "cantag"+strconv.Itoa(len(tc.name)), cal)
			hp := s.household(m)
			hid, err := uuid.Parse(strings.TrimPrefix(hp, "/api/v1/households/"))
			if err != nil {
				t.Fatalf("parse hid: %v", err)
			}
			fID := s.addFamilyMember(hid, "FamilyMember")
			fake.set(func(f *fakeFamilyGoogle) {
				f.roles[cal.ID] = tc.role
				e2 := dentist()
				e2["id"] = "E2"
				f.events[cal.ID] = []map[string]any{dentist(), e2}
			})
			if code, _ := s.setFamily(hp, m.AccessToken, cal.ID); code != http.StatusOK {
				t.Fatalf("set: %d", code)
			}
			s.seedConnection(m.User.ID, m.googleAccess, "refresh", tc.level, time.Now().Add(time.Hour), false)

			list := s.taggedEntries(hp, m.AccessToken)
			if list.Status != "ok" || len(list.Items) != 2 {
				t.Fatalf("entries = %+v", list)
			}
			for _, e := range list.Items {
				if e.CanTag != tc.want {
					t.Errorf("entry %s can_tag = %v, want %v", e.ID, e.CanTag, tc.want)
				}
			}
			n := len(fake.recorded())
			code, raw := s.tag(hp, m.AccessToken, "E", fID)
			reqs := fake.since(n)
			if !tc.want {
				if code != http.StatusForbidden {
					t.Errorf("tag: %d %s, want 403", code, raw)
				}
				if mut := mutating(reqs); len(mut) != 0 {
					t.Errorf("refused tag reached Google with %+v", mut)
				}
				if tc.level == "read_only" && len(reqs) != 0 {
					t.Errorf("read_only tag made Google requests %+v, want none", reqs)
				}
				return
			}
			if code != http.StatusOK || len(mutating(reqs)) != 1 {
				t.Errorf("writable tag: %d %s, %d PATCH; want 200 and one PATCH", code, raw, len(mutating(reqs)))
			}
		})
	}
}

// CLM-7: the household's login tags family members (the PATCH uses the
// connector's token); tagging a member of another household, a
// duplicate-named member or a name with ',' is 422 with no Google request;
// the members data marks them untaggable. The other household's login gets
// 404 on this household's tag route.
func TestFamilyCalendar_TagMembers(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	// A second router on the same database: sign-up is limited per instance
	// and its tokens are valid on both.
	s2 := newFamilyTestServer(t, fake)
	a := s.member(fake, "tagconn", calY)
	hp := s.household(a)
	hid, err := uuid.Parse(strings.TrimPrefix(hp, "/api/v1/households/"))
	if err != nil {
		t.Fatalf("parse hid: %v", err)
	}

	cUser := s2.member(fake, "tagoutsider")
	hpOutsider := s.household(cUser)
	hidOutsider, err := uuid.Parse(strings.TrimPrefix(hpOutsider, "/api/v1/households/"))
	if err != nil {
		t.Fatalf("parse outsider hid: %v", err)
	}

	aMemberID := s.addFamilyMember(hid, "Alex")
	bMemberID := s.addFamilyMember(hid, "Bea")
	d1MemberID := s.addFamilyMember(hid, "Sam")
	d2MemberID := s.addFamilyMember(hid, "sam ")
	eMemberID := s.addFamilyMember(hid, "Smith, Jo")
	cMemberID := s.addFamilyMember(hidOutsider, "Cy")

	fake.set(func(f *fakeFamilyGoogle) { f.events[calY.ID] = []map[string]any{dentist()} })
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}

	n := len(fake.recorded())
	if code, raw := s.tag(hp, a.AccessToken, "E", aMemberID); code != http.StatusOK || !strings.Contains(raw, `"title":"Dentist [Alex]"`) {
		t.Fatalf("A tags Alex: %d %s", code, raw)
	}
	if p := mutating(fake.since(n)); len(p) != 1 || p[0].Auth != a.googleAccess {
		t.Errorf("A's tag PATCH = %+v, want one with A's token", p)
	}

	// The other household's login cannot tag here, even its own member.
	n = len(fake.recorded())
	s.expectOutsider("PUT", tagRoute(hp, "E"), cUser.AccessToken, map[string]any{"tags": []uuid.UUID{cMemberID}})
	if reqs := fake.since(n); len(reqs) != 0 {
		t.Errorf("outsider login's tag reached Google: %+v", reqs)
	}

	for name, id := range map[string]uuid.UUID{"outsider C": cMemberID, "D1 (duplicate name)": d1MemberID, "D2 (duplicate name)": d2MemberID, "E (comma)": eMemberID} {
		n := len(fake.recorded())
		code, raw := s.tag(hp, a.AccessToken, "E", aMemberID, id)
		var ae apiError
		_ = json.Unmarshal([]byte(raw), &ae)
		if code != http.StatusUnprocessableEntity || ae.Error.Fields["tags"] == "" {
			t.Errorf("tag %s: %d %s, want 422 with a tags error", name, code, raw)
		}
		if reqs := fake.since(n); len(reqs) != 0 {
			t.Errorf("tag %s reached Google: %+v", name, reqs)
		}
	}
	if got := fake.entry(calY.ID, "E")["summary"]; got != "Dentist [Alex]" {
		t.Errorf("summary = %v, want 'Dentist [Alex]' (no refused name written)", got)
	}

	want := map[uuid.UUID]string{aMemberID: "", bMemberID: "", d1MemberID: "duplicate_name", d2MemberID: "duplicate_name", eMemberID: "reserved_character"}
	list := s.taggedEntries(hp, a.AccessToken)
	if len(list.Members) != len(want) {
		t.Fatalf("members = %+v, want %d", list.Members, len(want))
	}
	for _, m := range list.Members {
		r, ok := want[m.MemberID]
		if !ok || m.Reason != r || m.Taggable != (r == "") {
			t.Errorf("member %q: taggable=%v reason=%q, want reason %q", m.Name, m.Taggable, m.Reason, r)
		}
	}
}
