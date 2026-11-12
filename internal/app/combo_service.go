package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/db"
)

// ComboInput is the editable part of a household combo.
type ComboInput struct {
	Name        string
	BonusPoints NumberField
	QuestIDs    []uuid.UUID
}

// Combo represents a household combo with its associated chore IDs.
type Combo struct {
	db.Combo
	QuestIDs []uuid.UUID
}

// ComboService manages a household's combos. Any household member may read
// the household's combos; only the administrator (behind the admin PIN once one
// is set) may create, update, or delete them.
type ComboService struct {
	q    *db.Queries
	pool pgxBeginner
}

// NewComboService constructs a ComboService.
func NewComboService(q *db.Queries, pool pgxBeginner) *ComboService {
	return &ComboService{q: q, pool: pool}
}

// List returns every combo of the household, oldest first. None is an
// empty, non-nil slice.
func (s *ComboService) List(ctx context.Context, userID, householdID uuid.UUID) ([]Combo, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, err
	}
	combos, err := s.q.ListHouseholdCombos(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list combos: %w", err)
	}
	if len(combos) == 0 {
		return []Combo{}, nil
	}
	comboQuests, err := s.q.ListHouseholdComboQuests(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list combo quests: %w", err)
	}
	byCombo := make(map[uuid.UUID][]uuid.UUID)
	for _, cq := range comboQuests {
		byCombo[cq.ComboID] = append(byCombo[cq.ComboID], cq.QuestID)
	}
	res := make([]Combo, len(combos))
	for i, c := range combos {
		qids := byCombo[c.ID]
		if qids == nil {
			qids = []uuid.UUID{}
		}
		res[i] = Combo{Combo: c, QuestIDs: qids}
	}
	return res, nil
}

// Get returns one combo of the household with its chore IDs.
func (s *ComboService) Get(ctx context.Context, userID, householdID, comboID uuid.UUID) (Combo, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return Combo{}, err
	}
	c, err := notFoundIfNoRows(s.q.GetHouseholdCombo(ctx, db.GetHouseholdComboParams{
		HouseholdID: householdID,
		ID:          comboID,
	}))
	if err != nil {
		return Combo{}, err
	}
	qids, err := s.q.ListComboQuests(ctx, db.ListComboQuestsParams{
		HouseholdID: householdID,
		ComboID:     comboID,
	})
	if err != nil {
		return Combo{}, fmt.Errorf("get combo quests: %w", err)
	}
	if qids == nil {
		qids = []uuid.UUID{}
	}
	return Combo{Combo: c, QuestIDs: qids}, nil
}

// Create adds a combo to the household and links its quests.
func (s *ComboService) Create(ctx context.Context, userID, householdID uuid.UUID, in ComboInput, adminPin string) (Combo, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Combo{}, err
	}
	v, err := validateCombo(in)
	if err != nil {
		return Combo{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Combo{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	count, err := qtx.CountHouseholdQuestsByIDs(ctx, db.CountHouseholdQuestsByIDsParams{
		HouseholdID: householdID,
		QuestIds:    v.questIDs,
	})
	if err != nil {
		return Combo{}, fmt.Errorf("count quests: %w", err)
	}
	if count != int64(len(v.questIDs)) {
		return Combo{}, ErrNotFound
	}

	cid, err := uuid.NewV7()
	if err != nil {
		return Combo{}, fmt.Errorf("generate combo id: %w", err)
	}
	combo, err := qtx.CreateHouseholdCombo(ctx, db.CreateHouseholdComboParams{
		ID:          cid,
		HouseholdID: householdID,
		Name:        v.name,
		BonusPoints: v.bonusPoints,
	})
	if err != nil {
		return Combo{}, fmt.Errorf("create combo: %w", err)
	}

	for _, qid := range v.questIDs {
		if err := qtx.InsertComboQuest(ctx, db.InsertComboQuestParams{
			HouseholdID: householdID,
			ComboID:     cid,
			QuestID:     qid,
		}); err != nil {
			return Combo{}, fmt.Errorf("insert combo quest: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Combo{}, fmt.Errorf("commit: %w", err)
	}

	return Combo{Combo: combo, QuestIDs: v.questIDs}, nil
}

// Update replaces a combo's name, bonus_points, and chore list in one transaction.
// Updates never touch combo_awards.
func (s *ComboService) Update(ctx context.Context, userID, householdID, comboID uuid.UUID, in ComboInput, adminPin string) (Combo, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Combo{}, err
	}
	v, err := validateCombo(in)
	if err != nil {
		return Combo{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Combo{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	count, err := qtx.CountHouseholdQuestsByIDs(ctx, db.CountHouseholdQuestsByIDsParams{
		HouseholdID: householdID,
		QuestIds:    v.questIDs,
	})
	if err != nil {
		return Combo{}, fmt.Errorf("count quests: %w", err)
	}
	if count != int64(len(v.questIDs)) {
		return Combo{}, ErrNotFound
	}

	combo, err := notFoundIfNoRows(qtx.UpdateHouseholdCombo(ctx, db.UpdateHouseholdComboParams{
		HouseholdID: householdID,
		ID:          comboID,
		Name:        v.name,
		BonusPoints: v.bonusPoints,
	}))
	if err != nil {
		return Combo{}, err
	}

	if err := qtx.DeleteComboQuests(ctx, db.DeleteComboQuestsParams{
		HouseholdID: householdID,
		ComboID:     comboID,
	}); err != nil {
		return Combo{}, fmt.Errorf("delete combo quests: %w", err)
	}

	for _, qid := range v.questIDs {
		if err := qtx.InsertComboQuest(ctx, db.InsertComboQuestParams{
			HouseholdID: householdID,
			ComboID:     comboID,
			QuestID:     qid,
		}); err != nil {
			return Combo{}, fmt.Errorf("insert combo quest: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Combo{}, fmt.Errorf("commit: %w", err)
	}

	return Combo{Combo: combo, QuestIDs: v.questIDs}, nil
}

// Delete removes a combo from the household. Deletes never touch combo_awards.
func (s *ComboService) Delete(ctx context.Context, userID, householdID, comboID uuid.UUID, adminPin string) error {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return err
	}
	n, err := s.q.DeleteHouseholdCombo(ctx, db.DeleteHouseholdComboParams{
		HouseholdID: householdID,
		ID:          comboID,
	})
	if err != nil {
		return fmt.Errorf("delete combo: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type validCombo struct {
	name        string
	bonusPoints int32
	questIDs    []uuid.UUID
}

// validateCombo validates all fields of a combo at once and reports every problem:
// name 1-200 characters after trimming, bonus_points a required whole number >= 0
// fitting int32, and quest_ids at least two distinct IDs.
func validateCombo(in ComboInput) (validCombo, error) {
	var v validator
	out := validCombo{name: v.text("name", in.Name, 1, 200, false)}
	if !in.BonusPoints.Set {
		v.fail("bonus_points", "is required")
	} else {
		out.bonusPoints = v.intField("bonus_points", in.BonusPoints, 0, 0)
	}

	distinct := make([]uuid.UUID, 0, len(in.QuestIDs))
	seen := make(map[uuid.UUID]bool, len(in.QuestIDs))
	for _, id := range in.QuestIDs {
		if id != uuid.Nil && !seen[id] {
			seen[id] = true
			distinct = append(distinct, id)
		}
	}
	if len(distinct) < 2 {
		v.fail("quest_ids", "pick at least two chores")
	} else {
		out.questIDs = distinct
	}

	return out, v.err()
}
