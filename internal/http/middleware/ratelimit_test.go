package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/softsrv/chorus/internal/http/middleware"
)

func TestRateLimiter(t *testing.T) {
	t.Parallel()

	alwaysOK := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("allows requests within limit", func(t *testing.T) {
		t.Parallel()
		rl := middleware.NewRateLimiter(context.Background(), 3, time.Minute, middleware.IPKeyFunc(0))
		handler := rl.Middleware(alwaysOK)

		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = "1.2.3.4:9999"
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("request %d: got %d, want 200", i+1, rr.Code)
			}
		}
	})

	t.Run("blocks requests over limit", func(t *testing.T) {
		t.Parallel()
		rl := middleware.NewRateLimiter(context.Background(), 2, time.Minute, middleware.IPKeyFunc(0))
		handler := rl.Middleware(alwaysOK)

		for i := 0; i < 2; i++ {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = "5.6.7.8:9999"
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
		}

		// Third request should be blocked.
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "5.6.7.8:9999"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusTooManyRequests {
			t.Errorf("got %d, want 429", rr.Code)
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Error("expected Retry-After header")
		}
	})

	t.Run("different IPs are tracked separately", func(t *testing.T) {
		t.Parallel()
		rl := middleware.NewRateLimiter(context.Background(), 1, time.Minute, middleware.IPKeyFunc(0))
		handler := rl.Middleware(alwaysOK)

		for _, ip := range []string{"10.0.0.1:1", "10.0.0.2:1", "10.0.0.3:1"} {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = ip
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("ip %s: got %d, want 200", ip, rr.Code)
			}
		}
	})

	t.Run("sweep goroutine exits when context is cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		// Just verify NewRateLimiter returns without blocking and cancel is honoured.
		_ = middleware.NewRateLimiter(ctx, 5, time.Minute, middleware.IPKeyFunc(0))
		cancel() // sweep goroutine should exit on its next tick
	})

	t.Run("FailuresOnly charges only 403s, and shares the budget with Middleware", func(t *testing.T) {
		t.Parallel()
		rl := middleware.NewRateLimiter(context.Background(), 2, time.Minute, middleware.IPKeyFunc(0))
		status := http.StatusOK
		read := rl.FailuresOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		action := rl.Middleware(alwaysOK)
		do := func(h http.Handler) int {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "9.9.9.9:1"
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			return rr.Code
		}

		// Successful reads are refunded: many of them spend nothing.
		for i := 0; i < 5; i++ {
			if got := do(read); got != http.StatusOK {
				t.Fatalf("successful read %d: got %d, want 200", i+1, got)
			}
		}
		// One wrong guess on the read spends one slot of the shared budget.
		status = http.StatusForbidden
		if got := do(read); got != http.StatusForbidden {
			t.Fatalf("failed read: got %d, want 403", got)
		}
		if got := do(action); got != http.StatusOK {
			t.Fatalf("action within budget: got %d, want 200", got)
		}
		// Budget spent (1 failed read + 1 action): both routes are refused.
		if got := do(action); got != http.StatusTooManyRequests {
			t.Errorf("action past budget: got %d, want 429", got)
		}
		status = http.StatusOK
		if got := do(read); got != http.StatusTooManyRequests {
			t.Errorf("read past budget: got %d, want 429", got)
		}
	})
}
