package app

import (
	"net/url"
	"sort"
	"strings"
	"testing"
)

// CLM-14: the sign-in authorization request asks only for identity scopes.
func TestGoogleSignIn_AuthCodeURLScopesAreIdentityOnly(t *testing.T) {
	g := NewGoogleSignIn(nil, nil, GoogleSignInConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://chorus.test/api/v1/auth/google/callback",
	})

	raw := g.AuthCodeURL("some-state", "some-verifier-some-verifier-some-verifier-12")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth URL %q: %v", raw, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != googleAuthURL {
		t.Errorf("auth endpoint = %q, want %q", got, googleAuthURL)
	}
	q := u.Query()

	scopes := strings.Fields(q.Get("scope"))
	sort.Strings(scopes)
	want := []string{"email", "openid", "profile"}
	if strings.Join(scopes, " ") != strings.Join(want, " ") {
		t.Errorf("scope set = %v, want exactly %v", scopes, want)
	}
	for _, s := range scopes {
		if strings.Contains(s, "calendar") {
			t.Errorf("sign-in requested a calendar scope: %q", s)
		}
	}
	if q.Get("state") != "some-state" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Errorf("auth URL missing state or PKCE S256 challenge: %v", q)
	}
}
