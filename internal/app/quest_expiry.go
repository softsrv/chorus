package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/softsrv/chorus/internal/db"
)

// questExpiryBatch is the most overdue quests one run of the expiry sweep
// releases; any left over are listed again on the next run.
const questExpiryBatch = 500

// ExpireOverdueQuests returns to the pool every held quest whose claim has
// run out: its claim_expires_at, the household-local midnight ending the day
// it was accepted, has passed. Everyone on the quest drops it; no completion
// is recorded, no points move and available_at is left as it is.
//
// It is a system job, not a member action: there is no acting user, and each
// statement is keyed by the household_id of the listed quest. Each quest is
// released in a transaction of its own, after the row locks of every member
// on it and then the quest's row lock (lockQuestMembers, the order the member
// actions use), and the expiry is re-checked under those locks, so a quest
// marked done or abandoned meanwhile is left alone. A quest that fails is
// logged and left held; the next run lists it again. It returns how many
// quests were released, and an error only when the overdue quests cannot be
// listed.
func (s *QuestService) ExpireOverdueQuests(ctx context.Context) (int, error) {
	overdue, err := s.q.ListExpiredQuestClaims(ctx, questExpiryBatch)
	if err != nil {
		return 0, fmt.Errorf("list expired quest claims: %w", err)
	}
	released := 0
	for _, quest := range overdue {
		if err := ctx.Err(); err != nil {
			return released, err
		}
		ok, err := s.expireQuestClaim(ctx, quest)
		if err != nil {
			slog.Error("quest expiry: release quest", "household_id", quest.HouseholdID, "quest_id", quest.ID, "error", err)
			continue
		}
		if ok {
			released++
		}
	}
	return released, nil
}

// expireQuestClaim releases one overdue quest in its own transaction. It
// reports false when the quest was no longer held past its deadline once
// locked (marked done, abandoned, or its holder removed meanwhile).
func (s *QuestService) expireQuestClaim(ctx context.Context, quest db.ListExpiredQuestClaimsRow) (bool, error) {
	if quest.ClaimedBy == nil {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	if err := lockQuestMembers(ctx, qtx, quest.HouseholdID, quest.ID, *quest.ClaimedBy); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The holder was removed from the household meanwhile, which
			// released the quest already.
			return false, nil
		}
		return false, err
	}
	if _, err := qtx.ExpireQuestClaim(ctx, db.ExpireQuestClaimParams{HouseholdID: quest.HouseholdID, ID: quest.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("expire quest claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}
