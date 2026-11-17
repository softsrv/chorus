package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
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
// means 0 and an absent RepeatDays means 1 (daily). Shareable says whether
// the holder may share the quest with other members; TeamworkBonus is what
// each member of a shared completion earns on top of their split. An absent
// TeamworkBonus means 0, and a bonus above 0 needs Shareable.
type QuestInput struct {
	Name          string
	Points        NumberField
	RepeatDays    NumberField
	Notes         string
	Shareable     bool
	TeamworkBonus NumberField
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
// completions is waiting for the administrator. Participants lists every
// member on a shared quest, earliest first (the holder included); it is
// empty for a quest that is not shared.
type QuestView struct {
	db.HouseholdQuest
	AwaitingApproval bool
	Participants     []uuid.UUID
}

// Shared reports whether more than one member is on the quest.
func (v QuestView) Shared() bool { return len(v.Participants) > 1 }

// Completion is one time a member marked a chore done. Status is pending,
// approved, rejected or cancelled; ApprovedAt is set only when approved.
// MemberID is nil once the member has been removed. MemberIDs, set on an
// approval, lists every member the completion credited, earliest first.
type Completion struct {
	ID         uuid.UUID
	QuestID    uuid.UUID
	MemberID   *uuid.UUID
	MemberIDs  []uuid.UUID
	DoneAt     time.Time
	ApprovedAt *time.Time
	Status     string
}

// PendingCompletion is a completion waiting for approval, with the chore's
// name, current points and teamwork bonus, the member who marked it done
// (MemberID, MemberName) and every member it covers. Members is never empty:
// a completion that is not shared lists its one member. Each member's Award
// is the chore's current points split among them, rounded up; TeamworkBonus
// is paid on top only when more than one member is listed.
type PendingCompletion struct {
	ID            uuid.UUID
	QuestID       uuid.UUID
	QuestName     string
	Points        int32
	TeamworkBonus int32
	MemberID      uuid.UUID
	MemberName    string
	DoneAt        time.Time
	Members       []PendingMember
}

// PendingMember is one member of a pending completion and the points their
// split will credit (before any teamwork bonus).
type PendingMember struct {
	MemberID uuid.UUID
	Name     string
	Award    int32
}

// splitPoints is each member's share of points among n members, rounded up.
func splitPoints(points int32, n int) int32 {
	if n <= 1 {
		return points
	}
	return int32((int64(points) + int64(n) - 1) / int64(n))
}

// releaseMemberQuests takes a member who is leaving the household off every
// quest and every completion waiting for approval. It is the one place that
// clears or moves a quest's holder when a member goes; qtx is the caller's
// transaction, which already holds the member's row lock (member first, then
// quests and completions, the order the quest actions and Approve use), so the
// release commits or rolls back with the removal. No points move.
//
// Only the member's own part goes: their participant and completion-member
// rows. A shared quest they held passes to the earliest remaining
// participant and is released only when nobody is left; a shared completion
// they marked done passes to the earliest remaining recorded member, stays
// listed and is paid at approval as the remaining n. A quest or completion
// that was theirs alone is released or cancelled as before, and a quest
// resting on a cooldown that a cancelled completion started is available
// again right away, as on reject; every other cooldown stays.
func releaseMemberQuests(ctx context.Context, qtx *db.Queries, householdID, memberID uuid.UUID) error {
	if err := qtx.ReleaseMemberQuests(ctx, db.ReleaseMemberQuestsParams{HouseholdID: householdID, MemberID: memberID}); err != nil {
		return fmt.Errorf("release member quests: %w", err)
	}
	if err := qtx.CancelMemberPendingCompletions(ctx, db.CancelMemberPendingCompletionsParams{HouseholdID: householdID, MemberID: memberID}); err != nil {
		return fmt.Errorf("cancel member completions: %w", err)
	}
	return nil
}

// view adds awaiting_approval and the participants to a quest just read or
// written.
func (s *QuestService) view(ctx context.Context, quest db.HouseholdQuest) (QuestView, error) {
	pending, err := s.q.QuestHasPendingCompletion(ctx, db.QuestHasPendingCompletionParams{HouseholdID: quest.HouseholdID, QuestID: quest.ID})
	if err != nil {
		return QuestView{}, fmt.Errorf("read pending completion: %w", err)
	}
	participants, err := s.q.ListQuestParticipants(ctx, db.ListQuestParticipantsParams{HouseholdID: quest.HouseholdID, QuestID: quest.ID})
	if err != nil {
		return QuestView{}, fmt.Errorf("read participants: %w", err)
	}
	return QuestView{HouseholdQuest: quest, AwaitingApproval: pending, Participants: participants}, nil
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
		quests[i] = QuestView{HouseholdQuest: r.HouseholdQuest, AwaitingApproval: r.AwaitingApproval, Participants: r.Participants}
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
	return QuestView{HouseholdQuest: row.HouseholdQuest, AwaitingApproval: row.AwaitingApproval, Participants: row.Participants}, nil
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
		HouseholdID:   householdID,
		ID:            questID,
		Name:          v.name,
		Points:        v.points,
		RepeatDays:    v.repeatDays,
		Notes:         v.notes,
		Shareable:     v.shareable,
		TeamworkBonus: v.teamworkBonus,
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
	// The member row is locked before the quest row, the order
	// FamilyService.Remove uses, so a concurrent removal cannot deadlock.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return QuestView{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)
	if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: householdID, ID: act.MemberID})); err != nil {
		return QuestView{}, err
	}
	quest, err := qtx.ClaimQuest(ctx, db.ClaimQuestParams{HouseholdID: householdID, ID: questID, MemberID: &act.MemberID})
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return QuestView{}, fmt.Errorf("commit: %w", err)
		}
		return s.view(ctx, quest)
	}
	if isForeignKeyViolation(err) {
		return QuestView{}, ErrNotFound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return QuestView{}, fmt.Errorf("claim quest: %w", err)
	}
	// No row: either the quest is not in this household, or it is not available.
	if _, err := notFoundIfNoRows(qtx.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID})); err != nil {
		return QuestView{}, err
	}
	return QuestView{}, ErrQuestTaken
}

// Abandon takes the acting member off a quest they hold or are on. A quest
// that was theirs alone goes back in the pool for everyone. On a shared
// quest only their part goes: the holder of record passes to the earliest
// remaining participant, and once one member is left the quest is no longer
// shared (approval then pays that member the full points and no bonus). A
// member not on the quest, or a quest not in this household, is ErrNotFound
// and changes nothing.
//
// Every member row on the quest is locked first, in id order, then the
// quest (member first, the order FamilyService.Remove uses): handing the
// quest to another member checks that member's row, which must not wait on
// a concurrent removal that already waits on this quest.
func (s *QuestService) Abandon(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (QuestView, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return QuestView{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return QuestView{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)
	if err := lockQuestMembers(ctx, qtx, householdID, questID, act.MemberID); err != nil {
		return QuestView{}, err
	}
	quest, err := notFoundIfNoRows(qtx.AbandonQuest(ctx, db.AbandonQuestParams{HouseholdID: householdID, ID: questID, MemberID: act.MemberID}))
	if err != nil {
		return QuestView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return QuestView{}, fmt.Errorf("commit: %w", err)
	}
	return s.view(ctx, quest)
}

// Share adds target to the acting member's shareable quest, so both hold it.
// Both members prove themselves with their own PIN, as on accept: a wrong
// PIN for either is ErrWrongPIN. A quest that is not in this household, not
// shareable or not held by the acting member, and a target that is not a
// member of this household, are ErrNotFound. Sharing with oneself or with a
// member already on the quest is ErrAlreadyParticipant. Nothing changes on
// any error. The first share records the holder as a participant too.
func (s *QuestService) Share(ctx context.Context, userID, householdID, questID uuid.UUID, act, target MemberAction) (QuestView, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return QuestView{}, err
	}
	if _, err := VerifyMemberPin(ctx, s.q, householdID, target.MemberID, target.PIN); err != nil {
		return QuestView{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return QuestView{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)
	// Both member rows are locked before the quest row, in id order, the
	// order FamilyService.Remove and the other quest actions use (member
	// first), so neither a concurrent removal nor a crossed share deadlocks.
	for _, id := range memberLockOrder(act.MemberID, target.MemberID) {
		if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: householdID, ID: id})); err != nil {
			// A member was removed between the PIN check and the lock.
			return QuestView{}, err
		}
	}
	added, err := qtx.ShareQuest(ctx, db.ShareQuestParams{
		HouseholdID: householdID, ID: questID, MemberID: act.MemberID, TargetMemberID: target.MemberID,
	})
	if err != nil {
		return QuestView{}, fmt.Errorf("share quest: %w", err)
	}
	quest, err := notFoundIfNoRows(qtx.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID}))
	if err != nil {
		return QuestView{}, err
	}
	if len(added) == 0 {
		// Nothing written: either the quest is not one the actor may share,
		// or the target is the actor or already on it.
		if !quest.Shareable || quest.ClaimedBy == nil || *quest.ClaimedBy != act.MemberID {
			return QuestView{}, ErrNotFound
		}
		return QuestView{}, ErrAlreadyParticipant
	}
	if err := tx.Commit(ctx); err != nil {
		return QuestView{}, fmt.Errorf("commit: %w", err)
	}
	return s.view(ctx, quest)
}

// lockQuestMembers locks, in ascending id order, the acting member and every
// member on the quest (its holder and participants), so a statement that
// records them all on a completion takes no member lock after the quest row.
// The set is read without a lock and re-read under the locks; a share that
// slipped in between adds a member, and the locks are taken again in order
// on a fresh pass (a bounded number of times). The acting member removed
// meanwhile is ErrNotFound; another member removed meanwhile is skipped.
func lockQuestMembers(ctx context.Context, qtx *db.Queries, householdID, questID, actor uuid.UUID) error {
	locked := map[uuid.UUID]bool{}
	for range 3 {
		want, err := questMemberIDs(ctx, qtx, householdID, questID, actor)
		if err != nil {
			return err
		}
		missing := false
		for _, id := range want {
			if !locked[id] {
				missing = true
			}
		}
		if !missing {
			return nil
		}
		for _, id := range want {
			if locked[id] {
				continue
			}
			if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, db.LockFamilyMemberParams{HouseholdID: householdID, ID: id})); err != nil {
				if errors.Is(err, ErrNotFound) && id != actor {
					continue
				}
				// The acting member was removed between the PIN check and the lock.
				return err
			}
			locked[id] = true
		}
	}
	return fmt.Errorf("lock members of quest %s: set kept changing", questID)
}

// questMemberIDs is the actor, the quest's holder and its participants,
// distinct and in ascending id order. A quest not in the household yields
// the actor alone (the mark-done statement then matches nothing).
func questMemberIDs(ctx context.Context, qtx *db.Queries, householdID, questID, actor uuid.UUID) ([]uuid.UUID, error) {
	ids := []uuid.UUID{actor}
	quest, err := qtx.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ids, nil
	case err != nil:
		return nil, fmt.Errorf("get quest: %w", err)
	}
	if quest.ClaimedBy != nil {
		ids = append(ids, *quest.ClaimedBy)
	}
	participants, err := qtx.ListQuestParticipants(ctx, db.ListQuestParticipantsParams{HouseholdID: householdID, QuestID: questID})
	if err != nil {
		return nil, fmt.Errorf("list quest participants: %w", err)
	}
	ids = append(ids, participants...)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(ids), nil
}

// memberLockOrder returns the distinct member ids in ascending order, the
// order a transaction locking several member rows takes them in.
func memberLockOrder(a, b uuid.UUID) []uuid.UUID {
	switch c := bytes.Compare(a[:], b[:]); {
	case c == 0:
		return []uuid.UUID{a}
	case c < 0:
		return []uuid.UUID{a, b}
	default:
		return []uuid.UUID{b, a}
	}
}

// MarkDone records that the acting member finished their quest: its holder,
// or any member on it when it is shared. In one transaction it locks the
// row of every member on the quest, then in one statement inserts one
// pending completion (marker, mark-done time now) covering every member on
// the quest, clears the quest's participants, releases it and starts its
// cooldown from now: the quest comes back at local midnight in the
// household's timezone, repeat_days days after today, whether or not the
// administrator has approved by then. No points move until approval. A quest
// the member is not on, or a member removed meanwhile, is ErrNotFound.
//
// The member locks come first, in id order, because FamilyService.Remove
// takes a member's lock before it releases that member's quests and
// completions; taking the quest row first would deadlock against a
// concurrent removal of any member on the quest.
func (s *QuestService) MarkDone(ctx context.Context, userID, householdID, questID uuid.UUID, act MemberAction) (QuestView, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
		return QuestView{}, err
	}
	completionID, err := uuid.NewV7()
	if err != nil {
		return QuestView{}, fmt.Errorf("new completion id: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return QuestView{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	if err := lockQuestMembers(ctx, qtx, householdID, questID, act.MemberID); err != nil {
		return QuestView{}, err
	}
	row, err := notFoundIfNoRows(qtx.MarkQuestDone(ctx, db.MarkQuestDoneParams{
		HouseholdID: householdID, ID: questID, MemberID: act.MemberID, CompletionID: completionID,
	}))
	if isForeignKeyViolation(err) {
		return QuestView{}, ErrNotFound
	}
	if err != nil {
		return QuestView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return QuestView{}, fmt.Errorf("commit: %w", err)
	}
	return QuestView{HouseholdQuest: db.HouseholdQuest(row), AwaitingApproval: true}, nil
}

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

// ComboBonus is one combo bonus paid by an approval.
type ComboBonus struct {
	ComboID     uuid.UUID
	Name        string
	BonusPoints int32
}

// ApprovedMember is one member an approval credited: their balance after
// the credit (combo bonuses included), their split of the chore's points and
// the teamwork bonus paid on top (0 unless more than one member was on it).
type ApprovedMember struct {
	Member        db.FamilyMember
	PointsAwarded int32
	TeamworkBonus int32
}

// Approve approves one pending completion and credits every member it
// covers, in one transaction: either all of it happens or none does. Admin
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
	row, err := notFoundIfNoRows(qtx.ApproveCompletion(ctx, db.ApproveCompletionParams{HouseholdID: householdID, ID: completionID, MemberID: markerID}))
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
	credited := make([]ApprovedMember, 0, len(members))
	bonuses := []ComboBonus{}
	var marker db.FamilyMember
	for _, id := range members {
		if err := qtx.SetCompletionMemberAward(ctx, db.SetCompletionMemberAwardParams{
			PointsAwarded: pgtype.Int4{Int32: split + bonus, Valid: true}, HouseholdID: householdID, CompletionID: completionID, MemberID: id,
		}); err != nil {
			return fail(fmt.Errorf("record award: %w", err))
		}
		member, err := qtx.AddMemberPoints(ctx, db.AddMemberPointsParams{HouseholdID: householdID, ID: id, Delta: split + bonus})
		if err != nil {
			return fail(fmt.Errorf("credit points: %w", err))
		}
		member, paid, err := payCombos(ctx, qtx, householdID, row.QuestID, member, row.DoneAt)
		if err != nil {
			return fail(err)
		}
		if id == markerID {
			marker = member
			bonuses = paid
		}
		credited = append(credited, ApprovedMember{Member: member, PointsAwarded: split, TeamworkBonus: bonus})
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
		ID:            id,
		HouseholdID:   householdID,
		LibraryID:     libraryID,
		Name:          v.name,
		Points:        v.points,
		RepeatDays:    v.repeatDays,
		Notes:         v.notes,
		Shareable:     v.shareable,
		TeamworkBonus: v.teamworkBonus,
	})
	if err != nil {
		return QuestView{}, fmt.Errorf("create quest: %w", err)
	}
	return QuestView{HouseholdQuest: quest}, nil
}

// validQuest is a QuestInput that passed validateQuest. A library copy
// leaves shareable false and teamworkBonus 0.
type validQuest struct {
	name          string
	points        int32
	repeatDays    int32
	notes         string
	shareable     bool
	teamworkBonus int32
}

// validateQuest checks every field and reports each bad one: name 1-200
// characters, points a whole number >= 0, repeat_days a whole number >= 1,
// notes at most 2000 characters, teamwork_bonus a whole number >= 0 and
// above 0 only on a shareable quest.
func validateQuest(in QuestInput) (validQuest, error) {
	var v validator
	out := validQuest{
		name:          v.text("name", in.Name, 1, 200, false),
		points:        v.intField("points", in.Points, 0, 0),
		repeatDays:    v.intField("repeat_days", in.RepeatDays, 1, 1),
		notes:         v.text("notes", in.Notes, 0, 2000, true),
		shareable:     in.Shareable,
		teamworkBonus: v.intField("teamwork_bonus", in.TeamworkBonus, 0, 0),
	}
	if out.teamworkBonus > 0 && !out.shareable {
		v.fail("teamwork_bonus", "needs shareable")
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
