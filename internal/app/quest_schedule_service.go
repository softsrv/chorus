package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/db"
)

// QuestScheduleService manages the administrator's scheduled quests: a quest
// to be assigned to a member at the start of a date in the household's
// timezone. The assigning itself is RolloverDueHouseholds.
type QuestScheduleService struct {
	q    *db.Queries
	pool pgxBeginner
}

// NewQuestScheduleService constructs a QuestScheduleService. pool runs the
// transaction that stores a schedule and, for today, assigns it at once.
func NewQuestScheduleService(q *db.Queries, pool pgxBeginner) *QuestScheduleService {
	return &QuestScheduleService{q: q, pool: pool}
}

// ScheduleInput is a schedule request: the household's quest, its member and
// the local date (YYYY-MM-DD) the quest is to be assigned on.
type ScheduleInput struct {
	QuestID  uuid.UUID
	MemberID uuid.UUID
	Date     string
}

// QuestSchedule is one scheduled quest. StartDate is a household-local date,
// YYYY-MM-DD.
type QuestSchedule struct {
	ID          uuid.UUID
	HouseholdID uuid.UUID
	QuestID     uuid.UUID
	QuestName   string
	MemberID    uuid.UUID
	MemberName  string
	StartDate   string
	CreatedAt   time.Time
}

// Schedule stores a one-off schedule of a quest for a member. It needs the
// admin role and, once one is set, the admin PIN (ErrAdminPinRequired,
// ErrWrongPIN); a non-member gets ErrNotFound. A missing date, one that is
// not YYYY-MM-DD, or one before today in the household's timezone is a
// ValidationError on "date". A quest or member not in this household is
// ErrNotFound and nothing is written.
//
// A schedule for today, made after the household already rolled over today,
// is assigned at once under the rollover's rules (a held or resting quest
// ends it, an age skip keeps it); otherwise the next rollover assigns it.
func (s *QuestScheduleService) Schedule(ctx context.Context, userID, householdID uuid.UUID, in ScheduleInput, adminPin string) (QuestSchedule, error) {
	h, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin)
	if err != nil {
		return QuestSchedule{}, err
	}
	loc, err := time.LoadLocation(h.Timezone)
	if err != nil {
		return QuestSchedule{}, fmt.Errorf("load household timezone %q: %w", h.Timezone, err)
	}
	now := time.Now()
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	var v validator
	date := v.scheduleDate(in.Date, today)
	if err := v.err(); err != nil {
		return QuestSchedule{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return QuestSchedule{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	// The household row first, as the rollover locks it, so a schedule for
	// today and a rollover of today never interleave.
	if _, err := notFoundIfNoRows(qtx.LockHouseholdForRollover(ctx, householdID)); err != nil {
		return QuestSchedule{}, err
	}
	startDate := pgtype.Date{Time: date, Valid: true}
	row, err := qtx.CreateQuestSchedule(ctx, db.CreateQuestScheduleParams{
		ID:          uuid.Must(uuid.NewV7()),
		StartDate:   startDate,
		HouseholdID: householdID,
		QuestID:     in.QuestID,
		MemberID:    in.MemberID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isForeignKeyViolation(err) {
			return QuestSchedule{}, ErrNotFound
		}
		return QuestSchedule{}, fmt.Errorf("create quest schedule: %w", err)
	}

	ts := pgtype.Timestamptz{Time: now, Valid: true}
	state, err := qtx.GetHouseholdRolloverState(ctx, db.GetHouseholdRolloverStateParams{Now: ts, HouseholdID: householdID})
	if err != nil {
		return QuestSchedule{}, fmt.Errorf("get rollover state: %w", err)
	}
	if state.LastRolloverDate.Valid && !state.LastRolloverDate.Time.Before(state.LocalDate.Time) &&
		date.Equal(state.LocalDate.Time) {
		// Member row before the quest row, as Accept locks them.
		if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: householdID, ID: in.MemberID})); err != nil {
			return QuestSchedule{}, err
		}
		if _, err := assignSchedules(ctx, qtx, householdID, ts, state.LocalDate, pgtype.Date{}, &row.ID); err != nil {
			return QuestSchedule{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return QuestSchedule{}, fmt.Errorf("commit: %w", err)
	}
	return toQuestSchedule(db.ListQuestSchedulesRow(row)), nil
}

// List returns the household's schedules, soonest date first; an empty,
// non-nil slice when there are none. Admin only, with the admin PIN once one
// is set.
func (s *QuestScheduleService) List(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]QuestSchedule, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return nil, err
	}
	rows, err := s.q.ListQuestSchedules(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list quest schedules: %w", err)
	}
	out := make([]QuestSchedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, toQuestSchedule(r))
	}
	return out, nil
}

// Cancel removes one of the household's schedules, which then never fires.
// A quest it already assigned stays with the member. Admin only, with the
// admin PIN once one is set; a schedule not in this household is
// ErrNotFound.
func (s *QuestScheduleService) Cancel(ctx context.Context, userID, householdID, scheduleID uuid.UUID, adminPin string) error {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return err
	}
	n, err := s.q.DeleteQuestSchedule(ctx, db.DeleteQuestScheduleParams{HouseholdID: householdID, ID: scheduleID})
	if err != nil {
		return fmt.Errorf("delete quest schedule: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func toQuestSchedule(r db.ListQuestSchedulesRow) QuestSchedule {
	return QuestSchedule{
		ID:          r.ID,
		HouseholdID: r.HouseholdID,
		QuestID:     r.QuestID,
		QuestName:   r.QuestName,
		MemberID:    r.MemberID,
		MemberName:  r.MemberName,
		StartDate:   r.StartDate.Time.Format(time.DateOnly),
		CreatedAt:   r.CreatedAt.Time.UTC(),
	}
}

// scheduleDate checks s is a real calendar date written YYYY-MM-DD that is
// not before today (a UTC-midnight date the caller worked out in the
// household's timezone; today itself is allowed). It returns the date at UTC
// midnight.
func (v *validator) scheduleDate(s string, today time.Time) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		v.fail("date", "is required")
		return time.Time{}
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		v.fail("date", "must be a valid date (YYYY-MM-DD)")
		return time.Time{}
	}
	if d.Before(today) {
		v.fail("date", "can't be in the past")
	}
	return d
}

// UpcomingQuest is one of a member's schedules that has not fired yet. NextDate
// is the household-local date (YYYY-MM-DD) it will next be assigned on, see
// nextScheduleDate.
type UpcomingQuest struct {
	ID        uuid.UUID
	QuestID   uuid.UUID
	QuestName string
	StartDate string
	NextDate  string
	Repeat    bool
}

// Upcoming returns a member's schedules with the date each will next be
// assigned, soonest first and by quest name on a tie; an empty, non-nil slice
// when there are none. Any member of the household may read it (no admin PIN);
// a non-member, or a member id outside the household, is ErrNotFound.
func (s *QuestScheduleService) Upcoming(ctx context.Context, userID, householdID, memberID uuid.UUID) ([]UpcomingQuest, error) {
	h, err := getHousehold(ctx, s.q, userID, householdID)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(h.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load household timezone %q: %w", h.Timezone, err)
	}
	if _, err := notFoundIfNoRows(s.q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: householdID, ID: memberID})); err != nil {
		return nil, err
	}
	now := time.Now()
	state, err := s.q.GetHouseholdRolloverState(ctx, db.GetHouseholdRolloverStateParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, HouseholdID: householdID,
	})
	if err != nil {
		return nil, fmt.Errorf("get rollover state: %w", err)
	}
	rows, err := s.q.ListMemberUpcomingSchedules(ctx, db.ListMemberUpcomingSchedulesParams{HouseholdID: householdID, MemberID: memberID})
	if err != nil {
		return nil, fmt.Errorf("list member upcoming schedules: %w", err)
	}
	rolledToday := state.LastRolloverDate.Valid && !state.LastRolloverDate.Time.Before(state.LocalDate.Time)
	out := make([]UpcomingQuest, 0, len(rows))
	for _, r := range rows {
		var availableAt *time.Time
		if r.AvailableAt.Valid {
			availableAt = &r.AvailableAt.Time
		}
		next := nextScheduleDate(r.StartDate.Time, r.Repeat, availableAt, rolledToday, now, loc)
		out = append(out, UpcomingQuest{
			ID:        r.ID,
			QuestID:   r.QuestID,
			QuestName: r.QuestName,
			StartDate: r.StartDate.Time.Format(time.DateOnly),
			NextDate:  next.Format(time.DateOnly),
			Repeat:    r.Repeat,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NextDate != out[j].NextDate {
			return out[i].NextDate < out[j].NextDate
		}
		return out[i].QuestName < out[j].QuestName
	})
	return out, nil
}

// nextScheduleDate is the household-local date, as a UTC-midnight date, a
// schedule will next be assigned on. A one-off is its start date. A repeated
// schedule is the latest of its start date, the next local day not yet rolled
// over (today until today's rollover has run, then tomorrow) and the local
// date its quest rests until (availableAt, nil when it is not resting): a
// midnight pass cannot assign a quest still in its cooldown. Days are counted
// on the calendar in loc, so a 23- or 25-hour day moves nothing.
func nextScheduleDate(start time.Time, repeat bool, availableAt *time.Time, rolledToday bool, now time.Time, loc *time.Location) time.Time {
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	if !repeat {
		return start
	}
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	if rolledToday {
		next = next.AddDate(0, 0, 1)
	}
	if start.After(next) {
		next = start
	}
	if availableAt != nil {
		a := availableAt.In(loc)
		if rest := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC); rest.After(next) {
			next = rest
		}
	}
	return next
}
