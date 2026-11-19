package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeJWKSServer sets up an httptest.Server serving an RSA public key in JWKS format.
type fakeJWKSServer struct {
	server   *httptest.Server
	privKey  *rsa.PrivateKey
	kid      string
	hitCount atomic.Int64
	handler  http.HandlerFunc
}

func newFakeJWKSServer(t *testing.T, kid string) *fakeJWKSServer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	f := &fakeJWKSServer{
		privKey: priv,
		kid:     kid,
	}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hitCount.Add(1)
		if f.handler != nil {
			f.handler(w, r)
			return
		}

		nBytes := priv.N.Bytes()
		eBytes := big.NewInt(int64(priv.E)).Bytes()
		resp := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": kid,
					"n":   base64.RawURLEncoding.EncodeToString(nBytes),
					"e":   base64.RawURLEncoding.EncodeToString(eBytes),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.server.Close)

	return f
}

func signIDToken(t *testing.T, key any, kid, alg string, claims jwt.MapClaims) string {
	t.Helper()
	var method jwt.SigningMethod
	switch alg {
	case "RS256":
		method = jwt.SigningMethodRS256
	case "HS256":
		method = jwt.SigningMethodHS256
	case "none":
		method = jwt.SigningMethodNone
	default:
		t.Fatalf("unsupported alg: %s", alg)
	}

	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}

	var signed string
	var err error
	if alg == "none" {
		signed, err = tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	} else {
		signed, err = tok.SignedString(key)
	}
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func baseValidClaims(aud string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            aud,
		"sub":            "google-sub-12345",
		"email":          "person@example.test",
		"email_verified": true,
		"name":           "Test Person",
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
}

// CLM-6: Table-driven unit tests for the ID-token verifier.
func TestIDTokenVerifier_Table(t *testing.T) {
	const validKid = "test-kid-1"
	const clientWeb = "web-client.test"
	const clientIOS = "ios-client.test"

	jwks := newFakeJWKSServer(t, validKid)
	verifier := newIDTokenVerifier([]string{clientWeb, clientIOS}, jwks.server.URL, jwks.server.Client())

	otherPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other rsa key: %v", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&jwks.privKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}

	tests := []struct {
		name      string
		tokenFn   func() string
		wantErr   bool
		checkInfo func(t *testing.T, info googleUserInfo)
	}{
		{
			name: "valid token with https://accounts.google.com",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				c["iss"] = "https://accounts.google.com"
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: false,
			checkInfo: func(t *testing.T, info googleUserInfo) {
				if info.Subject != "google-sub-12345" || info.Email != "person@example.test" {
					t.Errorf("unexpected userInfo: %+v", info)
				}
				if !info.verified() {
					t.Errorf("expected email to be verified")
				}
			},
		},
		{
			name: "valid token with accounts.google.com",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				c["iss"] = "accounts.google.com"
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: false,
		},
		{
			name: "valid token with secondary configured audience (iOS)",
			tokenFn: func() string {
				c := baseValidClaims(clientIOS)
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: false,
		},
		{
			name: "bad signature",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				return signIDToken(t, otherPriv, validKid, "RS256", c)
			},
			wantErr: true,
		},
		{
			name: "alg none",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				return signIDToken(t, nil, validKid, "none", c)
			},
			wantErr: true,
		},
		{
			name: "alg HS256 keyed with public key",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				return signIDToken(t, pubDER, validKid, "HS256", c)
			},
			wantErr: true,
		},
		{
			name: "expired exp",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
				c["exp"] = time.Now().Add(-1 * time.Hour).Unix()
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: true,
		},
		{
			name: "iss outside Google allowed issuers",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				c["iss"] = "https://attacker.com"
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: true,
		},
		{
			name: "aud outside configured client IDs",
			tokenFn: func() string {
				c := baseValidClaims("unconfigured-client.test")
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: true,
		},
		{
			name: "email_verified boolean false",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				c["email_verified"] = false
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: true,
		},
		{
			name: "email_verified string false",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				c["email_verified"] = "false"
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: true,
		},
		{
			name: "email_verified absent",
			tokenFn: func() string {
				c := baseValidClaims(clientWeb)
				delete(c, "email_verified")
				return signIDToken(t, jwks.privKey, validKid, "RS256", c)
			},
			wantErr: true,
		},
		{
			name: "empty token",
			tokenFn: func() string {
				return ""
			},
			wantErr: true,
		},
		{
			name: "malformed token",
			tokenFn: func() string {
				return "header.payload"
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token := tc.tokenFn()
			info, err := verifier.verify(context.Background(), token)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil (info: %+v)", info)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.checkInfo != nil {
				tc.checkInfo(t, info)
			}
		})
	}
}

// CLM-6: Unknown kid refetches the JWKS exactly once, and fails if still unknown.
func TestIDTokenVerifier_UnknownKidRefetchesOnce(t *testing.T) {
	const initialKid = "kid-initial"
	const rotatedKid = "kid-rotated"
	const clientWeb = "web-client.test"

	jwks := newFakeJWKSServer(t, initialKid)
	verifier := newIDTokenVerifier([]string{clientWeb}, jwks.server.URL, jwks.server.Client())

	// 1. Initial valid token populates verifier cache (1 JWKS hit).
	c1 := baseValidClaims(clientWeb)
	t1 := signIDToken(t, jwks.privKey, initialKid, "RS256", c1)
	if _, err := verifier.verify(context.Background(), t1); err != nil {
		t.Fatalf("initial verify failed: %v", err)
	}
	if hits := jwks.hitCount.Load(); hits != 1 {
		t.Fatalf("expected 1 JWKS hit, got %d", hits)
	}

	// 2. Set fetchedAt past the 1-minute refetch throttle to simulate age >= minRefetch.
	verifier.mu.Lock()
	verifier.fetchedAt = time.Now().Add(-2 * time.Minute)
	verifier.mu.Unlock()

	// 3. Token with unknown kid should trigger exactly one refetch.
	c2 := baseValidClaims(clientWeb)
	t2 := signIDToken(t, jwks.privKey, rotatedKid, "RS256", c2)
	_, err := verifier.verify(context.Background(), t2)
	if err == nil {
		t.Fatalf("expected error for unknown kid, got nil")
	}

	// Hits should have increased by exactly 1 (to 2 total).
	if hits := jwks.hitCount.Load(); hits != 2 {
		t.Fatalf("expected 2 JWKS hits after refetch, got %d", hits)
	}

	// 4. Another attempt immediately after with unknown kid is throttled and does not hit JWKS again.
	_, _ = verifier.verify(context.Background(), t2)
	if hits := jwks.hitCount.Load(); hits != 2 {
		t.Fatalf("expected JWKS hits to remain 2 when throttled, got %d", hits)
	}
}

// CLM-6: Fail-closed cases: JWKS server returning 500 or timing out fails closed.
func TestIDTokenVerifier_FailClosed(t *testing.T) {
	const validKid = "fail-closed-kid"
	const clientWeb = "web-client.test"

	t.Run("JWKS returns 500", func(t *testing.T) {
		jwks := newFakeJWKSServer(t, validKid)
		jwks.handler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}

		verifier := newIDTokenVerifier([]string{clientWeb}, jwks.server.URL, jwks.server.Client())
		token := signIDToken(t, jwks.privKey, validKid, "RS256", baseValidClaims(clientWeb))

		_, err := verifier.verify(context.Background(), token)
		if err == nil {
			t.Fatalf("expected error on JWKS 500, got nil")
		}
	})

	t.Run("JWKS hang past client timeout", func(t *testing.T) {
		hangServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		t.Cleanup(hangServer.Close)

		client := &http.Client{Timeout: 50 * time.Millisecond}
		verifier := newIDTokenVerifier([]string{clientWeb}, hangServer.URL, client)

		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate rsa key: %v", err)
		}
		token := signIDToken(t, priv, validKid, "RS256", baseValidClaims(clientWeb))

		start := time.Now()
		_, err = verifier.verify(context.Background(), token)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatalf("expected error on JWKS timeout, got nil")
		}
		if elapsed > 1*time.Second {
			t.Errorf("expected fail-closed within bounded client timeout, took %v", elapsed)
		}
	})
}
