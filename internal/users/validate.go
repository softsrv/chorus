package users

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

var (
	ErrEmailInvalid     = errors.New("email address is invalid")
	ErrPasswordTooShort = errors.New("password is too short")
	ErrPasswordTooLong  = errors.New("password is too long")
)

// maxPasswordBytes is the effective ceiling imposed by bcrypt, which silently
// truncates any input beyond 72 bytes. We reject longer passwords outright so
// that the bytes a user types are the bytes that actually protect the account.
const maxPasswordBytes = 72

// NormalizeEmail lowercases and trims an email address.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail returns nil if the email is RFC 5322 compliant after normalization.
func ValidateEmail(email string) error {
	if _, err := mail.ParseAddress(email); err != nil {
		return fmt.Errorf("%w: %s", ErrEmailInvalid, email)
	}
	return nil
}

// ValidatePassword checks the password meets minimum requirements.
func ValidatePassword(password string, minLength int) error {
	if len(password) < minLength {
		return fmt.Errorf("%w: minimum %d characters", ErrPasswordTooShort, minLength)
	}
	// bcrypt truncates silently at 72 bytes; reject anything longer so the user
	// isn't lulled into thinking a long passphrase is fully protecting them.
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("%w: maximum %d bytes", ErrPasswordTooLong, maxPasswordBytes)
	}
	// Passphrases are encouraged up to the bcrypt limit.
	// No complexity requirements — length > complexity for user-chosen passwords.
	return nil
}
