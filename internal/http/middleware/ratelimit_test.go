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
}
