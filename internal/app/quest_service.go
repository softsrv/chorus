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

// QuestView is a household quest with awaiting_approval: whether one of its
// completions is waiting for the administrator.
type QuestView struct {
	db.HouseholdQuest
	AwaitingApproval bool
}

// Completion is one time a member marked a chore done. Status is pending,
// approved, rejected or cancelled; ApprovedAt is set only when approved.
// MemberID is nil once the member has been removed.
type Completion struct {
	ID         uuid.UUID
	QuestID    uuid.UUID
	MemberID   *uuid.UUID
	DoneAt     time.Time
	ApprovedAt *time.Time
	Status     string
}

// PendingCompletion is a completion waiting for approval, with the chore's
// name and current points and the name of the member who did it.
type PendingCompletion struct {
	ID         uuid.UUID
	QuestID    uuid.UUID
	QuestName  string
	Points     int32
	MemberID   uuid.UUID
	MemberName string
	DoneAt     time.Time
}

// releaseMemberQuests returns every quest the member holds to the household's
// pool and cancels the member's completions still waiting for approval. It is
// the one place that clears a quest's holder when a member goes; qtx is the
// caller's transaction, so the release commits or rolls back with the
// removal. No points move and no quest's cooldown changes.
func releaseMemberQuests(ctx context.Context, qtx *db.Queries, householdID, memberID uuid.UUID) error {
	if err := qtx.ReleaseMemberQuests(ctx, db.ReleaseMemberQuestsParams{HouseholdID: householdID, MemberID: &memberID}); err != nil {
		return fmt.Errorf("release member quests: %w", err)
	}
	if err := qtx.CancelMemberPendingCompletions(ctx, db.CancelMemberPendingCompletionsParams{HouseholdID: householdID, MemberID: &memberID}); err != nil {
		return fmt.Errorf("cancel member completions: %w", err)
	}
	return nil
}

// view adds awaiting_approval to a quest just read or written.
func (s *QuestService) view(ctx context.Context, quest db.HouseholdQuest) (QuestView, error) {
	pending, err := s.q.QuestHasPendingCompletion(ctx, db.QuestHasPendingCompletionParams{HouseholdID: quest.HouseholdID, QuestID: quest.ID})
	if err != nil {
		return QuestView{}, fmt.Errorf("read pending completion: %w", err)
	}
	return QuestView{HouseholdQuest: quest, AwaitingApproval: pending}, nil
}

// viewOf takes a single-quest query's result and adds awaiting_approval to
// its row; no row is ErrNotFound.
func (s *QuestService) viewOf(ctx context.Context, quest db.HouseholdQuest, err error) (QuestView, error) {
	quest, err = notFoundIfNoRows(quest, err)
	if err != nil {
		return QuestView{}, err
	}
	return s.view(ctx, quest)
}

// List returns up to p.Limit quests of the household, and whether more remain
// after them.
func (s *QuestService) List(ctx context.Context, userID, householdID uuid.UUID, p QuestPage) ([]QuestView, bool, error) {
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
	rows, err := s.q.ListHouseholdQuests(ctx, db.ListHouseholdQuestsParams{
		HouseholdID: householdID,
		RowLimit:    int32(p.Limit + 1),
		RowOffset:   int32(p.Offset),
	})
	if err != nil {
		return nil, false, fmt.Errorf("list quests: %w", err)
	}
	quests := make([]QuestView, len(rows))
	for i, r := range rows {
		quests[i] = QuestView{HouseholdQuest: r.HouseholdQuest, AwaitingApproval: r.AwaitingApproval}
	}
	if len(quests) > p.Limit {
		return quests[:p.Limit], true, nil
	}
	return quests, false, nil
}

// Get returns one quest of the household.
func (s *QuestService) Get(ctx context.Context, userID, householdID, questID uuid.UUID) (QuestView, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return QuestView{}, err
	}
	row, err := notFoundIfNoRows(s.q.GetHouseholdQuestView(ctx, db.GetHouseholdQuestViewParams{HouseholdID: householdID, ID: questID}))
	if err != nil {
		return QuestView{}, err
	}
	return QuestView{HouseholdQuest: row.HouseholdQuest, AwaitingApproval: row.AwaitingApproval}, nil
}

// Create adds a quest to the household.
func (s *QuestService) Create(ctx context.Context, userID, householdID uuid.UUID, in QuestInput, adminPin string) (QuestView, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return QuestView{}, err
	}
	v, err := validateQuest(in)
	if err != nil {
		return QuestView{}, err
	}
	return s.insert(ctx, householdID, nil, v)
}

// Update replaces a quest's editable fields.
func (s *QuestService) Update(ctx context.Context, userID, householdID, questID uuid.UUID, in QuestInput, adminPin string) (QuestView, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return QuestView{}, err
	}
	v, err := validateQuest(in)
	if err != nil {
		return QuestView{}, err
	}
	quest, err := s.q.UpdateHouseholdQuest(ctx, db.UpdateHouseholdQuestParams{
		HouseholdID: householdID,
		ID:          questID,
		Name:        v.name,
		Points:      v.points,
		RepeatDays:  v.repeatDays,
		Notes:       v.notes,
	})
	return s.viewOf(ctx, quest, err)
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
func (s *QuestService) CopyFromLibrary(ctx context.Context, userID, householdID, libraryID uuid.UUID, adminPin string) (QuestView, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return QuestView{}, err
	}
	item, err := notFoundIfNoRows(s.q.GetQuestLibraryItem(ctx, libraryID))
	if err != nil {
		return QuestView{}, err
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
// its available_at has arrived. available_at is the local midnight in the
// household's timezone repeat_days days after the day it was last marked
// done, whether or not that completion has been approved yet; a quest never
// marked done has none.
func QuestAvailable(q db.HouseholdQuest, now time.Time) bool {
	return q.ClaimedBy == nil && (!q.AvailableAt.Valid || !q.AvailableAt.Time.After(now))
}

// Accept assigns an available quest to the acting member. The member must
// belong to the household and prove themselves with their PIN. A quest that
// another member holds, or that is waiting out its repeat days, gives
// ErrQuestTaken; of two concurrent accepts exactly one succeeds.
func (s *QuestService) Accept(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (QuestView, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return QuestView{}, err
	}
	quest, err := s.q.ClaimQuest(ctx, db.ClaimQuestParams{HouseholdID: householdID, ID: questID, MemberID: &act.MemberID})
	if err == nil {
		return s.view(ctx, quest)
	}
	if isForeignKeyViolation(err) {
		return QuestView{}, ErrNotFound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return QuestView{}, fmt.Errorf("claim quest: %w", err)
	}
	// No row: either the quest is not in this household, or it is not available.
	if _, err := notFoundIfNoRows(s.q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID})); err != nil {
		return QuestView{}, err
	}
	return QuestView{}, ErrQuestTaken
}

// Abandon puts the acting member's quest back in the pool for everyone. Only
// the quest's holder can abandon it; any other case is ErrNotFound and
// changes nothing.
func (s *QuestService) Abandon(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (QuestView, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return QuestView{}, err
	}
	quest, err := s.q.AbandonQuest(ctx, db.AbandonQuestParams{HouseholdID: householdID, ID: questID, MemberID: &act.MemberID})
	return s.viewOf(ctx, quest, err)
}

// MarkDone records that the acting member finished their quest. In one
// statement it inserts a pending completion (member, mark-done time now),
// releases the quest and starts its cooldown from now: the quest comes back
// at local midnight in the household's timezone, repeat_days days after
// today, whether or not the administrator has approved by then. No points
// move until approval. A quest the member does not hold is ErrNotFound.
func (s *QuestService) MarkDone(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (QuestView, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return QuestView{}, err
	}
	completionID, err := uuid.NewV7()
	if err != nil {
		return QuestView{}, fmt.Errorf("new completion id: %w", err)
	}
	row, err := notFoundIfNoRows(s.q.MarkQuestDone(ctx, db.MarkQuestDoneParams{
		HouseholdID: householdID, ID: questID, MemberID: act.MemberID, CompletionID: completionID,
	}))
	if isForeignKeyViolation(err) {
		// The member was removed between the PIN check and the write.
		return QuestView{}, ErrNotFound
	}
	if err != nil {
		return QuestView{}, err
	}
	return QuestView{HouseholdQuest: db.HouseholdQuest(row), AwaitingApproval: true}, nil
}

// ListPending returns the completions waiting for approval, one per
// mark-done, oldest mark-done first, each with the chore's name and points
// and the member's name. Admin only, behind the admin PIN once one is set.
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
		out = append(out, PendingCompletion{
			ID: r.ID, QuestID: r.QuestID, QuestName: r.QuestName, Points: r.Points,
			MemberID: *r.MemberID, MemberName: r.MemberName, DoneAt: r.DoneAt.Time.UTC(),
		})
	}
	return out, nil
}

// ComboBonus is one combo bonus paid by an approval.
type ComboBonus struct {
	ComboID     uuid.UUID
	Name        string
	BonusPoints int32
}

// Approve approves one pending completion and credits the chore's points to
// the member who marked it done, in one transaction: either all of it
// happens or none does. Admin only, behind the admin PIN once one is set. A
// completion that is not pending (approved, rejected, cancelled, or not in
// this household) is ErrNotFound, so a repeated or concurrent approval
// credits the points once. The quest itself is not touched: its cooldown
// started when the member marked it done.
//
// The approval also pays every combo it finishes: a combo of two or more
// chores each of which the member marked done on the same household-local
// day (by each completion's done_at, however late the approvals), using
// approved completions not yet counted toward that combo. The returned
// member's points include the bonuses; the returned slice is empty, never
// nil, when none paid.
func (s *QuestService) Approve(ctx context.Context, userID, householdID, completionID uuid.UUID, adminPin string) (Completion, db.FamilyMember, []ComboBonus, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Completion{}, db.FamilyMember{}, nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Completion{}, db.FamilyMember{}, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	row, err := notFoundIfNoRows(qtx.ApproveCompletion(ctx, db.ApproveCompletionParams{HouseholdID: householdID, ID: completionID}))
	if err != nil {
		return Completion{}, db.FamilyMember{}, nil, err
	}
	if row.MemberID == nil {
		// The query approves only completions that still have a member.
		return Completion{}, db.FamilyMember{}, nil, fmt.Errorf("approve completion %s: no member", completionID)
	}
	memberID := *row.MemberID
	member, err := qtx.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: householdID, ID: memberID, Delta: row.Points})
	if err != nil {
		return Completion{}, db.FamilyMember{}, nil, fmt.Errorf("credit points: %w", err)
	}
	member, bonuses, err := payCombos(ctx, qtx, householdID, row.QuestID, member, row.DoneAt)
	if err != nil {
		return Completion{}, db.FamilyMember{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Completion{}, db.FamilyMember{}, nil, fmt.Errorf("commit: %w", err)
	}
	approvedAt := row.ApprovedAt.Time.UTC()
	return Completion{
		ID:         row.ID,
		QuestID:    row.QuestID,
		MemberID:   &memberID,
		DoneAt:     row.DoneAt.Time.UTC(),
		ApprovedAt: &approvedAt,
		Status:     "approved",
	}, member, bonuses, nil
}

// payCombos runs inside Approve's transaction (qtx), after the completion
// was approved. doneAt is that completion's mark-done time, which decides
// the household day it counts toward. It locks the combos containing the
// quest (so concurrent approvals of one combo's chores serialize and the
// later one sees the earlier one's approval), then pays each combo the member
// has finished: an award row with name and points snapshots, one claimed
// completion per chore, and the bonus credit. Any error is returned for the
// caller to roll back.
func payCombos(ctx context.Context, qtx *db.Queries, householdID, questID uuid.UUID, member db.FamilyMember, doneAt pgtype.Timestamptz) (db.FamilyMember, []ComboBonus, error) {
	if _, err := qtx.LockCombosForQuest(ctx, db.LockCombosForQuestParams{HouseholdID: householdID, QuestID: questID}); err != nil {
		return db.FamilyMember{}, nil, fmt.Errorf("lock combos: %w", err)
	}
	achieved, err := qtx.ListCombosCompletedByMemberOnDay(ctx, db.ListCombosCompletedByMemberOnDayParams{
		HouseholdID: householdID, QuestID: questID, MemberID: &member.ID, DoneAt: doneAt,
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
			ComboAwardID: awardID, MemberID: &member.ID, AwardDay: c.AwardDay, HouseholdID: householdID, ComboID: c.ID,
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

// Reject rejects one pending completion: no points are added. If that
// completion started the quest's running cooldown and the cooldown has not
// ended, the cooldown is cancelled and the chore is available again right
// away. Other completions and the quest's holder are untouched. Admin only,
// behind the admin PIN once one is set. A completion that is not pending is
// ErrNotFound.
func (s *QuestService) Reject(ctx context.Context, userID, householdID, completionID uuid.UUID, adminPin string) (Completion, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Completion{}, err
	}
	row, err := notFoundIfNoRows(s.q.RejectCompletion(ctx, db.RejectCompletionParams{HouseholdID: householdID, ID: completionID}))
	if err != nil {
		return Completion{}, err
	}
	return Completion{
		ID:       row.ID,
		QuestID:  row.QuestID,
		MemberID: row.MemberID,
		DoneAt:   row.DoneAt.Time.UTC(),
		Status:   row.Status,
	}, nil
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

// insert creates a quest; a new quest has no completions, so it is never
// awaiting approval.
func (s *QuestService) insert(ctx context.Context, householdID uuid.UUID, libraryID *uuid.UUID, v validQuest) (QuestView, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return QuestView{}, fmt.Errorf("generate quest id: %w", err)
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
		return QuestView{}, fmt.Errorf("create quest: %w", err)
	}
	return QuestView{HouseholdQuest: quest}, nil
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
