//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
}

type fakeGoogleCal struct {
	ID, Summary string
	Primary     bool
}

// fakeFamilyGoogle stands in for Google's token endpoint and the read side of
// the Calendar API: calendarList per access token and events per calendar id.
// It records every request it receives.
type fakeFamilyGoogle struct {
	srv *httptest.Server

	mu           sync.Mutex
	requests     []googleRequest
	calendars    map[string][]fakeGoogleCal // access token → calendar list
	events       map[string][]map[string]any
	refreshError string // "" = refresh succeeds
}

func newFakeFamilyGoogle(t *testing.T) *fakeFamilyGoogle {
	t.Helper()
	f := &fakeFamilyGoogle{calendars: map[string][]fakeGoogleCal{}, events: map[string][]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFamilyGoogle) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.requests = append(f.requests, googleRequest{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.Query(), Auth: auth})
	w.Header().Set("Content-Type", "application/json")

	switch {
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
		resp := map[string]any{"items": items[start:end]}
		if end < len(items) {
			resp["nextPageToken"] = fmt.Sprintf("p%d", end)
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
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
