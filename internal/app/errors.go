package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalidCredentials   = errors.New("invalid email or password")
	ErrAccountLocked        = errors.New("account is temporarily locked")
	ErrUserNotFound         = errors.New("user not found")
	ErrTokenNotFound        = errors.New("token not found")
	ErrTokenRevoked         = errors.New("token has been revoked")
	ErrTokenExpired         = errors.New("token has expired")
	ErrTokenInvalid         = errors.New("token is invalid")
	ErrTokenUsed            = errors.New("token has already been used")
	ErrEmailAlreadyVerified = errors.New("email is already verified")
	ErrForbidden            = errors.New("access denied")

	// ErrNotFound covers both "doesn't exist" and "exists but you can't see it"
	// so that resource IDs from other households can't be probed.
	ErrNotFound = errors.New("not found")
	// ErrLastAdmin is returned when an operation would leave a household that
	// still has members without any admin.
	ErrLastAdmin = errors.New("household must keep at least one admin")
	// ErrAlreadyMember is returned when accepting an invite to a household the
	// user already belongs to.
	ErrAlreadyMember = errors.New("already a member of this household")
	// ErrInviteInvalid covers unknown, expired, and already-used invite codes.
	ErrInviteInvalid = errors.New("invite code is invalid or has expired")
)

// RateLimitedError is returned when an operation is attempted too frequently.
// RetryAt is the earliest time the caller may try again.
type RateLimitedError struct {
	RetryAt time.Time
}

func (e RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited: retry after %s", e.RetryAt.Format(time.RFC3339))
}

// ValidationError reports one or more invalid input fields. Fields maps the
// JSON field name to a human-readable message.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// HouseholdBlockingError is returned when deleting an account would leave a
// household with members but no admin. Households lists the affected names.
type HouseholdBlockingError struct {
	Households []string
}

func (e HouseholdBlockingError) Error() string {
	return "account is the only admin of households with other members: " + strings.Join(e.Households, ", ")
}
