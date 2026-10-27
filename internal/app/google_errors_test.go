package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tagGoogleError: rate limits and Google outages are temporary
// (ErrGoogleUnavailable, 503 + Retry-After), a 401 is forbidden like the
// listing's broken, never a 502 rejection.
func TestTagGoogleError(t *testing.T) {
	notEditable := ErrEntryNotEditable
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"429", &googleAPIError{status: http.StatusTooManyRequests}, ErrGoogleUnavailable},
		{"500", &googleAPIError{status: http.StatusInternalServerError}, ErrGoogleUnavailable},
		{"502", &googleAPIError{status: http.StatusBadGateway}, ErrGoogleUnavailable},
		{"503", &googleAPIError{status: http.StatusServiceUnavailable}, ErrGoogleUnavailable},
		{"403 rate limit", &googleAPIError{status: http.StatusForbidden, rateLimited: true}, ErrGoogleUnavailable},
		{"401", &googleAPIError{status: http.StatusUnauthorized}, ErrForbidden},
		{"403", &googleAPIError{status: http.StatusForbidden}, notEditable},
		{"404", &googleAPIError{status: http.StatusNotFound}, ErrEntryGone},
		{"410", &googleAPIError{status: http.StatusGone}, ErrEntryGone},
		{"400", &googleAPIError{status: http.StatusBadRequest}, ErrGoogleRejected},
		{"409", &googleAPIError{status: http.StatusConflict}, ErrGoogleRejected},
		{"transport", errGoogleUnavailable, ErrGoogleUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tagGoogleError(tc.err, notEditable); !errors.Is(got, tc.want) {
				t.Errorf("tagGoogleError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The listing reports broken only for a refusal the setter must fix; a
// rate-limit 403, a 429 or a 5xx is transient instead.
func TestRefusedVersusTransient(t *testing.T) {
	for _, tc := range []struct {
		name               string
		err                error
		refused, transient bool
	}{
		{"401", &googleAPIError{status: 401}, true, false},
		{"403", &googleAPIError{status: 403}, true, false},
		{"404", &googleAPIError{status: 404}, true, false},
		{"403 rateLimitExceeded", &googleAPIError{status: 403, rateLimited: true}, false, true},
		{"429", &googleAPIError{status: 429}, false, true},
		{"500", &googleAPIError{status: 500}, false, true},
		{"503", &googleAPIError{status: 503}, false, true},
		{"400", &googleAPIError{status: 400}, false, false},
		{"other error", errors.New("x"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusedByGoogle(tc.err); got != tc.refused {
				t.Errorf("refusedByGoogle = %v, want %v", got, tc.refused)
			}
			if got := transientByGoogle(tc.err); got != tc.transient {
				t.Errorf("transientByGoogle = %v, want %v", got, tc.transient)
			}
		})
	}
}

// apiDo reads a 403's reason: rateLimitExceeded, userRateLimitExceeded,
// dailyLimitExceeded and quotaExceeded are temporary; any other 403
// (forbidden, insufficientPermissions) is a refusal.
func TestAPIDoRateLimitReason(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		rateLimited bool
	}{
		{"rateLimitExceeded", `{"error":{"code":403,"errors":[{"domain":"usageLimits","reason":"rateLimitExceeded"}]}}`, true},
		{"userRateLimitExceeded", `{"error":{"code":403,"errors":[{"domain":"usageLimits","reason":"userRateLimitExceeded"}]}}`, true},
		{"dailyLimitExceeded", `{"error":{"code":403,"errors":[{"domain":"usageLimits","reason":"dailyLimitExceeded"}]}}`, true},
		{"quotaExceeded", `{"error":{"code":403,"errors":[{"domain":"usageLimits","reason":"quotaExceeded"}]}}`, true},
		{"forbidden", `{"error":{"code":403,"errors":[{"domain":"global","reason":"forbidden"}]}}`, false},
		{"insufficientPermissions", `{"error":{"code":403,"errors":[{"domain":"global","reason":"insufficientPermissions"}]}}`, false},
		{"not json", `nope`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			g := NewGoogleCalendar(nil, nil, GoogleCalendarConfig{APIBaseURL: srv.URL})
			var out struct{}
			err := g.apiGet(t.Context(), "tok", "/x", nil, &out)
			var apiErr *googleAPIError
			if !errors.As(err, &apiErr) || apiErr.status != 403 || apiErr.rateLimited != tc.rateLimited {
				t.Fatalf("err = %#v, want 403 rateLimited=%v", err, tc.rateLimited)
			}
			if strings.Contains(err.Error(), "usageLimits") || strings.Contains(err.Error(), "nope") {
				t.Errorf("error %q carries Google's body", err)
			}
			if refusedByGoogle(err) == tc.rateLimited || transientByGoogle(err) != tc.rateLimited {
				t.Errorf("refused=%v transient=%v for rateLimited=%v", refusedByGoogle(err), transientByGoogle(err), tc.rateLimited)
			}
		})
	}
}

// The stored state hash depends on the browser nonce, so the state alone
// (as an attacker forwards it) never matches the row.
func TestCalendarStateHashBindsNonce(t *testing.T) {
	if calendarStateHash("s", "n1") == calendarStateHash("s", "n2") {
		t.Error("hash ignores the nonce")
	}
	if calendarStateHash("s", "n1") == calendarStateHash("s", "") {
		t.Error("hash with empty nonce matches")
	}
}
