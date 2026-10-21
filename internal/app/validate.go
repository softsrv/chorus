package app

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// validator accumulates field errors so a single response can report every
// problem with a request rather than just the first.
type validator struct {
	fields map[string]string
}

func (v *validator) fail(field, msg string) {
	if v.fields == nil {
		v.fields = map[string]string{}
	}
	if _, exists := v.fields[field]; !exists {
		v.fields[field] = msg
	}
}

// err returns a *ValidationError if any field failed, otherwise nil.
func (v *validator) err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: v.fields}
}

// text trims s and checks its length (in characters) and content. Control
// characters are rejected; newlines and tabs are permitted only when
// multiline is true. Returns the trimmed value.
func (v *validator) text(field, s string, minLen, maxLen int, multiline bool) string {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		v.fail(field, "must be valid UTF-8")
		return s
	}
	n := utf8.RuneCountInString(s)
	switch {
	case n < minLen && minLen == 1:
		v.fail(field, "is required")
	case n < minLen:
		v.fail(field, "is too short")
	case n > maxLen:
		v.fail(field, "is too long")
	}
	for _, r := range s {
		if multiline && (r == '\n' || r == '\r' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			v.fail(field, "contains invalid characters")
			break
		}
	}
	return s
}

// displayName validates and normalizes a user's display name.
func (v *validator) displayName(s string) string {
	return v.text("display_name", s, 1, 50, false)
}
