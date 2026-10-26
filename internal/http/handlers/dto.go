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

const dateLayout = "2006-01-02"

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

type memberJSON struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joined_at"`
}

func toMember(m db.ListHouseholdMembersRow) memberJSON {
	return memberJSON{UserID: m.UserID, DisplayName: m.DisplayName, Role: m.Role, JoinedAt: m.JoinedAt.Time.UTC()}
}

type choreJSON struct {
	ID          uuid.UUID  `json:"id"`
	HouseholdID uuid.UUID  `json:"household_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	AssigneeID  *uuid.UUID `json:"assignee_id"`
	DueOn       *string    `json:"due_on"`
	Completed   bool       `json:"completed"`
	CompletedAt *time.Time `json:"completed_at"`
	CompletedBy *uuid.UUID `json:"completed_by"`
	CreatedBy   *uuid.UUID `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func toChore(c db.Chore) choreJSON {
	return choreJSON{
		ID:          c.ID,
		HouseholdID: c.HouseholdID,
		Title:       c.Title,
		Description: c.Description,
		AssigneeID:  c.AssigneeID,
		DueOn:       dateString(c.DueOn),
		Completed:   c.CompletedAt.Valid,
		CompletedAt: timePtr(c.CompletedAt),
		CompletedBy: c.CompletedBy,
		CreatedBy:   c.CreatedBy,
		CreatedAt:   c.CreatedAt.Time.UTC(),
		UpdatedAt:   c.UpdatedAt.Time.UTC(),
	}
}

type eventJSON struct {
	ID          uuid.UUID  `json:"id"`
	HouseholdID uuid.UUID  `json:"household_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Location    string     `json:"location"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      time.Time  `json:"ends_at"`
	AllDay      bool       `json:"all_day"`
	CreatedBy   *uuid.UUID `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func toEvent(e db.Event) eventJSON {
	return eventJSON{
		ID:          e.ID,
		HouseholdID: e.HouseholdID,
		Title:       e.Title,
		Description: e.Description,
		Location:    e.Location,
		StartsAt:    e.StartsAt.Time.UTC(),
		EndsAt:      e.EndsAt.Time.UTC(),
		AllDay:      e.AllDay,
		CreatedBy:   e.CreatedBy,
		CreatedAt:   e.CreatedAt.Time.UTC(),
		UpdatedAt:   e.UpdatedAt.Time.UTC(),
	}
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func dateString(d pgtype.Date) *string {
	if !d.Valid {
		return nil
	}
	s := d.Time.Format(dateLayout)
	return &s
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}
