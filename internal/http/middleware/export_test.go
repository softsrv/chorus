package middleware

import (
	"net/http"
	"time"
)

// ExpireWindow replaces the entry r is keyed to with one whose window has
// already expired and whose budget is fully spent, so a test can force a
// window reset deterministically instead of sleeping past a short window.
func ExpireWindow(rl *RateLimiter, r *http.Request) {
	e := &entry{expiresAt: time.Now().Add(-time.Second)}
	e.count.Store(rl.limit)
	rl.store.Store(rl.keyFunc(r), e)
}
