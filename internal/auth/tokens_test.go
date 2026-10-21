package auth_test

import (
	"testing"

	"github.com/softsrv/chorus/internal/auth"
)

func TestGenerateRefreshToken(t *testing.T) {
	t.Parallel()

	raw, hashed, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	if raw == "" || hashed == "" {
		t.Fatal("expected non-empty raw and hashed tokens")
	}
	if raw == hashed {
		t.Fatal("raw and hashed must differ")
	}
}

func TestCompareTokenHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    string
		stored string
		want   bool
	}{
		{"matching tokens", "hello", auth.HashToken("hello"), true},
		{"mismatched tokens", "hello", auth.HashToken("world"), false},
		{"empty raw", "", auth.HashToken("hello"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := auth.CompareTokenHash(tt.raw, tt.stored)
			if got != tt.want {
				t.Errorf("CompareTokenHash(%q, ...): got %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestGenerateVerificationToken(t *testing.T) {
	t.Parallel()

	raw, hashed, err := auth.GenerateVerificationToken()
	if err != nil {
		t.Fatalf("GenerateVerificationToken: %v", err)
	}
	if raw == "" || hashed == "" {
		t.Fatal("expected non-empty raw and hashed tokens")
	}
	if raw == hashed {
		t.Fatal("raw and hashed must differ")
	}
	if !auth.CompareTokenHash(raw, hashed) {
		t.Fatal("CompareTokenHash must return true for matching pair")
	}
}
