package users_test

import (
	"strings"
	"testing"

	"github.com/softsrv/chorus/internal/users"
)

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"User@Example.COM", "user@example.com"},
		{"  user@example.com  ", "user@example.com"},
		{"UPPER@DOMAIN.ORG", "upper@domain.org"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := users.NormalizeEmail(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateEmail(t *testing.T) {
	t.Parallel()

	valid := []string{
		"user@example.com",
		"user+tag@example.org",
		"firstname.lastname@company.co.uk",
	}
	for _, email := range valid {
		t.Run("valid: "+email, func(t *testing.T) {
			t.Parallel()
			if err := users.ValidateEmail(email); err != nil {
				t.Errorf("ValidateEmail(%q) unexpected error: %v", email, err)
			}
		})
	}

	invalid := []string{
		"notanemail",
		"@nodomain",
		"missing@",
		"",
	}
	for _, email := range invalid {
		t.Run("invalid: "+email, func(t *testing.T) {
			t.Parallel()
			if err := users.ValidateEmail(email); err == nil {
				t.Errorf("ValidateEmail(%q) expected error, got nil", email)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
		minLen   int
		wantErr  bool
	}{
		{"valid 8 chars", "password", 8, false},
		{"valid passphrase", "correct horse battery staple", 8, false},
		{"too short", "short", 8, true},
		{"exactly min", "12345678", 8, false},
		{"empty", "", 8, true},
		{"exactly 72 bytes ok", strings.Repeat("a", 72), 8, false},
		{"73 bytes rejected", strings.Repeat("a", 73), 8, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := users.ValidatePassword(tt.password, tt.minLen)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePassword(%q, %d) error = %v, wantErr %v", tt.password, tt.minLen, err, tt.wantErr)
			}
		})
	}
}
