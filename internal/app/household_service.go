package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
)

// Household roles.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

const inviteLifetime = 7 * 24 * time.Hour

// Household is a household as seen by one of its members.
type Household struct {
	db.Household
	Role string // the viewing user's role
}

// HouseholdInput is the editable part of a household.
type HouseholdInput struct {
	Name     string
	Timezone string
}

// Invite is a newly created invite. Code is only available at creation time;
// the database stores just its hash.
type Invite struct {
	Code      string
	ExpiresAt time.Time
}

// HouseholdService manages households, their membership, and invites.
type HouseholdService struct {
	q    *db.Queries
	pool pgxBeginner
}

// NewHouseholdService constructs a HouseholdService.
func NewHouseholdService(q *db.Queries, pool pgxBeginner) *HouseholdService {
	return &HouseholdService{q: q, pool: pool}
}

// List returns every household the user belongs to.
func (s *HouseholdService) List(ctx context.Context, userID uuid.UUID) ([]Household, error) {
	rows, err := s.q.ListHouseholdsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list households: %w", err)
	}
	out := make([]Household, len(rows))
	for i, r := range rows {
		out[i] = Household{
			Household: db.Household{ID: r.ID, Name: r.Name, Timezone: r.Timezone, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt},
			Role:      r.Role,
		}
	}
	return out, nil
}

// Get returns a household the user belongs to, or ErrNotFound.
func (s *HouseholdService) Get(ctx context.Context, userID, householdID uuid.UUID) (Household, error) {
	return getHousehold(ctx, s.q, userID, householdID)
}

// Create makes a new household with the creator as its first admin.
func (s *HouseholdService) Create(ctx context.Context, userID uuid.UUID, in HouseholdInput) (Household, error) {
	in, err := validateHousehold(in)
	if err != nil {
		return Household{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Household{}, fmt.Errorf("generate household id: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Household{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	h, err := qtx.CreateHousehold(ctx, db.CreateHouseholdParams{ID: id, Name: in.Name, Timezone: in.Timezone})
	if err != nil {
		return Household{}, fmt.Errorf("create household: %w", err)
	}
	if err := qtx.AddHouseholdMember(ctx, db.AddHouseholdMemberParams{HouseholdID: id, UserID: userID, Role: RoleAdmin}); err != nil {
		return Household{}, fmt.Errorf("add creator as admin: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Household{}, fmt.Errorf("commit: %w", err)
	}
	return Household{Household: h, Role: RoleAdmin}, nil
}

// Update replaces a household's editable fields. Admins only.
func (s *HouseholdService) Update(ctx context.Context, userID, householdID uuid.UUID, in HouseholdInput) (Household, error) {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return Household{}, err
	}
	in, err := validateHousehold(in)
	if err != nil {
		return Household{}, err
	}
	h, err := s.q.UpdateHousehold(ctx, db.UpdateHouseholdParams{ID: householdID, Name: in.Name, Timezone: in.Timezone})
	if err != nil {
		return Household{}, fmt.Errorf("update household: %w", err)
	}
	return Household{Household: h, Role: RoleAdmin}, nil
}

// Delete removes a household and everything in it. Admins only.
func (s *HouseholdService) Delete(ctx context.Context, userID, householdID uuid.UUID) error {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return err
	}
	if err := s.q.DeleteHousehold(ctx, householdID); err != nil {
		return fmt.Errorf("delete household: %w", err)
	}
	return nil
}

// Members lists a household's members. Any member may call it.
func (s *HouseholdService) Members(ctx context.Context, userID, householdID uuid.UUID) ([]db.ListHouseholdMembersRow, error) {
	if _, err := getHousehold(ctx, s.q, userID, householdID); err != nil {
		return nil, err
	}
	members, err := s.q.ListHouseholdMembers(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return members, nil
}

// SetMemberRole changes a member's role. Admins only; refuses to demote the
// last admin.
func (s *HouseholdService) SetMemberRole(ctx context.Context, userID, householdID, memberID uuid.UUID, role string) error {
	if role != RoleAdmin && role != RoleMember {
		return &ValidationError{Fields: map[string]string{"role": "must be \"admin\" or \"member\""}}
	}
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return err
	}
	return s.withAdminLock(ctx, householdID, func(qtx *db.Queries, admins []uuid.UUID) error {
		if role == RoleMember && len(admins) == 1 && admins[0] == memberID {
			return ErrLastAdmin
		}
		n, err := qtx.UpdateHouseholdMemberRole(ctx, db.UpdateHouseholdMemberRoleParams{
			HouseholdID: householdID, UserID: memberID, Role: role,
		})
		if err != nil {
			return fmt.Errorf("update role: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RemoveMember removes memberID from the household. Admins may remove anyone;
// any member may remove themselves (leave). The last admin can't leave while
// other members remain; if they're the last member, the household is deleted.
func (s *HouseholdService) RemoveMember(ctx context.Context, userID, householdID, memberID uuid.UUID) error {
	h, err := getHousehold(ctx, s.q, userID, householdID)
	if err != nil {
		return err
	}
	if memberID != userID && h.Role != RoleAdmin {
		return ErrForbidden
	}
	return s.withAdminLock(ctx, householdID, func(qtx *db.Queries, admins []uuid.UUID) error {
		if len(admins) == 1 && admins[0] == memberID {
			members, err := qtx.ListHouseholdMembers(ctx, householdID)
			if err != nil {
				return fmt.Errorf("list members: %w", err)
			}
			if len(members) > 1 {
				return ErrLastAdmin
			}
			// Last member leaving: the household goes with them.
			return qtx.DeleteHousehold(ctx, householdID)
		}
		n, err := qtx.RemoveHouseholdMember(ctx, db.RemoveHouseholdMemberParams{HouseholdID: householdID, UserID: memberID})
		if err != nil {
			return fmt.Errorf("remove member: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CreateInvite issues a single-use invite code. Admins only.
func (s *HouseholdService) CreateInvite(ctx context.Context, userID, householdID uuid.UUID) (Invite, error) {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return Invite{}, err
	}
	raw, hashed, err := auth.GenerateInviteCode()
	if err != nil {
		return Invite{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Invite{}, fmt.Errorf("generate invite id: %w", err)
	}
	expiresAt := time.Now().Add(inviteLifetime)
	if _, err := s.q.InsertHouseholdInvite(ctx, db.InsertHouseholdInviteParams{
		ID:          id,
		HouseholdID: householdID,
		TokenHash:   hashed,
		CreatedBy:   &userID,
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return Invite{}, fmt.Errorf("insert invite: %w", err)
	}
	return Invite{Code: raw, ExpiresAt: expiresAt}, nil
}

// AcceptInvite redeems an invite code, adding the user as a member.
func (s *HouseholdService) AcceptInvite(ctx context.Context, userID uuid.UUID, code string) (Household, error) {
	hash := auth.HashToken(auth.NormalizeInviteCode(code))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Household{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	// Row lock serializes concurrent redemptions of the same code.
	inv, err := qtx.GetHouseholdInviteByHashForUpdate(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Household{}, ErrInviteInvalid
		}
		return Household{}, fmt.Errorf("get invite: %w", err)
	}
	if inv.UsedAt.Valid || inv.ExpiresAt.Time.Before(time.Now()) {
		return Household{}, ErrInviteInvalid
	}
	if err := qtx.AddHouseholdMember(ctx, db.AddHouseholdMemberParams{
		HouseholdID: inv.HouseholdID, UserID: userID, Role: RoleMember,
	}); err != nil {
		if isUniqueViolation(err) {
			return Household{}, ErrAlreadyMember
		}
		return Household{}, fmt.Errorf("add member: %w", err)
	}
	if err := qtx.MarkHouseholdInviteUsed(ctx, db.MarkHouseholdInviteUsedParams{ID: inv.ID, UsedBy: &userID}); err != nil {
		return Household{}, fmt.Errorf("mark invite used: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Household{}, fmt.Errorf("commit: %w", err)
	}
	return getHousehold(ctx, s.q, userID, inv.HouseholdID)
}

// withAdminLock runs fn in a transaction holding row locks on the household's
// admin memberships, so two concurrent demotions/removals can't both pass the
// "is this the last admin?" check.
func (s *HouseholdService) withAdminLock(ctx context.Context, householdID uuid.UUID, fn func(qtx *db.Queries, admins []uuid.UUID) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	admins, err := qtx.LockHouseholdAdmins(ctx, householdID)
	if err != nil {
		return fmt.Errorf("lock admins: %w", err)
	}
	if err := fn(qtx, admins); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ── shared membership checks (used by every household-scoped service) ─────────

// getHousehold loads a household the user belongs to. Non-members get
// ErrNotFound, indistinguishable from a household that doesn't exist.
func getHousehold(ctx context.Context, q *db.Queries, userID, householdID uuid.UUID) (Household, error) {
	row, err := q.GetHouseholdForMember(ctx, db.GetHouseholdForMemberParams{ID: householdID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Household{}, ErrNotFound
		}
		return Household{}, fmt.Errorf("get household: %w", err)
	}
	return Household{
		Household: db.Household{ID: row.ID, Name: row.Name, Timezone: row.Timezone, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt},
		Role:      row.Role,
	}, nil
}

// requireAdmin is getHousehold plus an admin-role check.
func requireAdmin(ctx context.Context, q *db.Queries, userID, householdID uuid.UUID) (Household, error) {
	h, err := getHousehold(ctx, q, userID, householdID)
	if err != nil {
		return Household{}, err
	}
	if h.Role != RoleAdmin {
		return Household{}, ErrForbidden
	}
	return h, nil
}

func validateHousehold(in HouseholdInput) (HouseholdInput, error) {
	var v validator
	in.Name = v.text("name", in.Name, 1, 100, false)
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil || in.Timezone == "Local" {
		v.fail("timezone", "must be an IANA time zone name such as \"America/Chicago\"")
	}
	return in, v.err()
}
