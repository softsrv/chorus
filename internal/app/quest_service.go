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
// TeamworkBonus means 0, and a bonus above 0 needs Shareable. An absent
// StreakBonus is 0 (no streak bonus) on Create; on Update an absent
// StreakBonus keeps the stored one. MinAge and MaxAge are the quest's
// inclusive age limits; an absent one is no limit on that side, on Create
// and on Update alike (Update replaces the whole range).
type QuestInput struct {
	Name          string
	Points        NumberField
	RepeatDays    NumberField
	Notes         string
	Shareable     bool
	TeamworkBonus NumberField
	StreakBonus   NumberField
	MinAge        NumberField
	MaxAge        NumberField
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
// empty for a quest that is not shared. IneligibleMemberIDs lists the
// household's members whose birthday puts them outside the quest's age range
// today (quest_age_eligible, the rule Accept and Share apply); it is empty
// for a quest with no limits and never holds a member without a birthday.
type QuestView struct {
	db.HouseholdQuest
	AwaitingApproval    bool
	Participants        []uuid.UUID
	IneligibleMemberIDs []uuid.UUID
}

// Shared reports whether more than one member is on the quest.
func (v QuestView) Shared() bool { return len(v.Participants) > 1 }

// Completion is one time a member marked a chore done. Status is pending,
// approved, rejected or cancelled; ApprovedAt is set only when approved.
// MemberID is the member who marked it done. When that member is removed
// while a shared completion is pending, MemberID moves to the earliest
// remaining recorded member, so the completion stays listed and approvable.
// It is nil only on a completion from before quest sharing whose member was
// removed (the key is ON DELETE SET NULL). MemberIDs, set on an approval,
// lists every member the completion credited, earliest first.
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
//
// Each ReleaseMemberQuests and CancelMemberPendingCompletions statement counts
// the members left on a quest or completion from its own snapshot, so the
// quests and the completions are locked first, each in a statement of its own
// (id order). A share, abandon or mark-done of the same quest takes that
// quest's row lock, and a removal of another member on the same completion
// takes that completion's lock. Either one commits before the lock is granted
// and is then in the snapshot, or it waits until this removal commits. A
// statement that read its members before such a commit would drop the
// holder's participant row and leave a new member's behind.
func releaseMemberQuests(ctx context.Context, qtx *db.Queries, householdID, memberID uuid.UUID) error {
	if _, err := qtx.LockMemberQuests(ctx, db.LockMemberQuestsParams{HouseholdID: householdID, MemberID: memberID}); err != nil {
		return fmt.Errorf("lock member quests: %w", err)
	}
	if err := qtx.ReleaseMemberQuests(ctx, db.ReleaseMemberQuestsParams{HouseholdID: householdID, MemberID: memberID}); err != nil {
		return fmt.Errorf("release member quests: %w", err)
	}
	if _, err := qtx.LockMemberPendingCompletions(ctx, db.LockMemberPendingCompletionsParams{HouseholdID: householdID, MemberID: memberID}); err != nil {
		return fmt.Errorf("lock member completions: %w", err)
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
	v := []QuestView{{HouseholdQuest: quest, AwaitingApproval: pending, Participants: participants}}
	if err := addIneligible(ctx, s.q, quest.HouseholdID, v); err != nil {
		return QuestView{}, err
	}
	return v[0], nil
}

// addIneligible fills each view's IneligibleMemberIDs from one household-
// scoped query over the views' quests that have an age limit; a quest with no
// limits has none, so it is not asked about.
func addIneligible(ctx context.Context, q *db.Queries, householdID uuid.UUID, views []QuestView) error {
	var ids []uuid.UUID
	for _, v := range views {
		if v.MinAge.Valid || v.MaxAge.Valid {
			ids = append(ids, v.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.ListIneligibleMemberIDs(ctx, db.ListIneligibleMemberIDsParams{HouseholdID: householdID, QuestIds: ids})
	if err != nil {
		return fmt.Errorf("list ineligible members: %w", err)
	}
	byQuest := make(map[uuid.UUID][]uuid.UUID, len(ids))
	for _, r := range rows {
		byQuest[r.QuestID] = append(byQuest[r.QuestID], r.MemberID)
	}
	for i := range views {
		views[i].IneligibleMemberIDs = byQuest[views[i].ID]
	}
	return nil
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
		quests = quests[:p.Limit]
		if err := addIneligible(ctx, s.q, householdID, quests); err != nil {
			return nil, false, err
		}
		return quests, true, nil
	}
	if err := addIneligible(ctx, s.q, householdID, quests); err != nil {
		return nil, false, err
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
	v := []QuestView{{HouseholdQuest: row.HouseholdQuest, AwaitingApproval: row.AwaitingApproval, Participants: row.Participants}}
	if err := addIneligible(ctx, s.q, householdID, v); err != nil {
		return QuestView{}, err
	}
	return v[0], nil
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

// Update replaces a quest's editable fields. An omitted StreakBonus keeps
// the quest's stored streak bonus instead of resetting it to 0, so an older
// client or a form that leaves the field blank never wipes it; every other
// omitted field is cleared to its default as on Create.
func (s *QuestService) Update(ctx context.Context, userID, householdID, questID uuid.UUID, in QuestInput, adminPin string) (QuestView, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return QuestView{}, err
	}
	v, err := validateQuest(in)
	if err != nil {
		return QuestView{}, err
	}
	if !in.StreakBonus.Set {
		existing, err := notFoundIfNoRows(s.q.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID}))
		if err != nil {
			return QuestView{}, err
		}
		v.streakBonus = existing.StreakBonus
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
		StreakBonus:   v.streakBonus,
		MinAge:        v.minAge,
		MaxAge:        v.maxAge,
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
// carries the library's name, points, days, notes and streak bonus and
// remembers its library_id; editing it later never changes the library. The
// library has no age limits, so the copy is all ages.
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
		streakBonus: item.StreakBonus,
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
// ErrQuestTaken; of two concurrent accepts exactly one succeeds. An
// available quest whose age range leaves the member out (quest_age_eligible,
// checked in ClaimQuest's own statement) gives ErrAgeRestricted, writes
// nothing and stays in the pool.
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
	// No row: the quest is not in this household, it is not available, or
	// the member is outside its age range.
	existing, err := notFoundIfNoRows(qtx.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID}))
	if err != nil {
		return QuestView{}, err
	}
	if QuestAvailable(existing, time.Now()) && (existing.MinAge.Valid || existing.MaxAge.Valid) {
		// The same household-scoped rule ClaimQuest applied, asked of this
		// one quest: a member it lists is out of range.
		rows, err := qtx.ListIneligibleMemberIDs(ctx, db.ListIneligibleMemberIDsParams{HouseholdID: householdID, QuestIds: []uuid.UUID{questID}})
		if err != nil {
			return QuestView{}, fmt.Errorf("list ineligible members: %w", err)
		}
		for _, r := range rows {
			if r.MemberID == act.MemberID {
				return QuestView{}, ErrAgeRestricted
			}
		}
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
// PIN for the acting member is ErrWrongPIN, one for the target is
// ErrWrongTargetPIN (also ErrWrongPIN). The quest is checked before the
// target's PIN so a wrong target PIN is only reported on a share that would
// otherwise succeed. A quest that is not in this household, not shareable or
// not held by the acting member, and a target that is not a member of this
// household, are ErrNotFound. A hold whose claim day has ended
// (claim_expires_at at or before the database's now, even before the expiry
// sweep has released it) is not held: ErrNotFound, checked before the
// target's PIN and before the already-participant and age checks. Sharing
// with oneself or with a member already on the quest is
// ErrAlreadyParticipant. A target outside the quest's age range is
// ErrAgeRestricted. Nothing changes on any error. The first share
// records the holder as a participant too.
func (s *QuestService) Share(ctx context.Context, userID, householdID, questID uuid.UUID, act, target MemberAction) (QuestView, error) {
	if err := s.verifyActingMember(ctx, userID, householdID, act); err != nil {
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
			return QuestView{}, err
		}
	}
	quest, err := notFoundIfNoRows(qtx.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID}))
	if err != nil {
		return QuestView{}, err
	}
	held, err := heldForShare(ctx, qtx, quest, act.MemberID)
	if err != nil {
		return QuestView{}, err
	}
	if !held {
		return QuestView{}, ErrNotFound
	}
	if _, err := VerifyMemberPin(ctx, qtx, householdID, target.MemberID, target.PIN); err != nil {
		if errors.Is(err, ErrWrongPIN) {
			return QuestView{}, ErrWrongTargetPIN
		}
		return QuestView{}, err
	}
	added, err := qtx.ShareQuest(ctx, db.ShareQuestParams{
		HouseholdID: householdID, ID: questID, MemberID: act.MemberID, TargetMemberID: target.MemberID,
	})
	if err != nil {
		return QuestView{}, fmt.Errorf("share quest: %w", err)
	}
	if len(added) == 0 {
		// Nothing written: either the quest is not one the actor may share,
		// or the target is the actor or already on it, or the target is
		// outside the quest's age range. The quest is read again first: a
		// mark-done, abandon or expiry sweep that committed after the read
		// above leaves it not held, which is ErrNotFound like any quest the
		// actor does not hold, never a target error.
		again, err := notFoundIfNoRows(qtx.GetHouseholdQuest(ctx, db.GetHouseholdQuestParams{HouseholdID: householdID, ID: questID}))
		if err != nil {
			return QuestView{}, err
		}
		if held, err := heldForShare(ctx, qtx, again, act.MemberID); err != nil {
			return QuestView{}, err
		} else if !held {
			return QuestView{}, ErrNotFound
		}
		if target.MemberID == act.MemberID {
			return QuestView{}, ErrAlreadyParticipant
		}
		participants, err := qtx.ListQuestParticipants(ctx, db.ListQuestParticipantsParams{
			HouseholdID: householdID,
			QuestID:     questID,
		})
		if err != nil {
			return QuestView{}, fmt.Errorf("list quest participants: %w", err)
		}
		for _, p := range participants {
			if p == target.MemberID {
				return QuestView{}, ErrAlreadyParticipant
			}
		}
		eligible, err := qtx.QuestMemberAgeEligible(ctx, db.QuestMemberAgeEligibleParams{
			HouseholdID: householdID,
			MemberID:    target.MemberID,
			QuestID:     questID,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return QuestView{}, ErrNotFound
		case err != nil:
			return QuestView{}, fmt.Errorf("check age eligibility: %w", err)
		case !eligible:
			return QuestView{}, ErrAgeRestricted
		}
		return QuestView{}, ErrAlreadyParticipant
	}
	if err := tx.Commit(ctx); err != nil {
		return QuestView{}, fmt.Errorf("commit: %w", err)
	}
	return s.view(ctx, quest)
}

// heldForShare reports whether quest is one member may share: shareable,
// held by them, and not past its claim deadline. The deadline is judged by
// the database's clock in this transaction (QuestClaimLapsed), the clock
// ShareQuest, MarkQuestDone and the expiry sweep judge it by, so a hold whose
// day has ended counts as not held even before the sweep has released it.
func heldForShare(ctx context.Context, qtx *db.Queries, quest db.HouseholdQuest, memberID uuid.UUID) (bool, error) {
	if !quest.Shareable || quest.ClaimedBy == nil || *quest.ClaimedBy != memberID {
		return false, nil
	}
	if !quest.ClaimExpiresAt.Valid {
		// A hold always carries a deadline (the household_quests CHECK and
		// trigger); one without is never treated as open-ended.
		return false, nil
	}
	lapsed, err := notFoundIfNoRows(qtx.QuestClaimLapsed(ctx, db.QuestClaimLapsedParams{HouseholdID: quest.HouseholdID, ID: quest.ID}))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !lapsed, nil
}

// lockQuestMembers locks, in ascending id order, the acting member and every
// member on the quest (its holder and participants), so a statement that
// records them all on a completion takes no member lock after the quest row.
// The set is read without a lock and re-read under the locks; a share that
// slipped in between adds a member, and the locks are taken again in order
// on a fresh pass (a bounded number of times). The acting member removed
// meanwhile is ErrNotFound; another member removed meanwhile is skipped.
//
// Once the members are locked, the quest row is locked too, in a statement
// of its own (LockHouseholdQuest). The caller's next statement then reads the
// quest's participants in a snapshot taken after that lock was granted, so a
// share, abandon or mark-done of the same quest that committed while it
// waited is seen, and none can commit under it. Counting the members left in
// the same statement that first waits on the quest row would read them as
// they stood before such a commit. A quest not in this household locks
// nothing; the caller's statement then matches no row.
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
			if _, err := qtx.LockHouseholdQuest(ctx, db.LockHouseholdQuestParams{HouseholdID: householdID, ID: questID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("lock quest: %w", err)
			}
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
// the member is not on, a quest whose claim day has ended (claim_expires_at
// at or before now, even before the expiry sweep has released it), or a
// member removed meanwhile, is ErrNotFound: no completion, no streak step.
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
	v := []QuestView{{HouseholdQuest: db.HouseholdQuest(row), AwaitingApproval: true}}
	if err := addIneligible(ctx, s.q, householdID, v); err != nil {
		return QuestView{}, err
	}
	return v[0], nil
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
	row, err := notFoundIfNoRows(qtx.RejectCompletion(ctx, db.RejectCompletionParams{HouseholdID: householdID, ID: completionID}))
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
		StreakBonus:   v.streakBonus,
		MinAge:        v.minAge,
		MaxAge:        v.maxAge,
	})
	if err != nil {
		return QuestView{}, fmt.Errorf("create quest: %w", err)
	}
	views := []QuestView{{HouseholdQuest: quest}}
	if err := addIneligible(ctx, s.q, householdID, views); err != nil {
		return QuestView{}, err
	}
	return views[0], nil
}

// validQuest is a QuestInput that passed validateQuest. A library copy
// leaves shareable false and teamworkBonus 0; its streakBonus is the
// library's default; its minAge and maxAge are unset (all ages).
type validQuest struct {
	name          string
	points        int32
	repeatDays    int32
	notes         string
	shareable     bool
	teamworkBonus int32
	streakBonus   int32
	minAge        pgtype.Int4
	maxAge        pgtype.Int4
}

// validateQuest checks every field and reports each bad one: name 1-200
// characters, points a whole number >= 0, repeat_days a whole number >= 1,
// notes at most 2000 characters, teamwork_bonus a whole number >= 0 and
// above 0 only on a shareable quest, streak_bonus a whole number >= 0
// (absent is 0, no streak bonus), min_age and max_age each absent (no limit)
// or a whole number >= 0, and min_age not above max_age when both are set.
func validateQuest(in QuestInput) (validQuest, error) {
	var v validator
	out := validQuest{
		name:          v.text("name", in.Name, 1, 200, false),
		points:        v.intField("points", in.Points, 0, 0),
		repeatDays:    v.intField("repeat_days", in.RepeatDays, 1, 1),
		notes:         v.text("notes", in.Notes, 0, 2000, true),
		shareable:     in.Shareable,
		teamworkBonus: v.intField("teamwork_bonus", in.TeamworkBonus, 0, 0),
		streakBonus:   v.intField("streak_bonus", in.StreakBonus, 0, 0),
		minAge:        optionalAge(&v, "min_age", in.MinAge),
		maxAge:        optionalAge(&v, "max_age", in.MaxAge),
	}
	if out.minAge.Valid && out.maxAge.Valid && out.minAge.Int32 > out.maxAge.Int32 {
		v.fail("max_age", "Minimum age can't be above the maximum age.")
	}
	if out.teamworkBonus > 0 && !out.shareable {
		v.fail("teamwork_bonus", "needs shareable")
	}
	// Each member of a shared completion is paid their split (never more
	// than points) plus the bonus, credited as one int32 amount, so the two
	// together must fit in it.
	if int64(out.points)+int64(out.teamworkBonus) > math.MaxInt32 {
		v.fail("teamwork_bonus", fmt.Sprintf("together with points must not exceed %d", math.MaxInt32))
	}
	return out, v.err()
}

// optionalAge checks an age limit: absent is no limit (not Valid); present it
// must be a whole number >= 0, with the same messages as every other number
// field.
func optionalAge(v *validator, field string, f NumberField) pgtype.Int4 {
	if !f.Set {
		return pgtype.Int4{}
	}
	n := v.intField(field, f, 0, 0)
	if _, bad := v.fields[field]; bad {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: n, Valid: true}
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
