package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/db"
)

// RewardInput is the editable part of a household reward. Cost is required.
type RewardInput struct {
	Description string
	Cost        NumberField
}

// RewardService manages a household's rewards and reads the global reward
// library. Any household member may read the household's rewards; only the
// administrator (behind the admin PIN once one is set) may change them.
// Rewards are a priced list only: nothing here redeems a reward or touches
// anyone's points.
type RewardService struct {
	q *db.Queries
}

// NewRewardService constructs a RewardService.
func NewRewardService(q *db.Queries) *RewardService {
	return &RewardService{q: q}
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
