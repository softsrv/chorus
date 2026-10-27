package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

// Outcomes of a tag change that Google refused. Google's title is unchanged
// in each case; its answer is never passed on.
var (
	// ErrEntryGone means the entry no longer exists in the family calendar.
	ErrEntryGone = errors.New("calendar entry no longer exists")
	// ErrEntryNotEditable means Google refused the change for this entry.
	ErrEntryNotEditable = errors.New("calendar entry can't be edited")
	// ErrGoogleRejected means Google answered the change with another error.
	ErrGoogleRejected = errors.New("google rejected the calendar change")
)

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

// FamilyEntry is one family calendar entry as members see it. Title is
// Google's title unchanged; Tags are the member ids parsed from its tag
// group. CanTag is true only when the family calendar's connection has
// read_write access and Google reports the entry editable.
type FamilyEntry struct {
	GoogleEntry
	Tags   []uuid.UUID
	CanTag bool
}

// HouseholdEntries is the result of listing a household's family calendar.
// Entries is non-nil (possibly empty) when Status is FamilyCalendarOK.
// Members are the household's current members as tag choices.
type HouseholdEntries struct {
	Status  string
	Entries []FamilyEntry
	Members []TagMember
}

// TaggedEntry is an entry after a tag change: the title Google stored and the
// tags read back from it.
type TaggedEntry struct {
	ID     string
	Title  string
	Tags   []uuid.UUID
	CanTag bool
}

// FamilyCalendar is a household's shared Google calendar: one member who has
// connected Google picks one of their calendars, and every member sees its
// entries, fetched server-side with that member's token. Its only write to
// Google is an entry's title, when a member tags the entry. A nil
// GoogleCalendar means Google Calendar is unavailable here.
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
	case transientByGoogle(err):
		return MyCalendars{}, ErrGoogleUnavailable
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
	case transientByGoogle(err):
		return HouseholdCalendarStatus{}, ErrGoogleUnavailable
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
// fetched with the setter's token from the stored calendar only, each with
// the tags parsed from its title, plus the household's members as tag
// choices. The range rules are checked before anything else is looked up.
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
	members, err := s.tagMembers(ctx, householdID)
	if err != nil {
		return HouseholdEntries{}, err
	}
	none := func(status string) HouseholdEntries {
		return HouseholdEntries{Status: status, Entries: []FamilyEntry{}, Members: members}
	}
	if s.gc == nil {
		return none(FamilyCalendarUnavailable), nil
	}
	row, err := s.q.GetHouseholdCalendar(ctx, householdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return none(FamilyCalendarNotConfigured), nil
		}
		return HouseholdEntries{}, fmt.Errorf("get household calendar: %w", err)
	}
	token, access, err := s.gc.validConnection(ctx, row.SetBy)
	switch {
	case errors.Is(err, errCalendarBroken):
		return none(FamilyCalendarBroken), nil
	case errors.Is(err, errCalendarNotConnected):
		return none(FamilyCalendarNotConfigured), nil
	case err != nil:
		return HouseholdEntries{}, err
	}
	entries, err := s.gc.listEvents(ctx, token, row.CalendarID, from, to)
	if err != nil {
		if transientByGoogle(err) {
			// Rate limited or a Google outage: the connection is fine, so
			// report it as temporary (503 + Retry-After), never as broken.
			slog.WarnContext(ctx, "google calendar: listing temporarily refused", "error", err)
			return HouseholdEntries{}, ErrGoogleUnavailable
		}
		if refusedByGoogle(err) {
			// The token or the calendar no longer works: only the setter
			// can fix it by reconnecting or choosing again.
			return none(FamilyCalendarBroken), nil
		}
		return HouseholdEntries{}, err
	}
	out := make([]FamilyEntry, 0, len(entries))
	for _, e := range entries {
		_, tags, _ := parseTagTitle(e.Title, members)
		out = append(out, FamilyEntry{GoogleEntry: e, Tags: tags, CanTag: access == CalendarReadWrite && e.Editable})
	}
	return HouseholdEntries{Status: FamilyCalendarOK, Entries: out, Members: members}, nil
}

func (s *FamilyCalendar) tagMembers(ctx context.Context, householdID uuid.UUID) ([]TagMember, error) {
	rows, err := s.q.ListHouseholdMembers(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list household members: %w", err)
	}
	return tagMembers(rows), nil
}

// SetEntryTags makes tags (member user ids; empty untags) the tag group of
// one entry of the household's family calendar. Any member may tag; the
// change is sent with the setter's token. The title is rebuilt from Google's
// current title, and only the summary is sent, in a PATCH. It returns the
// title Google stored.
//
// Before any request to Google: a non-member gets ErrNotFound, a tag that is
// not a taggable current member is a ValidationError, and a connection
// without read_write access is ErrForbidden. An entry Google reports as not
// editable is ErrForbidden with no PATCH. Google refusing the PATCH is
// ErrGoogleUnavailable (429, 5xx or a rate-limit 403), ErrEntryGone
// (404/410), ErrForbidden (401), ErrEntryNotEditable (403) or
// ErrGoogleRejected.
func (s *FamilyCalendar) SetEntryTags(ctx context.Context, userID, householdID uuid.UUID, eventID string, tags []uuid.UUID) (TaggedEntry, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return TaggedEntry{}, err
	}
	var v validator
	eventID = v.text("event_id", eventID, 1, 1024, false)
	members, err := s.tagMembers(ctx, householdID)
	if err != nil {
		return TaggedEntry{}, err
	}
	names, bad := tagNames(members, tags)
	if len(bad) > 0 {
		v.fail("tags", "must be household members who can be tagged")
	}
	if err := v.err(); err != nil {
		return TaggedEntry{}, err
	}
	if s.gc == nil {
		return TaggedEntry{}, ErrNotFound
	}
	row, err := s.q.GetHouseholdCalendar(ctx, householdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No family calendar: there is no entry to tag.
			return TaggedEntry{}, ErrNotFound
		}
		return TaggedEntry{}, fmt.Errorf("get household calendar: %w", err)
	}
	token, access, err := s.gc.validConnection(ctx, row.SetBy)
	switch {
	case errors.Is(err, errCalendarNotConnected):
		return TaggedEntry{}, ErrNotFound
	case errors.Is(err, errCalendarBroken):
		return TaggedEntry{}, ErrForbidden
	case err != nil:
		return TaggedEntry{}, err
	}
	if access != CalendarReadWrite {
		return TaggedEntry{}, ErrForbidden
	}
	current, err := s.gc.getEvent(ctx, token, row.CalendarID, eventID)
	if err != nil {
		return TaggedEntry{}, tagGoogleError(err, ErrForbidden)
	}
	if !current.Editable {
		return TaggedEntry{}, ErrForbidden
	}
	base, _, kept := parseTagTitle(current.Title, members)
	stored, err := s.gc.patchEventSummary(ctx, token, row.CalendarID, eventID, composeTagTitle(base, names, kept))
	if err != nil {
		return TaggedEntry{}, tagGoogleError(err, ErrEntryNotEditable)
	}
	_, storedTags, _ := parseTagTitle(stored, members)
	return TaggedEntry{ID: eventID, Title: stored, Tags: storedTags, CanTag: true}, nil
}

// tagGoogleError maps Google's answer to a tag request to the caller's
// outcome: a rate limit (429 or a rate-limit 403) or a 5xx is
// ErrGoogleUnavailable; 404/410 entry gone; 401 (Google refused the token, as
// the listing reports broken) ErrForbidden; another 403 forbidden (as given);
// anything else rejected. The status is logged; Google's answer never reaches
// the caller.
func tagGoogleError(err error, forbidden error) error {
	var apiErr *googleAPIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case transientByGoogle(err):
		slog.Warn("google calendar: tag change temporarily refused", "status", apiErr.status)
		return ErrGoogleUnavailable
	case apiErr.status == http.StatusNotFound, apiErr.status == http.StatusGone:
		return ErrEntryGone
	case apiErr.status == http.StatusUnauthorized:
		slog.Warn("google calendar: tag change refused the token", "status", apiErr.status)
		return ErrForbidden
	case apiErr.status == http.StatusForbidden:
		return forbidden
	default:
		slog.Warn("google calendar: tag change refused", "status", apiErr.status)
		return ErrGoogleRejected
	}
}
