package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/softsrv/chorus/internal/db"
)

// Household roles.
const (
	RoleAdmin = "admin"
)

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

// HouseholdService manages households and their single admin login.
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Household{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	h, err := createHouseholdTx(ctx, qtx, userID, in.Name, in.Timezone)
	if err != nil {
		return Household{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Household{}, fmt.Errorf("commit: %w", err)
	}
	return Household{Household: h, Role: RoleAdmin}, nil
}

func createHouseholdTx(ctx context.Context, qtx *db.Queries, userID uuid.UUID, name, timezone string) (db.Household, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return db.Household{}, fmt.Errorf("generate household id: %w", err)
	}
	h, err := qtx.CreateHousehold(ctx, db.CreateHouseholdParams{ID: id, Name: name, Timezone: timezone})
	if err != nil {
		return db.Household{}, fmt.Errorf("create household: %w", err)
	}
	if err := qtx.AddHouseholdMember(ctx, db.AddHouseholdMemberParams{HouseholdID: id, UserID: userID, Role: RoleAdmin}); err != nil {
		return db.Household{}, fmt.Errorf("add creator as admin: %w", err)
	}
	return h, nil
}

func provisionFirstHousehold(ctx context.Context, pool pgxBeginner, q *db.Queries, user db.User) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := q.WithTx(tx)
	rows, err := qtx.ClaimHouseholdProvisioning(ctx, user.ID)
	if err != nil {
		return fmt.Errorf("claim household provisioning: %w", err)
	}
	if rows == 0 {
		return nil
	}

	name := defaultHouseholdName(user.DisplayName)

	if _, err := createHouseholdTx(ctx, qtx, user.ID, name, "UTC"); err != nil {
		return fmt.Errorf("create default household: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// maxHouseholdNameRunes is the households.name limit (validateHousehold).
const maxHouseholdNameRunes = 100

// defaultHouseholdName is the name of a household created at first sign-in:
// "<display name>'s household", cut to the household name limit in runes.
func defaultHouseholdName(displayName string) string {
	runes := []rune(displayName + "'s household")
	if len(runes) > maxHouseholdNameRunes {
		runes = runes[:maxHouseholdNameRunes]
	}
	return string(runes)
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

// Delete removes a household and everything in it. Admins only. The user is
// then due a new household at their next sign-in.
func (s *HouseholdService) Delete(ctx context.Context, userID, householdID uuid.UUID) error {
	if _, err := requireAdmin(ctx, s.q, userID, householdID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	if err := qtx.DeleteHousehold(ctx, householdID); err != nil {
		return fmt.Errorf("delete household: %w", err)
	}
	// With no create or join, provisioning is the only way back into a
	// household: re-arm it so the next sign-in gives this user a new one.
	if err := qtx.ReleaseHouseholdProvisioning(ctx, userID); err != nil {
		return fmt.Errorf("release household provisioning: %w", err)
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

// requireAdminPin validates that the user is an admin of the household, and if the household has an
// admin PIN set, requires that adminPin matches the stored hash.
// If an admin PIN is set and adminPin is empty, ErrAdminPinRequired is returned.
// If adminPin does not match, ErrWrongPIN is returned.
// Non-members receive ErrNotFound (via requireAdmin -> getHousehold).
func requireAdminPin(ctx context.Context, q *db.Queries, userID, householdID uuid.UUID, adminPin string) (Household, error) {
	h, err := requireAdmin(ctx, q, userID, householdID)
	if err != nil {
		return Household{}, err
	}

	pinRow, err := q.GetAdminPin(ctx, householdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No admin PIN is set on this household; requireAdmin was sufficient.
			return h, nil
		}
		return Household{}, fmt.Errorf("get admin pin: %w", err)
	}

	if adminPin == "" {
		return Household{}, ErrAdminPinRequired
	}

	if err := bcrypt.CompareHashAndPassword([]byte(pinRow.PinHash), []byte(adminPin)); err != nil {
		return Household{}, ErrWrongPIN
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
