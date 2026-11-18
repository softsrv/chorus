package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/softsrv/chorus/internal/db"
)

// RewardInput is the editable part of a household reward. Cost is required.
type RewardInput struct {
	Description string
	Cost        NumberField
}

// RewardService manages a household's rewards, reads the global reward
// library, and runs reward redemptions. Any household member may read the
// household's rewards; only the administrator (behind the admin PIN once one
// is set) may change them. A member (proven by their PIN) may ask to redeem a
// reward they have enough points for: the request waits for the
// administrator and moves no points. Points move only when the administrator
// approves it, through creditPoints, as a 'reward' points award of minus the
// request's cost snapshot.
type RewardService struct {
	q    *db.Queries
	pool pgxBeginner
}

// NewRewardService constructs a RewardService. pool runs the approval and
// rejection transactions.
func NewRewardService(q *db.Queries, pool pgxBeginner) *RewardService {
	return &RewardService{q: q, pool: pool}
}

// List returns every reward of the household, oldest first. None is an
// empty, non-nil slice.
func (s *RewardService) List(ctx context.Context, userID, householdID uuid.UUID) ([]db.HouseholdReward, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, err
	}
	rewards, err := s.q.ListHouseholdRewards(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list rewards: %w", err)
	}
	return rewards, nil
}

// Get returns one reward of the household.
func (s *RewardService) Get(ctx context.Context, userID, householdID, rewardID uuid.UUID) (db.HouseholdReward, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.HouseholdReward{}, err
	}
	return notFoundIfNoRows(s.q.GetHouseholdReward(ctx, db.GetHouseholdRewardParams{HouseholdID: householdID, ID: rewardID}))
}

// Create adds a reward to the household.
func (s *RewardService) Create(ctx context.Context, userID, householdID uuid.UUID, in RewardInput, adminPin string) (db.HouseholdReward, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdReward{}, err
	}
	v, err := validateReward(in)
	if err != nil {
		return db.HouseholdReward{}, err
	}
	return s.insert(ctx, householdID, nil, v)
}

// Update replaces a reward's description and cost.
func (s *RewardService) Update(ctx context.Context, userID, householdID, rewardID uuid.UUID, in RewardInput, adminPin string) (db.HouseholdReward, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdReward{}, err
	}
	v, err := validateReward(in)
	if err != nil {
		return db.HouseholdReward{}, err
	}
	return notFoundIfNoRows(s.q.UpdateHouseholdReward(ctx, db.UpdateHouseholdRewardParams{
		HouseholdID: householdID,
		ID:          rewardID,
		Description: v.description,
		Cost:        v.cost,
	}))
}

// Delete removes a reward from the household.
func (s *RewardService) Delete(ctx context.Context, userID, householdID, rewardID uuid.UUID, adminPin string) error {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return err
	}
	n, err := s.q.DeleteHouseholdReward(ctx, db.DeleteHouseholdRewardParams{HouseholdID: householdID, ID: rewardID})
	if err != nil {
		return fmt.Errorf("delete reward: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CopyFromLibrary adds a copy of a library reward to the household. The copy
// carries the library's description and cost and remembers its library_id;
// editing it later never changes the library.
func (s *RewardService) CopyFromLibrary(ctx context.Context, userID, householdID, libraryID uuid.UUID, adminPin string) (db.HouseholdReward, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdReward{}, err
	}
	item, err := notFoundIfNoRows(s.q.GetRewardLibraryItem(ctx, libraryID))
	if err != nil {
		return db.HouseholdReward{}, err
	}
	return s.insert(ctx, householdID, &item.ID, validReward{description: item.Description, cost: item.Cost})
}

// ListLibrary returns the global reward library. It holds no household data,
// so any signed-in user may read it.
func (s *RewardService) ListLibrary(ctx context.Context) ([]db.RewardLibrary, error) {
	items, err := s.q.ListRewardLibrary(ctx)
	if err != nil {
		return nil, fmt.Errorf("list reward library: %w", err)
	}
	return items, nil
}

func (s *RewardService) insert(ctx context.Context, householdID uuid.UUID, libraryID *uuid.UUID, v validReward) (db.HouseholdReward, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return db.HouseholdReward{}, fmt.Errorf("generate reward id: %w", err)
	}
	reward, err := s.q.CreateHouseholdReward(ctx, db.CreateHouseholdRewardParams{
		ID:          id,
		HouseholdID: householdID,
		LibraryID:   libraryID,
		Description: v.description,
		Cost:        v.cost,
	})
	if err != nil {
		return db.HouseholdReward{}, fmt.Errorf("create reward: %w", err)
	}
	return reward, nil
}

// validReward is a RewardInput that passed validateReward.
type validReward struct {
	description string
	cost        int32
}

// validateReward checks every field and reports each bad one: description
// 1-200 characters after trimming, cost a required whole number >= 0 that
// fits the column.
func validateReward(in RewardInput) (validReward, error) {
	var v validator
	out := validReward{description: v.text("description", in.Description, 1, 200, false)}
	if !in.Cost.Set {
		v.fail("cost", "is required")
	} else {
		out.cost = v.intField("cost", in.Cost, 0, 0)
	}
	return out, v.err()
}

// RequestRedemption records the acting member's request to redeem a reward of
// the household, waiting for the administrator. The member is proven by
// their PIN (act). The request keeps the reward's description and cost as
// they are now; no points move until it is approved. A member whose points
// are below the cost is refused with a ValidationError on "points" and
// nothing is written. A reward not in the household (or a member not in it)
// is ErrNotFound. When the member already has this reward pending, that
// request is returned and created is false: no second row is written.
func (s *RewardService) RequestRedemption(ctx context.Context, userID, householdID, rewardID uuid.UUID, act MemberAction) (redemption db.RewardRedemption, created bool, err error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.RewardRedemption{}, false, err
	}
	member, err := VerifyMemberPin(ctx, s.q, householdID, act.MemberID, act.PIN)
	if err != nil {
		return db.RewardRedemption{}, false, err
	}
	reward, err := notFoundIfNoRows(s.q.GetHouseholdReward(ctx, db.GetHouseholdRewardParams{HouseholdID: householdID, ID: rewardID}))
	if err != nil {
		return db.RewardRedemption{}, false, err
	}
	if member.Points < reward.Cost {
		return db.RewardRedemption{}, false, &ValidationError{Fields: map[string]string{
			"points": fmt.Sprintf("%s has %d points — %s costs %d.", member.Name, member.Points, reward.Description, reward.Cost),
		}}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return db.RewardRedemption{}, false, fmt.Errorf("generate redemption id: %w", err)
	}
	row, err := s.q.CreateRewardRedemption(ctx, db.CreateRewardRedemptionParams{
		ID:          id,
		HouseholdID: householdID,
		MemberID:    member.ID,
		RewardID:    &reward.ID,
		Description: reward.Description,
		Cost:        reward.Cost,
	})
	switch {
	case err == nil:
		return row, true, nil
	case isForeignKeyViolation(err):
		// The member or the reward was removed since it was read.
		return db.RewardRedemption{}, false, ErrNotFound
	case !errors.Is(err, pgx.ErrNoRows):
		return db.RewardRedemption{}, false, fmt.Errorf("create redemption: %w", err)
	}
	// ON CONFLICT DO NOTHING: the member already has this reward pending.
	existing, err := notFoundIfNoRows(s.q.GetMemberPendingRedemptionForReward(ctx, db.GetMemberPendingRedemptionForRewardParams{
		HouseholdID: householdID, MemberID: member.ID, RewardID: &reward.ID,
	}))
	if err != nil {
		// Decided between the insert and this read: the caller may ask again.
		return db.RewardRedemption{}, false, err
	}
	return existing, false, nil
}

// ListMemberPending returns the member's requests waiting for approval,
// oldest first; none is an empty, non-nil slice. Access is that of the
// member's points history (authorizeMemberRead): the member's PIN, or the
// household admin (with the admin PIN once one is set).
func (s *RewardService) ListMemberPending(ctx context.Context, userID, householdID, memberID uuid.UUID, memberPin, adminPin string) ([]db.RewardRedemption, error) {
	if err := authorizeMemberRead(ctx, s.q, userID, householdID, memberID, memberPin, adminPin); err != nil {
		return nil, err
	}
	rows, err := s.q.ListMemberPendingRedemptions(ctx, db.ListMemberPendingRedemptionsParams{HouseholdID: householdID, MemberID: memberID})
	if err != nil {
		return nil, fmt.Errorf("list member pending redemptions: %w", err)
	}
	return rows, nil
}

// ListPending returns the household's requests waiting for approval, oldest
// first, each with the member's name; none is an empty, non-nil slice. Admin
// only, behind the admin PIN once one is set.
func (s *RewardService) ListPending(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]db.ListPendingRewardRedemptionsRow, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return nil, err
	}
	rows, err := s.q.ListPendingRewardRedemptions(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list pending redemptions: %w", err)
	}
	return rows, nil
}

// Approve approves a pending request and takes its cost snapshot from the
// member's points, once: one transaction locks the member row, then the
// request, re-reads the member's points under the lock, marks the request
// approved and debits through creditPoints (kind 'reward', reason
// "Redeemed: <description>", minus the cost). A request whose cost is 0
// moves no points and writes no history row. It returns the approved
// request and the member at the new total.
//
// A member whose points are now below the cost is refused with a
// ValidationError on "points" naming the member, their points and the cost:
// nothing changes and the request stays pending, so it can still be
// rejected. A request that is not pending (approved, rejected or cancelled
// meanwhile, including by a concurrent approval) or not in this household
// is ErrNotFound. Admin only, behind the admin PIN once one is set.
//
// Lock order is member row, then request, the order Reject and
// FamilyService.Remove use, so they serialize instead of deadlocking.
func (s *RewardService) Approve(ctx context.Context, userID, householdID, redemptionID uuid.UUID, adminPin string) (db.RewardRedemption, db.FamilyMember, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.RewardRedemption{}, db.FamilyMember{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.RewardRedemption{}, db.FamilyMember{}, fmt.Errorf("begin approve redemption: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	red, err := lockPendingRedemption(ctx, qtx, householdID, redemptionID)
	if err != nil {
		return db.RewardRedemption{}, db.FamilyMember{}, err
	}
	member, err := qtx.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: householdID, ID: red.MemberID})
	if err != nil {
		return db.RewardRedemption{}, db.FamilyMember{}, fmt.Errorf("get family member: %w", err)
	}
	if member.Points < red.Cost {
		return db.RewardRedemption{}, db.FamilyMember{}, &ValidationError{Fields: map[string]string{
			"points": fmt.Sprintf("Can't approve %s for %s: %s has %d points and it costs %d. Nothing was taken.",
				red.Description, member.Name, member.Name, member.Points, red.Cost),
		}}
	}
	approved, err := notFoundIfNoRows(qtx.ApproveRewardRedemption(ctx, db.ApproveRewardRedemptionParams{HouseholdID: householdID, ID: redemptionID}))
	if err != nil {
		return db.RewardRedemption{}, db.FamilyMember{}, err
	}
	if approved.Cost > 0 {
		member, err = creditPoints(ctx, qtx, householdID, member.ID, awardKindReward, "Redeemed: "+approved.Description, -approved.Cost, approved.DecidedAt)
		if err != nil {
			return db.RewardRedemption{}, db.FamilyMember{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.RewardRedemption{}, db.FamilyMember{}, fmt.Errorf("commit approve redemption: %w", err)
	}
	return approved, member, nil
}

// Reject rejects a pending request: no points move and no history row is
// written. Same lock order as Approve (member row, then request). A request
// that is not pending or not in this household is ErrNotFound. Admin only,
// behind the admin PIN once one is set.
func (s *RewardService) Reject(ctx context.Context, userID, householdID, redemptionID uuid.UUID, adminPin string) (db.RewardRedemption, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.RewardRedemption{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.RewardRedemption{}, fmt.Errorf("begin reject redemption: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	if _, err := lockPendingRedemption(ctx, qtx, householdID, redemptionID); err != nil {
		return db.RewardRedemption{}, err
	}
	rejected, err := notFoundIfNoRows(qtx.RejectRewardRedemption(ctx, db.RejectRewardRedemptionParams{HouseholdID: householdID, ID: redemptionID}))
	if err != nil {
		return db.RewardRedemption{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.RewardRedemption{}, fmt.Errorf("commit reject redemption: %w", err)
	}
	return rejected, nil
}

// lockPendingRedemption locks the member row of a pending request, then the
// request itself, on qtx (bound to the caller's transaction). A request that
// is not pending, not in the household, or whose member was removed
// meanwhile (its requests cancelled) is ErrNotFound.
func lockPendingRedemption(ctx context.Context, qtx *db.Queries, householdID, redemptionID uuid.UUID) (db.RewardRedemption, error) {
	memberID, err := notFoundIfNoRows(qtx.GetPendingRedemptionMember(ctx, db.GetPendingRedemptionMemberParams{HouseholdID: householdID, ID: redemptionID}))
	if err != nil {
		return db.RewardRedemption{}, err
	}
	if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: householdID, ID: memberID})); err != nil {
		return db.RewardRedemption{}, err
	}
	// Re-read under the member lock: a decision or removal that committed
	// first leaves no pending row.
	return notFoundIfNoRows(qtx.LockPendingRedemption(ctx, db.LockPendingRedemptionParams{HouseholdID: householdID, ID: redemptionID, MemberID: memberID}))
}

// cancelMemberRedemptions cancels every pending request of a member who is
// leaving the household, on qtx (bound to FamilyService.Remove's
// transaction, after its lock on the member row). No points move.
func cancelMemberRedemptions(ctx context.Context, qtx *db.Queries, householdID, memberID uuid.UUID) error {
	if _, err := qtx.CancelMemberPendingRedemptions(ctx, db.CancelMemberPendingRedemptionsParams{HouseholdID: householdID, MemberID: memberID}); err != nil {
		return fmt.Errorf("cancel member redemptions: %w", err)
	}
	return nil
}
