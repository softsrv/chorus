package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// roundTripFunc lets a test answer every request in-process.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const (
	newAccessSecret    = "new-access-FAKE-SECRET"
	storedAccessSecret = "stored-access-FAKE-SECRET"
	storedRefresh      = "stored-refresh-FAKE"
)

// tokenInfoFailingCalendar is a GoogleCalendar whose token endpoint mints
// storedAccessSecret from a refresh token and whose tokeninfo endpoint fails
// as fail says. Every tokeninfo request is checked to carry the token in its
// form body, never the URL.
func tokenInfoFailingCalendar(t *testing.T, fail func(*http.Request) (*http.Response, error)) *GoogleCalendar {
	t.Helper()
	cipher, err := NewTokenCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(rec, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, storedAccessSecret)
			return rec.Result(), nil
		case "/tokeninfo":
			if r.Method != http.MethodPost || r.URL.RawQuery != "" {
				t.Errorf("tokeninfo request %s %s: want POST with no query", r.Method, r.URL)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "access_token=") {
				t.Errorf("tokeninfo body %q carries no access_token", body)
			}
			return fail(r)
		}
		return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
	})
	return NewGoogleCalendar(nil, cipher, GoogleCalendarConfig{
		ClientID:     "web-client.test",
		ClientSecret: "fake-client-secret",
		TokenURL:     "https://google.test/token",
		TokenInfoURL: "https://google.test/tokeninfo",
		HTTPClient:   &http.Client{Transport: rt},
	})
}

// captureLogs routes slog's default logger to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A failing tokeninfo call (transport error or non-200) never puts an access
// token in the returned error or the server log, for the new token or the
// stored one, and the same-account check fails closed.
func TestSameGoogleAccount_TokenInfoFailureLeaksNoToken(t *testing.T) {
	failures := map[string]func(*http.Request) (*http.Response, error){
		"transport error": func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		},
		"status 500": func(*http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			rec.WriteHeader(http.StatusInternalServerError)
			return rec.Result(), nil
		},
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			g := tokenInfoFailingCalendar(t, fail)

			for _, tok := range []string{newAccessSecret, storedAccessSecret} {
				_, err := g.tokenSubject(context.Background(), tok)
				if err == nil {
					t.Fatalf("tokenSubject(%q) succeeded, want an error", tok)
				}
				if strings.Contains(err.Error(), tok) {
					t.Errorf("tokenSubject error carries the token: %v", err)
				}
			}

			enc, err := g.cipher.Encrypt(storedRefresh)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			if g.sameGoogleAccount(context.Background(), newAccessSecret, enc) {
				t.Fatal("sameGoogleAccount = true on a failing tokeninfo, want false")
			}
			out := logs.String()
			if !strings.Contains(out, "tokeninfo") {
				t.Fatalf("expected a tokeninfo warning in the log, got %q", out)
			}
			for _, secret := range []string{newAccessSecret, storedAccessSecret, storedRefresh} {
				if strings.Contains(out, secret) {
					t.Errorf("log carries %q: %s", secret, out)
				}
			}
		})
	}
}

// withoutURL drops the request URL of a *url.Error, wherever it sits.
func TestWithoutURL(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	_, err := client.Get("https://google.test/x?access_token=" + newAccessSecret)
	if err == nil || !strings.Contains(err.Error(), newAccessSecret) {
		t.Fatalf("precondition: net/http error should carry the URL, got %v", err)
	}
	got := withoutURL(err).Error()
	if strings.Contains(got, newAccessSecret) || strings.Contains(got, "google.test") {
		t.Errorf("withoutURL kept the URL: %q", got)
	}
	if !strings.Contains(got, "dial failed") {
		t.Errorf("withoutURL lost the cause: %q", got)
	}
	plain := errors.New("plain")
	if withoutURL(plain) != plain {
		t.Error("withoutURL changed an error with no *url.Error")
	}
}
