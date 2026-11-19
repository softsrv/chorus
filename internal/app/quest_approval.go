package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/db"
)

// ListPending returns the completions waiting for approval, one per
// mark-done (a shared quest's is one item naming every member), oldest
// mark-done first, each with the chore's name, points and teamwork bonus and
// every member with their split. Admin only, behind the admin PIN once one is set.
// None waiting is an empty, non-nil slice.
func (s *QuestService) ListPending(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]PendingCompletion, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return nil, err
	}
	rows, err := s.q.ListPendingCompletions(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list pending completions: %w", err)
	}
	out := make([]PendingCompletion, 0, len(rows))
	for _, r := range rows {
		if r.MemberID == nil {
			// The join on family_members drops completions without a member.
			continue
		}
		ids, names := r.MemberIds, r.MemberNames
		if len(ids) == 0 || len(ids) != len(names) {
			// Recorded before sharing: the marker is its one member.
			ids, names = []uuid.UUID{*r.MemberID}, []string{r.MemberName}
		}
		award := splitPoints(r.Points, len(ids))
		members := make([]PendingMember, len(ids))
		for i, id := range ids {
			members[i] = PendingMember{MemberID: id, Name: names[i], Award: award}
		}
		bonus := r.TeamworkBonus
		if len(members) < 2 {
			bonus = 0
		}
		out = append(out, PendingCompletion{
			ID: r.ID, QuestID: r.QuestID, QuestName: r.QuestName, Points: r.Points, TeamworkBonus: bonus,
			MemberID: *r.MemberID, MemberName: r.MemberName, DoneAt: r.DoneAt.Time.UTC(), Members: members,
		})
	}
	return out, nil
}

// Kinds of points award, as stored in points_awards.kind.
const (
	awardKindQuest  = "quest"
	awardKindCombo  = "combo"
	awardKindStreak = "streak"
	// awardKindAdmin is an admin's manual adjustment; its points are signed.
	awardKindAdmin = "admin"
	// awardKindReward is an approved reward redemption; its points are
	// always negative (the reward's cost snapshot, taken away).
	awardKindReward = "reward"
)

// streakBonusFrom is the streak count from which an approval pays the
// quest's streak bonus: the 3rd completion in a row and every one after.
const streakBonusFrom = 3

// creditPoints is the single path that changes a family member's points: it
// adds the signed delta points to the member's total (negative only for an
// admin adjustment or an approved reward redemption, whose callers check the
// total stays at or above 0) and
// writes the points_awards history row that explains it, both on q, which
// must be bound to the caller's
// transaction so that the credit, its history and the caller's own writes
// commit or roll back together. earnedAt is the moment the award was earned.
func creditPoints(ctx context.Context, q *db.Queries, householdID, memberID uuid.UUID, kind, reason string, points int32, earnedAt pgtype.Timestamptz) (db.FamilyMember, error) {
	member, err := q.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: householdID, ID: memberID, Delta: points})
	if err != nil {
		return db.FamilyMember{}, fmt.Errorf("credit points: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return db.FamilyMember{}, fmt.Errorf("generate points award id: %w", err)
	}
	if _, err := q.InsertPointsAward(ctx, db.InsertPointsAwardParams{
		ID:          id,
		HouseholdID: householdID,
		MemberID:    memberID,
		Kind:        kind,
		Reason:      reason,
		Points:      points,
		EarnedAt:    earnedAt,
	}); err != nil {
		return db.FamilyMember{}, fmt.Errorf("record points award: %w", err)
	}
	return member, nil
}

// ComboBonus is one combo bonus paid by an approval.
type ComboBonus struct {
	ComboID     uuid.UUID
	Name        string
	BonusPoints int32
}

// StreakBonus is what an approval did with the member's streak: Count is
// the streak as fixed at the completion's mark-done (0 for a completion
// marked done before streaks existed) and Points the streak bonus paid, 0
// when none.
type StreakBonus struct {
	Points int32
	Count  int32
}

// ApprovedMember is one member an approval credited: their balance after
// the credit (combo bonuses included), their split of the chore's points and
// the teamwork bonus paid on top (0 unless more than one member was on it).
type ApprovedMember struct {
	Member        db.FamilyMember
	PointsAwarded int32
	TeamworkBonus int32
	Streak        StreakBonus
}

// Approve approves one pending completion and credits every member it
// covers, in one transaction: either all of it happens or none does. Every
// credit (each member's split, teamwork bonus, streak bonus and combo
// bonuses) goes through creditPoints and so also writes that member's points
// history entry, earned at the completion's approval time. Admin
// only, behind the admin PIN once one is set. A completion that is not
// pending (approved, rejected, cancelled, or not in this household) is
// ErrNotFound, so a repeated or concurrent approval credits the points once.
// The quest itself is not touched: its cooldown started when the member
// marked it done.
//
// Each of the n members recorded on the completion at approval gets the
// chore's current points divided by n, rounded up, plus the quest's current
// teamwork bonus when n is more than 1. A completion that is not shared (n is
// 1) pays its one member the full points and no bonus. If shareable was
// turned off since the mark-done, the split still follows the recorded
// members and the bonus is 0 (the database keeps a bonus only on a
// shareable quest).
//
// From the marker's 3rd completion of the quest in a row (the streak count
// fixed at mark-done, however late the approval), a quest with a streak
// bonus above 0 also pays that bonus; that is paid to the member who marked
// it done (the only member whose streak moved at mark-done), and their
// ApprovedMember.Streak says what was paid and the count.
//
// The approval also pays every combo it finishes, for each credited member:
// a combo of two or more chores each of which the member did on the same
// household-local day (by each completion's done_at, however late the
// approvals), using approved completions not yet counted toward that combo.
// The returned member is the one who marked it done; it and every entry of
// the returned members carry points that include the bonuses. The returned
// bonus slice is empty, never nil, when none paid.
func (s *QuestService) Approve(ctx context.Context, userID, householdID, completionID uuid.UUID, adminPin string) (Completion, db.FamilyMember, []ComboBonus, []ApprovedMember, error) {
	fail := func(err error) (Completion, db.FamilyMember, []ComboBonus, []ApprovedMember, error) {
		return Completion{}, db.FamilyMember{}, nil, nil, err
	}
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return fail(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fail(fmt.Errorf("begin tx: %w", err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	// Lock order is member rows (ascending id), then completion, the order
	// FamilyService.Remove and the other quest actions use (member first).
	// Locking the completion first would deadlock against a concurrent removal.
	ref, err := notFoundIfNoRows(qtx.GetPendingCompletionMembers(ctx, db.GetPendingCompletionMembersParams{HouseholdID: householdID, ID: completionID}))
	if err != nil {
		return fail(err)
	}
	if ref.MemberID == nil {
		// The query reads only completions that still have a member.
		return fail(fmt.Errorf("approve completion %s: no member", completionID))
	}
	markerID := *ref.MemberID
	toLock := append([]uuid.UUID{markerID}, ref.MemberIds...)
	slices.SortFunc(toLock, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	locked := map[uuid.UUID]bool{}
	for _, id := range slices.Compact(toLock) {
		if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: householdID, ID: id})); err != nil {
			if errors.Is(err, ErrNotFound) && id != markerID {
				// Removed meanwhile: their part of the completion went with them.
				continue
			}
			// The marker was removed meanwhile; the completion is no longer theirs.
			return fail(err)
		}
		locked[id] = true
	}
	// Re-read under the locks: a concurrent approval, reject or removal that
	// committed first leaves no pending row, so this one is ErrNotFound.
	row, err := notFoundIfNoRows(qtx.ApproveCompletion(ctx, db.ApproveCompletionParams{HouseholdID: householdID, ID: completionID, MemberID: markerID, StatusBy: userID}))
	if err != nil {
		return fail(err)
	}
	members := row.MemberIds
	if len(members) == 0 {
		// Recorded before quest sharing: the marker is its one member.
		members = []uuid.UUID{markerID}
	}
	for _, id := range members {
		if !locked[id] {
			// Members join a completion only at mark-done, so the locked set
			// must cover it; anything else rolls the approval back.
			return fail(fmt.Errorf("approve completion %s: member %s not locked", completionID, id))
		}
	}
	split := splitPoints(row.Points, len(members))
	bonus := row.TeamworkBonus
	if len(members) < 2 {
		bonus = 0
	}
	// Validation keeps points + bonus inside int32; a row that is not (written
	// around the service) is refused rather than credited a wrapped amount.
	total64 := int64(split) + int64(bonus)
	if total64 > math.MaxInt32 {
		return fail(fmt.Errorf("approve completion %s: award %d exceeds the points range", completionID, total64))
	}
	total := int32(total64)
	credited := make([]ApprovedMember, 0, len(members))
	bonuses := []ComboBonus{}
	var marker db.FamilyMember
	for _, id := range members {
		if err := qtx.SetCompletionMemberAward(ctx, db.SetCompletionMemberAwardParams{
			PointsAwarded: pgtype.Int4{Int32: total, Valid: true}, HouseholdID: householdID, CompletionID: completionID, MemberID: id,
		}); err != nil {
			return fail(fmt.Errorf("record award: %w", err))
		}
		member, err := creditPoints(ctx, qtx, householdID, id, awardKindQuest, row.QuestName, split, row.ApprovedAt)
		if err != nil {
			return fail(err)
		}
		if bonus > 0 {
			// The teamwork bonus is its own history entry beside the split.
			if member, err = creditPoints(ctx, qtx, householdID, id, awardKindQuest, row.QuestName, bonus, row.ApprovedAt); err != nil {
				return fail(err)
			}
		}
		var streak StreakBonus
		if id == markerID {
			// Only the marker's streak moved at mark-done (streak_count).
			if member, streak, err = payStreak(ctx, qtx, householdID, member, row); err != nil {
				return fail(err)
			}
		}
		member, paid, err := payCombos(ctx, qtx, householdID, row.QuestID, member, row.DoneAt, row.ApprovedAt)
		if err != nil {
			return fail(err)
		}
		if id == markerID {
			marker = member
			bonuses = paid
		}
		credited = append(credited, ApprovedMember{Member: member, PointsAwarded: split, TeamworkBonus: bonus, Streak: streak})
	}
	if marker.ID != markerID {
		// The marker left the quest's members (removed meanwhile with the
		// completion handed on): report their unchanged balance.
		m, err := qtx.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: householdID, ID: markerID})
		if err != nil {
			return fail(fmt.Errorf("read marker: %w", err))
		}
		marker = m
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(fmt.Errorf("commit: %w", err))
	}
	approvedAt := row.ApprovedAt.Time.UTC()
	return Completion{
		ID:         row.ID,
		QuestID:    row.QuestID,
		MemberID:   &markerID,
		MemberIDs:  members,
		DoneAt:     row.DoneAt.Time.UTC(),
		ApprovedAt: &approvedAt,
		Status:     "approved",
	}, marker, bonuses, credited, nil
}

// payStreak runs inside Approve's transaction (qtx), after the chore's
// points were credited. row.StreakCount is the member's streak for the quest
// as fixed when this completion was marked done (never recomputed now);
// from the 3rd in a row on, a quest with a streak bonus above 0 pays it:
// the credit and its points history entry (kind streak, reason
// "streak: <quest name> <count>", earned at the approval time). The
// returned StreakBonus has Points 0 when nothing was paid. Any error is
// returned for the caller to roll back.
func payStreak(ctx context.Context, qtx *db.Queries, householdID uuid.UUID, member db.FamilyMember, row db.ApproveCompletionRow) (db.FamilyMember, StreakBonus, error) {
	if !row.StreakCount.Valid {
		// Marked done before streaks existed: no streak to pay.
		return member, StreakBonus{}, nil
	}
	streak := StreakBonus{Count: row.StreakCount.Int32}
	if streak.Count < streakBonusFrom || row.StreakBonus <= 0 {
		return member, streak, nil
	}
	reason := fmt.Sprintf("streak: %s %d", row.QuestName, streak.Count)
	member, err := creditPoints(ctx, qtx, householdID, member.ID, awardKindStreak, reason, row.StreakBonus, row.ApprovedAt)
	if err != nil {
		return db.FamilyMember{}, StreakBonus{}, fmt.Errorf("credit streak bonus: %w", err)
	}
	streak.Points = row.StreakBonus
	return member, streak, nil
}

// payCombos runs inside Approve's transaction (qtx), after the completion
// was approved. doneAt is that completion's mark-done time, which decides
// the household day it counts toward. earnedAt is the approval time, which
// dates the points history entry. It locks the combos containing the
// quest (so concurrent approvals of one combo's chores serialize and the
// later one sees the earlier one's approval), then pays each combo the member
// has finished: an award row with name and points snapshots, one claimed
// completion per chore, and the bonus credit with its points history entry
// (kind combo, the combo's name, earned at earnedAt). Any error is returned
// for the caller to roll back.
func payCombos(ctx context.Context, qtx *db.Queries, householdID, questID uuid.UUID, member db.FamilyMember, doneAt, earnedAt pgtype.Timestamptz) (db.FamilyMember, []ComboBonus, error) {
	if _, err := qtx.LockCombosForQuest(ctx, db.LockCombosForQuestParams{HouseholdID: householdID, QuestID: questID}); err != nil {
		return db.FamilyMember{}, nil, fmt.Errorf("lock combos: %w", err)
	}
	achieved, err := qtx.ListCombosCompletedByMemberOnDay(ctx, db.ListCombosCompletedByMemberOnDayParams{
		HouseholdID: householdID, QuestID: questID, MemberID: member.ID, DoneAt: doneAt,
	})
	if err != nil {
		return db.FamilyMember{}, nil, fmt.Errorf("find achieved combos: %w", err)
	}
	bonuses := make([]ComboBonus, 0, len(achieved))
	for _, c := range achieved {
		awardID, err := uuid.NewV7()
		if err != nil {
			return db.FamilyMember{}, nil, fmt.Errorf("new award id: %w", err)
		}
		comboID := c.ID
		if _, err := qtx.InsertComboAward(ctx, db.InsertComboAwardParams{
			ID: awardID, HouseholdID: householdID, ComboID: &comboID, MemberID: member.ID,
			ComboName: c.Name, Points: c.BonusPoints, AwardDay: c.AwardDay,
		}); err != nil {
			return db.FamilyMember{}, nil, fmt.Errorf("record combo award: %w", err)
		}
		claimed, err := qtx.ClaimComboCompletions(ctx, db.ClaimComboCompletionsParams{
			ComboAwardID: awardID, MemberID: member.ID, AwardDay: c.AwardDay, HouseholdID: householdID, ComboID: c.ID,
		})
		if err != nil {
			return db.FamilyMember{}, nil, fmt.Errorf("claim combo completions: %w", err)
		}
		if claimed != int64(c.ChoreCount) {
			// Under the combo lock the set just checked must still be there.
			return db.FamilyMember{}, nil, fmt.Errorf("claim combo %s: claimed %d completions, want %d", c.ID, claimed, c.ChoreCount)
		}
		member, err = creditPoints(ctx, qtx, householdID, member.ID, awardKindCombo, c.Name, c.BonusPoints, earnedAt)
		if err != nil {
			return db.FamilyMember{}, nil, fmt.Errorf("credit combo bonus: %w", err)
		}
		bonuses = append(bonuses, ComboBonus{ComboID: c.ID, Name: c.Name, BonusPoints: c.BonusPoints})
	}
	return member, bonuses, nil
}

// Reject rejects one pending completion: no points are added. If that
// completion started the quest's running cooldown and the cooldown has not
// ended, the cooldown is cancelled and the chore is available again right
// away. The quest's holder and the member's approved completions are
// untouched. The streak step the completion made at mark-done is undone in
// the same transaction,
// even when a later mark-done of the quest came after it: the member's
// streak for the quest is recomputed from their remaining pending and
// approved completions (RecomputeMemberQuestStreak), which also renumbers
// their other pending completions of the quest; approved ones keep their
// streak count and paid bonuses. With no completion left the streak is
// removed. Admin only, behind the admin PIN once one is set. A completion
// that is not pending (or not in this household) is ErrNotFound.
//
// Lock order is member row, then completion, the order Approve, MarkDone and
// FamilyService.Remove use, so a reject serializes with the member's
// mark-done and approvals instead of deadlocking against them.
func (s *QuestService) Reject(ctx context.Context, userID, householdID, completionID uuid.UUID, adminPin string) (Completion, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Completion{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Completion{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	ref, err := notFoundIfNoRows(qtx.GetPendingCompletionMembers(ctx, db.GetPendingCompletionMembersParams{HouseholdID: householdID, ID: completionID}))
	if err != nil {
		return Completion{}, err
	}
	if ref.MemberID == nil {
		// The query reads only completions that still have a member.
		return Completion{}, fmt.Errorf("reject completion %s: no member", completionID)
	}
	if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: householdID, ID: *ref.MemberID})); err != nil {
		// The member was removed meanwhile; their completion is cancelled.
		return Completion{}, err
	}
	// Re-read under the lock: a concurrent approval, reject or removal that
	// committed first leaves no pending row, so this one is ErrNotFound.
	row, err := notFoundIfNoRows(qtx.RejectCompletion(ctx, db.RejectCompletionParams{HouseholdID: householdID, ID: completionID, StatusBy: userID}))
	if err != nil {
		return Completion{}, err
	}
	if row.MemberID != nil {
		if err := qtx.RecomputeMemberQuestStreak(ctx, db.RecomputeMemberQuestStreakParams{
			HouseholdID: householdID, MemberID: *row.MemberID, QuestID: row.QuestID,
		}); err != nil {
			return Completion{}, fmt.Errorf("recompute streak: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Completion{}, fmt.Errorf("commit: %w", err)
	}
	return Completion{
		ID:       row.ID,
		QuestID:  row.QuestID,
		MemberID: row.MemberID,
		DoneAt:   row.DoneAt.Time.UTC(),
		Status:   row.Status,
	}, nil
}
