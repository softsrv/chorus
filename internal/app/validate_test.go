package app

import (
	"errors"
	"testing"
)

func TestValidator_PIN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		pin   string
		valid bool
	}{
		{"valid 4 digits", "0123", true},
		{"valid all zeros", "0000", true},
		{"valid all nines", "9999", true},
		{"too short 3 digits", "123", false},
		{"too long 5 digits", "12345", false},
		{"empty", "", false},
		{"alphabetic", "abcd", false},
		{"mixed digits and letters", "12a4", false},
		{"leading space", " 123", false},
		{"trailing space", "123 ", false},
		{"embedded space", "12 4", false},
		{"negative number", "-123", false},
		{"fullwidth unicode digits", "１２３４", false},
		{"eastern arabic unicode digits", "\u0661\u0662\u0663\u0664", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v validator
			got := v.pin(tc.pin)
			if got != tc.pin {
				t.Errorf("pin returned %q, want %q", got, tc.pin)
			}
			err := v.err()
			if tc.valid {
				if err != nil {
					t.Errorf("pin %q: expected valid, got err %v", tc.pin, err)
				}
			} else {
				if err == nil {
					t.Errorf("pin %q: expected validation error, got nil", tc.pin)
				}
				var vErr *ValidationError
				if !errors.As(err, &vErr) {
					t.Fatalf("pin %q: expected *ValidationError, got %T", tc.pin, err)
				}
				if vErr.Fields["pin"] == "" {
					t.Errorf("pin %q: expected field error on 'pin', got fields %v", tc.pin, vErr.Fields)
				}
			}
		})
	}
}
