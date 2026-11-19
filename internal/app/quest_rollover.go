package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/db"
)

// questRolloverBatch is how many due households one listing returns; the
// rollover pages through them until none are left.
const questRolloverBatch = 100

// questRolloverInterval is how often RunQuestRollover looks for households
// whose local day has turned.
const questRolloverInterval = time.Minute

// RunQuestRollover rolls over every due household once at startup, so a
// midnight missed while the server was down runs on the next start, and
// then every questRolloverInterval. It blocks until ctx is cancelled, then
// returns. A failed run is logged and the next tick tries again; a failed
// household is logged by RolloverDueHouseholds and the others still run.
func RunQuestRollover(ctx context.Context, q *db.Queries, pool pgxBeginner) {
	ticker := time.NewTicker(questRolloverInterval)
	defer ticker.Stop()
	for {
		if n, err := RolloverDueHouseholds(ctx, q, pool, time.Now()); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("quest rollover: run", "error", err)
		} else if n > 0 {
			slog.Info("quest rollover: assigned scheduled quests", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RolloverDueHouseholds runs the rollover of every household with a schedule
// due at now: a one-off dated on or before the household's local date at now
// and after the last date it rolled over, or a repeated schedule dated on or
// before that local date while the household has not rolled over to it yet
// (a repeated schedule is judged at every local midnight). Each household rolls over in a transaction
// of its own (rolloverHousehold). A household that fails is logged with its
// cause and left for the next run; the others still run. It returns how many
// quests were assigned, and an error only when the due households cannot be
// listed or ctx ends.
//
// It is a system job, not a member action: there is no acting user, and each
// statement is keyed by the household_id of the listed household.
func RolloverDueHouseholds(ctx context.Context, q *db.Queries, pool pgxBeginner, now time.Time) (int, error) {
	assigned := 0
	after := uuid.Nil
	for {
		ids, err := q.ListHouseholdsDueForRollover(ctx, db.ListHouseholdsDueForRolloverParams{
			AfterID: after,
			Now:     pgtype.Timestamptz{Time: now, Valid: true},
			MaxRows: questRolloverBatch,
		})
		if err != nil {
			return assigned, fmt.Errorf("list households due for rollover: %w", err)
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return assigned, err
			}
			n, err := rolloverHousehold(ctx, q, pool, id, now)
			if err != nil {
				slog.Error("quest rollover: household", "household_id", id, "error", err)
				continue
			}
			assigned += n
		}
		if len(ids) < questRolloverBatch {
			return assigned, nil
		}
		after = ids[len(ids)-1]
	}
}

// rolloverHousehold rolls one household over to its local date at now, in
// one transaction:
//
//  1. the household row is locked, so one rollover of it (or one schedule
//     created for its today) runs at a time, and its last rollover date is
//     read under that lock: a household already rolled over to that date
//     changes nothing, so a second or concurrent run never assigns twice;
//  2. every member row of the household is locked, in id order, before any
//     quest row (member first, as Accept, lockQuestMembers and
//     FamilyService.Remove lock);
//  3. the end-of-day rule (INT-0176) is applied to the household at now:
//     every held quest whose claim has run out goes back to the pool, so a
//     quest just put back can be assigned again at once;
//  4. the due schedules are assigned under ClaimQuest's rules (not held, not
//     resting, member within the age range): a one-off dated after the last
//     rollover date, and every repeated schedule already started, whatever
//     its date, so a repeated quest that was abandoned, expired in step 3 or
//     came off its cooldown is assigned again;
//  5. the one-off schedules just judged end, unless skipped for age; a
//     repeated schedule is kept, assigned or skipped, for the next pass;
//  6. the household is marked rolled over to its local date.
//
// A household deleted meanwhile assigns nothing.
func rolloverHousehold(ctx context.Context, q *db.Queries, pool pgxBeginner, householdID uuid.UUID, now time.Time) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := q.WithTx(tx)

	if _, err := qtx.LockHouseholdForRollover(ctx, householdID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("lock household: %w", err)
	}
	ts := pgtype.Timestamptz{Time: now, Valid: true}
	state, err := qtx.GetHouseholdRolloverState(ctx, db.GetHouseholdRolloverStateParams{Now: ts, HouseholdID: householdID})
	if err != nil {
		return 0, fmt.Errorf("get rollover state: %w", err)
	}
	if state.LastRolloverDate.Valid && !state.LastRolloverDate.Time.Before(state.LocalDate.Time) {
		return 0, nil
	}
	if _, err := qtx.LockHouseholdMembers(ctx, householdID); err != nil {
		return 0, fmt.Errorf("lock members: %w", err)
	}
	if _, err := qtx.ExpireHouseholdQuestClaims(ctx, db.ExpireHouseholdQuestClaimsParams{HouseholdID: householdID, Now: ts}); err != nil {
		return 0, fmt.Errorf("expire quest claims: %w", err)
	}
	n, err := assignSchedules(ctx, qtx, householdID, ts, state.LocalDate, state.LastRolloverDate, nil)
	if err != nil {
		return 0, err
	}
	if err := qtx.MarkHouseholdRolledOver(ctx, db.MarkHouseholdRolledOverParams{HouseholdID: householdID, RolloverDate: state.LocalDate}); err != nil {
		return 0, fmt.Errorf("mark rolled over: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}

// assignSchedules assigns the household's one-off schedules dated after
// afterDate (any, when not valid) and up to localDate, and its repeated
// schedules dated up to localDate, narrowed to scheduleID when it is set,
// then ends the one-off schedules it judged (all but those skipped for age).
// Repeated schedules are never ended here. The caller holds the household's
// lock and the member locks.
func assignSchedules(ctx context.Context, qtx *db.Queries, householdID uuid.UUID, now pgtype.Timestamptz, localDate, afterDate pgtype.Date, scheduleID *uuid.UUID) (int, error) {
	rows, err := qtx.AssignDueSchedules(ctx, db.AssignDueSchedulesParams{
		Now: now, HouseholdID: householdID, LocalDate: localDate, AfterDate: afterDate, ScheduleID: scheduleID,
	})
	if err != nil {
		return 0, fmt.Errorf("assign due schedules: %w", err)
	}
	if _, err := qtx.DeleteFinishedSchedules(ctx, db.DeleteFinishedSchedulesParams{
		HouseholdID: householdID, LocalDate: localDate, AfterDate: afterDate, ScheduleID: scheduleID,
	}); err != nil {
		return 0, fmt.Errorf("delete finished schedules: %w", err)
	}
	return len(rows), nil
}
