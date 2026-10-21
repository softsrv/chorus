package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// generateRandomToken is the shared implementation for all 32-byte URL-safe tokens.
// It returns both the raw base64 form (for delivery) and its SHA-256 hash (for storage).
func generateRandomToken(label string) (raw, hashed string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate %s: %w", label, err)
	}
	raw = base64.URLEncoding.EncodeToString(b)
	hashed = HashToken(raw)
	return raw, hashed, nil
}

// GenerateRefreshToken creates a cryptographically random 32-byte refresh token
// and returns both the raw (for the cookie) and hashed (for storage) forms.
func GenerateRefreshToken() (raw, hashed string, err error) {
	return generateRandomToken("refresh token")
}

// GenerateResetToken creates a cryptographically random URL-safe reset token.
func GenerateResetToken() (raw, hashed string, err error) {
	return generateRandomToken("reset token")
}

// HashToken returns the hex-encoded SHA-256 hash of the given token string.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CompareTokenHash performs a constant-time comparison of a raw token against a stored hash.
func CompareTokenHash(raw, storedHash string) bool {
	computed := HashToken(raw)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHash)) == 1
}

// GenerateVerificationToken creates a cryptographically random URL-safe token
// for email verification links, returning both the raw form (for the URL) and
// its SHA-256 hash (for storage).
func GenerateVerificationToken() (raw, hashed string, err error) {
	return generateRandomToken("verification token")
}

// inviteAlphabet is Crockford base32: no I, L, O, or U, so codes survive being
// read aloud or typed from a phone screen.
const inviteAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// GenerateInviteCode creates a human-typeable household invite code of the form
// XXXX-XXXX-XXXX (60 bits of entropy) and returns it with its SHA-256 hash.
// Codes are single-use, expire, and redemption is rate limited, so 60 bits is
// far beyond guessable.
func GenerateInviteCode() (raw, hashed string, err error) {
	b := make([]byte, 12)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate invite code: %w", err)
	}
	code := make([]byte, 0, 14)
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			code = append(code, '-')
		}
		code = append(code, inviteAlphabet[v&31])
	}
	raw = string(code)
	return raw, HashToken(NormalizeInviteCode(raw)), nil
}

// NormalizeInviteCode canonicalizes user-entered invite codes: uppercase,
// separators and whitespace removed, and commonly confused letters mapped to
// the digits Crockford base32 uses for them.
func NormalizeInviteCode(code string) string {
	out := make([]byte, 0, len(code))
	for _, c := range []byte(strings.ToUpper(code)) {
		switch c {
		case '-', ' ', '\t':
			continue
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		out = append(out, c)
	}
	return string(out)
}
