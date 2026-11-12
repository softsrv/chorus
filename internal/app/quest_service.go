package app

import (
	"context"
	"errors"
	"fmt"
	"math"
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

// NumberField is a numeric request field as it arrived: absent (Set false),
// present but not an integer (Invalid true), or an integer Value. Carrying the
// bad case to the service lets one validation error list every bad field.
type NumberField struct {
	Value   int64
	Set     bool
	Invalid bool
}

// QuestInput is the editable part of a household quest. An absent Points
// means 0 and an absent RepeatDays means 1 (daily).
type QuestInput struct {
	Name       string
	Points     NumberField
	RepeatDays NumberField
	Notes      string
}

// QuestPage narrows List to one page.
type QuestPage struct {
	Limit  int
	Offset int
}

// QuestService manages a household's quests and reads the global quest
// library. Any household member may read the household's quests; only the
// administrator (behind the admin PIN once one is set) may change them.
type QuestService struct {
	q    *db.Queries
	pool pgxBeginner
}

// NewQuestService constructs a QuestService. pool runs the approval
// transaction that completes a quest and credits its points together.
func NewQuestService(q *db.Queries, pool pgxBeginner) *QuestService {
	return &QuestService{q: q, pool: pool}
}

// releaseMemberQuests returns every quest the member holds, one awaiting
// approval included, to the household's pool. It is the one place that clears
// a quest's holder when a member goes; qtx is the caller's transaction, so the
// release commits or rolls back with the removal. No points move.
func releaseMemberQuests(ctx context.Context, qtx *db.Queries, householdID, memberID uuid.UUID) error {
	if err := qtx.ReleaseMemberQuests(ctx, db.ReleaseMemberQuestsParams{HouseholdID: householdID, MemberID: &memberID}); err != nil {
		return fmt.Errorf("release member quests: %w", err)
	}
	return nil
}

// List returns up to p.Limit quests of the household, and whether more remain
// after them.
func (s *QuestService) List(ctx context.Context, userID, householdID uuid.UUID, p QuestPage) ([]db.HouseholdQuest, bool, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, false, err
	}
	var v validator
	if p.Limit < 1 || p.Limit > MaxPageSize {
		v.fail("limit", fmt.Sprintf("must be between 1 and %d", MaxPageSize))
	}
	if p.Offset < 0 {
		v.fail("offset", "must not be negative")
	} else if p.Offset > math.MaxInt32-MaxPageSize {
		v.fail("offset", "is too large")
	}
	if err := v.err(); err != nil {
		return nil, false, err
	}
	// Fetch one extra row to learn whether another page exists.
	quests, err := s.q.ListHouseholdQuests(ctx, db.ListHouseholdQuestsParams{
		HouseholdID: householdID,
		RowLimit:    int32(p.Limit + 1),
		RowOffset:   int32(p.Offset),
	})
	if err != nil {
		return nil, false, fmt.Errorf("list quests: %w", err)
	}
	if len(quests) > p.Limit {
		return quests[:p.Limit], true, nil
	}
	return quests, false, nil
}

// Get returns one quest of the household.
func (s *QuestService) Get(ctx context.Context, userID, householdID, questID uuid.UUID) (db.HouseholdQuest, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return db.HouseholdQuest{}, err
	}
	return notFoundIfNoRows(s.q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID}))
}

// Create adds a quest to the household.
func (s *QuestService) Create(ctx context.Context, userID, householdID uuid.UUID, in QuestInput, adminPin string) (db.HouseholdQuest, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdQuest{}, err
	}
	v, err := validateQuest(in)
	if err != nil {
		return db.HouseholdQuest{}, err
	}
	return s.insert(ctx, householdID, nil, v)
}

// Update replaces a quest's editable fields.
func (s *QuestService) Update(ctx context.Context, userID, householdID, questID uuid.UUID, in QuestInput, adminPin string) (db.HouseholdQuest, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdQuest{}, err
	}
	v, err := validateQuest(in)
	if err != nil {
		return db.HouseholdQuest{}, err
	}
	return notFoundIfNoRows(s.q.UpdateHouseholdQuest(ctx, db.UpdateHouseholdQuestParams{
		HouseholdID: householdID,
		ID:          questID,
		Name:        v.name,
		Points:      v.points,
		RepeatDays:  v.repeatDays,
		Notes:       v.notes,
	}))
}

// Delete removes a quest from the household.
func (s *QuestService) Delete(ctx context.Context, userID, householdID, questID uuid.UUID, adminPin string) error {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return err
	}
	n, err := s.q.DeleteHouseholdQuest(ctx, db.DeleteHouseholdQuestParams{HouseholdID: householdID, ID: questID})
	if err != nil {
		return fmt.Errorf("delete quest: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CopyFromLibrary adds a copy of a library quest to the household. The copy
// carries the library's name, points, days and notes and remembers its
// library_id; editing it later never changes the library.
func (s *QuestService) CopyFromLibrary(ctx context.Context, userID, householdID, libraryID uuid.UUID, adminPin string) (db.HouseholdQuest, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdQuest{}, err
	}
	item, err := notFoundIfNoRows(s.q.GetQuestLibraryItem(ctx, libraryID))
	if err != nil {
		return db.HouseholdQuest{}, err
	}
	return s.insert(ctx, householdID, &item.ID, validQuest{
		name: item.Name, points: item.Points, repeatDays: item.RepeatDays, notes: item.Notes,
	})
}

// MemberAction names the family member taking a quest action and the PIN
// that proves it. PIN is empty when the member has none.
type MemberAction struct {
	MemberID uuid.UUID
	PIN      string
}

// QuestAvailable reports whether a quest is in the pool: nobody holds it and
// its repeat days since the last approval have passed.
func QuestAvailable(q db.HouseholdQuest, now time.Time) bool {
	return q.ClaimedBy == nil && (!q.AvailableAt.Valid || !q.AvailableAt.Time.After(now))
}

// Accept assigns an available quest to the acting member. The member must
// belong to the household and prove themselves with their PIN. A quest that
// another member holds, or that is waiting out its repeat days, gives
// ErrQuestTaken; of two concurrent accepts exactly one succeeds.
func (s *QuestService) Accept(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (db.HouseholdQuest, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return db.HouseholdQuest{}, err
	}
	quest, err := s.q.ClaimQuest(ctx, db.ClaimQuestParams{HouseholdID: householdID, ID: questID, MemberID: &act.MemberID})
	if err == nil {
		return quest, nil
	}
	if isForeignKeyViolation(err) {
		return db.HouseholdQuest{}, ErrNotFound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.HouseholdQuest{}, fmt.Errorf("claim quest: %w", err)
	}
	// No row: either the quest is not in this household, or it is not available.
	if _, err := notFoundIfNoRows(s.q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID})); err != nil {
		return db.HouseholdQuest{}, err
	}
	return db.HouseholdQuest{}, ErrQuestTaken
}

// Abandon puts the acting member's quest back in the pool for everyone. Only
// the quest's holder can abandon it, and not while it waits for approval;
// any other case is ErrNotFound and changes nothing.
func (s *QuestService) Abandon(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (db.HouseholdQuest, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return db.HouseholdQuest{}, err
	}
	return notFoundIfNoRows(s.q.AbandonQuest(ctx, db.AbandonQuestParams{HouseholdID: householdID, ID: questID, MemberID: &act.MemberID}))
}

// MarkDone records that the acting member finished their quest. It then
// waits for the administrator's approval; no points move until then. A quest
// the member does not hold, or one already waiting, is ErrNotFound.
func (s *QuestService) MarkDone(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (db.HouseholdQuest, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return db.HouseholdQuest{}, err
	}
	return notFoundIfNoRows(s.q.MarkQuestDone(ctx, db.MarkQuestDoneParams{HouseholdID: householdID, ID: questID, MemberID: &act.MemberID}))
}

// ListPending returns the quests waiting for approval, oldest first, each
// with the name of the member who holds it. Admin only, behind the admin PIN
// once one is set. None waiting is an empty, non-nil slice.
func (s *QuestService) ListPending(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]db.ListPendingQuestsRow, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return nil, err
	}
	rows, err := s.q.ListPendingQuests(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list pending quests: %w", err)
	}
	return rows, nil
}

// ComboBonus is one combo bonus paid by an approval.
type ComboBonus struct {
	ComboID     uuid.UUID
	Name        string
	BonusPoints int32
}

// Approve completes a pending quest and credits its points to the member who
// held it, in one transaction: either all of it happens or none does. The
// quest's repeat countdown starts now. Admin only, behind the admin PIN once
// one is set. A quest that is not pending (never marked done, sent back, or
// already approved) is ErrNotFound, so a repeated or concurrent approval
// credits the points once.
//
// The approval also records the completion (member, chore, mark-done time)
// and pays every combo it finishes: a combo of two or more chores each of
// which the member marked done on the same household-local day, using
// completions not yet counted toward that combo. The returned member's points
// include the bonuses; the returned slice is empty, never nil, when none paid.
func (s *QuestService) Approve(ctx context.Context, userID, householdID, questID uuid.UUID, adminPin string) (db.HouseholdQuest, db.FamilyMember, []ComboBonus, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdQuest{}, db.FamilyMember{}, nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.HouseholdQuest{}, db.FamilyMember{}, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	row, err := notFoundIfNoRows(qtx.ApproveQuest(ctx, db.ApproveQuestParams{HouseholdID: householdID, ID: questID}))
	if err != nil {
		return db.HouseholdQuest{}, db.FamilyMember{}, nil, err
	}
	if row.CreditedMemberID == nil || !row.CreditedDoneAt.Valid {
		// The schema forbids a pending quest without a holder or done_at.
		return db.HouseholdQuest{}, db.FamilyMember{}, nil, fmt.Errorf("approve quest %s: pending quest has no holder", questID)
	}
	memberID := *row.CreditedMemberID
	member, err := qtx.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: householdID, ID: memberID, Delta: row.Points})
	if err != nil {
		return db.HouseholdQuest{}, db.FamilyMember{}, nil, fmt.Errorf("credit points: %w", err)
	}
	member, bonuses, err := payCombos(ctx, qtx, householdID, questID, member, row.CreditedDoneAt)
	if err != nil {
		return db.HouseholdQuest{}, db.FamilyMember{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.HouseholdQuest{}, db.FamilyMember{}, nil, fmt.Errorf("commit: %w", err)
	}
	return db.HouseholdQuest{
		ID:          row.ID,
		HouseholdID: row.HouseholdID,
		LibraryID:   row.LibraryID,
		Name:        row.Name,
		Points:      row.Points,
		RepeatDays:  row.RepeatDays,
		Notes:       row.Notes,
		ClaimedBy:   row.ClaimedBy,
		ClaimedAt:   row.ClaimedAt,
		DoneAt:      row.DoneAt,
		CompletedAt: row.CompletedAt,
		AvailableAt: row.AvailableAt,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}, member, bonuses, nil
}

// payCombos runs inside Approve's transaction (qtx). It records the approved
// completion, locks the combos containing the quest (so concurrent approvals
// of one combo's chores serialize and the later one sees the earlier one's
// completion), then pays each combo the member has finished: an award row
// with name and points snapshots, one claimed completion per chore, and the
// bonus credit. Any error is returned for the caller to roll back.
func payCombos(ctx context.Context, qtx *db.Queries, householdID, questID uuid.UUID, member db.FamilyMember, doneAt pgtype.Timestamptz) (db.FamilyMember, []ComboBonus, error) {
	completionID, err := uuid.NewV7()
	if err != nil {
		return db.FamilyMember{}, nil, fmt.Errorf("new completion id: %w", err)
	}
	if _, err := qtx.InsertQuestCompletion(ctx, db.InsertQuestCompletionParams{
		ID: completionID, HouseholdID: householdID, QuestID: questID, MemberID: member.ID, DoneAt: doneAt,
	}); err != nil {
		return db.FamilyMember{}, nil, fmt.Errorf("record completion: %w", err)
	}
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
		member, err = qtx.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: householdID, ID: member.ID, Delta: c.BonusPoints})
		if err != nil {
			return db.FamilyMember{}, nil, fmt.Errorf("credit combo bonus: %w", err)
		}
		bonuses = append(bonuses, ComboBonus{ComboID: c.ID, Name: c.Name, BonusPoints: c.BonusPoints})
	}
	return member, bonuses, nil
}

// Reject sends a pending quest back to the member who holds it: it stays
// theirs, no points are added, and they can mark it done again. Admin only,
// behind the admin PIN once one is set. A quest that is not pending is
// ErrNotFound.
func (s *QuestService) Reject(ctx context.Context, userID, householdID, questID uuid.UUID, adminPin string) (db.HouseholdQuest, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return db.HouseholdQuest{}, err
	}
	return notFoundIfNoRows(s.q.RejectQuest(ctx, db.RejectQuestParams{HouseholdID: householdID, ID: questID}))
}

// verifyActingMember checks the user belongs to the household and that the
// acting member is one of its family members, proven by their PIN.
func (s *QuestService) verifyActingMember(ctx context.Context, userID, householdID uuid.UUID, act MemberAction) error {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return err
	}
	_, err := VerifyMemberPin(ctx, s.q, householdID, act.MemberID, act.PIN)
	return err
}

// ListLibrary returns the global quest library. It holds no household data,
// so any signed-in user may read it.
func (s *QuestService) ListLibrary(ctx context.Context) ([]db.QuestLibrary, error) {
	items, err := s.q.ListQuestLibrary(ctx)
	if err != nil {
		return nil, fmt.Errorf("list quest library: %w", err)
	}
	return items, nil
}

func (s *QuestService) insert(ctx context.Context, householdID uuid.UUID, libraryID *uuid.UUID, v validQuest) (db.HouseholdQuest, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return db.HouseholdQuest{}, fmt.Errorf("generate quest id: %w", err)
	}
	quest, err := s.q.CreateHouseholdQuest(ctx, db.CreateHouseholdQuestParams{
		ID:          id,
		HouseholdID: householdID,
		LibraryID:   libraryID,
		Name:        v.name,
		Points:      v.points,
		RepeatDays:  v.repeatDays,
		Notes:       v.notes,
	})
	if err != nil {
		return db.HouseholdQuest{}, fmt.Errorf("create quest: %w", err)
	}
	return quest, nil
}

// validQuest is a QuestInput that passed validateQuest.
type validQuest struct {
	name       string
	points     int32
	repeatDays int32
	notes      string
}

// validateQuest checks every field and reports each bad one: name 1-200
// characters, points a whole number >= 0, repeat_days a whole number >= 1,
// notes at most 2000 characters.
func validateQuest(in QuestInput) (validQuest, error) {
	var v validator
	out := validQuest{
		name:       v.text("name", in.Name, 1, 200, false),
		points:     v.intField("points", in.Points, 0, 0),
		repeatDays: v.intField("repeat_days", in.RepeatDays, 1, 1),
		notes:      v.text("notes", in.Notes, 0, 2000, true),
	}
	return out, v.err()
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

// isForeignKeyViolation reports whether err is a Postgres foreign-key violation (SQLSTATE 23503).
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
