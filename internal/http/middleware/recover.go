package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/softsrv/chorus/internal/http/respond"
)

// Recover turns a panic in a handler into a logged 500 response instead of a
// dropped connection. http.ErrAbortHandler is re-panicked so net/http can
// abort the response as intended.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity per net/http docs
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "panic in handler",
				"request_id", GetRequestID(r.Context()),
				"panic", rec,
				"stack", string(debug.Stack()),
			)
			respond.Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
		}()
		next.ServeHTTP(w, r)
	})
}
