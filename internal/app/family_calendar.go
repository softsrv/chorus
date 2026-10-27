package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/softsrv/chorus/internal/db"
)

// maxEventRange bounds one household entries query; clients page through
// longer periods.
const maxEventRange = 100 * 24 * time.Hour

// Family calendar statuses, shared by the household calendar status and the
// household events listing.
const (
	FamilyCalendarOK            = "ok"
	FamilyCalendarNotConfigured = "not_configured"
	FamilyCalendarBroken        = "broken"
	// FamilyCalendarUnavailable means Google Calendar isn't configured on
	// this deployment.
	FamilyCalendarUnavailable = "unavailable"
)

// ErrGoogleUnavailable means Google could not be reached or failed
// temporarily. Nothing was changed; the caller may retry.
var ErrGoogleUnavailable = errGoogleUnavailable

// HouseholdCalendarStatus is a household's family calendar as its members see
// it. The calendar fields are set whenever a family calendar is chosen. It
// never carries a token or the setter's calendar list.
type HouseholdCalendarStatus struct {
	Status       string
	CalendarID   string
	CalendarName string
	SetByID      uuid.UUID
	SetByName    string
}

// MyCalendars is the caller's own Google calendar list, for picking a family
// calendar. Status is a connection status (CalendarConnected, ...) or
// FamilyCalendarUnavailable.
type MyCalendars struct {
	Status    string
	Calendars []GoogleCalendarInfo
}

// HouseholdEntries is the result of listing a household's family calendar.
// Entries is non-nil (possibly empty) when Status is FamilyCalendarOK.
type HouseholdEntries struct {
	Status  string
	Entries []GoogleEntry
}

// FamilyCalendar is a household's shared Google calendar: one member who has
// connected Google picks one of their calendars, and every member sees its
// entries, fetched server-side with that member's token. It only reads from
// Google. A nil GoogleCalendar means Google Calendar is unavailable here.
type FamilyCalendar struct {
	q  *db.Queries
	gc *GoogleCalendar
}

// NewFamilyCalendar constructs a FamilyCalendar. gc may be nil.
func NewFamilyCalendar(q *db.Queries, gc *GoogleCalendar) *FamilyCalendar {
	return &FamilyCalendar{q: q, gc: gc}
}

// ListMyCalendars returns the caller's own Google calendars.
func (s *FamilyCalendar) ListMyCalendars(ctx context.Context, userID uuid.UUID) (MyCalendars, error) {
	if s.gc == nil {
		return MyCalendars{Status: FamilyCalendarUnavailable, Calendars: []GoogleCalendarInfo{}}, nil
	}
	cals, err := s.myCalendars(ctx, userID)
	switch {
	case errors.Is(err, errCalendarNotConnected):
		return MyCalendars{Status: CalendarNotConnected, Calendars: []GoogleCalendarInfo{}}, nil
	case errors.Is(err, errCalendarBroken) || refusedByGoogle(err):
		return MyCalendars{Status: CalendarBroken, Calendars: []GoogleCalendarInfo{}}, nil
	case err != nil:
		return MyCalendars{}, err
	}
	return MyCalendars{Status: CalendarConnected, Calendars: cals}, nil
}

func (s *FamilyCalendar) myCalendars(ctx context.Context, userID uuid.UUID) ([]GoogleCalendarInfo, error) {
	token, err := s.gc.validAccessToken(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.gc.listCalendars(ctx, token)
}

// Set makes calendarID, which must be on the caller's own Google calendar
// list, the household's family calendar. Once set, only the member who set it
// can change it; anyone else gets ErrForbidden and the stored value stays.
func (s *FamilyCalendar) Set(ctx context.Context, userID, householdID uuid.UUID, calendarID string) (HouseholdCalendarStatus, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return HouseholdCalendarStatus{}, err
	}
	var v validator
	calendarID = v.text("calendar_id", calendarID, 1, 1024, false)
	if err := v.err(); err != nil {
		return HouseholdCalendarStatus{}, err
	}
	if s.gc == nil {
		return HouseholdCalendarStatus{}, &ValidationError{Fields: map[string]string{"calendar_id": "Google Calendar isn't available on this server"}}
	}
	// Refuse another member's change before asking Google anything.
	if existing, err := s.q.GetHouseholdCalendar(ctx, householdID); err == nil && existing.SetBy != userID {
		return HouseholdCalendarStatus{}, ErrForbidden
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return HouseholdCalendarStatus{}, fmt.Errorf("get household calendar: %w", err)
	}

	cals, err := s.myCalendars(ctx, userID)
	switch {
	case errors.Is(err, errCalendarNotConnected):
		return HouseholdCalendarStatus{}, &ValidationError{Fields: map[string]string{"calendar_id": "connect Google Calendar on your Account page first"}}
	case errors.Is(err, errCalendarBroken) || refusedByGoogle(err):
		return HouseholdCalendarStatus{}, &ValidationError{Fields: map[string]string{"calendar_id": "reconnect Google Calendar on your Account page first"}}
	case err != nil:
		return HouseholdCalendarStatus{}, err
	}
	var chosen *GoogleCalendarInfo
	for i := range cals {
		if cals[i].ID == calendarID {
			chosen = &cals[i]
			break
		}
	}
	if chosen == nil {
		return HouseholdCalendarStatus{}, &ValidationError{Fields: map[string]string{"calendar_id": "is not one of your Google calendars"}}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return HouseholdCalendarStatus{}, fmt.Errorf("generate household calendar id: %w", err)
	}
	// The upsert only replaces a row set by the same member, so a concurrent
	// set by someone else still can't be overwritten.
	if _, err := s.q.UpsertHouseholdCalendar(ctx, db.UpsertHouseholdCalendarParams{
		ID:           id,
		HouseholdID:  householdID,
		SetBy:        userID,
		CalendarID:   chosen.ID,
		CalendarName: truncate(chosen.Name, 200),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HouseholdCalendarStatus{}, ErrForbidden
		}
		return HouseholdCalendarStatus{}, fmt.Errorf("upsert household calendar: %w", err)
	}
	return s.Status(ctx, userID, householdID)
}

// Clear removes the household's family calendar. Only the member who set it
// may; anyone else gets ErrForbidden. Clearing when none is set succeeds.
func (s *FamilyCalendar) Clear(ctx context.Context, userID, householdID uuid.UUID) error {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return err
	}
	n, err := s.q.DeleteHouseholdCalendar(ctx, db.DeleteHouseholdCalendarParams{HouseholdID: householdID, SetBy: userID})
	if err != nil {
		return fmt.Errorf("delete household calendar: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, err := s.q.GetHouseholdCalendar(ctx, householdID); err == nil {
		return ErrForbidden
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get household calendar: %w", err)
	}
	return nil
}

// Status reports the household's family calendar. With none set it is
// not_configured and Google is not contacted.
func (s *FamilyCalendar) Status(ctx context.Context, userID, householdID uuid.UUID) (HouseholdCalendarStatus, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return HouseholdCalendarStatus{}, err
	}
	if s.gc == nil {
		return HouseholdCalendarStatus{Status: FamilyCalendarUnavailable}, nil
	}
	row, err := s.q.GetHouseholdCalendar(ctx, householdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HouseholdCalendarStatus{Status: FamilyCalendarNotConfigured}, nil
		}
		return HouseholdCalendarStatus{}, fmt.Errorf("get household calendar: %w", err)
	}
	st := HouseholdCalendarStatus{
		Status:       FamilyCalendarOK,
		CalendarID:   row.CalendarID,
		CalendarName: row.CalendarName,
		SetByID:      row.SetBy,
		SetByName:    row.SetByName,
	}
	if _, err := s.gc.validAccessToken(ctx, row.SetBy); err != nil {
		switch {
		case errors.Is(err, errCalendarBroken):
			st.Status = FamilyCalendarBroken
		case errors.Is(err, errCalendarNotConnected):
			// The FK clears the row on disconnect; treat a race the same.
			return HouseholdCalendarStatus{Status: FamilyCalendarNotConfigured}, nil
		case errors.Is(err, errGoogleUnavailable):
			// Still configured; the listing will report the outage.
		default:
			return HouseholdCalendarStatus{}, err
		}
	}
	return st, nil
}

// ListEntries returns the family calendar's entries overlapping [from, to),
// fetched with the setter's token from the stored calendar only. The range
// rules are checked before anything else is looked up.
func (s *FamilyCalendar) ListEntries(ctx context.Context, userID, householdID uuid.UUID, from, to time.Time) (HouseholdEntries, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return HouseholdEntries{}, err
	}
	var v validator
	if !to.After(from) {
		v.fail("to", "must be after from")
	} else if to.Sub(from) > maxEventRange {
		v.fail("to", "range must not exceed 100 days")
	}
	if err := v.err(); err != nil {
		return HouseholdEntries{}, err
	}
	if s.gc == nil {
		return HouseholdEntries{Status: FamilyCalendarUnavailable, Entries: []GoogleEntry{}}, nil
	}
	row, err := s.q.GetHouseholdCalendar(ctx, householdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HouseholdEntries{Status: FamilyCalendarNotConfigured, Entries: []GoogleEntry{}}, nil
		}
		return HouseholdEntries{}, fmt.Errorf("get household calendar: %w", err)
	}
	token, err := s.gc.validAccessToken(ctx, row.SetBy)
	switch {
	case errors.Is(err, errCalendarBroken):
		return HouseholdEntries{Status: FamilyCalendarBroken, Entries: []GoogleEntry{}}, nil
	case errors.Is(err, errCalendarNotConnected):
		return HouseholdEntries{Status: FamilyCalendarNotConfigured, Entries: []GoogleEntry{}}, nil
	case err != nil:
		return HouseholdEntries{}, err
	}
	entries, err := s.gc.listEvents(ctx, token, row.CalendarID, from, to)
	if err != nil {
		if refusedByGoogle(err) {
			// The token or the calendar no longer works: only the setter
			// can fix it by reconnecting or choosing again.
			return HouseholdEntries{Status: FamilyCalendarBroken, Entries: []GoogleEntry{}}, nil
		}
		return HouseholdEntries{}, err
	}
	return HouseholdEntries{Status: FamilyCalendarOK, Entries: entries}, nil
}
