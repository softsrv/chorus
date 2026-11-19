package app

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/softsrv/chorus/internal/db"
)

// googleJWKSURL is where Google publishes the keys that sign its ID tokens.
const googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

const (
	// googleKeysTTL bounds how long fetched keys are trusted before a refetch.
	googleKeysTTL = time.Hour
	// googleKeysMinRefetch bounds refetches caused by an unknown key id, so
	// tokens with made-up key ids cannot make us hammer Google.
	googleKeysMinRefetch = time.Minute
	// googleTokenLeeway tolerates small clock skew on exp/iat/nbf.
	googleTokenLeeway = 30 * time.Second
)

// googleIssuers are the two iss values Google documents for its ID tokens.
var googleIssuers = map[string]bool{"accounts.google.com": true, "https://accounts.google.com": true}

// googleIDClaims is the subset of a Google ID token's claims sign-in uses.
type googleIDClaims struct {
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
	jwt.RegisteredClaims
}

// idTokenVerifier checks Google ID tokens: RS256 signature against Google's
// published keys (cached by key id), issuer, audience in the configured
// client IDs, and expiry. Whether the email is verified is left to
// resolveUser, which refuses an unverified identity.
type idTokenVerifier struct {
	audiences map[string]bool
	jwksURL   string
	client    *http.Client

	mu sync.Mutex
	// keys were fetched successfully at fetchedAt; failedAt is the last
	// failed attempt. Both count against the refetch throttle, so a failing
	// key endpoint is asked at most once per googleKeysMinRefetch.
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	failedAt  time.Time
	// flight is the fetch in progress, shared by every caller that needs
	// keys meanwhile (single-flight); the mutex is never held across it.
	flight *keyFetch
}

// keyFetch is one in-flight fetch of Google's key set. keys and err are set
// before done is closed.
type keyFetch struct {
	done chan struct{}
	keys map[string]*rsa.PublicKey
	err  error
}

func newIDTokenVerifier(clientIDs []string, jwksURL string, client *http.Client) *idTokenVerifier {
	aud := make(map[string]bool, len(clientIDs))
	for _, id := range clientIDs {
		if id = strings.TrimSpace(id); id != "" {
			aud[id] = true
		}
	}
	if jwksURL == "" {
		jwksURL = googleJWKSURL
	}
	return &idTokenVerifier{audiences: aud, jwksURL: jwksURL, client: client}
}

// verify returns the identity a valid token carries. Every error is a refusal;
// the caller logs the cause and shows only a generic error.
func (v *idTokenVerifier) verify(ctx context.Context, raw string) (googleUserInfo, error) {
	if strings.TrimSpace(raw) == "" {
		return googleUserInfo{}, errors.New("empty id token")
	}
	var c googleIDClaims
	_, err := jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no key id")
		}
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(googleTokenLeeway),
	)
	if err != nil {
		return googleUserInfo{}, fmt.Errorf("parse id token: %w", err)
	}
	if !googleIssuers[c.Issuer] {
		return googleUserInfo{}, fmt.Errorf("id token issuer %q is not Google", truncate(c.Issuer, 64))
	}
	if !v.audienceOK(c.Audience) {
		return googleUserInfo{}, errors.New("id token audience is not a configured client id")
	}
	info := googleUserInfo{Subject: c.Subject, Email: c.Email, EmailVerified: c.EmailVerified, Name: c.Name}
	if !info.verified() {
		return googleUserInfo{}, errors.New("id token email not verified")
	}
	return info, nil
}

func (v *idTokenVerifier) audienceOK(aud jwt.ClaimStrings) bool {
	for _, a := range aud {
		if v.audiences[a] {
			return true
		}
	}
	return false
}

// key returns the public key for kid, fetching Google's key set when the
// cache is empty or stale, or when kid is unknown, as happens after Google
// rotates its keys. Fetches, failed ones included, are throttled to one per
// googleKeysMinRefetch, so tokens with made-up key ids cannot make us hammer
// Google, and concurrent callers share one fetch without queueing on the
// mutex.
func (v *idTokenVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetchedAt) < googleKeysTTL {
		v.mu.Unlock()
		return k, nil
	}
	f := v.flight
	if f == nil {
		last := v.fetchedAt
		if v.failedAt.After(last) {
			last = v.failedAt
		}
		if !last.IsZero() && time.Since(last) < googleKeysMinRefetch {
			failed := v.failedAt.After(v.fetchedAt)
			v.mu.Unlock()
			if failed {
				return nil, errors.New("signing keys unavailable")
			}
			return nil, errors.New("unknown key id")
		}
		f = &keyFetch{done: make(chan struct{})}
		v.flight = f
		// The fetch outlives this caller's cancellation (others wait on it)
		// but is bounded by its own timeout.
		go v.refresh(context.WithoutCancel(ctx), f)
	}
	v.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	if k, ok := f.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown key id")
}

// refresh runs the fetch f stands for and records its outcome, success or
// failure, for the throttle.
func (v *idTokenVerifier) refresh(ctx context.Context, f *keyFetch) {
	ctx, cancel := context.WithTimeout(ctx, googleHTTPTimeout)
	defer cancel()
	keys, err := v.fetch(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "google sign-in: fetch signing keys", "error", err)
	}
	v.mu.Lock()
	if err != nil {
		v.failedAt = time.Now()
	} else {
		v.keys, v.fetchedAt = keys, time.Now()
	}
	v.flight = nil
	v.mu.Unlock()
	f.keys, f.err = keys, err
	close(f.done)
}

type jwkSet struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (v *idTokenVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build jwks request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := int(new(big.Int).SetBytes(e).Int64())
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks has no usable RSA keys")
	}
	return keys, nil
}

// SignIn verifies a Google ID token from any of the configured apps (web,
// iOS, Android) and returns the same token pair as password login. Every
// refusal (bad token, unverified or conflicting identity) is
// ErrGoogleSignInFailed; the cause is only logged.
func (g *GoogleSignIn) SignIn(ctx context.Context, idToken string, meta DeviceMeta) (TokenResult, error) {
	user, err := g.signInUser(ctx, strings.TrimSpace(idToken))
	if err != nil {
		slog.WarnContext(ctx, "google sign-in refused", "error", err)
		return TokenResult{}, ErrGoogleSignInFailed
	}
	return g.authSvc.issueTokenPair(ctx, user, meta)
}

func (g *GoogleSignIn) signInUser(ctx context.Context, idToken string) (db.User, error) {
	if idToken == "" {
		return db.User{}, errors.New("empty id token")
	}
	info, err := g.verifier.verify(ctx, idToken)
	if err != nil {
		return db.User{}, err
	}
	user, err := g.resolveUser(ctx, info)
	if isUniqueViolation(err) {
		// A concurrent first sign-in for the same identity created the
		// account between our lookup and insert; resolve again to find it.
		user, err = g.resolveUser(ctx, info)
	}
	return user, err
}

// WebClientID is the web client's OAuth client ID (the first configured
// client ID), which the web client needs to start Google's sign-in.
func (g *GoogleSignIn) WebClientID() string { return g.webClientID }
