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
	Repeat   bool
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
	Repeat      bool
	CreatedAt   time.Time
}

// Schedule stores a schedule of a quest for a member, one-off or repeated
// (in.Repeat). It needs the
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
		Repeat:      in.Repeat,
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

// UpcomingQuest is one of a member's schedules as the member sees it ahead
// of time. StartDate and NextDate are household-local dates, YYYY-MM-DD;
// NextDate is the date the next rollover that can assign it falls on.
type UpcomingQuest struct {
	ID        uuid.UUID
	QuestID   uuid.UUID
	QuestName string
	StartDate string
	NextDate  string
	Repeat    bool
}

// Upcoming returns the member's schedules, ordered by next date and then
// quest name; an empty, non-nil slice when there are none. Any household
// member may read it; a non-member of the household, or a member id not in
// it, gets ErrNotFound. A one-off whose date has passed (kept after an age
// skip) is left out.
func (s *QuestScheduleService) Upcoming(ctx context.Context, userID, householdID, memberID uuid.UUID) ([]UpcomingQuest, error) {
	h, err := getHousehold(ctx, s.q, userID, householdID)
	if err != nil {
		return nil, err
	}
	if _, err := s.q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: householdID, ID: memberID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get family member: %w", err)
	}
	loc, err := time.LoadLocation(h.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load household timezone %q: %w", h.Timezone, err)
	}
	now := time.Now()
	state, err := s.q.GetHouseholdRolloverState(ctx, db.GetHouseholdRolloverStateParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, HouseholdID: householdID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get rollover state: %w", err)
	}
	var lastRollover *time.Time
	if state.LastRolloverDate.Valid {
		d := state.LastRolloverDate.Time
		lastRollover = &d
	}
	rows, err := s.q.ListMemberUpcomingSchedules(ctx, db.ListMemberUpcomingSchedulesParams{HouseholdID: householdID, MemberID: memberID})
	if err != nil {
		return nil, fmt.Errorf("list member upcoming schedules: %w", err)
	}
	out := make([]UpcomingQuest, 0, len(rows))
	for _, r := range rows {
		var availableAt *time.Time
		if r.AvailableAt.Valid {
			a := r.AvailableAt.Time
			availableAt = &a
		}
		start := r.StartDate.Time.Format(time.DateOnly)
		next, ok := NextDate(start, r.Repeat, availableAt, lastRollover, now, loc)
		if !ok {
			continue
		}
		out = append(out, UpcomingQuest{
			ID: r.ID, QuestID: r.QuestID, QuestName: r.QuestName,
			StartDate: start, NextDate: next, Repeat: r.Repeat,
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

// NextDate is the household-local date (YYYY-MM-DD) a schedule next comes
// due, from the schedule's start date and repeat flag, its quest's
// available_at (nil when the quest is not resting), the household's last
// rollover date (nil when it never rolled over; a date at UTC midnight), now
// and the household's timezone.
//
// A one-off shows its start date; ok is false when that date is before the
// local today, so a passed one-off is not listed. A repeated schedule shows
// the latest of its start date, the next local day not yet rolled over
// (today when the household never rolled over or last rolled over before
// yesterday) and the date of the first local midnight at or after
// available_at, since the rollover assigns a resting quest only at a pass
// that available_at has reached. ok is false only when
// startDate is not a YYYY-MM-DD date.
func NextDate(startDate string, repeat bool, availableAt, lastRolloverDate *time.Time, now time.Time, loc *time.Location) (string, bool) {
	start, err := time.Parse(time.DateOnly, startDate)
	if err != nil {
		return "", false
	}
	today := localDate(now, loc)
	if !repeat {
		if start.Before(today) {
			return "", false
		}
		return start.Format(time.DateOnly), true
	}
	next := start
	notRolled := today
	if lastRolloverDate != nil {
		last := time.Date(lastRolloverDate.Year(), lastRolloverDate.Month(), lastRolloverDate.Day(), 0, 0, 0, 0, time.UTC)
		if after := last.AddDate(0, 0, 1); after.After(notRolled) {
			notRolled = after
		}
	}
	if notRolled.After(next) {
		next = notRolled
	}
	if availableAt != nil {
		if rest := firstMidnightFrom(*availableAt, loc); rest.After(next) {
			next = rest
		}
	}
	return next.Format(time.DateOnly), true
}

// firstMidnightFrom is the local date (at UTC midnight) of the first local
// midnight at or after t in loc: t's own date when t is exactly that
// midnight, else the day after. The rollover assigns a resting quest only
// at a pass with available_at at or before it, so a rest ending mid-day is
// assigned at the next midnight.
func firstMidnightFrom(t time.Time, loc *time.Location) time.Time {
	d := localDate(t, loc)
	l := t.In(loc)
	if l.Equal(time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)) {
		return d
	}
	return d.AddDate(0, 0, 1)
}

// localDate is t's calendar date in loc, at UTC midnight.
func localDate(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
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
		Repeat:      r.Repeat,
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
