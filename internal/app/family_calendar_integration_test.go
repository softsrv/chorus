//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// calendarStandIn answers the Calendar API reads (calendarList and events)
// for one connected user and records every request with its bearer token.
type calendarStandIn struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []standInRequest
	access   string                      // the only access token accepted
	cals     map[string]string           // calendar id → summary
	events   map[string][]map[string]any // calendar id → items
}

type standInRequest struct {
	Method, Path, Auth string
}

func newCalendarStandIn(t *testing.T, access string) *calendarStandIn {
	t.Helper()
	f := &calendarStandIn{access: access, cals: map[string]string{}, events: map[string][]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.requests = append(f.requests, standInRequest{Method: r.Method, Path: r.URL.Path, Auth: auth})
		w.Header().Set("Content-Type", "application/json")
		if auth != f.access {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/calendarList":
			items := []map[string]any{}
			for id, name := range f.cals {
				items = append(items, map[string]any{"id": id, "summary": name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/calendars/") && strings.HasSuffix(r.URL.Path, "/events"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/calendars/"), "/events")
			items := f.events[id]
			if items == nil {
				items = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *calendarStandIn) recorded() []standInRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]standInRequest(nil), f.requests...)
}

// connectCalendar stores a working Google connection for userID with the
// given access token, and returns a FamilyCalendar talking to the stand-in.
func connectCalendar(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, access, refresh string, f *calendarStandIn) *app.FamilyCalendar {
	t.Helper()
	q := db.New(pool)
	cipher, err := app.NewTokenCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	ae, _ := cipher.Encrypt(access)
	re, _ := cipher.Encrypt(refresh)
	if _, err := q.UpsertGoogleCalendarConnection(context.Background(), db.UpsertGoogleCalendarConnectionParams{
		ID: uuid.Must(uuid.NewV7()), UserID: userID, AccessTokenEnc: ae, RefreshTokenEnc: re,
		TokenExpiry: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, AccessLevel: "read_write",
	}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	gc := app.NewGoogleCalendar(q, cipher, app.GoogleCalendarConfig{
		ClientID: "client-id", ClientSecret: "client-secret", RedirectURL: "http://chorus.test/cb",
		TokenURL: f.srv.URL + "/token", RevokeURL: f.srv.URL + "/revoke", APIBaseURL: f.srv.URL,
	})
	return app.NewFamilyCalendar(q, gc)
}

// CLM-10: retiring Chorus's own event editing deletes no data. Rows in the
// events table, inserted through the sqlc query that remains, are unchanged
// after the full set of migrations (including 000007) and after the family
// calendar flows run for the same household.
func TestIntegration_FamilyCalendarKeepsEvents(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	ctx := context.Background()

	// The migration under test is applied: its table exists.
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('household_calendars') IS NOT NULL").Scan(&exists); err != nil || !exists {
		t.Fatalf("household_calendars missing (migration 000007 not applied?): %v", err)
	}

	alice := registerVerified(t, pool, authSvc, mailer, "keepevents").User.ID
	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Keep", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}
	start := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	var seeded []db.Event
	for i, title := range []string{"Soccer", "Dentist", "Holiday"} {
		ev, err := q.CreateEvent(ctx, db.CreateEventParams{
			ID: uuid.Must(uuid.NewV7()), HouseholdID: h.ID, Title: title, Location: "Here",
			StartsAt:  pgtype.Timestamptz{Time: start.Add(time.Duration(i) * 24 * time.Hour), Valid: true},
			EndsAt:    pgtype.Timestamptz{Time: start.Add(time.Duration(i)*24*time.Hour + time.Hour), Valid: true},
			AllDay:    i == 2,
			CreatedBy: &alice,
		})
		if err != nil {
			t.Fatalf("CreateEvent: %v", err)
		}
		seeded = append(seeded, ev)
	}

	// Re-apply this change's migration over the existing rows: down, then up.
	for _, file := range []string{"000007_family_calendar.down.sql", "000007_family_calendar.up.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	assertEventsUnchanged(t, q, h.ID, seeded, start)

	f := newCalendarStandIn(t, "access-keep")
	f.cals["fam@group.calendar.google.com"] = "Family"
	family := connectCalendar(t, pool, alice, "access-keep", "refresh-keep", f)
	if _, err := family.Set(ctx, alice, h.ID, "fam@group.calendar.google.com"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if res, err := family.ListEntries(ctx, alice, h.ID, start.Add(-24*time.Hour), start.Add(30*24*time.Hour)); err != nil || res.Status != app.FamilyCalendarOK {
		t.Fatalf("ListEntries: %+v %v", res, err)
	}
	if err := family.Clear(ctx, alice, h.ID); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	assertEventsUnchanged(t, q, h.ID, seeded, start)
}

func assertEventsUnchanged(t *testing.T, q *db.Queries, householdID uuid.UUID, seeded []db.Event, start time.Time) {
	t.Helper()
	ctx := context.Background()
	got, err := q.ListEventsInRange(ctx, db.ListEventsInRangeParams{
		HouseholdID: householdID,
		RangeStart:  pgtype.Timestamptz{Time: start.Add(-24 * time.Hour), Valid: true},
		RangeEnd:    pgtype.Timestamptz{Time: start.Add(30 * 24 * time.Hour), Valid: true},
		RowLimit:    100,
	})
	if err != nil {
		t.Fatalf("ListEventsInRange: %v", err)
	}
	if len(got) != len(seeded) {
		t.Fatalf("events rows = %d, want %d", len(got), len(seeded))
	}
	for i := range seeded {
		w, g := seeded[i], got[i]
		if g.ID != w.ID || g.Title != w.Title || g.Location != w.Location || g.AllDay != w.AllDay ||
			!g.StartsAt.Time.Equal(w.StartsAt.Time) || !g.EndsAt.Time.Equal(w.EndsAt.Time) || !g.UpdatedAt.Time.Equal(w.UpdatedAt.Time) {
			t.Errorf("event %d changed: got %+v, want %+v", i, g, w)
		}
	}
}
