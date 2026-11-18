package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/softsrv/chorus/internal/db"
)

// Member is a household family member as returned by FamilyService.
// PIN hashes are never exposed.
type Member struct {
	ID          uuid.UUID `json:"id"`
	HouseholdID uuid.UUID `json:"household_id"`
	Name        string    `json:"name"`
	HasPin      bool      `json:"has_pin"`
	Points      int32     `json:"points"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PointsAward represents a points award entry for a family member.
type PointsAward struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason"`
	Points    int32     `json:"points"`
	EarnedAt  time.Time `json:"earned_at"`
	CreatedAt time.Time `json:"created_at"`
}

// FamilyMember is an alias for Member.
type FamilyMember = Member

// FamilyMemberInput carries fields for creating a family member.
type FamilyMemberInput struct {
	Name string
	PIN  string
}

// RenameMemberInput carries the new name when renaming a family member.
type RenameMemberInput struct {
	Name string
}

// FamilyService manages family members within a household.
type FamilyService struct {
	q          *db.Queries
	pool       pgxBeginner
	bcryptCost int
}

// NewFamilyService constructs a FamilyService. pool runs the transaction that
// removes a member.
func NewFamilyService(q *db.Queries, pool pgxBeginner, bcryptCost int) *FamilyService {
	if bcryptCost < bcrypt.MinCost || bcryptCost > bcrypt.MaxCost {
		bcryptCost = 12
	}
	return &FamilyService{
		q:          q,
		pool:       pool,
		bcryptCost: bcryptCost,
	}
}

func toMember(m db.FamilyMember) Member {
	return Member{
		ID:          m.ID,
		HouseholdID: m.HouseholdID,
		Name:        m.Name,
		HasPin:      m.PinHash.Valid && m.PinHash.String != "",
		Points:      m.Points,
		CreatedAt:   m.CreatedAt.Time.UTC(),
		UpdatedAt:   m.UpdatedAt.Time.UTC(),
	}
}

// List returns all family members of the household, ordered by name, id.
// Non-members receive ErrNotFound.
// Returns an empty, non-nil slice if the household has no family members.
func (s *FamilyService) List(ctx context.Context, userID, householdID uuid.UUID) ([]Member, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, err
	}
	rows, err := s.q.ListFamilyMembers(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list family members: %w", err)
	}
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMember(r))
	}
	return out, nil
}

// Get returns one family member of the household, or ErrNotFound.
func (s *FamilyService) Get(ctx context.Context, userID, householdID, memberID uuid.UUID) (Member, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return Member{}, err
	}
	m, err := s.q.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: householdID, ID: memberID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Member{}, ErrNotFound
		}
		return Member{}, fmt.Errorf("get family member: %w", err)
	}
	return toMember(m), nil
}

// PointsHistory returns the member's awards ordered by created_at DESC then id DESC.
// Caller must be a member of the household (non-members get ErrNotFound).
// The target member must belong to the household (otherwise ErrNotFound).
// Access requires the member's PIN (via VerifyMemberPin), or the caller being the household admin
// with the admin PIN when one is set (no PIN needed when none is set), whatever the member's own PIN.
// Otherwise (wrong or missing PIN, or a non-admin without the member PIN) it returns ErrWrongPIN.
// Empty result returns an empty, non-nil slice.
func (s *FamilyService) PointsHistory(ctx context.Context, userID, householdID, memberID uuid.UUID, memberPin, adminPin string, limit, offset int32) ([]PointsAward, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, err
	}

	if _, err := s.q.GetFamilyMember(ctx, db.GetFamilyMemberParams{
		HouseholdID: householdID,
		ID:          memberID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get family member: %w", err)
	}

	authorized := false
	_, memberErr := VerifyMemberPin(ctx, s.q, householdID, memberID, memberPin)
	if memberErr == nil {
		authorized = true
	} else if !errors.Is(memberErr, ErrWrongPIN) {
		return nil, fmt.Errorf("verify member pin: %w", memberErr)
	}

	if !authorized {
		// Admin fallback: requireAdminPin passes for the household admin when the admin PIN
		// matches, or when no admin PIN is set. A non-admin caller, a missing admin PIN or a
		// wrong one is the same refusal as a wrong member PIN.
		if _, adminErr := requireAdminPin(ctx, s.q, userID, householdID, adminPin); adminErr == nil {
			authorized = true
		} else if !errors.Is(adminErr, ErrForbidden) && !errors.Is(adminErr, ErrAdminPinRequired) && !errors.Is(adminErr, ErrWrongPIN) {
			return nil, fmt.Errorf("verify admin pin: %w", adminErr)
		}
	}

	if !authorized {
		return nil, ErrWrongPIN
	}

	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.q.ListMemberPointsAwards(ctx, db.ListMemberPointsAwardsParams{
		HouseholdID: householdID,
		MemberID:    memberID,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list member points awards: %w", err)
	}

	awards := make([]PointsAward, 0, len(rows))
	for _, r := range rows {
		awards = append(awards, PointsAward{
			ID:        r.ID,
			Kind:      r.Kind,
			Reason:    r.Reason,
			Points:    r.Points,
			EarnedAt:  r.EarnedAt.Time.UTC(),
			CreatedAt: r.CreatedAt.Time.UTC(),
		})
	}
	return awards, nil
}

// AdjustPointsInput is an admin's change to a member's points: Points is the
// signed amount (positive adds, negative removes) and Reason the optional
// note shown in the history.
type AdjustPointsInput struct {
	Points NumberField
	Reason string
}

// Bounds of an admin points adjustment.
const (
	// maxAdjustPoints caps the size of one adjustment either way.
	maxAdjustPoints = 1_000_000
	// maxAdjustReasonLen is the longest reason, in characters.
	maxAdjustReasonLen = 200
	// defaultAdjustReason is stored when the admin leaves the reason blank.
	defaultAdjustReason = "Adjusted by admin"
)

// AdjustPoints adds or removes points for a member of the household and
// records it in the member's history as an admin adjustment. Requires the
// admin role and the admin PIN when one is set. Points must be a whole,
// non-zero number of at most maxAdjustPoints either way; a blank reason is
// stored as defaultAdjustReason.
//
// One transaction locks the member row, checks the new total, and credits
// through creditPoints, so the total and its history row change together or
// not at all, and concurrent adjustments to the same member wait for each
// other. A removal that would take the total below 0 is refused with a
// ValidationError on "points" and nothing is written.
func (s *FamilyService) AdjustPoints(ctx context.Context, userID, householdID, memberID uuid.UUID, in AdjustPointsInput, adminPin string) (Member, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Member{}, err
	}

	var v validator
	var delta int32
	switch p := in.Points; {
	case !p.Set:
		v.fail("points", "is required")
	case p.Invalid:
		v.fail("points", "must be a whole number")
	case p.Value == 0:
		v.fail("points", "must not be 0")
	case p.Value > maxAdjustPoints || p.Value < -maxAdjustPoints:
		v.fail("points", fmt.Sprintf("must be at most %d either way", maxAdjustPoints))
	default:
		delta = int32(p.Value)
	}
	reason := v.text("reason", in.Reason, 0, maxAdjustReasonLen, false)
	if err := v.err(); err != nil {
		return Member{}, err
	}
	if reason == "" {
		reason = defaultAdjustReason
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Member{}, fmt.Errorf("begin adjust points: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	ids := db.LockFamilyMemberParams{HouseholdID: householdID, ID: memberID}
	if _, err := notFoundIfNoRows(qtx.LockFamilyMember(ctx, ids)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Member{}, ErrNotFound
		}
		return Member{}, fmt.Errorf("lock family member: %w", err)
	}
	// Read the locked row in the same transaction: no other write to this
	// member's points can land between this read and the credit below.
	cur, err := qtx.GetFamilyMember(ctx, db.GetFamilyMemberParams{HouseholdID: householdID, ID: memberID})
	if err != nil {
		return Member{}, fmt.Errorf("get family member: %w", err)
	}
	total := int64(cur.Points) + int64(delta)
	if total < 0 {
		return Member{}, &ValidationError{Fields: map[string]string{
			"points": fmt.Sprintf("%s only has %d points — you can't remove %d.", cur.Name, cur.Points, -int64(delta)),
		}}
	}
	if total > math.MaxInt32 {
		return Member{}, &ValidationError{Fields: map[string]string{"points": "would make the total too large"}}
	}

	m, err := creditPoints(ctx, qtx, householdID, memberID, awardKindAdmin, reason, delta,
		pgtype.Timestamptz{Time: time.Now(), Valid: true})
	if err != nil {
		return Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Member{}, fmt.Errorf("commit adjust points: %w", err)
	}
	return toMember(m), nil
}

// Create adds a new family member to the household. Requires admin role and admin PIN if set.
// Name must be 1-50 characters. PIN is optional; if provided, it must be exactly 4 digits.
func (s *FamilyService) Create(ctx context.Context, userID, householdID uuid.UUID, in FamilyMemberInput, adminPin string) (Member, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Member{}, err
	}

	var v validator
	name := v.text("name", in.Name, 1, 50, false)

	var pinHash pgtype.Text
	if in.PIN != "" {
		validPin := v.pin(in.PIN)
		if v.err() == nil {
			hash, err := bcrypt.GenerateFromPassword([]byte(validPin), s.bcryptCost)
			if err != nil {
				return Member{}, fmt.Errorf("hash pin: %w", err)
			}
			pinHash = pgtype.Text{String: string(hash), Valid: true}
		}
	}

	if err := v.err(); err != nil {
		return Member{}, err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Member{}, fmt.Errorf("generate id: %w", err)
	}

	m, err := s.q.CreateFamilyMember(ctx, db.CreateFamilyMemberParams{
		ID:          id,
		HouseholdID: householdID,
		Name:        name,
		PinHash:     pinHash,
	})
	if err != nil {
		return Member{}, fmt.Errorf("create family member: %w", err)
	}
	return toMember(m), nil
}

// Rename changes a family member's name. Requires admin role and admin PIN if set.
func (s *FamilyService) Rename(ctx context.Context, userID, householdID, memberID uuid.UUID, in RenameMemberInput, adminPin string) (Member, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Member{}, err
	}

	var v validator
	name := v.text("name", in.Name, 1, 50, false)
	if err := v.err(); err != nil {
		return Member{}, err
	}

	m, err := s.q.RenameFamilyMember(ctx, db.RenameFamilyMemberParams{
		HouseholdID: householdID,
		ID:          memberID,
		Name:        name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Member{}, ErrNotFound
		}
		return Member{}, fmt.Errorf("rename family member: %w", err)
	}
	return toMember(m), nil
}

// Remove deletes a family member from the household. Requires admin role and admin PIN if set.
// Deleting a family member never deletes any users row.
//
// One transaction locks the member row, has the quest code hand back what the
// member held, and deletes the row: either all of it happens or none does.
// The row lock makes a concurrent Accept or MarkDone by this member wait, so
// it either lands before the lock (and is handed back here) or fails after.
func (s *FamilyService) Remove(ctx context.Context, userID, householdID, memberID uuid.UUID, adminPin string) error {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin remove member: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := s.q.WithTx(tx)

	ids := db.LockFamilyMemberParams{HouseholdID: householdID, ID: memberID}
	if _, err := qtx.LockFamilyMember(ctx, ids); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock family member: %w", err)
	}
	if err := releaseMemberQuests(ctx, qtx, householdID, memberID); err != nil {
		return err
	}
	rows, err := qtx.DeleteFamilyMember(ctx, db.DeleteFamilyMemberParams{
		HouseholdID: householdID,
		ID:          memberID,
	})
	if err != nil {
		return fmt.Errorf("delete family member: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit remove member: %w", err)
	}
	return nil
}

// SetPin updates or sets a family member's 4-digit PIN. Requires admin role and admin PIN if set.
func (s *FamilyService) SetPin(ctx context.Context, userID, householdID, memberID uuid.UUID, pin string, adminPin string) (Member, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Member{}, err
	}

	var v validator
	validPin := v.pin(pin)
	if err := v.err(); err != nil {
		return Member{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(validPin), s.bcryptCost)
	if err != nil {
		return Member{}, fmt.Errorf("hash pin: %w", err)
	}

	m, err := s.q.SetFamilyMemberPin(ctx, db.SetFamilyMemberPinParams{
		HouseholdID: householdID,
		ID:          memberID,
		PinHash:     pgtype.Text{String: string(hash), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Member{}, ErrNotFound
		}
		return Member{}, fmt.Errorf("set member pin: %w", err)
	}
	return toMember(m), nil
}

// ClearPin removes a family member's PIN. Requires admin role and admin PIN if set.
func (s *FamilyService) ClearPin(ctx context.Context, userID, householdID, memberID uuid.UUID, adminPin string) (Member, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return Member{}, err
	}

	m, err := s.q.SetFamilyMemberPin(ctx, db.SetFamilyMemberPinParams{
		HouseholdID: householdID,
		ID:          memberID,
		PinHash:     pgtype.Text{Valid: false},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Member{}, ErrNotFound
		}
		return Member{}, fmt.Errorf("clear member pin: %w", err)
	}
	return toMember(m), nil
}

// MemberBirthday is a member's birthday as only the admin sees it: Birthday is
// YYYY-MM-DD and Age the member's whole years today in the household's
// timezone, worked out by the database (quest_member_age, the same rule the
// quest age checks use). Both are nil when no birthday is set. It is never
// part of Member, so no member or quest read carries it.
type MemberBirthday struct {
	MemberID uuid.UUID
	Birthday *string
	Age      *int32
}

func toMemberBirthday(id uuid.UUID, birthday pgtype.Date, age int32) MemberBirthday {
	out := MemberBirthday{MemberID: id}
	if birthday.Valid {
		s := birthday.Time.Format(time.DateOnly)
		out.Birthday = &s
		out.Age = &age
	}
	return out
}

// SetBirthday sets a family member's birthday (YYYY-MM-DD), or clears it when
// birthday is nil. Requires admin role and admin PIN if set. A birthday that
// is not a real date, or is after today in the household's timezone, is a
// ValidationError on "birthday"; a member of another household is
// ErrNotFound.
func (s *FamilyService) SetBirthday(ctx context.Context, userID, householdID, memberID uuid.UUID, birthday *string, adminPin string) (MemberBirthday, error) {
	h, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin)
	if err != nil {
		return MemberBirthday{}, err
	}

	date := pgtype.Date{}
	if birthday != nil {
		loc, err := time.LoadLocation(h.Timezone)
		if err != nil {
			return MemberBirthday{}, fmt.Errorf("load household timezone %q: %w", h.Timezone, err)
		}
		now := time.Now().In(loc)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		var v validator
		d := v.birthday(*birthday, today)
		if err := v.err(); err != nil {
			return MemberBirthday{}, err
		}
		date = pgtype.Date{Time: d, Valid: true}
	}

	row, err := s.q.SetFamilyMemberBirthday(ctx, db.SetFamilyMemberBirthdayParams{
		Birthday:    date,
		HouseholdID: householdID,
		ID:          memberID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MemberBirthday{}, ErrNotFound
		}
		return MemberBirthday{}, fmt.Errorf("set member birthday: %w", err)
	}
	return toMemberBirthday(row.ID, row.Birthday, row.Age), nil
}

// ListBirthdays returns every member of the household with their birthday
// and age (nil for both when unset). Requires admin role and admin PIN if
// set; non-members get ErrNotFound.
func (s *FamilyService) ListBirthdays(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]MemberBirthday, error) {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, adminPin); err != nil {
		return nil, err
	}
	rows, err := s.q.ListFamilyMemberBirthdays(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list member birthdays: %w", err)
	}
	out := make([]MemberBirthday, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMemberBirthday(r.ID, r.Birthday, r.Age))
	}
	return out, nil
}

// VerifyMemberPin verifies a member's PIN within a household.
// It verifies that memberID belongs to householdID (returning ErrNotFound if not).
// If the member has a PIN set, pin must match its bcrypt hash; empty or mismatch returns ErrWrongPIN.
// If the member has no PIN set, an empty pin succeeds; a non-empty pin returns ErrWrongPIN.
func VerifyMemberPin(ctx context.Context, q *db.Queries, householdID, memberID uuid.UUID, pin string) (db.FamilyMember, error) {
	m, err := q.GetFamilyMember(ctx, db.GetFamilyMemberParams{
		HouseholdID: householdID,
		ID:          memberID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FamilyMember{}, ErrNotFound
		}
		return db.FamilyMember{}, fmt.Errorf("get family member: %w", err)
	}

	hasPin := m.PinHash.Valid && m.PinHash.String != ""
	if hasPin {
		if pin == "" {
			return db.FamilyMember{}, ErrWrongPIN
		}
		if err := bcrypt.CompareHashAndPassword([]byte(m.PinHash.String), []byte(pin)); err != nil {
			return db.FamilyMember{}, ErrWrongPIN
		}
	} else {
		if pin != "" {
			return db.FamilyMember{}, ErrWrongPIN
		}
	}

	return m, nil
}

// Verify checks a family member's PIN and returns the member details.
// Caller must be a member of the household (non-members get ErrNotFound).
func (s *FamilyService) Verify(ctx context.Context, userID, householdID, memberID uuid.UUID, pin string) (Member, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return Member{}, err
	}
	m, err := VerifyMemberPin(ctx, s.q, householdID, memberID, pin)
	if err != nil {
		return Member{}, err
	}
	return toMember(m), nil
}

// AdminPinSet returns whether an admin PIN has been set for the household.
// Requires admin role.
func (s *FamilyService) AdminPinSet(ctx context.Context, userID, householdID uuid.UUID) (bool, error) {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return false, err
	}
	_, err := s.q.GetAdminPin(ctx, householdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("get admin pin: %w", err)
	}
	return true, nil
}

// SetAdminPin sets or updates the household's 4-digit admin PIN.
// If an admin PIN is already set, currentAdminPin must match it.
func (s *FamilyService) SetAdminPin(ctx context.Context, userID, householdID uuid.UUID, newPin, currentAdminPin string) error {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, currentAdminPin); err != nil {
		return err
	}

	var v validator
	validPin := v.pin(newPin)
	if err := v.err(); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(validPin), s.bcryptCost)
	if err != nil {
		return fmt.Errorf("hash admin pin: %w", err)
	}

	if err := s.q.UpsertAdminPin(ctx, db.UpsertAdminPinParams{
		HouseholdID: householdID,
		PinHash:     string(hash),
	}); err != nil {
		return fmt.Errorf("upsert admin pin: %w", err)
	}
	return nil
}

// ClearAdminPin removes the household's admin PIN.
// If an admin PIN is currently set, currentAdminPin must match it.
func (s *FamilyService) ClearAdminPin(ctx context.Context, userID, householdID uuid.UUID, currentAdminPin string) error {
	if _, err := requireAdminPin(ctx, s.q, userID, householdID, currentAdminPin); err != nil {
		return err
	}

	if _, err := s.q.DeleteAdminPin(ctx, householdID); err != nil {
		return fmt.Errorf("delete admin pin: %w", err)
	}
	return nil
}

// VerifyAdminPin verifies that pin matches the household's admin PIN.
// If no admin PIN is set, or if pin does not match, ErrWrongPIN is returned.
func (s *FamilyService) VerifyAdminPin(ctx context.Context, userID, householdID uuid.UUID, pin string) error {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return err
	}

	pinRow, err := s.q.GetAdminPin(ctx, householdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWrongPIN
		}
		return fmt.Errorf("get admin pin: %w", err)
	}

	if pin == "" {
		return ErrWrongPIN
	}

	if err := bcrypt.CompareHashAndPassword([]byte(pinRow.PinHash), []byte(pin)); err != nil {
		return ErrWrongPIN
	}
	return nil
}
