package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/softsrv/chorus/internal/http/middleware"
)

func TestRecover(t *testing.T) {
	t.Parallel()

	h := middleware.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rr.Code)
	}
	if got := errorCode(t, rr); got != "internal_error" {
		t.Errorf("error code: got %q, want internal_error", got)
	}
}
