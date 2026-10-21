package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/db"
)

const (
	// maxEventSpan bounds a single event's duration.
	maxEventSpan = 366 * 24 * time.Hour
	// maxEventRange bounds a calendar range query.
	maxEventRange = 100 * 24 * time.Hour
	// maxEventsPerRange caps rows returned by one range query.
	maxEventsPerRange = 1000
)

// EventInput is the editable part of an event.
type EventInput struct {
	Title       string
	Description string
	Location    string
	StartsAt    time.Time
	EndsAt      time.Time
	AllDay      bool
}

// EventService manages calendar events within a household. Any household
// member may create, edit, and delete events.
type EventService struct {
	q *db.Queries
}

// NewEventService constructs an EventService.
func NewEventService(q *db.Queries) *EventService {
	return &EventService{q: q}
}

// ListRange returns events overlapping [from, to). The range may span at most
// 100 days; clients page through longer periods.
func (s *EventService) ListRange(ctx context.Context, userID, householdID uuid.UUID, from, to time.Time) ([]db.Event, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, err
	}
	var v validator
	if !to.After(from) {
		v.fail("to", "must be after from")
	} else if to.Sub(from) > maxEventRange {
		v.fail("to", "range must not exceed 100 days")
	}
	if err := v.err(); err != nil {
		return nil, err
	}
	events, err := s.q.ListEventsInRange(ctx, db.ListEventsInRangeParams{
		HouseholdID: householdID,
		RangeStart:  pgtype.Timestamptz{Time: from, Valid: true},
		RangeEnd:    pgtype.Timestamptz{Time: to, Valid: true},
		RowLimit:    maxEventsPerRange,
	})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

// Get returns one event.
func (s *EventService) Get(ctx context.Context, userID, householdID, eventID uuid.UUID) (db.Event, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.Event{}, err
	}
	return notFoundIfNoRows(s.q.GetEvent(ctx, db.GetEventParams{HouseholdID: householdID, ID: eventID}))
}

// Create adds an event to the household calendar.
func (s *EventService) Create(ctx context.Context, userID, householdID uuid.UUID, in EventInput) (db.Event, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.Event{}, err
	}
	in, err := validateEvent(in)
	if err != nil {
		return db.Event{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return db.Event{}, fmt.Errorf("generate event id: %w", err)
	}
	event, err := s.q.CreateEvent(ctx, db.CreateEventParams{
		ID:          id,
		HouseholdID: householdID,
		Title:       in.Title,
		Description: in.Description,
		Location:    in.Location,
		StartsAt:    pgtype.Timestamptz{Time: in.StartsAt, Valid: true},
		EndsAt:      pgtype.Timestamptz{Time: in.EndsAt, Valid: true},
		AllDay:      in.AllDay,
		CreatedBy:   &userID,
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("create event: %w", err)
	}
	return event, nil
}

// Update replaces an event's editable fields.
func (s *EventService) Update(ctx context.Context, userID, householdID, eventID uuid.UUID, in EventInput) (db.Event, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.Event{}, err
	}
	in, err := validateEvent(in)
	if err != nil {
		return db.Event{}, err
	}
	return notFoundIfNoRows(s.q.UpdateEvent(ctx, db.UpdateEventParams{
		HouseholdID: householdID,
		ID:          eventID,
		Title:       in.Title,
		Description: in.Description,
		Location:    in.Location,
		StartsAt:    pgtype.Timestamptz{Time: in.StartsAt, Valid: true},
		EndsAt:      pgtype.Timestamptz{Time: in.EndsAt, Valid: true},
		AllDay:      in.AllDay,
	}))
}

// Delete removes an event.
func (s *EventService) Delete(ctx context.Context, userID, householdID, eventID uuid.UUID) error {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return err
	}
	n, err := s.q.DeleteEvent(ctx, db.DeleteEventParams{HouseholdID: householdID, ID: eventID})
	if err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func validateEvent(in EventInput) (EventInput, error) {
	var v validator
	in.Title = v.text("title", in.Title, 1, 200, false)
	in.Description = v.text("description", in.Description, 0, 2000, true)
	in.Location = v.text("location", in.Location, 0, 200, false)
	if in.StartsAt.IsZero() {
		v.fail("starts_at", "is required")
	}
	if in.EndsAt.IsZero() {
		v.fail("ends_at", "is required")
	}
	if !in.StartsAt.IsZero() && !in.EndsAt.IsZero() {
		if in.EndsAt.Before(in.StartsAt) {
			v.fail("ends_at", "must not be before starts_at")
		} else if in.EndsAt.Sub(in.StartsAt) > maxEventSpan {
			v.fail("ends_at", "event must not span more than 366 days")
		}
	}
	return in, v.err()
}
