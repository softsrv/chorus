package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// These types define the API's wire format. They are deliberately separate
// from the sqlc models so schema changes can't silently change the API (or
// leak columns such as password_hash).

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type userJSON struct {
	ID            uuid.UUID `json:"id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	EmailVerified bool      `json:"email_verified"`
	// HasPassword is false for passwordless (Google-only) accounts, which
	// can't change a password or confirm destructive actions with one.
	HasPassword bool      `json:"has_password"`
	CreatedAt   time.Time `json:"created_at"`
}

func toUser(u db.User) userJSON {
	return userJSON{
		ID:            u.ID,
		Email:         u.Email,
		DisplayName:   u.DisplayName,
		EmailVerified: u.EmailVerified,
		HasPassword:   u.PasswordHash.Valid && u.PasswordHash.String != "",
		CreatedAt:     u.CreatedAt.Time.UTC(),
	}
}

type tokenJSON struct {
	TokenType             string    `json:"token_type"`
	AccessToken           string    `json:"access_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
	User                  userJSON  `json:"user"`
}

func toTokens(t app.TokenResult) tokenJSON {
	return tokenJSON{
		TokenType:             "Bearer",
		AccessToken:           t.AccessToken,
		AccessTokenExpiresAt:  t.AccessTokenExpiry.UTC(),
		RefreshToken:          t.RefreshToken,
		RefreshTokenExpiresAt: t.RefreshTokenExpiry.UTC(),
		User:                  toUser(t.User),
	}
}

type sessionJSON struct {
	ID         uuid.UUID `json:"id"`
	DeviceName string    `json:"device_name"`
	IPAddress  string    `json:"ip_address"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

func toSession(rt db.RefreshToken, current uuid.UUID) sessionJSON {
	s := sessionJSON{
		ID:         rt.ID,
		DeviceName: rt.DeviceName.String,
		CreatedAt:  rt.CreatedAt.Time.UTC(),
		LastUsedAt: rt.LastUsedAt.Time.UTC(),
		ExpiresAt:  rt.ExpiresAt.Time.UTC(),
		Current:    rt.ID == current,
	}
	if rt.IpAddress != nil {
		s.IPAddress = rt.IpAddress.String()
	}
	return s
}

type householdJSON struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Timezone  string    `json:"timezone"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toHousehold(h app.Household) householdJSON {
	return householdJSON{
		ID:        h.ID,
		Name:      h.Name,
		Timezone:  h.Timezone,
		Role:      h.Role,
		CreatedAt: h.CreatedAt.Time.UTC(),
		UpdatedAt: h.UpdatedAt.Time.UTC(),
	}
}

type familyMemberJSON struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	HasPIN bool      `json:"has_pin"`
	Points int32     `json:"points"`
}

func toFamilyMember(m db.FamilyMember) familyMemberJSON {
	return familyMemberJSON{
		ID:     m.ID,
		Name:   m.Name,
		HasPIN: m.PinHash.Valid && m.PinHash.String != "",
		Points: m.Points,
	}
}

// questJSON is a household quest. The lifecycle fields (claimed_by,
// claimed_at, done_at, completed_at, available_at) are read-only and change
// only through quest actions. available says the quest is in the pool;
// awaiting_approval says a member marked it done and the administrator has
// not yet approved or sent it back.
type questJSON struct {
	ID               uuid.UUID  `json:"id"`
	HouseholdID      uuid.UUID  `json:"household_id"`
	LibraryID        *uuid.UUID `json:"library_id"`
	Name             string     `json:"name"`
	Points           int32      `json:"points"`
	RepeatDays       int32      `json:"repeat_days"`
	Notes            string     `json:"notes"`
	Available        bool       `json:"available"`
	ClaimedBy        *uuid.UUID `json:"claimed_by"`
	ClaimedAt        *time.Time `json:"claimed_at"`
	DoneAt           *time.Time `json:"done_at"`
	AwaitingApproval bool       `json:"awaiting_approval"`
	CompletedAt      *time.Time `json:"completed_at"`
	AvailableAt      *time.Time `json:"available_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func toQuest(q db.HouseholdQuest) questJSON {
	return questJSON{
		ID:               q.ID,
		HouseholdID:      q.HouseholdID,
		LibraryID:        q.LibraryID,
		Name:             q.Name,
		Points:           q.Points,
		RepeatDays:       q.RepeatDays,
		Notes:            q.Notes,
		Available:        app.QuestAvailable(q, time.Now()),
		ClaimedBy:        q.ClaimedBy,
		ClaimedAt:        timePtr(q.ClaimedAt),
		DoneAt:           timePtr(q.DoneAt),
		AwaitingApproval: q.DoneAt.Valid,
		CompletedAt:      timePtr(q.CompletedAt),
		AvailableAt:      timePtr(q.AvailableAt),
		CreatedAt:        q.CreatedAt.Time.UTC(),
		UpdatedAt:        q.UpdatedAt.Time.UTC(),
	}
}

// libraryQuestJSON is a quest of the global, read-only library.
type libraryQuestJSON struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Points     int32     `json:"points"`
	RepeatDays int32     `json:"repeat_days"`
	Notes      string    `json:"notes"`
}

func toLibraryQuest(q db.QuestLibrary) libraryQuestJSON {
	return libraryQuestJSON{ID: q.ID, Name: q.Name, Points: q.Points, RepeatDays: q.RepeatDays, Notes: q.Notes}
}

type pointsAwardJSON struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	Reason   string    `json:"reason"`
	Points   int32     `json:"points"`
	EarnedAt time.Time `json:"earned_at"`
}

func toPointsAward(a app.PointsAward) pointsAwardJSON {
	return pointsAwardJSON{
		ID:       a.ID,
		Kind:     a.Kind,
		Reason:   a.Reason,
		Points:   a.Points,
		EarnedAt: a.EarnedAt,
	}
}

// rewardJSON is a household reward. library_id is set when it was adopted
// from the library.
type rewardJSON struct {
	ID          uuid.UUID  `json:"id"`
	HouseholdID uuid.UUID  `json:"household_id"`
	LibraryID   *uuid.UUID `json:"library_id"`
	Description string     `json:"description"`
	Cost        int32      `json:"cost"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func toReward(r db.HouseholdReward) rewardJSON {
	return rewardJSON{
		ID:          r.ID,
		HouseholdID: r.HouseholdID,
		LibraryID:   r.LibraryID,
		Description: r.Description,
		Cost:        r.Cost,
		CreatedAt:   r.CreatedAt.Time.UTC(),
		UpdatedAt:   r.UpdatedAt.Time.UTC(),
	}
}

// libraryRewardJSON is a reward of the global, read-only library.
type libraryRewardJSON struct {
	ID          uuid.UUID `json:"id"`
	Description string    `json:"description"`
	Cost        int32     `json:"cost"`
}

func toLibraryReward(r db.RewardLibrary) libraryRewardJSON {
	return libraryRewardJSON{ID: r.ID, Description: r.Description, Cost: r.Cost}
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}
