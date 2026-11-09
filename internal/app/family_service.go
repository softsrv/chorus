package app

import (
	"context"
	"errors"
	"fmt"
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
	bcryptCost int
}

// NewFamilyService constructs a FamilyService.
func NewFamilyService(q *db.Queries, bcryptCost int) *FamilyService {
	if bcryptCost < bcrypt.MinCost || bcryptCost > bcrypt.MaxCost {
		bcryptCost = 12
	}
	return &FamilyService{
		q:          q,
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
func (s *FamilyService) List(ctx context.Context, userID, householdID uuid.UUID) ([]Member, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, err
	}
	rows, err := s.q.ListFamilyMembers(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list family members: %w", err)
	}
	out := make([]Member, len(rows))
	for i, r := range rows {
		out[i] = toMember(r)
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

// Create adds a new family member to the household. Requires admin role.
// Name must be 1-50 characters. PIN is optional; if provided, it must be exactly 4 digits.
func (s *FamilyService) Create(ctx context.Context, userID, householdID uuid.UUID, in FamilyMemberInput) (Member, error) {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
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

// Rename changes a family member's name. Requires admin role.
func (s *FamilyService) Rename(ctx context.Context, userID, householdID, memberID uuid.UUID, in RenameMemberInput) (Member, error) {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
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

// Remove deletes a family member from the household. Requires admin role.
// Deleting a family member never deletes any users row.
func (s *FamilyService) Remove(ctx context.Context, userID, householdID, memberID uuid.UUID) error {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return err
	}

	rows, err := s.q.DeleteFamilyMember(ctx, db.DeleteFamilyMemberParams{
		HouseholdID: householdID,
		ID:          memberID,
	})
	if err != nil {
		return fmt.Errorf("delete family member: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPin updates or sets a family member's 4-digit PIN. Requires admin role.
func (s *FamilyService) SetPin(ctx context.Context, userID, householdID, memberID uuid.UUID, pin string) (Member, error) {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
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

// ClearPin removes a family member's PIN. Requires admin role.
func (s *FamilyService) ClearPin(ctx context.Context, userID, householdID, memberID uuid.UUID) (Member, error) {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
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
