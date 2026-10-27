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
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

// googleRequest is one request the family-calendar stand-in received.
type googleRequest struct {
	Method string
	Path   string // escaped path, e.g. /calendars/Y%40group/events
	Query  url.Values
	Auth   string // the bearer token, without "Bearer "
	Body   string // the raw request body
}

type fakeGoogleCal struct {
	ID, Summary string
	Primary     bool
}

// fakeFamilyGoogle stands in for Google's token endpoint and the Calendar
// API: calendarList per access token, events per calendar id, single-event
// GET, and PATCH (merging the body into the stored entry, like Google) or
// PUT (replacing it). It records every request it receives.
type fakeFamilyGoogle struct {
	srv *httptest.Server

	mu           sync.Mutex
	requests     []googleRequest
	calendars    map[string][]fakeGoogleCal // access token → calendar list
	events       map[string][]map[string]any
	roles        map[string]string // calendar id → accessRole; default owner
	refreshError string            // "" = refresh succeeds
	patchStatus  int               // non-zero: PATCH answers this status
	patchBody    string            // ... with this body
	listStatus   int               // non-zero: events.list answers this status
	listBody     string            // ... with this body
}

func newFakeFamilyGoogle(t *testing.T) *fakeFamilyGoogle {
	t.Helper()
	f := &fakeFamilyGoogle{calendars: map[string][]fakeGoogleCal{}, events: map[string][]map[string]any{}, roles: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFamilyGoogle) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method == http.MethodPatch || r.Method == http.MethodPut {
		body, _ = io.ReadAll(r.Body)
	} else {
		_ = r.ParseForm()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.requests = append(f.requests, googleRequest{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.Query(), Auth: auth, Body: string(body)})
	w.Header().Set("Content-Type", "application/json")
	const eventsPrefix = "/calendar/v3/calendars/"

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/calendar/v3/users/me/calendarList/"):
		calID := strings.TrimPrefix(r.URL.Path, "/calendar/v3/users/me/calendarList/")
		if !f.owns(auth, calID) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": calID, "accessRole": f.role(calID)})
	case strings.HasPrefix(r.URL.Path, eventsPrefix) && strings.Contains(r.URL.Path, "/events/"):
		calID, eventID, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, eventsPrefix), "/events/")
		item := f.item(calID, eventID)
		if !f.owns(auth, calID) || item == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			if f.patchStatus != 0 {
				w.WriteHeader(f.patchStatus)
				_, _ = io.WriteString(w, f.patchBody)
				return
			}
			var patch map[string]any
			if err := json.Unmarshal(body, &patch); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			maps.Copy(item, patch)
		case http.MethodPut:
			var full map[string]any
			if err := json.Unmarshal(body, &full); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			clear(item)
			maps.Copy(item, full)
			item["id"] = eventID
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(item)
	case r.Method == http.MethodPost && r.URL.Path == "/token":
		if f.refreshError != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, f.refreshError)
			return
		}
		access := "refreshed-" + uuid.NewString()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600})
	case r.Method == http.MethodPost && r.URL.Path == "/revoke":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/calendar/v3/users/me/calendarList":
		cals, ok := f.calendars[auth]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		items := []map[string]any{}
		for _, c := range cals {
			items = append(items, map[string]any{"id": c.ID, "summary": c.Summary, "primary": c.Primary})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/calendar/v3/calendars/") && strings.HasSuffix(r.URL.Path, "/events"):
		calID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/calendar/v3/calendars/"), "/events")
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			_, _ = io.WriteString(w, f.listBody)
			return
		}
		owned := false
		for _, c := range f.calendars[auth] {
			owned = owned || c.ID == calID
		}
		if !owned {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		items := f.events[calID]
		if items == nil {
			items = []map[string]any{}
		}
		// Page in twos so the client must follow nextPageToken.
		start := 0
		if pt := r.URL.Query().Get("pageToken"); pt != "" {
			fmt.Sscanf(pt, "p%d", &start)
		}
		end := min(start+2, len(items))
		resp := map[string]any{"items": items[start:end], "accessRole": f.role(calID)}
		if end < len(items) {
			resp["nextPageToken"] = fmt.Sprintf("p%d", end)
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeFamilyGoogle) owns(auth, calID string) bool {
	for _, c := range f.calendars[auth] {
		if c.ID == calID {
			return true
		}
	}
	return false
}

func (f *fakeFamilyGoogle) role(calID string) string {
	if r, ok := f.roles[calID]; ok {
		return r
	}
	return "owner"
}

// item returns the stored entry (callers hold mu).
func (f *fakeFamilyGoogle) item(calID, eventID string) map[string]any {
	for _, it := range f.events[calID] {
		if it["id"] == eventID {
			return it
		}
	}
	return nil
}

// entry returns a deep copy of a stored entry.
func (f *fakeFamilyGoogle) entry(calID, eventID string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out map[string]any
	b, _ := json.Marshal(f.item(calID, eventID))
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *fakeFamilyGoogle) set(fn func(f *fakeFamilyGoogle)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// recorded returns the requests received so far.
func (f *fakeFamilyGoogle) recorded() []googleRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]googleRequest(nil), f.requests...)
}

// since returns the requests received after the first n.
func (f *fakeFamilyGoogle) since(n int) []googleRequest { return f.recorded()[n:] }

func eventRequests(reqs []googleRequest) []googleRequest {
	var out []googleRequest
	for _, r := range reqs {
		if strings.HasSuffix(r.Path, "/events") {
			out = append(out, r)
		}
	}
	return out
}

// newFamilyTestServer is the full router with Google Calendar (token, revoke
// and API endpoints) pointed at fake.
func newFamilyTestServer(t *testing.T, fake *fakeFamilyGoogle) *calendarServer {
	t.Helper()
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
	cipher, err := app.NewTokenCipher(calendarKey)
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	gc := app.NewGoogleCalendar(q, cipher, app.GoogleCalendarConfig{
		ClientID: "client-id", ClientSecret: "client-secret", RedirectURL: calCallbackURL,
		TokenURL:   fake.srv.URL + "/token",
		RevokeURL:  fake.srv.URL + "/revoke",
		APIBaseURL: fake.srv.URL + "/calendar/v3",
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:        app.NewUserService(q),
		HouseholdSvc:   app.NewHouseholdService(q, pool),
		ChoreSvc:       app.NewChoreService(q),
		GoogleCalendar: gc,
		FamilyCalendar: app.NewFamilyCalendar(q, gc),
		JWTSecret:      secret,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &calendarServer{testServer: &testServer{t: t, srv: srv, mail: mail, pool: pool}, cipher: cipher}
}

// familyMember is a signed-up user with an optional Google connection whose
// access token the stand-in knows.
type familyMember struct {
	tokens
	googleAccess string
}

// member signs up a user; with cals it also stores a Google connection whose
// access token lists exactly cals.
func (s *calendarServer) member(fake *fakeFamilyGoogle, name string, cals ...fakeGoogleCal) familyMember {
	s.t.Helper()
	m := familyMember{tokens: s.signUp(name)}
	if len(cals) > 0 {
		m.googleAccess = "access-" + name + "-" + uuid.NewString()
		s.seedConnection(m.User.ID, m.googleAccess, "refresh-"+name, "read_write", time.Now().Add(time.Hour), false)
		fake.set(func(f *fakeFamilyGoogle) { f.calendars[m.googleAccess] = cals })
	}
	return m
}

// household creates a household owned by admin with the others joined, and
// returns its API path.
func (s *calendarServer) household(admin familyMember, others ...familyMember) string {
	s.t.Helper()
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", admin.AccessToken,
		map[string]string{"name": "Family", "timezone": "UTC"}, &h)
	hp := "/api/v1/households/" + h.ID.String()
	for _, o := range others {
		var inv struct{ Code string }
		s.expect(http.StatusCreated, "POST", hp+"/invites", admin.AccessToken, nil, &inv)
		s.expect(http.StatusOK, "POST", "/api/v1/invites/accept", o.AccessToken, map[string]string{"code": inv.Code}, nil)
	}
	return hp
}

type familyStatus struct {
	Status       string `json:"status"`
	CalendarID   string `json:"calendar_id"`
	CalendarName string `json:"calendar_name"`
	SetBy        *struct {
		ID          uuid.UUID `json:"id"`
		DisplayName string    `json:"display_name"`
	} `json:"set_by"`
}

type familyEntry struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Start    string `json:"start"`
	End      string `json:"end"`
	AllDay   bool   `json:"all_day"`
	Location string `json:"location"`
}

// taggedEntry is a listed entry with its tag fields.
type taggedEntry struct {
	familyEntry
	Tags   []uuid.UUID `json:"tags"`
	CanTag bool        `json:"can_tag"`
}

type familyEntries struct {
	Status string        `json:"status"`
	Items  []familyEntry `json:"items"`
}

func (s *calendarServer) familyStatus(hp, token string) familyStatus {
	s.t.Helper()
	var st familyStatus
	s.expect(http.StatusOK, "GET", hp+"/calendar", token, nil, &st)
	return st
}

func (s *calendarServer) setFamily(hp, token, calendarID string) (int, familyStatus) {
	s.t.Helper()
	var st familyStatus
	code := s.do("PUT", hp+"/calendar", token, map[string]string{"calendar_id": calendarID}, &st)
	return code, st
}

var (
	novFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	novTo   = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
)

func rangeQuery(from, to time.Time) string {
	return url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
}

func (s *calendarServer) entries(hp, token string, from, to time.Time) familyEntries {
	s.t.Helper()
	var out familyEntries
	s.expect(http.StatusOK, "GET", hp+"/events?"+rangeQuery(from, to), token, nil, &out)
	return out
}

func (s *calendarServer) familyRows(hp string) int {
	s.t.Helper()
	var n int
	id := strings.TrimPrefix(hp, "/api/v1/households/")
	if err := s.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM household_calendars WHERE household_id = $1", id).Scan(&n); err != nil {
		s.t.Fatalf("count household_calendars: %v", err)
	}
	return n
}

var (
	calX = fakeGoogleCal{ID: "x-primary@example.com", Summary: "Alice", Primary: true}
	calY = fakeGoogleCal{ID: "family-y@group.calendar.google.com", Summary: "Family"}
)

// CLM-1: a connected member lists their calendars and sets one as the family
// calendar; an id not on their list is refused and nothing changes; a
// primary-only list can be listed and set.
func TestFamilyCalendar_ListAndSet(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "famlista", calX, calY)
	hp := s.household(a)

	var mine struct {
		Status string `json:"status"`
		Items  []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Primary bool   `json:"primary"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/me/google-calendar/calendars", a.AccessToken, nil, &mine)
	if mine.Status != "connected" || len(mine.Items) != 2 ||
		mine.Items[0].ID != calX.ID || !mine.Items[0].Primary || mine.Items[0].Name != "Alice" ||
		mine.Items[1].ID != calY.ID || mine.Items[1].Primary || mine.Items[1].Name != "Family" {
		t.Fatalf("my calendars = %+v, want X (primary) and Y", mine)
	}

	if st := s.familyStatus(hp, a.AccessToken); st.Status != "not_configured" || st.CalendarID != "" {
		t.Fatalf("initial status = %+v, want not_configured", st)
	}
	code, st := s.setFamily(hp, a.AccessToken, calY.ID)
	if code != http.StatusOK || st.CalendarID != calY.ID {
		t.Fatalf("set Y: %d %+v", code, st)
	}
	st = s.familyStatus(hp, a.AccessToken)
	if st.Status != "ok" || st.CalendarID != calY.ID || st.CalendarName != "Family" || st.SetBy == nil ||
		st.SetBy.ID != a.User.ID || st.SetBy.DisplayName != "famlista" {
		t.Fatalf("status after set = %+v, want ok, Y, set by A", st)
	}

	var e apiError
	if code := s.do("PUT", hp+"/calendar", a.AccessToken, map[string]string{"calendar_id": "z-not-mine@example.com"}, &e); code != http.StatusUnprocessableEntity || e.Error.Fields["calendar_id"] == "" {
		t.Errorf("set Z: %d %+v, want 422 with a calendar_id error", code, e)
	}
	if st := s.familyStatus(hp, a.AccessToken); st.CalendarID != calY.ID || st.SetBy == nil || st.SetBy.ID != a.User.ID {
		t.Errorf("status after refused Z = %+v, want still Y by A", st)
	}

	// A connection with only the primary calendar.
	p := s.member(fake, "famprimary", fakeGoogleCal{ID: "solo@example.com", Summary: "Solo", Primary: true})
	php := s.household(p)
	s.expect(http.StatusOK, "GET", "/api/v1/me/google-calendar/calendars", p.AccessToken, nil, &mine)
	if len(mine.Items) != 1 || mine.Items[0].ID != "solo@example.com" || !mine.Items[0].Primary {
		t.Fatalf("primary-only list = %+v", mine)
	}
	if code, st := s.setFamily(php, p.AccessToken, "solo@example.com"); code != http.StatusOK || st.Status != "ok" || st.CalendarID != "solo@example.com" {
		t.Errorf("set primary-only: %d %+v", code, st)
	}

	// The routes sit behind the verified-email wrapper.
	if _, err := s.pool.Exec(context.Background(), "UPDATE users SET email_verified = false WHERE id = $1", p.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/v1/me/google-calendar/calendars"}, {"GET", php + "/calendar"},
		{"PUT", php + "/calendar"}, {"DELETE", php + "/calendar"}, {"GET", php + "/events?" + rangeQuery(novFrom, novTo)},
	} {
		e = apiError{}
		if code := s.do(rt.method, rt.path, p.AccessToken, map[string]string{"calendar_id": "solo@example.com"}, &e); code != http.StatusForbidden || e.Error.Code != "email_not_verified" {
			t.Errorf("unverified %s %s: %d %q, want 403 email_not_verified", rt.method, rt.path, code, e.Error.Code)
		}
	}
}

// CLM-2: the events route returns the chosen calendar's entries for the
// range (timed with location, all-day, recurring occurrences), requesting
// timeMin/timeMax = from/to with singleEvents=true; a 101-day range is 422
// with no Google request.
func TestFamilyCalendar_Entries(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "famentries", calX, calY)
	hp := s.household(a)
	fake.set(func(f *fakeFamilyGoogle) {
		f.events[calY.ID] = []map[string]any{
			{"id": "timed1", "summary": "Soccer", "location": "Park",
				"start": map[string]any{"dateTime": "2026-11-05T18:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-05T19:30:00Z"}},
			{"id": "allday1", "summary": "Thanksgiving",
				"start": map[string]any{"date": "2026-11-26"}, "end": map[string]any{"date": "2026-11-27"}},
			{"id": "piano_20261110", "summary": "Piano", "recurringEventId": "piano",
				"start": map[string]any{"dateTime": "2026-11-10T16:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-10T17:00:00Z"}},
			{"id": "piano_20261117", "summary": "Piano", "recurringEventId": "piano",
				"start": map[string]any{"dateTime": "2026-11-17T16:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-17T17:00:00Z"}},
			{"id": "gone", "status": "cancelled", "start": map[string]any{"dateTime": "2026-11-20T16:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-20T17:00:00Z"}},
		}
	})
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}

	before := len(fake.recorded())
	got := s.entries(hp, a.AccessToken, novFrom, novTo)
	if got.Status != "ok" || len(got.Items) != 4 {
		t.Fatalf("entries = %+v, want ok with 4 entries", got)
	}
	want := []familyEntry{
		{ID: "timed1", Title: "Soccer", Start: "2026-11-05T18:00:00Z", End: "2026-11-05T19:30:00Z", Location: "Park"},
		{ID: "allday1", Title: "Thanksgiving", Start: "2026-11-26", End: "2026-11-27", AllDay: true},
		{ID: "piano_20261110", Title: "Piano", Start: "2026-11-10T16:00:00Z", End: "2026-11-10T17:00:00Z"},
		{ID: "piano_20261117", Title: "Piano", Start: "2026-11-17T16:00:00Z", End: "2026-11-17T17:00:00Z"},
	}
	for i, w := range want {
		if got.Items[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, got.Items[i], w)
		}
	}
	evReqs := eventRequests(fake.since(before))
	if len(evReqs) != 3 { // 5 items paged in twos
		t.Fatalf("events requests = %d, want 3 pages", len(evReqs))
	}
	for _, r := range evReqs {
		q := r.Query
		if r.Method != "GET" || q.Get("timeMin") != "2026-11-01T00:00:00Z" || q.Get("timeMax") != "2026-12-01T00:00:00Z" || q.Get("singleEvents") != "true" {
			t.Errorf("events request %s %s?%s: want GET with timeMin/timeMax = from/to and singleEvents=true", r.Method, r.Path, q.Encode())
		}
	}

	before = len(fake.recorded())
	var e apiError
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/events?"+rangeQuery(novFrom, novFrom.Add(101*24*time.Hour)), a.AccessToken, nil, &e)
	if e.Error.Fields["to"] == "" {
		t.Errorf("101-day range: %+v, want a to field error", e)
	}
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/events?"+rangeQuery(novTo, novFrom), a.AccessToken, nil, nil)
	if reqs := fake.since(before); len(reqs) != 0 {
		t.Errorf("invalid ranges reached Google: %+v", reqs)
	}
	s.expect(http.StatusOK, "GET", hp+"/events?"+rangeQuery(novFrom, novFrom.Add(100*24*time.Hour)), a.AccessToken, nil, nil)
}

// CLM-3: every entries request addresses only the stored calendar, never the
// connector's other or primary calendar; switching the family calendar to X
// makes the next fetch address X (positive control).
func TestFamilyCalendar_OnlyChosenCalendar(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "famonlya", calX, calY)
	b := s.member(fake, "famonlyb")
	hp := s.household(a, b)
	fake.set(func(f *fakeFamilyGoogle) {
		f.events[calX.ID] = []map[string]any{{"id": "x-private", "summary": "X only",
			"start": map[string]any{"dateTime": "2026-11-03T10:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-03T11:00:00Z"}}}
		f.events[calY.ID] = []map[string]any{{"id": "y-family", "summary": "Y family",
			"start": map[string]any{"dateTime": "2026-11-04T10:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-04T11:00:00Z"}}}
	})
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}

	before := len(fake.recorded())
	for _, m := range []familyMember{a, b} {
		got := s.entries(hp, m.AccessToken, novFrom, novTo)
		if got.Status != "ok" || len(got.Items) != 1 || got.Items[0].ID != "y-family" {
			t.Errorf("entries for %s = %+v, want only y-family", m.User.ID, got)
		}
	}
	wantPath := "/calendar/v3/calendars/" + url.PathEscape(calY.ID) + "/events"
	evReqs := eventRequests(fake.since(before))
	if len(evReqs) != 2 {
		t.Fatalf("events requests = %d, want 2", len(evReqs))
	}
	for _, r := range evReqs {
		if r.Path != wantPath || strings.Contains(r.Path, "primary") || strings.Contains(r.Path, url.PathEscape(calX.ID)) {
			t.Errorf("events request path %q, want only %q", r.Path, wantPath)
		}
	}

	// Positive control: the setter switches to X.
	if code, _ := s.setFamily(hp, a.AccessToken, calX.ID); code != http.StatusOK {
		t.Fatalf("set X: %d", code)
	}
	before = len(fake.recorded())
	got := s.entries(hp, b.AccessToken, novFrom, novTo)
	if len(got.Items) != 1 || got.Items[0].ID != "x-private" {
		t.Errorf("entries after switching to X = %+v, want x-private", got)
	}
	evReqs = eventRequests(fake.since(before))
	if len(evReqs) != 1 || evReqs[0].Path != "/calendar/v3/calendars/"+url.PathEscape(calX.ID)+"/events" {
		t.Errorf("events requests after switch = %+v, want one to X", evReqs)
	}
}

// CLM-4: Chorus has no route to create, change or delete an entry, and the
// stand-in sees only GET requests on events across list, status and set.
func TestFamilyCalendar_NoWriteRoutes(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "famnowrite", calX, calY)
	hp := s.household(a)
	fake.set(func(f *fakeFamilyGoogle) {
		f.events[calY.ID] = []map[string]any{{"id": "e1", "summary": "Soccer",
			"start": map[string]any{"dateTime": "2026-11-05T18:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-05T19:00:00Z"}}}
	})
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}
	s.familyStatus(hp, a.AccessToken)
	s.entries(hp, a.AccessToken, novFrom, novTo)

	body := map[string]any{"title": "New", "starts_at": "2026-11-05T18:00:00Z", "ends_at": "2026-11-05T19:00:00Z"}
	for _, rt := range []struct{ method, path string }{
		{"POST", hp + "/events"},
		{"GET", hp + "/events/e1"},
		{"PUT", hp + "/events/e1"},
		{"PATCH", hp + "/events/e1"},
		{"DELETE", hp + "/events/e1"},
		{"DELETE", hp + "/events"},
	} {
		code := s.do(rt.method, rt.path, a.AccessToken, body, nil)
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 404 or 405", rt.method, rt.path, code)
		}
	}
	reqs := fake.recorded()
	if len(eventRequests(reqs)) == 0 {
		t.Fatal("no events request was made; the GET-only assertion would be vacuous")
	}
	for _, r := range reqs {
		if strings.Contains(r.Path, "/calendar/v3/") && r.Method != http.MethodGet {
			t.Errorf("Calendar API request %s %s, want only GET", r.Method, r.Path)
		}
	}
}

// CLM-5: with no family calendar, status and events report not_configured
// with no entries and the stand-in records no request.
func TestFamilyCalendar_NotConfigured(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	connected := s.member(fake, "famnotcfga", calX)
	plain := s.member(fake, "famnotcfgb")
	hp := s.household(connected, plain)

	before := len(fake.recorded())
	for _, m := range []familyMember{connected, plain} {
		if st := s.familyStatus(hp, m.AccessToken); st.Status != "not_configured" || st.CalendarID != "" || st.SetBy != nil {
			t.Errorf("status = %+v, want bare not_configured", st)
		}
		got := s.entries(hp, m.AccessToken, novFrom, novTo)
		if got.Status != "not_configured" || got.Items == nil || len(got.Items) != 0 {
			t.Errorf("entries = %+v, want not_configured with []", got)
		}
	}
	if reqs := fake.since(before); len(reqs) != 0 {
		t.Errorf("stand-in recorded %d requests, want 0: %+v", len(reqs), reqs)
	}
}

// CLM-6: an empty month is ok with [] (not not_configured), and the next
// month's range shifts timeMin/timeMax.
func TestFamilyCalendar_EmptyMonth(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "famempty", calY)
	hp := s.household(a)
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}

	before := len(fake.recorded())
	status, raw := s.doRaw("GET", hp+"/events?"+rangeQuery(novFrom, novTo), a.AccessToken, nil)
	if status != http.StatusOK || !strings.Contains(raw, `"items":[]`) || !strings.Contains(raw, `"status":"ok"`) {
		t.Fatalf("empty month: %d %s, want ok with items []", status, raw)
	}
	decFrom, decTo := novTo, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := s.entries(hp, a.AccessToken, decFrom, decTo); got.Status != "ok" || len(got.Items) != 0 {
		t.Errorf("next month = %+v, want ok []", got)
	}
	evReqs := eventRequests(fake.since(before))
	if len(evReqs) != 2 {
		t.Fatalf("events requests = %d, want 2", len(evReqs))
	}
	if evReqs[0].Query.Get("timeMin") != "2026-11-01T00:00:00Z" || evReqs[1].Query.Get("timeMin") != "2026-12-01T00:00:00Z" ||
		evReqs[1].Query.Get("timeMax") != "2027-01-01T00:00:00Z" {
		t.Errorf("timeMin/timeMax = %v / %v, want November then December", evReqs[0].Query, evReqs[1].Query)
	}
}

// CLM-8: only the member who set the family calendar can change or clear it.
func TestFamilyCalendar_OnlySetterChanges(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "famsettera", calX, calY)
	calW := fakeGoogleCal{ID: "w-bob@example.com", Summary: "Bob", Primary: true}
	b := s.member(fake, "famsetterb", calW)
	hp := s.household(a, b)
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}
	stillAY := func(when string) {
		t.Helper()
		st := s.familyStatus(hp, b.AccessToken)
		if st.CalendarID != calY.ID || st.SetBy == nil || st.SetBy.ID != a.User.ID {
			t.Errorf("%s: status = %+v, want (A, Y)", when, st)
		}
	}

	var e apiError
	if code := s.do("PUT", hp+"/calendar", b.AccessToken, map[string]string{"calendar_id": calW.ID}, &e); code != http.StatusForbidden || e.Error.Code != "forbidden" {
		t.Errorf("B sets W: %d %q, want 403 forbidden", code, e.Error.Code)
	}
	stillAY("after B's set")
	e = apiError{}
	if code := s.do("DELETE", hp+"/calendar", b.AccessToken, nil, &e); code != http.StatusForbidden || e.Error.Code != "forbidden" {
		t.Errorf("B clears: %d %q, want 403 forbidden", code, e.Error.Code)
	}
	stillAY("after B's clear")

	if code, st := s.setFamily(hp, a.AccessToken, calX.ID); code != http.StatusOK || st.CalendarID != calX.ID {
		t.Fatalf("A changes to X: %d %+v", code, st)
	}
	if st := s.familyStatus(hp, b.AccessToken); st.CalendarID != calX.ID || st.SetBy.ID != a.User.ID {
		t.Errorf("after A's change: %+v, want (A, X)", st)
	}
	s.expect(http.StatusNoContent, "DELETE", hp+"/calendar", a.AccessToken, nil, nil)
	if st := s.familyStatus(hp, b.AccessToken); st.Status != "not_configured" {
		t.Errorf("after A's clear: %+v, want not_configured", st)
	}
	if n := s.familyRows(hp); n != 0 {
		t.Errorf("rows after clear = %d", n)
	}
	// Once cleared, anyone connected may set it.
	if code, st := s.setFamily(hp, b.AccessToken, calW.ID); code != http.StatusOK || st.SetBy.ID != b.User.ID {
		t.Errorf("B sets after clear: %d %+v", code, st)
	}
}

// CLM-9: the family calendar is cleared, by the database alone, when its
// setter disconnects Google, leaves or is removed, or deletes their account.
func TestFamilyCalendar_ClearedWithSetter(t *testing.T) {
	// Each case gets its own server: sign-up is rate limited per instance.
	cases := []struct {
		name     string
		calendar bool // whether Google is contacted by the action itself
		act      func(s *calendarServer, a, b familyMember, hp string)
	}{
		{"disconnect", true, func(s *calendarServer, a, _ familyMember, _ string) {
			s.expect(http.StatusOK, "DELETE", "/api/v1/me/google-calendar", a.AccessToken, nil, nil)
		}},
		{"leave", false, func(s *calendarServer, a, _ familyMember, hp string) {
			s.expect(http.StatusNoContent, "DELETE", hp+"/members/"+a.User.ID.String(), a.AccessToken, nil, nil)
		}},
		{"removed by admin", false, func(s *calendarServer, a, b familyMember, hp string) {
			s.expect(http.StatusNoContent, "DELETE", hp+"/members/"+a.User.ID.String(), b.AccessToken, nil, nil)
		}},
		{"account deleted", false, func(s *calendarServer, a, _ familyMember, _ string) {
			s.expect(http.StatusNoContent, "DELETE", "/api/v1/me", a.AccessToken, map[string]string{"password": "correct-password-123"}, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeFamilyGoogle(t)
			s := newFamilyTestServer(t, fake)
			b := s.member(fake, "famclearb")
			a := s.member(fake, "famcleara", calY)
			hp := s.household(b, a) // B is the admin, A a member
			if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
				t.Fatalf("set Y: %d", code)
			}
			if n := s.familyRows(hp); n != 1 {
				t.Fatalf("rows before = %d", n)
			}

			before := len(fake.recorded())
			tc.act(s, a, b, hp)
			reqs := fake.since(before)
			for _, r := range reqs {
				// Disconnect itself revokes at Google; nothing else may call it.
				if !(tc.calendar && r.Path == "/revoke") {
					t.Errorf("%s made Google request %s %s", tc.name, r.Method, r.Path)
				}
			}
			if n := s.familyRows(hp); n != 0 {
				t.Errorf("rows after %s = %d, want 0", tc.name, n)
			}
			before = len(fake.recorded())
			if st := s.familyStatus(hp, b.AccessToken); st.Status != "not_configured" {
				t.Errorf("B's status after %s = %+v, want not_configured", tc.name, st)
			}
			if got := s.entries(hp, b.AccessToken, novFrom, novTo); got.Status != "not_configured" {
				t.Errorf("B's entries after %s = %+v, want not_configured", tc.name, got)
			}
			if reqs := fake.since(before); len(reqs) != 0 {
				t.Errorf("not_configured reads made %d Google requests", len(reqs))
			}
		})
	}
}

// A rate-limit 403, a 429 or a 5xx on the events listing is temporary: 503
// with Retry-After, not broken, and the connection is not marked broken. A
// plain 403 (Google refused the calendar) still reports broken.
func TestFamilyCalendar_ListingRateLimitNotBroken(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "famratea", calY)
	b := s.member(fake, "famrateb")
	hp := s.household(a, b)
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		want         int
		code, family string
	}{
		{"403 rateLimitExceeded", http.StatusForbidden, `{"error":{"code":403,"message":"INTERNAL-SENTINEL","errors":[{"domain":"usageLimits","reason":"rateLimitExceeded"}]}}`, http.StatusServiceUnavailable, "google_unavailable", ""},
		{"403 userRateLimitExceeded", http.StatusForbidden, `{"error":{"code":403,"errors":[{"domain":"usageLimits","reason":"userRateLimitExceeded"}]}}`, http.StatusServiceUnavailable, "google_unavailable", ""},
		{"429", http.StatusTooManyRequests, "INTERNAL-SENTINEL", http.StatusServiceUnavailable, "google_unavailable", ""},
		{"503", http.StatusServiceUnavailable, "INTERNAL-SENTINEL", http.StatusServiceUnavailable, "google_unavailable", ""},
		{"403 forbidden", http.StatusForbidden, `{"error":{"code":403,"errors":[{"domain":"global","reason":"forbidden"}]}}`, http.StatusOK, "", "broken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake.set(func(f *fakeFamilyGoogle) { f.listStatus, f.listBody = tc.status, tc.body })
			defer fake.set(func(f *fakeFamilyGoogle) { f.listStatus, f.listBody = 0, "" })
			resp := s.doResp("GET", hp+"/events?"+rangeQuery(novFrom, novTo), b.AccessToken, nil)
			if resp.status != tc.want {
				t.Fatalf("events: %d %s, want %d", resp.status, resp.body, tc.want)
			}
			if strings.Contains(resp.body, "INTERNAL-SENTINEL") || strings.Contains(resp.body, "usageLimits") {
				t.Errorf("events response leaks Google's body: %s", resp.body)
			}
			if tc.code != "" {
				var e apiError
				_ = json.Unmarshal([]byte(resp.body), &e)
				if e.Error.Code != tc.code || resp.retryAfter == "" {
					t.Errorf("events: code %q Retry-After %q, want %q with Retry-After", e.Error.Code, resp.retryAfter, tc.code)
				}
			} else {
				var got familyEntries
				_ = json.Unmarshal([]byte(resp.body), &got)
				if got.Status != tc.family {
					t.Errorf("events status %q, want %q", got.Status, tc.family)
				}
			}
			if c := s.connection(a.User.ID); c.Broken {
				t.Error("connection marked broken")
			}
		})
	}
}

// CLM-11: with the setter's token expired and the refresh rejected
// (invalid_grant), status and events report broken, with no /events request
// and no 5xx.
func TestFamilyCalendar_BrokenConnection(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	a := s.member(fake, "fambrokena", calY)
	b := s.member(fake, "fambrokenb")
	hp := s.household(a, b)
	fake.set(func(f *fakeFamilyGoogle) {
		f.events[calY.ID] = []map[string]any{{"id": "e1", "summary": "Soccer",
			"start": map[string]any{"dateTime": "2026-11-05T18:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-05T19:00:00Z"}}}
	})
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE google_calendar_connections SET token_expiry = $1 WHERE user_id = $2", time.Now().Add(-time.Hour), a.User.ID); err != nil {
		t.Fatal(err)
	}
	fake.set(func(f *fakeFamilyGoogle) { f.refreshError = "invalid_grant" })

	before := len(fake.recorded())
	status, raw := s.doRaw("GET", hp+"/events?"+rangeQuery(novFrom, novTo), b.AccessToken, nil)
	var got familyEntries
	_ = json.Unmarshal([]byte(raw), &got)
	if status != http.StatusOK || got.Status != "broken" || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("entries: %d %s, want 200 broken with []", status, raw)
	}
	reqs := fake.since(before)
	if len(reqs) != 1 || reqs[0].Path != "/token" {
		t.Errorf("requests = %+v, want only the refused refresh", reqs)
	}
	if c := s.connection(a.User.ID); !c.Broken {
		t.Error("A's connection not marked broken")
	}

	// Already broken: no refresh, no events, still broken.
	before = len(fake.recorded())
	if st := s.familyStatus(hp, b.AccessToken); st.Status != "broken" || st.CalendarID != calY.ID {
		t.Errorf("status = %+v, want broken (still Y)", st)
	}
	if got := s.entries(hp, b.AccessToken, novFrom, novTo); got.Status != "broken" || len(got.Items) != 0 {
		t.Errorf("entries again = %+v, want broken []", got)
	}
	if reqs := fake.since(before); len(reqs) != 0 {
		t.Errorf("broken connection still contacted Google: %+v", reqs)
	}
}

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

// rename sets a display name directly (names with ',' can't go through the
// sign-up email address).
func (s *calendarServer) rename(id uuid.UUID, name string) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), "UPDATE users SET display_name = $1 WHERE id = $2", name, id); err != nil {
		s.t.Fatalf("rename: %v", err)
	}
}

type tagMemberOut struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Taggable    bool      `json:"taggable"`
	Reason      string    `json:"reason"`
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

// tagFixture is household H with connector Mom (calendar Y, read_write) and
// member Sam, Y as the family calendar, and entry E "Dentist" on it.
func tagFixture(t *testing.T) (*calendarServer, *fakeFamilyGoogle, familyMember, familyMember, string) {
	t.Helper()
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	mom := s.member(fake, "tagmom", calY)
	sam := s.member(fake, "tagsam")
	s.rename(mom.User.ID, "Mom")
	s.rename(sam.User.ID, "Sam")
	hp := s.household(mom, sam)
	fake.set(func(f *fakeFamilyGoogle) { f.events[calY.ID] = []map[string]any{dentist()} })
	if code, _ := s.setFamily(hp, mom.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}
	return s, fake, mom, sam, hp
}

// CLM-1, CLM-2, CLM-3, CLM-4: a member tags E through the tag route; Chorus
// PATCHes only the summary 'Dentist [Mom, Sam]' with the connector's token
// and answers with Google's title; the listing reads tags from Google's
// title; untagging ends at 'Dentist'; a title changed at Google shows as is.
func TestFamilyCalendar_TagEntry(t *testing.T) {
	s, fake, mom, sam, hp := tagFixture(t)
	before := fake.entry(calY.ID, "E")

	n := len(fake.recorded())
	code, raw := s.tag(hp, sam.AccessToken, "E", mom.User.ID, sam.User.ID)
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
		!slices.Equal(res.Tags, []uuid.UUID{mom.User.ID, sam.User.ID}) || !res.CanTag {
		t.Errorf("response = %+v, want E with Google's title 'Dentist [Mom, Sam]' and both tags", res)
	}
	// CLM-2: only the summary went to Google; the other fields are intact.
	if problems := tagChangeProblems(reqs, before, after); len(problems) > 0 {
		t.Errorf("tag change touched more than the title: %v", problems)
	}

	// CLM-3: the listing shows Google's title with the parsed tags.
	list := s.taggedEntries(hp, mom.AccessToken)
	e := list.find("E")
	if e.Title != "Dentist [Mom, Sam]" || !slices.Equal(e.Tags, []uuid.UUID{mom.User.ID, sam.User.ID}) || !e.CanTag {
		t.Errorf("listed E = %+v, want Google's title, tags [Mom Sam], can_tag", e)
	}
	if len(list.Members) != 2 || !list.Members[0].Taggable || !list.Members[1].Taggable {
		t.Errorf("members = %+v, want Mom and Sam taggable", list.Members)
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
	e = s.taggedEntries(hp, sam.AccessToken).find("E")
	if e.Title != "Dentist moved [sam]" || !slices.Equal(e.Tags, []uuid.UUID{sam.User.ID}) {
		t.Errorf("listed E after Google-side change = %+v, want 'Dentist moved [sam]' tagged Sam", e)
	}

	// The tag route is not under the retired /events editing routes.
	if code := s.do("PUT", hp+"/events/E/tags", sam.AccessToken, map[string]any{"tags": []uuid.UUID{}}, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
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
	s, fake, mom, sam, hp := tagFixture(t)
	const rateLimit403 = `{"error":{"code":403,"message":"INTERNAL-SENTINEL","errors":[{"domain":"usageLimits","reason":"rateLimitExceeded"}]}}`
	for _, tc := range []struct {
		googleStatus, want int
		code               string
		body               string
	}{
		{http.StatusNotFound, http.StatusNotFound, "entry_gone", ""},
		{http.StatusGone, http.StatusNotFound, "entry_gone", ""},
		{http.StatusForbidden, http.StatusForbidden, "not_editable", ""},
		{http.StatusForbidden, http.StatusServiceUnavailable, "google_unavailable", rateLimit403},
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
		resp := s.tagResp(hp, sam.AccessToken, "E", mom.User.ID)
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
			code, raw := s.tag(hp, m.AccessToken, "E", m.User.ID)
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

// CLM-7: any member may tag (the PATCH uses the connector's token); tagging
// an outsider, a duplicate-named member or a name with ',' is 422 with no
// Google request; the members data marks them untaggable.
func TestFamilyCalendar_TagMembers(t *testing.T) {
	fake := newFakeFamilyGoogle(t)
	s := newFamilyTestServer(t, fake)
	// A second router on the same database: sign-up is limited per instance
	// and its tokens are valid on both.
	s2 := newFamilyTestServer(t, fake)
	a := s.member(fake, "tagconn", calY)
	b := s.member(fake, "tagplain")
	d1 := s.member(fake, "tagsam1")
	d2 := s2.member(fake, "tagsam2")
	e := s2.member(fake, "tagjo")
	c := s2.member(fake, "tagoutsider")
	for id, name := range map[uuid.UUID]string{a.User.ID: "Alex", b.User.ID: "Bea", d1.User.ID: "Sam", d2.User.ID: "sam ", e.User.ID: "Smith, Jo", c.User.ID: "Cy"} {
		s.rename(id, name)
	}
	hp := s.household(a, b, d1, d2, e)
	fake.set(func(f *fakeFamilyGoogle) { f.events[calY.ID] = []map[string]any{dentist()} })
	if code, _ := s.setFamily(hp, a.AccessToken, calY.ID); code != http.StatusOK {
		t.Fatalf("set Y: %d", code)
	}

	n := len(fake.recorded())
	if code, raw := s.tag(hp, b.AccessToken, "E", a.User.ID); code != http.StatusOK || !strings.Contains(raw, `"title":"Dentist [Alex]"`) {
		t.Fatalf("B tags A: %d %s", code, raw)
	}
	if p := mutating(fake.since(n)); len(p) != 1 || p[0].Auth != a.googleAccess {
		t.Errorf("B's tag PATCH = %+v, want one with A's token", p)
	}

	for name, id := range map[string]uuid.UUID{"outsider C": c.User.ID, "D1 (duplicate name)": d1.User.ID, "D2 (duplicate name)": d2.User.ID, "E (comma)": e.User.ID} {
		n := len(fake.recorded())
		code, raw := s.tag(hp, b.AccessToken, "E", a.User.ID, id)
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

	want := map[uuid.UUID]string{a.User.ID: "", b.User.ID: "", d1.User.ID: "duplicate_name", d2.User.ID: "duplicate_name", e.User.ID: "reserved_character"}
	list := s.taggedEntries(hp, b.AccessToken)
	if len(list.Members) != len(want) {
		t.Fatalf("members = %+v, want %d", list.Members, len(want))
	}
	for _, m := range list.Members {
		r, ok := want[m.UserID]
		if !ok || m.Reason != r || m.Taggable != (r == "") {
			t.Errorf("member %q: taggable=%v reason=%q, want reason %q", m.DisplayName, m.Taggable, m.Reason, r)
		}
	}
}
