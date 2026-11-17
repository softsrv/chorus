package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

	// After a forced window expiry, a burst of concurrent requests must see
	// exactly one window reset and share that new window's budget: no more
	// than limit are admitted, whichever wrapper charges the limiter.
	burstAfterExpiry := func(t *testing.T, wrap func(*middleware.RateLimiter) http.Handler, admittedCode int) {
		t.Helper()
		const limit = 5
		const numGoroutines = 64
		const addr = "192.168.1.1:1234"
		rl := middleware.NewRateLimiter(context.Background(), limit, time.Minute, middleware.IPKeyFunc(0))
		h := wrap(rl)

		for round := 0; round < 20; round++ {
			// The stored entry is spent and already expired: without a reset
			// every request would be refused, and a non-atomic reset lets
			// more than limit through.
			keyReq := httptest.NewRequest(http.MethodGet, "/", nil)
			keyReq.RemoteAddr = addr
			middleware.ExpireWindow(rl, keyReq)

			start := make(chan struct{})
			var (
				wg        sync.WaitGroup
				admitted  atomic.Int64
				overLimit atomic.Int64
				other     atomic.Int64
			)
			for i := 0; i < numGoroutines; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req := httptest.NewRequest(http.MethodGet, "/", nil)
					req.RemoteAddr = addr
					rec := httptest.NewRecorder()
					<-start
					h.ServeHTTP(rec, req)
					switch rec.Code {
					case admittedCode:
						admitted.Add(1)
					case http.StatusTooManyRequests:
						overLimit.Add(1)
					default:
						other.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()

			if got := admitted.Load(); got != limit {
				t.Fatalf("round %d: admitted %d requests after window expiry, want exactly %d", round, got, limit)
			}
			if got := overLimit.Load(); got != numGoroutines-limit {
				t.Fatalf("round %d: refused %d requests, want %d", round, got, numGoroutines-limit)
			}
			if got := other.Load(); got != 0 {
				t.Fatalf("round %d: %d responses with an unexpected status", round, got)
			}
		}
	}

	t.Run("concurrent Middleware requests after window expiry admit at most limit", func(t *testing.T) {
		t.Parallel()
		burstAfterExpiry(t, func(rl *middleware.RateLimiter) http.Handler {
			return rl.Middleware(alwaysOK)
		}, http.StatusOK)
	})

	t.Run("concurrent FailuresOnly 403s after window expiry admit at most limit", func(t *testing.T) {
		t.Parallel()
		burstAfterExpiry(t, func(rl *middleware.RateLimiter) http.Handler {
			return rl.FailuresOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			}))
		}, http.StatusForbidden)
	})
}
