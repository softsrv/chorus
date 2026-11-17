package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/softsrv/chorus/internal/http/respond"
)

// entry holds the request count and the time the window expires.
type entry struct {
	count     atomic.Int64
	expiresAt time.Time
}

// RateLimiter is an in-memory rate limiter using sync.Map with TTL-based expiry.
// Not shared across processes; suitable for single-instance deployments.
type RateLimiter struct {
	store   sync.Map
	limit   int64
	window  time.Duration
	keyFunc func(*http.Request) string
}

// NewRateLimiter creates a RateLimiter with the given limit, window, and key extractor.
// The provided ctx controls the lifetime of the background sweep goroutine; cancel it
// during application shutdown to cleanly stop the sweeper.
func NewRateLimiter(ctx context.Context, limit int, window time.Duration, keyFunc func(*http.Request) string) *RateLimiter {
	rl := &RateLimiter{
		limit:   int64(limit),
		window:  window,
		keyFunc: keyFunc,
	}
	go rl.sweep(ctx)
	return rl
}

// Middleware returns an http.Handler middleware that enforces the rate limit.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := rl.take(w, r); ok {
			next.ServeHTTP(w, r)
		}
	})
}

// FailuresOnly returns a middleware that charges this limiter only for
// requests that end in 403 Forbidden (a wrong or missing PIN). Each request
// reserves a slot before it runs, so once the budget is spent every request
// is refused with 429 and concurrent guesses can never overshoot it; a
// request that does not end in 403 gives its slot back. Wrap a PIN-checking
// read with it so wrong guesses share the PIN budget while correct reads
// leave it untouched.
func (rl *RateLimiter) FailuresOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, ok := rl.take(w, r)
		if !ok {
			return
		}
		sw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if sw.status != http.StatusForbidden {
			e.count.Add(-1)
		}
	})
}

// take charges one request against the caller's budget. It reports false,
// having already answered 429, when the budget is spent; otherwise it
// returns the entry it charged so the caller can refund it.
func (rl *RateLimiter) take(w http.ResponseWriter, r *http.Request) (*entry, bool) {
	key := rl.keyFunc(r)

	for {
		now := time.Now()
		val, loaded := rl.store.Load(key)
		if !loaded {
			newEntry := &entry{expiresAt: now.Add(rl.window)}
			actual, loaded := rl.store.LoadOrStore(key, newEntry)
			if !loaded {
				val = newEntry
			} else {
				val = actual
			}
		}

		e := val.(*entry)
		if now.After(e.expiresAt) {
			newEntry := &entry{expiresAt: now.Add(rl.window)}
			if !rl.store.CompareAndSwap(key, e, newEntry) {
				continue
			}
			e = newEntry
		}

		count := e.count.Add(1)
		if count > rl.limit {
			retryAfter := int(time.Until(e.expiresAt).Seconds()) + 1
			slog.WarnContext(r.Context(), "rate limit exceeded",
				"key", key,
				"path", r.URL.Path,
				"request_id", GetRequestID(r.Context()),
			)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			respond.Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again later.")
			return nil, false
		}
		return e, true
	}
}

// sweep periodically removes expired entries to prevent unbounded memory growth.
// It exits when ctx is cancelled, which should happen during application shutdown.
func (rl *RateLimiter) sweep(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			rl.store.Range(func(k, v any) bool {
				if e, ok := v.(*entry); ok && now.After(e.expiresAt) {
					rl.store.Delete(k)
				}
				return true
			})
		}
	}
}

// IPKeyFunc returns a rate-limit key for the client IP.
// trustedProxies is the number of trusted reverse-proxy hops; set to 0 if
// this service is exposed directly to the internet.
func IPKeyFunc(trustedProxies int) func(*http.Request) string {
	return func(r *http.Request) string {
		return "ip:" + ClientIP(r, trustedProxies)
	}
}

// UserKeyFunc keys on the authenticated user, falling back to the client IP.
// The limiter must be applied inside Authenticate for the user to be present.
func UserKeyFunc(trustedProxies int) func(*http.Request) string {
	return func(r *http.Request) string {
		if u, ok := UserFromContext(r.Context()); ok {
			return "user:" + u.ID.String()
		}
		return "ip:" + ClientIP(r, trustedProxies)
	}
}
