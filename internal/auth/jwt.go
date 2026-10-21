package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims are the JWT payload fields. Subject is the user ID; SessionID is the
// ID of the refresh token (session) the access token was issued under, which
// lets the server reject access tokens from revoked sessions immediately.
type Claims struct {
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// TokenPair holds a newly-issued access token and its signed string form.
type TokenPair struct {
	AccessToken string
	ExpiresAt   time.Time
}

var (
	ErrTokenExpired = errors.New("token expired")
	ErrTokenInvalid = errors.New("token invalid")
)

// IssueAccessToken creates and signs a new JWT access token.
func IssueAccessToken(userID, sessionID uuid.UUID, secret string, expiry time.Duration) (TokenPair, error) {
	now := time.Now()
	exp := now.Add(expiry)

	claims := Claims{
		SessionID: sessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    "chorus",
			Audience:  jwt.ClaimStrings{"chorus"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return TokenPair{}, fmt.Errorf("sign token: %w", err)
	}

	return TokenPair{AccessToken: signed, ExpiresAt: exp}, nil
}

// ValidateAccessToken parses and validates a JWT, returning its claims.
func ValidateAccessToken(tokenStr, secret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithIssuer("chorus"), jwt.WithAudience("chorus"), jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}
