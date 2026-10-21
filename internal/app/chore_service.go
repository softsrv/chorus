package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/db"
)

// MaxPageSize caps list endpoints so a single request can't pull an unbounded
// number of rows.
const MaxPageSize = 100

// ChoreInput is the editable part of a chore.
type ChoreInput struct {
	Title       string
	Description string
	AssigneeID  *uuid.UUID
	DueOn       *time.Time // date only; time-of-day is ignored
}

// ChoreFilter narrows ListChores. Status is "open", "done", or "all".
type ChoreFilter struct {
	Status     string
	AssigneeID *uuid.UUID
	Limit      int
	Offset     int
}

// ChoreService manages chores within a household. Any household member may
// create, edit, complete, and delete chores.
type ChoreService struct {
	q *db.Queries
}

// NewChoreService constructs a ChoreService.
func NewChoreService(q *db.Queries) *ChoreService {
	return &ChoreService{q: q}
}

// List returns up to f.Limit chores matching the filter, and whether more
// remain after them.
func (s *ChoreService) List(ctx context.Context, userID, householdID uuid.UUID, f ChoreFilter) ([]db.Chore, bool, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, false, err
	}
	var v validator
	switch f.Status {
	case "":
		f.Status = "open"
	case "open", "done", "all":
	default:
		v.fail("status", "must be one of open, done, all")
	}
	if f.Limit < 1 || f.Limit > MaxPageSize {
		v.fail("limit", fmt.Sprintf("must be between 1 and %d", MaxPageSize))
	}
	if f.Offset < 0 {
		v.fail("offset", "must not be negative")
	}
	if err := v.err(); err != nil {
		return nil, false, err
	}
	// Fetch one extra row to learn whether another page exists.
	chores, err := s.q.ListChores(ctx, db.ListChoresParams{
		HouseholdID: householdID,
		AssigneeID:  f.AssigneeID,
		Status:      f.Status,
		RowLimit:    int32(f.Limit + 1),
		RowOffset:   int32(f.Offset),
	})
	if err != nil {
		return nil, false, fmt.Errorf("list chores: %w", err)
	}
	if len(chores) > f.Limit {
		return chores[:f.Limit], true, nil
	}
	return chores, false, nil
}

// Get returns one chore.
func (s *ChoreService) Get(ctx context.Context, userID, householdID, choreID uuid.UUID) (db.Chore, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.Chore{}, err
	}
	return notFoundIfNoRows(s.q.GetChore(ctx, db.GetChoreParams{HouseholdID: householdID, ID: choreID}))
}

// Create adds a chore to the household.
func (s *ChoreService) Create(ctx context.Context, userID, householdID uuid.UUID, in ChoreInput) (db.Chore, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.Chore{}, err
	}
	in, err := validateChore(in)
	if err != nil {
		return db.Chore{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return db.Chore{}, fmt.Errorf("generate chore id: %w", err)
	}
	chore, err := s.q.CreateChore(ctx, db.CreateChoreParams{
		ID:          id,
		HouseholdID: householdID,
		Title:       in.Title,
		Description: in.Description,
		AssigneeID:  in.AssigneeID,
		DueOn:       toPgDate(in.DueOn),
		CreatedBy:   &userID,
	})
	if err != nil {
		return db.Chore{}, assigneeError(err)
	}
	return chore, nil
}

// Update replaces a chore's editable fields.
func (s *ChoreService) Update(ctx context.Context, userID, householdID, choreID uuid.UUID, in ChoreInput) (db.Chore, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.Chore{}, err
	}
	in, err := validateChore(in)
	if err != nil {
		return db.Chore{}, err
	}
	chore, err := s.q.UpdateChore(ctx, db.UpdateChoreParams{
		HouseholdID: householdID,
		ID:          choreID,
		Title:       in.Title,
		Description: in.Description,
		AssigneeID:  in.AssigneeID,
		DueOn:       toPgDate(in.DueOn),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Chore{}, ErrNotFound
		}
		return db.Chore{}, assigneeError(err)
	}
	return chore, nil
}

// SetCompleted marks a chore done (completed=true) or reopens it.
func (s *ChoreService) SetCompleted(ctx context.Context, userID, householdID, choreID uuid.UUID, completed bool) (db.Chore, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.Chore{}, err
	}
	if completed {
		return notFoundIfNoRows(s.q.SetChoreCompleted(ctx, db.SetChoreCompletedParams{
			HouseholdID: householdID, ID: choreID, CompletedBy: &userID,
		}))
	}
	return notFoundIfNoRows(s.q.SetChoreOpen(ctx, db.SetChoreOpenParams{HouseholdID: householdID, ID: choreID}))
}

// Delete removes a chore.
func (s *ChoreService) Delete(ctx context.Context, userID, householdID, choreID uuid.UUID) error {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return err
	}
	n, err := s.q.DeleteChore(ctx, db.DeleteChoreParams{HouseholdID: householdID, ID: choreID})
	if err != nil {
		return fmt.Errorf("delete chore: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func validateChore(in ChoreInput) (ChoreInput, error) {
	var v validator
	in.Title = v.text("title", in.Title, 1, 200, false)
	in.Description = v.text("description", in.Description, 0, 2000, true)
	return in, v.err()
}

// assigneeError maps the chores→household_members foreign-key violation (the
// assignee isn't in this household) to a validation error.
func assigneeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return &ValidationError{Fields: map[string]string{"assignee_id": "must be a member of this household"}}
	}
	return fmt.Errorf("save chore: %w", err)
}

func toPgDate(t *time.Time) pgtype.Date {
	if t == nil {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

// notFoundIfNoRows maps pgx.ErrNoRows to ErrNotFound for single-row queries.
func notFoundIfNoRows[T any](v T, err error) (T, error) {
	if err != nil {
		var zero T
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrNotFound
		}
		return zero, err
	}
	return v, nil
}
